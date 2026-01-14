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

```bash
make dev          # Start dev server
make dev-native   # Start without Docker (for containerized envs)
make test         # All backend tests
make test-e2e     # Playwright E2E
make sqlc         # Regenerate from SQL
make lint         # Run all linters
```

## Context Discovery

Load as needed, not upfront:
- Architecture decisions: `.ai/guides/architecture.md`
- Feature development: `.ai/guides/feature-development.md`
- Code patterns: `.ai/patterns/`
- Testing rules: `.ai/rules/testing.md`
- Code review standards: `.ai/rules/code-review.md`

## Session Hints

- Run `make test && make test-e2e` before pushing
- Read repository code before using methods (names vary, e.g., `SoftDeleteContact` not `DeleteContact`)
- Prefer integration tests over heavy mocking
- Use `accelerated.GetCurrentTime()` not `time.Now()`
