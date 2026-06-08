package repository

// TEST ONLY. Thin repository wrappers around the synthetic-seed toolkit's
// test-only sqlc bindings (queries/test.sql, Synthetic* prefix). The
// internal/synthetic package routes ALL its DB access through these wrappers so
// it never inlines raw SQL and never leaks generated db.* types across the
// package boundary (the test_fixtures.go precedent). Production code does not
// depend on these.
//
// Two responsibilities:
//   - Settle Gate B reads: per-replay-scoped unfinalized-River-job counts
//     (event-scoped + messaging-aggregate-scoped), keyed on this replay's
//     contacts so the whole non-prefixed cascade is covered without a global
//     kind count.
//   - ID-tracked, FK-ordered cleanup: deletes by tracked id (or ns-prefix for
//     the genuinely prefixed columns), never by a DB-wide source/kind value.

import (
	"context"
	"strconv"

	"personal-crm/backend/internal/db"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
)

// SyntheticSupportRepository wraps the Synthetic* test-only sqlc bindings.
type SyntheticSupportRepository struct {
	queries db.Querier
}

// NewSyntheticSupportRepository constructs the synthetic-support repository
// from a *db.Database's Queries.
func NewSyntheticSupportRepository(queries db.Querier) *SyntheticSupportRepository {
	return &SyntheticSupportRepository{queries: queries}
}

// contactIDStrings projects contact UUIDs to their canonical string form for
// the payload->>'contact_id' text comparison.
func contactIDStrings(contactIDs []uuid.UUID) []string {
	out := make([]string, len(contactIDs))
	for i, id := range contactIDs {
		out[i] = id.String()
	}
	return out
}

func pgUUIDs(ids []uuid.UUID) []pgtype.UUID {
	out := make([]pgtype.UUID, len(ids))
	for i, id := range ids {
		out[i] = uuidToPgUUID(id)
	}
	return out
}

func pgUUIDsToUUIDs(in []pgtype.UUID) []uuid.UUID {
	out := make([]uuid.UUID, 0, len(in))
	for _, v := range in {
		if v.Valid {
			out = append(out, uuid.UUID(v.Bytes))
		}
	}
	return out
}

// --- Settle Gate B ---------------------------------------------------------

// CountUnfinalizedRiverJobsForEventsByContacts counts unfinalized river_job
// rows whose target event was causally produced by one of this replay's
// contacts. Returns 0 when contactIDs is empty (no replay → nothing to wait
// on).
func (r *SyntheticSupportRepository) CountUnfinalizedRiverJobsForEventsByContacts(ctx context.Context, contactIDs []uuid.UUID) (int64, error) {
	if len(contactIDs) == 0 {
		return 0, nil
	}
	return r.queries.SyntheticCountUnfinalizedRiverJobsForEventsByContacts(ctx, contactIDStrings(contactIDs))
}

// CountUnfinalizedMessagingAggregateJobs counts unfinalized
// messaging_aggregate_for_contact jobs for this replay's contacts + source.
// Returns 0 when contactIDs is empty.
func (r *SyntheticSupportRepository) CountUnfinalizedMessagingAggregateJobs(ctx context.Context, contactIDs []uuid.UUID, source string) (int64, error) {
	if len(contactIDs) == 0 {
		return 0, nil
	}
	return r.queries.SyntheticCountUnfinalizedMessagingAggregateJobs(ctx, db.SyntheticCountUnfinalizedMessagingAggregateJobsParams{
		Source:     source,
		ContactIds: contactIDStrings(contactIDs),
	})
}

// --- Cleanup event-id capture ----------------------------------------------

// ListEventIdsForContacts returns every event.id whose payload references one
// of this replay's contacts (the contact-scoped cascade events). Returns nil
// when contactIDs is empty.
func (r *SyntheticSupportRepository) ListEventIdsForContacts(ctx context.Context, contactIDs []uuid.UUID) ([]uuid.UUID, error) {
	if len(contactIDs) == 0 {
		return nil, nil
	}
	rows, err := r.queries.SyntheticListEventIdsForContacts(ctx, contactIDStrings(contactIDs))
	if err != nil {
		return nil, err
	}
	return pgUUIDsToUUIDs(rows), nil
}

// ListEventIdsBySourceAndSourceIDPrefix returns adapter-direct root event ids
// matched by the synthetic (source, source_id-prefix) — the no-contact roots
// (raw_message.* / external_contact.upserted) that the contact-scoped read
// misses. prefix is matched as a LIKE pattern; the caller passes 'synth-<ns>-%'.
func (r *SyntheticSupportRepository) ListEventIdsBySourceAndSourceIDPrefix(ctx context.Context, source, prefix string) ([]uuid.UUID, error) {
	rows, err := r.queries.SyntheticListEventIdsBySourceAndSourceIdPrefix(ctx, db.SyntheticListEventIdsBySourceAndSourceIdPrefixParams{
		Source:         source,
		SourceIDPrefix: pgtype.Text{String: prefix, Valid: true},
	})
	if err != nil {
		return nil, err
	}
	return pgUUIDsToUUIDs(rows), nil
}

// --- ID-tracked, FK-ordered cleanup ----------------------------------------

// DeleteEventConsumerClaimsByEventIds removes claims for the tracked events
// (cleanup step 1). No-op on an empty set.
func (r *SyntheticSupportRepository) DeleteEventConsumerClaimsByEventIds(ctx context.Context, eventIDs []uuid.UUID) (int64, error) {
	if len(eventIDs) == 0 {
		return 0, nil
	}
	return r.queries.SyntheticDeleteEventConsumerClaimsByEventIds(ctx, pgUUIDs(eventIDs))
}

// DeleteInteractionsByIds removes interactions by tracked id (cleanup step 2).
func (r *SyntheticSupportRepository) DeleteInteractionsByIds(ctx context.Context, interactionIDs []uuid.UUID) (int64, error) {
	if len(interactionIDs) == 0 {
		return 0, nil
	}
	return r.queries.SyntheticDeleteInteractionsByIds(ctx, pgUUIDs(interactionIDs))
}

// DeleteEventsByIds removes events by tracked id (cleanup step 3).
func (r *SyntheticSupportRepository) DeleteEventsByIds(ctx context.Context, eventIDs []uuid.UUID) (int64, error) {
	if len(eventIDs) == 0 {
		return 0, nil
	}
	return r.queries.SyntheticDeleteEventsByIds(ctx, pgUUIDs(eventIDs))
}

// DeleteCommsMessagesByExternalIDPrefix removes comms_message rows whose
// external_id is ns-prefixed (cleanup step 4).
func (r *SyntheticSupportRepository) DeleteCommsMessagesByExternalIDPrefix(ctx context.Context, prefix string) (int64, error) {
	return r.queries.SyntheticDeleteCommsMessagesByExternalIdPrefix(ctx, pgtype.Text{String: prefix, Valid: true})
}

// DeleteMessagesMessageByGuidPrefix removes messages_message rows whose guid is
// ns-prefixed (cleanup step 5).
func (r *SyntheticSupportRepository) DeleteMessagesMessageByGuidPrefix(ctx context.Context, prefix string) (int64, error) {
	return r.queries.SyntheticDeleteMessagesMessageByGuidPrefix(ctx, pgtype.Text{String: prefix, Valid: true})
}

// DeleteTelegramMessagesByPeerUserID removes telegram_message rows for one
// synthetic peer (cleanup step 6). Reuses the existing prod-test query.
func (r *SyntheticSupportRepository) DeleteTelegramMessagesByPeerUserID(ctx context.Context, peerUserID int64) (int64, error) {
	return r.queries.DeleteTelegramMessagesByPeerUserID(ctx, pgtype.Int8{Int64: peerUserID, Valid: true})
}

// DeleteTelegramChatConfigsByChatIds removes telegram_chat_config rows for the
// tracked group chat ids. telegram_chat_config has no namespace column, so a
// group replay's config rows are deleted by exact chat id.
func (r *SyntheticSupportRepository) DeleteTelegramChatConfigsByChatIds(ctx context.Context, chatIDs []int64) (int64, error) {
	if len(chatIDs) == 0 {
		return 0, nil
	}
	return r.queries.SyntheticDeleteTelegramChatConfigsByChatIds(ctx, chatIDs)
}

// DeleteTelegramExternalContactsByPeerIds removes telegram discovery candidate
// external_contact rows keyed by the bare peer id (source='telegram'), which the
// ns-prefix delete misses. peerIDs are the tracked int64 peers.
func (r *SyntheticSupportRepository) DeleteTelegramExternalContactsByPeerIds(ctx context.Context, peerIDs []int64) (int64, error) {
	if len(peerIDs) == 0 {
		return 0, nil
	}
	return r.queries.SyntheticDeleteTelegramExternalContactsByPeerIds(ctx, int64sToStrings(peerIDs))
}

// DeleteTelegramExternalIdentitiesByPeerIds removes the external_identity rows
// MatchPeer creates for unmatched telegram peers, keyed by the bare peer id.
func (r *SyntheticSupportRepository) DeleteTelegramExternalIdentitiesByPeerIds(ctx context.Context, peerIDs []int64) (int64, error) {
	if len(peerIDs) == 0 {
		return 0, nil
	}
	return r.queries.SyntheticDeleteTelegramExternalIdentitiesByPeerIds(ctx, int64sToStrings(peerIDs))
}

// int64sToStrings projects int64 peer ids to their canonical decimal string form
// (the shape telegram source_id is stored as).
func int64sToStrings(ids []int64) []string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = strconv.FormatInt(id, 10)
	}
	return out
}

// DeleteCalendarEventsByGcalEventIDPrefix removes calendar_event rows whose
// gcal_event_id is ns-prefixed (cleanup step 7). Reuses the existing query.
func (r *SyntheticSupportRepository) DeleteCalendarEventsByGcalEventIDPrefix(ctx context.Context, prefix string) (int64, error) {
	return r.queries.DeleteCalendarEventsByGcalEventIdPrefix(ctx, pgtype.Text{String: prefix, Valid: true})
}

// DeleteExternalIdentitiesByIdentifierPrefix removes identities whose normalized
// identifier is ns-prefixed (cleanup step 8 primary). Catches the source_id-NULL
// identities MatchOrCreate produces for GCal/external_contact matching, keyed by
// the synthetic email/handle, which a source_id-prefix delete would miss.
func (r *SyntheticSupportRepository) DeleteExternalIdentitiesByIdentifierPrefix(ctx context.Context, prefix string) (int64, error) {
	return r.queries.SyntheticDeleteExternalIdentitiesByIdentifierPrefix(ctx, pgtype.Text{String: prefix, Valid: true})
}

// DeleteExternalIdentitiesBySourceIDPrefix is the prefix backstop for
// ns-prefixed identity source_ids (cleanup step 8 backstop).
func (r *SyntheticSupportRepository) DeleteExternalIdentitiesBySourceIDPrefix(ctx context.Context, prefix string) (int64, error) {
	return r.queries.SyntheticDeleteExternalIdentitiesBySourceIdPrefix(ctx, pgtype.Text{String: prefix, Valid: true})
}

// DeleteExternalContactsBySourceIDPrefix removes external_contact rows whose
// source_id is ns-prefixed (cleanup step 9). Reuses the existing query.
func (r *SyntheticSupportRepository) DeleteExternalContactsBySourceIDPrefix(ctx context.Context, prefix string) (int64, error) {
	return r.queries.DeleteExternalContactsBySourceIDPrefix(ctx, pgtype.Text{String: prefix, Valid: true})
}

// DeleteContactTasksByContactIds removes contact_task rows by contact (cleanup
// step 10; contact_task has no deleted_at, so a hard delete).
func (r *SyntheticSupportRepository) DeleteContactTasksByContactIds(ctx context.Context, contactIDs []uuid.UUID) (int64, error) {
	if len(contactIDs) == 0 {
		return 0, nil
	}
	return r.queries.SyntheticDeleteContactTasksByContactIds(ctx, pgUUIDs(contactIDs))
}

// ListContactTaskIdsByProvider returns the contact_task ids for a provider. The
// Todoist replay diffs this before/after its globally-scoped reconcile to track
// exactly the rows it created (even on cadence-bearing contacts it did not seed).
func (r *SyntheticSupportRepository) ListContactTaskIdsByProvider(ctx context.Context, provider string) ([]uuid.UUID, error) {
	rows, err := r.queries.SyntheticListContactTaskIdsByProvider(ctx, provider)
	if err != nil {
		return nil, err
	}
	return pgUUIDsToUUIDs(rows), nil
}

// DeleteContactTasksByIds removes contact_task rows by tracked id (the Todoist
// replay's before/after diff).
func (r *SyntheticSupportRepository) DeleteContactTasksByIds(ctx context.Context, taskIDs []uuid.UUID) (int64, error) {
	if len(taskIDs) == 0 {
		return 0, nil
	}
	return r.queries.SyntheticDeleteContactTasksByIds(ctx, pgUUIDs(taskIDs))
}

// DeleteContactMethodsByContactIds removes contact_method rows by contact
// (cleanup step 11).
func (r *SyntheticSupportRepository) DeleteContactMethodsByContactIds(ctx context.Context, contactIDs []uuid.UUID) (int64, error) {
	if len(contactIDs) == 0 {
		return 0, nil
	}
	return r.queries.SyntheticDeleteContactMethodsByContactIds(ctx, pgUUIDs(contactIDs))
}

// DeleteNotesByContactIds removes note rows by contact (cleanup step 12).
func (r *SyntheticSupportRepository) DeleteNotesByContactIds(ctx context.Context, contactIDs []uuid.UUID) (int64, error) {
	if len(contactIDs) == 0 {
		return 0, nil
	}
	return r.queries.SyntheticDeleteNotesByContactIds(ctx, pgUUIDs(contactIDs))
}

// DeleteContactsByIds removes contact rows by tracked id (cleanup step 13). A
// true DELETE so ON DELETE CASCADE fires for contact_enrichment.
func (r *SyntheticSupportRepository) DeleteContactsByIds(ctx context.Context, contactIDs []uuid.UUID) (int64, error) {
	if len(contactIDs) == 0 {
		return 0, nil
	}
	return r.queries.SyntheticDeleteContactsByIds(ctx, pgUUIDs(contactIDs))
}

// DeleteMacHostByID removes the seeded revoked synthetic mac_host by id
// (cleanup step 14).
func (r *SyntheticSupportRepository) DeleteMacHostByID(ctx context.Context, id uuid.UUID) (int64, error) {
	return r.queries.SyntheticDeleteMacHostById(ctx, uuidToPgUUID(id))
}

// CountContactsByIds counts surviving contact rows for the given ids (cleanup
// assertion). Returns 0 on an empty set.
func (r *SyntheticSupportRepository) CountContactsByIds(ctx context.Context, contactIDs []uuid.UUID) (int64, error) {
	if len(contactIDs) == 0 {
		return 0, nil
	}
	return r.queries.SyntheticCountContactsByIds(ctx, pgUUIDs(contactIDs))
}

// CountTelegramMessagesInPeerBand counts live telegram_message rows whose
// peer_user_id is in [bandStart, bandEnd). Used by NewHarness for setup-time
// peer-band collision detection (D5): a non-zero count means another namespace
// occupies this namespace's sub-block.
func (r *SyntheticSupportRepository) CountTelegramMessagesInPeerBand(ctx context.Context, bandStart, bandEnd int64) (int64, error) {
	return r.queries.SyntheticCountTelegramMessagesInPeerBand(ctx, db.SyntheticCountTelegramMessagesInPeerBandParams{
		BandStart: pgtype.Int8{Int64: bandStart, Valid: true},
		BandEnd:   pgtype.Int8{Int64: bandEnd, Valid: true},
	})
}

// CountExternalIdentitiesByIdentifierPrefix counts external_identity rows whose
// normalized identifier shares the given prefix. Used by NewHarness for setup-
// time phone-band collision detection (the synthetic phone-digit prefix).
func (r *SyntheticSupportRepository) CountExternalIdentitiesByIdentifierPrefix(ctx context.Context, prefix string) (int64, error) {
	return r.queries.SyntheticCountExternalIdentitiesByIdentifierPrefix(ctx, pgtype.Text{String: prefix, Valid: true})
}

// CountTelegramChatConfigInChatIdBand counts telegram_chat_config rows whose
// telegram_chat_id is in [bandStart, bandEnd). Used by NewHarness for setup-time
// group chat-id collision detection (group chat ids are drawn from the same
// peer band, and telegram_chat_config has no namespace column).
func (r *SyntheticSupportRepository) CountTelegramChatConfigInChatIdBand(ctx context.Context, bandStart, bandEnd int64) (int64, error) {
	return r.queries.SyntheticCountTelegramChatConfigInChatIdBand(ctx, db.SyntheticCountTelegramChatConfigInChatIdBandParams{
		BandStart: bandStart,
		BandEnd:   bandEnd,
	})
}

// CountTelegramMessagesByChatAndMessageID counts telegram_message rows for
// (chatID, messageID). The group adapter asserts 0 for the untracked-by-size
// case (the shouldTrackChat gate returned before UpsertMessage) and 1 for
// tracked.
func (r *SyntheticSupportRepository) CountTelegramMessagesByChatAndMessageID(ctx context.Context, chatID int64, messageID int32) (int64, error) {
	return r.queries.SyntheticCountTelegramMessagesByChatAndMessageId(ctx, db.SyntheticCountTelegramMessagesByChatAndMessageIdParams{
		TelegramChatID:    chatID,
		TelegramMessageID: messageID,
	})
}

// CountContactMethodsByValueNormalizedPrefix counts live (non-deleted-contact)
// contact_method rows whose normalized value shares the given prefix. This is the
// PRIMARY phone-band collision check: a seeded synthetic phone lives only as a
// contact_method (no external_identity until a later replay), and identity
// matching cross-matches via contact_method.value_normalized.
func (r *SyntheticSupportRepository) CountContactMethodsByValueNormalizedPrefix(ctx context.Context, prefix string) (int64, error) {
	return r.queries.SyntheticCountContactMethodsByValueNormalizedPrefix(ctx, pgtype.Text{String: prefix, Valid: true})
}

// --- Settle Gate A domain predicates (pending / match states) --------------

// CountLinkedCommsMessageByExternalID counts comms_message rows for (source,
// external_id) with a non-null interaction_id — the seeded gmail/gchat Gate A
// predicate (exact-to-replay via the synthetic external_id, idempotent across
// re-replays since the row stays linked).
func (r *SyntheticSupportRepository) CountLinkedCommsMessageByExternalID(ctx context.Context, source, externalID string) (int64, error) {
	return r.queries.SyntheticCountLinkedCommsMessageByExternalId(ctx, db.SyntheticCountLinkedCommsMessageByExternalIdParams{
		Source:     source,
		ExternalID: externalID,
	})
}

// CountLinkedMessagesMessageByGuid counts iMessage staging rows for the guid with
// a non-null interaction_id — the seeded iMessage Gate A predicate.
func (r *SyntheticSupportRepository) CountLinkedMessagesMessageByGuid(ctx context.Context, guid string) (int64, error) {
	return r.queries.SyntheticCountLinkedMessagesMessageByGuid(ctx, guid)
}

// CountLinkedTelegramMessageByMessageID counts telegram_message rows for
// (peerUserID, telegramMessageID) with a non-null interaction_id — the seeded
// telegram Gate A predicate. Scoped by peer (collision-checked at setup) so a
// colliding message-id bucket in another namespace cannot satisfy it early.
func (r *SyntheticSupportRepository) CountLinkedTelegramMessageByMessageID(ctx context.Context, peerUserID int64, telegramMessageID int32) (int64, error) {
	return r.queries.SyntheticCountLinkedTelegramMessageByMessageId(ctx, db.SyntheticCountLinkedTelegramMessageByMessageIdParams{
		PeerUserID:        pgtype.Int8{Int64: peerUserID, Valid: true},
		TelegramMessageID: telegramMessageID,
	})
}

// CountProcessedCalendarEventByGcalID counts calendar_event rows for the gcal id
// where the contact is in matched_contact_ids and last_contacted_updated=true
// (the attended interaction published) — the seeded gcal Gate A predicate.
func (r *SyntheticSupportRepository) CountProcessedCalendarEventByGcalID(ctx context.Context, gcalEventID string, contactID uuid.UUID) (int64, error) {
	return r.queries.SyntheticCountProcessedCalendarEventByGcalId(ctx, db.SyntheticCountProcessedCalendarEventByGcalIdParams{
		GcalEventID: gcalEventID,
		ContactID:   uuidToPgUUID(contactID),
	})
}

// CountStrandedTelegramMessagesByPeer counts telegram_message rows for the peer
// with matched_contact_id IS NULL (the stranded / discovery-candidate state).
func (r *SyntheticSupportRepository) CountStrandedTelegramMessagesByPeer(ctx context.Context, peerUserID int64) (int64, error) {
	return r.queries.SyntheticCountStrandedTelegramMessagesByPeer(ctx, pgtype.Int8{Int64: peerUserID, Valid: true})
}

// CountStrandedMessagesMessageByGuid counts the iMessage staging row for the guid
// with matched_contact_id IS NULL.
func (r *SyntheticSupportRepository) CountStrandedMessagesMessageByGuid(ctx context.Context, guid string) (int64, error) {
	return r.queries.SyntheticCountStrandedMessagesMessageByGuid(ctx, guid)
}

// CountUnmatchedExternalContactBySourceID counts external_contact rows for the
// entity id with match_status='unmatched'.
func (r *SyntheticSupportRepository) CountUnmatchedExternalContactBySourceID(ctx context.Context, sourceID string) (int64, error) {
	return r.queries.SyntheticCountUnmatchedExternalContactBySourceId(ctx, sourceID)
}

// CountMatchedExternalContactBySourceID counts external_contact rows for the
// entity id linked to a CRM contact (match_status='matched').
func (r *SyntheticSupportRepository) CountMatchedExternalContactBySourceID(ctx context.Context, sourceID string) (int64, error) {
	return r.queries.SyntheticCountMatchedExternalContactBySourceId(ctx, sourceID)
}

// CountUnmatchedCalendarEventByGcalID counts calendar_event rows for the gcal id
// with an empty matched_contact_ids array.
func (r *SyntheticSupportRepository) CountUnmatchedCalendarEventByGcalID(ctx context.Context, gcalEventID string) (int64, error) {
	return r.queries.SyntheticCountUnmatchedCalendarEventByGcalId(ctx, gcalEventID)
}

// CountCalendarEventByGcalID counts ALL calendar_event rows for the gcal id
// (any status/match). The GCal decline test settles on this reaching 0 (the
// cutover decline branch deletes the row).
func (r *SyntheticSupportRepository) CountCalendarEventByGcalID(ctx context.Context, gcalEventID string) (int64, error) {
	return r.queries.SyntheticCountCalendarEventByGcalId(ctx, gcalEventID)
}

// CountUnmatchedExternalContactByEmailPrefix counts gcal_attendee import
// candidates whose source_id (the normalized attendee email) is ns-prefixed and
// match_status='unmatched'. The GCal unmatched-attendee path stores the candidate
// keyed by the normalized email, not by a synthetic source_id, so it is matched
// by the email prefix here.
func (r *SyntheticSupportRepository) CountUnmatchedExternalContactByEmailPrefix(ctx context.Context, prefix string) (int64, error) {
	return r.queries.SyntheticCountUnmatchedExternalContactByEmailPrefix(ctx, pgtype.Text{String: prefix, Valid: true})
}

// --- revoked synthetic Mac host (host-only ingest kinds) -------------------

// SeedRevokedMacHost inserts a REVOKED mac_host (api_key_revoked_at set) and
// returns its id. The revoked state dodges the idx_mac_host_singleton partial
// unique (which only applies to non-revoked rows), so the synthetic harness can
// seed it freely on the shared test DB. Its id is used as the non-nil hostID for
// the host-only ingest-kind allowlist (with hostLiveness=nil so the active-host
// re-check is skipped). hostname carries the namespace so cleanup is targeted.
func (r *SyntheticSupportRepository) SeedRevokedMacHost(ctx context.Context, hostname string) (uuid.UUID, error) {
	row, err := r.queries.SeedRevokedMacHost(ctx, db.SeedRevokedMacHostParams{
		Hostname:        hostname,
		DaemonVersion:   "synthetic",
		ProtocolVersion: 1,
		// Revoked host never authenticates; a static non-empty hash satisfies
		// NOT NULL without minting a real credential.
		ApiKeyHash: "synthetic-revoked-no-auth",
	})
	if err != nil {
		return uuid.Nil, err
	}
	if !row.ID.Valid {
		return uuid.Nil, db.ErrNotFound
	}
	return uuid.UUID(row.ID.Bytes), nil
}
