-- Persist whether the row's peer entity fields came from an authoritative
-- tg.User in the update's entities (true) vs a sparse update where the
-- handler had to backfill from history (false). Used by GetPeerEntityByUserID
-- to prefer the MOST RECENT authoritative row when enriching subsequent
-- sparse updates — so a "user removed their username" event isn't undone by
-- resurrecting an older non-blank handle.
ALTER TABLE telegram_message
ADD COLUMN IF NOT EXISTS peer_entity_resolved BOOLEAN NOT NULL DEFAULT FALSE;
