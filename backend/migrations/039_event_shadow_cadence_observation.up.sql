-- 039_event_shadow_cadence_observation.up.sql
-- Shadow-mode observation table for the event-bus CadenceUpdater consumer
-- (PR 7 of #180). See .ai/spec/event-bus-foundation.md §3.4.2 and
-- .ai/log/plan/event-bus-foundation-pr7-cadence-updater-shadow.md
-- Design Decision 1.
--
-- Sibling table to event_shadow_observation (migration 038). Parallel
-- structure, distinct payload — the cadence observation captures the
-- four cadence-column pre/post images per event, whereas 038 captures
-- interaction-row resolution. PR 12 drops both shadow tables together.
--
-- Append-only. Each write path (direct vs consumer) produces its own
-- row — the UNIQUE (event_id, writer) key prevents duplicate writes
-- under river retries while leaving the FULL OUTER JOIN divergence
-- query able to surface missing-peer cases.
CREATE TABLE event_shadow_cadence_observation (
    id                     uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    event_id               uuid NOT NULL REFERENCES event(id),
    writer                 text NOT NULL,
    contact_id             uuid NOT NULL,
    source                 text NOT NULL,   -- interaction.source from the envelope
    direction              text NOT NULL,   -- outbound|inbound|mutual
    branch                 text NOT NULL,   -- 'forward' | 'unconditional'
    occurred_at            timestamptz NOT NULL,

    -- Pre-image (snapshot taken before applying).
    prev_last_contacted    timestamptz,
    prev_last_outreach_at  timestamptz,
    prev_last_response_at  timestamptz,
    prev_contact_by        date,

    -- Post-image (what the consumer WOULD write in cutover mode). For the
    -- forward-only branch these are the forward-max values; for the
    -- unconditional branch they are the incoming values straight through.
    -- NULL for any column whose apply-flag is false (direction rule).
    next_last_contacted    timestamptz,
    next_last_outreach_at  timestamptz,
    next_last_response_at  timestamptz,
    next_contact_by        date,

    -- Per-column apply flag snapshot (for post-bake audit of direction rules).
    apply_last_contacted   boolean NOT NULL,
    apply_last_outreach_at boolean NOT NULL,
    apply_last_response_at boolean NOT NULL,
    apply_contact_by       boolean NOT NULL,

    observed_at            timestamptz NOT NULL DEFAULT NOW(),

    CONSTRAINT event_shadow_cadence_observation_writer_chk
        CHECK (writer IN ('direct', 'consumer')),
    CONSTRAINT event_shadow_cadence_observation_direction_chk
        CHECK (direction IN ('outbound', 'inbound', 'mutual')),
    CONSTRAINT event_shadow_cadence_observation_branch_chk
        CHECK (branch IN ('forward', 'unconditional')),
    -- River retries can re-run the consumer worker after the insert commits
    -- but before the job is marked successful. Unique key + ON CONFLICT in
    -- the insert queries (plan Decision 4) gives at-most-one-row-per-writer
    -- semantics without corrupting the FULL OUTER JOIN in the divergence
    -- query. The same guard applies to the direct-path observer in case
    -- a caller accidentally fires it twice for the same event_id.
    CONSTRAINT event_shadow_cadence_observation_event_writer_key
        UNIQUE (event_id, writer)
);

CREATE INDEX idx_event_shadow_cadence_observation_contact
    ON event_shadow_cadence_observation (contact_id, observed_at DESC);

CREATE INDEX idx_event_shadow_cadence_observation_observed_at
    ON event_shadow_cadence_observation (observed_at DESC);
