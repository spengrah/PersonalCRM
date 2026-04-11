-- Allow 'dismissed' as a terminal state for contact_task.
-- Used for follow-up tasks that the user has dismissed via Todoist (deletion,
-- label removal, or deadline removal on a follow_up kind) without wanting to
-- record a new outbound interaction or create a successor follow-up.
ALTER TABLE contact_task DROP CONSTRAINT contact_task_state_check;
ALTER TABLE contact_task ADD CONSTRAINT contact_task_state_check
    CHECK (state IN ('managed', 'unmanaged', 'completed', 'dismissed'));
