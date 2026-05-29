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
