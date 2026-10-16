BEGIN;

DELETE FROM assertion_provenance WHERE assertion_id IN (
    SELECT id FROM assertion WHERE predicate_key = 'judgment_removal_target');
UPDATE assertion SET superseded_by = NULL WHERE predicate_key = 'judgment_removal_target';
DELETE FROM assertion WHERE predicate_key = 'judgment_removal_target';
DELETE FROM predicate WHERE key = 'judgment_removal_target';

COMMIT;
