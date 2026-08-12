-- Drops BOTH objects. The trigger first: dropping the function while a trigger
-- references it would fail without CASCADE, and CASCADE on a function is a
-- blunter instrument than this needs.
--
-- The DROP FUNCTION is load-bearing and is asserted directly by
-- TestDerivedWriterTrigger_MigrationUpDown via to_regprocedure(). Because the
-- up migration uses CREATE OR REPLACE FUNCTION, a down that dropped only the
-- trigger would leave the function orphaned in the catalog and every
-- behavioral assertion would still pass — the reapply would silently overwrite
-- it. Do not "simplify" this file to a single DROP TRIGGER.
DROP TRIGGER IF EXISTS reject_unauthorized_derived_contact_write ON contact;
DROP FUNCTION IF EXISTS reject_unauthorized_derived_contact_write();
