CREATE OR REPLACE FUNCTION normalize_contact_method_value(method_type TEXT, raw_value TEXT)
RETURNS TEXT AS $$
BEGIN
    IF method_type IN ('email', 'gchat') THEN
        RETURN lower(btrim(raw_value));
    END IF;

    IF method_type IN ('telegram', 'twitter', 'discord') THEN
        RETURN lower(regexp_replace(btrim(raw_value), '^@+', ''));
    END IF;

    IF method_type IN ('phone', 'signal', 'whatsapp') THEN
        IF btrim(raw_value) = '' THEN
            RETURN '';
        END IF;

        IF btrim(raw_value) ~ '^\\+' THEN
            RETURN '+' || regexp_replace(btrim(raw_value), '\\D', '', 'g');
        END IF;

        IF length(regexp_replace(btrim(raw_value), '\\D', '', 'g')) = 10 THEN
            RETURN '+1' || regexp_replace(btrim(raw_value), '\\D', '', 'g');
        END IF;

        IF length(regexp_replace(btrim(raw_value), '\\D', '', 'g')) = 11
            AND left(regexp_replace(btrim(raw_value), '\\D', '', 'g'), 1) = '1' THEN
            RETURN '+' || regexp_replace(btrim(raw_value), '\\D', '', 'g');
        END IF;

        RETURN '+' || regexp_replace(btrim(raw_value), '\\D', '', 'g');
    END IF;

    RETURN btrim(raw_value);
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS set_contact_method_value_normalized ON contact_method;

CREATE TRIGGER set_contact_method_value_normalized
BEFORE INSERT OR UPDATE ON contact_method
FOR EACH ROW EXECUTE FUNCTION set_contact_method_value_normalized();
