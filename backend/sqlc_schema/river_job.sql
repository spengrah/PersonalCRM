-- Auxiliary schema file for sqlc only. River manages its own schema via
-- rivermigrate at runtime; we never apply this file to a real database.
-- It exists so sqlc can resolve queries that reference river_job (e.g.
-- CountInFlightSyncJobs in internal/db/queries/external_sync.sql).
--
-- Keep this in lockstep with the river version pinned in go.mod if the
-- `state` enum values or the `args` column change. Source of truth:
--   github.com/riverqueue/river/riverdriver/riverpgxv5/migration/main/
--
-- NOTE: this file is intentionally NOT under backend/migrations/ so
-- golang-migrate ignores it at boot.

CREATE TYPE river_job_state AS ENUM (
    'available',
    'cancelled',
    'completed',
    'discarded',
    'pending',
    'retryable',
    'running',
    'scheduled'
);

CREATE TABLE river_job (
    id           bigserial PRIMARY KEY,
    state        river_job_state NOT NULL DEFAULT 'available',
    attempt      smallint NOT NULL DEFAULT 0,
    max_attempts smallint NOT NULL,
    attempted_at timestamptz,
    created_at   timestamptz NOT NULL DEFAULT NOW(),
    finalized_at timestamptz,
    scheduled_at timestamptz NOT NULL DEFAULT NOW(),
    priority     smallint NOT NULL DEFAULT 1,
    args         jsonb,
    attempted_by text[],
    errors       jsonb[],
    kind         text NOT NULL,
    metadata     jsonb NOT NULL DEFAULT '{}',
    queue        text NOT NULL DEFAULT 'default',
    tags         varchar(255)[],
    unique_key   bytea,
    unique_states bit(8)
);
