package edgeauth

import (
	"errors"
	"log/slog"
	"net/http"
)

// Middleware verifies the identity header and puts the identity in the
// request's context (FromContext). A request without the header passes on
// anonymous. A token that is invalid is refused with 401 and logged; one that
// cannot be checked because guard's JWKS is unreachable is refused with 503.
// The responses are plain text and never echo the token.
//
// With echo: e.Use(echo.WrapMiddleware(v.Middleware)).
func (v *Verifier) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := r.Header.Get(Header) //nolint:canonicalheader // the contract names it in lower case; Get canonicalizes
		if token == "" {
			next.ServeHTTP(w, r)
			return
		}
		id, err := v.Verify(r.Context(), token)
		if err != nil {
			attrs := []any{
				slog.String("host", r.Host),
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.String("error", err.Error()),
			}
			if errors.Is(err, ErrUnavailable) {
				v.logger.ErrorContext(r.Context(), "identity token not checked: guard's keys unavailable", attrs...)
				http.Error(w, "identity keys unavailable", http.StatusServiceUnavailable)
				return
			}
			v.logger.WarnContext(r.Context(), "identity token refused", attrs...)
			http.Error(w, "invalid identity", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r.WithContext(ContextWith(r.Context(), id)))
	})
}

// RequireIdentity refuses an anonymous request with 401. It goes inside
// Middleware, on the handlers that need a signed-in caller.
func RequireIdentity(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := FromContext(r.Context()); !ok {
			http.Error(w, "sign-in required", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// RequireRole refuses an anonymous request with 401 and a caller holding none
// of roles with 403. With no roles it is RequireIdentity.
func RequireRole(roles ...string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id, ok := FromContext(r.Context())
			if !ok {
				http.Error(w, "sign-in required", http.StatusUnauthorized)
				return
			}
			if len(roles) > 0 && !id.HasAnyRole(roles...) {
				http.Error(w, "missing role", http.StatusForbidden)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
