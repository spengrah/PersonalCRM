-- name: UpsertCalendarEvent :one
-- Insert or update a calendar event from Google Calendar
-- Note: last_contacted_updated is reset when matched_contact_ids changes (order-insensitive)
-- so newly matched contacts can be processed. Otherwise we preserve the processed state
-- to avoid duplicates.
INSERT INTO calendar_event (
    gcal_event_id,
    gcal_calendar_id,
    google_account_id,
    title,
    description,
    location,
    start_time,
    end_time,
    all_day,
    status,
    user_response,
    organizer_email,
    attendees,
    matched_contact_ids,
    synced_at,
    last_contacted_updated,
    html_link
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17
)
ON CONFLICT (gcal_event_id, gcal_calendar_id, google_account_id)
DO UPDATE SET
    title = EXCLUDED.title,
    description = EXCLUDED.description,
    location = EXCLUDED.location,
    start_time = EXCLUDED.start_time,
    end_time = EXCLUDED.end_time,
    all_day = EXCLUDED.all_day,
    status = EXCLUDED.status,
    user_response = EXCLUDED.user_response,
    organizer_email = EXCLUDED.organizer_email,
    attendees = EXCLUDED.attendees,
    matched_contact_ids = EXCLUDED.matched_contact_ids,
    last_contacted_updated = CASE
        WHEN array(SELECT unnest(calendar_event.matched_contact_ids) ORDER BY 1)
          IS DISTINCT FROM array(SELECT unnest(EXCLUDED.matched_contact_ids) ORDER BY 1)
        THEN FALSE
        ELSE calendar_event.last_contacted_updated
    END,
    synced_at = EXCLUDED.synced_at,
    html_link = EXCLUDED.html_link,
    updated_at = NOW()
RETURNING *;

-- name: GetCalendarEventByGcalID :one
-- Look up an event by its Google Calendar ID
SELECT * FROM calendar_event
WHERE gcal_event_id = $1
  AND gcal_calendar_id = $2
  AND google_account_id = $3
LIMIT 1;

-- name: GetCalendarEventByID :one
-- Look up an event by its UUID
SELECT * FROM calendar_event
WHERE id = $1
LIMIT 1;

-- name: ListEventsForContact :many
-- List calendar events involving a specific contact
SELECT * FROM calendar_event
WHERE sqlc.arg(contact_id)::uuid = ANY(matched_contact_ids)
  AND status != 'cancelled'
ORDER BY start_time DESC
LIMIT sqlc.arg(event_limit) OFFSET sqlc.arg(event_offset);

-- name: ListUpcomingEventsForContact :many
-- List upcoming calendar events for a specific contact
SELECT * FROM calendar_event
WHERE sqlc.arg(contact_id)::uuid = ANY(matched_contact_ids)
  AND status != 'cancelled'
  AND end_time >= sqlc.arg(after_time)
ORDER BY start_time ASC
LIMIT sqlc.arg(event_limit);

-- name: ListUpcomingEventsWithContacts :many
-- List upcoming events that have matched CRM contacts
SELECT * FROM calendar_event
WHERE array_length(matched_contact_ids, 1) > 0
  AND status != 'cancelled'
  AND start_time > $1
ORDER BY start_time ASC
LIMIT $2 OFFSET $3;

-- name: ListPastEventsNeedingUpdate :many
-- List past events that haven't updated last_contacted yet
SELECT * FROM calendar_event
WHERE last_contacted_updated = FALSE
  AND status = 'confirmed'
  AND end_time < $1
  AND array_length(matched_contact_ids, 1) > 0
ORDER BY end_time ASC
LIMIT $2;

-- name: MarkLastContactedUpdated :exec
-- Mark an event as having updated last_contacted for its contacts
UPDATE calendar_event
SET last_contacted_updated = TRUE,
    updated_at = NOW()
WHERE id = $1;

-- name: UpdateMatchedContacts :one
-- Update the matched contact IDs for an event
UPDATE calendar_event
SET matched_contact_ids = $2,
    updated_at = NOW()
WHERE id = $1
RETURNING *;

-- name: CountEventsForContact :one
-- Count events for a specific contact
SELECT COUNT(*) FROM calendar_event
WHERE sqlc.arg(contact_id)::uuid = ANY(matched_contact_ids)
  AND status != 'cancelled';

-- name: ListEventsByAccountAndDateRange :many
-- List events by Google account within a date range
SELECT * FROM calendar_event
WHERE google_account_id = $1
  AND start_time >= $2
  AND start_time <= $3
  AND status != 'cancelled'
ORDER BY start_time ASC;

-- name: DeleteEventsByAccount :exec
-- Delete all events for a Google account (used when revoking access)
DELETE FROM calendar_event
WHERE google_account_id = $1;

-- name: FindEventsByAttendeeEmailUnmatchedForContact :many
-- Finds events whose JSONB attendees contain the given normalized email but
-- do not yet have the contact in matched_contact_ids. Used by the rematch
-- service to retroactively link historical calendar events when a contact
-- method is added to a CRM contact. Backed by
-- idx_calendar_event_attendees_email_lower_gin via the
-- jsonb_array_lower_values helper.
-- calendar_event has no deleted_at column — do not filter on it.
SELECT * FROM calendar_event
WHERE jsonb_array_lower_values(attendees, 'email') && ARRAY[LOWER(@email::text)]
  AND NOT (@contact_id::uuid = ANY(matched_contact_ids))
  AND status != 'cancelled';

-- name: AppendMatchedContact :exec
-- Atomically appends a contact to an event's matched_contact_ids iff it isn't
-- already present. Does NOT reset last_contacted_updated — the rematch handler
-- records interactions directly for past events (see rematch plan Design
-- Decision 6) so the scheduler race is avoided at the source.
UPDATE calendar_event
SET matched_contact_ids = array_append(matched_contact_ids, @contact_id::uuid),
    updated_at = NOW()
WHERE id = @event_id::uuid
  AND NOT (@contact_id::uuid = ANY(matched_contact_ids));

-- name: TestHardDeleteCalendarEventByID :exec
-- TEST ONLY. Hard-deletes a calendar_event row by primary key. Used
-- by integration tests that exercise the "target row vanished between
-- snapshot and resolve-link" path. Production code must NOT call this.
DELETE FROM calendar_event WHERE id = sqlc.arg('id');

-- name: FindCalendarEventsInWindow :many
-- Returns candidate calendar_event rows for the meeting_note.recorded
-- linkage-detection algorithm. Filters out cancelled events. Backed by
-- idx_calendar_event_start (partial index on start_time WHERE
-- status != 'cancelled' — already exists from migration 016). The
-- output includes matched_contact_ids so the linkage handler can
-- compute walk-in supplementals.
SELECT * FROM calendar_event
WHERE start_time BETWEEN sqlc.arg('window_start') AND sqlc.arg('window_end')
  AND status != 'cancelled'
ORDER BY start_time ASC;

-- name: DeleteCalendarEventByGcalID :exec
-- Hard-deletes the stored calendar_event row keyed by its Google identity
-- triple. Used by the decline/cancel remove branch. calendar_event has no
-- deleted_at column — removal is a hard DELETE (cf. DeleteEventsByAccount).
DELETE FROM calendar_event
WHERE gcal_event_id = $1
  AND gcal_calendar_id = $2
  AND google_account_id = $3;

-- name: MarkCalendarEventCancelledByGcalID :exec
-- Off-mode deferral for the decline remove branch: marks a stored event
-- cancelled (keyed by its Google identity triple) instead of deleting,
-- when the event bus is unavailable. status='cancelled' excludes the row
-- from ListPastEventsNeedingUpdate (status='confirmed') and from all
-- contact-facing reads (status != 'cancelled'), so it neither re-fires
-- calendar.attended nor strands an already-recorded interaction.
UPDATE calendar_event
SET status = 'cancelled',
    updated_at = NOW()
WHERE gcal_event_id = $1
  AND gcal_calendar_id = $2
  AND google_account_id = $3;

-- name: GetCalendarEventByIDForShare :one
-- Locking read used by the InteractionRecorder calendar.attended branch to
-- serialize against a concurrent decline DELETE on the same row. FOR SHARE
-- holds the row until the attended tx commits, so an interleaving decline
-- DELETE either blocks (attended inserts, decline then soft-deletes) or has
-- already committed (this read returns no row, attended skips the insert).
SELECT * FROM calendar_event
WHERE id = $1
FOR SHARE;

-- name: TestGetCalendarEventByIDForUpdateNoWait :one
-- TEST ONLY. Probe a calendar_event row with FOR UPDATE NOWAIT: returns the
-- row if no conflicting lock is held, or fails immediately (lock_not_available)
-- when another tx holds a conflicting lock (e.g. a FOR SHARE from the attended
-- branch). Used by the attended-vs-decline lock-serialization integration
-- test to prove the attended FOR SHARE conflicts with a concurrent FOR UPDATE
-- without a sleep/timeout. Production code must NOT call this.
SELECT * FROM calendar_event
WHERE id = $1
FOR UPDATE NOWAIT;

-- name: ListCalendarEventsByIDs :many
SELECT * FROM calendar_event
WHERE id = ANY(@ids::uuid[]);
