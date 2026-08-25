CREATE INDEX idx_comms_message_interaction_id
    ON comms_message (interaction_id)
    WHERE interaction_id IS NOT NULL;

CREATE INDEX idx_telegram_message_interaction_id
    ON telegram_message (interaction_id)
    WHERE interaction_id IS NOT NULL;

CREATE INDEX idx_messages_message_interaction_id
    ON messages_message (interaction_id)
    WHERE interaction_id IS NOT NULL;

CREATE INDEX idx_phone_call_interaction_id
    ON phone_call (interaction_id)
    WHERE interaction_id IS NOT NULL;
