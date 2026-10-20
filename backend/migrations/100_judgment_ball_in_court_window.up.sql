-- The window ball-in-court read: a night a method judges in chunks asks
-- ball-in-court once, in its last chunk, so the reply-obligation rule compares
-- an obligation's evidence with that chunk's window, not with the night's
-- first window.
--
--   ball_in_court_window_start  the message key of the first message of the
--                               window ball-in-court was asked over; the
--                               judgment's window_start when it read one
--                               window. NULL exactly when window_start is, for
--                               a judgment of a version before migration 099,
--                               which read its whole context.
--
-- Wrapped in an explicit transaction: the postgres migration driver does NOT
-- auto-wrap a migration file, so multi-statement DDL must self-wrap to stay
-- all-or-nothing on a mid-file failure (Postgres DDL is transactional).

BEGIN;

ALTER TABLE judgment ADD COLUMN ball_in_court_window_start TEXT;

UPDATE judgment SET ball_in_court_window_start = window_start WHERE window_start IS NOT NULL;

ALTER TABLE judgment ADD CONSTRAINT judgment_ball_in_court_window_shape
    CHECK ((window_start IS NULL) = (ball_in_court_window_start IS NULL));

COMMIT;
