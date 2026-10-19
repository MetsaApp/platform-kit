package natsclient_test

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/MetsaApp/platform-kit/natsclient"
)

// counting is an endpoint handler that counts its requests before echoing.
func counting(n *atomic.Int64) natsclient.Handler {
	return func(_ context.Context, req natsclient.Request) error {
		n.Add(1)
		return req.Respond(req.Data())
	}
}

// Two instances of one service are two replicas: they share one queue
// group, so a burst is split between them. An endpoint in another group is
// a separate subscriber and sees every request.
func TestQueueGroupSharing(t *testing.T) {
	const burst = 40
	srv := startServer(t)
	c := connect(t, srv, identity, "", nil)
	var replicaA, replicaB, observer atomic.Int64
	for _, n := range []*atomic.Int64{&replicaA, &replicaB} {
		svc, err := c.Service("orders", "1.0.0")
		require.NoError(t, err)
		t.Cleanup(func() { _ = svc.Stop() })
		require.NoError(t, svc.AddEndpoint("get", counting(n)))
	}
	audit, err := c.Service("orders", "1.0.0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = audit.Stop() })
	require.NoError(t, audit.AddEndpoint("get", counting(&observer), natsclient.WithQueueGroup("audit")))

	subject := natsclient.EndpointSubject("orders", "get")
	for range burst {
		_, reqErr := request(t, c, subject, "x")
		require.NoError(t, reqErr)
	}
	assert.Equal(t, int64(burst), replicaA.Load()+replicaB.Load(), "the replicas split the burst")
	assert.Positive(t, replicaA.Load())
	assert.Positive(t, replicaB.Load())
	assert.Equal(t, int64(burst), observer.Load(), "another group sees every request")
}

// The caller's trace continues in the handler: the request carries a W3C
// traceparent, the handler's context holds the same trace id, and the
// exported spans form caller -> client -> server.
func TestTracePropagation(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	otel.SetTracerProvider(tp)
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	c := connect(t, startServer(t), identity, "", nil)
	svc, err := c.Service("orders", "1.0.0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Stop() })
	seen := make(chan trace.SpanContext, 1)
	require.NoError(t, svc.AddEndpoint("get", func(ctx context.Context, req natsclient.Request) error {
		seen <- trace.SpanContextFromContext(ctx)
		return req.Respond(nil)
	}))

	ctx, caller := tp.Tracer("test").Start(t.Context(), "caller")
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	_, err = c.Request(ctx, natsclient.EndpointSubject("orders", "get"), nil, nil)
	require.NoError(t, err)
	caller.End()

	handlerSC := <-seen
	assert.Equal(t, caller.SpanContext().TraceID(), handlerSC.TraceID())
	assert.NotEqual(t, caller.SpanContext().SpanID(), handlerSC.SpanID())

	kinds := map[trace.SpanKind]tracetest.SpanStub{}
	for _, s := range exporter.GetSpans() {
		kinds[s.SpanKind] = s
	}
	client, server := kinds[trace.SpanKindClient], kinds[trace.SpanKindServer]
	assert.Equal(t, "orders.svc.get", client.Name)
	assert.Equal(t, "orders.svc.get", server.Name)
	assert.Equal(t, caller.SpanContext().SpanID(), client.Parent.SpanID())
	assert.Equal(t, client.SpanContext.SpanID(), server.Parent.SpanID())
	assert.Equal(t, server.SpanContext.SpanID(), handlerSC.SpanID())
}
