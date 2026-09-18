BEGIN;

-- Connected outbound calls were recorded as outbound before the call ingest
-- used positive duration to distinguish connected calls from attempts. Repair
-- only the rows whose staging record proves a connected outbound call. This is
-- deliberately a data repair rather than replaying interaction.recorded events:
-- replay would be able to close a newer follow-up or clear a newer skip state.
-- Existing follow-up tasks, including those created by repaired calls, stay open.

-- The columns below are cadence-owned derived values. Use forward-max merges
-- so a later interaction remains authoritative. contact_by is advanced only
-- when it still equals the value cadence would have produced from the old
-- last_contacted (or created_at); a different value represents a user/skip
-- override and is preserved. Skip and awaiting-reply columns are intentionally
-- not touched by this historical correction.
-- Cadence intervals below are the one-time production snapshot used by the
-- 082 backfill: they mirror cadence.ProductionCadenceConfig (days × 24 hours).
-- Date conversion is explicitly UTC so this migration does not depend on the
-- database session TimeZone. This matches the deployed backend running in UTC;
-- unlike migration 082, date rollover is deterministic across migration sessions.
SET LOCAL crm.derived_writer = 'cadence';

WITH repaired AS (
    SELECT
        i.id AS interaction_id,
        i.contact_id,
        i.occurred_at
    FROM interaction i
    JOIN phone_call pc ON pc.interaction_id = i.id
    JOIN contact c ON c.id = i.contact_id AND c.deleted_at IS NULL
    WHERE i.source = 'phone_calls'
      AND i.direction = 'outbound'
      AND i.deleted_at IS NULL
      AND pc.direction = 'outbound'
      AND pc.duration_seconds > 0
), promoted AS (
    UPDATE interaction i
    SET direction = 'mutual'
    FROM repaired r
    WHERE i.id = r.interaction_id
    RETURNING i.contact_id, i.occurred_at
), repaired_contacts AS (
    SELECT
        p.contact_id,
        MAX(p.occurred_at) AS connected_at
    FROM promoted p
    GROUP BY p.contact_id
), projected AS (
    SELECT
        c.id,
        c.cadence,
        c.created_at,
        c.last_contacted AS old_last_contacted,
        c.contact_by AS old_contact_by,
        CASE
            WHEN c.last_contacted IS NULL OR r.connected_at > c.last_contacted
                THEN r.connected_at
            ELSE c.last_contacted
        END AS new_last_contacted,
        r.connected_at
    FROM contact c
    JOIN repaired_contacts r ON r.contact_id = c.id
    WHERE c.deleted_at IS NULL
), derived AS (
    SELECT
        p.*,
        CASE p.cadence
            WHEN 'weekly' THEN INTERVAL '168 hours'
            WHEN 'biweekly' THEN INTERVAL '336 hours'
            WHEN 'monthly' THEN INTERVAL '720 hours'
            WHEN 'quarterly' THEN INTERVAL '2160 hours'
            WHEN 'biannual' THEN INTERVAL '4320 hours'
            WHEN 'annual' THEN INTERVAL '8760 hours'
            ELSE NULL
        END AS cadence_interval
    FROM projected p
)
UPDATE contact c
SET
    last_contacted = d.new_last_contacted,
    last_interaction_at = CASE
        WHEN c.last_interaction_at IS NULL OR d.connected_at > c.last_interaction_at
            THEN d.connected_at
        ELSE c.last_interaction_at
    END,
    last_outreach_at = CASE
        WHEN c.last_outreach_at IS NULL OR d.connected_at > c.last_outreach_at
            THEN d.connected_at
        ELSE c.last_outreach_at
    END,
    last_response_at = CASE
        WHEN c.last_response_at IS NULL OR d.connected_at > c.last_response_at
            THEN d.connected_at
        ELSE c.last_response_at
    END,
    contact_by = CASE
        WHEN d.cadence_interval IS NULL
          OR d.new_last_contacted = d.old_last_contacted
          OR (
              d.old_contact_by IS NOT NULL
              AND d.old_contact_by <> ((
                  COALESCE(d.old_last_contacted, d.created_at) + d.cadence_interval
              ) AT TIME ZONE 'UTC')::date
          )
            THEN c.contact_by
        ELSE ((d.new_last_contacted + d.cadence_interval) AT TIME ZONE 'UTC')::date
    END,
    updated_at = NOW()
FROM derived d
WHERE c.id = d.id;

COMMIT;
