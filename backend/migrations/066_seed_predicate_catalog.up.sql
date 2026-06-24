-- Seed the curated-core predicate catalog + entity subtypes.
--
-- Wrapped in an explicit transaction: this is multi-statement DML (the postgres
-- migration driver does NOT auto-wrap a file), so it self-wraps to stay
-- all-or-nothing. Inverse pairs are seeded in TWO phases — insert every row with
-- inverse_predicate NULL, then UPDATE the links — so the self-FK never sees a
-- referenced key that hasn't been inserted yet, regardless of row order.

BEGIN;

-- Entity subtypes (the curated-core entity_type catalog). place carries the
-- hierarchical / synonym-normalization resolution config the location rollup
-- needs; the rest default to '{}'. ON CONFLICT keeps the seed idempotent.
INSERT INTO entity_type (key, description, resolution_config, status) VALUES
    ('organization', 'Companies, schools, teams, and other organizations', '{}', 'curated'),
    ('place',        'Geographic places (cities, regions, countries) with hierarchical rollup', '{"hierarchical": true, "normalize_synonyms": true}', 'curated'),
    ('topic',        'Topics and interests a person engages with', '{}', 'curated'),
    ('tag',          'User-defined tags applied to contacts', '{}', 'curated')
ON CONFLICT (key) DO NOTHING;

-- Curated-core predicates. Phase 1: insert every row with inverse_predicate NULL
-- (the inverse links are set in phase 2 below). embedding stays NULL (populated
-- later). Columns omitted from a row fall to their table default (symmetric
-- FALSE, default_salience 50, proposition_bucket 'day', synonyms '{}').
INSERT INTO predicate (
    key, kind, subject_type, object_type, value_type, cardinality, "symmetric",
    temporal_profile, base_rate_days, typical_duration_days,
    default_salience, default_review_policy, proposition_bucket, status, description
) VALUES
    ('lives_in',         'edge', 'person', 'place',        NULL,   'single', FALSE, 'mutable',   2190, NULL, 60, 'auto-if-confident', 'year', 'curated', 'Person currently lives in a place'),
    ('home_address',     'fact', 'person', NULL,           'text', 'single', FALSE, 'mutable',   2190, NULL, 45, 'auto-if-confident', 'year', 'curated', 'Person''s home address'),
    ('works_at',         'edge', 'person', 'organization', NULL,   'single', FALSE, 'mutable',   1460, NULL, 60, 'auto-if-confident', 'year', 'curated', 'Person''s current primary employer'),
    ('job_title',        'fact', 'person', NULL,           'text', 'single', FALSE, 'mutable',   1095, NULL, 45, 'auto-if-confident', 'year', 'curated', 'Person''s job title'),
    ('birthday',         'fact', 'person', NULL,           'date', 'single', FALSE, 'permanent', NULL, NULL, 85, 'auto-if-confident', 'none', 'curated', 'Person''s birthday'),
    ('partner_of',       'edge', 'person', 'person',       NULL,   'single', TRUE,  'mutable',   NULL, NULL, 85, 'always-confirm',    'none', 'curated', 'Romantic partner (one current partner per person)'),
    ('parent_of',        'edge', 'person', 'person',       NULL,   'multi',  FALSE, 'permanent', NULL, NULL, 80, 'always-confirm',    'none', 'curated', 'Person is a parent of another person'),
    ('child_of',         'edge', 'person', 'person',       NULL,   'multi',  FALSE, 'permanent', NULL, NULL, 80, 'always-confirm',    'none', 'curated', 'Person is a child of another person'),
    ('sibling_of',       'edge', 'person', 'person',       NULL,   'multi',  TRUE,  'permanent', NULL, NULL, 80, 'always-confirm',    'none', 'curated', 'Person is a sibling of another person'),
    ('grandparent_of',   'edge', 'person', 'person',       NULL,   'multi',  FALSE, 'permanent', NULL, NULL, 55, 'auto-if-confident', 'none', 'curated', 'Person is a grandparent of another person'),
    ('grandchild_of',    'edge', 'person', 'person',       NULL,   'multi',  FALSE, 'permanent', NULL, NULL, 55, 'auto-if-confident', 'none', 'curated', 'Person is a grandchild of another person'),
    ('aunt_uncle_of',    'edge', 'person', 'person',       NULL,   'multi',  FALSE, 'permanent', NULL, NULL, 55, 'auto-if-confident', 'none', 'curated', 'Person is an aunt or uncle of another person'),
    ('niece_nephew_of',  'edge', 'person', 'person',       NULL,   'multi',  FALSE, 'permanent', NULL, NULL, 55, 'auto-if-confident', 'none', 'curated', 'Person is a niece or nephew of another person'),
    ('cousin_of',        'edge', 'person', 'person',       NULL,   'multi',  TRUE,  'permanent', NULL, NULL, 55, 'auto-if-confident', 'none', 'curated', 'Person is a cousin of another person'),
    ('health_condition', 'fact', 'person', NULL,           'text', 'multi',  FALSE, 'mutable',   NULL, NULL, 80, 'always-confirm',    'none', 'curated', 'A health condition affecting the person'),
    ('interested_in',    'edge', 'person', 'topic',        NULL,   'multi',  FALSE, 'mutable',   3650, NULL, 45, 'auto-if-confident', 'none', 'curated', 'Person is interested in a topic'),
    ('preference',       'fact', 'person', NULL,           'text', 'multi',  FALSE, 'mutable',   NULL, NULL, 35, 'auto-if-confident', 'none', 'curated', 'A preference the person holds'),
    ('how_met',          'fact', 'person', NULL,           'text', 'single', FALSE, 'permanent', NULL, NULL, 60, 'auto-if-confident', 'none', 'curated', 'How the user met this person'),
    ('tagged_as',        'edge', 'person', 'tag',          NULL,   'multi',  FALSE, 'permanent', NULL, NULL, 50, 'auto-if-confident', 'none', 'curated', 'Person is tagged with a user-defined tag'),
    ('knows',            'edge', 'person', 'person',       NULL,   'multi',  TRUE,  'mutable',   3650, NULL, 55, 'auto-if-confident', 'none', 'curated', 'Person knows another person'),
    ('introduced_by',    'edge', 'person', 'person',       NULL,   'single', FALSE, 'permanent', NULL, NULL, 55, 'auto-if-confident', 'none', 'curated', 'Person was introduced by another person'),
    ('job_seeking',      'fact', 'person', NULL,           'bool', 'single', FALSE, 'bounded',   NULL, 180,  60, 'auto-if-confident', 'day',  'curated', 'Person is currently seeking a job'),
    ('on_sabbatical',    'fact', 'person', NULL,           'bool', 'single', FALSE, 'bounded',   NULL, 180,  55, 'auto-if-confident', 'day',  'curated', 'Person is currently on sabbatical'),
    ('traveling',        'fact', 'person', NULL,           'bool', 'single', FALSE, 'bounded',   NULL, 30,   50, 'auto-if-confident', 'day',  'curated', 'Person is currently traveling'),
    ('occurrence',       'fact', 'person', NULL,           'text', 'multi',  FALSE, 'bounded',   NULL, 7,    80, 'always-confirm',    'day',  'curated', 'A dated occurrence in the person''s life (date carried in valid_from)'),
    ('within',           'edge', 'place',  'place',        NULL,   'single', FALSE, 'permanent', NULL, NULL, 30, 'auto-if-confident', 'none', 'curated', 'A place is contained within a parent place (hierarchy)')
ON CONFLICT (key) DO NOTHING;

-- Phase 2: link the inverse pairs. Each direction points at the other; both keys
-- exist after phase 1, so neither UPDATE can dangle.
UPDATE predicate SET inverse_predicate = 'child_of'        WHERE key = 'parent_of';
UPDATE predicate SET inverse_predicate = 'parent_of'       WHERE key = 'child_of';
UPDATE predicate SET inverse_predicate = 'grandchild_of'   WHERE key = 'grandparent_of';
UPDATE predicate SET inverse_predicate = 'grandparent_of'  WHERE key = 'grandchild_of';
UPDATE predicate SET inverse_predicate = 'niece_nephew_of' WHERE key = 'aunt_uncle_of';
UPDATE predicate SET inverse_predicate = 'aunt_uncle_of'   WHERE key = 'niece_nephew_of';

COMMIT;
