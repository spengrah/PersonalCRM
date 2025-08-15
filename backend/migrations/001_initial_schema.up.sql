-- Personal CRM Initial Schema
-- Migration: 001_initial_schema

-- Enable required extensions
CREATE EXTENSION IF NOT EXISTS "uuid-ossp";
CREATE EXTENSION IF NOT EXISTS vector;

-- Contact table - core entity for people/organizations
CREATE TABLE contact (
    id              UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    full_name       TEXT NOT NULL,
    email           TEXT,
    phone           TEXT,
    location        TEXT,
    birthday        DATE,
    how_met         TEXT,
    cadence         TEXT CHECK (cadence IN ('weekly','monthly','quarterly','biannual','annual')),
    last_contacted  DATE,
    profile_photo   TEXT,
    deleted_at      TIMESTAMPTZ,  -- Soft delete support
    created_at      TIMESTAMPTZ DEFAULT NOW(),
    updated_at      TIMESTAMPTZ DEFAULT NOW()
);

-- Tag table - for categorizing contacts
CREATE TABLE tag (
    id          UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    name        TEXT UNIQUE NOT NULL,
    color       TEXT,  -- Optional color for UI
    created_at  TIMESTAMPTZ DEFAULT NOW()
);

-- Many-to-many relationship between contacts and tags
CREATE TABLE contact_tag (
    contact_id  UUID REFERENCES contact(id) ON DELETE CASCADE,
    tag_id      UUID REFERENCES tag(id) ON DELETE CASCADE,
    created_at  TIMESTAMPTZ DEFAULT NOW(),
    PRIMARY KEY (contact_id, tag_id)
);

-- Note table - for storing notes about contacts
CREATE TABLE note (
    id          UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    contact_id  UUID NOT NULL REFERENCES contact(id) ON DELETE CASCADE,
    body        TEXT NOT NULL,
    category    TEXT,  -- Optional categorization (personal, work, etc.)
    created_at  TIMESTAMPTZ DEFAULT NOW(),
    updated_at  TIMESTAMPTZ DEFAULT NOW()
);

-- Interaction table - for logging interactions with contacts
CREATE TABLE interaction (
    id              UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    contact_id      UUID NOT NULL REFERENCES contact(id) ON DELETE CASCADE,
    type            TEXT NOT NULL CHECK (type IN ('call', 'email', 'meeting', 'text', 'social', 'other')),
    description     TEXT,
    interaction_date TIMESTAMPTZ DEFAULT NOW(),
    created_at      TIMESTAMPTZ DEFAULT NOW()
);

-- Reminder table - for tracking follow-up reminders
CREATE TABLE reminder (
    id              UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    contact_id      UUID NOT NULL REFERENCES contact(id) ON DELETE CASCADE,
    title           TEXT NOT NULL,
    description     TEXT,
    due_date        TIMESTAMPTZ NOT NULL,
    completed       BOOLEAN DEFAULT FALSE,
    completed_at    TIMESTAMPTZ,
    created_at      TIMESTAMPTZ DEFAULT NOW()
);

-- Connection table - for tracking relationships between contacts
CREATE TABLE connection (
    id              UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    contact_a_id    UUID NOT NULL REFERENCES contact(id) ON DELETE CASCADE,
    contact_b_id    UUID NOT NULL REFERENCES contact(id) ON DELETE CASCADE,
    relationship    TEXT,  -- How they know each other
    strength        INTEGER CHECK (strength >= 1 AND strength <= 5),  -- 1-5 scale
    created_at      TIMESTAMPTZ DEFAULT NOW(),
    CONSTRAINT no_self_connection CHECK (contact_a_id != contact_b_id),
    CONSTRAINT unique_connection UNIQUE (contact_a_id, contact_b_id)
);

-- Contact summary table - for caching AI-generated summaries
CREATE TABLE contact_summary (
    id              UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    contact_id      UUID NOT NULL REFERENCES contact(id) ON DELETE CASCADE,
    summary         TEXT NOT NULL,
    generated_at    TIMESTAMPTZ DEFAULT NOW(),
    expires_at      TIMESTAMPTZ,  -- Optional expiration for cache invalidation
    UNIQUE(contact_id)  -- One summary per contact
);

-- Note embeddings table - for vector search (pgvector)
CREATE TABLE note_embedding (
    note_id         UUID PRIMARY KEY REFERENCES note(id) ON DELETE CASCADE,
    embedding       vector(1536),  -- Claude embeddings dimension
    created_at      TIMESTAMPTZ DEFAULT NOW()
);

-- Interaction embeddings table - for vector search
CREATE TABLE interaction_embedding (
    interaction_id  UUID PRIMARY KEY REFERENCES interaction(id) ON DELETE CASCADE,
    embedding       vector(1536),
    created_at      TIMESTAMPTZ DEFAULT NOW()
);

-- Prompt query table - for storing AI chat history
CREATE TABLE prompt_query (
    id              UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    query           TEXT NOT NULL,
    response        TEXT NOT NULL,
    context_used    JSONB,  -- Store metadata about what was retrieved
    created_at      TIMESTAMPTZ DEFAULT NOW()
);

-- Create indexes for performance
-- Contact indexes
CREATE INDEX idx_contact_full_name ON contact(full_name);
CREATE INDEX idx_contact_email ON contact(LOWER(email)) WHERE email IS NOT NULL;
CREATE INDEX idx_contact_deleted_at ON contact(deleted_at) WHERE deleted_at IS NULL;
CREATE INDEX idx_contact_last_contacted ON contact(last_contacted);
CREATE INDEX idx_contact_cadence ON contact(cadence);

-- Tag indexes
CREATE INDEX idx_tag_name ON tag(name);

-- Note indexes
CREATE INDEX idx_note_contact_id ON note(contact_id);
CREATE INDEX idx_note_created_at ON note(contact_id, created_at DESC);
CREATE INDEX idx_note_category ON note(category);

-- Interaction indexes
CREATE INDEX idx_interaction_contact_id ON interaction(contact_id);
CREATE INDEX idx_interaction_date ON interaction(contact_id, interaction_date DESC);
CREATE INDEX idx_interaction_type ON interaction(type);

-- Reminder indexes
CREATE INDEX idx_reminder_contact_id ON reminder(contact_id);
CREATE INDEX idx_reminder_due_date ON reminder(due_date) WHERE completed = FALSE;
CREATE INDEX idx_reminder_completed ON reminder(completed);

-- Connection indexes
CREATE INDEX idx_connection_contact_a ON connection(contact_a_id);
CREATE INDEX idx_connection_contact_b ON connection(contact_b_id);

-- Contact tag indexes
CREATE INDEX idx_contact_tag_contact_id ON contact_tag(contact_id);
CREATE INDEX idx_contact_tag_tag_id ON contact_tag(tag_id);

-- Add full-text search support
CREATE INDEX idx_contact_full_text ON contact USING gin(to_tsvector('english', full_name || ' ' || COALESCE(email, '')));
CREATE INDEX idx_note_full_text ON note USING gin(to_tsvector('english', body));

-- Vector similarity search indexes (will be added later when needed for performance)
-- CREATE INDEX idx_note_embedding_cosine ON note_embedding USING hnsw (embedding vector_cosine_ops);
-- CREATE INDEX idx_interaction_embedding_cosine ON interaction_embedding USING hnsw (embedding vector_cosine_ops);

-- Create a function to update the updated_at timestamp
CREATE OR REPLACE FUNCTION update_updated_at_column()
RETURNS TRIGGER AS $$
BEGIN
    NEW.updated_at = NOW();
    RETURN NEW;
END;
$$ language 'plpgsql';

-- Create triggers for updated_at
CREATE TRIGGER update_contact_updated_at BEFORE UPDATE ON contact
    FOR EACH ROW EXECUTE FUNCTION update_updated_at_column();

CREATE TRIGGER update_note_updated_at BEFORE UPDATE ON note
    FOR EACH ROW EXECUTE FUNCTION update_updated_at_column();
