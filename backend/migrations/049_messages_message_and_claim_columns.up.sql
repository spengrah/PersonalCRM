-- 049_messages_message_and_claim_columns.up.sql
-- Mac daemon: messages_message staging table + claim columns on
-- telegram_message + interaction.source CHECK extended to include
-- 'messages'. Spec: .ai/spec/mac-daemon.md §3 New tables, §3 Race
-- mechanics, §5 Failure modes.

-- ============================================================================
-- 1. Claim columns on telegram_message (additive; existing rows backfill NULL)
-- ============================================================================
ALTER TABLE telegram_message ADD COLUMN claimed_at TIMESTAMPTZ;
ALTER TABLE telegram_message ADD COLUMN claimed_session_ref TEXT;

-- Replace the existing unprocessed partial index with the claim-aware variant.
-- The new index covers: matched_contact_id, sent_at, ordered for chronological
-- scans, gated on the same partial predicate the unprocessed-list queries use.
DROP INDEX IF EXISTS idx_telegram_message_unprocessed;
CREATE INDEX idx_telegram_message_unprocessed_eligible
    ON telegram_message(matched_contact_id, sent_at)
    WHERE processed_at IS NULL
      AND claimed_at IS NULL
      AND matched_contact_id IS NOT NULL
      AND deleted_at IS NULL;

-- Recovery-path index: rows whose claim is past the TTL. Used by the stale-
-- claim recovery pass. Cheap because the partial predicate strips out the
-- vast majority of rows.
CREATE INDEX idx_telegram_message_stale_claim
    ON telegram_message(matched_contact_id, claimed_at)
    WHERE processed_at IS NULL
      AND claimed_at IS NOT NULL
      AND deleted_at IS NULL;

-- ============================================================================
-- 2. messages_message staging table
-- ============================================================================
-- Provenance via mac_host_id (nullable, ON DELETE SET NULL): uninstalling a
-- Mac daemon must not delete staged data (spec §5).
CREATE TABLE messages_message (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    -- guid is iMessage's per-message stable id (cross-host dedup). Spec §3.
    guid TEXT NOT NULL,
    -- chat.db's chat.guid for the conversation thread.
    chat_guid TEXT NOT NULL,
    -- Raw sender handle as observed in chat.db (phone or email). Daemon-side
    -- filter guarantees only handles matching a known contact_method reach
    -- this table.
    peer_handle TEXT NOT NULL,
    -- Canonicalized handle (E.164 phone or lowercased email) — match key.
    -- Populated by Pi identity-match path during ingest. Nullable so staging
    -- inserts that race the matcher are still valid rows.
    peer_normalized TEXT,
    -- Full message text (raw_message events forward the body).
    text TEXT,
    -- Type tag inferred from attachment.uti / mime_type.
    message_type TEXT NOT NULL DEFAULT 'text'
        CHECK (message_type IN ('text', 'photo', 'audio', 'video', 'document', 'other')),
    sent_at TIMESTAMPTZ NOT NULL,
    is_outgoing BOOLEAN NOT NULL,
    is_group_chat BOOLEAN NOT NULL DEFAULT FALSE,
    -- Reply target guid (optional; populated when chat.db row has a
    -- reply association). Opaque string matched against guid.
    reply_to_guid TEXT,
    -- Resolved contact (set by Pi ingest service identity-match).
    matched_contact_id UUID REFERENCES contact(id) ON DELETE SET NULL,
    -- Interaction this staging row was rolled into (set by Stage 3).
    interaction_id UUID REFERENCES interaction(id) ON DELETE SET NULL,
    -- Provenance: which Mac pushed this row. Cross-host dedup happens at
    -- the guid uniqueness constraint, so this is purely informational.
    mac_host_id UUID REFERENCES mac_host(id) ON DELETE SET NULL,
    -- Stage 3 commits set processed_at.
    processed_at TIMESTAMPTZ,
    -- Claim mechanism (spec §3 Race Mechanics). Set by aggregator's create
    -- path; cleared by InteractionRecorder when Stage 3 commits.
    claimed_at TIMESTAMPTZ,
    claimed_session_ref TEXT,
    -- Soft-delete (matches telegram_message pattern).
    deleted_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ DEFAULT NOW(),

    -- Globally unique on guid — first push wins; subsequent Mac hosts no-op.
    UNIQUE (guid)
);

-- Unprocessed-eligible scan: same shape as telegram_message's claim-aware
-- variant. Backs ListUnprocessedMessagesByContact* queries.
CREATE INDEX idx_messages_message_unprocessed_eligible
    ON messages_message(matched_contact_id, sent_at)
    WHERE processed_at IS NULL
      AND claimed_at IS NULL
      AND matched_contact_id IS NOT NULL
      AND deleted_at IS NULL;

-- Stale-claim recovery scan.
CREATE INDEX idx_messages_message_stale_claim
    ON messages_message(matched_contact_id, claimed_at)
    WHERE processed_at IS NULL
      AND claimed_at IS NOT NULL
      AND deleted_at IS NULL;

-- Reply-target lookup. Scoped to a chat for selectivity.
CREATE INDEX idx_messages_message_chat_guid
    ON messages_message(chat_guid, guid);

-- Sent-at scan for chronological queries.
CREATE INDEX idx_messages_message_sent_at
    ON messages_message(sent_at DESC);

-- Soft-delete filter (matches telegram_message pattern).
CREATE INDEX idx_messages_message_not_deleted
    ON messages_message(deleted_at) WHERE deleted_at IS NULL;

-- ============================================================================
-- 3. interaction.source CHECK extension: add 'messages'
-- ============================================================================
-- Existing constraint (migration 031): CHECK (source IN ('manual', 'gcal',
-- 'todoist', 'telegram')). This migration adds 'messages' only; 'whatsapp'
-- and 'anarlog_sessions' are deferred — each will need its own migration
-- when its ingest path is added.
ALTER TABLE interaction DROP CONSTRAINT interaction_source_check;
ALTER TABLE interaction ADD CONSTRAINT interaction_source_check
    CHECK (source IN ('manual', 'gcal', 'todoist', 'telegram', 'messages'));
