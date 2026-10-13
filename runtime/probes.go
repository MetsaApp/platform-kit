package runtime

import (
	"context"
	"net/http"
	"strings"
	"time"
)

// readyCheckTimeout bounds one readiness check, so a hung dependency reads
// as not ready instead of hanging the probe.
const readyCheckTimeout = 2 * time.Second

type readyCheck struct {
	name  string
	check func(context.Context) error
}

// Ready registers a readiness check, such as a database ping or the NATS
// connection state. /readyz answers 503 while any registered check returns
// an error. Liveness never runs them: a restart does not fix a dependency.
func (r *Runtime) Ready(name string, check func(context.Context) error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.checks = append(r.checks, readyCheck{name: name, check: check})
}

func (r *Runtime) probeHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /readyz", r.readyz)
	return mux
}

func (r *Runtime) readyz(w http.ResponseWriter, req *http.Request) {
	failing := r.failingChecks(req.Context())
	if len(failing) > 0 {
		http.Error(w, "not ready: "+strings.Join(failing, ", "), http.StatusServiceUnavailable)
		return
	}
	_, _ = w.Write([]byte("ok\n"))
}

func (r *Runtime) failingChecks(ctx context.Context) []string {
	// Not ready once shutdown has begun, so the endpoints drop the pod
	// while in-flight requests drain.
	if r.stopping.Load() {
		return []string{"shutting down"}
	}

	r.mu.Lock()
	checks := append([]readyCheck(nil), r.checks...)
	r.mu.Unlock()

	var failing []string
	for _, c := range checks {
		checkCtx, cancel := context.WithTimeout(ctx, readyCheckTimeout)
		err := c.check(checkCtx)
		cancel()
		if err != nil {
			failing = append(failing, c.name)
			r.Logger.WarnContext(ctx, "readiness check failed", "check", c.name, "error", err.Error())
		}
	}
	return failing
}
