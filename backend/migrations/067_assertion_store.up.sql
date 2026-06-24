-- Assertion store (relationship data model — graph foundation).
--
-- The assertion is the bi-temporal unit of the graph: one row asserts a fact
-- about (or an edge from) a subject node, under a catalog predicate. valid-time
-- (valid_from/valid_to) is when the statement is true in the world; knowledge-
-- time (knowledge_from/knowledge_to) is when WE learned it and when that belief
-- was closed. proposition_key is the write-API-computed dedup key (a plain
-- partial-unique index over it enforces "at most one LIVE assertion per
-- proposition"). assertion_provenance carries the corroborating source locators.
--
-- The write LOGIC (supersession, dedup, advisory locking) lives in the assert()
-- service (a later layer); this migration ships the SCHEMA + the constraints the
-- service relies on as its last line of defense.
--
-- Wrapped in an explicit transaction: the postgres migration driver does NOT
-- auto-wrap a migration file, so multi-statement DDL must self-wrap to stay
-- all-or-nothing on a mid-file failure (Postgres DDL is transactional).

BEGIN;

-- assertion: the bi-temporal fact/edge row. Exactly one payload is set (the
-- one-payload CHECK): object_node_id for an edge, or one of the typed scalars
-- for a fact. The FKs to node/predicate are restrict (NO ACTION) — we never
-- hard-delete nodes or predicates in normal operation, so a hard-delete attempt
-- should fail loudly rather than orphan history. superseded_by is a nullable
-- self-FK made DEFERRABLE INITIALLY DEFERRED so the write API can order
-- insert-new / close-prior either way within one tx. proposition_key is a plain
-- TEXT column the write API computes (it cannot be an expression index —
-- date_trunc/casts of timestamptz are not IMMUTABLE).
CREATE TABLE assertion (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    subject_node_id UUID NOT NULL REFERENCES node(id),
    predicate_key TEXT NOT NULL REFERENCES predicate(key),
    object_node_id UUID REFERENCES node(id),
    value_text TEXT,
    value_num DOUBLE PRECISION,
    value_date DATE,
    value_bool BOOLEAN,
    valid_from TIMESTAMPTZ,
    valid_to TIMESTAMPTZ,
    knowledge_from TIMESTAMPTZ NOT NULL,
    knowledge_to TIMESTAMPTZ,
    confidence SMALLINT NOT NULL CHECK (confidence BETWEEN 0 AND 100),
    salience SMALLINT NOT NULL CHECK (salience BETWEEN 0 AND 100),
    status TEXT NOT NULL CHECK (status IN ('proposed', 'accepted', 'rejected', 'superseded', 'retracted')),
    closure_reason TEXT,
    superseded_by UUID REFERENCES assertion(id) DEFERRABLE INITIALLY DEFERRED,
    trust_tier TEXT,
    proposition_key TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    -- Exactly one of {object_node_id, value_text, value_num, value_date,
    -- value_bool} is non-null. (object-present-iff-edge is a cross-table
    -- invariant enforced by the write API, not here.)
    CONSTRAINT assertion_one_payload CHECK (
        (CASE WHEN object_node_id IS NOT NULL THEN 1 ELSE 0 END)
      + (CASE WHEN value_text     IS NOT NULL THEN 1 ELSE 0 END)
      + (CASE WHEN value_num      IS NOT NULL THEN 1 ELSE 0 END)
      + (CASE WHEN value_date     IS NOT NULL THEN 1 ELSE 0 END)
      + (CASE WHEN value_bool     IS NOT NULL THEN 1 ELSE 0 END)
      = 1
    ),
    -- knowledge_to is set iff the status is terminal (the belief is closed).
    CONSTRAINT assertion_terminal_knowledge_to CHECK (
        (status IN ('rejected', 'superseded', 'retracted') AND knowledge_to IS NOT NULL)
     OR (status IN ('proposed', 'accepted')                AND knowledge_to IS NULL)
    ),
    -- Bi-temporal ordering: a NULL bound is open-ended; when both bounds are
    -- present the end must be strictly after the start.
    CONSTRAINT assertion_valid_range CHECK (
        valid_from IS NULL OR valid_to IS NULL OR valid_to > valid_from
    ),
    CONSTRAINT assertion_knowledge_range CHECK (
        knowledge_to IS NULL OR knowledge_to >= knowledge_from
    ),
    -- value_num must be a real finite number. PostgreSQL is NON-IEEE for NaN:
    -- 'NaN'=NaN is TRUE and 'NaN'<>NaN is FALSE, so a `value_num = value_num`
    -- guard does NOT reject NaN. The correct idiom is `value_num <> 'NaN'`
    -- (FALSE for NaN → CHECK fails → rejected; TRUE for any real → passes).
    CONSTRAINT assertion_value_num_finite CHECK (
        value_num IS NULL OR (
            value_num <> 'NaN'::float8
        AND value_num <> 'Infinity'::float8
        AND value_num <> '-Infinity'::float8
        )
    )
);

CREATE INDEX idx_assertion_subject_pred ON assertion (subject_node_id, predicate_key);
CREATE INDEX idx_assertion_object ON assertion (object_node_id) WHERE object_node_id IS NOT NULL;
CREATE INDEX idx_assertion_status ON assertion (status);
-- The proposition-identity index: a PLAIN partial-unique over the stored
-- proposition_key column (NOT an expression index). Enforces "at most one LIVE
-- assertion per proposition"; a concurrent-writer collision surfaces as 23505,
-- which the write path recovers via a savepoint.
CREATE UNIQUE INDEX idx_assertion_live_proposition
    ON assertion (proposition_key)
    WHERE status IN ('proposed', 'accepted') AND knowledge_to IS NULL;

-- assertion_provenance: the corroborating source locators for an assertion. The
-- PK is (assertion_id, locator_hash) — NOT (assertion_id, source_kind,
-- source_id) — because the FULL locator identity (source + field + span/chunk +
-- producer + version + input_hash) distinguishes two quotes from different spans
-- of the same message, or a re-assertion at a different extractor version. The
-- write API computes locator_hash from the length-prefixed encoding of that
-- tuple. source_id is TEXT (not UUID) and NO FK: locators are polymorphic
-- (content rows are UUIDs, but user/agent_session refs are not) and a later
-- hard-deleted source row degrades gracefully (quote + input_hash preserve
-- display). ON DELETE CASCADE provenance→assertion: provenance has no meaning
-- without its assertion.
CREATE TABLE assertion_provenance (
    assertion_id UUID NOT NULL REFERENCES assertion(id) ON DELETE CASCADE,
    locator_hash TEXT NOT NULL,
    source_kind TEXT NOT NULL CHECK (source_kind IN (
        'comms_message', 'telegram_message', 'messages_message', 'meeting_note',
        'anarlog_transcript', 'calendar_event', 'phone_call', 'user', 'agent_session'
    )),
    source_id TEXT NOT NULL,
    producer_kind TEXT NOT NULL CHECK (producer_kind IN ('extractor', 'agent', 'user')),
    producer_version TEXT NOT NULL DEFAULT '',
    field TEXT,
    start_offset INTEGER,
    end_offset INTEGER,
    chunk_id TEXT,
    input_hash TEXT NOT NULL DEFAULT '',
    quote TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (assertion_id, locator_hash)
);

-- Reverse lookup ("what did this source produce") + the source-row-deletion
-- sweep.
CREATE INDEX idx_provenance_source ON assertion_provenance (source_kind, source_id);

COMMIT;
