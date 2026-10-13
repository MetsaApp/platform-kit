package runtime_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/grpc"

	"github.com/MetsaApp/platform-kit/runtime"
)

// The runtime owns process-wide state (the contract's ports, the OTel
// globals, stderr), so every test drives it the way a pod does: as a
// process. The test binary re-executes itself as a small service when
// childEnv is set.
const (
	childEnv = "PLATFORM_KIT_TEST_CHILD"
	// workerEnv makes the child serve no traffic port.
	workerEnv = "PLATFORM_KIT_TEST_WORKER"
	// unreadyFileEnv names a file; the child's readiness check fails while
	// it exists.
	unreadyFileEnv = "PLATFORM_KIT_TEST_UNREADY_FILE"
)

func TestMain(m *testing.M) {
	if os.Getenv(childEnv) != "" {
		os.Exit(childMain())
	}
	os.Exit(m.Run())
}

// childMain is a service's main as the package intends it.
func childMain() int {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM)
	defer stop()

	rt, err := runtime.New(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	rt.Ready("dependency", func(context.Context) error {
		if _, statErr := os.Stat(os.Getenv(unreadyFileEnv)); statErr == nil {
			return errors.New("dependency down")
		}
		return nil
	})

	var handler http.Handler
	if os.Getenv(workerEnv) == "" {
		mux := http.NewServeMux()
		mux.HandleFunc("GET /config", func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(rt.Config) //nolint:musttag // Test-only dump; Config is not a wire type.
		})
		mux.HandleFunc("GET /work", func(_ http.ResponseWriter, r *http.Request) {
			spanCtx, span := otel.Tracer("child").Start(r.Context(), "work")
			defer span.End()
			rt.Logger.InfoContext(spanCtx, "working")
			rt.Logger.WarnContext(spanCtx, "careful")
		})
		handler = mux
	}

	if err = rt.Serve(ctx, handler); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

func TestServesTheContractPorts(t *testing.T) {
	startChild(t, nil)

	status, body := get(t, runtime.ProbePort, "/healthz")
	assert.Equal(t, http.StatusOK, status)
	assert.Equal(t, "ok\n", body)

	status, _ = get(t, runtime.ProbePort, "/readyz")
	assert.Equal(t, http.StatusOK, status)

	status, body = get(t, runtime.MetricsPort, "/metrics")
	assert.Equal(t, http.StatusOK, status)
	assert.Contains(t, body, `service_name="kit-test"`, "OTel metrics carry the configured service name")
	assert.Contains(t, body, "go_goroutines", "client_golang collectors are scraped on the same port")

	status, _ = get(t, runtime.HTTPPort, "/config")
	assert.Equal(t, http.StatusOK, status)

	// Probes and metrics are never routed: they are not on the traffic port.
	for _, path := range []string{"/healthz", "/readyz", "/metrics"} {
		status, _ = get(t, runtime.HTTPPort, path)
		assert.Equal(t, http.StatusNotFound, status, path)
	}
	assert.Equal(t, 8080, runtime.HTTPPort)
	assert.Equal(t, 8081, runtime.ProbePort)
	assert.Equal(t, 9464, runtime.MetricsPort)
}

func TestReadinessFollowsRegisteredChecks(t *testing.T) {
	unready := filepath.Join(t.TempDir(), "unready")
	require.NoError(t, os.WriteFile(unready, nil, 0o600))
	startChild(t, map[string]string{unreadyFileEnv: unready})

	status, body := get(t, runtime.ProbePort, "/readyz")
	assert.Equal(t, http.StatusServiceUnavailable, status)
	assert.Contains(t, body, "dependency")

	// Liveness only reflects the process: restarting would not fix it.
	status, _ = get(t, runtime.ProbePort, "/healthz")
	assert.Equal(t, http.StatusOK, status)

	require.NoError(t, os.Remove(unready))
	status, _ = get(t, runtime.ProbePort, "/readyz")
	assert.Equal(t, http.StatusOK, status)
}

func TestConfigReadsTheContractNames(t *testing.T) {
	startChild(t, map[string]string{
		"ENVIRONMENT":      "preview",
		"LOG_LEVEL":        "warn",
		"SERVICE_IDENTITY": "metsa-preview/kit-test",
		"DATABASE_URL":     "postgres://db/app?sslmode=require",
		"NATS_URL":         "nats://nats.nats.svc.cluster.local:4222",
		"NATS_TOKEN_FILE":  "/var/run/secrets/nats/token",
		"REDIS_URL":        "redis://dragonfly.dragonfly.svc:6379/3",
	})

	_, body := get(t, runtime.HTTPPort, "/config")
	assert.JSONEq(t, `{
		"Environment": "preview",
		"LogLevel": "WARN",
		"ServiceName": "kit-test",
		"ServiceIdentity": "metsa-preview/kit-test",
		"DatabaseURL": "postgres://db/app?sslmode=require",
		"NATSURL": "nats://nats.nats.svc.cluster.local:4222",
		"NATSTokenFile": "/var/run/secrets/nats/token",
		"RedisURL": "redis://dragonfly.dragonfly.svc:6379/3"
	}`, body)
}

func TestNewFailsWithoutAServiceName(t *testing.T) {
	t.Setenv("OTEL_SERVICE_NAME", "")

	_, err := runtime.New(t.Context())

	require.ErrorContains(t, err, "OTEL_SERVICE_NAME")
}

func TestTraceServiceNameComesFromConfig(t *testing.T) {
	collector := startCollector(t)
	child := startChild(t, map[string]string{
		"OTEL_SERVICE_NAME":                  "named-by-config",
		"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT": "http://" + collector.addr,
	})

	get(t, runtime.HTTPPort, "/work")
	child.stop(t) // A clean stop flushes the batch.

	assert.Equal(t, []string{"named-by-config"}, collector.serviceNames())
}

func TestLogsAreJSONWithTraceIDs(t *testing.T) {
	collector := startCollector(t)
	child := startChild(t, map[string]string{
		"ENVIRONMENT":                        "preview",
		"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT": "http://" + collector.addr,
	})

	get(t, runtime.HTTPPort, "/work")
	child.stop(t)

	line := child.logLine(t, "working")
	assert.Equal(t, "INFO", line["level"])
	assert.Equal(t, "kit-test", line["service"])
	assert.Equal(t, "preview", line["environment"])
	assert.Len(t, line["trace_id"], 32)
	assert.Len(t, line["span_id"], 16)
}

func TestLogLevelComesFromConfig(t *testing.T) {
	child := startChild(t, map[string]string{"LOG_LEVEL": "warn"})

	get(t, runtime.HTTPPort, "/work")
	child.stop(t)

	assert.NotNil(t, child.logLine(t, "careful"))
	assert.NotContains(t, child.stderr.String(), `"msg":"working"`)
}

func TestWorkerServesNoTrafficPort(t *testing.T) {
	startChild(t, map[string]string{workerEnv: "1"})

	status, _ := get(t, runtime.ProbePort, "/readyz")
	assert.Equal(t, http.StatusOK, status)

	var dialer net.Dialer
	conn, err := dialer.DialContext(t.Context(), "tcp", "127.0.0.1:"+strconv.Itoa(runtime.HTTPPort))
	if err == nil {
		_ = conn.Close()
	}
	require.Error(t, err)
}

func TestNotReadyWhileDraining(t *testing.T) {
	child := startChild(t, nil)

	// A request still in flight holds the drain open.
	var dialer net.Dialer
	conn, err := dialer.DialContext(t.Context(), "tcp", "127.0.0.1:"+strconv.Itoa(runtime.HTTPPort))
	require.NoError(t, err)
	_, err = conn.Write([]byte("GET /config HTTP/1.1\r\nHost: x\r\n"))
	require.NoError(t, err)

	require.NoError(t, child.cmd.Process.Signal(syscall.SIGTERM))
	assert.Eventually(t, func() bool {
		status, _, getErr := tryGet(t.Context(), runtime.ProbePort, "/readyz")
		return getErr == nil && status == http.StatusServiceUnavailable
	}, 5*time.Second, 20*time.Millisecond)

	require.NoError(t, conn.Close())
	child.stop(t)
}

type child struct {
	cmd    *exec.Cmd
	stderr *syncBuffer
	once   sync.Once
}

// startChild runs the test binary as a service with the chart's minimal
// environment plus env, and waits until its probe listener answers.
func startChild(t *testing.T, env map[string]string) *child {
	t.Helper()

	// t.Context is cancelled before cleanups run, which would kill the child
	// ahead of its clean stop.
	cmd := exec.CommandContext(context.WithoutCancel(t.Context()), os.Args[0])
	c := &child{cmd: cmd, stderr: &syncBuffer{}}
	c.cmd.Env = append(os.Environ(), childEnv+"=1", "OTEL_SERVICE_NAME=kit-test")
	for k, v := range env {
		c.cmd.Env = append(c.cmd.Env, k+"="+v)
	}
	c.cmd.Stderr = c.stderr
	require.NoError(t, c.cmd.Start())
	t.Cleanup(func() { c.stop(t) })

	require.Eventually(t, func() bool {
		status, _, err := tryGet(t.Context(), runtime.ProbePort, "/healthz")
		return err == nil && status == http.StatusOK
	}, 10*time.Second, 20*time.Millisecond, "child did not start: %s", c.stderr)
	return c
}

// stop sends SIGTERM, as the kubelet does, and requires a clean exit.
func (c *child) stop(t *testing.T) {
	t.Helper()
	c.once.Do(func() {
		require.NoError(t, c.cmd.Process.Signal(syscall.SIGTERM))
		require.NoError(t, c.cmd.Wait(), "child stderr: %s", c.stderr)
	})
}

func (c *child) logLine(t *testing.T, msg string) map[string]any {
	t.Helper()
	for raw := range strings.SplitSeq(c.stderr.String(), "\n") {
		var line map[string]any
		if json.Unmarshal([]byte(raw), &line) == nil && line["msg"] == msg {
			return line
		}
	}
	require.Failf(t, "log line not found", "msg %q in: %s", msg, c.stderr)
	return nil
}

func get(t *testing.T, port int, path string) (int, string) {
	t.Helper()
	status, body, err := tryGet(t.Context(), port, path)
	require.NoError(t, err)
	return status, body
}

func tryGet(ctx context.Context, port int, path string) (int, string, error) {
	url := "http://127.0.0.1:" + strconv.Itoa(port) + path
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, "", err
	}
	// No keep-alive: an idle connection would outlive the child it reached.
	req.Close = true
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body), err
}

// collector is an in-process OTLP gRPC trace receiver.
type collector struct {
	coltracepb.UnimplementedTraceServiceServer

	addr  string
	mu    sync.Mutex
	names []string
}

func startCollector(t *testing.T) *collector {
	t.Helper()
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)

	c := &collector{addr: ln.Addr().String()}
	srv := grpc.NewServer()
	coltracepb.RegisterTraceServiceServer(srv, c)
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(srv.Stop)
	return c
}

func (c *collector) Export(
	_ context.Context, req *coltracepb.ExportTraceServiceRequest,
) (*coltracepb.ExportTraceServiceResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, rs := range req.GetResourceSpans() {
		for _, attr := range rs.GetResource().GetAttributes() {
			if attr.GetKey() == "service.name" {
				c.names = append(c.names, attr.GetValue().GetStringValue())
			}
		}
	}
	return &coltracepb.ExportTraceServiceResponse{}, nil
}

func (c *collector) serviceNames() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.names...)
}

// syncBuffer is a [bytes.Buffer] the child's stderr copier and the test can
// share.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
