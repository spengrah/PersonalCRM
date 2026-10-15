BEGIN;

DELETE FROM contact_task WHERE lifecycle = 'obligation';

DROP INDEX idx_contact_task_obligation_open;
ALTER TABLE contact_task DROP CONSTRAINT contact_task_obligation_shape;

ALTER TABLE contact_task DROP CONSTRAINT contact_task_kind_lifecycle_check;
ALTER TABLE contact_task ADD CONSTRAINT contact_task_kind_lifecycle_check
    CHECK (
        (kind = 'reach_out' AND lifecycle IN ('manual', 'cadence_due', 'followup_loop')) OR
        (kind = 'send' AND lifecycle = 'manual') OR
        (kind = 'reminder' AND lifecycle = 'manual') OR
        (kind = 'meet' AND lifecycle = 'manual') OR
        (kind = 'action' AND lifecycle = 'manual')
    );

ALTER TABLE contact_task DROP CONSTRAINT contact_task_lifecycle_check;
ALTER TABLE contact_task ADD CONSTRAINT contact_task_lifecycle_check
    CHECK (lifecycle IN ('manual', 'cadence_due', 'followup_loop'));

ALTER TABLE contact_task
    DROP COLUMN obligation_cut_off,
    DROP COLUMN obligation_basis,
    DROP COLUMN obligation_debtor,
    DROP COLUMN obligation_thread_id;

COMMIT;
