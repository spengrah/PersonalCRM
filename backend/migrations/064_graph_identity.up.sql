-- Graph identity tables (SP1 relationship data model — graph foundation).
--
-- Introduces the uniform node registry plus the two structural subtypes that
-- attach to it (entity, venue) and the entity_type catalog. Person nodes are NOT
-- a subtype table: a person node's id is the owning contact's id (the dual-write
-- lands in a later PR), so a person needs no extra row. The edge/assertion FK
-- target is uniformly node.id across all three node types.
--
-- Wrapped in an explicit transaction: the postgres migration driver does NOT
-- auto-wrap a migration file, so multi-statement DDL must self-wrap to stay
-- all-or-nothing on a mid-file failure (Postgres DDL is transactional).

BEGIN;

-- node: the thin, uniform registry every graph entity is addressable through.
-- id is supplied by the caller (for persons, id == contact.id), so there is NO
-- default. type is a CHECK enum (matches the codebase convention; alterable
-- without ALTER TYPE ceremony). merged_into is the merge alias (loser → winner);
-- the self-FK is nullable and never cascades. deleted_at is the single tombstone
-- — entity/venue liveness flows from the parent node, not their own column. No
-- updated_at (nodes are append-mostly; canonical_label edits are rare).
CREATE TABLE node (
    id UUID PRIMARY KEY,
    type TEXT NOT NULL CHECK (type IN ('person', 'venue', 'entity')),
    canonical_label TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    deleted_at TIMESTAMPTZ,
    merged_into UUID REFERENCES node(id)
);

CREATE INDEX idx_node_type ON node (type);
CREATE INDEX idx_node_deleted_at ON node (deleted_at) WHERE deleted_at IS NULL;
CREATE INDEX idx_node_merged_into ON node (merged_into) WHERE merged_into IS NOT NULL;

-- entity_type: the per-TYPE catalog of entity subtypes. resolution_config is the
-- per-type resolution knob (consumed by SP3); status distinguishes curated-core
-- subtypes from provisional ones minted at runtime.
CREATE TABLE entity_type (
    key TEXT PRIMARY KEY,
    description TEXT NOT NULL DEFAULT '',
    resolution_config JSONB NOT NULL DEFAULT '{}',
    status TEXT NOT NULL CHECK (status IN ('curated', 'provisional'))
);

-- entity: the structural subtype for non-person, non-venue nodes (organizations,
-- places, topics, tags). node_id is both PK and FK to node (one entity row per
-- entity node). detail is the per-INSTANCE attribute bag (e.g. a tag's color, a
-- place's coordinates) — distinct from entity_type.resolution_config, which is
-- per-TYPE. ON DELETE CASCADE is safe because nodes are never hard-deleted in
-- normal operation (merge uses merged_into; person deletion is soft); the cascade
-- only matters for test teardown / reset TRUNCATE (CASCADE anyway).
CREATE TABLE entity (
    node_id UUID PRIMARY KEY REFERENCES node(id) ON DELETE CASCADE,
    subtype TEXT NOT NULL REFERENCES entity_type(key),
    normalized_name TEXT NOT NULL,
    external_ref TEXT,
    detail JSONB NOT NULL DEFAULT '{}'
);

CREATE INDEX idx_entity_subtype ON entity (subtype);
CREATE INDEX idx_entity_normalized_name ON entity (normalized_name);
-- One entity node per normalized name per subtype (entity-resolution dedup).
CREATE UNIQUE INDEX idx_entity_subtype_name ON entity (subtype, normalized_name);

-- venue: the structural subtype for shared-container nodes (email threads, group
-- chats, DMs, meetings, calls, sessions). source disambiguates collisions across
-- sources; (source, kind, source_container_id) is unique so one venue node maps
-- to one real container. Same ON DELETE CASCADE rationale as entity.
CREATE TABLE venue (
    node_id UUID PRIMARY KEY REFERENCES node(id) ON DELETE CASCADE,
    kind TEXT NOT NULL CHECK (kind IN ('email_thread', 'group_chat', 'dm', 'meeting', 'call', 'session')),
    source TEXT NOT NULL,
    source_container_id TEXT NOT NULL,
    title TEXT
);

-- One venue node per real container.
CREATE UNIQUE INDEX idx_venue_container ON venue (source, kind, source_container_id);
CREATE INDEX idx_venue_kind ON venue (kind);

COMMIT;
