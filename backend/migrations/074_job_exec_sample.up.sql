BEGIN;

-- job_exec_sample records one row per finished River job attempt (via the
-- Subscribe consumer in internal/jobsample). It exists to gather real
-- execution data — pool saturation, consumer wait-latency during saturation,
-- and run-duration by kind — over a multi-week window, since completed rows
-- prune from river_job after River's ~24h retention. Append-only, aggressively
-- trimmed (job_sample_trim periodic worker); no updated_at / trigger.
CREATE TABLE job_exec_sample (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    river_job_id  bigint      NOT NULL,   -- Job.ID (correlation; no FK — River owns river_job)
    kind          text        NOT NULL,
    queue         text        NOT NULL,
    attempted_at  timestamptz NOT NULL,   -- *Job.AttemptedAt (attempt start / slot acquire)
    finalized_at  timestamptz NOT NULL,   -- attempt end / slot release (synthesized for retryable failures)
    attempt       integer     NOT NULL,   -- Job.Attempt
    state         text        NOT NULL,   -- Job.State: completed | retryable | discarded | ...
    queue_wait_ms bigint      NOT NULL,   -- JobStats.QueueWaitDuration in ms (robust to retry scheduled_at mutation)
    created_at    timestamptz NOT NULL,   -- accelerated time at insert; trim key
    CONSTRAINT job_exec_sample_attempt_chk  CHECK (attempt >= 0),
    CONSTRAINT job_exec_sample_wait_chk     CHECK (queue_wait_ms >= 0),
    CONSTRAINT job_exec_sample_interval_chk CHECK (finalized_at >= attempted_at),
    -- attempted_at is part of the dedup key because River REUSES the same
    -- attempt value across a job's snooze re-executions (JobSnooze decrements
    -- attempt), while each re-execution's attempted_at is distinct (set fresh on
    -- every fetch→running). The triple separates genuine per-execution
    -- occupancy rows yet still dedups a re-delivered Subscribe event (identical
    -- river_job_id + attempt + attempted_at).
    CONSTRAINT job_exec_sample_unique       UNIQUE (river_job_id, attempt, attempted_at)
);
CREATE INDEX idx_job_exec_sample_created_at   ON job_exec_sample (created_at DESC);
CREATE INDEX idx_job_exec_sample_kind         ON job_exec_sample (kind);
-- attempted_at AND finalized_at both drive the sweep-line concurrency query
CREATE INDEX idx_job_exec_sample_attempted_at ON job_exec_sample (attempted_at);
CREATE INDEX idx_job_exec_sample_finalized_at ON job_exec_sample (finalized_at);

COMMIT;
