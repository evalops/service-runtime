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
- lightweight feature-flag and dynamic config snapshot loading
- reusable test helpers for backed Postgres, HTTP handlers, and auth-shaped requests
- `evalops-agent-hook` governance/approval gating for agent `PreToolUse` hooks

Current non-goals:

- domain-specific route trees
- domain-specific store methods
- SQL schema ownership
- service-specific logging policy

## Releases

Every merge to `main` now cuts the next patch release automatically.

The release workflow finds the latest `vX.Y.Z` tag, increments the patch
number, tags the merge commit, and publishes a GitHub release with generated
notes. This keeps downstream services on normal semver module versions instead
of timestamped pseudo-versions.

The automation intentionally stays conservative and only advances the patch
line. If maintainers need to start a new minor or major line, cut that seed tag
manually first and the workflow will continue from there on later merges.

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

### `testutil`

Helpers for reducing repeated test setup across service repositories.

Main entry points:

- `testutil.Context(t)`
- `testutil.NewTestDB(t, schemaSQL)`
- `testutil.NewTestPGXPool(t, schemaSQL)`
- `testutil.NewTestServer(t, handler)`
- `testutil.NewTestToken(t, claims)`
- `testutil.NewAuthenticatedRequest(t, method, target, orgID, scope, body...)`
- `testutil.AssertJSONResponse(t, response, status, body)`
- `testutil.AssertErrorCode(t, raw, expected)`

`NewTestDB` and `NewTestPGXPool` create an isolated schema inside the database
pointed to by `TEST_DATABASE_URL`, apply optional schema SQL, and drop the
schema during test cleanup. That keeps backed integration tests hermetic
without forcing each service to hand-roll its own schema lifecycle.

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
- `identityclient.(*Client).IntrospectProto(ctx, bearerToken)`
- `identityclient.New(identityclient.Config{...})`

Use `NewMTLSClient` when a service follows the standard Identity client TLS
contract and does not need to hand-build an HTTP client first.

Use `identityclient.New(...)` when a service also needs bootstrap-key-backed
`IssueServiceToken(...)` / `ResolveServiceToken(...)`, org/service/scope-aware
service token caching, or cached introspection fallback during transient
Identity outages. Outbound requests automatically use an OpenTelemetry-aware
transport so trace context is propagated to Identity when the caller has an
active span.

### `agenthook`

Standalone CLI for external coding-agent hook enforcement.

Main entry point:

- `go run ./cmd/evalops-agent-hook -- governance-check`

Environment contract:

- `EVALOPS_GOVERNANCE_URL`
- `EVALOPS_APPROVALS_URL`
- `EVALOPS_AGENT_TOKEN`
- `EVALOPS_WORKSPACE_ID`
- `EVALOPS_AGENT_ID`
- `EVALOPS_HOOK_SURFACE` or legacy `EVALOPS_SURFACE`
- `EVALOPS_APPROVAL_TIMEOUT`
- `EVALOPS_APPROVAL_POLL_INTERVAL`
- `EVALOPS_GOVERNANCE_TIMEOUT`
- `EVALOPS_APPROVALS_TIMEOUT`

Optional shared mTLS client settings:

- `EVALOPS_CA_FILE`
- `EVALOPS_CERT_FILE`
- `EVALOPS_KEY_FILE`
- `EVALOPS_SERVER_NAME`

The `governance-check` command reads a PreToolUse JSON payload from stdin,
calls governance for `ALLOW` / `DENY` / `REQUIRE_APPROVAL`, requests approval
when needed, and prints the deny response shape expected by Codex and Claude
Code when execution must be blocked.

Both service URLs accept either the service base URL or a full ConnectRPC
procedure URL; the hook normalizes either form before creating clients.

`EVALOPS_AGENT_ID` falls back to the hook `session_id` when it is not set, and
invalid hook/config/bootstrap states fail closed with the same deny payload the
hook uses for policy blocks.

The release workflow now attaches cross-compiled `evalops-agent-hook`
archives for `darwin/linux` and `amd64/arm64` to GitHub Releases so Claude
Code and Codex deployments can pull a pinned binary directly.

Example Codex hook:

```toml
[hooks.pre_tool_use]
command = "evalops-agent-hook governance-check"
```

Example Claude Code hook:

```json
{
  "hooks": {
    "PreToolUse": [{
      "matcher": "*",
      "hooks": [{
        "type": "command",
        "command": "evalops-agent-hook governance-check"
      }]
    }]
  }
}
```

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

### `featureflags`

Helpers for reading the shared `config/v1.FeatureFlagSnapshot` protojson file
that `deploy` mounts into workloads.

Main entry points:

- `featureflags.NewFileStore(path, opts)`
- `(*featureflags.FileStore).Enabled(key)`
- `(*featureflags.FileStore).Lookup(key)`
- `(*featureflags.FileStore).Snapshot()`

The file store keeps the last good snapshot in memory and lazily reloads on a
poll interval, which is enough for ConfigMap-backed runtime toggles without
forcing each service to hand-roll its own watcher logic.

### `health`

Helpers for dependency-aware readiness checks with shared caching defaults.

Main entry points:

- `health.New()`
- `(*health.Checker).Add(name, check)`
- `(*health.Checker).Check(ctx, timeout)`
- `(*health.Checker).CachedCheck(ctx, timeout, ttl)`
- `(*health.Checker).Handler(timeout)`
- `(*health.Checker).CachedHandler(timeout, ttl)`
- `(*health.Checker).ReadyzHandler()`
- `health.PostgresCheck(db)`
- `redischeck.Check(client)`
- `natscheck.Check(conn)`
- `health.HTTPCheck(client, url)`
- `health.PingCheck(pinger)`
- `health.TCPCheck(addr)`

Use this package when a service needs `/readyz` to reflect real downstream
dependency state instead of a hardcoded success response. The shared
`ReadyzHandler` caches readiness reports for 5 seconds and uses a 2-second
timeout per probe so Kubernetes does not hammer dependencies on every poll.
Redis and NATS adapters live in `health/redischeck` and `health/natscheck` so
generic health consumers do not inherit those dependencies by default.

- `httpkit.WriteJSON(writer, status, value)`
- `httpkit.WriteError(writer, status, code, message)`
- `httpkit.WriteMutationJSON(writer, status, payload, sequence)`
- `httpkit.DecodeJSON(writer, request, value)`
- `httpkit.PathUUID(writer, raw, name)`
- `httpkit.RequireIfMatchVersion(writer, request)`
- `httpkit.WithRequestID(next)`
- `httpkit.WithMaxBodySize(maxBytes)`
- `httpkit.WithRequestLogging(logger)`
- `httpkit.WithTelemetry(service)`
- `httpkit.HealthHandler(service)`
- `httpkit.ReadyHandler(ping)`
- `httpkit.MetricsHandler()`

Use this package when a service wants the shared JSON error shape, request ID
behavior, health endpoints, and optimistic concurrency helpers without copying
the same router utilities into every repo. `WithTelemetry(service)` wraps a
handler with `otelhttp` server spans using route-aware span names.

### `observability`

Helpers for service-scoped HTTP metrics, DB stats collectors, and request-level
wide event state.

Main entry points:

- `observability.NewMetrics(serviceName, opts)`
- `observability.RegisterDBStats(serviceName, statFunc, opts)`
- `observability.RequestLoggingMiddleware(logger, metrics)`
- `observability.NewBoundedLabel(name, values...)`
- `observability.NewWideEvent(name, category, resourceType, action)`
- `observability.SetWideEvent(request, event)`
- `observability.AddWideEventAttributes(request, attributes)`

Use this package when a service wants the shared Prometheus metric names and
per-request logging/metadata pattern without hard-coding those collectors in
its API package.

Metric label rules:

- Labels MUST be bounded to a known, enumerable set such as method, status, action, or downstream name.
- Labels MUST NOT contain tenant identifiers such as `workspace_id`, `org_id`, `user_id`, or `agent_id`.
- Labels MUST NOT contain request identifiers such as `request_id`, `trace_id`, or `session_id`.
- HTTP `route` labels MUST use templated route patterns such as `/agents/{agentID}`, not raw URL paths.
- For per-tenant drill-down, use structured logging with `slog` or exemplars instead of metric labels.
- When a label is finite but caller-controlled, clamp it with `observability.NewBoundedLabel(...).Value(...)` so unknown values collapse to `other`.

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

### `ratelimit`

Helpers for request-rate enforcement with shared in-memory and Redis-backed
token buckets.

Main entry points:

- `ratelimit.New(config)`
- `limiter.Middleware(next)`
- `limiter.Allow(key)`
- `limiter.AllowContext(ctx, key)`

Supporting types:

- `ratelimit.Config`
- `ratelimit.Policy`

Use this package when a service wants one shared rate-limiting middleware with
default IP-based keys, route or scope partitioning, Redis-backed coordination
across replicas, and service-scoped Prometheus counters instead of rebuilding
those transport safeguards per repo.

### `rterrors`

Helpers for structured runtime errors, Connect/HTTP status mapping, and panic
recovery middleware.

Main entry points:

- `rterrors.New(code, message)`
- `rterrors.Wrap(code, op, err)`
- `rterrors.E(code, op, message, err)`
- `rterrors.CodeOf(err)`
- `rterrors.MessageOf(err)`
- `rterrors.HTTPStatus(err)`
- `rterrors.ConnectCode(err)`
- `rterrors.ToConnectError(err)`
- `rterrors.WriteError(writer, err)`
- `rterrors.RecoverMiddleware(logger)`

Use this package when a service wants one shared error taxonomy and response
mapping across plain HTTP handlers and Connect services while preserving
wrapped causes and operation context for debugging.

### `natsbus`

Helpers for publishing service change events to NATS JetStream with a shared
CloudEvents envelope and subject convention.

Main entry points:

- `natsbus.Connect(ctx, natsURL, streamName, subjectPrefix, logger)`
- `natsbus.ConnectWithOptions(ctx, natsURL, streamName, subjectPrefix, opts)`
- `natsbus.ConnectReliable(ctx, natsURL, streamName, subjectPrefix, logger, deadLetterDir)`
- `natsbus.ConnectReliableWithOptions(ctx, natsURL, streamName, subjectPrefix, opts)`
- `publisher.PublishChange(ctx, change)`
- `publisher.Close()`
- `natsbus.NewPayload(message)`
- `natsbus.UnmarshalPayload(payload, target)`
- `natsbus.UnmarshalMessage(msg)`
- `natsbus.ExtractContext(ctx, envelope)`
- `natsbus.NoopPublisher`

Use this package when a service wants the shared stream bootstrap and event
envelope contract for change notifications without duplicating JetStream setup
and subject formatting in each repo. `Change.Payload` carries a typed
`google.protobuf.Any`. JSON CloudEvents remain the default wire format for
compatibility, and services can opt into protobuf transport bytes with
`Options.WireFormat = natsbus.WireFormatProto` (proto envelope bytes) or
`Options.WireFormat = natsbus.WireFormatProtoHeaders` (CloudEvent metadata in
NATS headers with protobuf body bytes). Consumers can use
`natsbus.UnmarshalEnvelope(...)` for legacy envelope bytes or
`natsbus.UnmarshalMessage(...)` to accept the new header/body format alongside
older JSON/proto envelopes during rollout. All envelope variants now preserve
`traceparent`, `tracestate`, and `baggage`, and consumers can call
`natsbus.ExtractContext(...)` to continue the upstream trace when handling a
message. Services that cannot afford silent event loss can opt into
`ReliablePublisher`, which wraps publish attempts with shared retry and circuit
breaker logic, persists failed messages to a file-backed dead-letter queue, and
replays them automatically when JetStream recovers.
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
- configures authenticated `git` access for private `github.com/evalops/*` modules using the workflow token
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

For Dockerfiles that need to fetch private Go modules during `go mod download`,
the shared image-build actions now expose the workflow token as a BuildKit
secret named `github_token`. A Dockerfile can opt into that secret with:

```dockerfile
RUN --mount=type=secret,id=github_token \
    git config --global http.https://github.com/.extraheader \
      "AUTHORIZATION: basic $(printf 'x-access-token:%s' \"$(cat /run/secrets/github_token)\" | base64 | tr -d '\n')" && \
    go mod download
```

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
      GO_BUILDER_IMAGE=golang:1.26-alpine
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
ghcr.io/evalops/service-runtime-go-builder:go1.26
```

A typical consumer Dockerfile can then start with:

```dockerfile
FROM ghcr.io/evalops/service-runtime-go-builder:go1.26 AS builder
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
ratelimit/     HTTP rate limiting middleware
rterrors/      Structured error handling and recovery
pgxpoolutil/   pgxpool bootstrap
testutil/      shared HTTP/auth/Postgres test helpers
mtls/          Shared mTLS client/server helpers
identityclient/ Shared Identity introspection client
images/        Shared builder image definitions
```

## Development

### Prerequisites

- Go (see `go.mod` for the required version)
- [golangci-lint](https://golangci-lint.run/welcome/install/) v2+

### Setup

After cloning the repo, install the pre-commit hook:

```bash
make install-hooks
```

This copies `scripts/pre-commit` into `.git/hooks/` so that `golangci-lint`
and `go test -race` run automatically on every commit that touches Go files.

### Lint

```bash
make lint
```

Runs the full `golangci-lint` suite configured in `.golangci.yml`. The
enabled linters catch type-assertion errors (`errcheck`), shadow variables
(`govet`), dead code (`staticcheck`, `unused`), and security patterns
(`gosec`), among others.

### Test

```bash
make test
```

Runs all tests with the race detector enabled.

### Adopting in other repos

To bring the same lint and hook setup to another EvalOps service:

1. Copy `.golangci.yml`, `Makefile`, and `scripts/pre-commit` from this repo.
2. Run `make install-hooks`.
3. Adjust linter settings in `.golangci.yml` if the service has different needs.
