-- External Contact queries

-- name: GetExternalContact :one
-- Tombstoned rows are not retrievable by ID through normal flows.
SELECT * FROM external_contact
WHERE id = $1
  AND deleted_at IS NULL;

-- name: FindExternalContactsBySourceAndSourceID :many
-- Finds all unmatched external_contact rows for a (source, source_id) pair
-- regardless of account_id. Used by the calendar rematch handler to mark
-- gcal_attendee import candidates as matched after a CRM contact links them.
-- Tombstoned candidates must not be rematched.
SELECT * FROM external_contact
WHERE source = sqlc.arg('source')
  AND source_id = sqlc.arg('source_id')
  AND match_status = 'unmatched'
  AND deleted_at IS NULL;

-- name: GetExternalContactBySource :one
-- Tombstone-aware: returns tombstoned rows too. The mac-daemon ingest path
-- needs visibility into tombstoned rows so it can revive them on a fresh
-- external_contact.upserted event. Existing telegram / gcontacts callers
-- never tombstone, so the broader read does not affect them. Callers that
-- want live-only rows must check `DeletedAt == nil` after the call.
SELECT * FROM external_contact
WHERE source = $1 AND source_id = $2 AND COALESCE(account_id, '') = COALESCE($3, '');

-- name: UpsertExternalContact :one
-- Named-param variant. host_id follows claim-on-first-non-NULL-emit:
-- legacy rows whose host_id IS NULL (pre-migration data) get claimed
-- by the first host that emits an upsert for them, but non-NULL
-- ownership is preserved thereafter. This keeps existing
-- icloud_contacts rows reachable from /known-ids on upgraded systems
-- without requiring destructive operator cleanup. Re-pair onto a
-- different Mac for already-owned rows is a documented limitation;
-- scripts/admin/reset_icloud_contacts.sh handles that path.
--
-- last_content_hash is written on both INSERT and UPDATE so the
-- /known-ids endpoint always returns the most recent payload's hash.
-- Application-layer regex enforces lowercase 64-char hex on write;
-- this query stores the value verbatim.
INSERT INTO external_contact (
    source, source_id, account_id, host_id, display_name, first_name, last_name,
    emails, phones, addresses, organization, job_title, birthday, photo_url,
    crm_contact_id, match_status, duplicate_of_id, etag, metadata, synced_at,
    last_content_hash
) VALUES (
    sqlc.arg('source'),
    sqlc.arg('source_id'),
    sqlc.arg('account_id'),
    sqlc.arg('host_id'),
    sqlc.arg('display_name'),
    sqlc.arg('first_name'),
    sqlc.arg('last_name'),
    sqlc.arg('emails'),
    sqlc.arg('phones'),
    sqlc.arg('addresses'),
    sqlc.arg('organization'),
    sqlc.arg('job_title'),
    sqlc.arg('birthday'),
    sqlc.arg('photo_url'),
    sqlc.arg('crm_contact_id'),
    sqlc.arg('match_status'),
    sqlc.arg('duplicate_of_id'),
    sqlc.arg('etag'),
    sqlc.arg('metadata'),
    sqlc.arg('synced_at'),
    sqlc.arg('last_content_hash')
)
ON CONFLICT (source, source_id, COALESCE(account_id, '')) DO UPDATE SET
    -- Claim-on-first-non-NULL: legacy rows with NULL host_id get
    -- claimed by the upserting host; non-NULL ownership is preserved.
    host_id           = COALESCE(external_contact.host_id, EXCLUDED.host_id),
    display_name      = EXCLUDED.display_name,
    first_name        = EXCLUDED.first_name,
    last_name         = EXCLUDED.last_name,
    emails            = EXCLUDED.emails,
    phones            = EXCLUDED.phones,
    addresses         = EXCLUDED.addresses,
    organization      = EXCLUDED.organization,
    job_title         = EXCLUDED.job_title,
    birthday          = EXCLUDED.birthday,
    photo_url         = EXCLUDED.photo_url,
    etag              = EXCLUDED.etag,
    metadata          = EXCLUDED.metadata,
    synced_at         = EXCLUDED.synced_at,
    last_content_hash = EXCLUDED.last_content_hash,
    updated_at        = NOW()
RETURNING *;

-- name: ListKnownExternalContactIDsByHostAndSource :many
-- Returns (source_id, last_content_hash) for every live
-- external_contact row owned by the given (host_id, source). Powers
-- GET /api/v1/host/:id/sync/:source/known-ids. Tombstoned rows are
-- excluded — the daemon's set-diff reconciliation requires that
-- rows the Pi has soft-deleted are NOT reported as known (else the
-- daemon never re-tombstones them).
--
-- Lowercase-hex of last_content_hash is enforced upstream by the
-- ingest layer's verifyExternalContactInvariants regex
-- (^[a-f0-9]{64}$) plus the JCS hash-verification check. This query
-- stores and returns the value verbatim. Legacy rows whose
-- last_content_hash is NULL cause the daemon to fall back to the
-- @deleted@unknown sentinel per the spec.
SELECT source_id, last_content_hash
FROM external_contact
WHERE host_id = sqlc.arg('host_id')
  AND source = sqlc.arg('source')
  AND deleted_at IS NULL
ORDER BY source_id;

-- name: ReviveExternalContact :one
-- Clears deleted_at on a tombstoned row. Defensive WHERE deleted_at IS NOT
-- NULL keeps the statement idempotent across concurrent revive races.
-- Preserves crm_contact_id, match_status, and all content columns.
UPDATE external_contact SET
    deleted_at = NULL,
    updated_at = NOW()
WHERE id = $1
  AND deleted_at IS NOT NULL
RETURNING *;

-- name: SoftDeleteExternalContact :exec
-- Tombstones a live row. Defensive WHERE deleted_at IS NULL keeps the
-- statement idempotent against a concurrent delete. crm_contact_id,
-- match_status, and duplicate_of_id are preserved per the external_contact
-- soft-delete contract.
UPDATE external_contact SET
    deleted_at = NOW(),
    updated_at = NOW()
WHERE id = $1
  AND deleted_at IS NULL;

-- name: ListExternalContactsBySource :many
SELECT * FROM external_contact
WHERE source = $1
  AND ($2::text IS NULL OR account_id = $2)
  AND deleted_at IS NULL
ORDER BY display_name
LIMIT $3 OFFSET $4;

-- name: ListUnmatchedExternalContacts :many
-- Per-source query; anarlog_title rows are intentionally NOT exposed
-- here — they live behind a dedicated grouped-by-token UI surface.
-- The `source != 'anarlog_title'` clause is defense-in-depth: if a
-- caller passes source='anarlog_title' (intentionally or via param
-- injection), this query returns empty rather than leaking weak
-- discovery rows into the per-source UI.
SELECT * FROM external_contact
WHERE source = sqlc.arg('source')
  AND source != 'anarlog_title'
  AND match_status = 'unmatched'
  AND duplicate_of_id IS NULL
  AND deleted_at IS NULL
  AND (
    sqlc.arg('include_unresolved_telegram')::bool
    OR NOT (
      source = 'telegram'
      AND NULLIF(BTRIM(COALESCE(display_name, '')), '') IS NULL
      AND NULLIF(BTRIM(COALESCE(first_name, '')), '') IS NULL
      AND NULLIF(BTRIM(COALESCE(last_name, '')), '') IS NULL
      AND NULLIF(BTRIM(COALESCE(metadata->>'username', '')), '') IS NULL
      AND COALESCE(jsonb_array_length(emails), 0) = 0
      AND COALESCE(jsonb_array_length(phones), 0) = 0
    )
  )
ORDER BY display_name
LIMIT sqlc.arg('page_limit') OFFSET sqlc.arg('page_offset');

-- name: ListAllUnmatchedExternalContacts :many
SELECT * FROM external_contact
WHERE match_status = 'unmatched'
  AND source != 'anarlog_title'
  AND duplicate_of_id IS NULL
  AND deleted_at IS NULL
  AND (
    sqlc.arg('include_unresolved_telegram')::bool
    OR NOT (
      source = 'telegram'
      AND NULLIF(BTRIM(COALESCE(display_name, '')), '') IS NULL
      AND NULLIF(BTRIM(COALESCE(first_name, '')), '') IS NULL
      AND NULLIF(BTRIM(COALESCE(last_name, '')), '') IS NULL
      AND NULLIF(BTRIM(COALESCE(metadata->>'username', '')), '') IS NULL
      AND COALESCE(jsonb_array_length(emails), 0) = 0
      AND COALESCE(jsonb_array_length(phones), 0) = 0
    )
  )
ORDER BY source, display_name
LIMIT sqlc.arg('page_limit') OFFSET sqlc.arg('page_offset');

-- name: CountUnmatchedExternalContacts :one
-- Per-source count; mirrors ListUnmatched's anarlog_title exclusion
-- so list+count cardinality stays consistent regardless of caller-
-- supplied source.
SELECT COUNT(*) FROM external_contact
WHERE source = sqlc.arg('source')
  AND source != 'anarlog_title'
  AND match_status = 'unmatched'
  AND duplicate_of_id IS NULL
  AND deleted_at IS NULL
  AND (
    sqlc.arg('include_unresolved_telegram')::bool
    OR NOT (
      source = 'telegram'
      AND NULLIF(BTRIM(COALESCE(display_name, '')), '') IS NULL
      AND NULLIF(BTRIM(COALESCE(first_name, '')), '') IS NULL
      AND NULLIF(BTRIM(COALESCE(last_name, '')), '') IS NULL
      AND NULLIF(BTRIM(COALESCE(metadata->>'username', '')), '') IS NULL
      AND COALESCE(jsonb_array_length(emails), 0) = 0
      AND COALESCE(jsonb_array_length(phones), 0) = 0
    )
  );

-- name: CountAllUnmatchedExternalContacts :one
SELECT COUNT(*) FROM external_contact
WHERE match_status = 'unmatched'
  AND source != 'anarlog_title'
  AND duplicate_of_id IS NULL
  AND deleted_at IS NULL
  AND (
    sqlc.arg('include_unresolved_telegram')::bool
    OR NOT (
      source = 'telegram'
      AND NULLIF(BTRIM(COALESCE(display_name, '')), '') IS NULL
      AND NULLIF(BTRIM(COALESCE(first_name, '')), '') IS NULL
      AND NULLIF(BTRIM(COALESCE(last_name, '')), '') IS NULL
      AND NULLIF(BTRIM(COALESCE(metadata->>'username', '')), '') IS NULL
      AND COALESCE(jsonb_array_length(emails), 0) = 0
      AND COALESCE(jsonb_array_length(phones), 0) = 0
    )
  );

-- name: CountHiddenUnresolvedTelegramContacts :one
SELECT COUNT(*) FROM external_contact
WHERE source = 'telegram'
  AND (sqlc.arg('source_filter')::text = '' OR source = sqlc.arg('source_filter')::text)
  AND match_status = 'unmatched'
  AND duplicate_of_id IS NULL
  AND deleted_at IS NULL
  AND NULLIF(BTRIM(COALESCE(display_name, '')), '') IS NULL
  AND NULLIF(BTRIM(COALESCE(first_name, '')), '') IS NULL
  AND NULLIF(BTRIM(COALESCE(last_name, '')), '') IS NULL
  AND NULLIF(BTRIM(COALESCE(metadata->>'username', '')), '') IS NULL
  AND COALESCE(jsonb_array_length(emails), 0) = 0
  AND COALESCE(jsonb_array_length(phones), 0) = 0;

-- name: UpdateExternalContactMatch :one
-- Filter `deleted_at IS NULL` so a tombstoned row cannot have its
-- match state mutated. A tombstoned row is invisible to every read
-- path; writes should be invisible too.
UPDATE external_contact SET
    crm_contact_id = $2,
    match_status = $3,
    updated_at = NOW()
WHERE id = $1
  AND deleted_at IS NULL
RETURNING *;

-- name: UpdateExternalContactDuplicate :exec
UPDATE external_contact SET
    duplicate_of_id = $2,
    updated_at = NOW()
WHERE id = $1
  AND deleted_at IS NULL;

-- name: IgnoreExternalContact :exec
UPDATE external_contact SET
    match_status = 'ignored',
    updated_at = NOW()
WHERE id = $1
  AND deleted_at IS NULL;

-- name: ListAnarlogTitleGroups :many
-- Groups unmatched anarlog_title weak candidates by normalized token,
-- one group per token, for the People-tab discovery surface. The
-- ranking signal is evidence_count = number of member external_contact
-- rows for the token (one row per (token, session) pair via the
-- deterministic source_id). The LEFT JOIN to meeting_note pulls the
-- human-readable session titles for display only; a tombstoned session
-- still counts via its member row even when its title is NULL. Both
-- array_agg calls carry an explicit ORDER BY so element order is
-- deterministic across runs (flake-free tests + stable UI). Casts to
-- text[]/uuid[] keep the generated Go types concrete.
SELECT
    (ec.metadata->>'token_normalized')::text                 AS normalized_token,
    MAX(ec.metadata->>'token_display')::text                 AS token_display,
    COUNT(DISTINCT ec.id)                                    AS evidence_count,
    array_agg(DISTINCT ec.id ORDER BY ec.id)::uuid[]         AS member_ids,
    array_remove(
        array_agg(DISTINCT mn.title ORDER BY mn.title), NULL
    )::text[]                                                AS session_titles
FROM external_contact ec
LEFT JOIN meeting_note mn
    ON mn.anarlog_session_id = (ec.metadata->>'session_uuid')::uuid
    AND mn.deleted_at IS NULL
WHERE ec.source = 'anarlog_title'
    AND ec.match_status = 'unmatched'
    AND ec.duplicate_of_id IS NULL
    AND ec.deleted_at IS NULL
GROUP BY ec.metadata->>'token_normalized'
ORDER BY evidence_count DESC, normalized_token ASC;

-- name: FindAnarlogTitleSiblingsByToken :many
-- Returns every live unmatched anarlog_title sibling row for a normalized
-- token. ORDER BY id ASC so the lowest-id row is a stable representative
-- for the reuse-existing-import-service resolve path. The predicate
-- mirrors ListAnarlogTitleGroups (and the batch-mark queries) EXACTLY so
-- the resolve service inspects the same row set it later marks.
SELECT * FROM external_contact
WHERE source = 'anarlog_title'
    AND metadata->>'token_normalized' = sqlc.arg('normalized_token')::text
    AND match_status = 'unmatched'
    AND duplicate_of_id IS NULL
    AND deleted_at IS NULL
ORDER BY id ASC;

-- name: MarkAnarlogTitleSiblingsImportedByToken :execrows
-- Single-statement batch mark for the action=import resolve path: every
-- live unmatched sibling for the token flips to 'imported' and points at
-- the newly created CRM contact atomically. The WHERE predicate mirrors
-- FindAnarlogTitleSiblingsByToken EXACTLY (incl. duplicate_of_id IS NULL)
-- so the mark touches precisely the row set the service inspected. Returns
-- the affected-row count so the service can detect a concurrent resolve
-- (zero rows) and roll back the contact it just created.
UPDATE external_contact SET
    crm_contact_id = sqlc.arg('crm_contact_id'),
    match_status = 'imported',
    updated_at = NOW()
WHERE source = 'anarlog_title'
    AND metadata->>'token_normalized' = sqlc.arg('normalized_token')::text
    AND match_status = 'unmatched'
    AND duplicate_of_id IS NULL
    AND deleted_at IS NULL;

-- name: MarkAnarlogTitleSiblingsMatchedByToken :execrows
-- Single-statement batch mark for the action=link resolve path: every
-- live unmatched sibling for the token flips to 'matched' and points at
-- the linked CRM contact atomically. Same predicate as the imported and
-- ignored variants. Returns the affected-row count so the service can
-- surface a clean not-found when a concurrent resolve already claimed the
-- group (zero rows).
UPDATE external_contact SET
    crm_contact_id = sqlc.arg('crm_contact_id'),
    match_status = 'matched',
    updated_at = NOW()
WHERE source = 'anarlog_title'
    AND metadata->>'token_normalized' = sqlc.arg('normalized_token')::text
    AND match_status = 'unmatched'
    AND duplicate_of_id IS NULL
    AND deleted_at IS NULL;

-- name: MarkAnarlogTitleSiblingsIgnoredByToken :exec
-- Single-statement batch mark for the action=ignore ("Not a person")
-- resolve path: every live unmatched sibling for the token flips to
-- 'ignored'. No crm_contact_id is set. Same predicate as the other two
-- batch marks.
UPDATE external_contact SET
    match_status = 'ignored',
    updated_at = NOW()
WHERE source = 'anarlog_title'
    AND metadata->>'token_normalized' = sqlc.arg('normalized_token')::text
    AND match_status = 'unmatched'
    AND duplicate_of_id IS NULL
    AND deleted_at IS NULL;

-- name: FindExternalContactsByEmail :many
SELECT * FROM external_contact
WHERE emails @> $1::jsonb
  AND duplicate_of_id IS NULL
  AND deleted_at IS NULL
ORDER BY created_at;

-- name: FindExternalContactsByNormalizedEmail :many
-- Finds external contacts whose JSONB emails contain the given normalized
-- email value. Backed by idx_external_contact_emails_value_lower_gin via
-- the jsonb_array_lower_values helper. Tombstoned rows are excluded so
-- duplicate detection ignores soft-deleted candidates.
SELECT * FROM external_contact
WHERE jsonb_array_lower_values(emails, 'value') && ARRAY[LOWER($1)]
  AND duplicate_of_id IS NULL
  AND deleted_at IS NULL
ORDER BY created_at;

-- name: ListExternalContactsForCRMContact :many
SELECT * FROM external_contact
WHERE crm_contact_id = $1
  AND deleted_at IS NULL
ORDER BY source, account_id;

-- name: ListLinkedAddressBookExternalContactsForReconcile :many
-- Driver query for the address-book method reconcile (forward hooks +
-- one-time catchup). Returns every live address-book row that is itself
-- linked OR is a duplicate of a (live) canonical row, joining to the
-- canonical so the Go reconcile can apply the effective-status
-- precedence (`ignored > imported > matched`) WITHOUT a self-first
-- COALESCE (which would let a dup's stale `matched` win over a canonical
-- `imported`). The canonical columns are explicitly aliased so the
-- generated struct does not collide with the row's own columns.
--
-- The reconcile is restricted to address-book sources via the
-- caller-supplied `sources` array (`{'gcontacts','icloud_contacts'}`);
-- telegram / gcal_attendee / anarlog_* are out of scope (their own
-- match/enrich flows). ORDER BY ec.id keeps the catchup deterministic.
SELECT
    ec.*,
    canon.crm_contact_id AS canon_crm_contact_id,
    canon.match_status   AS canon_match_status
FROM external_contact ec
LEFT JOIN external_contact canon
    ON ec.duplicate_of_id = canon.id
   AND canon.deleted_at IS NULL
WHERE ec.source = ANY(sqlc.arg('sources')::text[])
  AND ec.deleted_at IS NULL
  AND (ec.crm_contact_id IS NOT NULL OR canon.crm_contact_id IS NOT NULL)
ORDER BY ec.id;

-- name: SetExternalContactMethodSuggestions :one
-- Overwrite-not-append write of the pending suggestion set for a linked
-- row. The Go wrapper passes SQL NULL when the recomputed set is empty,
-- so a method later added by another path clears the stale suggestion on
-- the next reconcile. Writes the DEDICATED column, never `metadata` —
-- so it survives the wholesale-metadata-replace producer upserts.
UPDATE external_contact SET
    pending_method_suggestions = sqlc.arg('pending'),
    updated_at = NOW()
WHERE id = $1
  AND deleted_at IS NULL
RETURNING *;

-- name: GetExternalContactForUpdate :one
-- Row-locked read of a live row for the resolve/dismiss read-modify-write.
-- The caller runs this + SetExternalContactPendingAndDismissed in ONE
-- pgx.Tx so each action's own read-modify-write is atomic (a single
-- action cannot half-clobber its own suggestion columns).
SELECT * FROM external_contact
WHERE id = sqlc.arg('id')
  AND deleted_at IS NULL
FOR UPDATE;

-- name: SetExternalContactPendingAndDismissed :one
-- Atomically rewrite BOTH suggestion columns. Resolve passes the
-- unchanged dismissed set through and clears confirmed entries from
-- pending; dismiss appends to dismissed and drops the same entries from
-- pending. The Go layer computes both final sets from the FOR UPDATE
-- re-read (never trusting a stale client row); an empty slice marshals to
-- nil bytes → SQL NULL.
UPDATE external_contact SET
    pending_method_suggestions   = sqlc.arg('pending'),
    dismissed_method_suggestions = sqlc.arg('dismissed'),
    updated_at = NOW()
WHERE id = sqlc.arg('id')
  AND deleted_at IS NULL
RETURNING *;

-- name: ListExternalContactsWithPendingMethodSuggestions :many
-- Address-book rows carrying non-empty pending_method_suggestions, joined
-- to the canonical so the repo can apply the SAME effective-status
-- precedence (ignored > imported > matched) as the reconcile driver via
-- resolveEffectiveReconcileState — NOT a self-first COALESCE. Scoped to
-- the caller-supplied address-book sources, with an optional People-tab
-- source filter (empty = no chip).
SELECT
    ec.*,
    canon.crm_contact_id AS canon_crm_contact_id,
    canon.match_status   AS canon_match_status
FROM external_contact ec
LEFT JOIN external_contact canon
    ON ec.duplicate_of_id = canon.id
   AND canon.deleted_at IS NULL
WHERE ec.source = ANY(sqlc.arg('sources')::text[])
  AND ec.deleted_at IS NULL
  AND ec.pending_method_suggestions IS NOT NULL
  AND jsonb_array_length(ec.pending_method_suggestions) > 0
  AND (sqlc.arg('source_filter')::text = '' OR ec.source = sqlc.arg('source_filter')::text)
ORDER BY ec.id;

-- name: SetDismissedMethodSuggestionsForTest :one
-- TEST ONLY: pre-seeds the dismissed_method_suggestions column so the
-- dismissed-skip reconcile test can verify a dismissed (type,value) is
-- not re-suggested. The production dismissal path appends via a read-
-- modify-write; this direct setter exists only so integration tests can
-- establish the pre-state without raw SQL in Go.
UPDATE external_contact SET
    dismissed_method_suggestions = sqlc.arg('dismissed'),
    updated_at = NOW()
WHERE id = $1
  AND deleted_at IS NULL
RETURNING *;

-- name: DeleteExternalContactsBySourceAccount :exec
DELETE FROM external_contact
WHERE source = $1 AND COALESCE(account_id, '') = COALESCE($2, '');

-- name: DeleteExternalContact :exec
DELETE FROM external_contact WHERE id = $1;

-- name: UpsertDiscoveryCandidate :one
-- Source-parameterized discovery upsert that preserves populated peer fields
-- when a later message arrives with null entity data. Never clears a
-- name/handle that was previously captured. Metadata is merged (|| operator) so
-- keys from earlier writes (e.g. username) are retained when the new map omits
-- them.
INSERT INTO external_contact (
    source, source_id, display_name, first_name, last_name, metadata, synced_at
) VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT (source, source_id, COALESCE(account_id, '')) DO UPDATE SET
    display_name = COALESCE(EXCLUDED.display_name, external_contact.display_name),
    first_name   = COALESCE(EXCLUDED.first_name,   external_contact.first_name),
    last_name    = COALESCE(EXCLUDED.last_name,    external_contact.last_name),
    metadata     = external_contact.metadata || EXCLUDED.metadata,
    synced_at    = EXCLUDED.synced_at,
    updated_at   = NOW()
RETURNING *;

-- Contact Enrichment queries

-- name: GetEnrichmentsForContact :many
SELECT * FROM contact_enrichment
WHERE contact_id = $1
ORDER BY enriched_at DESC;

-- name: CreateEnrichment :one
INSERT INTO contact_enrichment (
    contact_id, source, account_id, field, external_contact_id, original_value
) VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (contact_id, source, field, COALESCE(account_id, '')) DO UPDATE SET
    external_contact_id = EXCLUDED.external_contact_id,
    original_value = EXCLUDED.original_value,
    enriched_at = NOW()
RETURNING *;

-- name: HasEnrichmentForField :one
SELECT EXISTS(
    SELECT 1 FROM contact_enrichment
    WHERE contact_id = $1 AND field = $2
);

-- name: GetEnrichmentByField :one
SELECT * FROM contact_enrichment
WHERE contact_id = $1 AND field = $2
LIMIT 1;

-- name: ListEnrichmentsBySource :many
SELECT * FROM contact_enrichment
WHERE source = $1
ORDER BY enriched_at DESC
LIMIT $2 OFFSET $3;

-- name: DeleteEnrichmentsForContact :exec
DELETE FROM contact_enrichment WHERE contact_id = $1;

-- name: DeleteEnrichment :exec
DELETE FROM contact_enrichment WHERE id = $1;
