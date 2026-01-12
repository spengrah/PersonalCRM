-- Fix phone normalization in normalize_contact_method_value to avoid double plus.

CREATE OR REPLACE FUNCTION normalize_contact_method_value(method_type TEXT, raw_value TEXT)
RETURNS TEXT AS $$
DECLARE
    digits TEXT;
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

        digits := regexp_replace(btrim(raw_value), '[^0-9]', '', 'g');
        IF digits = '' THEN
            RETURN '';
        END IF;

        IF btrim(raw_value) ~ '^\\+' THEN
            RETURN '+' || digits;
        END IF;

        IF length(digits) = 10 THEN
            RETURN '+1' || digits;
        END IF;

        IF length(digits) = 11 AND left(digits, 1) = '1' THEN
            RETURN '+' || digits;
        END IF;

        RETURN '+' || digits;
    END IF;

    RETURN btrim(raw_value);
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS set_contact_method_value_normalized ON contact_method;

CREATE TRIGGER set_contact_method_value_normalized
BEFORE INSERT OR UPDATE ON contact_method
FOR EACH ROW EXECUTE FUNCTION set_contact_method_value_normalized();

UPDATE contact_method
SET value_normalized = normalize_contact_method_value(type, value);
