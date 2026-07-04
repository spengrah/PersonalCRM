-- Queries over job_exec_sample (the River job-execution sampling table) plus
-- the Tier-0 one-shot read over live river_job.
--
-- Convention (mirrors health.sql / sync_staleness.sql): every timestamp is
-- supplied by the caller from accelerated time — these queries NEVER call
-- NOW(). Analyst decision queries take @window_start/@window_end; the trim + Tier-0 take an
-- accelerated @cutoff. This keeps insert-time (job_exec_sample.created_at) and
-- trim-time on the same clock under time acceleration.

-- name: InsertJobExecSample :exec
-- Plain per-event insert (no tx). created_at is supplied explicitly from
-- accelerated time (no DEFAULT NOW()). ON CONFLICT dedups a re-delivered
-- Subscribe event so it cannot double-count and skew the aggregates, without
-- erroring the writer.
INSERT INTO job_exec_sample (river_job_id, kind, queue, attempted_at, finalized_at, attempt, state, queue_wait_ms, created_at)
VALUES (@river_job_id, @kind, @queue, @attempted_at, @finalized_at, @attempt, @state, @queue_wait_ms, @created_at)
ON CONFLICT (river_job_id, attempt, attempted_at) DO NOTHING;

-- name: TrimJobExecSamples :execrows
-- Housekeeping DELETE. Cutoff is accelerated-now minus the retention window,
-- computed by the caller (NOT SQL NOW()).
DELETE FROM job_exec_sample WHERE created_at < @cutoff::timestamptz;

-- name: Tier0RiverJobStatsByKind :many
-- One-shot wait/run-by-kind read over river_job rows finalized since @cutoff,
-- before job_exec_sample has accrued. finalized_at is authoritative here (only
-- finished rows have it); this reads the live River table, not our sample table.
-- APPROXIMATE: wait here is attempted_at - scheduled_at over the LAST attempt
-- only, and River mutates scheduled_at on retry, so for retried jobs this
-- under/over-states the true available-wait. It is a rough first signal ONLY; do
-- NOT compare it numerically to the Tier-1 queue_wait_ms metric, which is
-- River's exact QueueWaitDuration.
SELECT kind,
       COUNT(*)::bigint AS n,
       percentile_cont(0.5) WITHIN GROUP (ORDER BY EXTRACT(EPOCH FROM (attempted_at - scheduled_at)))::float8 AS p50_wait_s,
       percentile_cont(0.5) WITHIN GROUP (ORDER BY EXTRACT(EPOCH FROM (finalized_at  - attempted_at)))::float8 AS p50_run_s
FROM river_job
WHERE finalized_at IS NOT NULL
  -- attempted_at is nullable: a job cancelled/discarded BEFORE it ran is
  -- finalized but never attempted. Without this guard its NULL attempted_at
  -- yields NULL percentiles, and the ::float8 cast makes sqlc scan into a
  -- non-nullable float64 → the whole read aborts. Exclude never-run rows.
  AND attempted_at IS NOT NULL
  AND finalized_at > @cutoff::timestamptz
GROUP BY kind
ORDER BY kind;

-- name: JobExecMaxConcurrency :one
-- Metric 1: peak concurrency over [@window_start,@window_end]. Intervals are selected by
-- OVERLAP with the window and clamped to it (so a job spanning the whole window
-- still contributes); deltas are netted per timestamp before the running sum
-- (so a release and an acquire at the same instant cancel — no false peak).
WITH ov AS (
    SELECT GREATEST(attempted_at, @window_start::timestamptz) AS start_t,
           LEAST(finalized_at,  @window_end::timestamptz)    AS end_t
    FROM job_exec_sample
    WHERE attempted_at <= @window_end::timestamptz AND finalized_at >= @window_start::timestamptz
),
endpoints AS (
    SELECT start_t AS t,  1 AS delta FROM ov
    UNION ALL
    SELECT end_t   AS t, -1 AS delta FROM ov
),
netted AS ( SELECT t, SUM(delta) AS d FROM endpoints GROUP BY t ),
running AS ( SELECT t, SUM(d) OVER (ORDER BY t) AS c FROM netted )
SELECT COALESCE(MAX(c), 0)::int AS max_concurrency FROM running;

-- name: JobExecSaturatedSeconds :one
-- Metric 1: total wall-seconds spent at concurrency >= @threshold (pass
-- MaxWorkers). Same netted, window-clipped timeline as JobExecMaxConcurrency.
WITH ov AS (
    SELECT GREATEST(attempted_at, @window_start::timestamptz) AS start_t,
           LEAST(finalized_at,  @window_end::timestamptz)    AS end_t
    FROM job_exec_sample
    WHERE attempted_at <= @window_end::timestamptz AND finalized_at >= @window_start::timestamptz
),
endpoints AS (
    SELECT start_t AS t,  1 AS delta FROM ov
    UNION ALL
    SELECT end_t   AS t, -1 AS delta FROM ov
),
netted AS ( SELECT t, SUM(delta) AS d FROM endpoints GROUP BY t ),
running AS ( SELECT t, SUM(d) OVER (ORDER BY t) AS c FROM netted ),
seg AS ( SELECT t, c, LEAD(t) OVER (ORDER BY t) AS next_t FROM running )
SELECT COALESCE(EXTRACT(EPOCH FROM SUM(next_t - t) FILTER (WHERE c >= @threshold::int)), 0)::float8
       AS saturated_seconds
FROM seg WHERE next_t IS NOT NULL;

-- name: JobExecRunDurationByKind :many
-- Metric 3: run duration percentiles per kind over [@window_start,@window_end].
SELECT kind,
       COUNT(*)::bigint AS n,
       percentile_cont(0.5)  WITHIN GROUP (ORDER BY EXTRACT(EPOCH FROM (finalized_at - attempted_at)))::float8 AS p50_run_s,
       percentile_cont(0.95) WITHIN GROUP (ORDER BY EXTRACT(EPOCH FROM (finalized_at - attempted_at)))::float8 AS p95_run_s,
       MAX(EXTRACT(EPOCH FROM (finalized_at - attempted_at)))::float8 AS max_run_s
FROM job_exec_sample
WHERE attempted_at >= @window_start::timestamptz AND attempted_at <= @window_end::timestamptz
GROUP BY kind
ORDER BY kind;

-- name: JobExecWaitByKind :many
-- Metric 2 (context): overall wait percentiles per kind, from the stored
-- queue_wait_ms (River's exact QueueWaitDuration — NOT a timestamp subtraction).
SELECT kind,
       COUNT(*)::bigint AS n,
       percentile_cont(0.5)  WITHIN GROUP (ORDER BY queue_wait_ms / 1000.0)::float8 AS p50_wait_s,
       percentile_cont(0.95) WITHIN GROUP (ORDER BY queue_wait_ms / 1000.0)::float8 AS p95_wait_s,
       (MAX(queue_wait_ms) / 1000.0)::float8 AS max_wait_s
FROM job_exec_sample
WHERE attempted_at >= @window_start::timestamptz AND attempted_at <= @window_end::timestamptz
GROUP BY kind
ORDER BY kind;

-- name: JobExecWaitDuringSaturationByKind :many
-- THE gate query for metric 2. For each waiter, its wait window is
-- [eligible_at, attempted_at] where eligible_at = attempted_at - queue_wait
-- (robust to retry scheduled_at mutation). Reports, per kind, the wait DURATION
-- that overlapped a SATURATED segment (c >= @threshold) vs. window-clipped
-- denominators. Denominators (all consistent, window-clipped so a
-- boundary-spanning wait can't understate the ratio): waited_in_window_s = the
-- wait window clipped to [@window_start,@window_end]; saturated_wait_s ⊆ waited_in_window_s
-- (both window-clipped). total_wait_s is the FULL per-attempt queue_wait for
-- context only (may exceed the window). Use saturated_wait_s / waited_in_window_s
-- as the "waits because the pool is full" ratio.
WITH ov AS (
    SELECT GREATEST(attempted_at, @window_start::timestamptz) AS start_t, LEAST(finalized_at, @window_end::timestamptz) AS end_t
    FROM job_exec_sample WHERE attempted_at <= @window_end::timestamptz AND finalized_at >= @window_start::timestamptz
),
endpoints AS ( SELECT start_t AS t, 1 AS delta FROM ov UNION ALL SELECT end_t, -1 FROM ov ),
netted   AS ( SELECT t, SUM(delta) AS d FROM endpoints GROUP BY t ),
running  AS ( SELECT t, SUM(d) OVER (ORDER BY t) AS c FROM netted ),
seg      AS ( SELECT t AS seg_start, LEAD(t) OVER (ORDER BY t) AS seg_end, c FROM running ),
sat      AS ( SELECT seg_start, seg_end FROM seg WHERE seg_end IS NOT NULL AND c >= @threshold::int ),
waiters  AS (
    SELECT id, kind, queue_wait_ms, attempted_at,
           attempted_at - make_interval(secs => queue_wait_ms / 1000.0) AS eligible_at,
           GREATEST(attempted_at - make_interval(secs => queue_wait_ms / 1000.0), @window_start::timestamptz) AS clip_start,
           LEAST(attempted_at, @window_end::timestamptz) AS clip_end
    FROM job_exec_sample
    WHERE attempted_at >= @window_start::timestamptz AND attempted_at <= @window_end::timestamptz AND queue_wait_ms > 0
),
wait_sat AS (
    SELECT w.id, w.kind, w.queue_wait_ms,
           GREATEST(EXTRACT(EPOCH FROM (w.clip_end - w.clip_start)), 0) AS waited_in_window_s,
           -- CASE guards the LEFT JOIN no-match row: LEAST/GREATEST ignore NULLs,
           -- so LEAST(clip_end, NULL) - GREATEST(clip_start, NULL) would wrongly
           -- yield the FULL wait window for a waiter that overlapped NO saturated
           -- segment. Force 0 in that case so saturated_wait_s ⊆ waited_in_window_s.
           COALESCE(SUM(
               CASE WHEN s.seg_start IS NULL THEN 0
                    ELSE EXTRACT(EPOCH FROM (LEAST(w.clip_end, s.seg_end) - GREATEST(w.clip_start, s.seg_start)))
               END), 0) AS sat_wait_s
    FROM waiters w
    LEFT JOIN sat s ON s.seg_start < w.clip_end AND s.seg_end > w.clip_start
    GROUP BY w.id, w.kind, w.queue_wait_ms, w.clip_start, w.clip_end
)
SELECT kind,
       COUNT(*)::bigint AS n_waiters,
       SUM(queue_wait_ms / 1000.0)::float8 AS total_wait_s,
       SUM(waited_in_window_s)::float8     AS waited_in_window_s,
       SUM(sat_wait_s)::float8             AS saturated_wait_s,
       percentile_cont(0.95) WITHIN GROUP (ORDER BY queue_wait_ms / 1000.0)::float8 AS p95_wait_s
FROM wait_sat
GROUP BY kind
ORDER BY kind;

-- name: JobExecWaitSlotBlameByKind :many
-- THE decisive metric-2 query: duration-weighted "who held the pool WHILE a
-- given kind was waiting during saturation." For each waiter, intersect its wait
-- window [eligible_at, attempted_at] with the saturated segments; then, over
-- exactly those intersections, attribute slot-seconds to each OTHER running
-- job's kind. Grouped by (wait_kind, running_kind), so a consumer kind's blame
-- mix is scoped to ITS OWN saturated waits — a different saturated period
-- dominated by other kinds cannot bleed into it.
WITH ov AS (
    SELECT id, kind, GREATEST(attempted_at, @window_start::timestamptz) AS start_t, LEAST(finalized_at, @window_end::timestamptz) AS end_t
    FROM job_exec_sample WHERE attempted_at <= @window_end::timestamptz AND finalized_at >= @window_start::timestamptz
),
endpoints AS ( SELECT start_t AS t, 1 AS delta FROM ov UNION ALL SELECT end_t, -1 FROM ov ),
netted   AS ( SELECT t, SUM(delta) AS d FROM endpoints GROUP BY t ),
running  AS ( SELECT t, SUM(d) OVER (ORDER BY t) AS c FROM netted ),
seg      AS ( SELECT t AS seg_start, LEAD(t) OVER (ORDER BY t) AS seg_end, c FROM running ),
sat      AS ( SELECT seg_start, seg_end FROM seg WHERE seg_end IS NOT NULL AND c >= @threshold::int ),
waiters  AS (
    SELECT id, kind, attempted_at,
           attempted_at - make_interval(secs => queue_wait_ms / 1000.0) AS eligible_at
    FROM job_exec_sample
    WHERE attempted_at >= @window_start::timestamptz AND attempted_at <= @window_end::timestamptz AND queue_wait_ms > 0
),
wait_sat AS (
    SELECT w.id AS wait_id, w.kind AS wait_kind,
           GREATEST(w.eligible_at, s.seg_start) AS iv_start,
           LEAST(w.attempted_at,  s.seg_end)    AS iv_end
    FROM waiters w
    JOIN sat s ON s.seg_start < w.attempted_at AND s.seg_end > w.eligible_at
),
blame AS (
    SELECT ws.wait_kind, r.kind AS running_kind,
           EXTRACT(EPOCH FROM (LEAST(ws.iv_end, r.end_t) - GREATEST(ws.iv_start, r.start_t))) AS secs
    FROM wait_sat ws
    JOIN ov r ON r.id <> ws.wait_id AND r.start_t < ws.iv_end AND r.end_t > ws.iv_start
)
SELECT wait_kind, running_kind, SUM(secs)::float8 AS blame_slot_s
FROM blame WHERE secs > 0
GROUP BY wait_kind, running_kind
ORDER BY wait_kind, blame_slot_s DESC;

-- name: ListJobExecSamplesByRiverJobIDForTest :many
-- Test-only read of sample rows by river_job_id (raw SQL is banned in Go tests,
-- so the real-Subscribe integration test reads rows back through this query).
SELECT river_job_id, kind, queue, attempted_at, finalized_at, attempt, state, queue_wait_ms, created_at
FROM job_exec_sample WHERE river_job_id = ANY(@river_job_ids::bigint[]) ORDER BY river_job_id, attempt;
