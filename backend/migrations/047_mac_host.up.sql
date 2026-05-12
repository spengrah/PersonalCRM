-- Mac host registration + pairing tokens.
--
-- mac_host: one row per paired Mac daemon. v1 enforces a single
-- non-revoked host via a partial unique index. Multi-host requires
-- resolving content-hash source_id collisions across hosts and is
-- explicitly deferred to a later phase.
--
-- mac_host_pairing_token: short-lived single-use tokens minted by the
-- admin UI and consumed by the daemon's first /api/v1/host call. The
-- daemon never sees the global API key; pairing tokens are the only
-- bootstrap path.

CREATE TABLE mac_host (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    hostname TEXT NOT NULL,
    daemon_version TEXT NOT NULL DEFAULT '',
    protocol_version INTEGER NOT NULL DEFAULT 1,
    last_heartbeat_at TIMESTAMPTZ,
    permissions JSONB NOT NULL DEFAULT '{}',
    source_health JSONB NOT NULL DEFAULT '{}',
    cursor_epoch BIGINT NOT NULL DEFAULT 1,
    api_key_hash TEXT NOT NULL,
    api_key_revoked_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ DEFAULT NOW(),
    updated_at TIMESTAMPTZ DEFAULT NOW()
);

-- v1 enforces single non-revoked host. The index expression `(true)`
-- evaluates to the same value for every row, which combined with the
-- partial predicate yields "at most one row where api_key_revoked_at
-- IS NULL". A second pairing attempt while another host is active
-- surfaces a unique-violation that the handler maps to 409.
CREATE UNIQUE INDEX idx_mac_host_singleton ON mac_host ((true))
    WHERE api_key_revoked_at IS NULL;

CREATE INDEX idx_mac_host_last_heartbeat ON mac_host(last_heartbeat_at);

CREATE TRIGGER update_mac_host_updated_at BEFORE UPDATE ON mac_host
    FOR EACH ROW EXECUTE FUNCTION update_updated_at_column();

CREATE TABLE mac_host_pairing_token (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    token_hash TEXT NOT NULL UNIQUE,
    expires_at TIMESTAMPTZ NOT NULL,
    consumed_at TIMESTAMPTZ,
    consumed_host_id UUID REFERENCES mac_host(id) ON DELETE SET NULL,
    created_at TIMESTAMPTZ DEFAULT NOW()
);

-- Index over unconsumed tokens for the janitor's WHERE clause and for
-- the consume-by-hash lookup path.
CREATE INDEX idx_mac_host_pairing_token_active
    ON mac_host_pairing_token (expires_at)
    WHERE consumed_at IS NULL;
