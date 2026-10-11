-- 038_event_shadow_observation.up.sql
-- Shadow-mode observation table for the event-bus InteractionRecorder
-- consumer. See .ai/spec/event-bus-foundation.md §3.8.
--
-- Append-only. Each write path (direct vs consumer) produces its own row so
-- the post-bake divergence query can FULL OUTER JOIN on (source, source_ref)
-- without racing on a shared jsonb column. Migration 044 drops this table.
CREATE TABLE event_shadow_observation (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    event_id       uuid REFERENCES event(id),
    writer         text NOT NULL,
    kind           text NOT NULL,
    source         text NOT NULL,
    source_ref     text,
    contact_id     uuid NOT NULL,
    direction      text NOT NULL,
    occurred_at    timestamptz NOT NULL,
    interaction_id uuid,
    replay         boolean NOT NULL DEFAULT false,
    observed_at    timestamptz NOT NULL DEFAULT NOW(),
    CONSTRAINT event_shadow_observation_writer_chk
        CHECK (writer IN ('direct', 'consumer')),
    CONSTRAINT event_shadow_observation_direction_chk
        CHECK (direction IN ('outbound', 'inbound', 'mutual'))
);

CREATE INDEX idx_event_shadow_observation_event
    ON event_shadow_observation (event_id);

CREATE INDEX idx_event_shadow_observation_source_ref
    ON event_shadow_observation (source, source_ref, writer);

CREATE INDEX idx_event_shadow_observation_observed_at
    ON event_shadow_observation (observed_at DESC);
