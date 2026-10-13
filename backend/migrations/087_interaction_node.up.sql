-- Interaction nodes (interaction-review-readiness PR4).
--
-- Every correctable observation (an interaction row of any source except the
-- message-burst sources) resolves to exactly one structural node of type
-- 'interaction'. Observations of the same source object share it. This
-- migration adds the node type, its subtype table, the interaction column that
-- names the node, and the ONE implementation of the per-source identity rule as
-- SQL functions: the live observation step and a backfill migration both call
-- these functions, so no source's key derivation exists anywhere else.
--
-- Wrapped in an explicit transaction: the postgres migration driver does NOT
-- auto-wrap a migration file, so multi-statement DDL must self-wrap to stay
-- all-or-nothing on a mid-file failure (Postgres DDL is transactional).

BEGIN;

ALTER TABLE node DROP CONSTRAINT node_type_check;
ALTER TABLE node ADD CONSTRAINT node_type_check
    CHECK (type IN ('person', 'venue', 'entity', 'interaction'));

-- interaction_node: the structural subtype for interaction nodes, the way venue
-- is for shared containers. identity_key is the persisted per-source identity
-- (see interaction_identity_key); NULL for a node with no source key (a manual
-- entry, or an observation whose source object cannot be keyed), which is a
-- node of its own. UNIQUE (source, identity_key) lets one keyed source object
-- hold exactly one node (NULL keys never collide). ON DELETE CASCADE matches
-- venue/entity: nodes are never hard-deleted in normal operation (a merge uses
-- node.merged_into), so the cascade only matters for test teardown.
CREATE TABLE interaction_node (
    node_id UUID PRIMARY KEY REFERENCES node(id) ON DELETE CASCADE,
    source TEXT NOT NULL,
    identity_key TEXT,
    UNIQUE (source, identity_key)
);

-- The FK targets interaction_node, so an observation can name only an
-- interaction node, never a person, venue or entity node. Nullable because
-- burst observations have no node and rows stored before this migration have
-- none until a later backfill.
ALTER TABLE interaction ADD COLUMN interaction_node_id UUID REFERENCES interaction_node(node_id);

-- interaction_source_is_burst: the message-burst sources, whose observations get
-- no interaction node. Every other source is correctable.
CREATE FUNCTION interaction_source_is_burst(p_source TEXT)
RETURNS BOOLEAN
LANGUAGE sql
IMMUTABLE
AS $$
    SELECT p_source IN ('telegram', 'messages', 'gchat', 'whatsapp');
$$;

-- interaction_identity_key: THE per-source identity rule. Maps an observation's
-- (source, source_ref) to the persisted key that all observations of one source
-- object share, or NULL when the source has no key (manual) or the object cannot
-- be keyed. The key ignores the components of source_ref that differ between
-- observations of the same object:
--   gcal              the length-prefixed (gcal_event_id, gcal_calendar_id,
--                     google_account_id) 3-tuple of the calendar_event that
--                     source_ref names (the event's real uniqueness)
--   phone_calls       the call_unique_id (source_ref)
--   todoist           the external task id (source_ref)
--   email             '<thread>:<local day>' of '<contact>:<thread>:<local day>'
--                     (the contact component dropped); NULL for an empty thread
--   anarlog_sessions  the session id of 'anarlog:<session>[:title|:walkin]:<contact>'
--                     (participant, title and walk-in components dropped)
-- STABLE, not IMMUTABLE: the gcal arm reads calendar_event.
CREATE FUNCTION interaction_identity_key(p_source TEXT, p_source_ref TEXT)
RETURNS TEXT
LANGUAGE sql
STABLE
AS $$
    SELECT CASE p_source
        WHEN 'gcal' THEN (
            SELECT octet_length(ce.gcal_event_id)::text || ':' || ce.gcal_event_id || '|' ||
                   octet_length(ce.gcal_calendar_id)::text || ':' || ce.gcal_calendar_id || '|' ||
                   octet_length(ce.google_account_id)::text || ':' || ce.google_account_id
            FROM calendar_event ce
            WHERE ce.id::text = p_source_ref)
        WHEN 'phone_calls' THEN p_source_ref
        WHEN 'todoist' THEN p_source_ref
        WHEN 'email' THEN
            CASE WHEN split_part(p_source_ref, ':', 2) <> ''
                 THEN substr(p_source_ref, strpos(p_source_ref, ':') + 1)
            END
        WHEN 'anarlog_sessions' THEN
            CASE WHEN split_part(p_source_ref, ':', 1) = 'anarlog'
                 THEN NULLIF(split_part(p_source_ref, ':', 2), '')
            END
    END;
$$;

-- resolve_interaction_node: finds or creates the interaction node of an
-- observation and returns its id; returns no row for a burst source (an
-- observation with no node), so a scalar use of it yields NULL. A keyed node's id is
-- a deterministic uuid_generate_v5 of the length-prefixed (source, key), so the
-- node insert and the subtype insert are both ON CONFLICT DO NOTHING: a
-- concurrent resolver of the same source object waits on the unique index, then
-- returns the same id, and no orphan node can leak. A keyless observation gets a
-- fresh node of its own. Called by the live observation step and by backfills.
CREATE FUNCTION resolve_interaction_node(p_source TEXT, p_source_ref TEXT)
RETURNS SETOF UUID
LANGUAGE plpgsql
AS $$
DECLARE
    v_key TEXT;
    v_node_id UUID;
BEGIN
    IF interaction_source_is_burst(p_source) THEN
        RETURN;
    END IF;
    v_key := interaction_identity_key(p_source, p_source_ref);
    IF v_key IS NULL THEN
        v_node_id := uuid_generate_v4();
    ELSE
        v_node_id := uuid_generate_v5(
            'c2b4d6f8-3a1e-4f5b-8c7d-9e0a1b2c3d4e'::uuid,
            octet_length(p_source)::text || ':' || p_source || '|' ||
            octet_length(v_key)::text || ':' || v_key);
    END IF;
    INSERT INTO node (id, type, canonical_label)
    VALUES (v_node_id, 'interaction', '')
    ON CONFLICT (id) DO NOTHING;
    INSERT INTO interaction_node (node_id, source, identity_key)
    VALUES (v_node_id, p_source, v_key)
    ON CONFLICT DO NOTHING;
    RETURN NEXT v_node_id;
END;
$$;

COMMIT;
