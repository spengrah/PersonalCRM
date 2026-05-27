-- ============================================================================
-- 053_meeting_note: anarlog session staging table + linkage columns
-- ============================================================================
-- Spec: .ai/spec/mac-daemon-phase-2-anarlog-matching.md
-- Parent spec: .ai/spec/mac-daemon.md (v2 — Anarlog humans + sessions)
--
-- Creates the staging table that the Mac daemon writes anarlog session payloads
-- into via the /ingest/events endpoint (`meeting_note.recorded`). Includes the
-- linkage columns (linked_kind, linked_id, linkage_state) from the sidecar
-- spec — they ship in the initial schema rather than as a follow-up because
-- the table has no callers yet, so there is no transitional shape worth
-- landing separately.
--
-- Also widens interaction.source CHECK to include 'anarlog_sessions' so PR 3
-- can write session-derived interactions without a schema change of its own.
-- ============================================================================

-- ----------------------------------------------------------------------------
-- 1. meeting_note staging table
-- ----------------------------------------------------------------------------
CREATE TABLE meeting_note (
    id                   UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    anarlog_session_id   UUID NOT NULL,
    title                TEXT,
    summary              TEXT,
    memo                 TEXT,
    participants         JSONB,
    mac_host_id          UUID REFERENCES mac_host(id) ON DELETE SET NULL,

    -- Polymorphic pointer to the canonical interaction-bearing row this
    -- session corresponds to. No FK constraint — the target table varies
    -- by linked_kind. Referential integrity is maintained by a periodic
    -- cleanup job that nullifies pairs whose target row no longer exists.
    -- Soft-deletes of the target leave the link semantically valid.
    linked_kind          TEXT,
    linked_id            UUID,

    -- linkage_state is assigned by the ingest tx; no transient default.
    -- See spec "Linkage detection algorithm" for the state machine.
    linkage_state        TEXT NOT NULL,

    deleted_at           TIMESTAMPTZ,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    CONSTRAINT meeting_note_linked_kind_check
        CHECK (linked_kind IN ('event', 'phone_call') OR linked_kind IS NULL),
    CONSTRAINT meeting_note_linked_pair_check
        CHECK ((linked_kind IS NULL) = (linked_id IS NULL)),
    CONSTRAINT meeting_note_linkage_state_check
        CHECK (linkage_state IN (
            'linked',
            'linked_impromptu',
            'orphan_title_augmented',
            'orphan_needs_review',
            'conflict_pending'
        ))
);

-- Dedup on session UUID across live rows only. Partial WHERE allows a
-- soft-deleted row and a live re-insert to coexist (re-sync pattern in
-- spec "Re-sync semantics").
CREATE UNIQUE INDEX idx_meeting_note_session_id
    ON meeting_note(anarlog_session_id)
    WHERE deleted_at IS NULL;

-- Backs the periodic cleanup job that nullifies stale polymorphic
-- pointers and the "find sessions linked to event X" lookups.
CREATE INDEX idx_meeting_note_linked
    ON meeting_note(linked_kind, linked_id)
    WHERE linked_id IS NOT NULL AND deleted_at IS NULL;

-- Backs the /imports "Sessions needing attention" query that filters
-- on linkage_state IN ('conflict_pending', 'orphan_needs_review').
CREATE INDEX idx_meeting_note_linkage_state
    ON meeting_note(linkage_state)
    WHERE deleted_at IS NULL;

-- ----------------------------------------------------------------------------
-- 2. interaction.source CHECK extension: add 'anarlog_sessions'
-- ----------------------------------------------------------------------------
-- Existing constraint (migration 049): CHECK (source IN ('manual', 'gcal',
-- 'todoist', 'telegram', 'messages')). This migration adds 'anarlog_sessions'.
-- 'whatsapp' and 'phone_calls' remain deferred — each will need its own
-- migration when its ingest path is added.
ALTER TABLE interaction DROP CONSTRAINT interaction_source_check;
ALTER TABLE interaction ADD CONSTRAINT interaction_source_check
    CHECK (source IN ('manual', 'gcal', 'todoist', 'telegram', 'messages', 'anarlog_sessions'));
