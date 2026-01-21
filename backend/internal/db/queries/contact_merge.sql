-- Contact merge queries
-- These queries support merging one contact (source) into another (target)

-- name: TransferContactMethods :exec
-- Transfer contact methods from source to target contact
UPDATE contact_method
SET contact_id = sqlc.arg(target_contact_id),
    updated_at = NOW()
WHERE contact_id = sqlc.arg(source_contact_id);

-- name: TransferReminders :exec
-- Transfer reminders from source to target contact
UPDATE reminder
SET contact_id = sqlc.arg(target_contact_id)
WHERE contact_id = sqlc.arg(source_contact_id)
  AND deleted_at IS NULL;

-- name: TransferInteractions :exec
-- Transfer interactions from source to target contact
UPDATE interaction
SET contact_id = sqlc.arg(target_contact_id)
WHERE contact_id = sqlc.arg(source_contact_id);

-- name: TransferNotes :exec
-- Transfer notes from source to target contact
UPDATE note
SET contact_id = sqlc.arg(target_contact_id),
    updated_at = NOW()
WHERE contact_id = sqlc.arg(source_contact_id);

-- name: TransferTimeEntries :exec
-- Transfer time entries from source to target contact
UPDATE time_entry
SET contact_id = sqlc.arg(target_contact_id),
    updated_at = NOW()
WHERE contact_id = sqlc.arg(source_contact_id);

-- name: TransferConnectionsAsContactA :exec
-- Transfer connections where source is contact_a to use target instead
-- This handles the bidirectional relationship table
UPDATE connection
SET contact_a_id = sqlc.arg(target_contact_id)
WHERE contact_a_id = sqlc.arg(source_contact_id)
  AND contact_b_id != sqlc.arg(target_contact_id);

-- name: TransferConnectionsAsContactB :exec
-- Transfer connections where source is contact_b to use target instead
UPDATE connection
SET contact_b_id = sqlc.arg(target_contact_id)
WHERE contact_b_id = sqlc.arg(source_contact_id)
  AND contact_a_id != sqlc.arg(target_contact_id);

-- name: DeleteDuplicateConnections :exec
-- Delete connections that would become duplicates after merge
-- (connections between source and target, or connections that now point same way)
DELETE FROM connection
WHERE (contact_a_id = sqlc.arg(source_contact_id) AND contact_b_id = sqlc.arg(target_contact_id))
   OR (contact_a_id = sqlc.arg(target_contact_id) AND contact_b_id = sqlc.arg(source_contact_id));

-- name: ReplaceContactInCalendarEvents :exec
-- Replace source contact ID with target contact ID in calendar event matched_contact_ids array
-- Uses array_replace for efficient in-place replacement
UPDATE calendar_event
SET matched_contact_ids = array_replace(matched_contact_ids, sqlc.arg(source_contact_id)::uuid, sqlc.arg(target_contact_id)::uuid),
    updated_at = NOW()
WHERE sqlc.arg(source_contact_id)::uuid = ANY(matched_contact_ids);

-- name: DeduplicateCalendarEventContacts :exec
-- Remove duplicate contact IDs that may result from merge
-- Uses subquery with DISTINCT to rebuild the array without duplicates
UPDATE calendar_event
SET matched_contact_ids = (
    SELECT array_agg(DISTINCT contact_id)
    FROM unnest(matched_contact_ids) AS contact_id
),
    updated_at = NOW()
WHERE sqlc.arg(target_contact_id)::uuid = ANY(matched_contact_ids)
  AND (
    SELECT COUNT(*) FROM unnest(matched_contact_ids) AS cid WHERE cid = sqlc.arg(target_contact_id)::uuid
  ) > 1;

-- name: CountMergeContactMethods :one
-- Count contact methods for a contact (for merge preview)
SELECT COUNT(*) FROM contact_method
WHERE contact_id = $1;

-- name: CountMergeReminders :one
-- Count active reminders for a contact (for merge preview)
SELECT COUNT(*) FROM reminder
WHERE contact_id = $1 AND deleted_at IS NULL;

-- name: CountMergeInteractions :one
-- Count interactions for a contact (for merge preview)
SELECT COUNT(*) FROM interaction
WHERE contact_id = $1;

-- name: CountMergeNotes :one
-- Count notes for a contact (for merge preview)
SELECT COUNT(*) FROM note
WHERE contact_id = $1;

-- name: CountMergeCalendarEvents :one
-- Count calendar events involving a contact (for merge preview)
SELECT COUNT(*) FROM calendar_event
WHERE $1::uuid = ANY(matched_contact_ids);

-- name: CountMergeTimeEntries :one
-- Count time entries for a contact (for merge preview)
SELECT COUNT(*) FROM time_entry
WHERE contact_id = $1;

-- name: FindDuplicateContactMethods :many
-- Find contact methods that exist in both source and target
-- Used to identify duplicates that will be skipped during merge
SELECT cm_source.value, cm_source.type
FROM contact_method cm_source
INNER JOIN contact_method cm_target
  ON cm_source.value_normalized = cm_target.value_normalized
  AND cm_source.type = cm_target.type
WHERE cm_source.contact_id = sqlc.arg(source_contact_id)
  AND cm_target.contact_id = sqlc.arg(target_contact_id);

-- name: DeleteDuplicateContactMethods :exec
-- Delete contact methods from source that already exist in target (by normalized value and type)
DELETE FROM contact_method cm_source
WHERE cm_source.contact_id = sqlc.arg(source_contact_id)
  AND EXISTS (
    SELECT 1 FROM contact_method cm_target
    WHERE cm_target.contact_id = sqlc.arg(target_contact_id)
      AND cm_target.value_normalized = cm_source.value_normalized
      AND cm_target.type = cm_source.type
  );
