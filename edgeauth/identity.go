package edgeauth

import (
	"context"
	"slices"
	"time"
)

// Sources of an identity (Identity.Source).
const (
	// SourceSession is a browser session guard holds.
	SourceSession = "session"
	// SourceBearer is a Zitadel access token guard validated.
	SourceBearer = "bearer"
)

// Identity is the caller guard vouches for.
type Identity struct {
	// Subject is the Zitadel user id.
	Subject string
	// Email may be empty.
	Email         string
	EmailVerified bool
	// Name is the display name and may be empty.
	Name string
	// Roles are the role keys of the host's Zitadel project, sorted and
	// de-duplicated; empty, never nil, when the user has none.
	Roles []string
	// SessionID is an opaque session id, empty for a bearer.
	SessionID string
	// AuthTime is when the user logged in at Zitadel; zero when unknown.
	AuthTime time.Time
	// ClientID is the Zitadel client the session or bearer belongs to.
	ClientID string
	// Source is SourceSession or SourceBearer.
	Source string
}

// HasRole reports whether the identity holds role.
func (i Identity) HasRole(role string) bool {
	return slices.Contains(i.Roles, role)
}

// HasAnyRole reports whether the identity holds at least one of roles. It is
// false when roles is empty.
func (i Identity) HasAnyRole(roles ...string) bool {
	return slices.ContainsFunc(roles, i.HasRole)
}

type contextKey struct{}

// ContextWith returns a copy of ctx carrying id. Middleware calls it for a
// verified token; a test calls it to run a handler as someone without minting
// a token.
func ContextWith(ctx context.Context, id Identity) context.Context {
	return context.WithValue(ctx, contextKey{}, id)
}

// FromContext returns the identity Middleware verified, and false for an
// anonymous request.
func FromContext(ctx context.Context) (Identity, bool) {
	id, ok := ctx.Value(contextKey{}).(Identity)
	return id, ok
}
