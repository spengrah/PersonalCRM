-- 045_jsonb_array_lower_indexes.up.sql
-- Speed up case-insensitive lookups inside JSONB array columns by adding a
-- shared helper function plus two functional GIN indexes. Replaces O(n) seq
-- scans through `jsonb_array_elements(...) WHERE LOWER(...) = LOWER($)` with
-- bitmap-index scans backed by GIN indexes on lowercased text[] projections.
--
-- The helper is intentionally STRICT IMMUTABLE PARALLEL SAFE so PostgreSQL
-- can use its result as the body of a functional index. The defensive
-- `jsonb_typeof(j) = 'array'` guard converts non-array JSONB into an empty
-- array on the fly, so a stray scalar/object value (zero rows have this
-- shape today; defensive against future schema drift) cannot fail the
-- index build or break the indexed query.
--
-- `CREATE FUNCTION` (not `CREATE OR REPLACE`) is intentional: a future
-- migration that tries to redefine this function will fail loudly,
-- forcing the author to consider whether the dependent indexes need
-- to be rebuilt.

CREATE FUNCTION jsonb_array_lower_values(j jsonb, key text)
  RETURNS text[]
  LANGUAGE sql
  IMMUTABLE
  STRICT
  PARALLEL SAFE
  AS $$
    SELECT array_agg(LOWER(elem->>key))
    FROM jsonb_array_elements(
        CASE WHEN jsonb_typeof(j) = 'array' THEN j ELSE '[]'::jsonb END
    ) AS elem
    WHERE elem->>key IS NOT NULL
  $$;

-- GIN index on lowercased calendar_event.attendees[].email.
-- Used by FindEventsByAttendeeEmailUnmatchedForContact (rematch).
CREATE INDEX idx_calendar_event_attendees_email_lower_gin
  ON calendar_event
  USING GIN (jsonb_array_lower_values(attendees, 'email'));

-- GIN index on lowercased external_contact.emails[].value.
-- Used by FindExternalContactsByNormalizedEmail (sync matching).
CREATE INDEX idx_external_contact_emails_value_lower_gin
  ON external_contact
  USING GIN (jsonb_array_lower_values(emails, 'value'));
