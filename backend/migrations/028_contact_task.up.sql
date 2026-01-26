-- Contact Task table for tracking external task provider links (Todoist, etc.)
-- Each contact can have at most one managed task per provider+kind combination

CREATE TABLE contact_task (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    contact_id UUID NOT NULL REFERENCES contact(id) ON DELETE CASCADE,
    provider TEXT NOT NULL,                    -- 'todoist'
    kind TEXT NOT NULL,                        -- 'cadence'
    external_task_id TEXT NOT NULL,            -- Todoist task ID
    state TEXT NOT NULL DEFAULT 'managed',     -- 'managed', 'unmanaged'
    metadata JSONB DEFAULT '{}',               -- future: content, due_date for one-off tasks
    created_at TIMESTAMPTZ DEFAULT NOW(),
    updated_at TIMESTAMPTZ DEFAULT NOW(),

    -- At most one task per contact+provider+kind combination
    CONSTRAINT unique_contact_provider_kind UNIQUE (contact_id, provider, kind)
);

-- Index for looking up by contact
CREATE INDEX idx_contact_task_contact_id ON contact_task(contact_id);

-- Index for looking up by external task ID (for processing Todoist sync responses)
CREATE INDEX idx_contact_task_external_task_id ON contact_task(provider, external_task_id);

-- Index for listing all managed tasks by provider
CREATE INDEX idx_contact_task_provider_state ON contact_task(provider, state);

-- Trigger to update updated_at on changes
CREATE TRIGGER contact_task_updated_at
    BEFORE UPDATE ON contact_task
    FOR EACH ROW
    EXECUTE FUNCTION update_updated_at_column();
