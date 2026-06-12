-- Sync-staleness watchdog breach store.
--
-- One row per active (and historically-resolved) staleness breach. The
-- watchdog (scheduler/staleness_watchdog_worker.go) reconciles this table
-- every 5 minutes: it opens a row when a source crosses its freshness
-- threshold and resolves the row when the source recovers, is disabled, or
-- is no longer evaluated. Resolved rows are retained for forensic history
-- and pruned past a fixed cutoff.
--
-- account_id is plain NOT NULL DEFAULT '' (empty = single-account/none;
-- Google email for multi-account pull rows; mac_host UUID string for
-- heartbeat/push breaches). The partial unique index doubles as the upsert
-- ON CONFLICT inference target, so a plain column avoids an expression
-- target in the conflict clause.
--
-- No updated_at column/trigger: last_observed_at is the meaningful "still
-- breaching" timestamp and is written from accelerated-time service params.
-- No deleted_at: breaches resolve, they are not user-deletable.

CREATE TABLE sync_staleness_breach (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    source TEXT NOT NULL,
    account_id TEXT NOT NULL DEFAULT '',
    breach_type TEXT NOT NULL CHECK (breach_type IN ('heartbeat', 'sync_stale', 'push_stale', 'sync_error')),
    stale_since TIMESTAMPTZ NOT NULL,
    threshold_seconds BIGINT NOT NULL,
    details TEXT NOT NULL DEFAULT '',
    detected_at TIMESTAMPTZ NOT NULL,
    last_observed_at TIMESTAMPTZ NOT NULL,
    resolved_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- At most one OPEN breach per (source, account_id, breach_type). The
-- partial predicate scopes uniqueness to unresolved rows so a resolved row
-- and a newly-reopened row for the same identity coexist. This index is
-- the ON CONFLICT inference target for the upsert-open path.
CREATE UNIQUE INDEX idx_sync_staleness_breach_open
    ON sync_staleness_breach (source, account_id, breach_type)
    WHERE resolved_at IS NULL;
