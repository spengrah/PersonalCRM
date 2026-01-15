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

## Search Query Pattern

```sql
-- name: SearchContacts :many
SELECT * FROM contact
WHERE
    deleted_at IS NULL
    AND (
        full_name ILIKE '%' || $1 || '%'
        OR email ILIKE '%' || $1 || '%'
    )
ORDER BY created_at DESC
LIMIT $2 OFFSET $3;
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

## Key Rules

1. **Always filter soft deletes:** `WHERE deleted_at IS NULL`
2. **Use parameterized queries:** sqlc enforces this automatically
3. **Use `:one`, `:many`, or `:exec`:** Match return type to query
4. **Return `*` for mutations:** Use `RETURNING *` for creates/updates
5. **Add indexes:** For foreign keys and commonly queried fields

## Regenerating Code

After modifying SQL files:

```bash
make sqlc
```

This generates type-safe Go code in `backend/internal/db/`.
