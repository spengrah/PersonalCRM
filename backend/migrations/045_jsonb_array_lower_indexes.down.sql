-- 045_jsonb_array_lower_indexes.down.sql
-- Reverse 045. Drop the indexes first (they reference the function), then
-- the function. IF EXISTS so partial rollback after a failed up doesn't
-- leave the down migration aborted.

DROP INDEX IF EXISTS idx_external_contact_emails_value_lower_gin;
DROP INDEX IF EXISTS idx_calendar_event_attendees_email_lower_gin;
DROP FUNCTION IF EXISTS jsonb_array_lower_values(jsonb, text);
