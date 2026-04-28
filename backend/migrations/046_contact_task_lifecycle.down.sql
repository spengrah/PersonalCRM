-- Migration 046 down: revert kind/lifecycle split.
--
-- Aborts loudly when the database contains rows that have no representation
-- in the pre-migration schema. Specifically:
--   1. Any (reach_out|send|reminder, manual) rows — these user-creatable
--      kinds did not exist pre-046.
--   2. Duplicate (contact_id, provider) within lifecycle='cadence_due'.
--   3. Duplicate (contact_id, provider) within lifecycle='followup_loop'
--      live states.
--
-- Cases (2) and (3) are unreachable under normal operation because the
-- post-migration partial unique indexes prevent them at insert time, but
-- the preconditions act as a safety net if those indexes are ever dropped
-- or corrupted.

-- Precondition 1: user-created rows that have no pre-046 representation.
DO $$
DECLARE
    blocking_count integer;
BEGIN
    SELECT COUNT(*) INTO blocking_count
    FROM contact_task
    WHERE lifecycle = 'manual'
      AND kind IN ('reach_out', 'send', 'reminder');
    IF blocking_count > 0 THEN
        RAISE EXCEPTION 'down-migration aborted: % user-created task rows have no representation in the old schema', blocking_count;
    END IF;
END $$;

-- Precondition 2: duplicate cadence_due (contact_id, provider).
DO $$
DECLARE
    blocking_count integer;
BEGIN
    SELECT COUNT(*) INTO blocking_count FROM (
        SELECT contact_id, provider
        FROM contact_task
        WHERE lifecycle = 'cadence_due'
        GROUP BY contact_id, provider
        HAVING COUNT(*) > 1
    ) dup;
    IF blocking_count > 0 THEN
        RAISE EXCEPTION 'down-migration aborted: % duplicate (contact_id, provider) groups exist within lifecycle=''cadence_due''; would violate pre-046 unique_contact_provider_cadence index', blocking_count;
    END IF;
END $$;

-- Precondition 3: duplicate followup_loop live (contact_id, provider).
DO $$
DECLARE
    blocking_count integer;
BEGIN
    SELECT COUNT(*) INTO blocking_count FROM (
        SELECT contact_id, provider
        FROM contact_task
        WHERE lifecycle = 'followup_loop'
          AND state IN ('managed', 'pending_remote_create')
        GROUP BY contact_id, provider
        HAVING COUNT(*) > 1
    ) dup;
    IF blocking_count > 0 THEN
        RAISE EXCEPTION 'down-migration aborted: % duplicate (contact_id, provider) groups exist within lifecycle=''followup_loop'' live states; would violate pre-046 idx_contact_task_followup_unique_live', blocking_count;
    END IF;
END $$;

-- Drop the new CHECK constraints.
ALTER TABLE contact_task DROP CONSTRAINT IF EXISTS contact_task_kind_lifecycle_check;
ALTER TABLE contact_task DROP CONSTRAINT IF EXISTS contact_task_lifecycle_check;
ALTER TABLE contact_task DROP CONSTRAINT IF EXISTS contact_task_kind_check;

-- Drop the new partial unique indexes.
DROP INDEX IF EXISTS unique_contact_provider_cadence;
DROP INDEX IF EXISTS idx_contact_task_followup_unique_live;
DROP INDEX IF EXISTS idx_contact_task_followup_idempotency;

-- Inverse backfill of (kind, lifecycle) pairs to legacy kind values.
UPDATE contact_task SET kind = 'cadence'
WHERE kind = 'reach_out' AND lifecycle = 'cadence_due';

UPDATE contact_task SET kind = 'follow_up'
WHERE kind = 'reach_out' AND lifecycle = 'followup_loop';

-- (action, manual) stays as 'action'; (meet, manual) stays as 'meet'.

-- Re-create the three indexes with their pre-046 kind-keyed predicates.
CREATE UNIQUE INDEX unique_contact_provider_cadence
    ON contact_task (contact_id, provider, kind)
    WHERE kind = 'cadence';

CREATE UNIQUE INDEX idx_contact_task_followup_unique_live
    ON contact_task (contact_id, provider)
    WHERE kind = 'follow_up'
      AND state IN ('managed', 'pending_remote_create');

CREATE UNIQUE INDEX idx_contact_task_followup_idempotency
    ON contact_task (contact_id, kind, idempotency_key)
    WHERE idempotency_key IS NOT NULL;

-- Drop the lifecycle column.
ALTER TABLE contact_task DROP COLUMN lifecycle;
