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
	"errors"
	"fmt"
	"strconv"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/db"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
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

// DeleteIdentitiesForOwnedCandidates removes the external_identity rows a
// post-import hook derived from a namespace's own import candidates, matched on
// the (source, source_id) pair the hook copies off the candidate. It is the
// stateless counterpart of DeleteTelegramExternalIdentitiesByPeerIds, for the
// cross-request cleanup that has no tracked peer-id list — and the only route to
// a telegram peer identity, whose decimal source_id and underscore-normalized
// handle both escape the ns-prefix sweeps.
func (r *SyntheticSupportRepository) DeleteIdentitiesForOwnedCandidates(ctx context.Context, ids []uuid.UUID) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	return r.queries.SyntheticDeleteIdentitiesForOwnedCandidates(ctx, pgUUIDs(ids))
}

// DeleteExternalContactsBySourceIDPrefix removes external_contact rows whose
// source_id is ns-prefixed (cleanup step 9). Reuses the existing query.
func (r *SyntheticSupportRepository) DeleteExternalContactsBySourceIDPrefix(ctx context.Context, prefix string) (int64, error) {
	return r.queries.DeleteExternalContactsBySourceIDPrefix(ctx, pgtype.Text{String: prefix, Valid: true})
}

// SelectLinkedContactIDsByExternalContactIDs returns the CRM contacts the product
// linked to a namespace's own import candidates — the third way cleanup recovers a
// contact, beside the name prefix and the ownership record.
func (r *SyntheticSupportRepository) SelectLinkedContactIDsByExternalContactIDs(ctx context.Context, ids []uuid.UUID) ([]uuid.UUID, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := r.queries.SyntheticSelectLinkedContactIdsByExternalContactIds(ctx, pgUUIDs(ids))
	if err != nil {
		return nil, err
	}
	return pgUUIDsToUUIDs(rows), nil
}

// DeleteExternalContactsByIds removes external_contact rows by the namespace's
// ownership records (cleanup step 9, unioned with the source_id-prefix sweep).
func (r *SyntheticSupportRepository) DeleteExternalContactsByIds(ctx context.Context, ids []uuid.UUID) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	return r.queries.SyntheticDeleteExternalContactsByIds(ctx, pgUUIDs(ids))
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

// DeleteMeetingNotesByHostID hard-deletes meeting_note rows scoped to the
// seeded synthetic mac_host (cleanup step, BEFORE the mac_host delete). A
// profile may seed orphan_needs_review meeting_note rows against the harness's
// host; without this the harness teardown would leak them (the mac_host FK is
// ON DELETE SET NULL, so the host delete alone leaves orphaned note rows). Reuses
// the existing meeting-note test-cleanup query.
func (r *SyntheticSupportRepository) DeleteMeetingNotesByHostID(ctx context.Context, hostID uuid.UUID) error {
	return r.queries.TestHardDeleteMeetingNotesByHostID(ctx, uuidToPgUUID(hostID))
}

// SelectLinkedContactIDsForHostSessionTitleCandidates returns the CRM contacts
// the product created by resolving an anarlog_title candidate derived from one of
// this namespace's meeting notes. It is the composition partner of
// DeleteTitleCandidatesForHostSessions: that delete removes the only rows linking
// back to these contacts, so this must be read first. Contacts another namespace
// recorded ownership of are excluded — the resolve marks siblings by normalized
// token DB-wide, so a shared token can point our rows at a live neighbour's
// contact.
func (r *SyntheticSupportRepository) SelectLinkedContactIDsForHostSessionTitleCandidates(
	ctx context.Context,
	hostname, namespace string,
) ([]uuid.UUID, error) {
	rows, err := r.queries.SyntheticSelectLinkedContactIdsForHostSessionTitleCandidates(
		ctx,
		db.SyntheticSelectLinkedContactIdsForHostSessionTitleCandidatesParams{Hostname: hostname, Namespace: namespace},
	)
	if err != nil {
		return nil, err
	}
	return pgUUIDsToUUIDs(rows), nil
}

// DeleteTitleCandidatesForHostSessions removes the anarlog_title candidates the
// product derived from this namespace's meeting notes when a user resolved one.
// Their SHA-256 source_id and bare-token display_name carry nothing
// namespace-derived, so the session uuid is the only route to them — which means
// this MUST run before the meeting notes it reads are deleted.
func (r *SyntheticSupportRepository) DeleteTitleCandidatesForHostSessions(ctx context.Context, hostID uuid.UUID) (int64, error) {
	return r.queries.SyntheticDeleteTitleCandidatesForHostSessions(ctx, uuidToPgUUID(hostID))
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

// CountTelegramBarePeerRowsInBand counts telegram external_contact +
// external_identity rows keyed by a bare peer-id source_id in [bandStart,
// bandEnd). Used by NewHarness for setup-time collision detection: a
// discovery/stranded replay creates these keyed by the bare peer id, and a
// crashed prior run can leave them with no telegram_message row, so the
// peer-band check on telegram_message alone would miss them.
func (r *SyntheticSupportRepository) CountTelegramBarePeerRowsInBand(ctx context.Context, bandStart, bandEnd int64) (int64, error) {
	return r.queries.SyntheticCountTelegramBarePeerRowsInBand(ctx, db.SyntheticCountTelegramBarePeerRowsInBandParams{
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

// --- Settle Gate A, batch (set-widened) forms --------------------------------
//
// A batch replay adapter drives N payloads through one provider pass and settles
// ONCE, so its gate is a single COUNT over the batch's identifiers compared
// against len(batch). Each wrapper widens the terminal predicate of the
// single-message gate above to a set without weakening it, and stays scoped to
// the passed identifiers.

// CountSettledGmailMessagesByExternalIDs counts how many of the given external
// ids have an interaction-linked comms_message row on the email source — the
// batch Gmail Gate A. Satisfied when it equals the batch size.
func (r *SyntheticSupportRepository) CountSettledGmailMessagesByExternalIDs(ctx context.Context, externalIDs []string) (int64, error) {
	return r.countSettledCommsMessages(ctx, "email", externalIDs)
}

// CountSettledGChatMessagesByExternalIDs counts how many of the given external
// ids have an interaction-linked comms_message row on the gchat source — the
// batch GChat Gate A. Satisfied when it equals the batch size.
func (r *SyntheticSupportRepository) CountSettledGChatMessagesByExternalIDs(ctx context.Context, externalIDs []string) (int64, error) {
	return r.countSettledCommsMessages(ctx, "gchat", externalIDs)
}

// countSettledCommsMessages backs both comms-source batch gates. gmail and gchat
// share one comms_message predicate differing only in the source literal — the
// same sharing the single-message gate already uses
// (CountLinkedCommsMessageByExternalID).
func (r *SyntheticSupportRepository) countSettledCommsMessages(ctx context.Context, source string, externalIDs []string) (int64, error) {
	return r.queries.SyntheticCountSettledCommsMessagesByExternalIds(ctx, db.SyntheticCountSettledCommsMessagesByExternalIdsParams{
		Source:      source,
		ExternalIds: externalIDs,
	})
}

// CountGChatMessagesByExternalIDs counts how many of the given external ids have
// a gchat comms_message row at all (linked or not). It is the GChat batch
// bucket loop's PROGRESS probe, not its gate: the provider writes these rows
// synchronously inside Sync while the interaction linkage arrives later from a
// River consumer, so only this read distinguishes an unpresented space from an
// unfinished aggregate.
func (r *SyntheticSupportRepository) CountGChatMessagesByExternalIDs(ctx context.Context, externalIDs []string) (int64, error) {
	return r.queries.SyntheticCountCommsMessagesByExternalIds(ctx, db.SyntheticCountCommsMessagesByExternalIdsParams{
		Source:      "gchat",
		ExternalIds: externalIDs,
	})
}

// CountSettledMessagesMessagesByGUIDs counts how many of the given guids have an
// interaction-linked messages_message staging row — the batch iMessage Gate A.
func (r *SyntheticSupportRepository) CountSettledMessagesMessagesByGUIDs(ctx context.Context, guids []string) (int64, error) {
	return r.queries.SyntheticCountSettledMessagesMessagesByGuids(ctx, guids)
}

// CountSettledTelegramMessagesByPeerAndMessageIDs counts how many of the given
// (peer, message id) PAIRS have an interaction-linked telegram_message row — the
// batch Telegram Gate A. The arrays are parallel and must be the same length;
// the composite key is required because only the peer band is collision-checked
// at namespace setup (see the query comment).
func (r *SyntheticSupportRepository) CountSettledTelegramMessagesByPeerAndMessageIDs(ctx context.Context, peerUserIDs []int64, telegramMessageIDs []int32) (int64, error) {
	if len(peerUserIDs) != len(telegramMessageIDs) {
		// The ordinality join would silently yield min(len) pairs, so the gate could
		// never be satisfied — a 30-second timeout naming the wrong cause, the
		// failure class the batch preflight exists to eliminate.
		return 0, fmt.Errorf("count settled telegram messages: %d peer ids but %d message ids", len(peerUserIDs), len(telegramMessageIDs))
	}
	return r.queries.SyntheticCountSettledTelegramMessagesByPeerAndMessageIds(ctx, db.SyntheticCountSettledTelegramMessagesByPeerAndMessageIdsParams{
		PeerUserIds:        peerUserIDs,
		TelegramMessageIds: telegramMessageIDs,
	})
}

// CountMatchedCalendarEventsByGcalIDs counts how many of the given (gcal event
// id, contact id) PAIRS have a calendar_event carrying the contact in
// matched_contact_ids with last_contacted_updated = true — the batch GCal Gate A.
// The arrays are parallel and must be the same length.
func (r *SyntheticSupportRepository) CountMatchedCalendarEventsByGcalIDs(ctx context.Context, gcalEventIDs []string, contactIDs []uuid.UUID) (int64, error) {
	if len(gcalEventIDs) != len(contactIDs) {
		// See CountSettledTelegramMessagesByPeerAndMessageIDs: a mismatch makes the
		// gate unsatisfiable rather than wrong-by-a-row.
		return 0, fmt.Errorf("count matched calendar events: %d event ids but %d contact ids", len(gcalEventIDs), len(contactIDs))
	}
	pg := make([]pgtype.UUID, 0, len(contactIDs))
	for _, id := range contactIDs {
		pg = append(pg, uuidToPgUUID(id))
	}
	return r.queries.SyntheticCountMatchedCalendarEventsByGcalIds(ctx, db.SyntheticCountMatchedCalendarEventsByGcalIdsParams{
		GcalEventIds: gcalEventIDs,
		ContactIds:   pg,
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

// --- crm-admin reset + additive-seed preflight -----------------------------

// ResetSyntheticData HARD-truncates the complete live data-table closure (every
// app data table EXCEPT schema_migrations + River's own internal tables;
// river_job IS wiped), then clears the runtime-minted PROVISIONAL rows from the
// migration-seeded catalog tables (predicate, entity_type). Those catalog tables
// are NOT truncated — their curated rows (migration 066) must survive a reset —
// but a reset must still restore a known baseline, so provisional pollution is
// removed here. Used ONLY by crm-admin --reset-and-seed, behind the
// CRM_ENV-production gate + the mandatory --yes confirm + a stopped service. The
// reset boundary is verified by synthetic_reset_integration_test.go (clone DB).
func (r *SyntheticSupportRepository) ResetSyntheticData(ctx context.Context) error {
	if err := r.queries.ResetSyntheticData(ctx); err != nil {
		return err
	}
	if err := r.queries.DeleteProvisionalPredicates(ctx); err != nil {
		return fmt.Errorf("clear provisional predicates: %w", err)
	}
	if err := r.queries.DeleteProvisionalEntityTypes(ctx); err != nil {
		return fmt.Errorf("clear provisional entity types: %w", err)
	}
	return nil
}

// CountNonFinalRiverJobs counts queued/in-flight river_job rows. The additive
// crm-admin --seed preflight REFUSES when this is non-zero (a drained queue is
// its precondition; it must not steal pre-existing work). NOT a liveness probe —
// River 0.34 does not populate river_client, so no sound DB liveness signal
// exists; --reset-and-seed skips this check (it wipes river_job).
func (r *SyntheticSupportRepository) CountNonFinalRiverJobs(ctx context.Context) (int64, error) {
	return r.queries.CountNonFinalRiverJobs(ctx)
}

// PrefixCleanupResult reports the per-table delete counts from CleanupByPrefix so
// the /cleanup HTTP response preserves its existing shape.
type PrefixCleanupResult struct {
	DeletedContacts         int64
	DeletedExternalContacts int64
	DeletedCalendarEvents   int64
}

// CleanupByPrefix runs the /test/cleanup prefix deletes (contacts by name,
// external_contact by display_name + source_id, calendar_event by title +
// gcal_event_id). It is the repository home for what the /cleanup handler used
// to inline via db.New(tx) — fixing the handler→queries layer violation. The
// caller (TestSeedService) constructs this repository over a tx-scoped querier
// so the deletes commit atomically; the LIKE-escaped prefix is supplied by the
// caller (escaping stays at the service boundary, mirroring the old handler).
func (r *SyntheticSupportRepository) CleanupByPrefix(ctx context.Context, escapedPrefix string) (PrefixCleanupResult, error) {
	var res PrefixCleanupResult
	prefix := pgtype.Text{String: escapedPrefix, Valid: true}

	deletedContacts, err := r.queries.DeleteContactsByNamePrefix(ctx, prefix)
	if err != nil {
		return res, err
	}
	res.DeletedContacts = deletedContacts

	deletedExternal, err := r.queries.DeleteExternalContactsByDisplayNamePrefix(ctx, prefix)
	if err != nil {
		return res, err
	}
	deletedBySourceID, err := r.queries.DeleteExternalContactsBySourceIDPrefix(ctx, prefix)
	if err != nil {
		return res, err
	}
	res.DeletedExternalContacts = deletedExternal + deletedBySourceID

	deletedCalEvents, err := r.queries.DeleteCalendarEventsByTitlePrefix(ctx, prefix)
	if err != nil {
		return res, err
	}
	deletedByGcalID, err := r.queries.DeleteCalendarEventsByGcalEventIdPrefix(ctx, prefix)
	if err != nil {
		return res, err
	}
	res.DeletedCalendarEvents = deletedCalEvents + deletedByGcalID

	return res, nil
}

// --- reset integration test support (clone DB only) ------------------------

// CountAllRows counts every row in tableName. Used ONLY by the clone-DB reset
// test to assert each wiped table is empty after the reset (and schema_migrations
// survives). The table name is validated against the wiped list by the test
// before this call.
func (r *SyntheticSupportRepository) CountAllRows(ctx context.Context, tableName string) (int64, error) {
	return r.queries.TestCountAllRows(ctx, tableName)
}

// ListPublicTables enumerates every base table in the public schema (the catalog
// guard's input). Reset test only.
func (r *SyntheticSupportRepository) ListPublicTables(ctx context.Context) ([]string, error) {
	return r.queries.TestListPublicTables(ctx)
}

// ListCommsIndexDefs returns Postgres's deterministic index definition for every
// index on comms_message, keyed by index name. Backs the migration-073
// index-definition test (exact key-column + partial-predicate assertions).
// Index-definition test only.
func (r *SyntheticSupportRepository) ListCommsIndexDefs(ctx context.Context) (map[string]string, error) {
	rows, err := r.queries.TestListIndexDefsForComms(ctx)
	if err != nil {
		return nil, err
	}
	defs := make(map[string]string, len(rows))
	for _, row := range rows {
		defs[row.Indexname] = row.Indexdef
	}
	return defs, nil
}

// InsertNonFinalRiverJob plants one queued river_job so a test can assert the
// additive --seed preflight refuses while --reset-and-seed proceeds. Test only.
func (r *SyntheticSupportRepository) InsertNonFinalRiverJob(ctx context.Context) error {
	return r.queries.TestInsertNonFinalRiverJob(ctx)
}

// InsertResetMarkers seeds a marker row into the standalone (harness-untouched)
// wiped tables — oauth_credential, external_sync_state, telegram_session, tag,
// the derived-storage projections (embedding, relationship_signal),
// synthetic_namespace_entity and job_exec_sample — so the reset test proves
// TRUNCATE empties tables the synthetic harness does not populate. These tables
// start empty otherwise, so
// without a marker the catalog guard could not catch their omission from the
// TRUNCATE list. Test only.
func (r *SyntheticSupportRepository) InsertResetMarkers(ctx context.Context) error {
	if err := r.queries.TestInsertOAuthCredentialMarker(ctx); err != nil {
		return err
	}
	if err := r.queries.TestInsertExternalSyncStateMarker(ctx); err != nil {
		return err
	}
	if err := r.queries.TestInsertTelegramSessionMarker(ctx); err != nil {
		return err
	}
	if err := r.queries.TestInsertTagMarker(ctx); err != nil {
		return err
	}
	// The relationship_signal marker FKs to a node, so the marker node goes first.
	if err := r.queries.TestInsertDerivedStorageMarkerNode(ctx); err != nil {
		return err
	}
	if err := r.queries.TestInsertEmbeddingMarker(ctx); err != nil {
		return err
	}
	if err := r.queries.TestInsertRelationshipSignalMarker(ctx); err != nil {
		return err
	}
	// synthetic_namespace_entity is written only by the DECLARED seed path, which
	// a profile reset never takes, so it starts empty too — plant a row through
	// the production insert so the guard genuinely fails if the table drops out
	// of the TRUNCATE list.
	if err := r.RecordNamespaceEntity(ctx, "reset-marker", EntityKindContact, uuid.New()); err != nil {
		return err
	}
	// job_exec_sample is written only by the live Subscribe recorder, never by
	// the synthetic harness, so it starts empty — seed one row (via the
	// production sqlc insert path; created_at = accelerated now, attempted_at
	// backdated 1s to satisfy the finalized_at >= attempted_at CHECK) so the
	// catalog guard genuinely fails if the table drops out of the TRUNCATE list.
	now := accelerated.GetCurrentTime()
	return r.queries.InsertJobExecSample(ctx, db.InsertJobExecSampleParams{
		RiverJobID:  1,
		Kind:        "reset_marker",
		Queue:       "default",
		AttemptedAt: pgtype.Timestamptz{Time: now.Add(-time.Second), Valid: true},
		FinalizedAt: pgtype.Timestamptz{Time: now, Valid: true},
		Attempt:     1,
		State:       "completed",
		QueueWaitMs: 0,
		CreatedAt:   pgtype.Timestamptz{Time: now, Valid: true},
	})
}

// --- Graph identity support -------------------------------------------------

// CountNodesByLabelPrefix counts nodes whose canonical_label is ns-prefixed, so
// a graph-identity test can scope its assertions to its own namespace on the
// shared test DB.
func (r *SyntheticSupportRepository) CountNodesByLabelPrefix(ctx context.Context, prefix string) (int64, error) {
	return r.queries.SyntheticCountNodesByLabelPrefix(ctx, pgtype.Text{String: prefix, Valid: true})
}

// DeleteNodesByLabelPrefix hard-deletes nodes whose canonical_label is
// ns-prefixed; entity and venue rows cascade via their ON DELETE CASCADE FK to
// node, so this one delete clears a namespace's node+entity+venue rows.
func (r *SyntheticSupportRepository) DeleteNodesByLabelPrefix(ctx context.Context, prefix string) (int64, error) {
	return r.queries.SyntheticDeleteNodesByLabelPrefix(ctx, pgtype.Text{String: prefix, Valid: true})
}

// DeleteEntityTypesByKeyPrefix hard-deletes entity_type catalog rows whose key
// is ns-prefixed (a test that seeds its own provisional subtypes; the curated
// catalog seed rows use bare keys and are never prefix-matched).
func (r *SyntheticSupportRepository) DeleteEntityTypesByKeyPrefix(ctx context.Context, prefix string) (int64, error) {
	return r.queries.SyntheticDeleteEntityTypesByKeyPrefix(ctx, pgtype.Text{String: prefix, Valid: true})
}

// DeletePredicatesByKeyPrefix hard-deletes predicate-catalog rows whose key is
// ns-prefixed (a test that mints its own provisional predicates; the curated
// seed rows use bare keys and are never prefix-matched).
func (r *SyntheticSupportRepository) DeletePredicatesByKeyPrefix(ctx context.Context, prefix string) (int64, error) {
	return r.queries.SyntheticDeletePredicatesByKeyPrefix(ctx, pgtype.Text{String: prefix, Valid: true})
}

// CountAssertionsForSubject counts the assertions whose subject is a given node,
// so an assertion-store test can scope its assertions to its own namespace's
// subject node on the shared test DB (assertion rows have no canonical_label to
// prefix-match, so the scope is the subject node id). Provenance rows cascade
// from assertion, so a node-prefix delete clears both.
func (r *SyntheticSupportRepository) CountAssertionsForSubject(ctx context.Context, subjectNodeID uuid.UUID) (int64, error) {
	return r.queries.SyntheticCountAssertionsForSubject(ctx, pgtype.UUID{Bytes: subjectNodeID, Valid: true})
}

// GetNodeForContact fetches the person node a contact owns (node.id ==
// contact.id), so a contact→node dual-write test can assert the node exists
// with the expected type and canonical_label. Returns db.ErrNotFound when no
// live node is at the contact's id.
func (r *SyntheticSupportRepository) GetNodeForContact(ctx context.Context, contactID uuid.UUID) (*Node, error) {
	dbNode, err := r.queries.SyntheticGetNodeForContact(ctx, pgtype.UUID{Bytes: contactID, Valid: true})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, db.ErrNotFound
		}
		return nil, err
	}
	node := convertDbNode(dbNode)
	return &node, nil
}

// CountContactsByFullName counts live contacts with an exact full_name, so a
// contact→node dual-write rollback test can assert a failed-tx contact did not
// survive without paging the whole contact list (namespace-prefixed names are
// unique per test).
func (r *SyntheticSupportRepository) CountContactsByFullName(ctx context.Context, fullName string) (int64, error) {
	return r.queries.SyntheticCountContactsByFullName(ctx, fullName)
}

// DeleteNodesByIds hard-deletes the person nodes a seeded contact owns
// (node.id == contact.id), so the harness teardown removes the nodes its
// dual-writing SeedContact created alongside the contacts it tracks.
func (r *SyntheticSupportRepository) DeleteNodesByIds(ctx context.Context, nodeIDs []uuid.UUID) (int64, error) {
	if len(nodeIDs) == 0 {
		return 0, nil
	}
	return r.queries.SyntheticDeleteNodesByIds(ctx, pgUUIDs(nodeIDs))
}

// DeleteVenueNodesByIds hard-deletes the venue nodes the real recorders minted
// on the replay path (interaction.venue_id), keyed by the tracked venue node
// ids. Guarded by type='venue' + no remaining interaction reference, so it never
// touches a person/entity node and never trips the interaction→venue restrict FK
// for a venue still referenced by an un-cleaned interaction.
func (r *SyntheticSupportRepository) DeleteVenueNodesByIds(ctx context.Context, nodeIDs []uuid.UUID) (int64, error) {
	if len(nodeIDs) == 0 {
		return 0, nil
	}
	return r.queries.SyntheticDeleteVenueNodesByIds(ctx, pgUUIDs(nodeIDs))
}

// CountVenueNodesByIds counts surviving venue nodes among the given ids (cleanup
// assertion, scoped to the run's tracked ids so it is parallel-test-safe).
func (r *SyntheticSupportRepository) CountVenueNodesByIds(ctx context.Context, nodeIDs []uuid.UUID) (int64, error) {
	if len(nodeIDs) == 0 {
		return 0, nil
	}
	return r.queries.SyntheticCountVenueNodesByIds(ctx, pgUUIDs(nodeIDs))
}

// DeleteAssertionsForNode hard-deletes the assertions touching a node in either
// position (provenance cascades). The assertion → node FK is restrict, so a test
// MUST clear its assertions before deleting its nodes — register this cleanup to
// run BEFORE DeleteNodesByLabelPrefix (i.e. register it LAST, since t.Cleanup is
// LIFO).
func (r *SyntheticSupportRepository) DeleteAssertionsForNode(ctx context.Context, nodeID uuid.UUID) (int64, error) {
	return r.queries.SyntheticDeleteAssertionsForNode(ctx, pgtype.UUID{Bytes: nodeID, Valid: true})
}

// InsertContactAtID inserts a contact row AT a caller-supplied id (node.id ==
// contact.id), so a latent-person promotion test can turn a latent person node
// into a real contact at the node's id. The production import/promotion pipeline
// that supplies the id is deferred per spec; this is the test-only mechanic.
func (r *SyntheticSupportRepository) InsertContactAtID(ctx context.Context, id uuid.UUID, fullName string) error {
	return r.queries.TestInsertContactAtID(ctx, db.TestInsertContactAtIDParams{
		ID:       pgtype.UUID{Bytes: id, Valid: true},
		FullName: fullName,
	})
}

// InsertTagForMigration seeds a legacy tag row with an explicit name + color (a
// nil color round-trips as SQL NULL), returning the generated id, so a
// --migrate-tags test can assert the color survives into the tag entity node's
// detail JSONB.
func (r *SyntheticSupportRepository) InsertTagForMigration(ctx context.Context, name string, color *string) (uuid.UUID, error) {
	id, err := r.queries.TestInsertTagForMigration(ctx, db.TestInsertTagForMigrationParams{
		Name:  name,
		Color: stringToPgText(color),
	})
	if err != nil {
		return uuid.Nil, err
	}
	return uuid.UUID(id.Bytes), nil
}

// InsertContactTagAtTime seeds a contact_tag row with an explicit created_at so a
// --migrate-tags test can assert the migration preserves it as the assertion's
// KnowledgeFrom (KnowledgeFromOverride).
func (r *SyntheticSupportRepository) InsertContactTagAtTime(ctx context.Context, contactID, tagID uuid.UUID, createdAt time.Time) error {
	return r.queries.TestInsertContactTagAtTime(ctx, db.TestInsertContactTagAtTimeParams{
		ContactID: pgtype.UUID{Bytes: contactID, Valid: true},
		TagID:     pgtype.UUID{Bytes: tagID, Valid: true},
		CreatedAt: pgtype.Timestamptz{Time: createdAt, Valid: true},
	})
}

// InsertContactTagNullCreatedAt seeds a contact_tag row with a NULL created_at
// (the column is nullable), so a --migrate-tags test can assert the migration
// falls back to "now" for the assertion knowledge time rather than a zero time.
func (r *SyntheticSupportRepository) InsertContactTagNullCreatedAt(ctx context.Context, contactID, tagID uuid.UUID) error {
	return r.queries.TestInsertContactTagNullCreatedAt(ctx, db.TestInsertContactTagNullCreatedAtParams{
		ContactID: pgtype.UUID{Bytes: contactID, Valid: true},
		TagID:     pgtype.UUID{Bytes: tagID, Valid: true},
	})
}

// DeleteContactTagsByContactIds hard-deletes the contact_tag rows a test seeded,
// keyed by the tracked contact ids.
func (r *SyntheticSupportRepository) DeleteContactTagsByContactIds(ctx context.Context, contactIDs []uuid.UUID) (int64, error) {
	if len(contactIDs) == 0 {
		return 0, nil
	}
	return r.queries.TestDeleteContactTagsByContactIds(ctx, pgUUIDs(contactIDs))
}

// DeleteTagsByIds hard-deletes the legacy tag rows a test seeded, keyed by the
// tracked tag ids.
func (r *SyntheticSupportRepository) DeleteTagsByIds(ctx context.Context, tagIDs []uuid.UUID) (int64, error) {
	if len(tagIDs) == 0 {
		return 0, nil
	}
	return r.queries.TestDeleteTagsByIds(ctx, pgUUIDs(tagIDs))
}

// CountTaggedAsAssertionsForSubject counts the LIVE accepted `tagged_as`
// assertions whose subject is a given node, so a test asserts exactly one per
// migrated contact_tag and that an idempotent re-run creates no duplicates.
func (r *SyntheticSupportRepository) CountTaggedAsAssertionsForSubject(ctx context.Context, subjectNodeID uuid.UUID) (int64, error) {
	return r.queries.TestCountTaggedAsAssertionsForSubject(ctx, pgtype.UUID{Bytes: subjectNodeID, Valid: true})
}

// --- relationship_signal support (graph derived-storage) -------------------

// UpsertRelationshipSignal writes one relationship_signal row through the
// PRODUCTION UpsertRelationshipSignal query (storage-only — SP1 has no signal
// generators, so the seed direct-writes the projection the way SP3 eventually
// will). Idempotent on (subject_node_id, signal_key): a re-seed overwrites the
// value + watermarks. computed_at is set by the query (NOW()); as_of is supplied
// by the caller (anchor-relative, no time.Now()).
func (r *SyntheticSupportRepository) UpsertRelationshipSignal(ctx context.Context, subjectNodeID uuid.UUID, signalKey string, value float64, asOf time.Time, methodVersion string) error {
	return r.queries.UpsertRelationshipSignal(ctx, db.UpsertRelationshipSignalParams{
		SubjectNodeID: uuidToPgUUID(subjectNodeID),
		SignalKey:     signalKey,
		Value:         value,
		AsOf:          pgtype.Timestamptz{Time: asOf, Valid: true},
		MethodVersion: methodVersion,
	})
}

// DeleteRelationshipSignalsForNodes hard-deletes the relationship_signal rows a
// profile seeded on the given subject nodes (cleanup). relationship_signal has no
// deleted_at and its subject_node_id → node FK is NO ACTION, so the teardown MUST
// run this BEFORE the node deletes. Keyed by the tracked node ids.
func (r *SyntheticSupportRepository) DeleteRelationshipSignalsForNodes(ctx context.Context, nodeIDs []uuid.UUID) (int64, error) {
	if len(nodeIDs) == 0 {
		return 0, nil
	}
	return r.queries.SyntheticDeleteRelationshipSignalsForNodes(ctx, pgUUIDs(nodeIDs))
}

// CountRelationshipSignalsForNodes counts surviving relationship_signal rows for
// the given subject nodes (scoped to the run's tracked node ids, so it is
// parallel-test-safe). Used to assert ≥1 signal exists for the seeded nodes
// (coverage) and that 0 remain after teardown.
func (r *SyntheticSupportRepository) CountRelationshipSignalsForNodes(ctx context.Context, nodeIDs []uuid.UUID) (int64, error) {
	if len(nodeIDs) == 0 {
		return 0, nil
	}
	return r.queries.SyntheticCountRelationshipSignalsForNodes(ctx, pgUUIDs(nodeIDs))
}

// CountLiveNodesByIds counts the live (deleted_at IS NULL) nodes among the given
// ids, scoped to the run's tracked node ids. Used by the merge/soft-delete coverage
// check to assert a merge winner stays live (== id count) while soft-deleted +
// merge-loser nodes are tombstoned (==0).
func (r *SyntheticSupportRepository) CountLiveNodesByIds(ctx context.Context, nodeIDs []uuid.UUID) (int64, error) {
	if len(nodeIDs) == 0 {
		return 0, nil
	}
	return r.queries.SyntheticCountLiveNodesByIds(ctx, pgUUIDs(nodeIDs))
}

// --- declared-seeding support (internal/synthetic/declare) -----------------
//
// The declared-seed endpoint seeds in one request and cleans up in a LATER one,
// so the harness's in-memory id ledger is gone by cleanup time. These wrappers
// back the stateless, namespace-recoverable path: id sets rebuilt from the
// namespace's own generator-derived tokens, a Gate-B-parity safety gate, and
// the session advisory locks that serialize concurrent runs.
//
// The SELECTs deliberately include soft-deleted rows — see the soft-delete
// exception documented on the queries themselves.

// SelectContactIDsByFullNamePrefix returns every contact (tombstones included)
// whose full_name carries the namespace prefix. Caller passes a BARE prefix.
func (r *SyntheticSupportRepository) SelectContactIDsByFullNamePrefix(ctx context.Context, prefix string) ([]uuid.UUID, error) {
	rows, err := r.queries.SyntheticSelectContactIdsByFullNamePrefix(ctx, pgtype.Text{String: prefix, Valid: true})
	if err != nil {
		return nil, err
	}
	return pgUUIDsToUUIDs(rows), nil
}

// SelectVenueNodeIDsForContacts returns the venue nodes referenced by these
// contacts' interactions. Must be read BEFORE the interaction delete — that
// delete removes the only link back to them.
func (r *SyntheticSupportRepository) SelectVenueNodeIDsForContacts(ctx context.Context, contactIDs []uuid.UUID) ([]uuid.UUID, error) {
	if len(contactIDs) == 0 {
		return nil, nil
	}
	rows, err := r.queries.SyntheticSelectVenueNodeIdsForContacts(ctx, pgUUIDs(contactIDs))
	if err != nil {
		return nil, err
	}
	return pgUUIDsToUUIDs(rows), nil
}

// DeleteInteractionsByContactIds hard-deletes interactions by contact.
func (r *SyntheticSupportRepository) DeleteInteractionsByContactIds(ctx context.Context, contactIDs []uuid.UUID) (int64, error) {
	if len(contactIDs) == 0 {
		return 0, nil
	}
	return r.queries.SyntheticDeleteInteractionsByContactIds(ctx, pgUUIDs(contactIDs))
}

// SelectMacHostIDByHostname looks up a synthetic host by EXACT hostname. The
// bool reports existence; a missing host is not an error (an un-seeded or
// already-cleaned namespace legitimately has none).
func (r *SyntheticSupportRepository) SelectMacHostIDByHostname(ctx context.Context, hostname string) (uuid.UUID, bool, error) {
	row, err := r.queries.SyntheticSelectMacHostIdByHostname(ctx, hostname)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return uuid.Nil, false, nil
		}
		return uuid.Nil, false, err
	}
	if !row.Valid {
		return uuid.Nil, false, nil
	}
	return uuid.UUID(row.Bytes), true, nil
}

// SelectLiveDescendantHostnames returns hostnames of namespaces nested under
// this one. Caller passes a BARE 'synth-<ns>-' prefix and filters out the
// namespace's own salt variants (which are expansion members, not descendants).
func (r *SyntheticSupportRepository) SelectLiveDescendantHostnames(ctx context.Context, namespacePrefix string) ([]string, error) {
	return r.queries.SyntheticSelectLiveDescendantHostnames(ctx, pgtype.Text{String: namespacePrefix, Valid: true})
}

// CountPendingJobsForNamespaceCleanup counts unfinalized River jobs still
// referencing this namespace, across BOTH Gate-B linkage classes. Zero is the
// precondition for deleting anything.
func (r *SyntheticSupportRepository) CountPendingJobsForNamespaceCleanup(ctx context.Context, eventIDs, contactIDs []uuid.UUID) (int64, error) {
	if len(eventIDs) == 0 && len(contactIDs) == 0 {
		return 0, nil
	}
	return r.queries.SyntheticCountPendingJobsForNamespaceCleanup(ctx, db.SyntheticCountPendingJobsForNamespaceCleanupParams{
		EventIds:   contactIDStrings(eventIDs),
		ContactIds: contactIDStrings(contactIDs),
	})
}

// --- namespace ownership -----------------------------------------------------
//
// Cross-request cleanup recovers most id sets from a generator-derived token
// carried by the row itself. For contacts that token is full_name, which the
// application lets a user rewrite (and rewrites node.canonical_label with it),
// so a renamed seeded contact is invisible to every name-derived sweep. These
// three wrap the durable, id-keyed ownership record that closes that hole.

// Ownership-record kinds. They grow with the declarable vocabulary.
const (
	// EntityKindContact is a seeded contact, whose recoverable token (full_name)
	// the application lets a user rewrite.
	EntityKindContact = "contact"
	// EntityKindExternalContact is a seeded import candidate. Three of the seven
	// declarable sources have a production source_id that carries no
	// namespace-prefixed string at all — a decimal telegram peer id, a SHA-256
	// (token ‖ session) digest — so for those the prefix sweep has nothing to match
	// and this record is the ONLY way cleanup can find the row.
	EntityKindExternalContact = "external_contact"
)

// RecordNamespaceEntity records that a namespace created an entity. Idempotent.
func (r *SyntheticSupportRepository) RecordNamespaceEntity(ctx context.Context, namespace, kind string, entityID uuid.UUID) error {
	return r.queries.SyntheticRecordNamespaceEntity(ctx, db.SyntheticRecordNamespaceEntityParams{
		Namespace:  namespace,
		EntityKind: kind,
		EntityID:   uuidToPgUUID(entityID),
	})
}

// SelectNamespaceEntityIDs returns the ids a namespace recorded for one kind.
func (r *SyntheticSupportRepository) SelectNamespaceEntityIDs(ctx context.Context, namespace, kind string) ([]uuid.UUID, error) {
	rows, err := r.queries.SyntheticSelectNamespaceEntityIds(ctx, db.SyntheticSelectNamespaceEntityIdsParams{
		Namespace:  namespace,
		EntityKind: kind,
	})
	if err != nil {
		return nil, err
	}
	return pgUUIDsToUUIDs(rows), nil
}

// DeleteNamespaceEntities drops a namespace's ownership records.
func (r *SyntheticSupportRepository) DeleteNamespaceEntities(ctx context.Context, namespace string) (int64, error) {
	return r.queries.SyntheticDeleteNamespaceEntities(ctx, namespace)
}

// DeleteUnfinalizedJobsInQueue drops the namespace's ORPHANED River jobs — the
// unfinalized ones sitting in the private queue whose client is gone. Scoped to
// that one queue, so the live application's `default`-queue jobs still gate the
// sweep. Caller must hold the namespace reservation.
func (r *SyntheticSupportRepository) DeleteUnfinalizedJobsInQueue(ctx context.Context, queue string, eventIDs, contactIDs []uuid.UUID) (int64, error) {
	if len(eventIDs) == 0 && len(contactIDs) == 0 {
		return 0, nil
	}
	return r.queries.SyntheticDeleteUnfinalizedJobsInQueue(ctx, db.SyntheticDeleteUnfinalizedJobsInQueueParams{
		Queue:      queue,
		EventIds:   contactIDStrings(eventIDs),
		ContactIds: contactIDStrings(contactIDs),
	})
}

// DeleteRiverQueue drops the river_queue row a harness producer left behind.
func (r *SyntheticSupportRepository) DeleteRiverQueue(ctx context.Context, name string) (int64, error) {
	return r.queries.SyntheticDeleteRiverQueue(ctx, name)
}

// CountRiverQueue reports whether a namespace's private queue row survives
// (cleanup assertion).
func (r *SyntheticSupportRepository) CountRiverQueue(ctx context.Context, name string) (int64, error) {
	return r.queries.SyntheticCountRiverQueue(ctx, name)
}

// CountEventConsumerClaimsByEventIds counts surviving claims for a set of event
// ids (cleanup assertion). The caller must capture the ids BEFORE the sweep —
// once the events are gone nothing can re-derive them.
func (r *SyntheticSupportRepository) CountEventConsumerClaimsByEventIds(ctx context.Context, eventIDs []uuid.UUID) (int64, error) {
	if len(eventIDs) == 0 {
		return 0, nil
	}
	return r.queries.SyntheticCountEventConsumerClaimsByEventIds(ctx, pgUUIDs(eventIDs))
}

// TryAdvisoryLock takes a NON-BLOCKING session advisory lock; false means
// another session holds it. MUST run on a dedicated connection (session locks
// belong to the connection that took them).
func (r *SyntheticSupportRepository) TryAdvisoryLock(ctx context.Context, key int64) (bool, error) {
	return r.queries.SyntheticTryAdvisoryLock(ctx, key)
}

// AdvisoryLock takes a BLOCKING session advisory lock, bounded by ctx. Same
// dedicated-connection rule as TryAdvisoryLock.
func (r *SyntheticSupportRepository) AdvisoryLock(ctx context.Context, key int64) error {
	return r.queries.SyntheticAdvisoryLock(ctx, key)
}

// AdvisoryUnlock releases a session advisory lock on the same connection.
func (r *SyntheticSupportRepository) AdvisoryUnlock(ctx context.Context, key int64) (bool, error) {
	return r.queries.SyntheticAdvisoryUnlock(ctx, key)
}

// InsertUnfinalizedRecorderJobForEvent plants an event-linked unfinalized job.
// Failure-injection fixture for the declared-seeding tests only.
func (r *SyntheticSupportRepository) InsertUnfinalizedRecorderJobForEvent(ctx context.Context, eventID uuid.UUID) (int64, error) {
	return r.queries.TestInsertUnfinalizedRecorderJobForEvent(ctx, eventID.String())
}

// InsertUnfinalizedAggregateJobForContact plants a contact-keyed unfinalized
// aggregate job (the linkage class carrying no event id). Fixture only.
func (r *SyntheticSupportRepository) InsertUnfinalizedAggregateJobForContact(ctx context.Context, contactID uuid.UUID, source string) (int64, error) {
	return r.queries.TestInsertUnfinalizedAggregateJobForContact(ctx, db.TestInsertUnfinalizedAggregateJobForContactParams{
		ContactID: contactID.String(),
		Source:    source,
	})
}

// FinalizeRiverJobByID finalizes a planted job so a "refuses while pending,
// cleans once safe" test has an explicit safe-state step. Fixture only.
func (r *SyntheticSupportRepository) FinalizeRiverJobByID(ctx context.Context, id int64) error {
	return r.queries.TestFinalizeRiverJobByID(ctx, id)
}

// InsertTelegramChatConfigInBand occupies a telegram peer band with a
// contact-less row, so a test can force the toolkit's band-collision re-salt
// cheaply. Fixture only.
func (r *SyntheticSupportRepository) InsertTelegramChatConfigInBand(ctx context.Context, chatID int64) error {
	return r.queries.TestInsertTelegramChatConfigInBand(ctx, chatID)
}

// InsertAvailableRematchJobForContact plants an immediately fetchable
// `default`-queue rematch_dispatcher job. It belongs to the live application;
// a harness that fetched it would be stealing its work. Fixture only.
// RiverJobDisposition is a planted job's outcome: its state, whether it is
// finalized, and its attempt counter. Attempt is what makes "never fetched"
// observable — River increments it when a job is handed to a worker.
type RiverJobDisposition struct {
	State     string
	Finalized bool
	Attempt   int32
}

// GetRiverJobDisposition reads a planted job's disposition. Fixture only.
func (r *SyntheticSupportRepository) GetRiverJobDisposition(ctx context.Context, id int64) (RiverJobDisposition, error) {
	row, err := r.queries.TestGetRiverJobDispositionByID(ctx, id)
	if err != nil {
		return RiverJobDisposition{}, err
	}
	return RiverJobDisposition{
		State:     row.State,
		Finalized: row.Finalized,
		Attempt:   row.Attempt,
	}, nil
}

func (r *SyntheticSupportRepository) InsertAvailableRematchJobForContact(ctx context.Context, contactID uuid.UUID) (int64, error) {
	return r.queries.TestInsertAvailableRematchJobForContact(ctx, db.TestInsertAvailableRematchJobForContactParams{
		EventID:      uuid.New().String(),
		ContactID:    contactID.String(),
		RematchJobID: uuid.New().String(),
	})
}
