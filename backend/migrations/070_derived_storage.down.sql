-- Reverse the derived storage scaffolding.
--
-- Both tables are disposable projections with no inbound FKs (embedding has no
-- FK at all; relationship_signal only points OUT to node), so dropping them is
-- safe in either order. Wrapped in an explicit transaction to match the up
-- (the postgres driver does not auto-wrap a migration file).

BEGIN;

DROP TABLE IF EXISTS relationship_signal;
DROP TABLE IF EXISTS embedding;

COMMIT;
