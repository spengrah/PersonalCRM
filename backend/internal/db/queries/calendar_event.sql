-- name: UpsertCalendarEvent :one
-- Insert or update a calendar event from Google Calendar
-- Note: last_contacted_updated is intentionally NOT included in the ON CONFLICT UPDATE clause.
-- Once an event has been processed (last_contacted_updated = TRUE), we preserve that state
-- even if the event is re-synced with updated details. This prevents duplicate last_contacted updates.
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
    last_contacted_updated
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16
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
    synced_at = EXCLUDED.synced_at,
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
  AND start_time > sqlc.arg(after_time)
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
