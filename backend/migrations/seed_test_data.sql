-- Comprehensive Test Data for Personal CRM
-- 100 realistic contacts with birthdays spread across all months
-- 10 diverse reminders/tasks for testing

-- First, clear any existing data
DELETE FROM reminder WHERE id IS NOT NULL;
DELETE FROM interaction WHERE id IS NOT NULL;
DELETE FROM note WHERE id IS NOT NULL;
DELETE FROM contact WHERE id IS NOT NULL;

-- Insert 100 realistic contacts with diverse birthdays
WITH contact_data (id, full_name, email, phone, birthday, last_contacted) AS (
VALUES
-- January birthdays (8 contacts)
(gen_random_uuid(), 'Sarah Johnson', 'sarah.johnson@email.com', '+1-555-0101', '1990-01-15', '2024-12-01 10:00:00'),
(gen_random_uuid(), 'Michael Chen', 'michael.chen@email.com', '+1-555-0102', '1985-01-02', '2024-11-28 15:30:00'),
(gen_random_uuid(), 'Emma Rodriguez', 'emma.rodriguez@email.com', '+1-555-0103', '1992-01-28', '2024-12-15 09:45:00'),
(gen_random_uuid(), 'David Kim', 'david.kim@email.com', NULL, '1988-01-08', NULL),
(gen_random_uuid(), 'Lisa Thompson', 'lisa.thompson@email.com', '+1-555-0105', '1995-01-22', '2024-12-10 14:20:00'),
(gen_random_uuid(), 'Robert Wilson', 'robert.wilson@email.com', '+1-555-0106', '1983-01-17', '2024-11-25 11:10:00'),
(gen_random_uuid(), 'Jennifer Lee', 'jennifer.lee@email.com', NULL, '1991-01-03', '2024-12-08 16:45:00'),
(gen_random_uuid(), 'Christopher Brown', 'chris.brown@email.com', '+1-555-0108', '1987-01-29', '2024-12-05 08:30:00'),

-- February birthdays (8 contacts)
(gen_random_uuid(), 'Amanda Davis', 'amanda.davis@email.com', '+1-555-0201', '1993-02-14', '2024-12-02 12:15:00'),
(gen_random_uuid(), 'James Miller', 'james.miller@email.com', '+1-555-0202', '1986-02-05', '2024-11-30 17:20:00'),
(gen_random_uuid(), 'Ashley Garcia', 'ashley.garcia@email.com', NULL, '1989-02-28', '2024-12-12 13:45:00'),
(gen_random_uuid(), 'Kevin Martinez', 'kevin.martinez@email.com', '+1-555-0204', '1994-02-11', NULL),
(gen_random_uuid(), 'Nicole Anderson', 'nicole.anderson@email.com', '+1-555-0205', '1984-02-20', '2024-12-07 10:30:00'),
(gen_random_uuid(), 'Ryan Taylor', 'ryan.taylor@email.com', '+1-555-0206', '1990-02-03', '2024-11-29 14:55:00'),
(gen_random_uuid(), 'Stephanie White', 'stephanie.white@email.com', NULL, '1992-02-25', '2024-12-14 11:20:00'),
(gen_random_uuid(), 'Brandon Moore', 'brandon.moore@email.com', '+1-555-0208', '1988-02-08', '2024-12-03 15:40:00'),

-- March birthdays (8 contacts)
(gen_random_uuid(), 'Melissa Jackson', 'melissa.jackson@email.com', '+1-555-0301', '1991-03-15', '2024-12-06 09:15:00'),
(gen_random_uuid(), 'Andrew Thompson', 'andrew.thompson@email.com', '+1-555-0302', '1985-03-22', '2024-11-27 16:30:00'),
(gen_random_uuid(), 'Rachel Green', 'rachel.green@email.com', NULL, '1993-03-07', '2024-12-11 12:50:00'),
(gen_random_uuid(), 'Matthew Harris', 'matthew.harris@email.com', '+1-555-0304', '1987-03-30', NULL),
(gen_random_uuid(), 'Samantha Clark', 'samantha.clark@email.com', '+1-555-0305', '1995-03-12', '2024-12-09 14:10:00'),
(gen_random_uuid(), 'Daniel Lewis', 'daniel.lewis@email.com', '+1-555-0306', '1982-03-25', '2024-11-26 10:45:00'),
(gen_random_uuid(), 'Lauren Walker', 'lauren.walker@email.com', NULL, '1990-03-05', '2024-12-13 17:25:00'),
(gen_random_uuid(), 'Tyler Young', 'tyler.young@email.com', '+1-555-0308', '1989-03-18', '2024-12-04 08:55:00'),

-- April birthdays (8 contacts)
(gen_random_uuid(), 'Brittany Hall', 'brittany.hall@email.com', '+1-555-0401', '1992-04-10', '2024-12-01 11:30:00'),
(gen_random_uuid(), 'Justin Allen', 'justin.allen@email.com', '+1-555-0402', '1986-04-27', '2024-11-28 13:20:00'),
(gen_random_uuid(), 'Kimberly Wright', 'kimberly.wright@email.com', NULL, '1988-04-14', '2024-12-15 15:45:00'),
(gen_random_uuid(), 'Joshua King', 'joshua.king@email.com', '+1-555-0404', '1994-04-03', '2024-12-08 09:10:00'),
(gen_random_uuid(), 'Megan Scott', 'megan.scott@email.com', '+1-555-0405', '1983-04-21', NULL),
(gen_random_uuid(), 'Nathan Green', 'nathan.green@email.com', '+1-555-0406', '1991-04-16', '2024-11-25 16:20:00'),
(gen_random_uuid(), 'Courtney Adams', 'courtney.adams@email.com', NULL, '1989-04-29', '2024-12-12 12:35:00'),
(gen_random_uuid(), 'Zachary Baker', 'zachary.baker@email.com', '+1-555-0408', '1987-04-06', '2024-12-07 14:50:00'),

-- May birthdays (8 contacts)
(gen_random_uuid(), 'Danielle Nelson', 'danielle.nelson@email.com', '+1-555-0501', '1993-05-19', '2024-11-30 10:25:00'),
(gen_random_uuid(), 'Aaron Mitchell', 'aaron.mitchell@email.com', '+1-555-0502', '1985-05-08', '2024-12-14 17:15:00'),
(gen_random_uuid(), 'Crystal Perez', 'crystal.perez@email.com', NULL, '1990-05-26', '2024-12-03 11:40:00'),
(gen_random_uuid(), 'Adam Roberts', 'adam.roberts@email.com', '+1-555-0504', '1988-05-13', '2024-12-10 08:20:00'),
(gen_random_uuid(), 'Heather Turner', 'heather.turner@email.com', '+1-555-0505', '1992-05-31', NULL),
(gen_random_uuid(), 'Jordan Phillips', 'jordan.phillips@email.com', '+1-555-0506', '1984-05-04', '2024-11-27 15:55:00'),
(gen_random_uuid(), 'Vanessa Campbell', 'vanessa.campbell@email.com', NULL, '1991-05-17', '2024-12-11 13:30:00'),
(gen_random_uuid(), 'Eric Parker', 'eric.parker@email.com', '+1-555-0508', '1986-05-23', '2024-12-06 16:10:00'),

-- June birthdays (8 contacts)
(gen_random_uuid(), 'Alexis Evans', 'alexis.evans@email.com', '+1-555-0601', '1994-06-12', '2024-12-05 09:45:00'),
(gen_random_uuid(), 'Connor Edwards', 'connor.edwards@email.com', '+1-555-0602', '1987-06-28', '2024-11-29 14:25:00'),
(gen_random_uuid(), 'Destiny Collins', 'destiny.collins@email.com', NULL, '1989-06-09', '2024-12-13 12:15:00'),
(gen_random_uuid(), 'Ian Stewart', 'ian.stewart@email.com', '+1-555-0604', '1993-06-24', '2024-12-08 17:30:00'),
(gen_random_uuid(), 'Jasmine Sanchez', 'jasmine.sanchez@email.com', '+1-555-0605', '1985-06-15', NULL),
(gen_random_uuid(), 'Marcus Morris', 'marcus.morris@email.com', '+1-555-0606', '1990-06-03', '2024-11-26 11:50:00'),
(gen_random_uuid(), 'Paige Rogers', 'paige.rogers@email.com', NULL, '1988-06-21', '2024-12-12 15:20:00'),
(gen_random_uuid(), 'Trevor Reed', 'trevor.reed@email.com', '+1-555-0608', '1992-06-18', '2024-12-04 10:05:00'),

-- July birthdays (8 contacts)
(gen_random_uuid(), 'Cassandra Cook', 'cassandra.cook@email.com', '+1-555-0701', '1991-07-11', '2024-12-02 13:45:00'),
(gen_random_uuid(), 'Derek Morgan', 'derek.morgan@email.com', '+1-555-0702', '1984-07-29', '2024-11-28 08:30:00'),
(gen_random_uuid(), 'Faith Bailey', 'faith.bailey@email.com', NULL, '1986-07-16', '2024-12-15 16:40:00'),
(gen_random_uuid(), 'Garrett Rivera', 'garrett.rivera@email.com', '+1-555-0704', '1989-07-07', '2024-12-09 11:15:00'),
(gen_random_uuid(), 'Holly Cooper', 'holly.cooper@email.com', '+1-555-0705', '1993-07-25', NULL),
(gen_random_uuid(), 'Lance Richardson', 'lance.richardson@email.com', '+1-555-0706', '1987-07-02', '2024-11-25 14:35:00'),
(gen_random_uuid(), 'Monica Cox', 'monica.cox@email.com', NULL, '1990-07-20', '2024-12-11 09:50:00'),
(gen_random_uuid(), 'Preston Ward', 'preston.ward@email.com', '+1-555-0708', '1985-07-14', '2024-12-07 17:05:00'),

-- August birthdays (9 contacts) - including today's date for testing
(gen_random_uuid(), 'Giovanni Gabriele', 'giovanni.gabriele@email.com', '+1-555-0801', '1983-08-18', '2024-12-01 12:00:00'),
(gen_random_uuid(), 'Blake Torres', 'blake.torres@email.com', '+1-555-0802', '1988-08-06', '2024-11-30 15:25:00'),
(gen_random_uuid(), 'Chloe Peterson', 'chloe.peterson@email.com', NULL, '1992-08-23', '2024-12-14 10:40:00'),
(gen_random_uuid(), 'Devin Gray', 'devin.gray@email.com', '+1-555-0804', '1986-08-31', '2024-12-06 14:15:00'),
(gen_random_uuid(), 'Gabrielle Ramirez', 'gabrielle.ramirez@email.com', '+1-555-0805', '1994-08-12', NULL),
(gen_random_uuid(), 'Hunter James', 'hunter.james@email.com', '+1-555-0806', '1989-08-28', '2024-11-27 16:55:00'),
(gen_random_uuid(), 'Jenna Watson', 'jenna.watson@email.com', NULL, '1991-08-05', '2024-12-13 11:30:00'),
(gen_random_uuid(), 'Kaleb Brooks', 'kaleb.brooks@email.com', '+1-555-0808', '1987-08-19', '2024-12-08 13:20:00'),
(gen_random_uuid(), 'Lindsey Kelly', 'lindsey.kelly@email.com', '+1-555-0809', '1985-08-14', '2024-12-03 15:45:00'),

-- September birthdays (8 contacts)
(gen_random_uuid(), 'Mason Sanders', 'mason.sanders@email.com', '+1-555-0901', '1990-09-10', '2024-12-05 09:25:00'),
(gen_random_uuid(), 'Natalie Price', 'natalie.price@email.com', '+1-555-0902', '1983-09-27', '2024-11-29 17:40:00'),
(gen_random_uuid(), 'Owen Bennett', 'owen.bennett@email.com', NULL, '1988-09-15', '2024-12-12 12:55:00'),
(gen_random_uuid(), 'Quinn Wood', 'quinn.wood@email.com', '+1-555-0904', '1992-09-03', '2024-12-10 08:45:00'),
(gen_random_uuid(), 'Riley Barnes', 'riley.barnes@email.com', '+1-555-0905', '1986-09-21', NULL),
(gen_random_uuid(), 'Sierra Ross', 'sierra.ross@email.com', '+1-555-0906', '1991-09-18', '2024-11-26 14:20:00'),
(gen_random_uuid(), 'Tanner Henderson', 'tanner.henderson@email.com', NULL, '1989-09-06', '2024-12-14 16:35:00'),
(gen_random_uuid(), 'Uma Coleman', 'uma.coleman@email.com', '+1-555-0908', '1994-09-24', '2024-12-07 10:15:00'),

-- October birthdays (8 contacts)
(gen_random_uuid(), 'Victor Jenkins', 'victor.jenkins@email.com', '+1-555-1001', '1987-10-13', '2024-12-04 11:50:00'),
(gen_random_uuid(), 'Wendy Perry', 'wendy.perry@email.com', '+1-555-1002', '1985-10-30', '2024-11-28 13:10:00'),
(gen_random_uuid(), 'Xavier Powell', 'xavier.powell@email.com', NULL, '1990-10-08', '2024-12-15 15:30:00'),
(gen_random_uuid(), 'Yolanda Long', 'yolanda.long@email.com', '+1-555-1004', '1988-10-25', '2024-12-11 09:20:00'),
(gen_random_uuid(), 'Zoe Patterson', 'zoe.patterson@email.com', '+1-555-1005', '1993-10-16', NULL),
(gen_random_uuid(), 'Adrian Hughes', 'adrian.hughes@email.com', '+1-555-1006', '1984-10-02', '2024-11-25 17:45:00'),
(gen_random_uuid(), 'Brianna Flores', 'brianna.flores@email.com', NULL, '1991-10-19', '2024-12-13 12:40:00'),
(gen_random_uuid(), 'Camden Washington', 'camden.washington@email.com', '+1-555-1008', '1989-10-07', '2024-12-09 14:55:00'),

-- November birthdays (8 contacts)
(gen_random_uuid(), 'Delilah Butler', 'delilah.butler@email.com', '+1-555-1101', '1992-11-14', '2024-12-06 10:35:00'),
(gen_random_uuid(), 'Edgar Simmons', 'edgar.simmons@email.com', '+1-555-1102', '1986-11-28', '2024-11-30 16:20:00'),
(gen_random_uuid(), 'Fiona Foster', 'fiona.foster@email.com', NULL, '1989-11-09', '2024-12-12 08:50:00'),
(gen_random_uuid(), 'Graham Gonzales', 'graham.gonzales@email.com', '+1-555-1104', '1994-11-22', '2024-12-08 13:15:00'),
(gen_random_uuid(), 'Hazel Bryant', 'hazel.bryant@email.com', '+1-555-1105', '1983-11-05', NULL),
(gen_random_uuid(), 'Ivan Alexander', 'ivan.alexander@email.com', '+1-555-1106', '1990-11-18', '2024-11-27 15:05:00'),
(gen_random_uuid(), 'Jade Russell', 'jade.russell@email.com', NULL, '1988-11-26', '2024-12-14 11:25:00'),
(gen_random_uuid(), 'Knox Griffin', 'knox.griffin@email.com', '+1-555-1108', '1991-11-11', '2024-12-05 17:10:00'),

-- December birthdays (8 contacts)
(gen_random_uuid(), 'Luna Diaz', 'luna.diaz@email.com', '+1-555-1201', '1987-12-17', '2024-12-03 09:30:00'),
(gen_random_uuid(), 'Miles Hayes', 'miles.hayes@email.com', '+1-555-1202', '1985-12-04', '2024-11-29 14:45:00'),
(gen_random_uuid(), 'Nora Myers', 'nora.myers@email.com', NULL, '1993-12-23', '2024-12-11 16:15:00'),
(gen_random_uuid(), 'Oscar Ford', 'oscar.ford@email.com', '+1-555-1204', '1989-12-31', '2024-12-07 12:25:00'),
(gen_random_uuid(), 'Piper Hamilton', 'piper.hamilton@email.com', '+1-555-1205', '1992-12-08', NULL),
(gen_random_uuid(), 'Quincy Graham', 'quincy.graham@email.com', '+1-555-1206', '1984-12-20', '2024-11-26 10:55:00'),
(gen_random_uuid(), 'Raven Sullivan', 'raven.sullivan@email.com', NULL, '1990-12-12', '2024-12-15 14:40:00'),
(gen_random_uuid(), 'Sage Wallace', 'sage.wallace@email.com', '+1-555-1208', '1986-12-29', '2024-12-10 08:05:00')
)
INSERT INTO contact (id, full_name, birthday, last_contacted, created_at, updated_at)
SELECT id, full_name, birthday, last_contacted, NOW(), NOW()
FROM contact_data;

INSERT INTO contact_method (contact_id, type, value, is_primary)
SELECT id, 'email_personal', email, TRUE
FROM contact_data
WHERE email IS NOT NULL AND btrim(email) <> '';

INSERT INTO contact_method (contact_id, type, value, is_primary)
SELECT id, 'phone', phone, CASE WHEN email IS NULL OR btrim(email) = '' THEN TRUE ELSE FALSE END
FROM contact_data
WHERE phone IS NOT NULL AND btrim(phone) <> '';

-- Insert 10 diverse reminders/tasks
INSERT INTO reminder (id, contact_id, title, description, due_date, completed, created_at) VALUES

-- Contact-linked reminders (7)
(gen_random_uuid(), (SELECT id FROM contact WHERE full_name = 'Sarah Johnson'), 'Follow up on job interview', 'Check how her tech interview went at the startup', '2024-12-20 14:00:00', false, NOW()),
(gen_random_uuid(), (SELECT id FROM contact WHERE full_name = 'Michael Chen'), 'Send holiday card', 'Remember to send a personalized holiday card', '2024-12-24 10:00:00', false, NOW()),
(gen_random_uuid(), (SELECT id FROM contact WHERE full_name = 'Emma Rodriguez'), 'Check on new baby', 'Ask how things are going with the newborn', '2024-12-22 16:00:00', false, NOW()),
(gen_random_uuid(), (SELECT id FROM contact WHERE full_name = 'Lisa Thompson'), 'Coffee catch-up', 'Monthly coffee to stay in touch', '2024-12-25 11:00:00', false, NOW()),
(gen_random_uuid(), (SELECT id FROM contact WHERE full_name = 'Jennifer Lee'), 'Wedding planning check-in', 'See how wedding planning is progressing', '2024-12-19 18:00:00', false, NOW()),
(gen_random_uuid(), (SELECT id FROM contact WHERE full_name = 'Amanda Davis'), 'Congratulate on promotion', 'Celebrate her new role as senior manager', '2024-12-21 09:00:00', false, NOW()),
(gen_random_uuid(), (SELECT id FROM contact WHERE full_name = 'James Miller'), 'Golf game reminder', 'Quarterly golf meetup with James', '2025-01-15 08:00:00', false, NOW()),

-- Standalone reminders (3)
(gen_random_uuid(), NULL, 'Buy Christmas gifts', 'Finish shopping for holiday presents', '2024-12-23 12:00:00', false, NOW()),
(gen_random_uuid(), NULL, 'Year-end tax prep', 'Gather documents for tax preparation', '2024-12-31 17:00:00', false, NOW()),
(gen_random_uuid(), NULL, 'Review CRM system', 'Monthly review of contact management and relationships', '2025-01-01 10:00:00', false, NOW());
