package natsclient

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"

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
//
// A credential renewal stops the micro service and registers it again with
// the same endpoints (see WithCredentialRenewal), so its ID and endpoint
// stats start over once an hour.
type Service struct {
	name    string
	version string
	client  *Client
	log     *slog.Logger

	mu        sync.Mutex
	micro     micro.Service // nil while paused
	endpoints []endpoint
	stopped   bool
}

// endpoint is what AddEndpoint registered, kept to register it again.
type endpoint struct {
	name    string
	subject string
	queue   string
	handler micro.HandlerFunc
}

// Service registers a micro service named name at version (SemVer) on the
// connection. Endpoints are added with AddEndpoint; Stop drains them.
func (c *Client) Service(name, version string) (*Service, error) {
	s := &Service{name: name, version: version, client: c, log: c.log.With("nats_service", name)}
	m, err := s.start()
	if err != nil {
		return nil, err
	}
	s.micro = m
	c.addService(s)
	return s, nil
}

// start adds the micro service and every endpoint registered so far.
func (s *Service) start() (micro.Service, error) {
	m, err := micro.AddService(s.client.nc, micro.Config{
		Name:       s.name,
		Version:    s.version,
		QueueGroup: s.name,
		ErrorHandler: func(_ micro.Service, natsErr *micro.NATSError) {
			s.log.Warn("nats service error", "subject", natsErr.Subject, "error", natsErr.Description)
		},
	})
	if err != nil {
		return nil, fmt.Errorf("natsclient: add service %s: %w", s.name, err)
	}
	for _, e := range s.endpoints {
		if err = addEndpoint(m, e); err != nil {
			_ = m.Stop()
			return nil, err
		}
	}
	return m, nil
}

func addEndpoint(m micro.Service, e endpoint) error {
	err := m.AddEndpoint(e.name, e.handler, micro.WithEndpointSubject(e.subject), micro.WithEndpointQueueGroup(e.queue))
	if err != nil {
		return fmt.Errorf("natsclient: add endpoint %s: %w", e.subject, err)
	}
	return nil
}

// AddEndpoint serves EndpointSubject(service, endpoint) with handler, in
// the service's queue group unless WithQueueGroup says otherwise. Requests
// to one endpoint are handled one at a time, in order, as nats.go delivers
// them; a slow handler is throttled by its own subscription, not the
// service's others.
func (s *Service) AddEndpoint(name string, handler Handler, opts ...EndpointOption) error {
	o := endpointOptions{queueGroup: s.name}
	for _, opt := range opts {
		opt(&o)
	}
	subject := EndpointSubject(s.name, name)
	e := endpoint{name: name, subject: subject, queue: o.queueGroup, handler: s.handle(subject, handler)}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.micro != nil {
		if err := addEndpoint(s.micro, e); err != nil {
			return err
		}
	}
	s.endpoints = append(s.endpoints, e)
	return nil
}

// Stop drains the endpoint subscriptions and answers no more requests.
func (s *Service) Stop() error {
	s.client.removeService(s)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stopped = true
	if s.micro == nil {
		return nil
	}
	m := s.micro
	s.micro = nil
	if err := m.Stop(); err != nil {
		return fmt.Errorf("natsclient: stop service %s: %w", s.name, err)
	}
	return nil
}

// pause drains the endpoints for a renewal: the server sends them no more
// requests, and the handlers answer the ones already delivered.
func (s *Service) pause() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.micro == nil {
		return
	}
	if err := s.micro.Stop(); err != nil {
		s.log.Warn("nats renewal: service not drained", "error", err)
	}
	s.micro = nil
}

// resume registers the service again after a renewal.
func (s *Service) resume() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped || s.micro != nil {
		return
	}
	m, err := s.start()
	if err != nil {
		// The service answers nothing until the process restarts: an
		// Error, like a closed connection.
		s.log.Error("nats renewal: service not registered again", "error", err)
		return
	}
	s.micro = m
}

// handle wraps handler for micro: it continues the caller's trace from the
// request headers in a server span and turns a returned error into an
// error response.
func (s *Service) handle(subject string, handler Handler) func(micro.Request) {
	return func(req micro.Request) {
		s.client.started.Add(1)
		s.client.handlers.Add(1)
		defer s.client.handlers.Add(-1)
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
