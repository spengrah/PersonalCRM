-- TEST ONLY. Fixture queries used by jsonb_gin_index_test.go to construct
-- edge-case JSONB shapes (non-array, NULL, missing keys) that production
-- code paths cannot create, plus permanent regression-guard queries that
-- mirror the legacy form of the rewritten production queries. Do NOT call
-- these from production code.

-- name: TestInsertExternalContactRawEmails :one
-- TEST ONLY. Inserts an external_contact with a literal JSONB value supplied
-- by the caller, bypassing the typed-Go marshalling that UpsertExternalContact
-- enforces. sqlc.narg('emails') makes the parameter nullable so callers can
-- exercise the NULL JSONB column case.
INSERT INTO external_contact (source, source_id, display_name, emails)
VALUES (@source::text, @source_id::text, @display_name::text, sqlc.narg('emails')::jsonb)
RETURNING *;

-- name: TestInsertCalendarEventRawAttendees :one
-- TEST ONLY. See TestInsertExternalContactRawEmails. Same rationale for
-- calendar_event.attendees.
INSERT INTO calendar_event (
  gcal_event_id, gcal_calendar_id, google_account_id,
  start_time, end_time, status, attendees, matched_contact_ids
)
VALUES (
  @gcal_event_id::text, @gcal_calendar_id::text, @google_account_id::text,
  @start_time::timestamptz, @end_time::timestamptz, @status::text,
  sqlc.narg('attendees')::jsonb, @matched_contact_ids::uuid[]
)
RETURNING *;

-- name: TestDeleteExternalContactsBySourceIDPrefix :exec
-- TEST ONLY. Hard-deletes external_contact rows whose source_id starts with
-- the given prefix. Used by t.Cleanup to remove fixtures inserted by a test.
DELETE FROM external_contact WHERE source_id LIKE @prefix::text || '%';

-- name: TestDeleteCalendarEventsByGcalEventIDPrefix :exec
-- TEST ONLY. Hard-deletes calendar_event rows whose gcal_event_id starts
-- with the given prefix. Used by t.Cleanup to remove fixtures.
DELETE FROM calendar_event WHERE gcal_event_id LIKE @prefix::text || '%';

-- name: TestParityFindExternalContactsByNormalizedEmailLegacy :many
-- TEST ONLY. Mirrors the legacy EXISTS / jsonb_array_elements form of
-- FindExternalContactsByNormalizedEmail. Permanent regression guard against
-- semantic drift in the rewritten query. The CASE guard maps non-array JSONB
-- to no-match instead of letting jsonb_array_elements raise — the table is
-- shared with concurrently-running tests whose rows this query cannot
-- restrict. Do NOT call from production code.
SELECT * FROM external_contact
WHERE EXISTS (
    SELECT 1 FROM jsonb_array_elements(CASE WHEN jsonb_typeof(emails) = 'array' THEN emails ELSE '[]'::jsonb END) AS e
    WHERE LOWER(e->>'value') = LOWER($1)
)
  AND duplicate_of_id IS NULL
ORDER BY created_at;

-- name: TestParityFindEventsByAttendeeEmailUnmatchedForContactLegacy :many
-- TEST ONLY. Mirrors the legacy EXISTS / jsonb_array_elements form of
-- FindEventsByAttendeeEmailUnmatchedForContact. Permanent regression guard.
-- The CASE guard maps non-array JSONB to no-match instead of letting
-- jsonb_array_elements raise — the table is shared with concurrently-running
-- tests whose rows this query cannot restrict. Do NOT call from production
-- code.
SELECT * FROM calendar_event
WHERE EXISTS (
    SELECT 1 FROM jsonb_array_elements(CASE WHEN jsonb_typeof(attendees) = 'array' THEN attendees ELSE '[]'::jsonb END) AS a
    WHERE LOWER(a->>'email') = LOWER(@email::text)
)
  AND NOT (@contact_id::uuid = ANY(matched_contact_ids))
  AND status != 'cancelled';

-- name: TestIndexExists :one
-- TEST ONLY. Checks whether a named index exists. Used by the integration
-- test as a structural guard that migration 045's GIN indexes are actually
-- present (a behavior-only test would pass even if a future migration
-- accidentally dropped them). to_regclass returns NULL when the index
-- does not exist.
SELECT (to_regclass(@index_name::text) IS NOT NULL)::boolean AS exists;
