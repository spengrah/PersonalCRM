-- Derived storage scaffolding (graph foundation).
--
-- Two disposable projection tables that SP3 fills with computed artifacts:
--
--   embedding             — vector(1536) embeddings keyed by a polymorphic
--                           (target_kind, target_id) + model_version. No FK:
--                           the kind names the source table and embeddings are
--                           rebuildable, so a hard FK to seven tables is neither
--                           possible nor wanted (matches the provenance posture).
--   relationship_signal   — per-node scalar signals (closeness, real cadence,
--                           trend, …) keyed by (subject_node_id, signal_key).
--                           subject_node_id IS a real FK→node: signals are
--                           per-node and node is the stable anchor.
--
-- Both carry NO envelope, NO deleted_at — they are wiped and rebuilt, and
-- as_of/computed_at/method_version are the only metadata. NO vector index in
-- SP1: there are no generators yet, so HNSW/IVFFlat is deferred to SP3 when
-- queries go live.
--
-- Wrapped in an explicit transaction: the postgres migration driver does NOT
-- auto-wrap a migration file, so this multi-statement DDL self-wraps to stay
-- all-or-nothing on a mid-file failure (Postgres DDL is transactional).

BEGIN;

CREATE TABLE embedding (
    target_kind TEXT NOT NULL CHECK (target_kind IN (
        'node', 'assertion', 'interaction', 'meeting_note',
        'comms_message', 'telegram_message', 'messages_message')),
    target_id UUID NOT NULL,
    model_version TEXT NOT NULL,
    vector vector(1536) NOT NULL,
    computed_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (target_kind, target_id, model_version)
);

CREATE TABLE relationship_signal (
    subject_node_id UUID NOT NULL REFERENCES node(id),
    signal_key TEXT NOT NULL,
    value DOUBLE PRECISION NOT NULL,
    computed_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    as_of TIMESTAMPTZ NOT NULL,
    method_version TEXT NOT NULL,
    PRIMARY KEY (subject_node_id, signal_key)
);

COMMIT;
