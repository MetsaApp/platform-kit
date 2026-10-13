// Command example is the smallest service on the runtime contract: load the
// runtime, register a readiness check, serve on 8080.
//
//	OTEL_SERVICE_NAME=example go run ./example
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/MetsaApp/platform-kit/runtime"
)

// warmup stands in for a dependency that takes a moment to come up.
const warmup = 2 * time.Second

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	rt, err := runtime.New(ctx)
	if err != nil {
		return fmt.Errorf("start runtime: %w", err)
	}

	// A real service registers its database ping or NATS connection here.
	var warm atomic.Bool
	time.AfterFunc(warmup, func() { warm.Store(true) })
	rt.Ready("warmup", func(context.Context) error {
		if !warm.Load() {
			return errors.New("still warming up")
		}
		return nil
	})

	mux := http.NewServeMux()
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		rt.Logger.InfoContext(r.Context(), "hello")
		_, _ = fmt.Fprintf(w, "hello from %s\n", rt.Config.ServiceName)
	})

	if err = rt.Serve(ctx, mux); err != nil {
		return fmt.Errorf("serve: %w", err)
	}
	return nil
}
