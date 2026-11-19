-- Restore the own-domain configuration key discovery_own_domains on email
-- sync states. own_addresses, which no earlier code reads, stays as it is.

BEGIN;

UPDATE external_sync_state
SET metadata = (metadata - 'own_domains')
    || jsonb_build_object('discovery_own_domains', metadata -> 'own_domains')
WHERE source = 'email'
  AND metadata ? 'own_domains';

COMMIT;
