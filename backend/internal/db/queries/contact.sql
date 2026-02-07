-- Contact queries

-- name: GetContact :one
SELECT * FROM contact 
WHERE id = $1 AND deleted_at IS NULL;

-- name: ListContacts :many
SELECT * FROM contact 
WHERE deleted_at IS NULL
LIMIT $1 OFFSET $2;

-- name: ListContactsSorted :many
SELECT * FROM contact
WHERE deleted_at IS NULL
ORDER BY
  CASE WHEN sqlc.arg(sort_field) = 'name' AND sqlc.arg(sort_order) = 'asc' THEN full_name END ASC,
  CASE WHEN sqlc.arg(sort_field) = 'name' AND sqlc.arg(sort_order) = 'desc' THEN full_name END DESC,
  CASE WHEN sqlc.arg(sort_field) = 'location' AND sqlc.arg(sort_order) = 'asc' THEN COALESCE(location, '') END ASC,
  CASE WHEN sqlc.arg(sort_field) = 'location' AND sqlc.arg(sort_order) = 'desc' THEN COALESCE(location, '') END DESC,
  CASE WHEN sqlc.arg(sort_field) = 'birthday' AND sqlc.arg(sort_order) = 'asc' THEN birthday END ASC NULLS LAST,
  CASE WHEN sqlc.arg(sort_field) = 'birthday' AND sqlc.arg(sort_order) = 'desc' THEN birthday END DESC NULLS FIRST,
  CASE WHEN sqlc.arg(sort_field) = 'last_contacted' AND sqlc.arg(sort_order) = 'asc' THEN last_contacted END ASC NULLS LAST,
  CASE WHEN sqlc.arg(sort_field) = 'last_contacted' AND sqlc.arg(sort_order) = 'desc' THEN last_contacted END DESC NULLS LAST,
  CASE WHEN sqlc.arg(sort_field) = 'contact_by' AND sqlc.arg(sort_order) = 'asc' THEN contact_by END ASC NULLS LAST,
  CASE WHEN sqlc.arg(sort_field) = 'contact_by' AND sqlc.arg(sort_order) = 'desc' THEN contact_by END DESC NULLS LAST,
  -- Cadence sort by frequency: weekly=1 (most frequent) to annual=6 (least frequent), null=7
  -- 'desc' = most frequent first (ASC on number), 'asc' = least frequent first (DESC on number)
  CASE WHEN sqlc.arg(sort_field) = 'cadence' AND sqlc.arg(sort_order) = 'desc' THEN
    CASE cadence WHEN 'weekly' THEN 1 WHEN 'biweekly' THEN 2 WHEN 'monthly' THEN 3 WHEN 'quarterly' THEN 4 WHEN 'biannual' THEN 5 WHEN 'annual' THEN 6 ELSE 7 END
  END ASC,
  CASE WHEN sqlc.arg(sort_field) = 'cadence' AND sqlc.arg(sort_order) = 'asc' THEN
    CASE cadence WHEN 'weekly' THEN 1 WHEN 'biweekly' THEN 2 WHEN 'monthly' THEN 3 WHEN 'quarterly' THEN 4 WHEN 'biannual' THEN 5 WHEN 'annual' THEN 6 ELSE 7 END
  END DESC,
  -- Secondary sort by name for cadence sorting
  CASE WHEN sqlc.arg(sort_field) = 'cadence' THEN full_name END ASC
LIMIT sqlc.arg(page_limit) OFFSET sqlc.arg(page_offset);

-- name: SearchContacts :many
SELECT c.* FROM contact c
LEFT JOIN (
  SELECT contact_id, string_agg(value, ' ') AS method_values
  FROM contact_method
  GROUP BY contact_id
) cm ON cm.contact_id = c.id
WHERE c.deleted_at IS NULL
  AND to_tsvector('english', c.full_name || ' ' || COALESCE(cm.method_values, '')) @@ plainto_tsquery('english', $1)
ORDER BY ts_rank(
  to_tsvector('english', c.full_name || ' ' || COALESCE(cm.method_values, '')),
  plainto_tsquery('english', $1)
) DESC
LIMIT $2 OFFSET $3;

-- name: SearchContactsSorted :many
SELECT c.* FROM contact c
LEFT JOIN (
  SELECT contact_id, string_agg(value, ' ') AS method_values
  FROM contact_method
  GROUP BY contact_id
) cm ON cm.contact_id = c.id
WHERE c.deleted_at IS NULL
  AND to_tsvector('english', c.full_name || ' ' || COALESCE(cm.method_values, '')) @@ plainto_tsquery('english', sqlc.arg(search_query))
ORDER BY
  CASE WHEN sqlc.arg(sort_field) = 'name' AND sqlc.arg(sort_order) = 'asc' THEN c.full_name END ASC,
  CASE WHEN sqlc.arg(sort_field) = 'name' AND sqlc.arg(sort_order) = 'desc' THEN c.full_name END DESC,
  CASE WHEN sqlc.arg(sort_field) = 'location' AND sqlc.arg(sort_order) = 'asc' THEN COALESCE(c.location, '') END ASC,
  CASE WHEN sqlc.arg(sort_field) = 'location' AND sqlc.arg(sort_order) = 'desc' THEN COALESCE(c.location, '') END DESC,
  CASE WHEN sqlc.arg(sort_field) = 'birthday' AND sqlc.arg(sort_order) = 'asc' THEN c.birthday END ASC NULLS LAST,
  CASE WHEN sqlc.arg(sort_field) = 'birthday' AND sqlc.arg(sort_order) = 'desc' THEN c.birthday END DESC NULLS FIRST,
  CASE WHEN sqlc.arg(sort_field) = 'last_contacted' AND sqlc.arg(sort_order) = 'asc' THEN c.last_contacted END ASC NULLS LAST,
  CASE WHEN sqlc.arg(sort_field) = 'last_contacted' AND sqlc.arg(sort_order) = 'desc' THEN c.last_contacted END DESC NULLS LAST,
  CASE WHEN sqlc.arg(sort_field) = 'contact_by' AND sqlc.arg(sort_order) = 'asc' THEN c.contact_by END ASC NULLS LAST,
  CASE WHEN sqlc.arg(sort_field) = 'contact_by' AND sqlc.arg(sort_order) = 'desc' THEN c.contact_by END DESC NULLS LAST,
  -- Cadence sort by frequency: weekly=1 (most frequent) to annual=6 (least frequent), null=7
  CASE WHEN sqlc.arg(sort_field) = 'cadence' AND sqlc.arg(sort_order) = 'desc' THEN
    CASE c.cadence WHEN 'weekly' THEN 1 WHEN 'biweekly' THEN 2 WHEN 'monthly' THEN 3 WHEN 'quarterly' THEN 4 WHEN 'biannual' THEN 5 WHEN 'annual' THEN 6 ELSE 7 END
  END ASC,
  CASE WHEN sqlc.arg(sort_field) = 'cadence' AND sqlc.arg(sort_order) = 'asc' THEN
    CASE c.cadence WHEN 'weekly' THEN 1 WHEN 'biweekly' THEN 2 WHEN 'monthly' THEN 3 WHEN 'quarterly' THEN 4 WHEN 'biannual' THEN 5 WHEN 'annual' THEN 6 ELSE 7 END
  END DESC,
  -- Secondary sort by name for cadence sorting
  CASE WHEN sqlc.arg(sort_field) = 'cadence' THEN c.full_name END ASC
LIMIT sqlc.arg(page_limit) OFFSET sqlc.arg(page_offset);

-- name: CreateContact :one
INSERT INTO contact (
  full_name, location, birthday, how_met, cadence, last_contacted, profile_photo, created_at, contact_by
) VALUES (
  $1, $2, $3, $4, $5, $6, $7, $8, $9
) RETURNING *;

-- name: UpdateContact :one
UPDATE contact SET
  full_name = $2,
  location = $3,
  birthday = $4,
  how_met = $5,
  cadence = $6,
  profile_photo = $7,
  contact_by = $8,
  updated_at = NOW()
WHERE id = $1 AND deleted_at IS NULL
RETURNING *;

-- name: UpdateContactLastContacted :exec
UPDATE contact SET
  last_contacted = $2,
  contact_by = $3,
  updated_at = NOW()
WHERE id = $1 AND deleted_at IS NULL;

-- name: UpdateContactLastContactedIfLater :exec
-- Updates last_contacted and contact_by only if the new date is later.
-- contact_by is recalculated from the new last_contacted date using the contact's existing cadence.
-- Cadence day mappings: weekly=7, biweekly=14, monthly=30, quarterly=90, biannual=180, annual=365
UPDATE contact SET
  last_contacted = GREATEST(COALESCE(last_contacted, '1970-01-01'::timestamptz), $2),
  contact_by = CASE
    WHEN $2 > COALESCE(last_contacted, '1970-01-01'::timestamptz) AND cadence IS NOT NULL AND cadence != '' THEN
      ($2::date + CASE cadence
        WHEN 'weekly' THEN 7
        WHEN 'biweekly' THEN 14
        WHEN 'monthly' THEN 30
        WHEN 'quarterly' THEN 90
        WHEN 'biannual' THEN 180
        WHEN 'annual' THEN 365
        ELSE 0
      END)
    WHEN $2 > COALESCE(last_contacted, '1970-01-01'::timestamptz) THEN NULL
    ELSE contact_by
  END,
  updated_at = NOW()
WHERE id = $1 AND deleted_at IS NULL;

-- name: SoftDeleteContact :exec
UPDATE contact SET
  deleted_at = NOW(),
  updated_at = NOW()
WHERE id = $1 AND deleted_at IS NULL;

-- name: HardDeleteContact :exec
DELETE FROM contact WHERE id = $1;

-- name: CountContacts :one
SELECT COUNT(*) FROM contact WHERE deleted_at IS NULL;

-- name: ListContactIDs :many
-- Lightweight query returning only IDs for navigation
SELECT id FROM contact
WHERE deleted_at IS NULL;

-- name: ListContactIDsSorted :many
-- Lightweight query returning only IDs with sorting for navigation
SELECT id FROM contact
WHERE deleted_at IS NULL
ORDER BY
  CASE WHEN sqlc.arg(sort_field) = 'name' AND sqlc.arg(sort_order) = 'asc' THEN full_name END ASC,
  CASE WHEN sqlc.arg(sort_field) = 'name' AND sqlc.arg(sort_order) = 'desc' THEN full_name END DESC,
  CASE WHEN sqlc.arg(sort_field) = 'location' AND sqlc.arg(sort_order) = 'asc' THEN COALESCE(location, '') END ASC,
  CASE WHEN sqlc.arg(sort_field) = 'location' AND sqlc.arg(sort_order) = 'desc' THEN COALESCE(location, '') END DESC,
  CASE WHEN sqlc.arg(sort_field) = 'birthday' AND sqlc.arg(sort_order) = 'asc' THEN birthday END ASC NULLS LAST,
  CASE WHEN sqlc.arg(sort_field) = 'birthday' AND sqlc.arg(sort_order) = 'desc' THEN birthday END DESC NULLS FIRST,
  CASE WHEN sqlc.arg(sort_field) = 'last_contacted' AND sqlc.arg(sort_order) = 'asc' THEN last_contacted END ASC NULLS LAST,
  CASE WHEN sqlc.arg(sort_field) = 'last_contacted' AND sqlc.arg(sort_order) = 'desc' THEN last_contacted END DESC NULLS LAST,
  CASE WHEN sqlc.arg(sort_field) = 'contact_by' AND sqlc.arg(sort_order) = 'asc' THEN contact_by END ASC NULLS LAST,
  CASE WHEN sqlc.arg(sort_field) = 'contact_by' AND sqlc.arg(sort_order) = 'desc' THEN contact_by END DESC NULLS LAST,
  -- Cadence sort by frequency: weekly=1 (most frequent) to annual=6 (least frequent), null=7
  CASE WHEN sqlc.arg(sort_field) = 'cadence' AND sqlc.arg(sort_order) = 'desc' THEN
    CASE cadence WHEN 'weekly' THEN 1 WHEN 'biweekly' THEN 2 WHEN 'monthly' THEN 3 WHEN 'quarterly' THEN 4 WHEN 'biannual' THEN 5 WHEN 'annual' THEN 6 ELSE 7 END
  END ASC,
  CASE WHEN sqlc.arg(sort_field) = 'cadence' AND sqlc.arg(sort_order) = 'asc' THEN
    CASE cadence WHEN 'weekly' THEN 1 WHEN 'biweekly' THEN 2 WHEN 'monthly' THEN 3 WHEN 'quarterly' THEN 4 WHEN 'biannual' THEN 5 WHEN 'annual' THEN 6 ELSE 7 END
  END DESC,
  -- Secondary sort by name for cadence sorting
  CASE WHEN sqlc.arg(sort_field) = 'cadence' THEN full_name END ASC;

-- name: SearchContactIDs :many
-- Lightweight query returning only IDs with search for navigation
SELECT c.id FROM contact c
LEFT JOIN (
  SELECT contact_id, string_agg(value, ' ') AS method_values
  FROM contact_method
  GROUP BY contact_id
) cm ON cm.contact_id = c.id
WHERE c.deleted_at IS NULL
  AND to_tsvector('english', c.full_name || ' ' || COALESCE(cm.method_values, '')) @@ plainto_tsquery('english', $1)
ORDER BY ts_rank(
  to_tsvector('english', c.full_name || ' ' || COALESCE(cm.method_values, '')),
  plainto_tsquery('english', $1)
) DESC;

-- name: SearchContactIDsSorted :many
-- Lightweight query returning only IDs with search and sorting for navigation
SELECT c.id FROM contact c
LEFT JOIN (
  SELECT contact_id, string_agg(value, ' ') AS method_values
  FROM contact_method
  GROUP BY contact_id
) cm ON cm.contact_id = c.id
WHERE c.deleted_at IS NULL
  AND to_tsvector('english', c.full_name || ' ' || COALESCE(cm.method_values, '')) @@ plainto_tsquery('english', sqlc.arg(search_query))
ORDER BY
  CASE WHEN sqlc.arg(sort_field) = 'name' AND sqlc.arg(sort_order) = 'asc' THEN c.full_name END ASC,
  CASE WHEN sqlc.arg(sort_field) = 'name' AND sqlc.arg(sort_order) = 'desc' THEN c.full_name END DESC,
  CASE WHEN sqlc.arg(sort_field) = 'location' AND sqlc.arg(sort_order) = 'asc' THEN COALESCE(c.location, '') END ASC,
  CASE WHEN sqlc.arg(sort_field) = 'location' AND sqlc.arg(sort_order) = 'desc' THEN COALESCE(c.location, '') END DESC,
  CASE WHEN sqlc.arg(sort_field) = 'birthday' AND sqlc.arg(sort_order) = 'asc' THEN c.birthday END ASC NULLS LAST,
  CASE WHEN sqlc.arg(sort_field) = 'birthday' AND sqlc.arg(sort_order) = 'desc' THEN c.birthday END DESC NULLS FIRST,
  CASE WHEN sqlc.arg(sort_field) = 'last_contacted' AND sqlc.arg(sort_order) = 'asc' THEN c.last_contacted END ASC NULLS LAST,
  CASE WHEN sqlc.arg(sort_field) = 'last_contacted' AND sqlc.arg(sort_order) = 'desc' THEN c.last_contacted END DESC NULLS LAST,
  CASE WHEN sqlc.arg(sort_field) = 'contact_by' AND sqlc.arg(sort_order) = 'asc' THEN c.contact_by END ASC NULLS LAST,
  CASE WHEN sqlc.arg(sort_field) = 'contact_by' AND sqlc.arg(sort_order) = 'desc' THEN c.contact_by END DESC NULLS LAST,
  -- Cadence sort by frequency: weekly=1 (most frequent) to annual=6 (least frequent), null=7
  CASE WHEN sqlc.arg(sort_field) = 'cadence' AND sqlc.arg(sort_order) = 'desc' THEN
    CASE c.cadence WHEN 'weekly' THEN 1 WHEN 'biweekly' THEN 2 WHEN 'monthly' THEN 3 WHEN 'quarterly' THEN 4 WHEN 'biannual' THEN 5 WHEN 'annual' THEN 6 ELSE 7 END
  END ASC,
  CASE WHEN sqlc.arg(sort_field) = 'cadence' AND sqlc.arg(sort_order) = 'asc' THEN
    CASE c.cadence WHEN 'weekly' THEN 1 WHEN 'biweekly' THEN 2 WHEN 'monthly' THEN 3 WHEN 'quarterly' THEN 4 WHEN 'biannual' THEN 5 WHEN 'annual' THEN 6 ELSE 7 END
  END DESC,
  -- Secondary sort by name for cadence sorting
  CASE WHEN sqlc.arg(sort_field) = 'cadence' THEN c.full_name END ASC;

-- name: CountSearchContacts :one
SELECT COUNT(*) FROM contact c
LEFT JOIN (
  SELECT contact_id, string_agg(value, ' ') AS method_values
  FROM contact_method
  GROUP BY contact_id
) cm ON cm.contact_id = c.id
WHERE c.deleted_at IS NULL
  AND to_tsvector('english', c.full_name || ' ' || COALESCE(cm.method_values, '')) @@ plainto_tsquery('english', $1);

-- name: FindSimilarContacts :many
SELECT
  c.id,
  c.full_name,
  similarity(c.full_name, sqlc.arg(search_name)::text) as name_similarity,
  COALESCE(
    json_agg(
      json_build_object(
        'type', cm.type,
        'value', cm.value
      )
    ) FILTER (WHERE cm.id IS NOT NULL),
    '[]'
  )::jsonb as methods_json
FROM contact c
LEFT JOIN contact_method cm ON c.id = cm.contact_id
WHERE c.deleted_at IS NULL
  AND similarity(c.full_name, sqlc.arg(search_name)::text) > sqlc.arg(threshold)::real
GROUP BY c.id, c.full_name
ORDER BY similarity(c.full_name, sqlc.arg(search_name)::text) DESC
LIMIT sqlc.arg(result_limit);

-- name: FindSimilarContactsBatch :many
-- Finds similar contacts for multiple candidate names in a single batch query.
-- Uses UNNEST to expand input arrays and LATERAL join to find matches per candidate.
-- Returns results grouped by candidate_id with matches ordered by similarity.
WITH candidate_names AS (
  SELECT
    unnest(sqlc.arg(candidate_names)::text[])::text as candidate_name,
    unnest(sqlc.arg(candidate_ids)::text[])::text as candidate_id
)
SELECT
  cn.candidate_id::text as candidate_id,
  cn.candidate_name::text as candidate_name,
  c.id as contact_id,
  c.full_name as contact_name,
  similarity(c.full_name, cn.candidate_name) as name_similarity,
  COALESCE(
    json_agg(
      json_build_object(
        'type', cm.type,
        'value', cm.value
      )
    ) FILTER (WHERE cm.id IS NOT NULL),
    '[]'
  )::jsonb as methods_json
FROM candidate_names cn
CROSS JOIN LATERAL (
  SELECT c.id, c.full_name
  FROM contact c
  WHERE c.deleted_at IS NULL
    AND similarity(c.full_name, cn.candidate_name) > sqlc.arg(threshold)::real
  ORDER BY similarity(c.full_name, cn.candidate_name) DESC
  LIMIT sqlc.arg(limit_per_candidate)
) c
LEFT JOIN contact_method cm ON c.id = cm.contact_id
GROUP BY cn.candidate_id, cn.candidate_name, c.id, c.full_name
ORDER BY cn.candidate_id, similarity(c.full_name, cn.candidate_name) DESC;

-- name: ListOverdueContacts :many
-- Lists contacts whose contact_by date is before today (overdue).
-- Returns contacts ordered by how overdue they are (most overdue first).
SELECT * FROM contact
WHERE deleted_at IS NULL
  AND contact_by IS NOT NULL
  AND contact_by < sqlc.arg(today)::date
ORDER BY contact_by ASC
LIMIT sqlc.arg(limit_count);

-- name: ListContactsWithContactBy :many
-- Lists contacts that have a contact_by date set (used for testing mode filtering).
-- Returns contacts ordered by contact_by (soonest first).
SELECT * FROM contact
WHERE deleted_at IS NULL
  AND contact_by IS NOT NULL
ORDER BY contact_by ASC
LIMIT $1;

-- name: UpdateContactBy :exec
-- Updates just the contact_by field (for Todoist deadline sync).
UPDATE contact SET
  contact_by = $2,
  updated_at = NOW()
WHERE id = $1 AND deleted_at IS NULL;

-- name: ListContactsWithCadence :many
-- Lists contacts that have a cadence set (used for Todoist sync reconciliation).
SELECT * FROM contact
WHERE deleted_at IS NULL
  AND cadence IS NOT NULL
  AND cadence != ''
ORDER BY full_name ASC
LIMIT $1;
