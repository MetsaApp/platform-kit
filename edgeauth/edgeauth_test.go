package edgeauth_test

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/MetsaApp/platform-kit/edgeauth"
	"github.com/MetsaApp/platform-kit/edgeauth/edgeauthtest"
)

const host = "live.metsa.app"

var ada = edgeauth.Identity{ //nolint:gochecknoglobals // a fixture shared by the tests
	Subject: "u1", Email: "ada@example.org", EmailVerified: true, Name: "Ada",
	Roles: []string{"admin", "member"}, SessionID: "0123456789abcdef", ClientID: "c1",
	Source: edgeauth.SourceSession, AuthTime: time.Unix(1_700_000_000, 0),
}

// clock is a settable clock for WithClock.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// countingProxy forwards to the issuer's JWKS and counts the fetches.
func countingProxy(t *testing.T, iss *edgeauthtest.Issuer, fetches *atomic.Int32) string {
	t.Helper()
	target, err := url.Parse(iss.JWKSURL())
	require.NoError(t, err)
	proxy := httputil.NewSingleHostReverseProxy(target)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fetches.Add(1)
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/jwks.json"
}

// mintAt mints ada's token as issued at now, for a verifier on a test clock.
func mintAt(iss *edgeauthtest.Issuer, now time.Time) string {
	return iss.MintWith(ada, host, func(c *edgeauthtest.Claims) {
		c.IssuedAt, c.Expiry = now.Unix(), now.Add(edgeauthtest.Lifetime).Unix()
	})
}

func TestNewNeedsAudiences(t *testing.T) {
	t.Parallel()
	_, err := edgeauth.New(edgeauth.Config{JWKSURL: edgeauth.DefaultJWKSURL})
	require.ErrorIs(t, err, edgeauth.ErrConfig)
	_, err = edgeauth.New(edgeauth.Config{Audiences: []string{host}})
	require.ErrorIs(t, err, edgeauth.ErrConfig)
}

func TestConfigFromEnv(t *testing.T) {
	t.Setenv("PLATFORM_KIT_EDGEAUTH_AUDIENCES", "live.metsa.app,admin.live.metsa.app")
	cfg, err := edgeauth.ConfigFromEnv()
	require.NoError(t, err)
	assert.Equal(t, edgeauth.DefaultJWKSURL, cfg.JWKSURL)
	assert.Equal(t, []string{"live.metsa.app", "admin.live.metsa.app"}, cfg.Audiences)
}

func TestVerifyRoundTrip(t *testing.T) {
	t.Parallel()
	iss := edgeauthtest.NewIssuer(t)
	v, err := edgeauth.New(iss.Config(host))
	require.NoError(t, err)

	id, err := v.Verify(t.Context(), iss.Mint(ada, host))
	require.NoError(t, err)
	assert.Equal(t, ada, id)
	assert.True(t, id.HasRole("admin"))
	assert.False(t, id.HasRole("host"))
	assert.True(t, id.HasAnyRole("host", "member"))
	assert.False(t, id.HasAnyRole())

	_, err = v.Verify(t.Context(), iss.Mint(ada, "other.metsa.app"))
	require.ErrorIs(t, err, edgeauth.ErrInvalid)

	expired := iss.MintWith(ada, host, func(c *edgeauthtest.Claims) {
		c.IssuedAt -= 600
		c.Expiry -= 600
	})
	_, err = v.Verify(t.Context(), expired)
	require.ErrorIs(t, err, edgeauth.ErrInvalid)

	// A token from another issuer's key is unknown here.
	_, err = v.Verify(t.Context(), edgeauthtest.NewIssuer(t).Mint(ada, host))
	require.ErrorIs(t, err, edgeauth.ErrInvalid)
}

func TestNewDoesNotFetch(t *testing.T) {
	t.Parallel()
	iss := edgeauthtest.NewIssuer(t)
	var fetches atomic.Int32
	cfg := edgeauth.Config{JWKSURL: countingProxy(t, iss, &fetches), Audiences: []string{host}}
	v, err := edgeauth.New(cfg)
	require.NoError(t, err)
	assert.Zero(t, fetches.Load())

	_, err = v.Verify(t.Context(), iss.Mint(ada, host))
	require.NoError(t, err)
	assert.EqualValues(t, 1, fetches.Load())
}

func TestJWKSCache(t *testing.T) {
	t.Parallel()
	iss := edgeauthtest.NewIssuer(t)
	var fetches atomic.Int32
	clk := &clock{now: time.Now()}
	v, err := edgeauth.New(
		edgeauth.Config{JWKSURL: countingProxy(t, iss, &fetches), Audiences: []string{host}},
		edgeauth.WithClock(clk.Now),
	)
	require.NoError(t, err)
	verify := func() error {
		_, verr := v.Verify(t.Context(), mintAt(iss, clk.Now()))
		return verr
	}
	stranger := edgeauthtest.NewIssuer(t).Mint(ada, host) // signed by a key with the same kid, never published

	require.NoError(t, verify())
	require.NoError(t, verify())
	assert.EqualValues(t, 1, fetches.Load(), "a fresh cache is reused")

	clk.Advance(20 * time.Second)
	_, err = v.Verify(t.Context(), stranger)
	require.ErrorIs(t, err, edgeauth.ErrInvalid)
	assert.EqualValues(t, 1, fetches.Load(), "a known kid does not refetch")

	clk.Advance(4*time.Minute + 50*time.Second)
	require.NoError(t, verify())
	assert.EqualValues(t, 2, fetches.Load(), "an expired cache refetches")
}

func TestJWKSRefetchOnUnknownKid(t *testing.T) {
	t.Parallel()
	iss := edgeauthtest.NewIssuer(t)
	var fetches atomic.Int32
	proxy := countingProxy(t, iss, &fetches)
	// Serve a JWKS without the key first, as during a rotation that has not
	// reached this verifier's cache.
	var published atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !published.Load() {
			fetches.Add(1)
			_, _ = w.Write([]byte(`{"keys":[]}`))
			return
		}
		http.Redirect(w, r, proxy, http.StatusFound)
	}))
	t.Cleanup(srv.Close)
	clk := &clock{now: time.Now()}
	v, err := edgeauth.New(
		edgeauth.Config{JWKSURL: srv.URL, Audiences: []string{host}},
		edgeauth.WithClock(clk.Now),
	)
	require.NoError(t, err)
	token := iss.Mint(ada, host)

	_, err = v.Verify(t.Context(), token)
	require.ErrorIs(t, err, edgeauth.ErrInvalid)
	assert.EqualValues(t, 1, fetches.Load())

	published.Store(true)
	clk.Advance(10 * time.Second)
	_, err = v.Verify(t.Context(), token)
	require.ErrorIs(t, err, edgeauth.ErrInvalid, "an unknown kid refetches at most once per 30 s")
	assert.EqualValues(t, 1, fetches.Load())

	clk.Advance(20 * time.Second)
	_, err = v.Verify(t.Context(), token)
	require.NoError(t, err)
	assert.EqualValues(t, 2, fetches.Load())
}

func TestJWKSSingleFlight(t *testing.T) {
	t.Parallel()
	iss := edgeauthtest.NewIssuer(t)
	var fetches atomic.Int32
	proxy := countingProxy(t, iss, &fetches)
	release := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
		http.Redirect(w, r, proxy, http.StatusFound)
	}))
	t.Cleanup(slow.Close)
	v, err := edgeauth.New(edgeauth.Config{JWKSURL: slow.URL, Audiences: []string{host}})
	require.NoError(t, err)
	token := iss.Mint(ada, host)

	const callers = 20
	errs := make(chan error, callers)
	for range callers {
		go func() {
			_, verr := v.Verify(t.Context(), token)
			errs <- verr
		}()
	}
	time.Sleep(50 * time.Millisecond)
	close(release)
	for range callers {
		require.NoError(t, <-errs)
	}
	assert.EqualValues(t, 1, fetches.Load())
}

func TestJWKSUnavailable(t *testing.T) {
	t.Parallel()
	iss := edgeauthtest.NewIssuer(t)
	var fail atomic.Bool
	fail.Store(true)
	var fetches atomic.Int32
	proxy := countingProxy(t, iss, &fetches)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail.Load() {
			http.Error(w, "down", http.StatusBadGateway)
			return
		}
		http.Redirect(w, r, proxy, http.StatusFound)
	}))
	t.Cleanup(srv.Close)
	clk := &clock{now: time.Now()}
	v, err := edgeauth.New(
		edgeauth.Config{JWKSURL: srv.URL, Audiences: []string{host}},
		edgeauth.WithClock(clk.Now),
	)
	require.NoError(t, err)
	token := iss.Mint(ada, host)

	_, err = v.Verify(t.Context(), token)
	require.ErrorIs(t, err, edgeauth.ErrUnavailable)

	fail.Store(false)
	_, err = v.Verify(t.Context(), token)
	require.ErrorIs(t, err, edgeauth.ErrUnavailable, "a failed fetch is not retried at once")

	clk.Advance(5 * time.Second)
	_, err = v.Verify(t.Context(), token)
	require.NoError(t, err)

	// Once the cache expires, an outage still verifies with the keys it had.
	fail.Store(true)
	clk.Advance(6 * time.Minute)
	_, err = v.Verify(t.Context(), mintAt(iss, clk.Now()))
	require.NoError(t, err)
}

func serve(
	t *testing.T, v *edgeauth.Verifier, guard func(http.Handler) http.Handler, r *http.Request,
) *httptest.ResponseRecorder {
	t.Helper()
	h := http.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, ok := edgeauth.FromContext(r.Context())
		if !ok {
			_, _ = w.Write([]byte("anonymous"))
			return
		}
		_, _ = w.Write([]byte(id.Subject))
	}))
	if guard != nil {
		h = guard(h)
	}
	rec := httptest.NewRecorder()
	v.Middleware(h).ServeHTTP(rec, r)
	return rec
}

func TestMiddleware(t *testing.T) {
	t.Parallel()
	iss := edgeauthtest.NewIssuer(t)
	var logs bytes.Buffer
	v, err := edgeauth.New(iss.Config(host), edgeauth.WithLogger(slog.New(slog.NewJSONHandler(&logs, nil))))
	require.NoError(t, err)
	req := func() *http.Request {
		return httptest.NewRequest(http.MethodGet, "https://live.metsa.app:443/admin", nil)
	}

	rec := serve(t, v, nil, req())
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "anonymous", rec.Body.String())

	r := req()
	iss.Sign(r, ada)
	rec = serve(t, v, nil, r)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "u1", rec.Body.String())

	r = req()
	token := iss.Mint(ada, "other.metsa.app")
	r.Header.Set(edgeauth.Header, token)
	rec = serve(t, v, nil, r)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.Equal(t, "text/plain; charset=utf-8", rec.Header().Get("Content-Type"))
	assert.NotContains(t, rec.Body.String(), token)
	assert.Contains(t, logs.String(), "identity token refused")
	assert.NotContains(t, logs.String(), token)
}

func TestMiddlewareUnavailable(t *testing.T) {
	t.Parallel()
	iss := edgeauthtest.NewIssuer(t)
	down := httptest.NewServer(http.NotFoundHandler())
	down.Close()
	v, err := edgeauth.New(edgeauth.Config{JWKSURL: down.URL, Audiences: []string{host}})
	require.NoError(t, err)
	r := httptest.NewRequest(http.MethodGet, "https://live.metsa.app/", nil)
	iss.Sign(r, ada)
	rec := serve(t, v, nil, r)
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
}

func TestRequire(t *testing.T) {
	t.Parallel()
	iss := edgeauthtest.NewIssuer(t)
	v, err := edgeauth.New(iss.Config(host))
	require.NoError(t, err)
	member := ada
	member.Roles = []string{"member"}
	tests := []struct {
		name  string
		guard func(http.Handler) http.Handler
		id    *edgeauth.Identity
		want  int
	}{
		{"identity, anonymous", edgeauth.RequireIdentity, nil, http.StatusUnauthorized},
		{"identity, signed in", edgeauth.RequireIdentity, &member, http.StatusOK},
		{"role, anonymous", edgeauth.RequireRole("admin"), nil, http.StatusUnauthorized},
		{"role, missing", edgeauth.RequireRole("admin", "host"), &member, http.StatusForbidden},
		{"role, any of", edgeauth.RequireRole("host", "member"), &member, http.StatusOK},
		{"no roles, signed in", edgeauth.RequireRole(), &member, http.StatusOK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r := httptest.NewRequest(http.MethodGet, "https://live.metsa.app/admin", nil)
			if tt.id != nil {
				iss.Sign(r, *tt.id)
			}
			assert.Equal(t, tt.want, serve(t, v, tt.guard, r).Code)
		})
	}
}

func TestContextWith(t *testing.T) {
	t.Parallel()
	_, ok := edgeauth.FromContext(t.Context())
	assert.False(t, ok)
	id, ok := edgeauth.FromContext(edgeauth.ContextWith(t.Context(), ada))
	assert.True(t, ok)
	assert.Equal(t, ada, id)
}
