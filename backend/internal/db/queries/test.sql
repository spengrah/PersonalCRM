-- Test data management queries
-- These queries are used by the test API endpoints to seed and cleanup test data

-- name: DeleteContactsByNamePrefix :execrows
DELETE FROM contact WHERE full_name LIKE $1 || '%';

-- name: DeleteExternalContactsByDisplayNamePrefix :execrows
DELETE FROM external_contact WHERE display_name LIKE $1 || '%';

-- name: DeleteExternalContactsBySourceIDPrefix :execrows
DELETE FROM external_contact WHERE source_id LIKE $1 || '%';

-- name: CountContactsByNamePrefix :one
SELECT COUNT(*) FROM contact WHERE full_name LIKE $1 || '%';

-- name: CountExternalContactsByDisplayNamePrefix :one
SELECT COUNT(*) FROM external_contact WHERE display_name LIKE $1 || '%';

-- name: DeleteCalendarEventsByTitlePrefix :execrows
DELETE FROM calendar_event WHERE title LIKE $1 || '%';

-- name: DeleteCalendarEventsByGcalEventIdPrefix :execrows
DELETE FROM calendar_event WHERE gcal_event_id LIKE $1 || '%';
