package natsclient_test

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/MetsaApp/platform-kit/natsclient"
)

const (
	serverStart    = 5 * time.Second
	requestTimeout = 2 * time.Second
	identity       = "metsa-test/api"
)

// startServer runs an embedded core NATS server. With users it enforces
// their publish and subscribe permissions, the way the auth callout's
// grants do in the cluster.
func startServer(t *testing.T, users ...*natsserver.User) *natsserver.Server {
	t.Helper()
	srv, err := natsserver.NewServer(&natsserver.Options{Port: -1, NoLog: true, NoSigs: true, Users: users})
	require.NoError(t, err)
	srv.Start()
	require.True(t, srv.ReadyForConnections(serverStart), "nats server not ready")
	t.Cleanup(srv.Shutdown)
	return srv
}

// connect dials srv as id with a token file holding token, which is how a
// pod authenticates: the user is the identity, the password the projected
// token. An empty token connects without credentials.
func connect(
	t *testing.T, srv *natsserver.Server, id, token string, log *slog.Logger, opts ...natsclient.Option,
) *natsclient.Client {
	t.Helper()
	cfg := natsclient.Config{URL: srv.ClientURL(), Identity: id, ServiceName: "test"}
	if token != "" {
		cfg.TokenFile = filepath.Join(t.TempDir(), "token")
		require.NoError(t, os.WriteFile(cfg.TokenFile, []byte(token+"\n"), 0o600))
	}
	c, err := natsclient.Connect(t.Context(), cfg, log, opts...)
	require.NoError(t, err)
	t.Cleanup(c.Conn().Close)
	require.Eventually(t, c.Conn().IsConnected, serverStart, 10*time.Millisecond)
	return c
}

// echo is an endpoint handler that answers with the request data.
func echo(_ context.Context, req natsclient.Request) error {
	return req.Respond(req.Data())
}

// serve registers a service with one echo endpoint and returns its subject.
func serve(t *testing.T, c *natsclient.Client, service, endpoint string, opts ...natsclient.EndpointOption) string {
	t.Helper()
	svc, err := c.Service(service, "1.0.0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Stop() })
	require.NoError(t, svc.AddEndpoint(endpoint, echo, opts...))
	return natsclient.EndpointSubject(service, endpoint)
}

func request(t *testing.T, c *natsclient.Client, subject string, data string) (*nats.Msg, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), requestTimeout)
	defer cancel()
	return c.Request(ctx, subject, []byte(data), nil)
}

// logBuffer collects log lines from nats.go's callback goroutines.
type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *logBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestInboxPrefix(t *testing.T) {
	for _, tc := range []struct{ identity, prefix string }{
		{"metsa-production/api", "_INBOX_metsa-production_api"},
		{"a.b/c.d", "_INBOX_a_b_c_d"},
	} {
		prefix, err := natsclient.InboxPrefix(tc.identity)
		require.NoError(t, err)
		assert.Equal(t, tc.prefix, prefix)
	}
	for _, bad := range []string{"", "no-slash", "/sa", "ns/"} {
		_, err := natsclient.InboxPrefix(bad)
		assert.Error(t, err, bad)
	}
}

func TestConnectNeedsIdentityForPrivateInbox(t *testing.T) {
	srv := startServer(t)
	_, err := natsclient.Connect(t.Context(), natsclient.Config{URL: srv.ClientURL()}, nil)
	require.ErrorContains(t, err, "WithSharedInbox")

	c := connect(t, srv, "", "", nil, natsclient.WithSharedInbox())
	assert.True(t, strings.HasPrefix(c.Conn().NewRespInbox(), "_INBOX."))
}

// The private inbox: each identity's grant allows its own prefix and nothing
// else, so a request's reply is readable by its sender only, and the
// responder, which publishes replies, cannot subscribe to any inbox.
func TestPrivateInboxIsolation(t *testing.T) {
	const idA, idB, idSvc = "metsa-test/a", "metsa-test/b", "metsa-test/orders"
	subject := natsclient.EndpointSubject("orders", "get")
	inboxA, _ := natsclient.InboxPrefix(idA)
	inboxB, _ := natsclient.InboxPrefix(idB)
	caller := func(name, inbox string) *natsserver.User {
		return &natsserver.User{Username: name, Password: "tok-" + name, Permissions: &natsserver.Permissions{
			Publish:   &natsserver.SubjectPermission{Allow: []string{"orders.svc.>", inbox + ".>"}},
			Subscribe: &natsserver.SubjectPermission{Allow: []string{inbox + ".>"}},
		}}
	}
	srv := startServer(t, caller(idA, inboxA), caller(idB, inboxB), &natsserver.User{
		Username: idSvc, Password: "tok-" + idSvc, Permissions: &natsserver.Permissions{
			Publish:   &natsserver.SubjectPermission{Allow: []string{inboxA + ".>", inboxB + ".>", "$SRV.>"}},
			Subscribe: &natsserver.SubjectPermission{Allow: []string{"orders.svc.>", "$SRV.>"}},
		},
	})
	serve(t, connect(t, srv, idSvc, "tok-"+idSvc, nil), "orders", "get")

	var logs logBuffer
	a := connect(t, srv, idA, "tok-"+idA, nil)
	b := connect(t, srv, idB, "tok-"+idB, slog.New(slog.NewTextHandler(&logs, nil)))
	assert.True(t, strings.HasPrefix(a.Conn().NewRespInbox(), inboxA+"."))

	// B tries to read A's replies: the server refuses the subscription.
	eavesdrop, err := b.Conn().SubscribeSync(inboxA + ".>")
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		return strings.Contains(logs.String(), "permissions violation")
	}, serverStart, 10*time.Millisecond)

	reply, err := request(t, a, subject, "hello")
	require.NoError(t, err)
	assert.Equal(t, "hello", string(reply.Data))
	_, err = eavesdrop.NextMsg(100 * time.Millisecond)
	require.ErrorIs(t, err, nats.ErrTimeout, "B must not see A's reply")

	// B's own requests still work on B's own inbox.
	reply, err = request(t, b, subject, "hi")
	require.NoError(t, err)
	assert.Equal(t, "hi", string(reply.Data))
}
