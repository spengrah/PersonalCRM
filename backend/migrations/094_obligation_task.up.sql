-- Obligation tasks: the task lifecycle of a judgment's obligations.
--
-- An obligation is who owes whom what on one thread, keyed by the thread and
-- the owing party (the user, or the participant contact the task belongs to).
-- It has no table of its own: an obligation is its one task of lifecycle
-- 'obligation', and it is open exactly while that task's state is managed or
-- pending_remote_create, so any act that ends the task's liveness closes the
-- obligation in the same write. The task's kind is what completing it records:
-- reach_out for the user's reply and for anything a participant owes, reminder
-- for a commitment the user owes. obligation_cut_off is the cut-off of the
-- judgment that opened it, after which a new message closes a reply obligation.
--
-- Wrapped in an explicit transaction: the postgres migration driver does NOT
-- auto-wrap a migration file, so multi-statement DDL must self-wrap to stay
-- all-or-nothing on a mid-file failure (Postgres DDL is transactional).

BEGIN;

ALTER TABLE contact_task
    ADD COLUMN obligation_thread_id UUID REFERENCES node(id),
    ADD COLUMN obligation_debtor TEXT CHECK (obligation_debtor IN ('user', 'contact')),
    ADD COLUMN obligation_basis TEXT CHECK (obligation_basis IN ('reply', 'commitment')),
    ADD COLUMN obligation_cut_off TIMESTAMPTZ;

ALTER TABLE contact_task DROP CONSTRAINT contact_task_lifecycle_check;
ALTER TABLE contact_task ADD CONSTRAINT contact_task_lifecycle_check
    CHECK (lifecycle IN ('manual', 'cadence_due', 'followup_loop', 'obligation'));

ALTER TABLE contact_task DROP CONSTRAINT contact_task_kind_lifecycle_check;
ALTER TABLE contact_task ADD CONSTRAINT contact_task_kind_lifecycle_check
    CHECK (
        (kind = 'reach_out' AND lifecycle IN ('manual', 'cadence_due', 'followup_loop', 'obligation')) OR
        (kind = 'send' AND lifecycle = 'manual') OR
        (kind = 'reminder' AND lifecycle IN ('manual', 'obligation')) OR
        (kind = 'meet' AND lifecycle = 'manual') OR
        (kind = 'action' AND lifecycle = 'manual')
    );

-- The obligation columns are set exactly on an obligation task, and its kind
-- follows from who owes what.
ALTER TABLE contact_task ADD CONSTRAINT contact_task_obligation_shape
    CHECK (
        (lifecycle <> 'obligation'
            AND obligation_thread_id IS NULL AND obligation_debtor IS NULL
            AND obligation_basis IS NULL AND obligation_cut_off IS NULL)
        OR (lifecycle = 'obligation'
            AND obligation_thread_id IS NOT NULL AND obligation_cut_off IS NOT NULL
            AND ((obligation_debtor = 'user' AND obligation_basis = 'reply' AND kind = 'reach_out')
              OR (obligation_debtor = 'user' AND obligation_basis = 'commitment' AND kind = 'reminder')
              OR (obligation_debtor = 'contact' AND obligation_basis IS NOT NULL AND kind = 'reach_out')))
    );

-- At most one open obligation per thread and owing party. A participant's
-- obligation is keyed by its contact; the user's by the thread alone.
CREATE UNIQUE INDEX idx_contact_task_obligation_open
    ON contact_task (
        obligation_thread_id,
        obligation_debtor,
        (CASE WHEN obligation_debtor = 'contact' THEN contact_id END)
    ) NULLS NOT DISTINCT
    WHERE lifecycle = 'obligation' AND state IN ('managed', 'pending_remote_create');

COMMIT;
