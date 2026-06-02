-- 058_comms_message.up.sql
-- Shared cross-source content store (Gmail integration phase 1).
-- See .ai/spec/2026-06-01-gmail-integration-design.md §6.1.
-- One row = one message x one qualifying contact (per-participant granularity).
-- Email uses it now; gchat/telegram/messages migrate onto it later (separate work).
CREATE TABLE comms_message (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    source TEXT NOT NULL,                       -- 'email' (later 'telegram','messages','gchat')
    external_id TEXT NOT NULL,                  -- email: RFC822 Message-ID; fallback nomsgid:<account>:<gmail_id>
    thread_id TEXT,                             -- email: Gmail threadId
    subject TEXT,                               -- email subject; null for chat sources
    body TEXT,                                  -- canonical plaintext content
    snippet TEXT,                               -- short preview
    peer_handle TEXT,                           -- raw address of the contact side
    peer_normalized TEXT,                       -- normalized (lowercased email)
    direction TEXT NOT NULL CHECK (direction IN ('inbound', 'outbound')),  -- per-message
    sent_at TIMESTAMPTZ NOT NULL,
    account_id TEXT,                            -- connected account that observed it (provenance)
    source_metadata JSONB NOT NULL DEFAULT '{}',-- html body, labels, to/cc/bcc, attachments[], observed_accounts[], per-account gmail ids
    matched_contact_id UUID NOT NULL REFERENCES contact(id) ON DELETE CASCADE,
    interaction_id UUID REFERENCES interaction(id) ON DELETE SET NULL,
    claimed_at TIMESTAMPTZ,                     -- reserved for future telegram/messages migration; unused by email
    claimed_session_ref TEXT,                   -- reserved (as above)
    processed_at TIMESTAMPTZ,                   -- set on aggregation
    deleted_at TIMESTAMPTZ,                     -- soft delete
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Idempotent per-participant dedup, cross-account safe. Partial on deleted_at
-- so a soft-deleted row never blocks re-ingestion; this is the ON CONFLICT target.
CREATE UNIQUE INDEX idx_comms_message_dedup
    ON comms_message(source, external_id, matched_contact_id)
    WHERE deleted_at IS NULL;

-- Per-contact content lookup (newest first).
CREATE INDEX idx_comms_message_contact_sent
    ON comms_message(matched_contact_id, sent_at DESC);

-- Thread grouping.
CREATE INDEX idx_comms_message_thread
    ON comms_message(source, thread_id);

-- Soft-delete filter (matches idx_messages_message_not_deleted).
CREATE INDEX idx_comms_message_not_deleted
    ON comms_message(deleted_at) WHERE deleted_at IS NULL;
