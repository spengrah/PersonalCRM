-- Migration 046: split contact_task.kind into orthogonal kind/lifecycle axes.
--
-- Pre-migration: contact_task.kind held a single label conflating direction
-- (cadence/follow_up/action) with origin/dispatch. Post-migration:
--   kind       — semantic completion behaviour (reach_out / send / reminder /
--                meet [legacy] / action [legacy])
--   lifecycle  — origin / scheduling / dismissal rules
--                (manual / cadence_due / followup_loop)
--
-- Order of operations:
--   1. Add lifecycle column with DEFAULT 'manual' (permanent — also acts as
--      a floor for any insert path that forgets to set it; producer-side
--      validation in the Go service layer is the primary defence).
--   2. Backfill kind/lifecycle from existing kind values.
--   3. Drop + recreate the three partial unique indexes that previously
--      keyed on kind, now keyed on lifecycle.
--   4. Add three new CHECK constraints (kind, lifecycle, composite). Adding
--      these AFTER backfill keeps the migration ordered for partial-failure
--      recovery and avoids ACCESS EXCLUSIVE / row lock contention.

-- 1. Add column.
ALTER TABLE contact_task ADD COLUMN lifecycle TEXT NOT NULL DEFAULT 'manual';

-- 2. Backfill (kind, lifecycle) pairs from the legacy kind column.
UPDATE contact_task
SET lifecycle = 'cadence_due', kind = 'reach_out'
WHERE kind = 'cadence';

UPDATE contact_task
SET lifecycle = 'followup_loop', kind = 'reach_out'
WHERE kind = 'follow_up';

-- kind = 'action' rows: kind unchanged; lifecycle defaults to 'manual'.
-- kind = 'meet' rows: none expected (no creator path); kind unchanged;
-- lifecycle defaults to 'manual'.

-- 3. Rewrite partial unique indexes lifecycle-keyed.
DROP INDEX IF EXISTS unique_contact_provider_cadence;
CREATE UNIQUE INDEX unique_contact_provider_cadence
    ON contact_task (contact_id, provider)
    WHERE lifecycle = 'cadence_due';

-- Index name PRESERVED — followup_manager.go pattern-matches on
-- pgErr.ConstraintName == "idx_contact_task_followup_unique_live" for
-- unique-violation recovery.
DROP INDEX IF EXISTS idx_contact_task_followup_unique_live;
CREATE UNIQUE INDEX idx_contact_task_followup_unique_live
    ON contact_task (contact_id, provider)
    WHERE lifecycle = 'followup_loop'
      AND state IN ('managed', 'pending_remote_create');

-- The post-migration predicate constrains rows to followup_loop only, so
-- kind no longer needs to participate in the indexed columns.
DROP INDEX IF EXISTS idx_contact_task_followup_idempotency;
CREATE UNIQUE INDEX idx_contact_task_followup_idempotency
    ON contact_task (contact_id, idempotency_key)
    WHERE lifecycle = 'followup_loop' AND idempotency_key IS NOT NULL;

-- 4. Add CHECK constraints. The pre-existing contact_task_state_check is
-- left untouched (already covers managed/unmanaged/completed/dismissed/
-- pending_remote_create per migration 041).
ALTER TABLE contact_task ADD CONSTRAINT contact_task_kind_check
    CHECK (kind IN ('reach_out', 'send', 'reminder', 'meet', 'action'));

ALTER TABLE contact_task ADD CONSTRAINT contact_task_lifecycle_check
    CHECK (lifecycle IN ('manual', 'cadence_due', 'followup_loop'));

-- Composite: only kind=reach_out participates in all three lifecycles.
-- Every other kind is only valid with lifecycle=manual.
ALTER TABLE contact_task ADD CONSTRAINT contact_task_kind_lifecycle_check
    CHECK (
        (kind = 'reach_out' AND lifecycle IN ('manual', 'cadence_due', 'followup_loop')) OR
        (kind = 'send' AND lifecycle = 'manual') OR
        (kind = 'reminder' AND lifecycle = 'manual') OR
        (kind = 'meet' AND lifecycle = 'manual') OR
        (kind = 'action' AND lifecycle = 'manual')
    );
