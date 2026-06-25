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
-- Explicit transaction: the golang-migrate postgres driver does not auto-wrap a
-- migration file.
--
-- Safety guard: each table is asserted empty inside the transaction before any
-- DROP. If any has rows (none should — no live writer exists), the migration
-- aborts and rolls back, dropping nothing. Mirrors 055_phone_call.down.sql.
BEGIN;

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

COMMIT;
