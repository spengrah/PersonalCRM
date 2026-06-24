-- Predicate catalog (relationship data model — graph foundation).
--
-- The predicate catalog is data, not code: each row declares an edge or fact
-- type the assertion store can reference (subject/object/value typing,
-- cardinality, symmetry, inverse pairing, temporal profile, the review policy,
-- and the valid-time dedup-bucket granularity). The curated-core rows are
-- seeded by the next migration; provisional predicates are minted at runtime.
--
-- Wrapped in an explicit transaction: the postgres migration driver does NOT
-- auto-wrap a migration file, so multi-statement DDL must self-wrap to stay
-- all-or-nothing on a mid-file failure (Postgres DDL is transactional).

BEGIN;

-- predicate: the catalog of edge/fact types. kind splits the payload contract —
-- an 'edge' points at another node (object_type set, value_type NULL); a 'fact'
-- carries a typed scalar (value_type set, object_type NULL). inverse_predicate
-- is a nullable self-FK (e.g. parent_of ↔ child_of), set via a second pass so
-- the pair can be seeded without ordering constraints. embedding is nullable and
-- populated later (map-on-write top-k lookup is over predicate.embedding); the
-- column ships here so the catalog never needs an ALTER to gain it. No vector
-- index in this layer (lookups go live later). default_salience is a 0–100
-- integer scale.
CREATE TABLE predicate (
    key TEXT PRIMARY KEY,
    kind TEXT NOT NULL CHECK (kind IN ('edge', 'fact')),
    subject_type TEXT NOT NULL,
    object_type TEXT,
    value_type TEXT,
    cardinality TEXT NOT NULL CHECK (cardinality IN ('single', 'multi')),
    -- "symmetric" is a SQL reserved word; quote it so the sqlc parser accepts it.
    "symmetric" BOOLEAN NOT NULL DEFAULT FALSE,
    inverse_predicate TEXT REFERENCES predicate(key),
    temporal_profile TEXT NOT NULL CHECK (temporal_profile IN ('permanent', 'mutable', 'bounded')),
    base_rate_days INTEGER,
    typical_duration_days INTEGER,
    default_salience SMALLINT NOT NULL DEFAULT 50 CHECK (default_salience BETWEEN 0 AND 100),
    default_review_policy TEXT NOT NULL CHECK (default_review_policy IN ('auto-if-confident', 'always-confirm')),
    proposition_bucket TEXT NOT NULL DEFAULT 'day' CHECK (proposition_bucket IN ('day', 'month', 'year', 'none')),
    status TEXT NOT NULL CHECK (status IN ('curated', 'provisional')),
    description TEXT NOT NULL DEFAULT '',
    synonyms TEXT[] NOT NULL DEFAULT '{}',
    embedding vector(1536),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    -- Exactly the payload columns the kind allows: edges name an object_type and
    -- carry no value_type; facts name a value_type and carry no object_type.
    CONSTRAINT predicate_kind_payload CHECK (
           (kind = 'edge' AND object_type IS NOT NULL AND value_type IS NULL)
        OR (kind = 'fact' AND value_type IS NOT NULL AND object_type IS NULL)
    ),
    -- value_type, when present, is one of the typed-scalar shapes.
    CONSTRAINT predicate_value_type CHECK (
        value_type IS NULL OR value_type IN ('text', 'num', 'date', 'bool')
    )
);

CREATE INDEX idx_predicate_status ON predicate (status);
CREATE INDEX idx_predicate_kind ON predicate (kind);

COMMIT;
