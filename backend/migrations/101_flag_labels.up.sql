-- Labels of flag decisions and clearing decisions, for the Threads tab.
--
-- A flag decision is one message of a thread and one flag kind (migration 099);
-- a clearing decision is one commitment's message of a thread at the cut-off of
-- the judgment that re-checked it. The user's label of either belongs to the
-- decision, not to any method's judgment of it, so every method, run and
-- cut-off that judged the decision reads the same label.
--
--   judgment_flag_label      holds: whether the message holds the kind
--   judgment_clearing_label  done:  whether the commitment is done as of the
--                                   cut-off
--
-- The primary key is the decision, so a decision has at most one current
-- label and a later review replaces it. The judgment store's one review
-- writer is the only code that writes either table, so a label made by a
-- flag chip, a clearing control, Confirm Nones or a close reads the same.
-- Neither table is read by a method (HC4 of the message-flags spec).
--
-- Wrapped in an explicit transaction: the postgres migration driver does NOT
-- auto-wrap a migration file, so multi-statement DDL must self-wrap to stay
-- all-or-nothing on a mid-file failure (Postgres DDL is transactional).

BEGIN;

CREATE TABLE judgment_flag_label (
    thread_id UUID NOT NULL REFERENCES node(id),
    message_key TEXT NOT NULL,
    kind TEXT NOT NULL CHECK (kind IN ('commitment', 'big_moment', 'claim')),
    holds BOOLEAN NOT NULL,
    labeled_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (thread_id, message_key, kind)
);

CREATE TABLE judgment_clearing_label (
    thread_id UUID NOT NULL REFERENCES node(id),
    commitment_message_key TEXT NOT NULL,
    cut_off TIMESTAMPTZ NOT NULL,
    done BOOLEAN NOT NULL,
    labeled_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (thread_id, commitment_message_key, cut_off)
);

COMMIT;
