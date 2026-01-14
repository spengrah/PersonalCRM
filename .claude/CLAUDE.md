# PersonalCRM - Claude Code Context

## Tech Stack
- **Backend:** Go 1.24 + Gin + PostgreSQL 16 + sqlc (not an ORM)
- **Frontend:** Next.js 15 + React 19 + TailwindCSS 4 + TanStack Query
- **Package Manager:** bun (never npm)
- **Testing:** Go testing + Playwright E2E
- **Styling:** TailwindCSS with clsx for conditional classes

📖 *Detailed: @.ai/architecture.md*

## Development Commands

```bash
# Start development
make dev                 # Start dev servers (Docker PostgreSQL)
make dev-native          # Start without Docker (for containerized envs)
make dev-stop            # Stop dev servers
make dev-api-restart     # Restart just the backend

# Testing
make test-unit           # Backend unit tests (fast, no DB needed)
make test-integration    # Backend integration tests (needs DB)
make test-frontend       # Frontend unit tests
make test-e2e            # Playwright E2E tests
make smoke-test          # Full system verification

# Code generation & linting
make sqlc                # Regenerate Go from SQL (after query changes)
make lint                # Run all linters
```

📖 *Detailed: @.ai/development.md*

## Development

**Before pushing any changes**, run the full test suite locally:
```bash
make test                # All backend tests (unit + integration)
make test-e2e            # Playwright E2E tests
```

This catches issues that CI would flag, avoiding multiple push-fix-push cycles.

**Integration tests suffice for unit tests** when unit tests would require heavy mock infrastructure. If the codebase doesn't have mock interfaces for repositories, write integration tests that exercise the real code path rather than creating mock infrastructure for a single test file.

**Before using repository methods**, read the actual repository code to verify method names and signatures. Don't assume - grep or read the file first.

## Git

Commits must be signed. Pre-commit hook auto-formats code (gofmt, prettier) and re-stages - it never blocks.

```bash
git commit -S -m "feat: description"
```

**After creating a PR or pushing commits:** Always monitor CI status and reviewer comments using `gh pr checks` and `gh pr view --comments`. Address any failures or feedback promptly.

📖 *Detailed: @.github/README.md*

## Absolute Rules

1. **Never use `time.Now()`** → Use `accelerated.GetCurrentTime()`
2. **Never write raw SQL in Go** → Use sqlc-generated queries
3. **Never skip layers** → Handler → Service → Repository → DB
4. **Never use npm/npx** → Use bun/bunx
5. **Never call queries from handlers** → Go through repository
6. **Always sign commits** → `git commit -S -m "..."`
7. **Always add comprehensive tests** → Unit tests for logic, integration tests for DB operations, E2E tests for user flows. Cover edge cases and verify unrelated data is unaffected

📖 *Detailed: @.ai/rules.md*

## Key Patterns

**Error handling in handlers:**
```go
if errors.Is(err, db.ErrNotFound) {
    api.SendNotFound(c, "Contact")
    return
}
```

**Error propagation in services:** When a service method performs multiple operations, don't silently ignore failures. Either fail-fast on the first error, or collect all errors and return them to the caller. Users should know when operations partially fail.

**Soft deletes:** All queries must filter `WHERE deleted_at IS NULL`

📖 *Detailed: @.ai/patterns.md*

## Project Structure

```
backend/
├── cmd/crm-api/main.go          # Entry point
├── internal/
│   ├── api/handlers/            # HTTP handlers
│   ├── service/                 # Business logic
│   ├── repository/              # Data access layer
│   ├── db/queries/              # SQL files (sqlc input)
│   └── accelerated/             # Time functions (use this!)
├── migrations/                  # SQL migrations (up + down)
└── tests/{unit,integration,api}/

frontend/
├── src/
│   ├── app/                     # Next.js App Router pages
│   ├── components/              # React components
│   ├── hooks/                   # React Query hooks
│   └── lib/                     # API client, query keys, form-classes
└── tests/e2e/                   # Playwright tests
```

📖 *Detailed: @.ai/architecture.md, @.ai/development.md*

## Common Gotchas

| Mistake | Fix |
|---------|-----|
| `go test ./backend/...` | Use `make test-unit` or `cd backend && go test` |
| `npm install` | Use `bun install` |
| `sqlc generate` | Use `make sqlc` (sqlc is in ~/go/bin) |
| Calling `queries.X()` from handler | Call `repo.X()` instead |
| Using `time.Now()` | Use `accelerated.GetCurrentTime()` |
| Missing `deleted_at IS NULL` in queries | All queries must filter soft deletes |
| Comparing errors with `==` | Use `errors.Is(err, db.ErrNotFound)` |
| Querying DB directly | `docker exec crm-postgres psql -U crm_user -d personal_crm -c "..."` |
| Assuming repository method names | Read the repository file first - e.g., `SoftDeleteContact` not `DeleteContact` |

📖 *Detailed: @.ai/reviewers.md*

## UI Prototyping

Before implementing new UI components in React, create standalone HTML preview files to explore design options. This allows rapid visual iteration without build cycles.

**When to use:** New forms, dashboard widgets, data displays, or any UI with multiple valid approaches.

**Process:**
1. Create HTML file in `/temp` (gitignored) with Tailwind CSS via CDN
2. Show multiple design options side-by-side with labels
3. Get user approval before implementing in React

📖 *Detailed: @.ai/development.md (Section 8)*
