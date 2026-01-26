# Agent Instructions

Read and follow: `.ai/rules/core.md`

## About This Repo

Personal CRM: single-user, local-first CRM for privacy-focused personal use.
Target deployment: Raspberry Pi backend, access via Tailscale.

**Stack:** Go 1.24 + Gin + PostgreSQL 16 + sqlc | Next.js 15 + React 19 + TailwindCSS 4 | bun (never npm)

**Structure:**
```
backend/internal/{api/handlers, service, repository, db/queries}
frontend/src/{app, components, hooks}
```

## Commands

From project root:

```bash
make setup          # First-time setup (install deps, git hooks)
make dev            # Start dev server
make test           # All backend tests
make test-e2e       # Playwright E2E (for ci)
make test-e2e-diff  # Diff-selected E2E (local default)
make sqlc           # Regenerate from SQL
make lint           # Run all linters
make help           # Full command reference
```

## Context Discovery

Load as needed, not upfront:
- Architecture decisions: `.ai/guides/architecture.md`
- Feature development: `.ai/guides/feature-development.md`
- Code patterns: `.ai/patterns/`
- Testing rules: `.ai/rules/testing.md`
- Code review standards: `.ai/rules/code-review.md`

## Quick Symbol Searches

Find all instances of a layer:
- All handlers: `type *Handler struct`
- All services: `type *Service struct`
- All repositories: `type *Repository struct`
- All sync providers: `providerRegistry.Register`

## Key File Locations

| What | Where |
|------|-------|
| API routes | `backend/cmd/crm-api/main.go` (search for `v1.Group`) |
| Scheduler/cron jobs | `backend/internal/scheduler/scheduler.go` |
| Time acceleration | `backend/internal/accelerated/time.go` |
| Query invalidation | `frontend/src/lib/query-invalidation.ts` |
| Query keys | `frontend/src/lib/query-keys.ts` |
| Fuzzy matching | `backend/internal/matching/` |

## Session Hints

- Run `make test && make test-e2e-diff` to verify changes work (also runs automatically on push)
- Read repository code before using methods (names vary, e.g., `SoftDeleteContact` not `DeleteContact`)
- Prefer integration tests over heavy mocking
- Use `accelerated.GetCurrentTime()` not `time.Now()`
