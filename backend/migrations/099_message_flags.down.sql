BEGIN;

DROP TABLE judgment_flag_threshold;
DROP TABLE judgment_flag_method;
DROP TABLE judgment_unjudged;
DROP TABLE judgment_clearing;
DROP TABLE judgment_flag;

DELETE FROM judgment_incomplete WHERE reason = 'message_over_limit';
ALTER TABLE judgment_incomplete DROP CONSTRAINT judgment_incomplete_reason_check;
ALTER TABLE judgment_incomplete ADD CONSTRAINT judgment_incomplete_reason_check
    CHECK (reason IN ('over_limit', 'call_failed', 'input_changed'));

-- A judgment that read a window has no identity before this migration: remove
-- it with its outcomes and their assertions.
DELETE FROM judgment_appearance WHERE judgment_id IN (SELECT id FROM judgment WHERE read_digest <> '');
DELETE FROM assertion_provenance WHERE assertion_id IN (
    SELECT a.id FROM assertion a
    JOIN judgment_outcome o ON o.node_id = a.subject_node_id
    JOIN judgment j ON j.id = o.judgment_id
    WHERE j.read_digest <> '');
UPDATE assertion SET superseded_by = NULL WHERE subject_node_id IN (
    SELECT o.node_id FROM judgment_outcome o JOIN judgment j ON j.id = o.judgment_id WHERE j.read_digest <> '');
DELETE FROM assertion WHERE subject_node_id IN (
    SELECT o.node_id FROM judgment_outcome o JOIN judgment j ON j.id = o.judgment_id WHERE j.read_digest <> '');
DELETE FROM node WHERE id IN (
    SELECT o.node_id FROM judgment_outcome o JOIN judgment j ON j.id = o.judgment_id WHERE j.read_digest <> '');
DELETE FROM judgment WHERE read_digest <> '';

ALTER TABLE judgment DROP CONSTRAINT judgment_method_example_read_key;
ALTER TABLE judgment ADD CONSTRAINT judgment_method_key_method_version_wording_version_example__key
    UNIQUE (method_key, method_version, wording_version, example_id);
ALTER TABLE judgment
    DROP CONSTRAINT judgment_read_shape,
    DROP COLUMN read_digest,
    DROP COLUMN new_messages,
    DROP COLUMN window_start;

COMMIT;
