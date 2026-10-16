-- The live effect a judgment outcome stands for.
--
-- judgment_removal_target is written beside judgment_removal when an outcome
-- lands as a removal: the outcome value whose live effect the removal would
-- close, end or move later (i_owe_reply for the user's reply obligation,
-- they_owe_reply for the participant's reply obligation or the timestamp
-- rule's wait, open for a commitment). The bench places the removal under that
-- value's list and offers it as the value that keeps the live item; nothing
-- re-derives it from the thread's obligations, which later correspondence and
-- task completion change.
--
-- judgment_outcome.obligation_task_id is the obligation an outcome's live
-- effect is: the task its effect opened or found when it landed or was
-- reviewed, or for a removal the task it would take away. NULL when that
-- effect is no obligation (a flag, a value with no effect, a wait the
-- timestamp rule shows). The effect holds exactly while that obligation stays
-- open; no other obligation of the same party, whatever judgment, method or
-- review opened it, stands in for it.
--
-- contact_task.obligation_continued_by names the task that continues a closed
-- task's obligation: the commitment task that superseded a reply task, or the
-- task a contact merge kept when two open obligations became one. An
-- obligation stays open while the last task of that chain is live.
--
-- Wrapped in an explicit transaction: the postgres migration driver does NOT
-- auto-wrap a migration file, so multi-statement DDL must self-wrap to stay
-- all-or-nothing on a mid-file failure (Postgres DDL is transactional).

BEGIN;

INSERT INTO predicate (
    key, kind, subject_type, object_type, value_type, cardinality, "symmetric",
    temporal_profile, default_review_policy, proposition_bucket, status,
    description, judgment, judgment_confidence_threshold
) VALUES
    ('judgment_removal_target', 'fact', 'judgment_outcome', NULL, 'text', 'single', FALSE,
     'permanent', 'auto-if-confident', 'none', 'curated',
     'The outcome value whose live effect a judgment''s removal would take away', TRUE, NULL);

ALTER TABLE contact_task
    ADD COLUMN obligation_continued_by UUID REFERENCES contact_task(id) ON DELETE SET NULL,
    ADD CONSTRAINT contact_task_obligation_continued_by
        CHECK (obligation_continued_by IS NULL OR lifecycle = 'obligation');

ALTER TABLE judgment_outcome
    ADD COLUMN obligation_task_id UUID REFERENCES contact_task(id) ON DELETE SET NULL;

COMMIT;
