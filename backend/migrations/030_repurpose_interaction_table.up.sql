-- Drop old unused interaction table and related embedding table
DROP TABLE IF EXISTS interaction_embedding;
DROP TABLE IF EXISTS interaction;

-- Create new interaction table for unified interaction tracking
CREATE TABLE interaction (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    contact_id UUID NOT NULL REFERENCES contact(id) ON DELETE CASCADE,
    source TEXT NOT NULL,
    source_ref TEXT,
    occurred_at TIMESTAMPTZ NOT NULL,
    description TEXT,
    created_at TIMESTAMPTZ DEFAULT NOW(),
    deleted_at TIMESTAMPTZ
);

-- Indexes
CREATE INDEX idx_interaction_contact_id ON interaction(contact_id);
CREATE INDEX idx_interaction_occurred_at ON interaction(occurred_at);
CREATE INDEX idx_interaction_contact_occurred ON interaction(contact_id, occurred_at);
CREATE INDEX idx_interaction_deleted_at ON interaction(deleted_at) WHERE deleted_at IS NULL;
CREATE UNIQUE INDEX idx_interaction_source_ref ON interaction(contact_id, source, source_ref) WHERE source_ref IS NOT NULL AND deleted_at IS NULL;

-- Source constraint (extend as new integrations are added)
ALTER TABLE interaction ADD CONSTRAINT interaction_source_check
    CHECK (source IN ('manual', 'gcal', 'todoist'));
