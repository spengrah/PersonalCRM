-- Drop trigger first
DROP TRIGGER IF EXISTS update_calendar_event_updated_at ON calendar_event;

-- Drop calendar event table and indexes
DROP TABLE IF EXISTS calendar_event;
