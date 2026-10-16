-- The live value a judgment's removal takes away.
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

COMMIT;
