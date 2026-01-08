# PersonalCRM Continuity Ledger

## Project Overview

Single-user, local-first Personal CRM for privacy-focused personal use. Self-hosted on Raspberry Pi with PostgreSQL.

**Owner context:** AI-driven development - Claude handles most coding. Many extensions and features planned.

## Tech Stack

| Layer | Tech |
|-------|------|
| Backend | Go 1.24, Gin framework, sqlc |
| Database | PostgreSQL 16 + pgvector |
| Frontend | Next.js 15, React 19, TailwindCSS 4 |
| Data fetching | TanStack Query |
| Forms | React Hook Form + Zod |
| Testing | Playwright E2E |
| Package manager | bun (not npm) |
| Infrastructure | Docker Compose |

## Architecture

```
Handler -> Service -> Repository -> sqlc -> PostgreSQL
```

**Key directories:**
- `backend/` - Go API server
- `frontend/` - Next.js app
- `backend/internal/api/handlers/` - HTTP handlers
- `backend/internal/services/` - Business logic
- `backend/internal/repository/` - Data access (sqlc-generated)
- `backend/db/migrations/` - SQL migrations
- `backend/db/queries/` - sqlc query definitions

## Features (Current State)

**Complete:**
- Contacts CRUD with external identities
- Reminders with recurrence
- Time tracking
- Google Calendar sync (OAuth2)
- Birthday tracking
- Import from CSV/JSON

**Planned (AI features):**
- Embeddings with pgvector
- RAG for contact context
- LLM-generated summaries

## Database

16 migrations covering:
- contacts, reminders, time_entries
- calendar_events, calendar_sync_state
- external_identities, external_sources
- oauth_tokens, user_settings

## Frontend Pages

- `/` - Dashboard
- `/contacts` - List, `/contacts/[id]` - Detail, `/contacts/new` - Create
- `/reminders` - Reminders
- `/time-tracking` - Time entries
- `/birthdays` - Birthday list
- `/settings` - App settings
- `/imports` - Data import

## Commands

```bash
# Backend
go build ./...
go test ./...
make run-backend

# Frontend
bun install
bun run dev
bun run build
bun run test:e2e

# Database
docker-compose up -d postgres
sqlc generate
```

## Key Files for Common Tasks

| Task | Files |
|------|-------|
| Add API endpoint | `backend/internal/api/handlers/`, `backend/internal/api/routes.go` |
| Add DB table | `backend/db/migrations/`, `backend/db/queries/`, then `sqlc generate` |
| Add frontend page | `frontend/src/app/` (App Router) |
| Add API hook | `frontend/src/hooks/` |
| Modify service logic | `backend/internal/services/` |

## Guidelines

- See `.ai/rules.md` for development rules
- See `.ai/reviewers.md` for code review standards
- See `AGENTS.md` for agent-specific context
- Use `bun` not `npm` for frontend

## Session Notes

### 2026-01-07: Contact Notes Feature (GH Issue #111)

**Problem:** Frontend had notes field but backend never implemented it. Notes silently ignored.

**Solution - Full stack implementation:**
1. Migration `018_add_contact_notes` - adds `notes TEXT` column to contact table
2. Updated sqlc queries (`contact.sql`) - CreateContact and UpdateContact include notes
3. Repository layer - added Notes field to structs and conversion functions
4. Handler layer - added Notes to request/response structs with `max=2000` validation

**Files changed:**
- `backend/migrations/018_add_contact_notes.up.sql` (new)
- `backend/migrations/018_add_contact_notes.down.sql` (new)
- `backend/internal/db/queries/contact.sql`
- `backend/internal/repository/contact.go`
- `backend/internal/api/handlers/contact.go`

**Tests added:**
- Backend API: 6 tests in `TestContactAPI_NotesField` (create, validation, update, get)
- E2E: `should create and edit contact with notes` in contacts.spec.ts

**Status:** Implementation complete, all tests passing. Ready to commit.

**Branch:** `continuous-claude-setup` (also contains Continuous Claude setup)

**Continuous Claude hook fixes (same session):**
- Fixed bundling bug in `pre-compact-continuity.mjs` (CLI check incorrectly triggering)
- Fixed path resolution in all shell wrappers (set CLAUDE_PROJECT_DIR before cd)

---
*Created: 2026-01-07*
*Last updated: 2026-01-07*

## Agent Reports

### onboard (2026-01-08T14:46:15.089Z)
- Task: 
- Summary: 
- Output: `.claude/cache/agents/onboard/latest-output.md`

### onboard (2026-01-08T14:40:10.555Z)
- Task: 
- Summary: 
- Output: `.claude/cache/agents/onboard/latest-output.md`

### onboard (2026-01-08T14:39:50.858Z)
- Task: 
- Summary: 
- Output: `.claude/cache/agents/onboard/latest-output.md`

### onboard (2026-01-08T14:18:42.248Z)
- Task: 
- Summary: 
- Output: `.claude/cache/agents/onboard/latest-output.md`

### onboard (2026-01-08T00:26:11.687Z)
- Task: 
- Summary: 
- Output: `.claude/cache/agents/onboard/latest-output.md`

### onboard (2026-01-07T22:32:07.675Z)
- Task: 
- Summary: 
- Output: `.claude/cache/agents/onboard/latest-output.md`

### onboard (2026-01-07T22:27:36.352Z)
- Task: 
- Summary: 
- Output: `.claude/cache/agents/onboard/latest-output.md`

### onboard (2026-01-07T22:20:35.513Z)
- Task: 
- Summary: 
- Output: `.claude/cache/agents/onboard/latest-output.md`

### onboard (2026-01-07T22:20:30.252Z)
- Task: 
- Summary: 
- Output: `.claude/cache/agents/onboard/latest-output.md`

### onboard (2026-01-07T22:20:30.009Z)
- Task: 
- Summary: 
- Output: `.claude/cache/agents/onboard/latest-output.md`

### onboard (2026-01-07T22:17:51.868Z)
- Task: 
- Summary: 
- Output: `.claude/cache/agents/onboard/latest-output.md`

### onboard (2026-01-07T21:59:17.351Z)
- Task: 
- Summary: 
- Output: `.claude/cache/agents/onboard/latest-output.md`

### onboard (2026-01-07T21:57:44.810Z)
- Task: 
- Summary: 
- Output: `.claude/cache/agents/onboard/latest-output.md`

