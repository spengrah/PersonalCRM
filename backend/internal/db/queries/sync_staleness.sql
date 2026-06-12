-- Sync-staleness watchdog breach queries.
--
-- The watchdog reconciles sync_staleness_breach every tick: upsert-open for
-- each current breach candidate, resolve any open row no longer observed,
-- then prune resolved history past a fixed cutoff. All timestamps are
-- supplied by the service from accelerated time — these queries never call
-- NOW() (except the table's ops-only created_at default).

-- name: UpsertOpenStalenessBreach :one
-- Opens a breach for (source, account_id, breach_type) or refreshes the
-- existing open row. The ON CONFLICT target infers the partial unique index
-- idx_sync_staleness_breach_open, so two overlapping watchdog runs converge
-- on the same open row instead of raising 23505. detected_at is set on
-- insert and intentionally NOT in the update list (first-detection time is
-- immutable); last_observed_at advances each tick the breach persists. The
-- @observed_at arg seeds both detected_at and last_observed_at on insert so a
-- freshly-opened row has detected_at == last_observed_at (the "new breach"
-- signal the service logs on).
INSERT INTO sync_staleness_breach (
    source,
    account_id,
    breach_type,
    stale_since,
    threshold_seconds,
    details,
    detected_at,
    last_observed_at
) VALUES (
    @source,
    @account_id,
    @breach_type,
    @stale_since,
    @threshold_seconds,
    @details,
    @observed_at,
    @observed_at
)
ON CONFLICT (source, account_id, breach_type) WHERE resolved_at IS NULL
DO UPDATE SET
    stale_since = EXCLUDED.stale_since,
    threshold_seconds = EXCLUDED.threshold_seconds,
    details = EXCLUDED.details,
    last_observed_at = EXCLUDED.last_observed_at
RETURNING *;

-- name: ListOpenStalenessBreaches :many
-- All currently-open breaches, deterministically ordered for stable log
-- output and the reconcile diff. Used both by the watchdog (to compute the
-- resolve set) and by the read endpoint (active breaches only).
SELECT * FROM sync_staleness_breach
WHERE resolved_at IS NULL
ORDER BY detected_at ASC, id ASC;

-- name: ResolveStalenessBreach :execrows
-- Marks one open breach resolved. The resolved_at IS NULL guard makes this
-- idempotent under overlapping ticks: a concurrent run that already resolved
-- the row affects zero rows here.
UPDATE sync_staleness_breach
SET resolved_at = @resolved_at
WHERE id = @id AND resolved_at IS NULL;

-- name: DeleteResolvedStalenessBreachesBefore :exec
-- Retention prune: drops resolved breaches whose resolved_at predates the
-- cutoff. Open breaches (resolved_at IS NULL) are never touched.
DELETE FROM sync_staleness_breach
WHERE resolved_at IS NOT NULL AND resolved_at < @cutoff;

-- name: CountStalenessBreachesByAccountForTest :one
-- Test-only: counts ALL breach rows (open or resolved) for an account_id. The
-- production read path exposes open breaches only, so the retention test uses
-- this to confirm resolved history was (or was not) pruned. Production code
-- never calls this.
SELECT COUNT(*) FROM sync_staleness_breach WHERE account_id = @account_id;
