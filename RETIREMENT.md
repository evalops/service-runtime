# Standalone Service Runtime Retirement

`evalops/service-runtime` is no longer the active source of truth for shared
EvalOps runtime code. Active runtime work belongs in `evalops/platform`.

## Source Of Truth

| Concern | Location |
| --- | --- |
| Shared runtime packages | `evalops/platform` `pkg/` |
| Runtime commands and service integration points | `evalops/platform` `cmd/` and `internal/` |
| Drift and consolidation tracking | `evalops/platform` `docs/repositories/consolidation.json` |
| Wave 2 tracker | `evalops/platform#1768` |

## Allowed Changes Here

- Critical Go module compatibility fixes while consumers finish moving to
  Platform-owned runtime packages.
- README, issue-routing, and repository metadata updates that make the retired
  state clearer.
- Emergency release repair, with a matching Platform issue or PR link.

## Not Allowed Here

- New shared runtime APIs that do not originate in Platform.
- New service templates, generated package surfaces, or build images that make
  this repository look like the active runtime upstream again.
- Dependency churn without a Platform consolidation reference.

## Contributor Flow

1. Open runtime changes against `evalops/platform`.
2. Run the relevant Platform Go tests and consolidation checks there.
3. Link the Platform PR or issue from any temporary compatibility work in this
   repository.
4. Prefer archiving this repository once the Go module compatibility window is
   closed.
