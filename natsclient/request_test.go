package natsclient_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/MetsaApp/platform-kit/natsclient"
)

func TestRequestNeedsDeadline(t *testing.T) {
	c := connect(t, startServer(t), identity, "", nil)
	_, err := c.Request(t.Context(), "orders.svc.get", nil, nil)
	require.ErrorIs(t, err, natsclient.ErrNoDeadline)
}

// A slow endpoint and an absent one fail differently: the first is a
// timeout the caller may retry, the second a deployment problem.
func TestRequestTimeoutAndNoResponders(t *testing.T) {
	srv := startServer(t)
	c := connect(t, srv, identity, "", nil)
	svc, err := c.Service("slow", "1.0.0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Stop() })
	// The handler's context is not cancelled by the caller's deadline (NATS
	// does not carry one), so the slow handler simply outlasts it.
	require.NoError(t, svc.AddEndpoint("get", func(_ context.Context, req natsclient.Request) error {
		time.Sleep(requestTimeout)
		return req.Respond(nil)
	}))

	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	_, timeoutErr := c.Request(ctx, natsclient.EndpointSubject("slow", "get"), nil, nil)
	require.ErrorIs(t, timeoutErr, natsclient.ErrTimeout)
	require.ErrorIs(t, timeoutErr, context.DeadlineExceeded)
	require.NotErrorIs(t, timeoutErr, natsclient.ErrNoResponders)

	_, noneErr := request(t, c, natsclient.EndpointSubject("nobody", "get"), "")
	require.ErrorIs(t, noneErr, natsclient.ErrNoResponders)
	require.NotErrorIs(t, noneErr, natsclient.ErrTimeout)
	require.NotErrorIs(t, noneErr, context.DeadlineExceeded)
}

// A handler's error reaches the caller as a ServiceError with the code the
// handler chose, or 500 when it did not.
func TestRequestServiceError(t *testing.T) {
	c := connect(t, startServer(t), identity, "", nil)
	svc, err := c.Service("orders", "1.0.0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Stop() })
	require.NoError(t, svc.AddEndpoint("missing", func(context.Context, natsclient.Request) error {
		return &natsclient.ServiceError{Code: "404", Description: "no such order"}
	}))
	require.NoError(t, svc.AddEndpoint("broken", func(context.Context, natsclient.Request) error {
		return errors.New("boom")
	}))

	_, err = request(t, c, natsclient.EndpointSubject("orders", "missing"), "")
	serviceErr, ok := errors.AsType[*natsclient.ServiceError](err)
	require.True(t, ok, err)
	assert.Equal(t, &natsclient.ServiceError{Code: "404", Description: "no such order"}, serviceErr)

	_, err = request(t, c, natsclient.EndpointSubject("orders", "broken"), "")
	serviceErr, ok = errors.AsType[*natsclient.ServiceError](err)
	require.True(t, ok, err)
	assert.Equal(t, &natsclient.ServiceError{Code: "500", Description: "boom"}, serviceErr)
}

// Every request lands in nats_requests_total and
// nats_request_duration_seconds with the service, the subject and its
// outcome, which is what the dashboards and alerts key on.
func TestRequestMetrics(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))

	srv := startServer(t)
	c := connect(t, srv, identity, "", nil)
	subject := serve(t, c, "orders", "get")
	_, err := request(t, c, subject, "x")
	require.NoError(t, err)
	_, err = request(t, c, subject, "y")
	require.NoError(t, err)
	_, err = request(t, c, "nobody.svc.get", "")
	require.ErrorIs(t, err, natsclient.ErrNoResponders)

	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(t.Context(), &rm))
	okAttrs := attribute.NewSet(
		attribute.String("service", "test"), attribute.String("endpoint", subject), attribute.String("outcome", "ok"))
	noneAttrs := attribute.NewSet(
		attribute.String("service", "test"), attribute.String("endpoint", "nobody.svc.get"),
		attribute.String("outcome", "no_responders"))

	counter := findMetric(t, rm, "nats_requests_total")
	sum, ok := counter.Data.(metricdata.Sum[int64])
	require.True(t, ok, "counter data is %T", counter.Data)
	counts := map[attribute.Set]int64{}
	for _, dp := range sum.DataPoints {
		counts[dp.Attributes] = dp.Value
	}
	assert.Equal(t, map[attribute.Set]int64{okAttrs: 2, noneAttrs: 1}, counts)

	histogram := findMetric(t, rm, "nats_request_duration_seconds")
	assert.Equal(t, "s", histogram.Unit)
	hist, ok := histogram.Data.(metricdata.Histogram[float64])
	require.True(t, ok, "histogram data is %T", histogram.Data)
	samples := map[attribute.Set]uint64{}
	for _, dp := range hist.DataPoints {
		samples[dp.Attributes] = dp.Count
		assert.Positive(t, dp.Sum)
	}
	assert.Equal(t, map[attribute.Set]uint64{okAttrs: 2, noneAttrs: 1}, samples)
}

func findMetric(t *testing.T, rm metricdata.ResourceMetrics, name string) metricdata.Metrics {
	t.Helper()
	for _, sm := range rm.ScopeMetrics {
		if sm.Scope.Name != "github.com/MetsaApp/platform-kit/natsclient" {
			continue
		}
		for _, m := range sm.Metrics {
			if m.Name == name {
				return m
			}
		}
	}
	t.Fatalf("metric %s not collected", name)
	return metricdata.Metrics{}
}
