# Service Runtime

Shared Go runtime helpers for EvalOps services.

Current scope:
- startup retry helpers
- PostgreSQL bootstrap helpers for `database/sql`
- Redis bootstrap helpers
- `pgxpool` bootstrap helpers

This module is intentionally narrow. It is for service startup and dependency
wiring, not business-domain code.
