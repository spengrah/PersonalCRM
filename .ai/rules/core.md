# Core Rules

These rules apply to all AI agents working on this project.

## Absolute Rules (Never Violate)

1. **Never use `time.Now()`** → Use `accelerated.GetCurrentTime()`
2. **Never write raw SQL in Go** → Use sqlc-generated queries (`make sqlc`)
3. **Never skip layers** → Handler → Service → Repository → DB
4. **Never use npm/npx** → Use bun/bunx
5. **Never call queries from handlers** → Go through repository
6. **Always sign commits** → `git commit -S -m "..."`
7. **Always add comprehensive tests** → Unit for logic, integration for DB, E2E for flows

## Code Quality

- Keep solutions simple and direct
- Prefer boring, readable code over clever abstractions
- Do not over-engineer or add unrequested features
- Run lint/format after code changes
- Run tests to verify changes work during development

## Testing Requirements

- **Always add comprehensive tests** for new/changed code
- Run `make test && make test-e2e` to verify changes work
- Unit tests for business logic, integration tests for DB operations, E2E for user flows

## Pre-push Hooks

Git pre-push hooks run automatically and may block push:

- **Tests**: Runs lint and all test suites. Push blocked if tests fail.
- **Learnings**: Extracts session learnings. If new learnings found, push blocked with instructions to review and apply them.

When push is blocked by learnings extraction:
1. Read the new learnings in `.ai/log/learnings/`
2. For each actionable learning, decide if/where to apply it
3. Commit any applied changes plus the learnings files
4. Push again

## Code Review Approval Criteria

PRs must meet ALL of these to pass review:
- No security concerns (SQL injection, XSS, auth issues, secrets)
- No bugs or unhandled edge cases
- Comprehensive test coverage for new/changed code
- Follows repository conventions (this file)
- Proper error handling and validation
- No TODOs or technical debt introduced

## Git Practices

- Use conventional commits (feat:, fix:, docs:, refactor:, test:, chore:)
- First line under 72 characters
- Commit logical units of work, not partial changes
- Use conventional branches (feat/, fix/, refactor/, docs/, test/, chore/)

## Layered Architecture

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

## Common Gotchas

| Mistake | Fix |
|---------|-----|
| `make test` from subdirectory | Run `make` commands from project root |
| `go test ./backend/...` | Use `make test-unit` or `cd backend && go test` |
| `npm install` | Use `bun install` |
| `sqlc generate` | Use `make sqlc` (sqlc is in ~/go/bin) |
| Calling `queries.X()` from handler | Call `repo.X()` instead |
| Using `time.Now()` | Use `accelerated.GetCurrentTime()` |
| Missing `deleted_at IS NULL` in queries | All queries must filter soft deletes |
| Comparing errors with `==` | Use `errors.Is(err, db.ErrNotFound)` |
| Querying DB directly | `docker exec crm-postgres psql -U crm_user -d personal_crm -c "..."` |
| Assuming repository method names | Read the repository file first |
| Not building after `make sqlc` | Always run `go build ./cmd/crm-api` to verify compilation |
| sqlc changed types after regeneration | Update repository to use `pgtype.X{Value: v, Valid: true}` wrappers |
| Assuming all tables have `updated_at` | Only contact, contact_method, note, time_entry, calendar_event have it |
| `git add -A` includes binaries | Review `git status` before commit, exclude `backend/crm-api` |
| Merging PRs with UI changes | Never merge UI PRs autonomously - wait for human review |
| `git add path/[id]/file` fails | Use quotes: `git add "path/[id]/file"` (bash interprets brackets as globs) |
| Fixing only one instance of a pattern | Search entire codebase and fix ALL instances to maintain consistency |
| Creating prototype HTML in repo root | Place prototypes in `temp/` (git-ignored), attach to issues for reference |
| Expecting soft-delete to cascade to related records | Soft-delete (UPDATE deleted_at) does NOT trigger ON DELETE CASCADE - explicitly delete related records first |
| Building multi-step wizard modals | Use single-view scrollable modals (like ImportLinkModal) - all steps visible in one view |

## Anti-Patterns

### Never Do These

```go
// ❌ WRONG - time.Now() breaks time acceleration
now := time.Now()

// ✅ CORRECT
now := accelerated.GetCurrentTime()
```

```go
// ❌ WRONG - raw SQL in Go
rows, err := db.Query("SELECT * FROM contact WHERE id = ?", id)

// ✅ CORRECT - use sqlc
contact, err := queries.GetContact(ctx, id)
```

```go
// ❌ WRONG - handler calling queries directly
contact, err := queries.GetContact(ctx, id)

// ✅ CORRECT - go through repository
contact, err := h.contactRepo.GetContact(ctx, id)
```

```tsx
// ❌ WRONG - leading-7 with truncate clips descenders (y, g, j, p, q)
<h2 className="leading-7 sm:text-3xl sm:truncate">Gregory</h2>

// ✅ CORRECT - leading-normal provides adequate line height
<h2 className="leading-normal sm:text-3xl sm:truncate">Gregory</h2>
```

## Error Handling

```go
// Proper error wrapping
if err != nil {
    return fmt.Errorf("create contact: %w", err)
}

// Proper error comparison
if errors.Is(err, db.ErrNotFound) {
    api.SendNotFound(c, "Contact")
    return
}
```

## Soft Deletes

All queries must filter `WHERE deleted_at IS NULL`. This is enforced in sqlc queries.

**Important:** Soft-delete (`UPDATE deleted_at = NOW()`) does NOT trigger FK cascades. When soft-deleting a parent record (e.g., contact), you must explicitly delete or reassign related records (e.g., contact_methods, notes) first. The ON DELETE CASCADE constraint only fires on actual DELETE statements.
