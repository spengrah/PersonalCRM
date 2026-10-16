-- Judgment predicates and the apply rule's catalog data.
--
-- predicate.judgment marks the predicates whose landing status the catalog
-- alone decides: the assertion write path reads default_review_policy and, for
-- an auto-if-confident judgment predicate with a threshold, accepts only an
-- assertion whose confidence reaches it. No request field overrides either, so
-- loosening the apply rule is an UPDATE of these rows.
--
-- The judgment predicates, each a text fact about one outcome node:
--   judgment_answer     the outcome a method chose (always accepted: it records
--                       what the method said)
--   judgment_addition   an outcome that opens an obligation, raises a flag or
--                       moves an obligation's date earlier; accepted at or above
--                       the confidence threshold, a claim below it
--   judgment_removal    an outcome that would close an obligation, end an
--                       awaiting-reply wait or move a date later; always a claim
--   judgment_no_effect  an outcome that changes nothing; always accepted
--   judgment_review     the user's review, holding the right outcome
-- big_moment_flag is the accepted flag on a thread an applied big-moment
-- outcome raises; it is an ordinary predicate, written only by the judgment
-- store.
--
-- Wrapped in an explicit transaction: the postgres migration driver does NOT
-- auto-wrap a migration file, so multi-statement DDL must self-wrap to stay
-- all-or-nothing on a mid-file failure (Postgres DDL is transactional).

BEGIN;

ALTER TABLE predicate
    ADD COLUMN judgment BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN judgment_confidence_threshold SMALLINT,
    ADD CONSTRAINT predicate_judgment_threshold CHECK (
        judgment_confidence_threshold IS NULL
        OR (judgment
            AND default_review_policy = 'auto-if-confident'
            AND judgment_confidence_threshold BETWEEN 0 AND 100)
    );

INSERT INTO predicate (
    key, kind, subject_type, object_type, value_type, cardinality, "symmetric",
    temporal_profile, default_review_policy, proposition_bucket, status,
    description, judgment, judgment_confidence_threshold
) VALUES
    ('judgment_answer',    'fact', 'judgment_outcome', NULL, 'text', 'single', FALSE,
     'permanent', 'auto-if-confident', 'none', 'curated',
     'The outcome a decision-model method chose for one question', TRUE, NULL),
    ('judgment_addition',  'fact', 'judgment_outcome', NULL, 'text', 'multi',  FALSE,
     'permanent', 'auto-if-confident', 'none', 'curated',
     'A judgment outcome that opens an obligation, raises a flag or moves a date earlier', TRUE, 50),
    ('judgment_removal',   'fact', 'judgment_outcome', NULL, 'text', 'multi',  FALSE,
     'permanent', 'always-confirm', 'none', 'curated',
     'A judgment outcome that would close an obligation, end a wait or move a date later', TRUE, NULL),
    ('judgment_no_effect', 'fact', 'judgment_outcome', NULL, 'text', 'multi',  FALSE,
     'permanent', 'auto-if-confident', 'none', 'curated',
     'A judgment outcome that changes no live state', TRUE, NULL),
    ('judgment_review',    'fact', 'judgment_outcome', NULL, 'text', 'single', FALSE,
     'permanent', 'auto-if-confident', 'none', 'curated',
     'The right outcome the user stated when reviewing a judgment outcome', TRUE, NULL),
    ('big_moment_flag',    'fact', 'venue',            NULL, 'text', 'multi',  FALSE,
     'permanent', 'auto-if-confident', 'none', 'curated',
     'A significant moment in a participant''s or another person''s life the thread reveals', FALSE, NULL);

COMMIT;
