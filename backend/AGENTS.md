# Backend Rules

Follow [core rules](../.ai/rules/core.md). These are the essential Go/Gin/PostgreSQL/sqlc constraints.

- Use pinned generators through `make sqlc`, `make api-types`, and `make api-docs`. Verify compilation after query/type regeneration.
- Handlers never call sqlc directly. Read repository APIs and reuse existing conversion helpers.
- Use deterministic ordering and explicit tie-breakers; handle null and empty-string semantics deliberately.
- Preserve unrelated JSONB metadata when updating keys.
- Publish transaction-bound events before mutation; commit before external HTTP calls. Start River with the root context.
- Use synthetic factories/harnesses and namespace isolation for tests. Follow the [testing rules](../.ai/rules/testing.md) for shared databases, River isolation, and parallelism.

Read the relevant entries in [backend troubleshooting](../.ai/guides/backend-troubleshooting.md)
when changing SQL/schema, repositories, OAuth or sync providers, transactions,
or integration-test infrastructure. It includes sqlc overrides, generated-artifact
pitfalls, provider-specific behavior, deduplication, and worktree PostgreSQL recovery.

For contact deletion/merge, derived fields, migrations, or cross-layer contracts,
also read the [change checklist](../.ai/guides/change-checklist.md).
