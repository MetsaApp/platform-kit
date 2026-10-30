# platform-kit

Shared Go module for services on the platform. The `runtime` package
implements the runtime contract once, so a service's `main` is: load the
runtime, register readiness checks, serve. The `natsclient` package is the
NATS client every service shares, and `edgeauth` verifies the identity guard
hands every authenticated request.

```go
ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
defer stop()

rt, err := runtime.New(ctx)        // config, logger, tracing, metrics
if err != nil { ... }
rt.Ready("database", pool.Ping)    // /readyz is 503 while this fails
err = rt.Serve(ctx, handler)       // until ctx is done, then drain and flush
```

```
go get github.com/MetsaApp/platform-kit
```

## The contract

| Port | Serves |
|---|---|
| 8080 | the handler passed to `Serve`; a nil handler serves nothing, for a worker |
| 8081 | `/healthz` (the process is up) and `/readyz` (every registered check passes) |
| 9464 | `/metrics`: OTel instruments and `client_golang` collectors on the default registry |

The ports are fixed. `/healthz` never runs the readiness checks, because a
restart does not fix a dependency. `/readyz` also answers 503 once the
process has begun to stop.

| Variable | `rt.Config` | |
|---|---|---|
| `ENVIRONMENT` | `Environment` | default `local` |
| `LOG_LEVEL` | `LogLevel` | default `info` |
| `OTEL_SERVICE_NAME` | `ServiceName` | required; the name in traces, metrics and logs |
| `SERVICE_IDENTITY` | `ServiceIdentity` | `<namespace>/<serviceaccount>` |
| `DATABASE_URL` | `DatabaseURL` | |
| `NATS_URL` | `NATSURL` | |
| `NATS_TOKEN_FILE` | `NATSTokenFile` | rotates hourly: read it on every connect |
| `REDIS_URL` | `RedisURL` | |

The connection settings are empty unless the service's definition enables
them; a service checks the ones it needs. The other `OTEL_*` variables
(endpoint, sampler, resource attributes) are read by the OTel SDK. Traces are
exported over OTLP gRPC only when `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` or
`OTEL_EXPORTER_OTLP_ENDPOINT` is set.

These names are the contract's and carry no prefix. Anything else is
namespaced: a service prefixes its own settings with its name (`LIVE_...`,
`BILLING_...`) and parses them itself, and any variable this module adds
beyond the contract starts with `PLATFORM_KIT_`:

| Variable | Read by | |
|---|---|---|
| `PLATFORM_KIT_EDGEAUTH_JWKS_URL` | `edgeauth.Config.JWKSURL` | default `http://guard.guard-production.svc.cluster.local:8082/jwks.json` |
| `PLATFORM_KIT_EDGEAUTH_AUDIENCES` | `edgeauth.Config.Audiences` | required by `edgeauth.New`: the hosts the app serves, comma-separated |

Logs are JSON on stderr with `service`, `environment` and, when logged with a
`*Context` method inside a span, `trace_id` and `span_id`. Use `rt.Logger`;
the module never sets the global logger.

## Example

`example/` is a complete service:

```
OTEL_SERVICE_NAME=example go run ./example
curl localhost:8080/
curl -i localhost:8081/readyz     # 503 for two seconds, then 200
curl localhost:9464/metrics
```

## natsclient

The `natsclient` package is the one NATS client every Go service uses: it
connects with the projected token and the service's identity, serves
request/reply endpoints and makes requests with deadlines, tracing and
metrics. Events and commands (JetStream) come from the service's AsyncAPI
contract, not from this package.

```go
nc, err := natsclient.Connect(ctx, natsclient.FromRuntime(rt.Config), rt.Logger)
if err != nil { ... }
rt.Ready("nats", nc.Connected())

svc, err := nc.Service("orders", version)                 // $SRV discovery for free
err = svc.AddEndpoint("get", func(ctx context.Context, req natsclient.Request) error {
    return req.Respond(data)                              // orders.svc.get, queue group "orders"
})

ctx, cancel := context.WithTimeout(ctx, 2*time.Second)    // a deadline is required
reply, err := nc.Request(ctx, natsclient.EndpointSubject("orders", "get"), body, nil)
```

Subjects follow ADR 0006: `<service>.svc.<endpoint>` for synchronous calls on
core NATS, `<service>.evt.<event>` and `<service>.cmd.<command>` on JetStream.
Endpoints join a queue group named after the service, so replicas share the
load; `WithQueueGroup` picks another group for an endpoint that must see every
request. The connection re-reads the token on every (re)connect, reconnects
forever, is named after the service, and logs every connection event: the
hourly credential expiry at Info, a closed connection at Error.

Replies arrive on a private inbox, `_INBOX_<ns>_<sa>` from `SERVICE_IDENTITY`
with dots turned into `_`, which the auth callout grants to that identity
alone. Until the grant is issued (ENG-447) an identity requests with
`WithSharedInbox()`, which keeps nats.go's `_INBOX` prefix.

`Request` tells failures apart, so a caller can act on them:

| Error | Meaning |
|---|---|
| `ErrNoDeadline` | the context has no deadline; a request without one would hang on a lost reply |
| `ErrNoResponders` | nobody serves the subject: the target is down or not deployed (wraps `nats.ErrNoResponders`) |
| `ErrTimeout` | the deadline passed with no reply: the target is slow (wraps `context.DeadlineExceeded`) |
| `*ServiceError` | the endpoint answered with an error response; a handler returns one to pick the code, any other error is 500 |

Every request is counted in `nats_requests_total{service,endpoint,outcome}`
(`ok`, `no_responders`, `timeout`, `error`) and timed in
`nats_request_duration_seconds` with the same labels. The caller's trace
goes along as a W3C `traceparent` header, and the handler's context carries
it, so logs and spans on both sides share one trace.

### Conventions for Rust and .NET

The other languages follow the same conventions with their own clients;
nothing is wrapped.

- Connect as the user `SERVICE_IDENTITY` with the projected token at
  `NATS_TOKEN_FILE` as the password, read on every connect attempt, never
  once at start (async-nats: `ConnectOptions::auth_callback`; NATS.Net:
  `NatsAuthOpts.AuthCredCallback`), reconnect without limit, and name the
  connection after the service.
- Set the private inbox prefix `_INBOX_<ns>_<sa>` (async-nats:
  `ConnectOptions::custom_inbox_prefix`; NATS.Net: `NatsOpts.InboxPrefix`).
- Subjects are `<service>.svc.<endpoint>`, `<service>.evt.<event>` and
  `<service>.cmd.<command>`; endpoints join the queue group named after the
  service.
- Every request has a deadline, and "no responders" is handled apart from a
  timeout (async-nats: `RequestErrorKind::NoResponders` vs `TimedOut`;
  NATS.Net: `NatsNoRespondersException` vs `NatsNoReplyException`).
- The W3C `traceparent` header carries the trace; the handler continues it in
  a server span.
- Metrics are `nats_requests_total{service,endpoint,outcome}` and
  `nats_request_duration_seconds` with the same labels.

## edgeauth

Guard is the only OIDC relying party on the platform. It sends every
authenticated request upstream with a short-lived identity JWT in the
`x-bastion-identity` header, and `edgeauth` verifies it, so an app never runs
OIDC and never trusts a plain header. A token is accepted when:

- the JOSE header is `alg: ES256`, `typ: bastion-id+jwt` and a `kid` in
  guard's JWKS, and the signature verifies;
- `iss` is `bastion` and `aud` is one of `Config.Audiences`;
- `exp` has not passed and `iat` is not in the future, each with 10 s leeway,
  and `sub` is set.

The JWKS is fetched on the first token, never at start, cached for 5 minutes,
and refetched for an unknown `kid` at most once every 30 seconds (concurrent
requests share one fetch). An app boots and serves anonymous traffic while
guard is down.

| Request | `Middleware` |
|---|---|
| no header | passes on anonymous: `FromContext` reports false |
| valid token | passes on with the `Identity` in the context |
| invalid token (`ErrInvalid`) | 401, logged through `WithLogger` |
| JWKS unreachable (`ErrUnavailable`) | 503 |

Responses are plain text and never echo the token. `RequireIdentity` (401
when anonymous) and `RequireRole(roles...)` (any of; 401 anonymous, 403
without a role) go on the handlers that need a caller; they are the app's
second fail-closed check after guard's own. Domain authorization stays in the
app.

```go
cfg, err := edgeauth.ConfigFromEnv()           // PLATFORM_KIT_EDGEAUTH_*
if err != nil { ... }
v, err := edgeauth.New(cfg, edgeauth.WithLogger(rt.Logger))
if err != nil { ... }

mux := http.NewServeMux()
mux.Handle("GET /", publicHandler)
mux.Handle("GET /me", edgeauth.RequireIdentity(meHandler))
mux.Handle("/admin/", edgeauth.RequireRole("admin")(adminHandler))
err = rt.Serve(ctx, v.Middleware(mux))

// in a handler
id, ok := edgeauth.FromContext(r.Context())   // id.Subject, id.Roles, id.HasRole("admin")
```

With echo:

```go
e.Use(echo.WrapMiddleware(v.Middleware))
admin := e.Group("/admin", echo.WrapMiddleware(edgeauth.RequireRole("admin")))
```

Tests mint identities with `edgeauthtest`, which signs with a throwaway key
and serves its JWKS on an `httptest` server closed at the end of the test:

```go
iss := edgeauthtest.NewIssuer(t)
v, err := edgeauth.New(iss.Config("live.metsa.app"))

req := httptest.NewRequest(http.MethodGet, "https://live.metsa.app/admin", nil)
iss.Sign(req, edgeauth.Identity{Subject: "u1", Roles: []string{"admin"}}) // aud = req.Host

expired := iss.MintWith(id, "live.metsa.app", func(c *edgeauthtest.Claims) { c.Expiry -= 600 })
```

A handler test that does not care about tokens puts the identity in the
context directly with `edgeauth.ContextWith`.

`edgeauth/testdata/vectors.json` holds conformance vectors: a JWKS, a fixed
`now`, and tokens with the identity or the error class (`invalid`,
`unavailable`) each must give. The Go and TypeScript tests and guard's signer
test run them; `go generate ./edgeauth` rewrites them, and a test fails when
the committed file is stale.

### TypeScript

`@metsaapp/edgeauth` (`ts/edgeauth/`) applies the same rules on top of
[jose](https://github.com/panva/jose). It is installed from this repository
by tag; there is no registry:

```
bun add @metsaapp/edgeauth@github:MetsaApp/platform-kit#v0.3.0
npm install @metsaapp/edgeauth@github:MetsaApp/platform-kit#v0.3.0
```

```ts
import { createVerifier, EdgeAuthInvalid, EdgeAuthUnavailable } from "@metsaapp/edgeauth";

const verifier = createVerifier({ audiences: ["umami.metsa.app"] }); // jwksUrl defaults as in Go
const id = await verifier.fromHeaders(request.headers); // null when anonymous
// rejects with EdgeAuthInvalid (answer 401) or EdgeAuthUnavailable (503)
```

The package's `package.json` is at the repository root, with `files` limited
to `ts/edgeauth`. Bun imports the TypeScript source (the `bun` export
condition); Node and bundlers import the compiled `ts/edgeauth/dist`, which is
committed, because git installs do not run a build reliably (Bun and pnpm skip
lifecycle scripts of git dependencies). CI fails when `dist` does not match
the source. release-please bumps the root `package.json` version with each
release.

```
cd ts/edgeauth && npm ci
npm run build && npm run typecheck && npm test   # commit dist/ with the source
```

## Working here

```
go build ./... && go vet ./... && go test -race ./...
go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.11.3 run
```

The tests run a small service as a child process on the real ports, so 8080,
8081 and 9464 must be free.

Releases are cut by release-please from conventional commit titles; only
`feat:` and `fix:` produce one.
