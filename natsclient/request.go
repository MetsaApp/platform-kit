package natsclient

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/micro"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// The request outcomes, as recorded in nats_requests_total{outcome}.
var (
	// ErrNoDeadline is returned for a context without a deadline: a request
	// with no bound would hang a handler forever on a lost reply.
	ErrNoDeadline = errors.New("natsclient: request context needs a deadline")
	// ErrNoResponders means nobody is subscribed to the subject: the target
	// service is down or not deployed, which is a different problem from a
	// slow one. It wraps nats.ErrNoResponders.
	ErrNoResponders = errors.New("natsclient: no responders")
	// ErrTimeout means the deadline passed with no reply: the target is
	// there but slow, or the reply was lost. It wraps
	// [context.DeadlineExceeded].
	ErrTimeout = errors.New("natsclient: request timed out")
)

// ServiceError is an endpoint's error response, the micro protocol's
// Nats-Service-Error-Code and Nats-Service-Error headers. A handler returns
// one to choose the code the caller sees; any other error is reported as
// code 500 with the error's text.
type ServiceError struct {
	Code        string
	Description string
}

func (e *ServiceError) Error() string {
	return "natsclient: service error " + e.Code + ": " + e.Description
}

// Outcome values, one per error the caller can tell apart.
const (
	outcomeOK           = "ok"
	outcomeNoResponders = "no_responders"
	outcomeTimeout      = "timeout"
	outcomeError        = "error"
)

// Request sends data to subject and waits for the reply until ctx's
// deadline, which it must have. The caller's trace context goes along as a
// W3C traceparent header, so the handler's span continues the caller's
// trace. hdr may be nil; it is not modified.
//
// The error says what went wrong in a way the caller can act on:
// ErrNoResponders when nobody serves the subject, ErrTimeout when the
// deadline passed, a *ServiceError when the endpoint answered with an error
// response, and anything else wrapped as is. Each request is counted in
// nats_requests_total{service,endpoint,outcome} and timed in
// nats_request_duration_seconds.
func (c *Client) Request(ctx context.Context, subject string, data []byte, hdr nats.Header) (*nats.Msg, error) {
	if _, ok := ctx.Deadline(); !ok {
		return nil, fmt.Errorf("%w: %s", ErrNoDeadline, subject)
	}
	ctx, span := c.tracer.Start(ctx, subject,
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(attribute.String("messaging.system", "nats"),
			attribute.String("messaging.destination.name", subject)))
	defer span.End()

	msg := &nats.Msg{Subject: subject, Data: data, Header: maps.Clone(hdr)}
	if msg.Header == nil {
		msg.Header = nats.Header{}
	}
	c.propagator.Inject(ctx, propagation.HeaderCarrier(msg.Header))

	start := time.Now()
	reply, err := c.nc.RequestMsgWithContext(ctx, msg)
	outcome, err := classify(subject, reply, err)
	elapsed := time.Since(start)

	attrs := metric.WithAttributes(
		attribute.String("service", c.service),
		attribute.String("endpoint", subject),
		attribute.String("outcome", outcome),
	)
	c.requests.Add(ctx, 1, attrs)
	c.latency.Record(ctx, elapsed.Seconds(), attrs)
	span.SetAttributes(attribute.String("nats.outcome", outcome))
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, outcome)
		return nil, err
	}
	return reply, nil
}

// classify sorts a request's result into its outcome and the error the
// caller gets, including an error response from the endpoint, which nats.go
// delivers as a plain reply with the micro error headers set.
func classify(subject string, reply *nats.Msg, err error) (string, error) {
	switch {
	case err == nil:
		if code := reply.Header.Get(micro.ErrorCodeHeader); code != "" {
			return outcomeError, &ServiceError{Code: code, Description: reply.Header.Get(micro.ErrorHeader)}
		}
		return outcomeOK, nil
	case errors.Is(err, nats.ErrNoResponders):
		return outcomeNoResponders, fmt.Errorf("%w: %s: %w", ErrNoResponders, subject, err)
	case errors.Is(err, context.DeadlineExceeded):
		return outcomeTimeout, fmt.Errorf("%w: %s: %w", ErrTimeout, subject, err)
	default:
		return outcomeError, fmt.Errorf("natsclient: request %s: %w", subject, err)
	}
}
