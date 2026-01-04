-- External Sync Infrastructure
-- Tracks sync state and audit logs for external data sources (Gmail, iMessage, Telegram, etc.)

-- external_sync_state: tracks sync state per source/account
CREATE TABLE external_sync_state (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    source TEXT NOT NULL,                    -- 'gmail', 'imessage', 'telegram', 'gcal', 'gcontacts', 'icloud_contacts'
    account_id TEXT,                         -- NULL for local sources (iMessage), email for Google accounts
    enabled BOOLEAN NOT NULL DEFAULT TRUE,
    status TEXT NOT NULL DEFAULT 'idle' CHECK (status IN ('idle', 'syncing', 'error', 'disabled')),
    strategy TEXT NOT NULL DEFAULT 'contact_driven' CHECK (strategy IN ('contact_driven', 'fetch_all', 'fetch_filtered')),
    last_sync_at TIMESTAMPTZ,
    last_successful_sync_at TIMESTAMPTZ,
    next_sync_at TIMESTAMPTZ,
    sync_cursor TEXT,                        -- Source-specific cursor (e.g., Gmail historyId, message timestamp)
    error_message TEXT,
    error_count INTEGER NOT NULL DEFAULT 0,
    metadata JSONB DEFAULT '{}',             -- Source-specific config (e.g., scopes, filters)
    created_at TIMESTAMPTZ DEFAULT NOW(),
    updated_at TIMESTAMPTZ DEFAULT NOW()
);

-- external_sync_log: audit log of sync runs
CREATE TABLE external_sync_log (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    sync_state_id UUID NOT NULL REFERENCES external_sync_state(id) ON DELETE CASCADE,
    source TEXT NOT NULL,
    account_id TEXT,
    started_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    completed_at TIMESTAMPTZ,
    status TEXT NOT NULL CHECK (status IN ('running', 'success', 'partial', 'error')),
    items_processed INTEGER DEFAULT 0,
    items_matched INTEGER DEFAULT 0,         -- How many mapped to CRM contacts
    items_created INTEGER DEFAULT 0,         -- How many new records created
    error_message TEXT,
    metadata JSONB DEFAULT '{}',             -- Stats, debug info
    created_at TIMESTAMPTZ DEFAULT NOW()
);

-- Multi-account support: unique per source+account combination
-- COALESCE handles NULL account_id for local sources
CREATE UNIQUE INDEX idx_external_sync_state_source_account ON external_sync_state(source, COALESCE(account_id, ''));

-- Indexes for efficient queries
CREATE INDEX idx_external_sync_state_source ON external_sync_state(source);
CREATE INDEX idx_external_sync_state_status ON external_sync_state(status);
CREATE INDEX idx_external_sync_state_next_sync ON external_sync_state(next_sync_at) WHERE enabled = TRUE;
CREATE INDEX idx_external_sync_state_enabled ON external_sync_state(enabled);

CREATE INDEX idx_external_sync_log_sync_state_id ON external_sync_log(sync_state_id);
CREATE INDEX idx_external_sync_log_source ON external_sync_log(source);
CREATE INDEX idx_external_sync_log_started_at ON external_sync_log(started_at DESC);
CREATE INDEX idx_external_sync_log_status ON external_sync_log(status);
