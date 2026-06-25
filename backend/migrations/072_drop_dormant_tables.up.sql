-- 072_drop_dormant_tables.up.sql
-- Drop four dormant tables left from the pre-SP1 schema (all created in 001,
-- never populated by a live write path):
--   connection       - contact↔contact edges, superseded by the assertion graph;
--                      no Create query, only merge-path transfer/dedup queries.
--   contact_summary  - AI-summary cache; no query, regenerable.
--   note_embedding   - dormant vector cache; superseded by the generic `embedding`
--                      table (070); never populated.
--   prompt_query     - never-wired prompt/query-log scaffolding.
-- Forward-only: the .down.sql re-creates the tables empty for rollback safety
-- only; no historical rows are restored (the tables are empty/derived).
--
-- Atomicity: golang-migrate sends this whole file as ONE simple-query exec, so
-- PostgreSQL runs it inside a single implicit transaction — the guard + the four
-- DROPs are all-or-nothing without an explicit BEGIN/COMMIT. (Self-wrapping in
-- BEGIN/COMMIT is deliberately AVOIDED: on the guard's RAISE it would leave
-- migrate's pinned connection inside an open, aborted transaction block, so the
-- post-failure Force/SetVersion recovery — used by the guard test and any
-- operator re-run — fails with "database locked". The implicit transaction gives
-- the same atomicity while keeping the failure path recoverable, matching the
-- repo's other guarded migrations, e.g. 055_phone_call.down.sql.)
--
-- Safety guard: each table is asserted empty before any DROP. If any has rows
-- (none should — no live writer exists), the migration RAISEs and the implicit
-- transaction rolls back, dropping nothing. Mirrors 055_phone_call.down.sql.

-- Guard: refuse to drop a non-empty table. LOCK first so a concurrent writer
-- (there is none, but be correct) cannot insert between the check and the DROP.
DO $$
BEGIN
    LOCK TABLE connection, contact_summary, note_embedding, prompt_query IN ACCESS EXCLUSIVE MODE;
    IF EXISTS (SELECT 1 FROM connection LIMIT 1) THEN
        RAISE EXCEPTION 'cannot drop connection: % rows present; export before dropping',
            (SELECT count(*) FROM connection);
    END IF;
    IF EXISTS (SELECT 1 FROM contact_summary LIMIT 1) THEN
        RAISE EXCEPTION 'cannot drop contact_summary: % rows present; export before dropping',
            (SELECT count(*) FROM contact_summary);
    END IF;
    IF EXISTS (SELECT 1 FROM note_embedding LIMIT 1) THEN
        RAISE EXCEPTION 'cannot drop note_embedding: % rows present; export before dropping',
            (SELECT count(*) FROM note_embedding);
    END IF;
    IF EXISTS (SELECT 1 FROM prompt_query LIMIT 1) THEN
        RAISE EXCEPTION 'cannot drop prompt_query: % rows present; export before dropping',
            (SELECT count(*) FROM prompt_query);
    END IF;
END $$;

DROP TABLE connection;
DROP TABLE contact_summary;
DROP TABLE note_embedding;
DROP TABLE prompt_query;
