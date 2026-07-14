-- Contact merge queries
-- These queries support merging one contact (source) into another (target)

-- name: TransferContactMethods :exec
-- Transfer contact methods from source to target contact
UPDATE contact_method
SET contact_id = sqlc.arg(target_contact_id),
    updated_at = NOW()
WHERE contact_id = sqlc.arg(source_contact_id);

-- name: TransferInteractions :exec
-- Transfer interactions from source to target contact (includes soft-deleted for audit trail)
UPDATE interaction
SET contact_id = sqlc.arg(target_contact_id)
WHERE contact_id = sqlc.arg(source_contact_id);

-- name: TransferNotes :exec
-- Transfer notes from source to target contact
UPDATE note
SET contact_id = sqlc.arg(target_contact_id),
    updated_at = NOW()
WHERE contact_id = sqlc.arg(source_contact_id);

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

-- name: RepointIdentitiesToContact :exec
-- Re-point identity-cache rows from the merge source (loser) to the target
-- (winner) so future inbound events from the loser's handles attribute to
-- the survivor. Collision-free: external_identity uniqueness is on
-- (identifier, identifier_type, source) globally, so at most one row exists
-- per triple regardless of contact.
UPDATE external_identity
SET contact_id = sqlc.arg(target_contact_id),
    updated_at = NOW()
WHERE contact_id = sqlc.arg(source_contact_id);

-- name: RepointExternalContactsToContact :exec
-- Re-point import links from the merge source to the target. Both
-- crm_contact_id indexes are non-unique, so this is collision-free. The
-- external_contact upsert preserves crm_contact_id on re-sync, so the
-- repoint is not overwritten by the next daemon sync.
UPDATE external_contact
SET crm_contact_id = sqlc.arg(target_contact_id),
    updated_at = NOW()
WHERE crm_contact_id = sqlc.arg(source_contact_id);

-- name: RepointMessagesMessageContact :exec
-- Re-point iMessage staging rows (committed pre-merge, incl. unprocessed)
-- from the merge source to the target. messages_message uniqueness is on
-- the message guid, not the contact — collision-free. NOTE: the table has
-- no updated_at column (049); do not set one.
UPDATE messages_message
SET matched_contact_id = sqlc.arg(target_contact_id)
WHERE matched_contact_id = sqlc.arg(source_contact_id);

-- name: RepointTelegramMessageContact :exec
-- Re-point Telegram staging rows from the merge source to the target.
-- Uniqueness is on (telegram_chat_id, telegram_message_id) — collision-free.
-- NOTE: telegram_message has no updated_at column (032); do not set one.
UPDATE telegram_message
SET matched_contact_id = sqlc.arg(target_contact_id)
WHERE matched_contact_id = sqlc.arg(source_contact_id);

-- name: RepointPhoneCallContact :exec
-- Re-point call staging rows from the merge source to the target.
-- Uniqueness is on call_unique_id — collision-free. NOTE: phone_call has
-- no updated_at column (055); do not set one.
UPDATE phone_call
SET matched_contact_id = sqlc.arg(target_contact_id)
WHERE matched_contact_id = sqlc.arg(source_contact_id);

-- name: CountMergeContactMethods :one
-- Count contact methods for a contact (for merge preview)
SELECT COUNT(*) FROM contact_method
WHERE contact_id = $1;

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

-- name: DemoteSourcePrimaryMethods :exec
-- Demote source's primary contact methods when the target already has a primary.
-- The one-primary rule is per contact — idx_contact_method_primary is a unique
-- partial index on (contact_id) WHERE is_primary = TRUE — so the source's primary
-- must be demoted regardless of method type before TransferContactMethods, or a
-- cross-type dual-primary pair violates the index and fails the merge.
UPDATE contact_method cm_source
SET is_primary = false,
    updated_at = NOW()
WHERE cm_source.contact_id = sqlc.arg(source_contact_id)
  AND cm_source.is_primary = true
  AND EXISTS (
    SELECT 1 FROM contact_method cm_target
    WHERE cm_target.contact_id = sqlc.arg(target_contact_id)
      AND cm_target.is_primary = true
  );
