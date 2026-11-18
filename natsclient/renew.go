package natsclient

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"time"
)

// DefaultCredentialRenewal is how long after each (re)connect a client with
// a TokenFile renews its credential: ten minutes inside the auth callout's
// one-hour JWT lifetime, which leaves room for the renewal to wait for the
// requests in flight.
const DefaultCredentialRenewal = 50 * time.Minute

const (
	// renewWait bounds each of the renewal's waits: for the endpoints'
	// handlers to finish and for an instant with no request in flight.
	renewWait = 30 * time.Second
	// renewQuiet is how long the drained endpoints must stay idle before
	// the renewal counts them as done: a request the server delivered
	// before the drain can still sit in the subscription's queue for a
	// moment between two handlers.
	renewQuiet = 100 * time.Millisecond
	// renewPoll is how often the renewal looks for idle handlers.
	renewPoll = 5 * time.Millisecond
	// renewRequestWait bounds how long the renewal holds new Requests back
	// while it waits for the ones in flight, so a request that waits on
	// Stripe still gets its reply. One that takes longer loses its reply to
	// the reconnect, as it would have to the expiry.
	renewRequestWait = 5 * time.Second
)

// WithCredentialRenewal sets how long after each (re)connect the client
// renews its credential; 0 turns renewal off. The default is
// DefaultCredentialRenewal with a TokenFile and off without one.
//
// Why: the server closes a connection when the callout's credential
// expires, and a reply to a request that arrived on the closed connection
// is lost. The server grants a responder its reply only through the
// response permission of the connection the request came in on, so the
// reply published after the reconnect is refused ("Publish Violation"),
// and a requester's reply that arrives while it is disconnected has no
// subscriber. Either way the caller times out, though the handler ran
// (COR-1588).
//
// A renewal therefore drains first: it stops this client's services
// (their queue group's other members take the requests), lets their
// handlers answer what already arrived, holds new Requests back for at
// most five seconds while the ones in flight finish, reconnects at once
// with a fresh credential, and registers the services again. Whatever is
// still running when a bound runs out loses its reply, as it would have to
// the expiry. Subscriptions this package does not own, such as JetStream
// consumers on Conn, are carried over by the reconnect as before.
func WithCredentialRenewal(after time.Duration) Option {
	return func(o *options) { o.renewAfter = &after }
}

func renewAfter(cfg Config, o options) time.Duration {
	if o.renewAfter != nil {
		return *o.renewAfter
	}
	if cfg.TokenFile == "" {
		return 0
	}
	return DefaultCredentialRenewal
}

// renewal is the Client's credential renewal state.
type renewal struct {
	renewAfter time.Duration

	mu       sync.Mutex
	timer    *time.Timer
	gen      uint64
	stopped  bool
	services []*Service

	// handlers counts endpoint handlers running; started counts every one
	// that began, so a quiet wait can tell "idle throughout" from "idle
	// now".
	handlers atomic.Int64
	started  atomic.Uint64
	// gate holds Requests back while the renewal reconnects.
	gate requestGate
}

// armRenewal schedules the next renewal; it runs on every (re)connect.
func (c *Client) armRenewal() {
	if c.renewAfter <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stopped {
		return
	}
	if c.timer != nil {
		c.timer.Stop()
	}
	c.gen++
	gen := c.gen
	c.timer = time.AfterFunc(c.renewAfter, func() { c.renew(gen) })
}

func (c *Client) stopRenewal() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stopped = true
	if c.timer != nil {
		c.timer.Stop()
	}
}

func (c *Client) addService(s *Service) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.services = append(c.services, s)
}

func (c *Client) removeService(s *Service) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.services = slices.DeleteFunc(c.services, func(o *Service) bool { return o == s })
}

// renew reconnects with a fresh credential once nothing is in flight, if no
// reconnect happened since the timer was armed.
func (c *Client) renew(gen uint64) {
	c.mu.Lock()
	current := gen == c.gen && !c.stopped
	services := slices.Clone(c.services)
	nc := c.nc
	c.mu.Unlock()
	if !current || nc == nil || !nc.IsConnected() {
		return
	}
	start := time.Now()

	// Stop in reverse order: micro restores the connection's callbacks
	// each service wrapped, so they unwind in the order they were stacked.
	for _, s := range slices.Backward(services) {
		s.pause()
	}
	// The flush's round trip proves the server has processed the drains'
	// unsubscribes: no request reaches the stopped endpoints after it.
	if err := nc.FlushTimeout(renewWait); err != nil {
		c.log.Warn("nats renewal: flush failed, renewing anyway", "error", err)
	}
	drained := c.waitHandlersIdle(time.Now().Add(renewWait))
	requestsDrained := c.gate.close(renewRequestWait)
	err := nc.ForceReconnect()
	// New Requests are buffered until the new connection is up, which
	// sends the reply subscription before them.
	c.gate.release()
	// The subscriptions are sent as soon as the new connection is up.
	for _, s := range services {
		s.resume()
	}
	if err != nil {
		c.log.Warn("nats renewal: reconnect refused", "error", err)
		return
	}
	c.log.Info("nats credential renewed before expiry", "waited", time.Since(start),
		"endpoints_drained", drained, "requests_drained", requestsDrained)
}

// waitHandlersIdle waits until no handler has run for renewQuiet, or until
// deadline. It reports whether the handlers went idle.
func (c *Client) waitHandlersIdle(deadline time.Time) bool {
	for time.Now().Before(deadline) {
		before := c.started.Load()
		if c.handlers.Load() == 0 {
			time.Sleep(renewQuiet)
			if c.handlers.Load() == 0 && c.started.Load() == before {
				return true
			}
			continue
		}
		time.Sleep(renewPoll)
	}
	return false
}

// requestGate lets the renewal wait for the Requests in flight while it
// holds new ones back, each wait bounded, so a Request never sends on a
// connection the renewal is about to close.
type requestGate struct {
	mu   sync.Mutex
	n    int
	hold chan struct{} // closed when the renewal releases new Requests
	idle chan struct{} // closed when the last Request in flight leaves
}

// enter waits while a renewal holds new Requests back, until ctx is done.
func (g *requestGate) enter(ctx context.Context) error {
	g.mu.Lock()
	for g.hold != nil {
		hold := g.hold
		g.mu.Unlock()
		select {
		case <-hold:
		case <-ctx.Done():
			return fmt.Errorf("natsclient: waiting out a credential renewal: %w", ctx.Err())
		}
		g.mu.Lock()
	}
	g.n++
	g.mu.Unlock()
	return nil
}

func (g *requestGate) leave() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.n--
	if g.n == 0 && g.idle != nil {
		close(g.idle)
		g.idle = nil
	}
}

// close holds new Requests back and waits up to wait for the ones in
// flight to finish. It reports whether they did; release must follow
// either way.
func (g *requestGate) close(wait time.Duration) bool {
	g.mu.Lock()
	g.hold = make(chan struct{})
	if g.n == 0 {
		g.mu.Unlock()
		return true
	}
	idle := make(chan struct{})
	g.idle = idle
	g.mu.Unlock()
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case <-idle:
		return true
	case <-t.C:
		return false
	}
}

func (g *requestGate) release() {
	g.mu.Lock()
	defer g.mu.Unlock()
	close(g.hold)
	g.hold, g.idle = nil, nil
}
