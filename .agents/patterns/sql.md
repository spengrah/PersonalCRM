# SQL Patterns

sqlc query patterns for type-safe database access.

## Basic CRUD Queries

```sql
-- Get one (with soft delete check)
-- name: GetContact :one
SELECT * FROM contact
WHERE id = $1 AND deleted_at IS NULL
LIMIT 1;

-- List with pagination
-- name: ListContacts :many
SELECT * FROM contact
WHERE deleted_at IS NULL
ORDER BY created_at DESC
LIMIT $1 OFFSET $2;

-- Create
-- name: CreateContact :one
INSERT INTO contact (
    full_name, email, phone
) VALUES (
    $1, $2, $3
) RETURNING *;

-- Update
-- name: UpdateContact :one
UPDATE contact
SET
    full_name = $2,
    email = $3,
    updated_at = NOW()
WHERE id = $1 AND deleted_at IS NULL
RETURNING *;

-- Soft delete
-- name: SoftDeleteContact :exec
UPDATE contact
SET deleted_at = NOW()
WHERE id = $1 AND deleted_at IS NULL;
```

## Optional-Search Query Pattern

Fold search into the listing query with `sqlc.narg` (NULL = no search) instead
of maintaining a separate search variant — see `ListContacts` in
`contact.sql` for the real full-text version:

```sql
-- name: ListWidgets :many
SELECT * FROM widget
WHERE
    deleted_at IS NULL
    AND (sqlc.narg(search_query)::text IS NULL
         OR name ILIKE '%' || sqlc.narg(search_query)::text || '%')
ORDER BY name ASC, id ASC
LIMIT sqlc.arg(page_limit) OFFSET sqlc.arg(page_offset);
```

## Aggregate Query Pattern

```sql
-- name: CountContactsByCadence :many
SELECT
    cadence,
    COUNT(*) as count
FROM contact
WHERE deleted_at IS NULL
GROUP BY cadence
ORDER BY count DESC;
```

## Deduplication Window Queries

When implementing time-window deduplication (e.g., "find if this event already exists within ±30 minutes"), the WHERE clause must include ALL semantically-significant dimensions, not just entity + time.

**Example:** An interaction dedup query needs:
- Entity (contact_id)
- Time window (occurred_at BETWEEN @start AND @end)
- Source (to distinguish manual vs automated)
- **Direction** (to distinguish outbound vs inbound)

**Why:** Without all dimensions, false positives occur. A user logging an outbound interaction followed by an inbound interaction on the same contact within 30 minutes would incorrectly match the first interaction despite different directions.

**Pattern:**
```sql
-- name: FindInteractionInWindow :one
SELECT * FROM interaction
WHERE contact_id = $1
  AND occurred_at BETWEEN $2 AND $3
  AND source = $4
  AND direction = $5  -- Don't forget semantic dimensions!
  AND deleted_at IS NULL
LIMIT 1;
```

**Cross-cutting impact:** When adding a semantic column to a table with existing dedup logic:
1. Audit all FindInWindow queries to include the new dimension
2. Update unique constraints if applicable
3. Update repository method signatures
4. Update all service-layer call sites
5. Run `make sqlc`
6. Add regression test for the false-positive case

## Key Rules

1. **Always filter soft deletes:** `WHERE deleted_at IS NULL`
2. **Use parameterized queries:** sqlc enforces this automatically
3. **Use `:one`, `:many`, or `:exec`:** Match return type to query
4. **Return `*` for mutations:** Use `RETURNING *` for creates/updates
5. **Add indexes:** For foreign keys and commonly queried fields
6. **Include all semantic dimensions in dedup queries:** Entity + time is not enough if events can differ in other meaningful ways

## Regenerating Code

After modifying SQL files:

```bash
make sqlc
```

This generates type-safe Go code in `backend/internal/db/`.
