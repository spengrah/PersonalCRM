-- Rename the own-domain configuration key on email sync states.
--
-- An email account's own-domain list moves from metadata key
-- discovery_own_domains to own_domains: the list now makes its addresses count
-- as the user for Gmail sync and the rematch scan as well as for participant
-- discovery, so the key no longer names discovery. The value moves unchanged;
-- a state without the old key is untouched.

BEGIN;

UPDATE external_sync_state
SET metadata = (metadata - 'discovery_own_domains')
    || jsonb_build_object('own_domains', metadata -> 'discovery_own_domains')
WHERE source = 'email'
  AND metadata ? 'discovery_own_domains';

COMMIT;
