-- 051_messaging_aggregate_unique_states.down.sql
-- Restoring River's default mask reintroduces completed-state uniqueness.
-- Once the forward migration has allowed a fresh aggregate job after an older
-- completed one, rolling back may be impossible without violating River's
-- unique index. Refuse instead of silently corrupting queue semantics.
DO $$
BEGIN
    IF EXISTS (
        SELECT 1
        FROM river_job
        WHERE kind = 'messaging_aggregate_for_contact'
          AND unique_key IS NOT NULL
          AND state IN ('available', 'completed', 'pending', 'retryable', 'running', 'scheduled')
        GROUP BY unique_key
        HAVING count(*) > 1
    ) THEN
        RAISE EXCEPTION 'cannot restore completed-state uniqueness for messaging aggregate jobs: duplicate active-or-completed unique keys exist';
    END IF;
END $$;

-- River's v0.34 default unique-state mask:
-- available + completed + pending + retryable + running + scheduled = B'11110101'.
UPDATE river_job
SET unique_states = B'11110101'
WHERE kind = 'messaging_aggregate_for_contact'
  AND unique_states IS NOT NULL
  AND unique_states <> B'11110101';
