-- 044_drop_event_shadow_observations.down.sql
-- Re-creates the three shadow observation tables empty. Rollback-safety
-- only; historical data is not restored. Bodies mirror 038, 039, 042.
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

CREATE TABLE event_shadow_cadence_observation (
    id                     uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    event_id               uuid NOT NULL REFERENCES event(id),
    writer                 text NOT NULL,
    contact_id             uuid NOT NULL,
    source                 text NOT NULL,
    direction              text NOT NULL,
    branch                 text NOT NULL,
    occurred_at            timestamptz NOT NULL,

    prev_last_contacted    timestamptz,
    prev_last_outreach_at  timestamptz,
    prev_last_response_at  timestamptz,
    prev_contact_by        date,

    next_last_contacted    timestamptz,
    next_last_outreach_at  timestamptz,
    next_last_response_at  timestamptz,
    next_contact_by        date,

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
    CONSTRAINT event_shadow_cadence_observation_event_writer_key
        UNIQUE (event_id, writer)
);

CREATE INDEX idx_event_shadow_cadence_observation_contact
    ON event_shadow_cadence_observation (contact_id, observed_at DESC);

CREATE INDEX idx_event_shadow_cadence_observation_observed_at
    ON event_shadow_cadence_observation (observed_at DESC);

CREATE TABLE event_shadow_followup_observation (
    id                      uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    event_id                uuid NOT NULL REFERENCES event(id),
    writer                  text NOT NULL,
    contact_id              uuid NOT NULL,
    source                  text NOT NULL,
    direction               text NOT NULL,
    occurred_at             timestamptz NOT NULL,

    action                  text NOT NULL,
    skip_reason             text,
    would_idempotency_key   text,
    would_deadline          date,
    direct_contact_task_id  uuid,
    consumer_called_todoist boolean NOT NULL DEFAULT false,

    observed_at             timestamptz NOT NULL DEFAULT NOW(),

    CONSTRAINT event_shadow_followup_observation_writer_chk
        CHECK (writer IN ('direct', 'consumer')),
    CONSTRAINT event_shadow_followup_observation_direction_chk
        CHECK (direction IN ('outbound', 'inbound', 'mutual')),
    CONSTRAINT event_shadow_followup_observation_action_chk
        CHECK (action IN ('create', 'refresh', 'complete', 'skip')),
    CONSTRAINT event_shadow_followup_observation_skip_reason_chk
        CHECK (skip_reason IS NULL OR skip_reason IN (
            'backdated', 'out_of_order', 'duplicate_pending'
        )),
    CONSTRAINT event_shadow_followup_observation_event_writer_key
        UNIQUE (event_id, writer)
);

CREATE INDEX idx_event_shadow_followup_observation_contact
    ON event_shadow_followup_observation (contact_id, observed_at DESC);

CREATE INDEX idx_event_shadow_followup_observation_observed_at
    ON event_shadow_followup_observation (observed_at DESC);
