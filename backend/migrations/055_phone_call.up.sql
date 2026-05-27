-- 055_phone_call.up.sql
-- Mac daemon Phase 1.5: phone_call staging table + interaction.source CHECK
-- extension for 'phone_calls'. See .ai/spec/mac-daemon.md (the `phone_calls`
-- source section, the `phone_call` staging-table definition, and the
-- `interaction.source` CHECK).

-- ============================================================================
-- 1. phone_call staging table
-- ============================================================================
-- Provenance via mac_host_id (nullable, ON DELETE SET NULL): uninstalling a
-- Mac daemon must not delete staged data.
--
-- NO `deleted_at` column: unlike messages_message, phone_call has no
-- aggregator-driven lifecycle. Test cleanup uses hard delete via
-- HardDeletePhoneCallsByMacHost. Soft-delete on staging rows is a
-- messages_message artifact tied to the burst aggregator's claim machinery.
--
-- `interaction_id` is NULLABLE: missed-inbound-no-voicemail rows stay NULL
-- forever because no interaction is created (content-delivered cadence).
-- The future contact-detail timeline UI will project both rows (via union)
-- so the audit trail is preserved.
CREATE TABLE phone_call (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    -- ZCALLRECORD.ZUNIQUE_ID from CallHistoryDB. Apple's Continuity propagates
    -- this across hosts, so the UNIQUE constraint dedups cross-host pushes.
    call_unique_id TEXT NOT NULL UNIQUE,
    -- Raw ZADDRESS from CallHistoryDB (phone or email).
    peer_handle TEXT NOT NULL,
    -- Canonicalized handle (E.164 phone or lowercased email). Daemon emits
    -- both raw and normalized per the `phone_calls` payload spec.
    peer_normalized TEXT NOT NULL,
    -- Derived service enum. The set is frozen; adding a new service requires
    -- a coordinated daemon + Pi migration (see ServiceDerivation.swift).
    service TEXT NOT NULL CHECK (service IN ('voice', 'facetime_audio', 'facetime_video')),
    -- Direction: 'inbound' = received, 'outbound' = sent. Mirrors event kind.
    direction TEXT NOT NULL CHECK (direction IN ('inbound', 'outbound')),
    -- NULLable, only set for inbound rows. CallHistoryDB's ZANSWERED is
    -- unreliable for outbound calls (frequently FALSE even on connected
    -- outbound calls), so outbound rows store NULL even if payload sets a value.
    answered BOOLEAN,
    -- ZHASMESSAGE: voicemail-was-left flag. Only meaningful for inbound;
    -- forced FALSE for outbound (a "voicemail" left by the caller has no
    -- analog in the outbound direction).
    has_voicemail BOOLEAN NOT NULL DEFAULT FALSE,
    -- ZDURATION in whole seconds. 0 for missed inbound and missed outbound.
    duration_seconds INTEGER NOT NULL DEFAULT 0,
    -- ZDATE converted from Apple-epoch seconds to UTC timestamptz.
    started_at TIMESTAMPTZ NOT NULL,
    -- Resolved contact (set by Pi ingest service identity-match).
    matched_contact_id UUID REFERENCES contact(id) ON DELETE SET NULL,
    -- Interaction this staging row was rolled into. NULLable forever for
    -- missed-inbound-no-voicemail rows (content-delivered cadence: a call
    -- you missed and where the caller left no voicemail conveys no content,
    -- so no interaction is recorded).
    interaction_id UUID REFERENCES interaction(id) ON DELETE SET NULL,
    -- Provenance: which Mac pushed this row.
    mac_host_id UUID REFERENCES mac_host(id) ON DELETE SET NULL,
    -- Stage 3 commits set processed_at.
    processed_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ DEFAULT NOW()
);

-- Per-contact chronological index for the future contact-detail timeline
-- UI's per-contact list query. No `deleted_at` predicate (no soft-delete
-- column on this table).
CREATE INDEX idx_phone_call_matched_contact
    ON phone_call(matched_contact_id, started_at);

-- Provenance lookup index for the test-helper HardDeletePhoneCallsByMacHost
-- query and operator debugging.
CREATE INDEX idx_phone_call_mac_host
    ON phone_call(mac_host_id);

-- ============================================================================
-- 2. interaction.source CHECK extension: add 'phone_calls'
-- ============================================================================
-- Existing constraint (after migration 053_meeting_note): CHECK (source IN
-- ('manual', 'gcal', 'todoist', 'telegram', 'messages', 'anarlog_sessions')).
-- This migration adds 'phone_calls' to the cumulative set.
ALTER TABLE interaction DROP CONSTRAINT interaction_source_check;
ALTER TABLE interaction ADD CONSTRAINT interaction_source_check
    CHECK (source IN ('manual', 'gcal', 'todoist', 'telegram', 'messages', 'anarlog_sessions', 'phone_calls'));
