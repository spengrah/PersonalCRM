-- Message flags: what a judgment read, the flags and clearings it records, the
-- messages no judgment answered, and the thresholds that decide which flags
-- show.
--
-- A judgment of a version before this migration read its thread's whole
-- context: its window_start is NULL, its read_digest is empty and it records no
-- flag or clearing. A judgment of a newer version reads a window of the context
-- and records, for the messages new at its cut-off, one flag decision per flag
-- kind, and, for each earlier commitment it re-checked, one clearing decision.
--
--   window_start    the message key of the first message of the window
--   new_messages    the keys of the messages new at the cut-off inside the
--                   window, in thread order
--   read_digest     identifies what the judgment read beside its context: its
--                   window start, its new messages and the earlier commitments
--                   it re-checked (the judgment and message key each of its
--                   clearings names). The store alone writes it, so a judgment
--                   is unique by method version, example and what it read; a
--                   judgment of a version before this migration keeps the
--                   empty digest, and with it its old identity of method
--                   version, thread and input hash.
--
-- A flag decision is identified by its thread, message key and kind, whichever
-- method, run or cut-off judged it; judgment_flag holds each judgment's answer
-- for it, so its thread is its judgment's. A clearing decision is identified by
-- its thread, the commitment's message key and the cut-off of the judgment that
-- made it; judgment_clearing holds each judgment's answer for it. That a
-- clearing names a commitment flag decision of an earlier judgment of the same
-- thread is kept by the judgment store's writer alone, not by a constraint.
--
-- Wrapped in an explicit transaction: the postgres migration driver does NOT
-- auto-wrap a migration file, so multi-statement DDL must self-wrap to stay
-- all-or-nothing on a mid-file failure (Postgres DDL is transactional).

BEGIN;

ALTER TABLE judgment
    ADD COLUMN window_start TEXT,
    ADD COLUMN new_messages TEXT[] NOT NULL DEFAULT '{}',
    ADD COLUMN read_digest TEXT NOT NULL DEFAULT '',
    ADD CONSTRAINT judgment_read_shape CHECK (
        (window_start IS NULL AND read_digest = '' AND cardinality(new_messages) = 0)
        OR (window_start IS NOT NULL AND read_digest <> '')
    );

ALTER TABLE judgment DROP CONSTRAINT judgment_method_key_method_version_wording_version_example__key;
ALTER TABLE judgment ADD CONSTRAINT judgment_method_example_read_key
    UNIQUE (method_key, method_version, wording_version, example_id, read_digest);

-- judgment_flag: a judgment's answer for one new message and one flag kind.
-- probability is stored whatever the display threshold.
CREATE TABLE judgment_flag (
    judgment_id UUID NOT NULL REFERENCES judgment(id),
    message_key TEXT NOT NULL,
    kind TEXT NOT NULL CHECK (kind IN ('commitment', 'big_moment', 'claim')),
    probability DOUBLE PRECISION NOT NULL CHECK (probability BETWEEN 0 AND 1),
    PRIMARY KEY (judgment_id, message_key, kind)
);

-- judgment_clearing: a judgment's answer to whether one earlier commitment is
-- done as of its cut-off. commitment_judgment_id names the earlier judgment
-- holding the commitment flag decision. done is the method's answer and
-- probability the answer's probability of done.
CREATE TABLE judgment_clearing (
    judgment_id UUID NOT NULL REFERENCES judgment(id),
    commitment_message_key TEXT NOT NULL,
    commitment_judgment_id UUID NOT NULL REFERENCES judgment(id),
    done BOOLEAN NOT NULL,
    probability DOUBLE PRECISION NOT NULL CHECK (probability BETWEEN 0 AND 1),
    PRIMARY KEY (judgment_id, commitment_message_key)
);

CREATE INDEX idx_judgment_clearing_commitment ON judgment_clearing (commitment_judgment_id);

-- judgment_unjudged: a new message no judgment of the run answered at a
-- cut-off, and why. It is neither a flag decision nor an answer of no.
CREATE TABLE judgment_unjudged (
    run_id UUID NOT NULL REFERENCES judgment_run(id),
    thread_id UUID NOT NULL REFERENCES node(id),
    cut_off TIMESTAMPTZ NOT NULL,
    message_key TEXT NOT NULL,
    reason TEXT NOT NULL CHECK (reason IN ('call_failed', 'message_over_limit', 'before_window')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (run_id, thread_id, cut_off, message_key)
);

-- A context whose newest message alone exceeds a method's limits is reported
-- incomplete with its own reason.
ALTER TABLE judgment_incomplete DROP CONSTRAINT judgment_incomplete_reason_check;
ALTER TABLE judgment_incomplete ADD CONSTRAINT judgment_incomplete_reason_check
    CHECK (reason IN ('over_limit', 'call_failed', 'input_changed', 'message_over_limit'));

-- The clearing floor per method and the display threshold per method and flag
-- kind: the one source each reader reads. A flag shows when its probability
-- reaches the display threshold; a commitment above the clearing floor is
-- re-checked at later cut-offs. Every threshold row carries its method's floor
-- through a composite key (the floor changes in judgment_flag_method alone and
-- cascades), so a threshold at or below its floor cannot be stored.
CREATE TABLE judgment_flag_method (
    method_key TEXT PRIMARY KEY,
    clearing_floor DOUBLE PRECISION NOT NULL CHECK (clearing_floor >= 0 AND clearing_floor < 1),
    UNIQUE (method_key, clearing_floor)
);

CREATE TABLE judgment_flag_threshold (
    method_key TEXT NOT NULL,
    kind TEXT NOT NULL CHECK (kind IN ('commitment', 'big_moment', 'claim')),
    display_threshold DOUBLE PRECISION NOT NULL,
    clearing_floor DOUBLE PRECISION NOT NULL,
    PRIMARY KEY (method_key, kind),
    FOREIGN KEY (method_key, clearing_floor)
        REFERENCES judgment_flag_method (method_key, clearing_floor) ON UPDATE CASCADE,
    CHECK (display_threshold > clearing_floor AND display_threshold <= 1)
);

INSERT INTO judgment_flag_method (method_key, clearing_floor) VALUES
    ('clef-masked', 0.1),
    ('clef-masked-reasoned', 0.1),
    ('clef-flash-masked', 0.1),
    ('jev-masked', 0.1),
    ('sage-masked', 0.1);

INSERT INTO judgment_flag_threshold (method_key, kind, display_threshold, clearing_floor)
SELECT m.method_key, k.kind, 0.3, m.clearing_floor
FROM judgment_flag_method m
CROSS JOIN (VALUES ('commitment'), ('big_moment'), ('claim')) AS k(kind);

COMMIT;
