BEGIN;

DROP VIEW effective_participation;
DROP VIEW interaction_observation_verdict;
DROP FUNCTION interaction_alias_tree(UUID);
DROP FUNCTION interaction_node_root(UUID);
DROP INDEX idx_interaction_interaction_node;

-- Correction assertions reference their predicates (restrict FK); provenance
-- cascades with its assertion.
DELETE FROM assertion
WHERE predicate_key IN ('not_participant_in', 'removed_for_everyone', 'same_interaction_as');
DELETE FROM predicate
WHERE key IN ('not_participant_in', 'removed_for_everyone', 'same_interaction_as');

ALTER TABLE predicate DROP COLUMN correction;

COMMIT;
