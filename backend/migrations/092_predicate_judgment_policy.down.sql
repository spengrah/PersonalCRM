BEGIN;

ALTER TABLE predicate
    DROP CONSTRAINT predicate_judgment_policy,
    DROP CONSTRAINT predicate_judgment_threshold,
    DROP COLUMN judgment_no_effect_values,
    DROP COLUMN judgment_removal_values,
    DROP COLUMN judgment_addition_values,
    DROP COLUMN judgment_confidence_threshold;

COMMIT;
