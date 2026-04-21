-- Event consumer claim queries (migration 040; spec §3.4.2).
-- The (event_id, consumer) primary key enforces at-most-once
-- processing across the inline + queued delivery paths for the
-- CadenceUpdater consumer.

-- name: InsertEventConsumerClaim :execrows
-- Attempts to claim (event_id, consumer). Returns the number of rows
-- inserted: 1 if this caller claimed the event, 0 if another caller
-- already holds the claim. Callers treat 0 as "someone else wrote this
-- event; return nil without mutating state".
INSERT INTO event_consumer_claim (event_id, consumer)
VALUES (@event_id, @consumer)
ON CONFLICT (event_id, consumer) DO NOTHING;

-- name: ExistsEventConsumerClaim :one
-- Non-mutating lookup. Returns true when a claim row exists for the
-- given (event_id, consumer). Useful for assertions in tests and for
-- read-only operator diagnostics; the production dedupe path uses
-- InsertEventConsumerClaim's rows-inserted signal instead of polling.
SELECT EXISTS (
    SELECT 1 FROM event_consumer_claim
    WHERE event_id = @event_id AND consumer = @consumer
) AS claimed;
