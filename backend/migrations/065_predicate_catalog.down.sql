-- Drop the predicate catalog. The nullable inverse_predicate self-FK is internal
-- to this table, so the single DROP removes it cleanly.

BEGIN;

DROP TABLE predicate;

COMMIT;
