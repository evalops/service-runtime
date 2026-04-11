# Service Runtime

`service-runtime` is the shared Go runtime layer for EvalOps services.

It exists to remove repeated startup/bootstrap code from service repos without
pulling business logic into a central package. The module is intentionally
narrow: it handles dependency bring-up and retry behavior, not routes, schemas,
domain models, or product logic.

## Scope

Current shared concerns:

- startup retry primitives
- PostgreSQL bootstrap helpers for `database/sql`
- Redis bootstrap helpers
- `pgxpool` bootstrap helpers
- mTLS client/server bootstrap helpers
- Identity token introspection client bootstrap
- HTTP request/response helpers and standard endpoints
- service-scoped Prometheus metrics and request observability helpers
- auth middleware primitives for bearer tokens, API keys, and actor context
- atomic audit-entry and change-journal mutation recording
- idempotent mutation middleware and Postgres-backed replay storage
- NATS JetStream CloudEvents publishing primitives

Current non-goals:

- domain-specific route trees
- domain-specific store methods
- SQL schema ownership
- service-specific logging policy

## Packages

### `startup`

Generic retry helpers for service startup paths.

Main entry points:

- `startup.Do(ctx, cfg, fn)`
- `startup.Value[T](ctx, cfg, fn)`

Config:

- `MaxAttempts`
- `Delay`

Defaults:

- `startup.DefaultMaxAttempts`
- `startup.DefaultDelay`

Use this package directly when a service needs retry behavior but still wants
to own the actual bootstrap logic and logging. This is the pattern used by
`gate`, where the service wants retry logs around each failed database attempt.

```go
value, err := startup.Value(ctx, startup.Config{
	MaxAttempts: 30,
	Delay:       2 * time.Second,
}, func(ctx context.Context) (*Thing, error) {
	return openThing(ctx)
})
```

### `postgres`

Helpers for opening and validating `database/sql` PostgreSQL connections.

Main entry points:

- `postgres.Open(ctx, databaseURL, opts)`
- `postgres.OpenAndInit(ctx, databaseURL, init, opts)`

Use `OpenAndInit` when a service needs to run bootstrap logic after the DB is
reachable, such as:

- schema creation
- store construction that depends on a live DB handle
- lightweight startup validation

Example:

```go
db, err := postgres.OpenAndInit(ctx, databaseURL, func(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, schema); err != nil {
		return fmt.Errorf("init_schema: %w", err)
	}
	return nil
}, postgres.Options{})
```

This is the pattern used by `memory`, `meter`, and `audit`.

### `redisutil`

Helpers for opening and validating Redis clients with startup retry.

Main entry point:

- `redisutil.Open(ctx, redisURL, opts)`

Example:

```go
client, err := redisutil.Open(ctx, redisURL, redisutil.Options{})
if err != nil {
	return nil, err
}
```

This is the pattern used by `registry` and `identity`.

### `pgxpoolutil`

Helpers for services that want a validated `pgxpool.Pool` directly rather than
going through `database/sql`.

Main entry point:

- `pgxpoolutil.Open(ctx, dsn, opts)`

Optional hooks:

- `Configure` for mutating parsed pool config before connect
- `PingTimeout`
- retry config via `startup.Config`

Example:

```go
pool, err := pgxpoolutil.Open(ctx, dsn, pgxpoolutil.Options{
	Configure: func(cfg *pgxpool.Config) error {
		cfg.MaxConns = 20
		return nil
	},
})
```

### `mtls`

Helpers for the shared EvalOps mTLS contract.

Main entry points:

- `mtls.BuildServerTLSConfig(cfg)`
- `mtls.BuildClientTLSConfig(cfg)`
- `mtls.BuildHTTPClient(cfg)`
- `mtls.RequireVerifiedClientCertificate(...)`
- `mtls.RequireVerifiedClientCertificateForIdentities(...)`

Use this package when a service needs the same client/server TLS file-path
contract that `memory`, `registry`, `meter`, and `audit` share today.

Example:

```go
httpClient, err := mtls.BuildHTTPClient(mtls.ClientConfig{
	CAFile:     cfg.IdentityTLS.CAFile,
	CertFile:   cfg.IdentityTLS.CertFile,
	KeyFile:    cfg.IdentityTLS.KeyFile,
	ServerName: cfg.IdentityTLS.ServerName,
})
```

### `identityclient`

Helpers for talking to the shared `identity` service introspection endpoint.

Main entry points:

- `identityclient.NewClient(introspectURL, requestTimeout, httpClient)`
- `identityclient.NewMTLSClient(introspectURL, requestTimeout, tlsConfig)`

Use `NewMTLSClient` when a service follows the standard Identity client TLS
contract and does not need to hand-build an HTTP client first.

Example:

```go
identityClient, err := identityclient.NewMTLSClient(
	cfg.IdentityIntrospectURL,
	cfg.IdentityRequestTimeout,
	mtls.ClientConfig{
		CAFile:     cfg.IdentityTLS.CAFile,
		CertFile:   cfg.IdentityTLS.CertFile,
		KeyFile:    cfg.IdentityTLS.KeyFile,
		ServerName: cfg.IdentityTLS.ServerName,
	},
)
```

### `httpkit`

Helpers for shared HTTP request handling primitives without owning a service's
route tree.

Main entry points:

- `httpkit.WriteJSON(writer, status, value)`
- `httpkit.WriteError(writer, status, code, message)`
- `httpkit.WriteMutationJSON(writer, status, payload, sequence)`
- `httpkit.DecodeJSON(writer, request, value)`
- `httpkit.PathUUID(writer, raw, name)`
- `httpkit.RequireIfMatchVersion(writer, request)`
- `httpkit.WithRequestID(next)`
- `httpkit.WithMaxBodySize(maxBytes)`
- `httpkit.WithRequestLogging(logger)`
- `httpkit.HealthHandler(service)`
- `httpkit.ReadyHandler(ping)`
- `httpkit.MetricsHandler()`

Use this package when a service wants the shared JSON error shape, request ID
behavior, health endpoints, and optimistic concurrency helpers without copying
the same router utilities into every repo.

### `observability`

Helpers for service-scoped HTTP metrics, DB stats collectors, and request-level
wide event state.

Main entry points:

- `observability.NewMetrics(serviceName, opts)`
- `observability.RegisterDBStats(serviceName, statFunc, opts)`
- `observability.RequestLoggingMiddleware(logger, metrics)`
- `observability.NewWideEvent(name, category, resourceType, action)`
- `observability.SetWideEvent(request, event)`
- `observability.AddWideEventAttributes(request, attributes)`

Use this package when a service wants the shared Prometheus metric names and
per-request logging/metadata pattern without hard-coding those collectors in
its API package.

### `authmw`

Helpers for shared bearer-token and API-key request authorization without
owning a service's token backend.

Main entry points:

- `authmw.New(config)`
- `middleware.WithAuth(scopes...)`
- `authmw.ActorFromContext(ctx)`
- `authmw.HasAllScopes(available, required)`
- `authmw.BearerToken(header)`

Use this package when a service wants the shared `Authorization` parsing,
API-key scope checks, actor context injection, and transport-level auth
middleware shape while still keeping token verification and API-key lookup in
service-owned backends.

### `changejournal`

Helpers for atomically writing an audit entry and change-journal row inside an
existing transaction with the shared EvalOps schema shape.

Main entry points:

- `changejournal.WriteMutation(ctx, tx, actor, resourceType, resourceID, operation, payload, metadata)`
- `changejournal.WriteMutationWithOptions(ctx, tx, actor, resourceType, resourceID, operation, payload, metadata, opts)`
- `changejournal.Templates(style)`

Supporting types:

- `changejournal.Actor`
- `changejournal.Change`
- `changejournal.AuditEntry`
- `changejournal.Versioned`

Use this package when a service wants one shared write path for mutation
auditing and event-sourcing records instead of re-implementing the same insert
sequence and payload marshaling in each store package. Protobuf payloads are
stored as proto-JSON so the journal stays queryable.

### `idempotency`

Helpers for enforcing idempotent mutation requests and replaying stored
responses from the shared `api_idempotency_keys` schema.

Main entry points:

- `idempotency.Middleware(store, ttl)`
- `idempotency.MiddlewareWithOptions(store, opts)`
- `idempotency.NewPostgresStore(db)`
- `idempotency.DefaultScope(request)`
- `idempotency.RequestHash(method, path, body)`

Supporting types:

- `idempotency.ReplayResult`
- `idempotency.Store`

Use this package when a service wants the shared `Idempotency-Key` behavior,
request hashing, replay handling, and Postgres storage contract instead of
re-implementing those transport semantics per service.

### `natsbus`

Helpers for publishing service change events to NATS JetStream with a shared
CloudEvents envelope and subject convention.

Main entry points:

- `natsbus.Connect(ctx, natsURL, streamName, subjectPrefix, logger)`
- `natsbus.ConnectWithOptions(ctx, natsURL, streamName, subjectPrefix, opts)`
- `publisher.PublishChange(ctx, change)`
- `publisher.Close()`
- `natsbus.NewPayload(message)`
- `natsbus.UnmarshalPayload(payload, target)`
- `natsbus.NoopPublisher`

Use this package when a service wants the shared stream bootstrap and event
envelope contract for change notifications without duplicating JetStream setup
and subject formatting in each repo. `Change.Payload` carries a typed
`google.protobuf.Any`, and the published CloudEvents `data` field stays JSON by
using the protobuf JSON encoding with the embedded `@type` URL.
## Consumption

Add the module to a consumer repo:

```bash
go get github.com/evalops/service-runtime@latest
```

Import only the package you need:

```go
import (
	runtimepostgres "github.com/evalops/service-runtime/postgres"
	runtimeredis "github.com/evalops/service-runtime/redisutil"
	runtimestartup "github.com/evalops/service-runtime/startup"
)
```

## Integration Guidance

Use the shared module when:

- multiple services are carrying the same startup retry loop
- the logic is about dependency bring-up, not request handling
- behavior should be consistent across services

Keep logic local when:

- the code is domain-specific
- a service has distinct operational semantics
- the shared abstraction would erase useful service-level logging or policy

Good pattern:

- keep the service-specific behavior at the edges
- use `service-runtime` for the boring bootstrap mechanics underneath

That is why `gate` uses `startup.Value(...)` directly instead of a one-size
fits-all database helper: it keeps control-plane retry logging while still
reusing the shared retry semantics.

The same rule applies to `identityclient`: it centralizes the boring
introspection transport and error mapping, while services still keep their own
scope checks and request-level auth behavior locally.

## CI and Image Builds

`service-runtime` is public so other EvalOps repos can consume it without
introducing a separate cross-repo credentials flow just for Go module fetches.

If a consuming repo also depends on other private `evalops` modules, keep the
standard Go module environment in CI and builder images:

```bash
GOPRIVATE=github.com/evalops/*
GONOSUMDB=github.com/evalops/*
GOPROXY=direct
```

That pattern is now in the first adoption wave across:

- `memory`
- `registry`
- `identity`
- `gate`
- `meter`
- `audit`

This repo now also publishes the shared bootstrap artifacts that consumers can
reuse directly.

### GitHub Actions bootstrap

Use the composite action:

```yaml
- uses: evalops/service-runtime/.github/actions/setup-go-service@main
```

That action:

- installs the Go version declared by `go.mod`, or an explicit `go-version` override
- exports `GOPRIVATE=github.com/evalops/*`
- exports `GONOSUMDB=github.com/evalops/*`
- exports `GOPROXY=direct`
- optionally runs `go mod download`

Useful knobs:

- `go-version` when CI intentionally tracks a newer toolchain than the repo `go.mod`
- `go-version-file` when the repo keeps Go code in a subdirectory such as `chat/backend`
- `working-directory` to run `go mod download` outside the repo root
- `cache=false` when a workflow manages its own Go cache, such as sharded `cerebro` jobs
- `cache-dependency-path` when the `go.sum` file is not at repo root
- `check-latest=true` when a repo intentionally tracks the latest patch release in CI

### GitHub Actions image publishing

Use the shared GHCR publish action when a repo wants the standard EvalOps
metadata, Buildx setup, and GHCR login flow without re-copying the same steps
into every workflow:

```yaml
- uses: evalops/service-runtime/.github/actions/publish-ghcr-image@main
  with:
    image_name: ghcr.io/evalops/my-service
    github_actor: ${{ github.actor }}
    github_token: ${{ secrets.GITHUB_TOKEN }}
    dockerfile: ./Dockerfile
    push: ${{ github.event_name != 'pull_request' }}
    load: ${{ github.event_name == 'pull_request' }}
```

Useful knobs:

- `target` for multi-stage Dockerfiles such as `gate`
- `build_args` for publish-time overrides such as `GO_BUILDER_IMAGE`
- `build-args` is accepted as a deprecated alias to keep older callers from
  silently dropping overrides
- `platforms` for multi-arch publishes
- `setup_qemu=true` when a workflow needs QEMU for multi-arch image builds
- `metadata_tags` when a repo needs a non-default tagging contract

Outputs:

- `tags` for downstream release notes or artifact manifests
- `labels` for metadata-sensitive follow-up steps
- `digest` for signing, attestation, or SBOM generation

### GitHub Actions local image builds

Use the shared non-push build action when a repo wants a standard smoke-build
step in CI without repeating raw `docker build` commands:

```yaml
- uses: evalops/service-runtime/.github/actions/build-docker-image@main
  with:
    dockerfile: ./Dockerfile
    target: connector
    tags: gate-connector:test
    build_args: |
      GO_BUILDER_IMAGE=golang:1.25-alpine
```

Useful knobs:

- `target` for multi-stage smoke builds
- `build_args` for test-time builder overrides
- `setup_qemu=true` when the CI build itself is multi-arch
- `load` if a later step needs the built image in the local Docker daemon

`build-args` is also accepted here as a deprecated alias so a caller typo does
not silently revert to the default builder image.

### Shared Go builder image

The shared builder image is published from
`images/go-service-builder/Dockerfile` to:

```text
ghcr.io/evalops/service-runtime-go-builder:go1.25
```

A typical consumer Dockerfile can then start with:

```dockerfile
FROM ghcr.io/evalops/service-runtime-go-builder:go1.25 AS builder
```

## Design Rules

When adding new shared helpers here:

- prefer narrow packages over a monolithic runtime package
- share bootstrap mechanics, not product behavior
- keep function signatures explicit
- make retry and timeout behavior configurable
- keep tests hermetic; hook package-level seams only when necessary
- do not centralize service logging policy unless every consumer wants the same behavior

## Repository Layout

```text
startup/       Retry primitives
postgres/      database/sql PostgreSQL bootstrap
redisutil/     Redis bootstrap
pgxpoolutil/   pgxpool bootstrap
mtls/          Shared mTLS client/server helpers
identityclient/ Shared Identity introspection client
images/        Shared builder image definitions
```

## Local Validation

```bash
go test ./... -count=1
go build ./...
```
