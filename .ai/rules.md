# Agent Development Rules

**Read this before making any changes to the codebase.**

---

## Repository Overview

**Personal CRM** is a single-user, local-first CRM system built for privacy and personal use.

**Current State:**
- ✅ Core CRM features (contacts, reminders, time tracking)
- ✅ Production-ready architecture
- ⏳ AI features planned (embeddings, RAG, LLM summaries)

**Target Deployment:**
- Raspberry Pi backend (Go API + PostgreSQL)
- Access via Tailscale from laptop/phone
- Optional Mac native app (Tauri)
- LLM inference on MacBook M2 Max, not Pi

**Development Strategy:**
- Fork-first approach: build for your needs, contribute back later
- Focus on features that work for you
- Upstream contribution happens after features are stable and tested

---

## Architecture Principles

### 1. Layered Backend Architecture

```
HTTP Request
    ↓
Handler (HTTP concerns, validation, status codes)
    ↓
Service (business logic, orchestration)
    ↓
Repository (data access, type conversion)
    ↓
sqlc-generated DB layer (type-safe SQL)
    ↓
PostgreSQL
```

**Key Rule:** Never skip layers. Handlers should not call DB directly.

### 2. Type Safety Everywhere

- Go's strong typing
- sqlc for compile-time SQL safety (NOT an ORM)
- TypeScript in frontend (not JavaScript)
- Zod for runtime validation in frontend

### 3. Technology Stack

**Backend:**
- Go 1.24, Gin framework
- PostgreSQL 16 with pgvector
- sqlc (NOT an ORM - writes type-safe Go from SQL)
- golang-migrate for migrations
- robfig/cron for scheduler

**Frontend:**
- Next.js 15 (React 19, App Router)
- TailwindCSS 4
- TanStack Query (React Query) for server state
- Zod + React Hook Form for validation

**Infrastructure:**
- Docker Compose (PostgreSQL)
- Go's `testing` package, Playwright for E2E
- Tauri (Rust wrapper, optional)

---

## Issue-Driven Development Workflow

### 1. Pick Up Work

```bash
# Find ready tasks
gh issue list --label "agent-ready" --state open

# View issue details
gh issue view 123

# Create branch from issue
gh issue develop 123 --name "feat/auto-migrations"
```

### 2. Git Workflow Rules

**NEVER commit directly to main.** All work must happen in issue-specific branches.

**Branch naming convention (Conventional Commits style):**

- `feat/short-description` - New features (from "feature" issues)
- `fix/bug-description` - Bug fixes (from "bug" issues)
- `refactor/refactor-name` - Code refactoring (from "refactor" issues)
- `docs/doc-update` - Documentation only changes
- `test/add-tests` - Adding or updating tests
- `chore/deps-update` - Maintenance (dependencies, config, tooling)
- `perf/optimize-query` - Performance improvements

**Issue type → Branch prefix mapping:**
- Feature issue → `feat/`
- Bug issue → `fix/`
- Refactor issue → `refactor/`
- Improvement issue → Choose based on change type: `feat/`, `refactor/`, `perf/`, or `chore/`

**Workflow:**
1. Create branch from issue: `gh issue develop 123 --name "feat/auto-migrations"`
2. Make changes and commit with conventional commit messages
3. **Always sign commits** with `-S` flag: `git commit -S -m "..."`
4. Reference issue in commits: `Fixes #123` or `Closes #123`
5. Create PR: `gh pr create --fill`
6. PR automatically links to issue and closes it on merge. If there are multiple commits, they will be squashed into a single commit on merge.

### 3. Before Starting Any Work

1. **Read the context:**
   - **Existing code first** - The code is the source of truth
   - Check relevant test files to understand expected behavior
   - Check `PLAN.md` for historical context (may be outdated)
   - Read issue comments for additional context

2. **Understand the feature:**
   - Does it require database schema changes?
   - What layers are affected? (handlers, services, repos, frontend)

3. **Plan the implementation:**
   - Start with database schema if needed
   - Then repository layer
   - Then service layer
   - Then handlers
   - Finally frontend
   - **Always** write tests

### 4. Development Commands

See [README.md](../README.md#development-commands) for full command reference.

**Most used:**
```bash
make dev                    # Start everything
make test-unit              # Unit tests
make test-integration       # Integration tests
./smoke-test.sh            # Full system test

make testing                # Ultra-fast cadences (testing)
make staging                # Fast cadences (hours)
make prod                   # Real-world timing
```

### 4. Code Style

**Go:**
- Follow standard Go formatting (`gofmt`, `goimports`)
- Error messages: lowercase, no punctuation
- Group imports: stdlib, external, internal

**TypeScript/React:**
- Use functional components with hooks
- Prefer `'use client'` directive only when needed
- Use TanStack Query for all API calls
- Keep components small and focused

**SQL:**
- All queries in `backend/internal/db/queries/*.sql`
- Use sqlc comments: `-- name: FunctionName :one` or `:many`
- Always use parameterized queries (sqlc enforces this)
- Add indexes for foreign keys and commonly queried fields

---

## Critical Rules and Anti-Patterns

### ❌ DO NOT Do These Things

1. **Never use `time.Now()` directly**
   ```go
   // ❌ WRONG
   now := time.Now()
   
   // ✅ CORRECT
   now := accelerated.GetCurrentTime()
   ```
   *Reason:* Breaks time acceleration feature for testing

2. **Never write raw SQL in Go code**
   ```go
   // ❌ WRONG
   rows, err := db.Query("SELECT * FROM contact WHERE id = ?", id)
   
   // ✅ CORRECT
   contact, err := queries.GetContact(ctx, id)
   ```
   *Reason:* No type safety, potential SQL injection, defeats purpose of sqlc

3. **Never call database queries from handlers**
   ```go
   // ❌ WRONG (in handler)
   contact, err := queries.GetContact(ctx, id)
   
   // ✅ CORRECT (in handler)
   contact, err := h.contactRepo.GetContact(ctx, id)
   ```
   *Reason:* Violates layered architecture

4. **Never use ORMs**
   - This codebase intentionally avoids ORMs
   - Uses sqlc for type-safe SQL instead
   - Performance matters (runs on Raspberry Pi)

5. **Never hardcode secrets**
   ```go
   // ❌ WRONG
   apiKey := "sk-1234567890"
   
   // ✅ CORRECT
   apiKey := os.Getenv("API_KEY")
   ```

6. **Never skip migrations**
   - Always create up AND down migrations
   - Test migrations before committing

7. **Never commit `.env` files**
   - Already in `.gitignore`
   - Use `.env.example` as template

8. **Never use `console.log` in frontend production code**
   ```typescript
   // ❌ WRONG
   console.log("User data:", user)
   
   // ✅ CORRECT
   // Remove or use proper logging
   ```

### ✅ DO Do These Things

1. **Always write tests for new features**
   - Minimum: unit tests for business logic
   - Ideal: unit + integration + E2E

2. **Always handle errors properly**
   ```go
   // ✅ GOOD
   if err != nil {
       return fmt.Errorf("create contact: %w", err)
   }
   ```

3. **Always validate input**
   - Backend: use validator struct tags
   - Frontend: use Zod schemas

4. **Always use context for database operations**
   ```go
   // ✅ CORRECT
   contact, err := repo.GetContact(ctx, id)
   ```

5. **Always check for null/undefined in frontend**
   ```typescript
   // ✅ GOOD
   {contact.email && <a href={`mailto:${contact.email}`}>{contact.email}</a>}
   ```

6. **Always use React Query for API calls**
   - Provides caching, loading states, error handling
   - Never use raw `fetch()` in components

7. **Always run smoke test before committing major changes**
   ```bash
   ./smoke-test.sh
   ```

8. **Always sign commits**
   ```bash
   git commit -S -m "feat: your message here"
   ```

---

## Testing Strategy

### Test Pyramid

```
        E2E Tests (Playwright)
       - Full user workflows
      - Browser automation
     - Slowest, run pre-deploy
    
       Integration Tests
      - DB + Repository layer
     - Real PostgreSQL
    - Run in CI
   
      Unit Tests
     - Pure functions
    - Mocked dependencies
   - Fastest, run frequently
```

### When to Write What

**Unit Tests:**
- Business logic calculations (e.g., cadence calculations)
- Validation logic
- Utility functions
- Handler response formatting

**Integration Tests:**
- Repository CRUD operations
- Database constraints
- Transaction handling
- Migration correctness

**E2E Tests:**
- Critical user flows (create contact → view → delete)
- Navigation
- Form submissions
- Error states

### Test File Locations

```
backend/tests/
  ├── unit/           # Fast, isolated tests
  ├── integration/    # Database tests
  └── api/            # HTTP endpoint tests

tests/e2e/            # Playwright browser tests
```

---

## Debugging Tips

### Check Service Status

```bash
make status                # Overall health
curl http://localhost:8080/health  # Backend health
curl http://localhost:8080/api/v1/contacts  # API test
```

### View Logs

```bash
# Smoke test logs
cat smoke-test.log

# Development logs
tail -f logs/backend-dev.log
tail -f logs/frontend-dev.log

# Docker logs
docker logs crm-postgres
```

### Database Inspection

```bash
# Connect to database
docker exec -it crm-postgres psql -U crm_user -d personal_crm

# Useful queries
\dt                    # List tables
\d contact            # Describe table
SELECT * FROM contact LIMIT 5;
```

### Common Issues

**"DATABASE_URL not set"**
- Source `.env`: `source .env` or use `make` commands

**"Port already in use"**
- Kill processes: `pkill -f crm-api` or `make stop`

**"Migrations not applied"**
- Run migrations: They should auto-run on startup (if implemented)

**"Frontend won't start"**
- Install dependencies: `cd frontend && npm install`

---

## Quick Reference

### File Structure

See [README.md](../README.md#project-structure) for full structure.

**Key locations for agents:**
```
backend/
  ├── cmd/crm-api/main.go              # Entry point
  ├── internal/
  │   ├── api/handlers/                # HTTP handlers
  │   ├── db/queries/                  # SQL files (sqlc input)
  │   ├── repository/                  # Data access layer
  │   ├── service/                     # Business logic
  │   └── accelerated/                 # Time acceleration
  ├── migrations/                      # SQL migrations
  └── tests/                           # Backend tests

frontend/
  ├── src/
  │   ├── app/                         # Next.js pages (App Router)
  │   ├── components/                  # React components
  │   ├── hooks/                       # Custom hooks
  │   └── lib/                         # API clients, utils

tests/e2e/                             # Playwright E2E tests
```

### Important Files

- **[`.ai/development.md`](./development.md)** - Feature development guide
- **[`.ai/patterns.md`](./patterns.md)** - Common code patterns
- **[`.ai/architecture.md`](./architecture.md)** - Architecture rationale
- **[`.github/README.md`](../.github/README.md)** - Issue workflow
- **[`PLAN.md`](../PLAN.md)** - Historical context (may be outdated)
- **[`TEST_GUIDE.md`](../TEST_GUIDE.md)** - Testing documentation
- **[`README.md`](../README.md)** - User documentation

---

## Summary

**Core Philosophy:**
- Local-first, privacy-first
- Type-safe at compile time
- Layered architecture (no shortcuts)
- Test everything
- Optimize for Raspberry Pi constraints

**When in doubt:**
1. Read existing code for patterns
2. Follow the layered architecture
3. Write SQL in `.sql` files, not Go
4. Use `accelerated.GetCurrentTime()` not `time.Now()`
5. Write tests
6. Run `./smoke-test.sh` before committing

**For complex features:**
- Start with database schema
- Build up through layers (repo → service → handler → frontend)
- Test each layer independently
- Integration test the whole flow

**Remember:**
- This is personal software (quality over perfection)
- But it should work reliably (it's running on a Pi 24/7)
- And it may be contributed back (follow best practices)

---

*For detailed feature development process, see [`.ai/development.md`](./development.md)*

*For common code patterns, see [`.ai/patterns.md`](./patterns.md)*

*For architecture rationale, see [`.ai/architecture.md`](./architecture.md)*

