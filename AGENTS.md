# Agent Instructions

Read and follow `.agents/rules/core.md`. If its contents are already loaded into the session, use that context rather than reading it again.

## About This Repo

Personal CRM: single-user, local-first CRM for privacy-focused personal use.
Target deployment: Raspberry Pi backend, access via Tailscale.

**Stack:** Go 1.25 + Gin + PostgreSQL 16 + sqlc | Next.js 15 + React 19 + TailwindCSS 4 | bun (never npm)

Run `make help` from project root for the full command reference.

## Context Discovery

Load as needed, not upfront:

- Architecture decisions: `.agents/guides/architecture.md`
- Feature development: `.agents/guides/feature-development.md`
- Code patterns: `.agents/patterns/`
- Code review standards: `.agents/rules/code-review.md`
- Behavior specs (intended-behavior SSOT): `spec/README.md`
- Cross-layer change checklist and troubleshooting: `.agents/guides/change-checklist.md`
- Domain troubleshooting: `.agents/guides/backend-troubleshooting.md`, `.agents/guides/frontend-troubleshooting.md` (read relevant entries only)

## Quick Symbol Searches

Find all instances of a layer:

- All handlers: `type *Handler struct`
- All services: `type *Service struct`
- All repositories: `type *Repository struct`
- All sync providers: `providerRegistry.Register`

## Key File Locations

| What                | Where                                                                                                                                                                              |
| ------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| API routes          | `backend/internal/api/handlers/*_routes.go` (per-domain `RegisterXRoutes`); gated call sites in `registerRoutes` in `backend/cmd/crm-api/routes.go` (search for `RegisterXRoutes`) |
| Scheduler/cron jobs | `backend/internal/scheduler/scheduler.go`                                                                                                                                          |
| Time acceleration   | `backend/internal/accelerated/time.go`                                                                                                                                             |
| Query invalidation  | `frontend/src/lib/query-invalidation.ts`                                                                                                                                           |
| Query keys          | `frontend/src/lib/query-keys.ts`                                                                                                                                                   |
| Fuzzy matching      | `backend/internal/matching/`                                                                                                                                                       |

## Session Hints

- Run focused tests appropriate to the change; pre-push runs static checks, and required CI suites gate merging.
- Read repository code before using methods (names vary, e.g., `SoftDeleteContact` not `DeleteContact`)
- Testing rules are in `.agents/rules/core.md` "Writing tests"; read them before writing or reviewing a test
- Use `accelerated.GetCurrentTime()` not `time.Now()`
- New worktrees link env files but do not install dependencies. For frontend work, run `make worktree-deps` when needed (main checkout: `cd frontend && bun install --frozen-lockfile`). Backend/docs-only work needs no frontend install.

## Where Knowledge Goes

Repo rules get anything another agent or environment needs. Private memory gets what is specific to one machine or account. If a fresh agent in a clean checkout would need it, commit it.

## Privacy

- **Never include PII in code, comments, commit messages, PR descriptions, GitHub issues, plan docs, or any other repo artifact.** PII = real contact full names, email addresses, phone numbers, contact UUIDs, account IDs, prod hostnames, anything that could identify a real person or system. Use placeholders ("contact A", "the affected contact", "two contacts on date X", `<prod-host>`), redact UUIDs, and keep specifics in private notes outside the repo. The repo is the user's own CRM and the data inside it is real — leaking PII into git history is irreversible.
- `.agents/log/` is gitignored (safe scratch space for raw prod observations and verify evidence). Design documents (specs, arcs, plans, reviews and the rest) live in the bosun memory store, reached at `.agents/docs/<class>/` (classes: spec, arc, plan, progress, exploration, review, roadmap, research, pr, archive). A fresh clone has no `.agents/docs/`, so no tracked file points there.
