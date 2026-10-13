// Package runtime implements the platform's runtime contract once: the
// configuration names the metsa-service chart injects, a probe listener on
// 8081, a metrics listener on 9464, OTel tracing named from configuration
// and JSON logging. A service's main loads the runtime, registers its
// readiness checks and serves on 8080.
package runtime

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
)

const (
	readHeaderTimeout = 10 * time.Second
	// drainTimeout is how long in-flight requests get after the context is
	// done; it must stay under the pod's termination grace period.
	drainTimeout = 10 * time.Second
	flushTimeout = 5 * time.Second
)

// Runtime is one service process on the contract.
type Runtime struct {
	// Config is the contract's configuration as loaded by New.
	Config Config
	// Logger is the process logger. Pass it on; there is no global logger.
	Logger *slog.Logger

	flushTelemetry func(context.Context) error
	mu             sync.Mutex
	checks         []readyCheck
	stopping       atomic.Bool
}

// New loads the configuration, builds the logger and sets up tracing and
// metrics. It fails when a required name is missing, so a misconfigured pod
// never starts serving.
func New(ctx context.Context) (*Runtime, error) {
	cfg, err := loadConfig()
	if err != nil {
		return nil, err
	}
	logger := newLogger(os.Stderr, cfg)
	flush, err := initTelemetry(ctx, logger)
	if err != nil {
		return nil, err
	}
	return &Runtime{Config: cfg, Logger: logger, flushTelemetry: flush}, nil
}

// Serve listens on the contract's ports until ctx is done or a listener
// fails: handler on 8080, the probes on 8081 and the metrics on 9464. A nil
// handler serves no traffic port, for a worker that only consumes. On the
// way out it drains in-flight requests and flushes telemetry. It returns nil
// after a clean stop.
//
// handler is served as given; tracing its routes (otelhttp, otelecho) is the
// service's choice.
func (r *Runtime) Serve(ctx context.Context, handler http.Handler) error {
	metrics := http.NewServeMux()
	metrics.Handle("GET /metrics", promhttp.Handler())

	// Drained in this order: traffic first, while the probe listener still
	// answers "not ready".
	var servers []*http.Server
	if handler != nil {
		servers = append(servers, newServer(HTTPPort, handler))
	}
	servers = append(servers, newServer(MetricsPort, metrics), newServer(ProbePort, r.probeHandler()))

	listeners, err := listenAll(ctx, servers)
	if err != nil {
		return err
	}
	serveErrs := make(chan error, len(servers))
	for i, srv := range servers {
		go func() { serveErrs <- srv.Serve(listeners[i]) }()
	}
	r.Logger.InfoContext(ctx, "serving", "traffic", handler != nil)

	var serveErr error
	select {
	case <-ctx.Done():
	case serveErr = <-serveErrs:
	}
	r.stopping.Store(true)
	r.Logger.InfoContext(ctx, "stopping")

	// ctx is already done on the normal path; the drain needs its own time.
	stopCtx := context.WithoutCancel(ctx)
	errs := []error{serveErr}
	drainCtx, cancelDrain := context.WithTimeout(stopCtx, drainTimeout)
	defer cancelDrain()
	for _, srv := range servers {
		if shutdownErr := srv.Shutdown(drainCtx); shutdownErr != nil {
			errs = append(errs, fmt.Errorf("shut down %s: %w", srv.Addr, shutdownErr))
		}
	}
	flushCtx, cancelFlush := context.WithTimeout(stopCtx, flushTimeout)
	defer cancelFlush()
	if flushErr := r.flushTelemetry(flushCtx); flushErr != nil {
		errs = append(errs, fmt.Errorf("flush telemetry: %w", flushErr))
	}
	return errors.Join(errs...)
}

func newServer(port int, handler http.Handler) *http.Server {
	// No WriteTimeout: a service may stream (SSE) on the traffic port.
	return &http.Server{
		Addr:              ":" + strconv.Itoa(port),
		Handler:           handler,
		ReadHeaderTimeout: readHeaderTimeout,
	}
}

// listenAll binds every server's address up front, so a taken port fails
// Serve instead of being logged from a goroutine.
func listenAll(ctx context.Context, servers []*http.Server) ([]net.Listener, error) {
	listeners := make([]net.Listener, 0, len(servers))
	for _, srv := range servers {
		var lc net.ListenConfig
		ln, err := lc.Listen(ctx, "tcp", srv.Addr)
		if err != nil {
			for _, open := range listeners {
				_ = open.Close()
			}
			return nil, fmt.Errorf("listen on %s: %w", srv.Addr, err)
		}
		listeners = append(listeners, ln)
	}
	return listeners, nil
}
