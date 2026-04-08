-- Telegram MTProto update state (pts/qts/seq for gap recovery)
CREATE TABLE telegram_update_state (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    user_id BIGINT NOT NULL,
    pts INTEGER NOT NULL DEFAULT 0,
    qts INTEGER NOT NULL DEFAULT 0,
    seq INTEGER NOT NULL DEFAULT 0,
    date INTEGER NOT NULL DEFAULT 0,
    updated_at TIMESTAMPTZ DEFAULT NOW(),
    UNIQUE (user_id)
);

-- Channel-specific pts state
CREATE TABLE telegram_channel_state (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    channel_id BIGINT NOT NULL,
    pts INTEGER NOT NULL DEFAULT 0,
    access_hash BIGINT NOT NULL DEFAULT 0,
    updated_at TIMESTAMPTZ DEFAULT NOW(),
    UNIQUE (channel_id)
);

-- Group chat configuration
CREATE TABLE telegram_chat_config (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    telegram_chat_id BIGINT NOT NULL,
    chat_title TEXT,
    chat_type TEXT NOT NULL CHECK (chat_type IN ('private', 'group', 'supergroup')),
    member_count INTEGER,
    status TEXT NOT NULL DEFAULT 'auto'
        CHECK (status IN ('auto', 'ignored', 'tracked')),
    backfill_cursor INTEGER,
    backfill_complete BOOLEAN NOT NULL DEFAULT FALSE,
    created_at TIMESTAMPTZ DEFAULT NOW(),
    updated_at TIMESTAMPTZ DEFAULT NOW(),
    UNIQUE (telegram_chat_id)
);

CREATE INDEX idx_telegram_chat_config_status ON telegram_chat_config(status);

-- Telegram session storage (encrypted MTProto auth keys)
CREATE TABLE telegram_session (
    id INTEGER PRIMARY KEY DEFAULT 1 CHECK (id = 1),
    session_data_encrypted BYTEA NOT NULL,
    encryption_nonce BYTEA NOT NULL,
    phone_number TEXT,
    telegram_user_id BIGINT,
    username TEXT,
    auth_state TEXT NOT NULL DEFAULT 'disconnected'
        CHECK (auth_state IN ('disconnected', 'connected')),
    created_at TIMESTAMPTZ DEFAULT NOW(),
    updated_at TIMESTAMPTZ DEFAULT NOW()
);

-- Telegram message staging (used in Phase 3+, but table created now)
CREATE TABLE telegram_message (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    telegram_message_id INTEGER NOT NULL,
    telegram_chat_id BIGINT NOT NULL,
    chat_type TEXT NOT NULL CHECK (chat_type IN ('private', 'group', 'supergroup')),
    chat_title TEXT,
    message_text TEXT,
    message_type TEXT NOT NULL DEFAULT 'text'
        CHECK (message_type IN ('text', 'photo', 'voice', 'video', 'document', 'sticker', 'other')),
    sent_at TIMESTAMPTZ NOT NULL,
    edited_at TIMESTAMPTZ,
    is_outgoing BOOLEAN NOT NULL,
    reply_to_msg_id INTEGER,
    peer_user_id BIGINT,
    peer_username TEXT,
    peer_first_name TEXT,
    peer_last_name TEXT,
    peer_phone TEXT,
    matched_contact_id UUID REFERENCES contact(id) ON DELETE SET NULL,
    interaction_id UUID REFERENCES interaction(id) ON DELETE SET NULL,
    processed_at TIMESTAMPTZ,
    deleted_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ DEFAULT NOW(),
    UNIQUE (telegram_chat_id, telegram_message_id)
);

CREATE INDEX idx_telegram_message_contact ON telegram_message(matched_contact_id)
    WHERE matched_contact_id IS NOT NULL;
CREATE INDEX idx_telegram_message_sent_at ON telegram_message(sent_at DESC);
CREATE INDEX idx_telegram_message_chat_msg ON telegram_message(telegram_chat_id, telegram_message_id DESC);
CREATE INDEX idx_telegram_message_unprocessed ON telegram_message(matched_contact_id, sent_at)
    WHERE processed_at IS NULL AND matched_contact_id IS NOT NULL;
CREATE INDEX idx_telegram_message_peer ON telegram_message(peer_user_id)
    WHERE matched_contact_id IS NULL AND peer_user_id IS NOT NULL;
