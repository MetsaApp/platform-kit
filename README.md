# platform-kit

Shared Go module for services on the platform. The `runtime` package
implements the runtime contract once, so a service's `main` is: load the
runtime, register readiness checks, serve.

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
beyond the contract starts with `PLATFORM_KIT_`.

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

## Working here

```
go build ./... && go vet ./... && go test -race ./...
go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.11.3 run
```

The tests run a small service as a child process on the real ports, so 8080,
8081 and 9464 must be free.

Releases are cut by release-please from conventional commit titles; only
`feat:` and `fix:` produce one.
