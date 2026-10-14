BEGIN;

ALTER TABLE messages_message
    DROP COLUMN credits_reply;

COMMIT;
