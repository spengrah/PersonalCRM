-- 040_event_consumer_claim.up.sql
-- Durable claim table for consumer-side exactly-once processing of
-- interaction.recorded events under the PR 8 cutover (see issue #180 and
-- .ai/log/plan/event-bus-foundation-pr8-cadence-updater-cutover.md).
--
-- Why this table exists:
--
-- PR 8 makes the CadenceUpdater consumer the sole writer of the four
-- cadence columns on contact. InteractionRecorder inline-calls
-- CadenceUpdater.HandleEvent immediately after it publishes
-- interaction.recorded so manual corrections and provider-driven writes
-- apply synchronously. The same event_id is ALSO enqueued on the river
-- cadence_updater worker and will be delivered again post-commit. Without
-- a durable claim, a later re-delivery of the same event_id can
-- re-apply cadence — clobbering newer activity that happened in between.
--
-- The claim table is a narrow dedupe mechanism keyed by (event_id,
-- consumer). Whichever path (inline or queued) inserts the claim row
-- first is the path that runs applyTx; the other path sees the existing
-- row and returns nil without mutating contact.
--
-- claimed_at is present so operators can observe claim latency and later
-- prune old rows if needed. PR 8 does NOT add a TTL or cleanup index;
-- forward pointer for future ops: a post-bake cleanup job can DELETE
-- rows older than an operationally-safe horizon keyed on claimed_at.
CREATE TABLE event_consumer_claim (
    event_id   uuid NOT NULL,
    consumer   text NOT NULL,
    claimed_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (event_id, consumer)
);
