package natsclient

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"github.com/MetsaApp/platform-kit/runtime"
)

// scope names the OTel instruments and spans this package emits.
const scope = "github.com/MetsaApp/platform-kit/natsclient"

// pingInterval is how often the client pings the server; with nats.go's
// default of two missed pings, a wedged connection is noticed in under a
// minute rather than the default six.
const pingInterval = 20 * time.Second

// Config is what Connect needs. FromRuntime fills it from the runtime
// contract; a test fills it by hand.
type Config struct {
	// URL is the server to dial (NATS_URL).
	URL string
	// TokenFile holds the projected ServiceAccount token (NATS_TOKEN_FILE).
	// It rotates hourly and is read on every (re)connect. Empty connects
	// without credentials, for a dev server only.
	TokenFile string
	// Identity is <namespace>/<serviceaccount> (SERVICE_IDENTITY), the NATS
	// user and the owner of the private reply inbox.
	Identity string
	// ServiceName names the connection on the server and the service in the
	// request metrics (OTEL_SERVICE_NAME).
	ServiceName string
}

// FromRuntime maps the runtime contract's names onto a Config.
func FromRuntime(cfg runtime.Config) Config {
	return Config{
		URL:         cfg.NATSURL,
		TokenFile:   cfg.NATSTokenFile,
		Identity:    cfg.ServiceIdentity,
		ServiceName: cfg.ServiceName,
	}
}

// Option tunes Connect.
type Option func(*options)

type options struct {
	sharedInbox bool
	natsOpts    []nats.Option
}

// WithSharedInbox keeps nats.go's default _INBOX prefix instead of the
// identity's private one. Today's auth callout grants only _INBOX.>, so an
// identity whose grant does not yet include its private prefix (ENG-447)
// requests with this option; without it the reply subscription is refused
// and every request times out.
func WithSharedInbox() Option {
	return func(o *options) { o.sharedInbox = true }
}

// WithNATSOptions appends raw nats.go options, applied after this package's
// own, for the rare setting (TLS, a test hook) the Config does not cover.
func WithNATSOptions(opts ...nats.Option) Option {
	return func(o *options) { o.natsOpts = append(o.natsOpts, opts...) }
}

// InboxPrefix is the identity's private reply inbox prefix,
// "_INBOX_<ns>_<sa>", with dots inside the names turned into "_" so the
// prefix is one token. The auth callout grants the identity
// "<prefix>.>" and nobody else, which is what keeps one identity's replies
// out of another's reach on the shared account.
func InboxPrefix(identity string) (string, error) {
	ns, sa, ok := strings.Cut(identity, "/")
	if !ok || ns == "" || sa == "" {
		return "", fmt.Errorf("natsclient: identity %q: want <namespace>/<serviceaccount>", identity)
	}
	safe := func(s string) string { return strings.ReplaceAll(s, ".", "_") }
	return "_INBOX_" + safe(ns) + "_" + safe(sa), nil
}

// Client is one process's connection to NATS and the instruments its
// requests record into.
type Client struct {
	nc         *nats.Conn
	log        *slog.Logger
	service    string
	tracer     trace.Tracer
	propagator propagation.TextMapPropagator
	requests   metric.Int64Counter
	latency    metric.Float64Histogram
}

// Connect dials NATS as cfg.Identity, authenticating with the projected
// token at cfg.TokenFile, and returns once the connection is set up, not
// once it is established: an unreachable server is retried in the
// background rather than failing the caller.
//
// One connection serves a process for its lifetime. It reconnects forever
// with nats.go's paced, jittered sweep. The token is re-read on every
// (re)connect: both it and the credential the auth callout mints rotate
// hourly, and because a stale read is possible at the moment of rotation,
// repeated auth errors do not close the connection the way nats.go
// otherwise would. Every connection event is logged: the hourly expiry at
// Info, a disconnect or reconnect at Info, a closed connection at Error,
// since that one is permanent and the process should exit. A nil log
// discards.
//
// The reply inbox is the identity's private prefix (see InboxPrefix) unless
// WithSharedInbox says otherwise.
func Connect(ctx context.Context, cfg Config, log *slog.Logger, opts ...Option) (*Client, error) {
	if cfg.URL == "" {
		return nil, errors.New("natsclient: URL is empty (NATS_URL)")
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	var o options
	for _, opt := range opts {
		opt(&o)
	}
	natsOpts := connectOptions(cfg, log)
	inbox := "_INBOX"
	if !o.sharedInbox {
		prefix, err := InboxPrefix(cfg.Identity)
		if err != nil {
			return nil, fmt.Errorf("%w (set SERVICE_IDENTITY or use WithSharedInbox)", err)
		}
		inbox = prefix
		natsOpts = append(natsOpts, nats.CustomInboxPrefix(prefix))
	}

	c := &Client{log: log, service: cfg.ServiceName, tracer: otel.Tracer(scope), propagator: propagation.TraceContext{}}
	if err := c.initInstruments(); err != nil {
		return nil, err
	}
	nc, err := nats.Connect(cfg.URL, append(natsOpts, o.natsOpts...)...)
	if err != nil {
		return nil, fmt.Errorf("natsclient: connect: %w", err)
	}
	c.nc = nc
	log.InfoContext(ctx, "nats connecting", "url", cfg.URL, "identity", cfg.Identity, "inbox", inbox)
	return c, nil
}

// connectOptions is Connect's nats.go configuration. It is built outside
// Connect so the callbacks, which run for the life of the connection, do not
// close over Connect's context.
func connectOptions(cfg Config, log *slog.Logger) []nats.Option {
	opts := []nats.Option{
		nats.Name(cfg.ServiceName),
		nats.MaxReconnects(-1),
		nats.RetryOnFailedConnect(true),
		nats.IgnoreAuthErrorAbort(),
		nats.PingInterval(pingInterval),
		nats.ErrorHandler(asyncErrorLogger(log)),
		nats.DisconnectErrHandler(func(nc *nats.Conn, err error) {
			log.Info("nats disconnected, reconnecting", "error", err, "server", nc.ConnectedUrl())
		}),
		nats.ReconnectHandler(func(nc *nats.Conn) {
			log.Info("nats reconnected", "server", nc.ConnectedUrl())
		}),
		nats.ClosedHandler(func(nc *nats.Conn) {
			log.Error("nats connection closed for good", "error", nc.LastError())
		}),
	}
	if cfg.TokenFile != "" {
		opts = append(opts, nats.UserInfoHandler(func() (string, string) {
			tok, err := os.ReadFile(cfg.TokenFile)
			if err != nil {
				// Empty fails this attempt; nats.go keeps retrying and the
				// projected token will reappear.
				return cfg.Identity, ""
			}
			return cfg.Identity, strings.TrimSpace(string(tok))
		}))
	}
	return opts
}

// asyncErrorLogger replaces nats.go's default handler, which wrote a raw
// "nats: authentication expired on connection [N]" line to stderr that no
// log parser could read. The hourly expiry is by design, so it is Info; any
// other async error (slow consumer, permission violation) stays a Warn.
func asyncErrorLogger(log *slog.Logger) nats.ErrHandler {
	return func(nc *nats.Conn, sub *nats.Subscription, err error) {
		attrs := []any{"error", err}
		if nc != nil {
			attrs = append(attrs, "server", nc.ConnectedUrl())
		}
		if sub != nil {
			attrs = append(attrs, "subject", sub.Subject)
		}
		if errors.Is(err, nats.ErrAuthExpired) {
			log.Info("nats credential expired, reconnecting", attrs...)
			return
		}
		log.Warn("nats async error", attrs...)
	}
}

func (c *Client) initInstruments() error {
	meter := otel.Meter(scope)
	var err error
	c.requests, err = meter.Int64Counter("nats_requests_total",
		metric.WithDescription("Requests sent, by subject and outcome."))
	if err != nil {
		return fmt.Errorf("natsclient: request counter: %w", err)
	}
	c.latency, err = meter.Float64Histogram("nats_request_duration_seconds",
		metric.WithDescription("Request round trip, by subject and outcome."), metric.WithUnit("s"))
	if err != nil {
		return fmt.Errorf("natsclient: request histogram: %w", err)
	}
	return nil
}

// Conn is the underlying connection, for JetStream and anything else this
// package does not wrap.
func (c *Client) Conn() *nats.Conn { return c.nc }

// Connected is a readiness check: an error while the client has no server.
// The hourly credential expiry reconnects within a probe period, so only an
// outage that lasts fails the probe's threshold.
func (c *Client) Connected() func(context.Context) error {
	return func(context.Context) error {
		if !c.nc.IsConnected() {
			return fmt.Errorf("natsclient: connection is %s", c.nc.Status())
		}
		return nil
	}
}

// Drain shuts the connection down gracefully: no new messages are
// delivered, in-flight replies and publishes are flushed, and only then is
// the connection closed. It returns once the connection has closed or ctx is
// done, whichever comes first, so a SIGTERM handler waits for the work drain
// was meant to save rather than exiting on Drain's return.
func (c *Client) Drain(ctx context.Context) error {
	closed := make(chan struct{})
	c.nc.SetClosedHandler(func(*nats.Conn) { close(closed) })
	if c.nc.IsClosed() {
		return nil
	}
	if err := c.nc.Drain(); err != nil {
		return fmt.Errorf("natsclient: drain: %w", err)
	}
	select {
	case <-closed:
		return nil
	case <-ctx.Done():
		c.nc.Close()
		return fmt.Errorf("natsclient: drain: %w", ctx.Err())
	}
}
