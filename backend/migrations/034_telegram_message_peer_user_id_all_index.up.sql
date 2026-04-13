-- Non-match-status-filtered partial index on peer_user_id, used by the
-- sparse-entity enrichment fallback in the live message handler
-- (GetPeerEntityByUserID). Coexists with the existing
-- `idx_telegram_message_peer` (WHERE matched_contact_id IS NULL) which
-- continues to serve identity-matching queries.
CREATE INDEX IF NOT EXISTS idx_telegram_message_peer_user_id_all
ON telegram_message(peer_user_id)
WHERE deleted_at IS NULL AND peer_user_id IS NOT NULL;
