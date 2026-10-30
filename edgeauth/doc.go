// Package edgeauth verifies the identity guard hands every authenticated
// request upstream, so an app knows who is calling without running OIDC
// itself.
//
// Guard is the platform's only OIDC relying party. It owns browser sessions,
// validates Zitadel bearer tokens for native clients, and forwards each
// authenticated request with one short-lived signed identity JWT in the
// x-bastion-identity header (Header). Envoy strips a client's copy of that
// header and guard overwrites it, but an app still never trusts it as a plain
// header: it verifies the token, and keeps its own domain authorization on top
// of the identity it gets.
//
// A token is accepted only when every one of these holds:
//
//   - the JOSE header is alg ES256 and typ bastion-id+jwt, with a kid that is
//     in guard's JWKS;
//   - the ES256 signature verifies against that key;
//   - iss is "bastion" and aud is one of the hosts the app serves
//     (Config.Audiences);
//   - exp has not passed and iat is not in the future, both with a 10 s leeway
//     for clock skew, and sub is set.
//
// The JWKS is fetched lazily, on the first token, never in New, so an app
// boots and serves anonymous traffic while guard is unreachable. It is cached
// for 5 minutes; a token whose kid is not in the cache refetches it at most
// once every 30 seconds, and concurrent requests share one fetch. Guard
// publishes a new signing key at least 10 minutes before it signs with it, so
// a rotation never meets a verifier with a stale cache.
//
// Verify tells two failures apart. ErrInvalid means the token is wrong
// (forged, expired, for another host, signed by an unknown key): Middleware
// answers 401 and logs it, because behind guard it can only be an attack or a
// key mistake. ErrUnavailable means the token could not be checked because the
// JWKS could not be fetched: Middleware answers 503, and the request may
// succeed on a retry. A request without the header is anonymous: Middleware
// passes it on with no identity in the context, and RequireIdentity or
// RequireRole refuse it on the handlers that need one. Guard fails closed on
// routes with an auth rule, and the app fails closed again on its protected
// handlers, so a request that slipped past guard is never treated as signed in.
//
// The edgeauthtest package mints identities for an app's own tests, and
// testdata/vectors.json holds conformance vectors that the Go verifier, the
// TypeScript verifier (@metsaapp/edgeauth, ts/edgeauth in this repository) and
// guard's signer all run.
package edgeauth

//go:generate go run ./internal/vectorgen -o testdata/vectors.json
