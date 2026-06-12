-- /health component queries over river_job.
--
-- These are the production-grade reads behind the river + sync /health
-- components (#483). All timestamps are supplied by the handler from
-- accelerated time — these queries never call NOW() (mirroring the
-- sync_staleness.sql convention).

-- name: CountDiscardedRiverJobs :one
-- Count jobs that exhausted their retries and landed in 'discarded'. Discarded
-- rows are finalized and retained 90d; any non-zero count is an operator
-- signal (remediation: crm-admin --retry-job).
SELECT COUNT(*) FROM river_job WHERE state = 'discarded';

-- name: OldestDueRiverJobScheduledAt :one
-- The earliest scheduled_at among jobs that are due now but not yet picked up
-- by a worker — the stall signal for a starved/dead worker pool. scheduled_at
-- is the next-eligibility time: for 'available' it equals creation time, for
-- 'retryable' River sets it to now+backoff, so it is the correct "oldest due"
-- column (created_at would be the original creation time for a retryable job;
-- attempted_at is NULL until the first attempt). The scheduled_at <= @now guard
-- excludes retryable jobs whose backoff lies in the future — those are not
-- waiting, they are not yet due.
--
-- Deliberate state-set asymmetry vs CountInFlightSyncJobs (which also counts
-- 'pending' and 'scheduled' as in-flight): 'pending' jobs are gated on River
-- workflow conditions, and past-due 'scheduled' rows transition to 'available'
-- on the scheduler's next pass — neither is "a due job the workers are failing
-- to pick up", which is what this query measures.
--
-- MIN(...) over zero rows returns NULL; the ::timestamptz cast pins the
-- aggregate's generated type. The repository maps NULL to a nil *time.Time
-- ("no due jobs").
SELECT MIN(scheduled_at)::timestamptz AS oldest_scheduled_at FROM river_job
WHERE state IN ('available', 'retryable') AND scheduled_at <= @now::timestamptz;

-- name: LatestCompletedRiverJobByKind :one
-- The newest finalized_at among COMPLETED jobs of a given kind — the
-- watchdog-liveness signal that breaks the circular dependency where the sync
-- component trusts a breach table that only the watchdog (itself a River job)
-- keeps fresh. Filtering to 'completed' (not any-state) is load-bearing: an
-- enqueue-trail check would stay fresh while a persistently-FAILING watchdog
-- worker froze the breach table; the completion trail proves the watchdog
-- actually RAN successfully. MAX(...) over zero rows returns NULL → nil
-- *time.Time ("watchdog not running"); the ::timestamptz cast pins the type.
SELECT MAX(finalized_at)::timestamptz AS latest_finalized_at FROM river_job
WHERE kind = @kind AND state = 'completed';
