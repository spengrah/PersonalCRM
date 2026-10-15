BEGIN;

ALTER TABLE predicate
    ADD COLUMN judgment_confidence_threshold SMALLINT,
    ADD COLUMN judgment_addition_values TEXT[] NOT NULL DEFAULT '{}',
    ADD COLUMN judgment_removal_values TEXT[] NOT NULL DEFAULT '{}',
    ADD COLUMN judgment_no_effect_values TEXT[] NOT NULL DEFAULT '{}',
    ADD CONSTRAINT predicate_judgment_threshold CHECK (
        judgment_confidence_threshold IS NULL OR judgment_confidence_threshold BETWEEN 0 AND 100
    ),
    ADD CONSTRAINT predicate_judgment_policy CHECK (
        judgment_confidence_threshold IS NOT NULL
        OR (cardinality(judgment_addition_values) = 0
            AND cardinality(judgment_removal_values) = 0
            AND cardinality(judgment_no_effect_values) = 0)
    );

COMMIT;
