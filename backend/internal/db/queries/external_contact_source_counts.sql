-- name: CountExternalContactsByHostAndSource :many
-- Counts live external_contact rows per source, scoped to a host. Powers
-- the GET /api/v1/host/:id/source-counts endpoint (issue #327). Filters
-- match the "caught up" semantic the Hosts page UI needs:
--   - deleted_at IS NULL: excludes tombstoned rows.
--   - duplicate_of_id IS NULL: excludes merge-dupe rows that the import
--     UI doesn't surface.
SELECT source, COUNT(*) AS count
FROM external_contact
WHERE host_id = @host_id
  AND deleted_at IS NULL
  AND duplicate_of_id IS NULL
GROUP BY source
ORDER BY source;
