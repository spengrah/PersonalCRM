BEGIN;

DROP TABLE judgment_outcome;
DROP TABLE judgment_incomplete;
DROP TABLE judgment_appearance;
DROP TABLE judgment;
DROP TABLE judgment_example;
DROP TABLE judgment_run;

DELETE FROM node WHERE type = 'judgment_outcome';
ALTER TABLE node DROP CONSTRAINT node_type_check;
ALTER TABLE node ADD CONSTRAINT node_type_check
    CHECK (type IN ('person', 'venue', 'entity', 'interaction'));

DELETE FROM assertion_provenance WHERE source_kind = 'judgment';
ALTER TABLE assertion_provenance DROP CONSTRAINT assertion_provenance_source_kind_check;
ALTER TABLE assertion_provenance ADD CONSTRAINT assertion_provenance_source_kind_check
    CHECK (source_kind IN (
        'comms_message', 'telegram_message', 'messages_message', 'meeting_note',
        'anarlog_transcript', 'calendar_event', 'phone_call', 'user', 'agent_session'
    ));

COMMIT;
