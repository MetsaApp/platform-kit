package natsclient

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/nats-io/nats.go/micro"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// serviceErrorCode is the code of an error response from a handler that
// did not choose one with a *ServiceError.
const serviceErrorCode = "500"

// EndpointSubject is the subject of a synchronous endpoint per ADR 0006:
// "<service>.svc.<endpoint>". Callers build their request subjects with
// it, so the convention lives in one place.
func EndpointSubject(service, endpoint string) string {
	return service + ".svc." + endpoint
}

// Request is one request to an endpoint: its data and headers, and the
// reply methods. A handler answers with Respond or RespondJSON, or returns
// an error; it does not do both.
type Request = micro.Request

// Handler serves one request. ctx carries the caller's trace, so spans and
// *Context log calls inside join it. A returned error becomes an error
// response: a *ServiceError chooses the code, anything else is code 500
// with the error's text, which the caller sees as a *ServiceError.
type Handler func(ctx context.Context, req Request) error

// EndpointOption tunes one endpoint.
type EndpointOption func(*endpointOptions)

type endpointOptions struct {
	queueGroup string
}

// WithQueueGroup overrides the endpoint's queue group, which is the
// service's name by default so every replica shares the load. A different
// group receives every request too, for an endpoint that must see all of
// them.
func WithQueueGroup(name string) EndpointOption {
	return func(o *endpointOptions) { o.queueGroup = name }
}

// Service is a set of request/reply endpoints under one service name. It is
// a nats.go micro service, so it also answers the $SRV.PING, $SRV.INFO and
// $SRV.STATS discovery subjects, which the nats CLI's "micro" commands read.
type Service struct {
	micro  micro.Service
	name   string
	client *Client
	log    *slog.Logger
}

// Service registers a micro service named name at version (SemVer) on the
// connection. Endpoints are added with AddEndpoint; Stop drains them.
func (c *Client) Service(name, version string) (*Service, error) {
	log := c.log.With("nats_service", name)
	svc, err := micro.AddService(c.nc, micro.Config{
		Name:       name,
		Version:    version,
		QueueGroup: name,
		ErrorHandler: func(_ micro.Service, natsErr *micro.NATSError) {
			log.Warn("nats service error", "subject", natsErr.Subject, "error", natsErr.Description)
		},
	})
	if err != nil {
		return nil, fmt.Errorf("natsclient: add service %s: %w", name, err)
	}
	return &Service{micro: svc, name: name, client: c, log: log}, nil
}

// AddEndpoint serves EndpointSubject(service, endpoint) with handler, in
// the service's queue group unless WithQueueGroup says otherwise. Requests
// to one endpoint are handled one at a time, in order, as nats.go delivers
// them; a slow handler is throttled by its own subscription, not the
// service's others.
func (s *Service) AddEndpoint(endpoint string, handler Handler, opts ...EndpointOption) error {
	o := endpointOptions{queueGroup: s.name}
	for _, opt := range opts {
		opt(&o)
	}
	subject := EndpointSubject(s.name, endpoint)
	err := s.micro.AddEndpoint(endpoint, micro.HandlerFunc(s.handle(subject, handler)),
		micro.WithEndpointSubject(subject), micro.WithEndpointQueueGroup(o.queueGroup))
	if err != nil {
		return fmt.Errorf("natsclient: add endpoint %s: %w", subject, err)
	}
	return nil
}

// Stop drains the endpoint subscriptions and answers no more requests.
func (s *Service) Stop() error {
	if err := s.micro.Stop(); err != nil {
		return fmt.Errorf("natsclient: stop service %s: %w", s.name, err)
	}
	return nil
}

// handle wraps handler for micro: it continues the caller's trace from the
// request headers in a server span and turns a returned error into an
// error response.
func (s *Service) handle(subject string, handler Handler) func(micro.Request) {
	return func(req micro.Request) {
		ctx := s.client.propagator.Extract(context.Background(), propagation.HeaderCarrier(req.Headers()))
		ctx, span := s.client.tracer.Start(ctx, subject,
			trace.WithSpanKind(trace.SpanKindServer),
			trace.WithAttributes(attribute.String("messaging.system", "nats"),
				attribute.String("messaging.destination.name", subject)))
		defer span.End()

		err := handler(ctx, req)
		if err == nil {
			return
		}
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		code, description := serviceErrorCode, err.Error()
		if serviceErr, ok := errors.AsType[*ServiceError](err); ok {
			code, description = serviceErr.Code, serviceErr.Description
		}
		s.log.WarnContext(ctx, "endpoint failed", "subject", subject, "code", code, "error", err)
		if respondErr := req.Error(code, description, nil); respondErr != nil {
			s.log.WarnContext(ctx, "error response not sent", "subject", subject, "error", respondErr)
		}
	}
}
