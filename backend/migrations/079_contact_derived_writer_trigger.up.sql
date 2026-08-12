-- Push the sole-writer rule for contact's eight derived columns down from Go
-- convention into the schema. Until now it was enforced twice, and both
-- enforcers only see this repository's Go: an AST walker
-- (backend/tests/sole_writer_static_test.go) and a grep guard
-- (scripts/check-cadence-sole-writer.sh). Neither sees a psql session, admin
-- SQL, or a future non-Go client. Both stay — they fail earlier and with a
-- better message; this trigger covers what they cannot reach.
--
-- Authorization is per OWNER, not one truthy flag: crm.derived_writer takes
-- exactly one of two literal values and each authorizes a DISJOINT column set.
--
-- The comparison is EXACT string equality. No trim, no lower. A GUC of
-- ' cadence ' or 'CADENCE' is not 'cadence' and must be rejected; the
-- wrong-owner test table pins that in both directions.
--
-- current_setting's second argument MUST stay true. It makes an unset GUC
-- return NULL instead of raising, which is the difference between "this write
-- is unauthorized" and "this database has never seen an authorized write".
--
-- Per-column IS DISTINCT FROM, never a blanket UPDATE veto: the profile-only
-- UpdateContact path (full_name, cadence, profile_photo, updated_at) and
-- SoftDeleteContact leave all eight unchanged and must pass with no GUC set.

CREATE OR REPLACE FUNCTION reject_unauthorized_derived_contact_write()
RETURNS TRIGGER AS $$
DECLARE
    writer text := current_setting('crm.derived_writer', true);
    got    text := COALESCE(writer, '<unset>');
BEGIN
    IF writer IS DISTINCT FROM 'cadence' THEN
        IF NEW.last_contacted IS DISTINCT FROM OLD.last_contacted THEN
            RAISE EXCEPTION 'derived column contact.last_contacted requires crm.derived_writer=cadence (got %)', got;
        END IF;
        IF NEW.last_interaction_at IS DISTINCT FROM OLD.last_interaction_at THEN
            RAISE EXCEPTION 'derived column contact.last_interaction_at requires crm.derived_writer=cadence (got %)', got;
        END IF;
        IF NEW.last_outreach_at IS DISTINCT FROM OLD.last_outreach_at THEN
            RAISE EXCEPTION 'derived column contact.last_outreach_at requires crm.derived_writer=cadence (got %)', got;
        END IF;
        IF NEW.last_response_at IS DISTINCT FROM OLD.last_response_at THEN
            RAISE EXCEPTION 'derived column contact.last_response_at requires crm.derived_writer=cadence (got %)', got;
        END IF;
        IF NEW.contact_by IS DISTINCT FROM OLD.contact_by THEN
            RAISE EXCEPTION 'derived column contact.contact_by requires crm.derived_writer=cadence (got %)', got;
        END IF;
    END IF;

    IF writer IS DISTINCT FROM 'knowledge_cache' THEN
        IF NEW.location IS DISTINCT FROM OLD.location THEN
            RAISE EXCEPTION 'derived column contact.location requires crm.derived_writer=knowledge_cache (got %)', got;
        END IF;
        IF NEW.birthday IS DISTINCT FROM OLD.birthday THEN
            RAISE EXCEPTION 'derived column contact.birthday requires crm.derived_writer=knowledge_cache (got %)', got;
        END IF;
        IF NEW.how_met IS DISTINCT FROM OLD.how_met THEN
            RAISE EXCEPTION 'derived column contact.how_met requires crm.derived_writer=knowledge_cache (got %)', got;
        END IF;
    END IF;

    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS reject_unauthorized_derived_contact_write ON contact;

CREATE TRIGGER reject_unauthorized_derived_contact_write
BEFORE UPDATE ON contact
FOR EACH ROW EXECUTE FUNCTION reject_unauthorized_derived_contact_write();
