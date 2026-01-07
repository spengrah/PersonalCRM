-- Add html_link column to store Google Calendar event URL for deep linking
ALTER TABLE calendar_event ADD COLUMN html_link TEXT;
