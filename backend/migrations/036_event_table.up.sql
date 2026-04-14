-- 036_event_table.up.sql
-- Append-only event log feeding the river-backed worker queue. See
-- .ai/spec/event-bus-foundation.md §3.1.
CREATE TABLE event (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    source      text NOT NULL,
    source_id   text,
    kind        text NOT NULL,
    payload     jsonb NOT NULL,
    observed_at timestamptz NOT NULL,
    received_at timestamptz NOT NULL DEFAULT NOW(),
    created_at  timestamptz NOT NULL DEFAULT NOW()
);

-- Publisher-side idempotency: (source, source_id) is globally unique when
-- source_id is present. Partial index so NULL source_ids (e.g., manual
-- interactions with no external ref) always insert.
CREATE UNIQUE INDEX idx_event_source_source_id
    ON event (source, source_id)
    WHERE source_id IS NOT NULL;

-- Query indexes: latest-N-by-source and latest-N-by-kind are both needed for
-- ad hoc debugging / admin tools (spec §3.1 notes).
CREATE INDEX idx_event_source_observed_at
    ON event (source, observed_at DESC);

CREATE INDEX idx_event_kind_observed_at
    ON event (kind, observed_at DESC);
