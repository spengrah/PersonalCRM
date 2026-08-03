-- 076_whatsapp_source_staging_and_history.up.sql
-- WhatsApp source foundations. Four independent pieces, one migration:
--   1. interaction.source gains 'whatsapp' (mirrors 061).
--   2. comms_message.matched_contact_id becomes nullable, bounded by a
--      source-scoped CHECK so only WhatsApp can stage a contactless row.
--   3. whatsapp_history_notification — the durable one-shot history inbox.
--   4. whatsapp_chat_config — the persistent per-chat group gate.
-- Pieces 3 and 4 are inert until a writer exists; they land here so the arc
-- carries exactly one migration.

-- ---------------------------------------------------------------------------
-- 1. interaction.source CHECK
-- Existing set (after 061_interaction_source_gchat): manual, gcal, todoist,
-- telegram, messages, anarlog_sessions, phone_calls, email, gchat.
-- ---------------------------------------------------------------------------
ALTER TABLE interaction DROP CONSTRAINT interaction_source_check;
ALTER TABLE interaction ADD CONSTRAINT interaction_source_check
    CHECK (source IN ('manual', 'gcal', 'todoist', 'telegram', 'messages', 'anarlog_sessions', 'phone_calls', 'email', 'gchat', 'whatsapp'));

-- ---------------------------------------------------------------------------
-- 2. comms_message: allow a message whose peer is not yet a contact
-- Gmail and Google Chat pre-resolve their contacts before staging. WhatsApp
-- cannot: its history arrives once, so a message from an unknown peer must be
-- storable before that peer becomes a contact. The FK (and its ON DELETE
-- CASCADE) are unchanged.
-- ---------------------------------------------------------------------------
ALTER TABLE comms_message ALTER COLUMN matched_contact_id DROP NOT NULL;

COMMENT ON COLUMN comms_message.matched_contact_id IS
    'Contact this row is attributed to. NULL means the message was staged before identity resolution; only source=''whatsapp'' may write NULL (comms_message_contact_source_check), and the row is attached later by import/rematch. Every eligible/aggregation query excludes NULL rows.';

-- The relaxation is bounded at the database boundary, not by convention: no
-- other source can ever write a contactless row. Named separately from the
-- column so the down migration drops exactly it.
ALTER TABLE comms_message ADD CONSTRAINT comms_message_contact_source_check
    CHECK (matched_contact_id IS NOT NULL OR source = 'whatsapp');

-- Dedup target for unmatched rows. The existing idx_comms_message_dedup
-- (source, external_id, matched_contact_id) never matches a NULL row, so the
-- unmatched case needs its own partial unique index; this is the ON CONFLICT
-- target of UpsertChatCommsMessageUnmatched.
CREATE UNIQUE INDEX idx_comms_message_dedup_unmatched
    ON comms_message(source, external_id)
    WHERE matched_contact_id IS NULL AND deleted_at IS NULL;

-- Discovery scan: unmatched rows grouped by peer within one source
-- (ListUnmatchedCommsPeerCounts) and the peer-scoped retroactive attach.
CREATE INDEX idx_comms_message_unmatched_peer
    ON comms_message(source, peer_handle)
    WHERE matched_contact_id IS NULL AND deleted_at IS NULL;

-- 073's two eligible indexes omitted `matched_contact_id IS NOT NULL` from
-- their partial predicates BECAUSE the column was NOT NULL (073's comment says
-- so explicitly). That justification is now false: without the clause every
-- unmatched WhatsApp row would sit in the eligible indexes permanently, even
-- though no eligible query can ever return it. Rebuild both with the predicate
-- added; everything else about them is carried forward verbatim.
DROP INDEX idx_comms_message_unprocessed_eligible;
DROP INDEX idx_comms_message_stale_claim;

-- Unprocessed-eligible scan (claimed_at IS NULL branch).
CREATE INDEX idx_comms_message_unprocessed_eligible
    ON comms_message(source, matched_contact_id, sent_at)
    WHERE processed_at IS NULL
      AND claimed_at IS NULL
      AND deleted_at IS NULL
      AND matched_contact_id IS NOT NULL;

-- Stale-claim recovery scan (claimed_at IS NOT NULL branch).
CREATE INDEX idx_comms_message_stale_claim
    ON comms_message(source, matched_contact_id, claimed_at)
    WHERE processed_at IS NULL
      AND claimed_at IS NOT NULL
      AND deleted_at IS NULL
      AND matched_contact_id IS NOT NULL;

-- ---------------------------------------------------------------------------
-- 3. whatsapp_history_notification — the durable anchor for the one-shot
-- history protocol.
--
-- This table stores NO message content, on any path. `notification` holds the
-- marshalled waE2E.HistorySyncNotification with InitialHistBootstrapInlinePayload
-- nil'd before marshalling, so it is a media POINTER by construction: media key,
-- direct path, file-enc-SHA256, enc handle. A chunk the server inlines anyway is
-- recorded with disposition='dropped_inline' at phase='projected' and its payload
-- is discarded, never written. The downloaded blob is never persisted either —
-- the backfill clamp runs against it in memory.
--
-- No deleted_at: the row is the only record that a chunk is outstanding, and a
-- soft-delete filter would be a way to lose it. Done rows are a few hundred
-- bytes and carry no message content, so no trim job exists.
-- ---------------------------------------------------------------------------
CREATE TABLE whatsapp_history_notification (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    -- types.MessageID of the protocol message; needed to send its receipt.
    -- UNIQUE makes the recording handler idempotent under WhatsApp's
    -- redelivery (the handler deliberately withholds the ack on failure, so
    -- redelivery is the expected path).
    protocol_msg_id TEXT NOT NULL UNIQUE,
    -- Marshalled waE2E.HistorySyncNotification: a media pointer, never content.
    notification BYTEA NOT NULL,
    sync_type TEXT NOT NULL,
    chunk_order INTEGER NOT NULL DEFAULT 0,
    state TEXT NOT NULL DEFAULT 'pending'
        CHECK (state IN ('pending', 'processing', 'done', 'failed')),
    -- A dropped chunk is a DISPOSITION, not a terminal state: it still runs the
    -- phase machine so its protocol receipt is sent exactly once.
    disposition TEXT NOT NULL DEFAULT 'project'
        CHECK (disposition IN ('project', 'dropped_inline')),
    -- Resume point for a reclaiming worker.
    phase TEXT NOT NULL DEFAULT 'recorded'
        CHECK (phase IN ('recorded', 'downloaded', 'projected', 'acked', 'deleted')),
    -- OldestMsgInChunkTimestampSec, operator triage only.
    oldest_msg_ts TIMESTAMPTZ,
    attempts INTEGER NOT NULL DEFAULT 0,
    -- Fencing token: every transition after a claim must present it, so an
    -- over-running worker cannot clobber its successor.
    claim_token UUID,
    last_error TEXT,
    checkpoint JSONB NOT NULL DEFAULT '{}',
    claimed_at TIMESTAMPTZ,
    received_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    processed_at TIMESTAMPTZ
);

-- Claim seek: ordered scan over the claimable slice only.
CREATE INDEX idx_whatsapp_history_notification_claim
    ON whatsapp_history_notification(state, chunk_order, received_at)
    WHERE state IN ('pending', 'processing');

COMMENT ON COLUMN whatsapp_history_notification.notification IS
    'Marshalled waE2E.HistorySyncNotification with InitialHistBootstrapInlinePayload nil''d: a media pointer (key, direct path, file-enc-SHA256, enc handle). NEVER message content.';
COMMENT ON COLUMN whatsapp_history_notification.disposition IS
    'project: a media-backed chunk to download and project. dropped_inline: the server inlined the bootstrap payload against our request; the payload was discarded un-projected and the row is created at phase=projected so only its receipt and completion remain.';

-- ---------------------------------------------------------------------------
-- 4. whatsapp_chat_config — the persistent group gate, mirroring
-- telegram_chat_config (032). One divergence, deliberate: an unresolved member
-- count fails CLOSED here.
-- ---------------------------------------------------------------------------
CREATE TABLE whatsapp_chat_config (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    chat_jid TEXT NOT NULL,
    chat_title TEXT,
    chat_type TEXT NOT NULL CHECK (chat_type IN ('private', 'group')),
    member_count INTEGER,
    status TEXT NOT NULL DEFAULT 'auto'
        CHECK (status IN ('auto', 'ignored', 'tracked')),
    last_lookup_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ DEFAULT NOW(),
    updated_at TIMESTAMPTZ DEFAULT NOW(),
    UNIQUE (chat_jid)
);

CREATE INDEX idx_whatsapp_chat_config_status ON whatsapp_chat_config(status);

COMMENT ON COLUMN whatsapp_chat_config.member_count IS
    'Group participant count. NULL means UNRESOLVED, and the gate treats unresolved as NOT tracked (fails closed) — the deliberate divergence from telegram_chat_config, whose gate tracks by default on an unknown size.';
