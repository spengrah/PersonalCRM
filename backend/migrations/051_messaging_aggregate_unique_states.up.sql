-- 051_messaging_aggregate_unique_states.up.sql
-- River stores each unique job's state mask on the row itself. Aggregate
-- jobs created before the active-state uniqueness fix still carry River's
-- default mask (which includes completed jobs), so those legacy completed
-- rows can continue blocking fresh aggregate work after deploy.
--
-- Match the application-level MessagingAggregateUniqueOpts helper:
-- available + pending + retryable + running + scheduled = B'11110001'.
--
-- river_job is created programmatically by river.Client at app boot rather
-- than via this migrator, so on a fresh DB the table does not exist when
-- this runs. Guard the UPDATE so the migration is a no-op when there is
-- nothing to retrofit.
DO $$
BEGIN
    IF to_regclass('public.river_job') IS NOT NULL THEN
        UPDATE river_job
        SET unique_states = B'11110001'
        WHERE kind = 'messaging_aggregate_for_contact'
          AND unique_states IS NOT NULL
          AND unique_states <> B'11110001';
    END IF;
END $$;
