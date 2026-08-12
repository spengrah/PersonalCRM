-- One definition of "a live (non-soft-deleted) contact" for child-table reads
-- that join contact only to enforce contact.deleted_at IS NULL. Selecting from
-- this view instead of the base table replaces a hand-copied predicate with a
-- single definition.
--
-- Explicit column list, not SELECT *: Postgres freezes a view's column set at
-- creation time either way, but an explicit list makes that freeze visible in
-- the DDL. Adding a column to contact does NOT appear here automatically — see
-- the When-You-Change row in .ai/rules/core.md.
--
-- Column order matches db.Contact (backend/internal/db/models.go) field for
-- field, so a query that selects live_contact.* generates the same row shape
-- as one that selects contact.*. deleted_at is kept in the projection (always
-- NULL here) purely to preserve that shape match.
--
-- Plain view — not materialized, not security_barrier. A plain view of this
-- shape (no DISTINCT/GROUP BY/aggregate/window/LIMIT/set-op/volatile target) is
-- substituted into the query and flattened by subquery pull-up, so every
-- rewritten query keeps its current plan by construction.
CREATE VIEW live_contact AS
SELECT
    id,
    full_name,
    location,
    birthday,
    how_met,
    cadence,
    last_contacted,
    profile_photo,
    deleted_at,
    created_at,
    updated_at,
    contact_by,
    last_interaction_at,
    last_outreach_at,
    last_response_at
FROM contact
WHERE deleted_at IS NULL;
