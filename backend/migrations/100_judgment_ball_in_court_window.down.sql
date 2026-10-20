BEGIN;

ALTER TABLE judgment
    DROP CONSTRAINT judgment_ball_in_court_window_shape,
    DROP COLUMN ball_in_court_window_start;

COMMIT;
