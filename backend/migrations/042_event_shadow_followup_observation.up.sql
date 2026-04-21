-- 042_event_shadow_followup_observation.up.sql
-- Shadow-mode observation table for the FollowUpManager consumer
-- (event-bus foundation, spec §3.4.3). Sibling to event_shadow_observation
-- (038) and event_shadow_cadence_observation (039); the three tables are
-- dropped together when the event-bus migration completes.
--
-- Each write path (direct vs consumer) emits its own row keyed on
-- event_id so the post-bake divergence query can FULL OUTER JOIN on
-- event_id. The (event_id, writer) UNIQUE prevents duplicate observation
-- rows under river retries while leaving missing-peer cases visible.
CREATE TABLE event_shadow_followup_observation (
    id                      uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    event_id                uuid NOT NULL REFERENCES event(id),
    writer                  text NOT NULL,
    contact_id              uuid NOT NULL,
    source                  text NOT NULL,
    direction               text NOT NULL,
    occurred_at             timestamptz NOT NULL,

    -- Action the writer DID (direct) or WOULD do (consumer):
    --   'create'   — new follow-up task
    --   'refresh'  — existing pending follow-up's deadline updated
    --   'complete' — pending follow-up marked completed
    --   'skip'     — nothing to do (outbound + guard fired,
    --                no-cadence skip, or inbound/mutual with no pending)
    action                  text NOT NULL,

    -- For action='skip' on outbound events: which guard fired. NULL for
    -- non-skip actions, no-cadence skips, and inbound/mutual no-pending
    -- skips (those are not guard-class skips).
    skip_reason             text,

    -- Deterministic idempotency key the consumer WOULD use for the local
    -- pending_remote_create insert in cutover. Recorded in shadow so the
    -- post-bake report can validate uniqueness on the prod-Pi data set
    -- before cutover starts writing rows. NULL for non-create actions.
    would_idempotency_key   text,

    -- Deadline the writer DID set (direct) or WOULD set (consumer) for
    -- create / refresh actions. NULL for skip / complete actions.
    would_deadline          date,

    -- For direct rows: the contact_task.id touched by the action. NULL on
    -- skip or on Todoist failure during the direct-path call.
    direct_contact_task_id  uuid,

    -- Consumer-only flag. Must be false for ALL consumer rows in shadow
    -- mode. Post-bake assertion:
    --   SELECT COUNT(*) FROM event_shadow_followup_observation
    --     WHERE writer='consumer' AND consumer_called_todoist = true
    --   → expected 0.
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
