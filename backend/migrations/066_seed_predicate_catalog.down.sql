-- Remove ONLY the seeded curated-core rows (by key), never TRUNCATE — a re-run
-- or a partial state must leave any non-seeded (provisional) rows intact. Clear
-- the inverse_predicate self-FK links first so the subsequent DELETE can't trip
-- the self-reference, then delete the predicate rows, then the entity subtypes.

BEGIN;

UPDATE predicate SET inverse_predicate = NULL
WHERE key IN (
    'parent_of', 'child_of',
    'grandparent_of', 'grandchild_of',
    'aunt_uncle_of', 'niece_nephew_of'
);

DELETE FROM predicate WHERE key IN (
    'lives_in', 'home_address', 'works_at', 'job_title', 'birthday',
    'partner_of', 'parent_of', 'child_of', 'sibling_of',
    'grandparent_of', 'grandchild_of', 'aunt_uncle_of', 'niece_nephew_of', 'cousin_of',
    'health_condition', 'interested_in', 'preference', 'how_met', 'tagged_as',
    'knows', 'introduced_by',
    'job_seeking', 'on_sabbatical', 'traveling', 'occurrence', 'within'
);

DELETE FROM entity_type WHERE key IN ('organization', 'place', 'topic', 'tag');

COMMIT;
