-- Judgment store: decision-model judgments of thread contexts.
--
-- A judgment is one method's answers to one question tree for one thread at one
-- cut-off. This migration holds the judgment records the assertion store does
-- not: runs, the judgment record itself (method, input, usage and the raw
-- answered questions), each run's appearances of a judgment, the threads a run
-- could not judge, and the opaque example IDs. What a method chose, whether an
-- outcome is a claim, and the user's reviews are assertions (the predicates of
-- migration 093) whose subject is the outcome's node, of the node type added
-- here.
--
-- Wrapped in an explicit transaction: the postgres migration driver does NOT
-- auto-wrap a migration file, so multi-statement DDL must self-wrap to stay
-- all-or-nothing on a mid-file failure (Postgres DDL is transactional).

BEGIN;

ALTER TABLE node DROP CONSTRAINT node_type_check;
ALTER TABLE node ADD CONSTRAINT node_type_check
    CHECK (type IN ('person', 'venue', 'entity', 'interaction', 'judgment_outcome'));

-- A model answer's provenance names the judgment record that holds its input
-- hash, usage and raw answers.
ALTER TABLE assertion_provenance DROP CONSTRAINT assertion_provenance_source_kind_check;
ALTER TABLE assertion_provenance ADD CONSTRAINT assertion_provenance_source_kind_check
    CHECK (source_kind IN (
        'comms_message', 'telegram_message', 'messages_message', 'meeting_note',
        'anarlog_transcript', 'calendar_event', 'phone_call', 'user', 'agent_session',
        'judgment'
    ));

-- judgment_run: one run of one method version over a set of threads.
CREATE TABLE judgment_run (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    kind TEXT NOT NULL CHECK (kind IN ('scheduled', 'historical', 'rerun')),
    method_key TEXT NOT NULL,
    method_version TEXT NOT NULL,
    wording_version TEXT NOT NULL,
    window_from TIMESTAMPTZ,
    window_to TIMESTAMPTZ,
    started_at TIMESTAMPTZ NOT NULL,
    finished_at TIMESTAMPTZ
);

-- judgment_example: the one issuer of example IDs, one per thread and input
-- hash. The ID is random, so it carries neither a thread ID nor a contact ID
-- and only this table maps it back to a thread.
CREATE TABLE judgment_example (
    id TEXT PRIMARY KEY DEFAULT uuid_generate_v4()::text,
    thread_id UUID NOT NULL REFERENCES node(id),
    input_hash TEXT NOT NULL,
    UNIQUE (thread_id, input_hash)
);

-- judgment: one method version's judgment of one example. A method version
-- holds at most one judgment per example (thread and input hash). cut_off is
-- its first appearance's; judged_at is when the call that produced it ran.
CREATE TABLE judgment (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    method_key TEXT NOT NULL,
    method_version TEXT NOT NULL,
    wording_version TEXT NOT NULL,
    example_id TEXT NOT NULL REFERENCES judgment_example(id),
    context_kind TEXT NOT NULL CHECK (context_kind IN ('conversation', 'meeting')),
    cut_off TIMESTAMPTZ NOT NULL,
    judged_at TIMESTAMPTZ NOT NULL,
    context_tokens INTEGER NOT NULL,
    question_count INTEGER NOT NULL,
    billed_tokens INTEGER,
    cost_usd DOUBLE PRECISION,
    latency_ns BIGINT NOT NULL,
    -- The answered questions: each question's ID, every option's probability
    -- and the model's original answer.
    questions JSONB NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (method_key, method_version, wording_version, example_id)
);

-- judgment_appearance: a judgment recorded in a run at that run's cut-off.
-- fresh marks the appearance whose call produced the judgment; a reused
-- appearance sent nothing. The effect counts are what that appearance changed.
CREATE TABLE judgment_appearance (
    run_id UUID NOT NULL REFERENCES judgment_run(id),
    judgment_id UUID NOT NULL REFERENCES judgment(id),
    cut_off TIMESTAMPTZ NOT NULL,
    fresh BOOLEAN NOT NULL,
    obligations_opened INTEGER NOT NULL DEFAULT 0,
    flags_raised INTEGER NOT NULL DEFAULT 0,
    claims_raised INTEGER NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (run_id, judgment_id, cut_off)
);

CREATE INDEX idx_judgment_appearance_judgment ON judgment_appearance (judgment_id);

-- judgment_incomplete: a thread a run could not judge at a cut-off, and why.
CREATE TABLE judgment_incomplete (
    run_id UUID NOT NULL REFERENCES judgment_run(id),
    thread_id UUID NOT NULL REFERENCES node(id),
    cut_off TIMESTAMPTZ NOT NULL,
    reason TEXT NOT NULL CHECK (reason IN ('over_limit', 'call_failed', 'input_changed')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (run_id, thread_id, cut_off)
);

-- judgment_outcome: the structural subtype of an outcome node, one per answered
-- question of a judgment, naming its question, its participant contact (none
-- for big_moment), a commitment's selected date and the method's confidence at
-- the precision the method reported it (the answer assertion's whole-percent
-- confidence serves the apply rule's threshold; ranking outcomes needs this).
-- question_id is the method's question ID; it and confidence are NULL for an
-- omission, the outcome the store raises for a party whose open obligation the
-- judgment does not mention.
-- contact_id cascades like contact_task's: contacts are soft-deleted in normal
-- operation, so only a test teardown's hard delete reaches it.
CREATE TABLE judgment_outcome (
    node_id UUID PRIMARY KEY REFERENCES node(id) ON DELETE CASCADE,
    judgment_id UUID NOT NULL REFERENCES judgment(id),
    question TEXT NOT NULL CHECK (question IN ('ball_in_court', 'commitment_mine', 'commitment_theirs', 'big_moment')),
    question_id TEXT,
    contact_id UUID REFERENCES contact(id) ON DELETE CASCADE,
    due_date DATE,
    confidence DOUBLE PRECISION CHECK (confidence BETWEEN 0 AND 1),
    CONSTRAINT judgment_outcome_answered CHECK ((question_id IS NULL) = (confidence IS NULL)),
    CONSTRAINT judgment_outcome_contact CHECK ((question = 'big_moment') = (contact_id IS NULL)),
    CONSTRAINT judgment_outcome_due_date CHECK (due_date IS NULL OR question IN ('commitment_mine', 'commitment_theirs'))
);

CREATE UNIQUE INDEX idx_judgment_outcome_question
    ON judgment_outcome (judgment_id, question_id)
    WHERE question_id IS NOT NULL;
CREATE UNIQUE INDEX idx_judgment_outcome_omission
    ON judgment_outcome (judgment_id, question, contact_id) NULLS NOT DISTINCT
    WHERE question_id IS NULL;

COMMIT;
