-- Auxiliary schema file for sqlc only. PostgreSQL provides the system
-- catalog pg_constraint and the pg_get_constraintdef() function at
-- runtime; sqlc v1.30.0 does not model them, so the read-only catalog
-- query GetInteractionSourceCheckDef (internal/db/queries/test.sql)
-- fails to compile without these minimal stub declarations.
--
-- We declare only the columns + function signature that query touches.
-- This file is intentionally NOT under backend/migrations/ so
-- golang-migrate never applies it to a real database — the live catalog
-- is always the source of truth at runtime.

CREATE TABLE pg_constraint (
    oid      oid NOT NULL,
    conname  text NOT NULL,
    conrelid oid NOT NULL
);

CREATE FUNCTION pg_get_constraintdef(constraint_oid oid) RETURNS text AS $$
    SELECT ''::text;
$$ LANGUAGE sql;

-- pg_indexes is a system catalog VIEW (over pg_class/pg_index) that sqlc
-- v1.30.0 does not model; the read-only catalog query TestListIndexDefsForTable
-- (internal/db/queries/test.sql) fails to compile without this stub. We declare
-- only the columns that query touches. As above, the live catalog is the source
-- of truth at runtime; this file is never applied by golang-migrate.
CREATE TABLE pg_indexes (
    schemaname text NOT NULL,
    tablename  text NOT NULL,
    indexname  text NOT NULL,
    indexdef   text NOT NULL
);
