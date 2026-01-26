-- Drop contact_task table and related objects
DROP TRIGGER IF EXISTS contact_task_updated_at ON contact_task;
DROP TABLE IF EXISTS contact_task;
