package runtime

import (
	"context"
	"io"
	"log/slog"

	"go.opentelemetry.io/otel/trace"
)

// newLogger returns the process logger: JSON in every environment (the log
// pipeline parses one shape), with service and environment as base
// attributes and trace_id/span_id injected from the active span on every
// *Context call.
func newLogger(w io.Writer, cfg Config) *slog.Logger {
	inner := slog.NewJSONHandler(w, &slog.HandlerOptions{Level: cfg.LogLevel})
	return slog.New(&traceHandler{inner: inner}).
		With("service", cfg.ServiceName, "environment", cfg.Environment)
}

type traceHandler struct {
	inner slog.Handler
}

func (h *traceHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.inner.Enabled(ctx, level)
}

func (h *traceHandler) Handle(ctx context.Context, r slog.Record) error {
	sc := trace.SpanFromContext(ctx).SpanContext()
	if sc.HasTraceID() {
		r.AddAttrs(
			slog.String("trace_id", sc.TraceID().String()),
			slog.String("span_id", sc.SpanID().String()),
		)
	}
	return h.inner.Handle(ctx, r) //nolint:wrapcheck // slog.Handler contract.
}

func (h *traceHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &traceHandler{inner: h.inner.WithAttrs(attrs)}
}

func (h *traceHandler) WithGroup(name string) slog.Handler {
	return &traceHandler{inner: h.inner.WithGroup(name)}
}
