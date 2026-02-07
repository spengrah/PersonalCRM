-- Drop new interaction table
DROP TABLE IF EXISTS interaction;

-- Restore original interaction table from migration 001
CREATE TABLE interaction (
    id              UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    contact_id      UUID NOT NULL REFERENCES contact(id) ON DELETE CASCADE,
    type            TEXT NOT NULL CHECK (type IN ('call', 'email', 'meeting', 'text', 'social', 'other')),
    description     TEXT,
    interaction_date TIMESTAMPTZ DEFAULT NOW(),
    created_at      TIMESTAMPTZ DEFAULT NOW()
);

-- Restore original interaction_embedding table from migration 001
CREATE TABLE interaction_embedding (
    interaction_id  UUID PRIMARY KEY REFERENCES interaction(id) ON DELETE CASCADE,
    embedding       vector(1536),
    created_at      TIMESTAMPTZ DEFAULT NOW()
);
