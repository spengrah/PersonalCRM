-- Drop the assertion store. provenance is dropped first (it FK-references
-- assertion); assertion's nullable superseded_by self-FK is internal to the
-- table, so the single DROP removes it cleanly.

BEGIN;

DROP TABLE assertion_provenance;
DROP TABLE assertion;

COMMIT;
