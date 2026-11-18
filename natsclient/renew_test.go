package natsclient_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/jwt/v2"
	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nkeys"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/MetsaApp/platform-kit/natsclient"
)

const (
	calloutAccount = "APP"
	calloutUser    = "auth"
	calloutPass    = "auth"
	// responder and requester are the two identities of the expiry tests,
	// the way billing serves the api.
	responder = "metsa-test/billing"
	requester = "metsa-test/api"
	// shortLifetime is the responders' credential lifetime. The JWT's exp
	// is whole seconds, so the server drops the connection between one
	// and two seconds before this.
	shortLifetime = 3 * time.Second
)

// startCalloutServer runs an embedded server that authenticates every
// client through an auth callout, as the cluster does: the callout mints a
// user JWT with an expiry and the identity's grants, and a responder may
// publish a reply only through the response permission, which the server
// tracks per connection. lifetime says how long each identity's credential
// lasts.
func startCalloutServer(t *testing.T, lifetime func(identity string) time.Duration) *natsserver.Server {
	t.Helper()
	issuer, err := nkeys.CreateAccount()
	require.NoError(t, err)
	issuerPub, err := issuer.PublicKey()
	require.NoError(t, err)

	acc := natsserver.NewAccount(calloutAccount)
	srv, err := natsserver.NewServer(&natsserver.Options{
		Port: -1, NoLog: true, NoSigs: true,
		Accounts: []*natsserver.Account{acc},
		Users:    []*natsserver.User{{Username: calloutUser, Password: calloutPass, Account: acc}},
		AuthCallout: &natsserver.AuthCallout{
			Issuer: issuerPub, Account: calloutAccount, AuthUsers: []string{calloutUser},
		},
	})
	require.NoError(t, err)
	srv.Start()
	require.True(t, srv.ReadyForConnections(serverStart), "nats server not ready")
	t.Cleanup(srv.Shutdown)

	nc, err := nats.Connect(srv.ClientURL(), nats.UserInfo(calloutUser, calloutPass))
	require.NoError(t, err)
	t.Cleanup(nc.Close)
	_, err = nc.Subscribe("$SYS.REQ.USER.AUTH", func(m *nats.Msg) {
		rc, decErr := jwt.DecodeAuthorizationRequestClaims(string(m.Data))
		if decErr != nil {
			return
		}
		id := rc.ConnectOptions.Username
		uc := jwt.NewUserClaims(rc.UserNkey)
		uc.Name = id
		uc.Audience = calloutAccount
		uc.Permissions = grants(t, id)
		uc.Expires = time.Now().Add(lifetime(id)).Unix()
		resp := jwt.NewAuthorizationResponseClaims(rc.UserNkey)
		resp.Audience = rc.Server.ID
		resp.Jwt, err = uc.Encode(issuer)
		if err != nil {
			resp.Error = err.Error()
		}
		out, encErr := resp.Encode(issuer)
		if encErr == nil {
			_ = m.Respond([]byte(out))
		}
	})
	require.NoError(t, err)
	require.NoError(t, nc.Flush())
	return srv
}

// grants are the callout's permissions for the two test identities: the
// responder serves expiry.svc.> and replies only through the response
// permission; the requester calls it and reads its private inbox.
func grants(t *testing.T, identity string) jwt.Permissions {
	t.Helper()
	inbox, err := natsclient.InboxPrefix(identity)
	require.NoError(t, err)
	var p jwt.Permissions
	p.Sub.Allow.Add(inbox + ".>")
	switch identity {
	case responder:
		p.Sub.Allow.Add("expiry.svc.>", "$SRV.>")
		p.Resp = &jwt.ResponsePermission{MaxMsgs: 1}
	case requester:
		p.Pub.Allow.Add("expiry.svc.>")
	}
	return p
}

// slowEcho answers after a pause, the way a billing handler waits on its
// database and Stripe, so a request is in flight most of the time.
func slowEcho(pause time.Duration) natsclient.Handler {
	return func(_ context.Context, req natsclient.Request) error {
		time.Sleep(pause)
		return req.Respond(req.Data())
	}
}

// callThrough sends one request after another to subject for d and
// returns every failure.
func callThrough(t *testing.T, c *natsclient.Client, subject string, d time.Duration) (int, []error) {
	t.Helper()
	var failed []error
	n := 0
	for end := time.Now().Add(d); time.Now().Before(end); n++ {
		data := strconv.Itoa(n)
		reply, err := request(t, c, subject, data)
		switch {
		case err != nil:
			failed = append(failed, err)
		case string(reply.Data) != data:
			failed = append(failed, fmt.Errorf("reply %q to request %q", reply.Data, data))
		}
	}
	return n, failed
}

// expiryRun serves expiry.svc.echo from two replicas whose credentials
// expire every few seconds, as billing's two pods do every hour, and calls
// it for long enough that each replica's credential expires at least
// twice. It returns how many requests were sent and every failure.
func expiryRun(t *testing.T, opts ...natsclient.Option) (int, []error) {
	t.Helper()
	srv := startCalloutServer(t, func(id string) time.Duration {
		if id == responder {
			return shortLifetime
		}
		return time.Hour
	})
	logs := &logBuffer{}
	subject := natsclient.EndpointSubject("expiry", "echo")
	for range 2 {
		c := connect(t, srv, responder, "token", newLogger(logs), opts...)
		svc, err := c.Service("expiry", "1.0.0")
		require.NoError(t, err)
		t.Cleanup(func() { _ = svc.Stop() })
		require.NoError(t, svc.AddEndpoint("echo", slowEcho(150*time.Millisecond)))
		// Stagger the replicas, as pods that started apart are.
		time.Sleep(700 * time.Millisecond)
	}
	api := connect(t, srv, requester, "token", nil)
	n, failed := callThrough(t, api, subject, 3*shortLifetime)
	t.Logf("%d requests, %d failed", n, len(failed))
	t.Logf("responder logs:\n%s", logs.String())
	return n, failed
}

// TestReplyLostAcrossCredentialExpiry pins what COR-1588 saw: a responder
// whose credential expires mid-request answers on the new connection, the
// server refuses the reply (the response permission belonged to the old
// one), and the caller times out although the handler ran.
func TestReplyLostAcrossCredentialExpiry(t *testing.T) {
	_, failed := expiryRun(t, natsclient.WithCredentialRenewal(0))
	require.NotEmpty(t, failed, "every request survived the expiry; the test no longer reproduces the loss")
	for _, err := range failed {
		assert.True(t, errors.Is(err, natsclient.ErrTimeout) || errors.Is(err, natsclient.ErrNoResponders), "%v", err)
	}
}

// TestRenewalKeepsEveryReply is the fix: each replica renews its credential
// before it expires, after draining its endpoints, so no request is in
// flight on a connection that closes, and the other replica answers in the
// meantime.
func TestRenewalKeepsEveryReply(t *testing.T) {
	n, failed := expiryRun(t, natsclient.WithCredentialRenewal(time.Second))
	require.Greater(t, n, 20)
	assert.Empty(t, failed)
}

// TestRenewalKeepsRequesterReplies covers the other side: a requester
// whose own credential expires while it waits for a reply loses that
// reply, unless it renews between requests.
func TestRenewalKeepsRequesterReplies(t *testing.T) {
	srv := startCalloutServer(t, func(id string) time.Duration {
		if id == requester {
			return shortLifetime
		}
		return time.Hour
	})
	svc := connect(t, srv, responder, "token", nil)
	s, err := svc.Service("expiry", "1.0.0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Stop() })
	require.NoError(t, s.AddEndpoint("echo", slowEcho(150*time.Millisecond)))
	subject := natsclient.EndpointSubject("expiry", "echo")

	var wg sync.WaitGroup
	results := map[string][]error{}
	var mu sync.Mutex
	for name, renewal := range map[string]time.Duration{"without renewal": 0, "with renewal": time.Second} {
		wg.Go(func() {
			logs := &logBuffer{}
			api := connect(t, srv, requester, "token", newLogger(logs), natsclient.WithCredentialRenewal(renewal))
			_, failed := callThrough(t, api, subject, 3*shortLifetime)
			t.Logf("%s: %d failed, logs:\n%s", name, len(failed), logs.String())
			mu.Lock()
			results[name] = failed
			mu.Unlock()
		})
	}
	wg.Wait()
	assert.NotEmpty(t, results["without renewal"], "the requester's expiry no longer loses a reply")
	assert.Empty(t, results["with renewal"])
}

func newLogger(w *logBuffer) *slog.Logger {
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: slog.LevelDebug}))
}
