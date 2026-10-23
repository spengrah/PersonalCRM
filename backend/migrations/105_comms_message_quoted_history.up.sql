-- An email's quoted history, stored apart from its body (ING-047).
--
-- A message that starts its Gmail thread in the mailbox, such as an
-- introduction received fresh, carries earlier mail the mailbox holds no other
-- copy of. Its body is its own text, and the quoted mail it carries is kept
-- here, capped, so the thread page can show it while the decision model reads
-- the body alone. NULL means the message carries none: every other email, and
-- every other source, stores none. Every existing row stores none until
-- crm-admin --trim-email-quotes splits the stored bodies.
ALTER TABLE comms_message
    ADD COLUMN quoted_history TEXT,
    ADD CONSTRAINT comms_message_quoted_history_check CHECK (
        quoted_history IS NULL OR (source = 'email' AND quoted_history <> ''));
