-- Auxiliary schema file for sqlc only, same contract as river_job.sql: River
-- manages its own schema via rivermigrate at runtime and we never apply this
-- file to a real database. It exists so sqlc can resolve the test-only cleanup
-- query that drops a queue row.
--
-- Why a query needs it: the synthetic replay harness runs its River client on a
-- PRIVATE per-namespace queue (so it can never fetch the live application's
-- jobs, nor the application its). A producer upserts a river_queue row for its
-- queue on start and on every heartbeat, so each seeded namespace leaves one
-- row behind; declared cleanup drops it with the rest of the namespace.
--
-- Keep in lockstep with the river version pinned in go.mod. Source of truth:
--   github.com/riverqueue/river/riverdriver/riverpgxv5/migration/main/

CREATE TABLE river_queue (
    name       text PRIMARY KEY NOT NULL,
    created_at timestamptz NOT NULL DEFAULT NOW(),
    metadata   jsonb NOT NULL DEFAULT '{}'::jsonb,
    paused_at  timestamptz,
    updated_at timestamptz NOT NULL
);
