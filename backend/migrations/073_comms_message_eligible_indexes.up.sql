-- 073_comms_message_eligible_indexes.up.sql
-- Two partial indexes backing the eligible-row scans on comms_message, mirroring
-- the sibling tables' 049 pattern (telegram_message / messages_message) adapted
-- for comms_message's multi-source `source` column.
--
-- Five queries in queries/comms_message.sql filter the eligible predicate
--   processed_at IS NULL
--   AND (claimed_at IS NULL OR claimed_at < NOW() - INTERVAL '5 minutes')
--   AND deleted_at IS NULL
-- The 5-min messaging sweeper runs ListUnprocessedCommsContactIDs on every tick.
-- Without these indexes the planner prefix-scans the dedup index on `source` and
-- heap-filters all-time source history; with them the recurring sweep is
-- O(eligible rows). The (claimed_at IS NULL OR claimed_at < ...) disjunction is
-- served by BitmapOr-ing the two partial indexes (one per branch). Both stay
-- near-empty in steady state because a row leaves the index once processed_at is
-- set.
--
-- `source` LEADS the key (vs. 049's matched_contact_id-first key): comms_message
-- is multi-source and every eligible query equality-filters `source = @source`,
-- so a single seek lands on the per-source slice. This matches the column order
-- the existing dedup index (source, external_id, matched_contact_id) and thread
-- index (source, thread_id) already use.
--
-- `matched_contact_id IS NOT NULL` is OMITTED from the partial predicate (049
-- includes it): on comms_message that column is NOT NULL (058:20), so the clause
-- would be permanently true — dead weight that also implies a nullability the
-- column does not have.
--
-- Plain (non-concurrent) CREATE INDEX, mirroring 049: takes a SHARE lock (blocks
-- writes, allows reads) and scans the full heap to evaluate each partial
-- predicate. golang-migrate runs this whole file as one implicit transaction, so
-- the lock spans both builds. At the expected comms_message size this is a
-- sub-second-to-few-seconds write stall during a deploy migration, which is
-- acceptable. No migration in backend/migrations/ uses CONCURRENTLY.

-- Unprocessed-eligible scan (claimed_at IS NULL branch).
CREATE INDEX idx_comms_message_unprocessed_eligible
    ON comms_message(source, matched_contact_id, sent_at)
    WHERE processed_at IS NULL
      AND claimed_at IS NULL
      AND deleted_at IS NULL;

-- Stale-claim recovery scan (claimed_at IS NOT NULL branch).
CREATE INDEX idx_comms_message_stale_claim
    ON comms_message(source, matched_contact_id, claimed_at)
    WHERE processed_at IS NULL
      AND claimed_at IS NOT NULL
      AND deleted_at IS NULL;
