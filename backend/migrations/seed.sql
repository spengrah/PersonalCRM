-- Seed data for Personal CRM
-- This file creates demo data for development and testing

-- Insert demo tags
INSERT INTO tag (id, name, color) VALUES
    (uuid_generate_v4(), 'friend', '#3B82F6'),
    (uuid_generate_v4(), 'family', '#EF4444'),
    (uuid_generate_v4(), 'colleague', '#10B981'),
    (uuid_generate_v4(), 'client', '#F59E0B'),
    (uuid_generate_v4(), 'mentor', '#8B5CF6'),
    (uuid_generate_v4(), 'neighbor', '#06B6D4')
ON CONFLICT (name) DO NOTHING;

-- Insert demo contacts
WITH contact_data AS (
    SELECT 
        uuid_generate_v4() as id,
        'John Doe' as full_name,
        'john.doe@example.com' as email,
        '+1-555-0123' as phone,
        'San Francisco, CA' as location,
        '1985-03-15'::date as birthday,
        'Met at a tech conference' as how_met,
        'quarterly' as cadence,
        NOW() - INTERVAL '1 day' * (RANDOM() * 30)::int as last_contacted  -- Random last contact within 30 days
    UNION ALL
    SELECT 
        uuid_generate_v4(),
        'Jane Smith',
        'jane.smith@example.com',
        '+1-555-0124',
        'New York, NY',
        '1990-07-22'::date,
        'Former colleague',
        'monthly',
        NOW() - INTERVAL '1 day' * (RANDOM() * 30)::int
    UNION ALL
    SELECT 
        uuid_generate_v4(),
        'Mike Johnson',
        'mike.johnson@example.com',
        '+1-555-0125',
        'Austin, TX',
        '1988-11-08'::date,
        'University friend',
        'biannual',
        NOW() - INTERVAL '1 day' * (RANDOM() * 30)::int
    UNION ALL
    SELECT 
        uuid_generate_v4(),
        'Sarah Wilson',
        NULL,
        '+1-555-0126',
        'Seattle, WA',
        NULL,
        'Neighbor',
        'monthly',
        NOW() - INTERVAL '1 day' * (RANDOM() * 30)::int
    UNION ALL
    SELECT 
        uuid_generate_v4(),
        'David Brown',
        'david.brown@company.com',
        NULL,
        'Boston, MA',
        '1982-05-30'::date,
        'Client introduction',
        'weekly',
        NOW() - INTERVAL '1 day' * (RANDOM() * 30)::int
)
INSERT INTO contact (id, full_name, location, birthday, how_met, cadence, last_contacted)
SELECT 
    id, full_name, location, birthday, how_met, cadence, last_contacted
FROM contact_data;

INSERT INTO contact_method (contact_id, type, value, value_normalized, is_primary)
SELECT id, 'email', email, lower(btrim(email)), TRUE
FROM contact_data
WHERE email IS NOT NULL AND btrim(email) <> '';

INSERT INTO contact_method (contact_id, type, value, value_normalized, is_primary)
SELECT
    id,
    'phone',
    phone,
    CASE
        WHEN btrim(phone) = '' THEN ''
        WHEN btrim(phone) ~ '^\\+' THEN '+' || regexp_replace(btrim(phone), '\\D', '', 'g')
        ELSE
            CASE
                WHEN length(regexp_replace(btrim(phone), '\\D', '', 'g')) = 10 THEN
                    '+1' || regexp_replace(btrim(phone), '\\D', '', 'g')
                WHEN length(regexp_replace(btrim(phone), '\\D', '', 'g')) = 11
                    AND left(regexp_replace(btrim(phone), '\\D', '', 'g'), 1) = '1' THEN
                    '+' || regexp_replace(btrim(phone), '\\D', '', 'g')
                ELSE
                    '+' || regexp_replace(btrim(phone), '\\D', '', 'g')
            END
    END,
    CASE WHEN email IS NULL OR btrim(email) = '' THEN TRUE ELSE FALSE END
FROM contact_data
WHERE phone IS NOT NULL AND btrim(phone) <> '';

-- Get contact and tag IDs for relationships
WITH contact_ids AS (
    SELECT id, full_name FROM contact WHERE full_name IN ('John Doe', 'Jane Smith', 'Mike Johnson', 'Sarah Wilson', 'David Brown')
),
tag_ids AS (
    SELECT id, name FROM tag WHERE name IN ('friend', 'family', 'colleague', 'client', 'mentor', 'neighbor')
)
-- Add contact-tag relationships
INSERT INTO contact_tag (contact_id, tag_id)
SELECT c.id, t.id FROM contact_ids c, tag_ids t
WHERE 
    (c.full_name = 'John Doe' AND t.name IN ('friend', 'colleague')) OR
    (c.full_name = 'Jane Smith' AND t.name IN ('colleague', 'mentor')) OR
    (c.full_name = 'Mike Johnson' AND t.name IN ('friend')) OR
    (c.full_name = 'Sarah Wilson' AND t.name IN ('neighbor')) OR
    (c.full_name = 'David Brown' AND t.name IN ('client'));

-- Insert demo notes
WITH contact_notes AS (
    SELECT 
        c.id as contact_id,
        n.body,
        n.category
    FROM contact c
    CROSS JOIN (
        VALUES 
            ('Had a great conversation about the new project. Very interested in collaboration.', 'work'),
            ('Mentioned they might be moving to a new city next year.', 'personal'),
            ('Shared some great book recommendations on productivity.', 'personal'),
            ('Discussed potential partnership opportunities.', 'work'),
            ('Invited me to their birthday party next month.', 'personal')
    ) AS n(body, category)
    WHERE c.full_name IN ('John Doe', 'Jane Smith', 'Mike Johnson')
    LIMIT 15  -- 3 contacts × 5 notes each
)
INSERT INTO note (contact_id, body, category, created_at)
SELECT 
    contact_id, 
    body, 
    category,
    NOW() - INTERVAL '1 day' * (RANDOM() * 60)::int  -- Random creation within 60 days
FROM contact_notes;

-- Insert demo interactions
WITH contact_interactions AS (
    SELECT 
        c.id as contact_id,
        i.type,
        i.description,
        NOW() - INTERVAL '1 day' * (RANDOM() * 14)::int as interaction_date
    FROM contact c
    CROSS JOIN (
        VALUES 
            ('call', 'Weekly check-in call'),
            ('email', 'Sent project proposal'),
            ('meeting', 'Coffee meeting downtown'),
            ('text', 'Quick hello message'),
            ('email', 'Follow-up on previous discussion')
    ) AS i(type, description)
    WHERE c.full_name IN ('John Doe', 'Jane Smith', 'David Brown')
    LIMIT 12  -- 3 contacts × 4 interactions each
)
INSERT INTO interaction (contact_id, type, description, interaction_date)
SELECT contact_id, type, description, interaction_date
FROM contact_interactions;

-- Insert demo reminders
WITH contact_reminders AS (
    SELECT 
        c.id as contact_id,
        r.title,
        r.description,
        r.due_date
    FROM contact c
    CROSS JOIN (
        VALUES 
            ('Follow up on project proposal', 'Check if they have reviewed the proposal and provide any clarifications needed', NOW() + INTERVAL '3 days'),
            ('Birthday reminder', 'Send birthday wishes and maybe a small gift', NOW() + INTERVAL '15 days'),
            ('Quarterly check-in', 'Schedule quarterly catch-up call to maintain relationship', NOW() + INTERVAL '30 days'),
            ('Book recommendation follow-up', 'Ask if they enjoyed the book I recommended last month', NOW() + INTERVAL '7 days')
    ) AS r(title, description, due_date)
    WHERE c.full_name IN ('John Doe', 'Jane Smith', 'Mike Johnson', 'Sarah Wilson')
    LIMIT 8  -- 4 contacts × 2 reminders each
)
INSERT INTO reminder (contact_id, title, description, due_date)
SELECT contact_id, title, description, due_date
FROM contact_reminders;
