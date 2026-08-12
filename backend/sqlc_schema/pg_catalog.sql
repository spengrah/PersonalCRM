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
--
-- convalidated and confdeltype are added for GetContactIdNodeFkCatalog: real
-- Postgres types are bool and "char" respectively (confdeltype is a
-- single-byte code — 'a' NO ACTION, 'r' RESTRICT, 'c' CASCADE, 'n' SET NULL,
-- 'd' SET DEFAULT); the stub uses text for confdeltype since sqlc's "char"
-- support is unreliable, and the query casts it explicitly.
CREATE TABLE pg_constraint (
    oid          oid NOT NULL,
    conname      text NOT NULL,
    conrelid     oid NOT NULL,
    convalidated bool NOT NULL,
    confdeltype  text NOT NULL
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

-- pg_stat_activity, pg_blocking_pids(), and pg_backend_pid() back the merge
-- transfer race test's blocking probe (TestBackendPID, TestCountBackendsBlockedBy
-- in internal/db/queries/test.sql); sqlc v1.30.0 does not model any of them.
-- As above, only the columns/signatures those queries touch are declared, and
-- the live catalog is the source of truth at runtime — the stub function
-- bodies are placeholders sqlc only type-checks against.
CREATE TABLE pg_stat_activity (
    pid             integer NOT NULL,
    state           text,
    wait_event_type text,
    datname         text
);

CREATE FUNCTION pg_blocking_pids(pid integer) RETURNS integer[] AS $$
    SELECT ARRAY[]::integer[];
$$ LANGUAGE sql;

CREATE FUNCTION pg_backend_pid() RETURNS integer AS $$
    SELECT 0;
$$ LANGUAGE sql;
