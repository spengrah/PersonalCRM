-- 041_contact_task_followup_idempotency.up.sql
-- Schema groundwork for the FollowUpManager consumer (event-bus foundation,
-- spec §3.4.3). Adds the columns and indexes the consumer needs to perform
-- crash-safe two-step follow-up creation:
--   1. idempotency_key column for local idempotency lookups.
--   2. pending_remote_create state for the row inserted before the remote
--      Todoist task exists.
--   3. Partial unique index restricting "live" follow-ups to at most one row
--      per (contact_id, provider, kind) — terminal-state rows accumulate
--      freely so a fresh follow-up can be inserted alongside a completed or
--      dismissed predecessor.
--   4. Partial unique index on (contact_id, kind, idempotency_key) for
--      deterministic local idempotency.
--
-- The shadow consumer in this migration's accompanying code computes the
-- values it WOULD write but does not insert any rows; the partial indexes
-- are empty until the cutover consumer starts writing. Existing pre-
-- migration rows have idempotency_key = NULL and are unaffected by either
-- partial index.

ALTER TABLE contact_task
    ADD COLUMN idempotency_key text;

-- Replace the state CHECK to add the new pending_remote_create value while
-- keeping the four existing values from migration 033. DROP IF EXISTS for
-- both the auto-named and explicitly-named variants in case Postgres ever
-- assigned a different name.
ALTER TABLE contact_task DROP CONSTRAINT IF EXISTS contact_task_state_check;
ALTER TABLE contact_task DROP CONSTRAINT IF EXISTS contact_task_state_chk;
ALTER TABLE contact_task
    ADD CONSTRAINT contact_task_state_check
    CHECK (state IN (
        'managed', 'unmanaged', 'completed', 'dismissed',
        'pending_remote_create'
    ));

-- Add a partial unique index over follow-up rows in live states, so at
-- most one live follow-up row per (contact_id, provider) exists.
-- Terminal-state rows (completed / dismissed / unmanaged) are unbounded;
-- only one live follow-up is permitted. This unblocks the consumer's
-- two-step cutover from inserting a fresh pending_remote_create row
-- when prior completed/dismissed follow-ups exist.
--
-- Scoped to kind='follow_up' only: action tasks (kind='action') allow
-- multiple live rows per contact/provider (see migration 029), and
-- cadence uniqueness is already enforced by unique_contact_provider_cadence.
--
-- Context: migration 028 originally declared
--   CONSTRAINT unique_contact_provider_kind UNIQUE (contact_id, provider, kind)
-- Migration 029 DROPped that hard constraint. The DROP below is a
-- safety net for environments that never ran 029.
ALTER TABLE contact_task DROP CONSTRAINT IF EXISTS unique_contact_provider_kind;

CREATE UNIQUE INDEX idx_contact_task_followup_unique_live
    ON contact_task (contact_id, provider)
    WHERE kind = 'follow_up'
      AND state IN ('managed', 'pending_remote_create');

-- Local idempotency key. Predicate is on idempotency_key presence only —
-- contact_task has no deleted_at column, so the spec's extra
-- "AND deleted_at IS NULL" clause is not applicable here.
CREATE UNIQUE INDEX idx_contact_task_followup_idempotency
    ON contact_task (contact_id, kind, idempotency_key)
    WHERE idempotency_key IS NOT NULL;
