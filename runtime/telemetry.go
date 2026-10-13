package runtime

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	otelruntime "go.opentelemetry.io/contrib/instrumentation/runtime"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	otelprom "go.opentelemetry.io/otel/exporters/prometheus"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// runtimeMemStatsInterval throttles the runtime collector; at 30s it is a
// no-op against the scrape interval and is kept for clarity.
const runtimeMemStatsInterval = 30 * time.Second

// initTelemetry configures the global tracer and meter providers and returns
// a func that flushes what was built. Traces ship over OTLP gRPC to whatever
// OTEL_EXPORTER_OTLP[_TRACES]_ENDPOINT points at; with no endpoint the
// exporter is not built at all, so local runs dial nothing. Metrics are read
// by the Prometheus exporter and served by the metrics listener.
func initTelemetry(ctx context.Context, logger *slog.Logger) (func(context.Context) error, error) {
	// SDK errors (export failures) are logged at WARN so a collector outage
	// is visible in the log stream.
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) {
		logger.WarnContext(context.Background(), "otel sdk error", "error", err.Error())
	}))

	// service.name and deployment.environment come from OTEL_SERVICE_NAME
	// and OTEL_RESOURCE_ATTRIBUTES: nothing about the deployment is named
	// in code.
	res, err := resource.New(ctx, resource.WithFromEnv(), resource.WithHost())
	if err != nil {
		return nil, fmt.Errorf("build telemetry resource: %w", err)
	}

	// Always set, even without an exporter: instrumented clients and
	// servers still forward W3C traceparent so downstream hops keep one
	// trace.
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	var shutdowns []func(context.Context) error

	if tracesConfigured() {
		// No explicit transport security or sampler: the endpoint's scheme
		// and OTEL_TRACES_SAMPLER / _ARG decide.
		traceExporter, traceErr := otlptracegrpc.New(ctx)
		if traceErr != nil {
			return nil, fmt.Errorf("build OTLP trace exporter: %w", traceErr)
		}
		tp := sdktrace.NewTracerProvider(
			sdktrace.WithResource(res),
			sdktrace.WithBatcher(traceExporter),
		)
		otel.SetTracerProvider(tp)
		shutdowns = append(shutdowns, tp.Shutdown)
	}

	// The exporter registers on the default Prometheus registry, so OTel
	// instruments and plain client_golang collectors are scraped together.
	promExporter, promErr := otelprom.New()
	if promErr != nil {
		return nil, errors.Join(fmt.Errorf("build prometheus exporter: %w", promErr), joinShutdowns(shutdowns)(ctx))
	}
	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithResource(res),
		sdkmetric.WithReader(promExporter),
		// Keep only the RED latency histogram from the HTTP server
		// instrumentation; the two body size histograms triple the
		// per-route series for nothing.
		sdkmetric.WithView(sdkmetric.NewView(
			sdkmetric.Instrument{Name: "http.server.request.body.size"},
			sdkmetric.Stream{Aggregation: sdkmetric.AggregationDrop{}},
		)),
		sdkmetric.WithView(sdkmetric.NewView(
			sdkmetric.Instrument{Name: "http.server.response.body.size"},
			sdkmetric.Stream{Aggregation: sdkmetric.AggregationDrop{}},
		)),
	)
	otel.SetMeterProvider(mp)
	shutdowns = append(shutdowns, mp.Shutdown)

	if runtimeErr := otelruntime.Start(
		otelruntime.WithMinimumReadMemStatsInterval(runtimeMemStatsInterval),
	); runtimeErr != nil {
		return nil, errors.Join(
			fmt.Errorf("start runtime instrumentation: %w", runtimeErr),
			joinShutdowns(shutdowns)(ctx),
		)
	}

	return joinShutdowns(shutdowns), nil
}

// tracesConfigured reports whether OTLP trace export has a destination, per
// the standard env precedence: the per-signal var, then the shared one.
func tracesConfigured() bool {
	return os.Getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT") != "" ||
		os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") != ""
}

func joinShutdowns(fns []func(context.Context) error) func(context.Context) error {
	return func(ctx context.Context) error {
		errs := make([]error, 0, len(fns))
		for _, fn := range fns {
			errs = append(errs, fn(ctx))
		}
		return errors.Join(errs...)
	}
}
