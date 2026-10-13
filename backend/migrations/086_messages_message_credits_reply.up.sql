BEGIN;

-- Reply eligibility (R12/R37): whether a stored message is the user's own group
-- message as ingest stored it, the only kind of row that reads as a reply to a
-- member's inbound burst. A durable marker, because neither processing state
-- nor peer attribution can tell an ingested reply from a row the enrichment
-- added or cleared. Every existing row takes the default.
ALTER TABLE messages_message
    ADD COLUMN credits_reply BOOLEAN NOT NULL DEFAULT FALSE;

COMMIT;
