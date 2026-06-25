-- 072_drop_dormant_tables.down.sql
-- Re-creates the four dormant tables empty. Rollback-safety only; historical
-- data is not restored. Table bodies mirror 001_initial_schema.
BEGIN;

CREATE TABLE connection (
    id              UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    contact_a_id    UUID NOT NULL REFERENCES contact(id) ON DELETE CASCADE,
    contact_b_id    UUID NOT NULL REFERENCES contact(id) ON DELETE CASCADE,
    relationship    TEXT,
    strength        INTEGER CHECK (strength >= 1 AND strength <= 5),
    created_at      TIMESTAMPTZ DEFAULT NOW(),
    CONSTRAINT no_self_connection CHECK (contact_a_id != contact_b_id),
    CONSTRAINT unique_connection UNIQUE (contact_a_id, contact_b_id)
);
CREATE INDEX idx_connection_contact_a ON connection(contact_a_id);
CREATE INDEX idx_connection_contact_b ON connection(contact_b_id);

CREATE TABLE contact_summary (
    id              UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    contact_id      UUID NOT NULL REFERENCES contact(id) ON DELETE CASCADE,
    summary         TEXT NOT NULL,
    generated_at    TIMESTAMPTZ DEFAULT NOW(),
    expires_at      TIMESTAMPTZ,
    UNIQUE(contact_id)
);

CREATE TABLE note_embedding (
    note_id         UUID PRIMARY KEY REFERENCES note(id) ON DELETE CASCADE,
    embedding       vector(1536),
    created_at      TIMESTAMPTZ DEFAULT NOW()
);
-- (the HNSW index is left commented-out to mirror 001 exactly)

CREATE TABLE prompt_query (
    id              UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    query           TEXT NOT NULL,
    response        TEXT NOT NULL,
    context_used    JSONB,
    created_at      TIMESTAMPTZ DEFAULT NOW()
);

COMMIT;
