BEGIN;

DELETE FROM assertion_provenance WHERE assertion_id IN (
    SELECT id FROM assertion WHERE predicate_key IN (
        'judgment_answer', 'judgment_addition', 'judgment_removal',
        'judgment_no_effect', 'judgment_review', 'big_moment_flag'));
UPDATE assertion SET superseded_by = NULL WHERE predicate_key IN (
    'judgment_answer', 'judgment_addition', 'judgment_removal',
    'judgment_no_effect', 'judgment_review', 'big_moment_flag');
DELETE FROM assertion WHERE predicate_key IN (
    'judgment_answer', 'judgment_addition', 'judgment_removal',
    'judgment_no_effect', 'judgment_review', 'big_moment_flag');
DELETE FROM predicate WHERE key IN (
    'judgment_answer', 'judgment_addition', 'judgment_removal',
    'judgment_no_effect', 'judgment_review', 'big_moment_flag');

ALTER TABLE predicate
    DROP CONSTRAINT predicate_judgment_threshold,
    DROP COLUMN judgment_confidence_threshold,
    DROP COLUMN judgment;

COMMIT;
