// Package service provides business-logic wrappers around repositories.
// IngestService owns the single-tx batch-publish semantics for the HTTP
// ingestion endpoint introduced in PR 4 of the event-bus-foundation spec.
package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/anarlog"
	"personal-crm/backend/internal/consumer/consumerjobs"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/events"
	"personal-crm/backend/internal/identity"
	"personal-crm/backend/internal/logger"
	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
	"github.com/gowebpki/jcs"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
)

// Per-event rejection codes surfaced through perEventRejections so the
// HTTP handler can translate them into the response's errors[] array.
const (
	ingestRejectUnsupportedHostAuthKind           = "UNSUPPORTED_HOST_AUTH_KIND"
	ingestRejectHostOnlyRequiresHost              = "HOST_ONLY_REQUIRES_HOST_AUTH"
	ingestRejectPayloadInvariant                  = "PAYLOAD_INVARIANT"
	ingestRejectPayloadInvalid                    = "PAYLOAD_INVALID"
	ingestRejectIdentityMatchFailed               = "IDENTITY_MATCH_FAILED"
	ingestRejectStagingUpsertFailed               = "STAGING_UPSERT_FAILED"
	ingestRejectExternalContactGetFailed          = "EXTERNAL_CONTACT_GET_FAILED"
	ingestRejectExternalContactUpsertFailed       = "EXTERNAL_CONTACT_UPSERT_FAILED"
	ingestRejectExternalContactReviveFailed       = "EXTERNAL_CONTACT_REVIVE_FAILED"
	ingestRejectExternalContactUpdateMatchFailed  = "EXTERNAL_CONTACT_UPDATE_MATCH_FAILED"
	ingestRejectExternalContactDeleteFailed       = "EXTERNAL_CONTACT_DELETE_FAILED"
	ingestRejectExternalContactHashMismatch       = "EXTERNAL_CONTACT_HASH_MISMATCH"
	ingestRejectExternalContactDeleteHashMismatch = "EXTERNAL_CONTACT_DELETE_HASH_MISMATCH"
	ingestRejectMeetingNoteUpsertFailed           = "MEETING_NOTE_UPSERT_FAILED"
	ingestRejectMeetingNoteDeleteFailed           = "MEETING_NOTE_DELETE_FAILED"
	ingestRejectMeetingNoteHashMismatch           = "MEETING_NOTE_HASH_MISMATCH"
	ingestRejectMeetingNoteDeleteHashMismatch     = "MEETING_NOTE_DELETE_HASH_MISMATCH"
	ingestRejectLinkageQueryFailed                = "LINKAGE_QUERY_FAILED"
	ingestRejectParticipantResolveFailed          = "PARTICIPANT_RESOLVE_FAILED"
	ingestRejectInteractionWriteFailed            = "INTERACTION_WRITE_FAILED"
	ingestRejectTitleMatchFailed                  = "TITLE_MATCH_FAILED"
	ingestRejectTitleDiscoveryUpsertFailed        = "TITLE_DISCOVERY_UPSERT_FAILED"
)

// ErrHostRevokedDuringBatch is returned by IngestBatch when the
// tx-internal host-liveness re-check (SELECT ... FOR UPDATE on
// mac_host) finds the host revoked between auth-middleware validation
// and the batch's lock acquire. The handler translates this to
// 401 UNKNOWN_HOST, matching the cursor-commit precedent in
// MacHostHandler.CommitCursor.
var ErrHostRevokedDuringBatch = errors.New("host revoked during ingest batch")

// reExternalContactUpsertSourceID matches the envelope SourceID shape
// for external_contact.upserted events: `<entity_uuid>@<sha256-hex>`.
// The entity portion may not contain `@`; the hash is exactly 64 hex
// characters (SHA-256). Compiled once at package load.
var reExternalContactUpsertSourceID = regexp.MustCompile(`^[^@]+@[a-f0-9]{64}$`)

// reExternalContactDeleteSourceID matches the envelope SourceID shape
// for external_contact.deleted events:
// `<entity_uuid>@deleted@<sha256-hex>` or `<entity_uuid>@deleted@unknown`.
var reExternalContactDeleteSourceID = regexp.MustCompile(`^[^@]+@deleted@([a-f0-9]{64}|unknown)$`)

// reMeetingNoteUpsertSourceID matches the envelope SourceID shape for
// meeting_note.recorded events: `<session_uuid>@<sha256-hex>`. The
// session-UUID portion is structurally checked by the regex; the
// inline verifier additionally enforces it parses as a UUID.
var reMeetingNoteUpsertSourceID = regexp.MustCompile(`^[^@]+@[a-f0-9]{64}$`)

// reMeetingNoteDeleteSourceID matches the envelope SourceID shape for
// meeting_note.deleted events: `<session_uuid>@deleted@<sha256-hex>` or
// `<session_uuid>@deleted@unknown`.
var reMeetingNoteDeleteSourceID = regexp.MustCompile(`^[^@]+@deleted@([a-f0-9]{64}|unknown)$`)

// meetingNoteDeleteUnknownSuffix is the spec-defined fallback suffix
// on meeting_note.deleted source_ids when the daemon has no local
// cache of the prior content hash. Mirrors externalContactDeleteUnknownSuffix.
const meetingNoteDeleteUnknownSuffix = "@deleted@unknown"

// IngestPerEventRejection is a per-event domain rejection emitted by the
// service-layer inline handlers. Index is the caller-original index from
// originalIndices, NOT the compacted in-service position — so the HTTP
// handler can surface it directly in the response's errors[] field.
type IngestPerEventRejection struct {
	Index   int
	Code    string
	Message string
}

// NeedsAttentionItem is a service-layer record surfaced to the daemon
// after a meeting_note.recorded event lands in a state that requires
// user attention (conflict_pending or orphan_needs_review). The
// dispatch loop appends one item per qualifying event AFTER the
// per-event savepoint commits successfully — never on the rollback
// path. The handler maps this to the HTTP response's needs_attention
// field for the daemon to consume.
type NeedsAttentionItem struct {
	SessionID string
	Reason    string
}

// NeedsAttentionReason* are the canonical reason strings for
// NeedsAttentionItem. The daemon pattern-matches on these.
const (
	NeedsAttentionReasonConflict = "conflict"
	NeedsAttentionReasonOrphan   = "orphan"
)

// JobInsertTxer is the narrow surface for transactional River inserts.
// Concrete is *river.Client[pgx.Tx]. Interface keeps the service
// testable.
type JobInsertTxer interface {
	InsertTx(ctx context.Context, tx pgx.Tx, args river.JobArgs, opts *river.InsertOpts) (*rivertype.JobInsertResult, error)
}

// IdentityMatcher is the narrow surface for the per-event identity
// match call. Concrete is *IdentityService.
type IdentityMatcher interface {
	MatchOrCreateTx(ctx context.Context, tx pgx.Tx, req MatchRequest, policy NormalizationPolicy) (*MatchResult, error)
}

// MessagesUpserter is the narrow surface for the per-event staging
// upsert. Concrete is *repository.MessagesMessageRepository.
type MessagesUpserter interface {
	UpsertMessageTx(ctx context.Context, tx pgx.Tx, params repository.UpsertMessagesMessageParams) (*repository.MessagesMessage, error)
}

// ExternalContactWriter is the narrow surface for the per-event
// external_contact.* inline handlers. Concrete is
// *repository.ExternalContactRepository. The interface keeps the
// service testable — tests substitute a stub that errors at chosen
// methods so the savepoint-rollback path can be exercised without a
// live DB.
type ExternalContactWriter interface {
	GetBySourceTx(ctx context.Context, tx pgx.Tx, source, sourceID string, accountID *string) (*repository.ExternalContact, error)
	UpsertTx(ctx context.Context, tx pgx.Tx, req repository.UpsertExternalContactRequest) (*repository.ExternalContact, error)
	UpdateMatchTx(ctx context.Context, tx pgx.Tx, id uuid.UUID, crmContactID *uuid.UUID, status repository.MatchStatus) (*repository.ExternalContact, error)
	ReviveTx(ctx context.Context, tx pgx.Tx, id uuid.UUID) (*repository.ExternalContact, error)
	SoftDeleteTx(ctx context.Context, tx pgx.Tx, id uuid.UUID) error
}

// AddressBookReconciler is the narrow surface the external_contact
// upserted handler uses to re-propagate methods onto an already-linked
// (or dup-of-linked) contact AFTER the batch tx commits. icloud is
// push-only with no periodic resync, so this post-commit hook is the
// ONLY forward path that closes the "icloud enriches nothing" leak —
// it must fire on FIRST MATCH too, not only re-upsert. Optional: nil is
// supported for tests that don't exercise the reconcile; the handler
// then skips scheduling the post-commit closure. Concrete is
// *AddressBookReconcileService (+ *repository.ExternalContactRepository
// for target resolution); see the addressBookReconcilerAdapter wired in
// main.go.
type AddressBookReconciler interface {
	// ResolveAndReconcile re-reads the committed row by id, resolves its
	// effective contact + status (duplicate-aware precedence), and
	// reconciles. A no-op for unmatched / ignored / unresolved rows. Runs
	// on the default pool (never inside the ingest tx).
	ResolveAndReconcile(ctx context.Context, externalID uuid.UUID) error
}

// HostLivenessChecker is the narrow surface IngestService needs to
// acquire the row-level write lock on the authed host's mac_host row
// inside the batch tx. Concrete is *repository.MacHostRepository. The
// SELECT ... FOR UPDATE the method runs blocks any concurrent revoke
// of the same row until the batch tx commits or rolls back. Returns
// db.ErrNotFound when the host has been revoked.
type HostLivenessChecker interface {
	GetActiveHostByIDForUpdateTx(ctx context.Context, tx pgx.Tx, id uuid.UUID) (*repository.MacHost, error)
}

// MeetingNoteWriter is the narrow surface for the meeting_note.*
// inline handlers. Concrete is *repository.MeetingNoteRepository.
type MeetingNoteWriter interface {
	GetMeetingNoteBySessionIDForUpdateTx(ctx context.Context, tx pgx.Tx, sessionID uuid.UUID) (*repository.MeetingNote, error)
	GetTombstonedMeetingNoteBySessionIDTx(ctx context.Context, tx pgx.Tx, sessionID uuid.UUID) (*repository.MeetingNote, error)
	InsertMeetingNoteTx(ctx context.Context, tx pgx.Tx, params repository.InsertMeetingNoteParams) (*repository.MeetingNote, error)
	UpdateMeetingNoteOnResyncTx(ctx context.Context, tx pgx.Tx, params repository.UpdateMeetingNoteOnResyncParams) (*repository.MeetingNote, error)
	ReviveMeetingNoteTx(ctx context.Context, tx pgx.Tx, params repository.ReviveMeetingNoteParams) (*repository.MeetingNote, error)
	SoftDeleteMeetingNoteBySessionIDTx(ctx context.Context, tx pgx.Tx, sessionID uuid.UUID) error
}

// CalendarLinkageReader is the narrow surface the meeting_note
// handler needs to enumerate candidate calendar events in a time
// window. Concrete is *repository.CalendarEventRepository.
type CalendarLinkageReader interface {
	FindLinkageCandidatesTx(ctx context.Context, tx pgx.Tx, windowStart, windowEnd time.Time) ([]repository.LinkageCandidate, error)
}

// PhoneCallLinkageReader is the narrow surface the meeting_note
// handler needs to enumerate candidate phone_call rows in a time
// window. Concrete is *repository.PhoneCallRepository. Optional —
// nil is supported for tests that don't exercise the phone_call
// linkage path; the inline handler skips the phone_call query when
// the dep is nil.
type PhoneCallLinkageReader interface {
	FindLinkageCandidatesTx(ctx context.Context, tx pgx.Tx, windowStart, windowEnd time.Time) ([]repository.LinkageCandidate, error)
}

// InteractionWriter is the narrow surface for the re-sync diff path
// (list session-attributed live interactions, soft-delete obsoletes)
// and the meeting_note.deleted cascade. Refreshes of existing
// interactions go through ContactInteractionRecorder.ExtendInteractionTx
// instead so cadence stays under the sole-writer invariant. Concrete
// is *repository.InteractionRepository.
type InteractionWriter interface {
	ListSessionAttributedInteractionsTx(ctx context.Context, tx pgx.Tx, sourceRefPrefix string) ([]repository.Interaction, error)
	SoftDeleteInteractionTx(ctx context.Context, tx pgx.Tx, id uuid.UUID) error
}

// AnarlogIdentityLookup is the narrow surface used by the
// meeting_note.recorded handler to resolve payload participant_ids
// (anarlog_human UUIDs) into CRM contact_ids. Concrete is
// *repository.IdentityRepository. Also covers the LinkTx variant used
// by handleExternalContactUpserted to wire the anarlog identity row's
// contact_id when the underlying external_contact's email/phone
// resolved a contact.
type AnarlogIdentityLookup interface {
	FindContactIDByAnarlogHumanIDTx(ctx context.Context, tx pgx.Tx, anarlogHumanID string) (*uuid.UUID, error)
	LinkIdentityToContactTx(ctx context.Context, tx pgx.Tx, req repository.LinkIdentityRequest) (*repository.ExternalIdentity, error)
}

// ContactInteractionRecorder is the narrow surface for routing
// session-attributed interaction writes through the higher-level
// ContactService methods (so cadence updates + follow-up evaluation
// fire correctly on both inserts and refreshes). Concrete is *ContactService.
type ContactInteractionRecorder interface {
	RecordInteractionTx(ctx context.Context, tx pgx.Tx, publishesEvent bool, req repository.RecordInteractionRequest) (*RecordInteractionResult, error)
	ExtendInteractionTx(ctx context.Context, tx pgx.Tx, interactionID, contactID uuid.UUID, direction string, occurredAt time.Time, description *string) (func(context.Context), error)
}

// PhoneCallWriter is the narrow surface for the per-event phone_call
// staging path. Concrete is *repository.PhoneCallRepository.
type PhoneCallWriter interface {
	UpsertCallTx(ctx context.Context, tx pgx.Tx, params repository.UpsertPhoneCallParams) (*repository.PhoneCall, error)
	MarkProcessedTx(ctx context.Context, tx pgx.Tx, params repository.MarkProcessedParams) error
}

// ContactRecorder is the narrow surface IngestService needs for the
// inline interaction.recorded path. Concrete is *ContactService.
// publishesEvent=true populates res.PrevCadence + res.CadenceAtEmit on
// the returned result so the caller can attach them to the
// interaction.recorded event it publishes itself; the service does NOT
// publish the event (mirrors consumer/interaction_recorder.go).
type ContactRecorder interface {
	RecordInteractionTx(
		ctx context.Context, tx pgx.Tx, publishesEvent bool, req repository.RecordInteractionRequest,
	) (*repository.RecordInteractionResult, error)
}

// CadenceApplier is the narrow surface IngestService needs to inline-
// apply cadence after publishing interaction.recorded. Concrete is
// *consumer.CadenceUpdater.
type CadenceApplier interface {
	HandleEvent(ctx context.Context, tx pgx.Tx, env *events.Envelope) error
}

// FollowUpApplier is the narrow surface IngestService needs to inline-
// apply follow-up after publishing interaction.recorded. Concrete is
// *consumer.FollowUpManager. Returns a post-commit closure (non-nil on
// the refresh branch) that the caller folds into a batch-level
// post-commit slice so the Todoist item_update runs outside the tx.
type FollowUpApplier interface {
	HandleEvent(ctx context.Context, tx pgx.Tx, env *events.Envelope) (postCommit func(context.Context), err error)
}

// IngestTitleMatcher is the narrow surface the meeting_note.recorded
// handler uses to disambiguate a single title-extracted name token to
// a CRM contact. Concrete is *anarlog.TitleMatcher.
type IngestTitleMatcher interface {
	MatchTitleToken(ctx context.Context, token string) (*repository.ContactMatch, error)
}

// IngestTitleDiscoveryWriter is the narrow surface the meeting_note
// handler uses to persist anarlog_title weak-candidate rows for tokens
// that didn't resolve to an existing CRM contact. Concrete is
// *anarlog.DiscoveryWriter.
type IngestTitleDiscoveryWriter interface {
	UpsertTitleCandidateTx(ctx context.Context, tx pgx.Tx, sessionUUID uuid.UUID, normalizedToken, displayToken string) error
}

// IngestService persists pre-validated event envelopes inside a single
// transaction. All envelopes in a batch commit together or roll back
// together — spec §3.5 "all-or-nothing on unexpected errors."
//
// Duplicates (ON CONFLICT hits on the (source, source_id) partial unique
// index) are silent no-ops inside Bus.PublishTx and do NOT abort the tx;
// they're counted separately via the env.ID-sentinel pattern (see
// IngestBatch docs).
//
// For raw_message.* kinds, the service runs an inline per-event handler
// (identity match → staging upsert → aggregator job enqueue) inside a
// per-event savepoint. Per-event domain failures surface as
// IngestPerEventRejection entries; the batch continues. Infrastructure
// failures (begin-tx, publish-tx, savepoint commit, end-of-batch
// aggregator enqueue) abort the whole batch.
type IngestService struct {
	database         *db.Database
	bus              *events.Bus
	identity         IdentityMatcher
	messages         MessagesUpserter
	riverClient      JobInsertTxer
	externalContacts ExternalContactWriter
	hostLiveness     HostLivenessChecker
	meetingNotes     MeetingNoteWriter
	calendar         CalendarLinkageReader
	phoneCallLinkage PhoneCallLinkageReader
	interactions     InteractionWriter
	identityLookup   AnarlogIdentityLookup
	contactSvc       ContactInteractionRecorder
	phoneCalls       PhoneCallWriter
	contactRecorder  ContactRecorder
	cadence          CadenceApplier
	followUp         FollowUpApplier
	titleMatcher     IngestTitleMatcher
	discovery        IngestTitleDiscoveryWriter
	// addressBookReconciler re-propagates address-book methods onto
	// already-linked contacts after the batch commits. Optional (nil-safe);
	// wired post-construction via SetAddressBookReconciler so the big
	// NewIngestService signature is unchanged.
	addressBookReconciler AddressBookReconciler
	// venue resolves the shared-container venue node for phone-call and
	// anarlog-session interactions, so venue_id is set atomically with the
	// insert. Optional (nil-safe); wired post-construction via SetVenueResolver
	// to keep the NewIngestService signature unchanged.
	venue IngestVenueResolver
}

// IngestVenueResolver is the venue-resolution surface the phone-call and
// anarlog-session recorders need: resolve a container the recorder already has
// in hand (phone call_unique_id, anarlog session id), and resolve the meeting
// venue of a linked calendar_event (anarlog→gcal reuse). Satisfied by
// *repository.VenueResolverRegistry.
type IngestVenueResolver interface {
	ResolveVenueForInteractionTx(ctx context.Context, tx pgx.Tx, source, kind, containerKey, title string) (uuid.UUID, error)
	ResolveGCalVenueTx(ctx context.Context, tx pgx.Tx, calendarEventID uuid.UUID) (*uuid.UUID, error)
}

// SetVenueResolver injects the venue resolver. Optional — when unset, the
// phone-call and anarlog recorders record interactions with a NULL venue_id.
// Mirrors SetAddressBookReconciler's post-construction injection.
func (s *IngestService) SetVenueResolver(v IngestVenueResolver) {
	s.venue = v
}

// SetAddressBookReconciler injects the post-commit address-book method
// reconciler. Optional — when unset, handleExternalContactUpserted skips
// scheduling the reconcile closure (icloud method propagation then only
// happens via the one-time catchup subcommand). Must be called before
// the service handles concurrent batches. Mirrors EnrichmentService's
// SetCadenceUpdater injection pattern.
func (s *IngestService) SetAddressBookReconciler(r AddressBookReconciler) {
	s.addressBookReconciler = r
}

// NewIngestService builds an IngestService. Per-kind dependencies may
// be nil — in that case the corresponding inline handler rejects events
// of that kind with PAYLOAD_INVARIANT explaining the missing wiring.
// Production constructs all dependencies; unit tests can pass nils for
// the kinds they don't exercise.
//
// hostLiveness may be nil for tests that don't exercise the host-auth
// path. When nil, the per-batch FOR UPDATE re-check is skipped and the
// batch trusts the auth-middleware's read. Production always wires a
// concrete repository so the race window between auth and commit is
// closed.
//
// meetingNotes/calendar/interactions/identityLookup/contactSvc/
// titleMatcher/discovery are the meeting_note.* inline handler's
// dependency set. Passing nil here is supported for callers that don't
// need the meeting_note path; the handler returns PAYLOAD_INVARIANT for
// missing wiring.
func NewIngestService(
	database *db.Database,
	bus *events.Bus,
	identityMatcher IdentityMatcher,
	messages MessagesUpserter,
	riverClient JobInsertTxer,
	externalContacts ExternalContactWriter,
	hostLiveness HostLivenessChecker,
	meetingNotes MeetingNoteWriter,
	calendar CalendarLinkageReader,
	interactions InteractionWriter,
	identityLookup AnarlogIdentityLookup,
	contactSvc ContactInteractionRecorder,
	phoneCalls PhoneCallWriter,
	contactRecorder ContactRecorder,
	cadence CadenceApplier,
	followUp FollowUpApplier,
	titleMatcher IngestTitleMatcher,
	discovery IngestTitleDiscoveryWriter,
	phoneCallLinkage PhoneCallLinkageReader,
) *IngestService {
	return &IngestService{
		database:         database,
		bus:              bus,
		identity:         identityMatcher,
		messages:         messages,
		riverClient:      riverClient,
		externalContacts: externalContacts,
		hostLiveness:     hostLiveness,
		meetingNotes:     meetingNotes,
		calendar:         calendar,
		phoneCallLinkage: phoneCallLinkage,
		interactions:     interactions,
		identityLookup:   identityLookup,
		contactSvc:       contactSvc,
		phoneCalls:       phoneCalls,
		contactRecorder:  contactRecorder,
		cadence:          cadence,
		followUp:         followUp,
		titleMatcher:     titleMatcher,
		discovery:        discovery,
	}
}

// isHostOnlyKind reports whether the kind is a daemon-push kind (i.e.,
// belongs to one of the daemonFamilies, so it may be submitted ONLY via
// the host-auth path). Derived from the descriptor table — a kind is
// host-only iff it has a kindToFamily entry. Used on BOTH sides of the
// auth dispatch:
//   - on the host-auth path, kinds NOT host-only are rejected as
//     "host-auth doesn't accept this kind"
//   - on the global-API-key path, host-only kinds are rejected as
//     "this kind requires host-auth"
//
// These kinds have exactly one producer — the Mac daemon — so an
// internal Pi publisher writing one would either be a bug or a hostile
// actor with the global key.
func isHostOnlyKind(k events.Kind) bool {
	_, ok := kindToFamily[k]
	return ok
}

// pendingAggregateKey is the (source, contactID) pair used to dedupe
// aggregator enqueues within a single batch.
type pendingAggregateKey struct {
	Source    string
	ContactID uuid.UUID
}

// IngestBatch persists envs in one pgx transaction. Returns
// (accepted, duplicate, perEventRejections, needsAttention, err):
//
//   - accepted: number of envelopes whose INSERT produced a fresh row.
//   - duplicate: number of envelopes whose INSERT hit the (source,
//     source_id) unique index and was silently dropped by the ON CONFLICT
//     DO NOTHING clause.
//   - perEventRejections: per-event domain rejections (host-auth
//     allowlist violation, payload-invariant mismatch, identity-match
//     failure, staging-upsert FK violation). Each entry's Index is the
//     caller-original position from originalIndices, so the handler can
//     append them to the response's errors[] field with correct
//     indexing.
//   - needsAttention: meeting_note.recorded events whose final
//     linkage_state requires user attention (conflict_pending or
//     orphan_needs_review). Appended only after the per-event savepoint
//     commits; nil when no qualifying events occurred.
//   - err: any unexpected DB/infrastructure failure (begin-tx, publish-
//     tx, savepoint commit, end-of-batch aggregator enqueue, outer
//     commit). The whole tx rolls back in this case; all return values
//     are zero on error return.
//
// Precondition: every envelope MUST have env.ID == uuid.Nil on entry
// (the env.ID-sentinel duplicate-detection contract relies on it).
// originalIndices[i] is the caller-original position of envs[i] in the
// caller's pre-validation request array. len(originalIndices) ==
// len(envs); a length mismatch is a caller bug and returns an error.
//
// hostID is the authenticated Mac-host UUID when the request was
// received on the host-auth path; nil on the global-API-key path. The
// service uses hostID to enforce the per-path kind allowlist and
// to stamp staging rows with mac_host_id.
func (s *IngestService) IngestBatch(
	ctx context.Context,
	envs []*events.Envelope,
	originalIndices []int,
	hostID *uuid.UUID,
) (accepted, duplicate int, perEventRejections []IngestPerEventRejection, needsAttention []NeedsAttentionItem, err error) {
	if len(envs) == 0 {
		return 0, 0, nil, nil, nil
	}
	if len(originalIndices) != len(envs) {
		return 0, 0, nil, nil, fmt.Errorf(
			"ingest: originalIndices length %d does not match envs length %d",
			len(originalIndices), len(envs))
	}

	for i, env := range envs {
		if env == nil {
			return 0, 0, nil, nil, fmt.Errorf("ingest: envelope at index %d is nil", i)
		}
		if env.ID != uuid.Nil {
			return 0, 0, nil, nil, fmt.Errorf(
				"ingest: envelope at index %d has a pre-assigned ID; IngestBatch "+
					"requires ID=uuid.Nil so the duplicate sentinel works", i)
		}
	}

	tx, err := s.database.Pool.Begin(ctx)
	if err != nil {
		return 0, 0, nil, nil, fmt.Errorf("begin tx: %w", err)
	}
	// Rollback on any non-commit return. Safe to call after Commit: pgx
	// returns ErrTxClosed which we ignore. If rollback itself fails and no
	// prior error has been recorded, surface it.
	defer func() {
		if rollbackErr := tx.Rollback(ctx); rollbackErr != nil &&
			!errors.Is(rollbackErr, pgx.ErrTxClosed) && err == nil {
			err = fmt.Errorf("rollback: %w", rollbackErr)
		}
	}()

	// Tx-internal host-liveness re-check via SELECT ... FOR UPDATE.
	// Closes the race window between auth-middleware validation and
	// the batch's commit: a concurrent revoke blocks on the same row
	// lock until this batch commits or rolls back, then proceeds. If
	// the host was already revoked when the lock acquired, we abort
	// the batch (nothing commits) and surface ErrHostRevokedDuringBatch
	// so the handler can return 401 UNKNOWN_HOST.
	//
	// Skipped when hostID is nil (global-API-key path) or hostLiveness
	// is nil (test wiring). Production always sets both.
	if hostID != nil && s.hostLiveness != nil {
		if _, livenessErr := s.hostLiveness.GetActiveHostByIDForUpdateTx(ctx, tx, *hostID); livenessErr != nil {
			if errors.Is(livenessErr, db.ErrNotFound) {
				return 0, 0, nil, nil, ErrHostRevokedDuringBatch
			}
			return 0, 0, nil, nil, fmt.Errorf("re-validate host liveness: %w", livenessErr)
		}
	}

	pendingAggregate := make(map[pendingAggregateKey]struct{})

	// batchPostCommit accumulates post-commit closures returned by
	// per-event handlers — meeting_note.recorded routes session
	// interactions through ContactService.RecordInteractionTx whose
	// FollowUpFn fires the FollowUpManager after commit, and the
	// inline call handler returns FollowUpManager.HandleEvent
	// closures. They run AFTER tx.Commit so any external HTTP work
	// (Todoist item_update) does not stall the connection inside the
	// tx.
	var batchPostCommit []func(context.Context)

	for i, env := range envs {
		originalIdx := originalIndices[i]

		// Step 1 — host-auth allowlist enforcement.
		//   * Host-only kinds (raw_message.*, external_contact.*,
		//     meeting_note.*) are allowed ONLY on the host-auth path
		//     (hostID != nil).
		//   * Other kinds (Pi internal publishers) are allowed ONLY on
		//     the global-key path (hostID == nil).
		if hostID != nil {
			if !isHostOnlyKind(env.Kind) {
				perEventRejections = append(perEventRejections, IngestPerEventRejection{
					Index:   originalIdx,
					Code:    ingestRejectUnsupportedHostAuthKind,
					Message: fmt.Sprintf("kind %q not allowed on host-auth path; daemon only emits raw_message.* / external_contact.* / meeting_note.* / call.*", env.Kind),
				})
				continue
			}
		} else if isHostOnlyKind(env.Kind) {
			perEventRejections = append(perEventRejections, IngestPerEventRejection{
				Index:   originalIdx,
				Code:    ingestRejectHostOnlyRequiresHost,
				Message: fmt.Sprintf("kind %q requires X-Mac-Host-ID auth path", env.Kind),
			})
			continue
		}

		// Step 2 — payload-invariant cross-checks (daemon-emitted
		// kinds only). Done BEFORE opening the savepoint so a clean
		// failure doesn't even open one. The handler's pre-validation
		// already enforced required-field presence; we cross-check the
		// envelope/payload pair. Routing is table-driven: the family
		// descriptor owns which verifier runs for the kind.
		if fam, ok := kindToFamily[env.Kind]; ok {
			if rejection := fam.verify(env, *hostID); rejection != nil {
				rejection.Index = originalIdx
				perEventRejections = append(perEventRejections, *rejection)
				continue
			}
		}

		// Step 3 — open a savepoint so per-event domain failures roll
		// back JUST this event's writes (event-log row, identity row,
		// staging row) rather than poisoning the outer batch tx.
		sp, spErr := tx.Begin(ctx)
		if spErr != nil {
			return 0, 0, nil, nil, fmt.Errorf("begin savepoint for index %d (original %d): %w", i, originalIdx, spErr)
		}

		// Step 4 — durable publish to event log. PublishTx is idempotent
		// by (source, source_id): on the partial-unique-index hit the
		// RETURNING clause matches zero rows; the repository leaves
		// env.ID = uuid.Nil so the caller detects duplicate-skip.
		//
		// PublishTx errors are UNEXPECTED (not user-input-shaped). Per
		// the contract, err != nil is reserved for unexpected DB
		// failures so the batch rolls back as a unit; we do NOT degrade
		// to per-event rejection here.
		if pubErr := s.bus.PublishTx(ctx, sp, env); pubErr != nil {
			if rbErr := sp.Rollback(ctx); rbErr != nil {
				return 0, 0, nil, nil, fmt.Errorf("publish event index %d (original %d): %w (rollback: %v)", i, originalIdx, pubErr, rbErr)
			}
			return 0, 0, nil, nil, fmt.Errorf("publish event index %d (original %d): %w", i, originalIdx, pubErr)
		}

		isDuplicate := env.ID == uuid.Nil

		// Step 5 — inline handler for daemon-emitted kinds. On
		// duplicate (Step 4 detected a dedup hit) we normally SKIP the
		// inline handler entirely: re-running identity-match + re-upserting
		// staging on every duplicate would spuriously bump
		// external_identity.last_seen_at and re-add the row to
		// pendingAggregate (raw_message), or re-revive a row whose
		// content hash hasn't actually changed (external_contact).
		// The guard makes a duplicate re-submit a true no-op.
		//
		// Exception for meeting_note.recorded: the spec-defined
		// source_id is `<session_uuid>@<content_hash>`. A tombstoned
		// session that is later re-recorded with identical content
		// produces the SAME source_id and hits the bus-dedup path —
		// without the probe the inline handler is skipped and the row
		// stays tombstoned. The same probe also covers participant
		// resolution drift on live rows after an import.
		runInline := !isDuplicate
		if isDuplicate {
			should, probeErr := s.shouldRunInlineOnDuplicate(ctx, sp, env)
			if probeErr != nil {
				if rbErr := sp.Rollback(ctx); rbErr != nil {
					return 0, 0, nil, nil, fmt.Errorf("probe revive-bypass for index %d (original %d): %w (rollback: %v)", i, originalIdx, probeErr, rbErr)
				}
				return 0, 0, nil, nil, fmt.Errorf("probe revive-bypass for index %d (original %d): %w", i, originalIdx, probeErr)
			}
			runInline = should
		}

		// Per-event optional accumulator items. Captured during the
		// handler's run and appended to the batch-level slice ONLY
		// after the savepoint commits — never on the rollback path
		// (single-point append).
		var pendingNA *NeedsAttentionItem
		// pendingFollowUps holds the post-commit closures returned by
		// ContactService.RecordInteractionTx for any session-attributed
		// interactions written by handleMeetingNoteRecorded. Executed
		// once the outer tx commits so the cadence-task + Todoist side
		// effects fire correctly.
		var pendingFollowUps []func(context.Context)

		if runInline {
			// Inline dispatch is table-driven: the family descriptor owns
			// which handler runs for the kind and threads its outputs back
			// through the uniform dispatch tuple. The post-handler
			// bookkeeping (rollback on rejection, pendingAggregate for
			// raw_message, pendingNA/pendingFollowUps for meeting_note,
			// batchPostCommit for call) is identical to the prior per-arm
			// switch — only the routing is centralized.
			if fam, ok := kindToFamily[env.Kind]; ok {
				contactID, naItem, postCommits, rejection := fam.dispatch(s, ctx, sp, env, *hostID)
				if rejection != nil {
					rejection.Index = originalIdx
					perEventRejections = append(perEventRejections, *rejection)
					if rbErr := sp.Rollback(ctx); rbErr != nil {
						return 0, 0, nil, nil, fmt.Errorf("rollback savepoint for rejected index %d (original %d): %w", i, originalIdx, rbErr)
					}
					continue
				}
				if contactID != nil {
					pendingAggregate[pendingAggregateKey{Source: env.Source, ContactID: *contactID}] = struct{}{}
				}
				pendingNA = naItem
				pendingFollowUps = postCommits
			}
		}

		if commitErr := sp.Commit(ctx); commitErr != nil {
			return 0, 0, nil, nil, fmt.Errorf("commit savepoint %d (original %d): %w", i, originalIdx, commitErr)
		}

		// Post-commit: append the per-event needs_attention item, if any.
		if pendingNA != nil {
			needsAttention = append(needsAttention, *pendingNA)
		}
		// Collect post-commit follow-up closures for execution after
		// the outer tx commits. Discarding them here would silently lose
		// follow-up task side effects on session-attributed interactions.
		if len(pendingFollowUps) > 0 {
			batchPostCommit = append(batchPostCommit, pendingFollowUps...)
		}

		if isDuplicate {
			duplicate++
		} else {
			accepted++
		}
	}

	// Step 6 — end-of-batch aggregator-enqueue. Each enqueue is in the
	// OUTER tx (not a savepoint) — a failure here rolls the whole
	// batch back. Partial-enqueue stranding would be worse than a
	// daemon retry.
	for pair := range pendingAggregate {
		if s.riverClient == nil {
			// No river client wired (test mode). Skip enqueue; the
			// staging rows are still durable so the periodic sweeper
			// will eventually pick them up.
			continue
		}
		args := consumerjobs.MessagingAggregateForContactArgs{
			ContactID: pair.ContactID,
			Source:    pair.Source,
		}
		if _, insErr := s.riverClient.InsertTx(ctx, tx, args, &river.InsertOpts{
			UniqueOpts: consumerjobs.MessagingAggregateUniqueOpts(),
		}); insErr != nil {
			return 0, 0, nil, nil, fmt.Errorf("enqueue messaging aggregate (contact=%s source=%s): %w",
				pair.ContactID, pair.Source, insErr)
		}
	}

	if commitErr := tx.Commit(ctx); commitErr != nil {
		return 0, 0, nil, nil, fmt.Errorf("commit: %w", commitErr)
	}
	// Execute post-commit follow-up closures (FollowUpFn returned by
	// ContactService.RecordInteractionTx for meeting_note.recorded; also
	// FollowUpManager refresh-branch Todoist item_update closures from
	// the inline call handler). Run sequentially AFTER tx.Commit so any
	// external HTTP work doesn't stall the connection inside the tx.
	// Each closure is best-effort: failures are logged inside the
	// closure and do NOT roll back the batch (the interactions already
	// committed).
	for _, fn := range batchPostCommit {
		fn(ctx)
	}
	return accepted, duplicate, perEventRejections, needsAttention, nil
}

// handleRawMessage runs the per-event domain logic for raw_message.*
// envelopes inside the per-event savepoint:
//
//  1. Decode the payload (already structurally validated by handler).
//  2. Detect identifier type (phone vs email) and call IdentityService.
//     MatchOrCreateTx — propagates repo errors so the daemon does not
//     advance its cursor on a transient identity-match failure.
//  3. Upsert messages_message staging via the tx-bound repository.
//  4. Return the matched contactID (nil when unmatched — caller skips
//     aggregator enqueue) and nil rejection on success.
//
// Returns (contactID, rejection): contactID may be nil even on success
// (unmatched peer). rejection != nil means the inline handler refused
// the event for a domain reason; the caller rolls back the savepoint
// and continues the batch.
func (s *IngestService) handleRawMessage(
	ctx context.Context,
	tx pgx.Tx,
	env *events.Envelope,
	hostID uuid.UUID,
) (*uuid.UUID, *IngestPerEventRejection) {
	// The dependencies are constructor-injected; a missing dep here is
	// a wiring bug (the handler should not let raw_message events
	// through when the service wasn't configured for them).
	if s.identity == nil || s.messages == nil {
		return nil, &IngestPerEventRejection{
			Code:    ingestRejectPayloadInvariant,
			Message: "ingest service was not configured for raw_message processing",
		}
	}

	// Re-decode the payload (structurally validated by the handler;
	// verifyRawMessageInvariants ran on the cross-field invariants).
	// Re-decode here rather than passing the decoded struct through —
	// keeps the handler/service contract on []byte payloads, matching
	// the rest of the ingest flow.
	var p events.RawMessageReceivedPayload
	if err := json.Unmarshal(env.Payload, &p); err != nil {
		return nil, &IngestPerEventRejection{
			Code:    ingestRejectPayloadInvalid,
			Message: fmt.Sprintf("decode raw_message payload: %s", err.Error()),
		}
	}

	idType := identity.DetectIdentifierType(p.PeerHandle)
	matchReq := MatchRequest{
		RawIdentifier: p.PeerHandle,
		Type:          idType,
		Source:        env.Source,
		SourceID:      &p.Guid,
		DisplayName:   p.PeerName,
	}
	// FailEmpty: a raw_message carries exactly one peer handle, so an
	// un-normalizable handle is fatal data — reject the event (the
	// daemon holds its cursor for retry) rather than dropping it.
	matchResult, err := s.identity.MatchOrCreateTx(ctx, tx, matchReq, NormalizationFailEmpty)
	if err != nil {
		return nil, &IngestPerEventRejection{
			Code:    ingestRejectIdentityMatchFailed,
			Message: fmt.Sprintf("identity match: %s", err.Error()),
		}
	}

	var peerNormalized *string
	if matchResult.Identity != nil && matchResult.Identity.Identifier != "" {
		s := matchResult.Identity.Identifier
		peerNormalized = &s
	}

	upsertParams := repository.UpsertMessagesMessageParams{
		Guid:             p.Guid,
		ChatGuid:         p.ChatID,
		PeerHandle:       p.PeerHandle,
		PeerNormalized:   peerNormalized,
		Text:             p.Text,
		MessageType:      p.MessageType,
		SentAt:           p.SentAt,
		IsOutgoing:       env.Kind == events.KindRawMessageSent,
		IsGroupChat:      p.IsGroup,
		ReplyToGuid:      p.ReplyToGuid,
		MatchedContactID: matchResult.ContactID,
		MacHostID:        &hostID,
	}
	if _, err := s.messages.UpsertMessageTx(ctx, tx, upsertParams); err != nil {
		return nil, &IngestPerEventRejection{
			Code:    ingestRejectStagingUpsertFailed,
			Message: fmt.Sprintf("upsert messages_message: %s", err.Error()),
		}
	}

	return matchResult.ContactID, nil
}

// verifyRawMessageInvariants enforces the cross-field consistency
// properties that ValidatePayload (lenient unknown-field decode) does
// not. Returns a *IngestPerEventRejection (caller fills in Index) on
// any violation, nil on success.
//
// Properties checked:
//  1. payload.HostID matches the authenticated host (no host
//     cross-impersonation).
//  2. env.Source matches the currently-supported "messages" source.
//  3. payload.Source matches env.Source.
//  4. payload.Guid is non-empty and equals env.SourceID (so event-log
//     dedup key and staging-table dedup key are the same string).
func verifyRawMessageInvariants(env *events.Envelope, authenticatedHostID uuid.UUID) *IngestPerEventRejection {
	var p events.RawMessageReceivedPayload
	if err := json.Unmarshal(env.Payload, &p); err != nil {
		return &IngestPerEventRejection{
			Code:    ingestRejectPayloadInvalid,
			Message: fmt.Sprintf("decode raw_message payload: %s", err.Error()),
		}
	}
	if p.HostID != authenticatedHostID {
		return &IngestPerEventRejection{
			Code:    ingestRejectPayloadInvariant,
			Message: "payload host_id does not match authenticated host",
		}
	}
	if _, ok := rawMessageAllowedSources[env.Source]; !ok {
		return &IngestPerEventRejection{
			Code:    ingestRejectPayloadInvariant,
			Message: fmt.Sprintf("env.source %q not supported on raw_message kinds", env.Source),
		}
	}
	if p.Source != env.Source {
		return &IngestPerEventRejection{
			Code:    ingestRejectPayloadInvariant,
			Message: "payload source does not match envelope source",
		}
	}
	if p.Guid == "" {
		return &IngestPerEventRejection{
			Code:    ingestRejectPayloadInvariant,
			Message: "payload guid is required",
		}
	}
	if p.Guid != env.SourceID {
		return &IngestPerEventRejection{
			Code:    ingestRejectPayloadInvariant,
			Message: "payload guid must equal envelope source_id",
		}
	}
	return nil
}

// verifyExternalContactInvariants enforces the cross-field consistency
// properties for external_contact.* envelopes. Returns a
// *IngestPerEventRejection (caller fills in Index) on any violation,
// nil on success.
//
// Properties checked:
//  1. payload decodes cleanly into the per-kind struct.
//  2. payload.HostID matches the authenticated host (no host
//     cross-impersonation).
//  3. env.Source is in the external_contact family's allowed sources.
//  4. payload.Source matches env.Source.
//  5. payload.EntityID is non-empty.
//  6. env.SourceID format matches the kind's content-hash discriminator
//     shape AND its entity prefix matches payload.EntityID.
func verifyExternalContactInvariants(env *events.Envelope, authenticatedHostID uuid.UUID) *IngestPerEventRejection {
	switch env.Kind {
	case events.KindExternalContactUpserted:
		var p events.ExternalContactUpsertedPayload
		if err := json.Unmarshal(env.Payload, &p); err != nil {
			return &IngestPerEventRejection{
				Code:    ingestRejectPayloadInvalid,
				Message: fmt.Sprintf("decode external_contact.upserted payload: %s", err.Error()),
			}
		}
		if rej := commonExternalContactInvariants(env, authenticatedHostID, p.HostID, p.Source, p.EntityID); rej != nil {
			return rej
		}
		if !reExternalContactUpsertSourceID.MatchString(env.SourceID) {
			return &IngestPerEventRejection{
				Code:    ingestRejectPayloadInvariant,
				Message: "external_contact.upserted source_id must match <entity>@<sha256-hex>",
			}
		}
		if !strings.HasPrefix(env.SourceID, p.EntityID+"@") {
			return &IngestPerEventRejection{
				Code:    ingestRejectPayloadInvariant,
				Message: "external_contact.upserted source_id entity prefix does not match payload entity_id",
			}
		}
		// Recompute SHA-256(JCS(payload \ {host_id})) and compare to the
		// hash suffix in env.SourceID. Closes the daemon-side contract
		// the spec defines at line 342 — a mismatch signals a stale
		// daemon cache, a JCS-library bug, or protocol drift, and is
		// rejected as PAYLOAD_INVARIANT rather than silently stored.
		computedHash, hashErr := ComputeContentHash(env.Payload)
		if hashErr != nil {
			return &IngestPerEventRejection{
				Code:    ingestRejectPayloadInvalid,
				Message: fmt.Sprintf("compute content hash: %s", hashErr.Error()),
			}
		}
		claimedHash := env.SourceID[len(env.SourceID)-64:]
		if computedHash != claimedHash {
			return &IngestPerEventRejection{
				Code:    ingestRejectExternalContactHashMismatch,
				Message: fmt.Sprintf("source_id hash %s does not match computed JCS hash %s", claimedHash, computedHash),
			}
		}
		return nil
	case events.KindExternalContactDeleted:
		var p events.ExternalContactDeletedPayload
		if err := json.Unmarshal(env.Payload, &p); err != nil {
			return &IngestPerEventRejection{
				Code:    ingestRejectPayloadInvalid,
				Message: fmt.Sprintf("decode external_contact.deleted payload: %s", err.Error()),
			}
		}
		if rej := commonExternalContactInvariants(env, authenticatedHostID, p.HostID, p.Source, p.EntityID); rej != nil {
			return rej
		}
		if !reExternalContactDeleteSourceID.MatchString(env.SourceID) {
			return &IngestPerEventRejection{
				Code:    ingestRejectPayloadInvariant,
				Message: "external_contact.deleted source_id must match <entity>@deleted@<sha256-hex|unknown>",
			}
		}
		if !strings.HasPrefix(env.SourceID, p.EntityID+"@deleted@") {
			return &IngestPerEventRejection{
				Code:    ingestRejectPayloadInvariant,
				Message: "external_contact.deleted source_id entity prefix does not match payload entity_id",
			}
		}
		return nil
	default:
		// Defence in depth — the dispatcher only routes
		// external_contact.* kinds here.
		return &IngestPerEventRejection{
			Code:    ingestRejectPayloadInvariant,
			Message: fmt.Sprintf("verifyExternalContactInvariants: unexpected kind %q", env.Kind),
		}
	}
}

// commonExternalContactInvariants enforces the host/source/entity_id
// checks shared by both external_contact.* kinds. Returns a rejection
// on any violation, nil on success.
func commonExternalContactInvariants(
	env *events.Envelope,
	authenticatedHostID uuid.UUID,
	payloadHostID uuid.UUID,
	payloadSource string,
	payloadEntityID string,
) *IngestPerEventRejection {
	if payloadHostID != authenticatedHostID {
		return &IngestPerEventRejection{
			Code:    ingestRejectPayloadInvariant,
			Message: "payload host_id does not match authenticated host",
		}
	}
	if _, ok := externalContactAllowedSources[env.Source]; !ok {
		return &IngestPerEventRejection{
			Code:    ingestRejectPayloadInvariant,
			Message: fmt.Sprintf("env.source %q not supported on external_contact.* kinds", env.Source),
		}
	}
	if payloadSource != env.Source {
		return &IngestPerEventRejection{
			Code:    ingestRejectPayloadInvariant,
			Message: "payload source does not match envelope source",
		}
	}
	if payloadEntityID == "" {
		return &IngestPerEventRejection{
			Code:    ingestRejectPayloadInvariant,
			Message: "payload entity_id is required",
		}
	}
	return nil
}

// handleExternalContactUpserted runs the per-event domain logic for an
// external_contact.upserted envelope inside the per-event savepoint.
//
// Sequence:
//  1. Pre-read by (source, entity_id, account_id=NULL) — distinguishes
//     first-insert / re-upsert / revive paths.
//  2. Upsert the external_contact row. UpsertExternalContact uses
//     ON CONFLICT DO UPDATE; the underlying query does NOT touch
//     deleted_at, crm_contact_id, or match_status.
//  3. If the pre-read showed a tombstone, issue ReviveTx to clear
//     deleted_at. Idempotent via the defensive WHERE clause.
//  4. Identity match per (email, phone). On FIRST-INSERT, the first
//     successful match sets crm_contact_id + match_status='matched' via
//     UpdateMatchTx. On re-upsert / revive, the external_contact row's
//     match state is preserved — identity rows are refreshed via
//     MatchOrCreateTx but the external_contact UpdateMatchTx call is
//     intentionally skipped so a user's manually-resolved `imported` or
//     `ignored` state is not silently flipped on a content edit.
//
// Returns (postCommit, rejection). rejection != nil rolls back the
// savepoint and postCommit is nil. On success, postCommit is a
// best-effort closure (nil when no reconcile is scheduled) that the
// dispatch loop runs AFTER the batch tx commits — it re-reads the
// committed row and reconciles its methods onto the linked contact.
// Running post-commit is load-bearing: EnrichContactFromExternal opens
// its OWN tx, so calling it inside this savepoint would nest a
// pool-acquired tx inside the open batch tx (deadlock/pool-stall risk).
// Reading the row AFTER commit also sidesteps the stale-local-struct
// problem (the `external` struct here does not reflect the in-tx
// UpdateMatchTx write) — the committed DB row is the source of truth for
// the freshly-set match on first insert.
func (s *IngestService) handleExternalContactUpserted(
	ctx context.Context,
	tx pgx.Tx,
	env *events.Envelope,
	hostID uuid.UUID,
) (postCommit func(context.Context), rejection *IngestPerEventRejection) {
	if s.identity == nil || s.externalContacts == nil {
		return nil, &IngestPerEventRejection{
			Code:    ingestRejectPayloadInvariant,
			Message: "ingest service was not configured for external_contact processing",
		}
	}

	var p events.ExternalContactUpsertedPayload
	if err := json.Unmarshal(env.Payload, &p); err != nil {
		return nil, &IngestPerEventRejection{
			Code:    ingestRejectPayloadInvalid,
			Message: fmt.Sprintf("decode external_contact.upserted payload: %s", err.Error()),
		}
	}

	// Soft check: warn when icloud_contacts upserts arrive without
	// metadata (the daemon should always populate
	// container_identifier). This is a developer-aid log; we don't
	// reject — metadata is forward-extensible.
	if env.Source == "icloud_contacts" && len(p.Metadata) == 0 {
		logger.Warn().
			Str("source", env.Source).
			Str("entity_id", p.EntityID).
			Str("host_id", hostID.String()).
			Msg("external_contact.upserted carries empty metadata; daemon should populate container_identifier")
	}

	// Pre-read to determine first-insert / re-upsert / revive.
	// GetBySourceTx is intentionally tombstone-aware.
	prior, getErr := s.externalContacts.GetBySourceTx(ctx, tx, env.Source, p.EntityID, nil)
	if getErr != nil && !errors.Is(getErr, db.ErrNotFound) {
		return nil, &IngestPerEventRejection{
			Code:    ingestRejectExternalContactGetFailed,
			Message: fmt.Sprintf("pre-read external_contact: %s", getErr.Error()),
		}
	}
	firstInsert := errors.Is(getErr, db.ErrNotFound)
	wasTombstoned := !firstInsert && prior != nil && prior.DeletedAt != nil

	// Upsert the row. The underlying query does not touch deleted_at,
	// crm_contact_id, or match_status on the UPDATE branch. host_id is
	// written on INSERT and on UPDATE via COALESCE(existing, EXCLUDED) —
	// legacy NULL rows are claimed on first non-NULL emit; non-NULL
	// ownership is preserved across all subsequent upserts.
	// last_content_hash is written on every UPSERT so the /known-ids
	// endpoint always returns the most recent payload's hash; the value
	// is the envelope's source_id suffix (already verified upstream
	// against SHA-256(JCS(payload \ {host_id}))). Concurrent upserts
	// for the same entity with different content hashes are
	// last-write-wins.
	birthday := parseExternalContactBirthday(p.Birthday)
	syncedAt := accelerated.GetCurrentTime()
	contentHash := env.SourceID[len(env.SourceID)-64:]
	payloadHostID := p.HostID
	upsertReq := repository.UpsertExternalContactRequest{
		Source:          env.Source,
		SourceID:        p.EntityID,
		AccountID:       nil, // icloud_contacts has no account_id concept
		DisplayName:     p.DisplayName,
		FirstName:       p.FirstName,
		LastName:        p.LastName,
		Emails:          toRepoEmailEntries(p.Emails),
		Phones:          toRepoPhoneEntries(p.Phones),
		Addresses:       toRepoAddressEntries(p.Addresses),
		Organization:    p.Organization,
		JobTitle:        p.JobTitle,
		Birthday:        birthday,
		PhotoURL:        p.PhotoURL,
		Metadata:        p.Metadata,
		SyncedAt:        &syncedAt,
		HostID:          &payloadHostID,
		LastContentHash: &contentHash,
	}
	external, err := s.externalContacts.UpsertTx(ctx, tx, upsertReq)
	if err != nil {
		return nil, &IngestPerEventRejection{
			Code:    ingestRejectExternalContactUpsertFailed,
			Message: fmt.Sprintf("upsert external_contact: %s", err.Error()),
		}
	}

	// Revive if the row is tombstoned. We check BOTH the pre-read AND
	// the post-upsert returned row's deleted_at — a concurrent delete
	// that committed between our pre-read and our upsert leaves
	// wasTombstoned=false even though the row is tombstoned at this
	// point. The post-upsert check closes that race.
	needsRevive := wasTombstoned || (external != nil && external.DeletedAt != nil)
	if needsRevive {
		revived, err := s.externalContacts.ReviveTx(ctx, tx, external.ID)
		if err != nil && !errors.Is(err, db.ErrNotFound) {
			return nil, &IngestPerEventRejection{
				Code:    ingestRejectExternalContactReviveFailed,
				Message: fmt.Sprintf("revive external_contact: %s", err.Error()),
			}
		}
		if revived != nil {
			external = revived
		}
	}

	// Identity match per (email, phone). Iterate the full set so
	// external_identity rows get refreshed for every method,
	// regardless of which one triggered the match.
	//
	// The external_contact.crm_contact_id / match_status update only
	// fires on FIRST INSERT and only on the first successful match —
	// re-upsert / revive preserves match state verbatim so a user's
	// manually-resolved `imported` or `ignored` decision is not
	// silently flipped on a content edit.
	//
	// Defensive guards (closing concurrent-INSERT-between-pre-read-and-
	// upsert races):
	//   - external.CRMContactID == nil: another tx may have set the
	//     match; do not clobber.
	//   - external.MatchStatus is unmatched: the user may have
	//     `imported` / `ignored` the row manually; preserve that.
	//   - external.DuplicateOfID == nil: the row is not marked as a
	//     duplicate of another contact.
	canSetMatchOnRow := firstInsert &&
		external.CRMContactID == nil &&
		external.MatchStatus == repository.MatchStatusUnmatched &&
		external.DuplicateOfID == nil
	matched := false
	// matchedContactID tracks the CRM contact_id resolved by the
	// email/phone match loops. The local `external` struct is from the
	// initial UpsertTx and does NOT reflect the subsequent UpdateMatchTx
	// write — capture the resolution separately so downstream logic
	// (e.g., anarlog identity linking) sees the freshly-matched contact.
	// On re-upsert / revive (canSetMatchOnRow=false) we fall back to the
	// existing external.CRMContactID since that's the source of truth
	// for the preserved match.
	var matchedContactID *uuid.UUID
	if external != nil && external.CRMContactID != nil {
		matchedContactID = external.CRMContactID
	}
	for _, em := range p.Emails {
		if em.Value == "" {
			continue
		}
		// SkipEmpty: a value that normalizes to empty (e.g. whitespace)
		// is a no-op rather than an envelope rejection — one junk field
		// must not reject the whole external_contact upsert.
		result, err := s.identity.MatchOrCreateTx(ctx, tx, MatchRequest{
			RawIdentifier: em.Value,
			Type:          identity.IdentifierTypeEmail,
			Source:        env.Source,
			DisplayName:   p.DisplayName,
		}, NormalizationSkipEmpty)
		if err != nil {
			return nil, &IngestPerEventRejection{
				Code:    ingestRejectIdentityMatchFailed,
				Message: fmt.Sprintf("identity match (email): %s", err.Error()),
			}
		}
		if canSetMatchOnRow && !matched && result != nil && result.ContactID != nil {
			if _, err := s.externalContacts.UpdateMatchTx(ctx, tx,
				external.ID, result.ContactID, repository.MatchStatusMatched); err != nil {
				return nil, &IngestPerEventRejection{
					Code:    ingestRejectExternalContactUpdateMatchFailed,
					Message: fmt.Sprintf("update external_contact match (email): %s", err.Error()),
				}
			}
			matched = true
			matchedContactID = result.ContactID
		}
	}
	for _, ph := range p.Phones {
		if ph.Value == "" {
			continue
		}
		// SkipEmpty: a value that normalizes to empty (e.g. "+",
		// whitespace) is a no-op rather than an envelope rejection — one
		// junk field must not reject the whole external_contact upsert.
		result, err := s.identity.MatchOrCreateTx(ctx, tx, MatchRequest{
			RawIdentifier: ph.Value,
			Type:          identity.IdentifierTypePhone,
			Source:        env.Source,
			DisplayName:   p.DisplayName,
		}, NormalizationSkipEmpty)
		if err != nil {
			return nil, &IngestPerEventRejection{
				Code:    ingestRejectIdentityMatchFailed,
				Message: fmt.Sprintf("identity match (phone): %s", err.Error()),
			}
		}
		if canSetMatchOnRow && !matched && result != nil && result.ContactID != nil {
			if _, err := s.externalContacts.UpdateMatchTx(ctx, tx,
				external.ID, result.ContactID, repository.MatchStatusMatched); err != nil {
				return nil, &IngestPerEventRejection{
					Code:    ingestRejectExternalContactUpdateMatchFailed,
					Message: fmt.Sprintf("update external_contact match (phone): %s", err.Error()),
				}
			}
			matched = true
			matchedContactID = result.ContactID
		}
	}

	// Anarlog-humans identity registration. For source='anarlog_humans'
	// only, write an external_identity row keyed by the anarlog_human_id
	// itself so the meeting_note.recorded handler can resolve
	// participant_ids → CRM contact_ids via FindContactIDByAnarlogHumanIDTx.
	// If the email/phone path above resolved a contact_id, link the new
	// identity row to it in the same tx. The Import-handler backfill
	// (handlers/import.go) covers the "tag now, import later" path.
	if env.Source == "anarlog_humans" && s.identityLookup != nil {
		// FailEmpty: the anarlog_human_id is the single structural key
		// the meeting_note resolution chain hangs on. A whitespace-only
		// ID (which passes the upstream non-empty-string guard but
		// normalizes to empty) is fatal data — reject the event rather
		// than landing a row with no resolvable identity.
		result, idErr := s.identity.MatchOrCreateTx(ctx, tx, MatchRequest{
			RawIdentifier: p.EntityID,
			Type:          identity.IdentifierTypeAnarlogHuman,
			Source:        env.Source,
			SourceID:      &p.EntityID,
			DisplayName:   p.DisplayName,
		}, NormalizationFailEmpty)
		if idErr != nil {
			return nil, &IngestPerEventRejection{
				Code:    ingestRejectIdentityMatchFailed,
				Message: fmt.Sprintf("identity match (anarlog_human_id): %s", idErr.Error()),
			}
		}
		// If the external_contact's email/phone resolved a CRM contact
		// AND the new anarlog identity row is unmatched, link it.
		// Defensive guards: only link when both sides have populated
		// pointers AND the identity row is genuinely unmatched (avoid
		// clobbering a manual link from a prior import). matchedContactID
		// reflects the freshly-resolved contact from the email/phone
		// loops above (the local external struct is stale across the
		// UpdateMatchTx write).
		if matchedContactID != nil &&
			result != nil && result.Identity != nil && result.Identity.ContactID == nil {
			if _, linkErr := s.identityLookup.LinkIdentityToContactTx(ctx, tx, repository.LinkIdentityRequest{
				IdentityID: result.Identity.ID,
				ContactID:  *matchedContactID,
				MatchType:  repository.MatchTypeExact,
			}); linkErr != nil {
				return nil, &IngestPerEventRejection{
					Code:    ingestRejectIdentityMatchFailed,
					Message: fmt.Sprintf("link anarlog_human_id identity to contact: %s", linkErr.Error()),
				}
			}
		}
	}

	// Schedule a post-commit method reconcile for address-book rows.
	// icloud_contacts is the only address-book source on this handler
	// (anarlog_humans is out of scope — its own match/enrich flow). The
	// closure re-reads the COMMITTED row, so it sees the freshly-set
	// match on first insert (the local `external` struct is stale across
	// UpdateMatchTx) and the preserved status on re-upsert. The
	// reconciler internally resolves the effective contact/status
	// (duplicate-aware precedence) and no-ops for unmatched/ignored rows,
	// so it is safe to schedule whenever the row exists.
	if s.addressBookReconciler != nil && env.Source == "icloud_contacts" && external != nil {
		externalID := external.ID
		reconciler := s.addressBookReconciler
		postCommit = func(pcCtx context.Context) {
			if err := reconciler.ResolveAndReconcile(pcCtx, externalID); err != nil {
				// No Err()/id attached: a downstream enrichment error can
				// embed a normalized method value (PII). Log only that the
				// post-commit reconcile failed for one row.
				logger.Warn().Msg("icloud: post-commit method reconcile failed for one row")
			}
		}
	}

	return postCommit, nil
}

// handleExternalContactDeleted runs the per-event domain logic for an
// external_contact.deleted envelope inside the per-event savepoint.
//
// Behavior:
//   - Unknown entity → silent no-op. The event-log row from
//     Bus.PublishTx is preserved for audit; no row materialization.
//     Rationale: a late-arriving delete after a never-upserted row
//     (e.g., Pi was wiped, daemon resends a queued delete) must not
//     stall the daemon's cursor.
//   - Already-tombstoned → idempotent silent no-op.
//   - Live row → validate the source_id's hash suffix against the
//     row's stored last_content_hash (three exceptions: @unknown
//     sentinel, NULL stored hash, already-tombstoned — the last fires
//     before the check). Mismatch rejects as
//     EXTERNAL_CONTACT_DELETE_HASH_MISMATCH. On match, SoftDeleteTx
//     sets deleted_at. crm_contact_id, match_status, and
//     duplicate_of_id are preserved.
//
// The lookup-based hash check (vs. the upsert's JCS recomputation)
// is by spec design — spec line 343 defines the delete source_id
// over the PRIOR upsert's payload, which the Pi does not see in the
// delete payload.
func (s *IngestService) handleExternalContactDeleted(
	ctx context.Context,
	tx pgx.Tx,
	env *events.Envelope,
	authenticatedHostID uuid.UUID,
) *IngestPerEventRejection {
	if s.externalContacts == nil {
		return &IngestPerEventRejection{
			Code:    ingestRejectPayloadInvariant,
			Message: "ingest service was not configured for external_contact processing",
		}
	}
	var p events.ExternalContactDeletedPayload
	if err := json.Unmarshal(env.Payload, &p); err != nil {
		return &IngestPerEventRejection{
			Code:    ingestRejectPayloadInvalid,
			Message: fmt.Sprintf("decode external_contact.deleted payload: %s", err.Error()),
		}
	}
	prior, getErr := s.externalContacts.GetBySourceTx(ctx, tx, env.Source, p.EntityID, nil)
	if getErr != nil {
		if errors.Is(getErr, db.ErrNotFound) {
			// Unknown entity. Silent no-op — event-log row durable.
			return nil
		}
		return &IngestPerEventRejection{
			Code:    ingestRejectExternalContactGetFailed,
			Message: fmt.Sprintf("pre-read external_contact: %s", getErr.Error()),
		}
	}
	if prior == nil || prior.DeletedAt != nil {
		// Already tombstoned. Idempotent no-op.
		return nil
	}
	// Host-scope guard: don't let host B tombstone a row owned by host A.
	// The payload-host check in commonExternalContactInvariants only verifies
	// the daemon's own claim; this check verifies the stored row's owner.
	// NULL prior.HostID = legacy row (pre-migration 052 or non-mac source);
	// pass through so the self-heal-on-upsert path keeps working.
	// Silent no-op (not a rejection) so a stranded post-re-pair delete in
	// the daemon's event queue doesn't stall the cursor; warn-log so the
	// operator has a breadcrumb when this fires (hypothetical today;
	// realistic if a second Mac is ever paired).
	if prior.HostID != nil && *prior.HostID != authenticatedHostID {
		logger.Warn().
			Str("source", env.Source).
			Str("entity_id", p.EntityID).
			Str("prior_host_id", prior.HostID.String()).
			Str("authenticated_host_id", authenticatedHostID.String()).
			Msg("external_contact.deleted dropped: row owned by a different host")
		return nil
	}
	// Validate the source_id hash suffix against the row's stored
	// last_content_hash. Three exceptions: @unknown sentinel
	// (daemon-declared hash-unknown per the spec fallback), NULL
	// stored hash (legacy row written before this column existed),
	// already-tombstoned (handled above).
	if !strings.HasSuffix(env.SourceID, externalContactDeleteUnknownSuffix) && prior.LastContentHash != nil {
		claimed := env.SourceID[len(env.SourceID)-64:]
		if *prior.LastContentHash != claimed {
			return &IngestPerEventRejection{
				Code: ingestRejectExternalContactDeleteHashMismatch,
				Message: fmt.Sprintf(
					"delete source_id hash %s does not match stored last_content_hash %s for entity %s",
					claimed, *prior.LastContentHash, p.EntityID),
			}
		}
	}
	if err := s.externalContacts.SoftDeleteTx(ctx, tx, prior.ID); err != nil {
		return &IngestPerEventRejection{
			Code:    ingestRejectExternalContactDeleteFailed,
			Message: fmt.Sprintf("soft-delete external_contact: %s", err.Error()),
		}
	}
	return nil
}

// externalContactDeleteUnknownSuffix is the spec-defined fallback
// suffix on external_contact.deleted source_ids when the daemon has
// no local cache of the prior content hash (spec line 343). The
// delete-hash validation short-circuits and accepts the event.
const externalContactDeleteUnknownSuffix = "@deleted@unknown"

// parseExternalContactBirthday parses the payload's *string Birthday
// (ISO YYYY-MM-DD) into a *time.Time. ValidatePayload already rejected
// malformed input at the boundary, but defence-in-depth — return nil
// on parse failure so a downstream bug doesn't crash the handler.
func parseExternalContactBirthday(s *string) *time.Time {
	if s == nil || *s == "" {
		return nil
	}
	t, err := time.Parse("2006-01-02", *s)
	if err != nil {
		return nil
	}
	return &t
}

// toRepoEmailEntries converts payload ExternalContactMethodValue slice
// into the repository's EmailEntry slice. Identical shape; just the
// package boundary.
func toRepoEmailEntries(in []events.ExternalContactMethodValue) []repository.EmailEntry {
	if len(in) == 0 {
		return nil
	}
	out := make([]repository.EmailEntry, 0, len(in))
	for _, v := range in {
		out = append(out, repository.EmailEntry{
			Value:   v.Value,
			Type:    v.Type,
			Primary: v.Primary,
		})
	}
	return out
}

// toRepoPhoneEntries converts payload ExternalContactMethodValue slice
// into the repository's PhoneEntry slice.
func toRepoPhoneEntries(in []events.ExternalContactMethodValue) []repository.PhoneEntry {
	if len(in) == 0 {
		return nil
	}
	out := make([]repository.PhoneEntry, 0, len(in))
	for _, v := range in {
		out = append(out, repository.PhoneEntry{
			Value:   v.Value,
			Type:    v.Type,
			Primary: v.Primary,
		})
	}
	return out
}

// toRepoAddressEntries converts payload ExternalContactAddressValue
// slice into the repository's AddressEntry slice.
func toRepoAddressEntries(in []events.ExternalContactAddressValue) []repository.AddressEntry {
	if len(in) == 0 {
		return nil
	}
	out := make([]repository.AddressEntry, 0, len(in))
	for _, v := range in {
		out = append(out, repository.AddressEntry{
			Formatted: v.Formatted,
			Type:      v.Type,
		})
	}
	return out
}

// verifyMeetingNoteInvariants enforces the cross-field consistency
// properties for meeting_note.* envelopes. Returns a
// *IngestPerEventRejection (caller fills in Index) on any violation,
// nil on success.
//
// Properties checked (mirrors verifyExternalContactInvariants):
//  1. payload decodes cleanly into the per-kind struct.
//  2. payload.HostID matches the authenticated host.
//  3. env.Source is in the meeting_note family's allowed sources.
//  4. payload.Source matches env.Source.
//  5. payload.SourceID parses as a UUID.
//  6. env.SourceID matches the kind's content-hash discriminator shape
//     AND its entity prefix matches payload.SourceID.
//  7. For upsert: recompute SHA-256(JCS(payload \ {host_id})) and assert
//     it equals the 64-char hex suffix on env.SourceID. Mismatch is
//     MEETING_NOTE_HASH_MISMATCH (delete uses the stored-hash lookup
//     path inside the handler — same convention as external_contact).
func verifyMeetingNoteInvariants(env *events.Envelope, authenticatedHostID uuid.UUID) *IngestPerEventRejection {
	switch env.Kind {
	case events.KindMeetingNoteRecorded:
		var p events.MeetingNoteRecordedPayload
		if err := json.Unmarshal(env.Payload, &p); err != nil {
			return &IngestPerEventRejection{
				Code:    ingestRejectPayloadInvalid,
				Message: fmt.Sprintf("decode meeting_note.recorded payload: %s", err.Error()),
			}
		}
		if rej := commonMeetingNoteInvariants(env, authenticatedHostID, p.HostID, p.Source, p.SourceID); rej != nil {
			return rej
		}
		if !reMeetingNoteUpsertSourceID.MatchString(env.SourceID) {
			return &IngestPerEventRejection{
				Code:    ingestRejectPayloadInvariant,
				Message: "meeting_note.recorded source_id must match <session_uuid>@<sha256-hex>",
			}
		}
		if !strings.HasPrefix(env.SourceID, p.SourceID+"@") {
			return &IngestPerEventRejection{
				Code:    ingestRejectPayloadInvariant,
				Message: "meeting_note.recorded source_id entity prefix does not match payload source_id",
			}
		}
		computedHash, hashErr := ComputeContentHash(env.Payload)
		if hashErr != nil {
			return &IngestPerEventRejection{
				Code:    ingestRejectPayloadInvalid,
				Message: fmt.Sprintf("compute content hash: %s", hashErr.Error()),
			}
		}
		claimedHash := env.SourceID[len(env.SourceID)-64:]
		if computedHash != claimedHash {
			return &IngestPerEventRejection{
				Code:    ingestRejectMeetingNoteHashMismatch,
				Message: fmt.Sprintf("source_id hash %s does not match computed JCS hash %s", claimedHash, computedHash),
			}
		}
		return nil
	case events.KindMeetingNoteDeleted:
		var p events.MeetingNoteDeletedPayload
		if err := json.Unmarshal(env.Payload, &p); err != nil {
			return &IngestPerEventRejection{
				Code:    ingestRejectPayloadInvalid,
				Message: fmt.Sprintf("decode meeting_note.deleted payload: %s", err.Error()),
			}
		}
		if rej := commonMeetingNoteInvariants(env, authenticatedHostID, p.HostID, p.Source, p.SourceID); rej != nil {
			return rej
		}
		if !reMeetingNoteDeleteSourceID.MatchString(env.SourceID) {
			return &IngestPerEventRejection{
				Code:    ingestRejectPayloadInvariant,
				Message: "meeting_note.deleted source_id must match <session_uuid>@deleted@<sha256-hex|unknown>",
			}
		}
		if !strings.HasPrefix(env.SourceID, p.SourceID+"@deleted@") {
			return &IngestPerEventRejection{
				Code:    ingestRejectPayloadInvariant,
				Message: "meeting_note.deleted source_id entity prefix does not match payload source_id",
			}
		}
		return nil
	default:
		return &IngestPerEventRejection{
			Code:    ingestRejectPayloadInvariant,
			Message: fmt.Sprintf("verifyMeetingNoteInvariants: unexpected kind %q", env.Kind),
		}
	}
}

// commonMeetingNoteInvariants enforces the host/source/source_id
// checks shared by both meeting_note.* kinds.
func commonMeetingNoteInvariants(
	env *events.Envelope,
	authenticatedHostID uuid.UUID,
	payloadHostID uuid.UUID,
	payloadSource string,
	payloadSourceID string,
) *IngestPerEventRejection {
	if payloadHostID != authenticatedHostID {
		return &IngestPerEventRejection{
			Code:    ingestRejectPayloadInvariant,
			Message: "payload host_id does not match authenticated host",
		}
	}
	if _, ok := meetingNoteAllowedSources[env.Source]; !ok {
		return &IngestPerEventRejection{
			Code:    ingestRejectPayloadInvariant,
			Message: fmt.Sprintf("env.source %q not supported on meeting_note.* kinds", env.Source),
		}
	}
	if payloadSource != env.Source {
		return &IngestPerEventRejection{
			Code:    ingestRejectPayloadInvariant,
			Message: "payload source does not match envelope source",
		}
	}
	if payloadSourceID == "" {
		return &IngestPerEventRejection{
			Code:    ingestRejectPayloadInvariant,
			Message: "payload source_id is required",
		}
	}
	if _, err := uuid.Parse(payloadSourceID); err != nil {
		return &IngestPerEventRejection{
			Code:    ingestRejectPayloadInvariant,
			Message: fmt.Sprintf("payload source_id %q is not a UUID", payloadSourceID),
		}
	}
	return nil
}

// linkageWindow is the symmetric ± window around payload.MeetingAt that
// FindLinkageCandidatesTx searches for calendar (and future phone_call)
// rows. 15 minutes per sidecar spec §Linkage detection algorithm.
const linkageWindow = 15 * time.Minute

// phoneCoalesceWindow is the maximum gap between two same-number
// phone_call candidates for them to be treated as one interaction (a
// dropped/unanswered attempt + its redial, or two halves of one
// dropped-and-resumed call). Tunable.
const phoneCoalesceWindow = 5 * time.Minute

// shouldRunInlineOnDuplicate is the dispatch-loop probe that allows
// the inline handler to run even when Bus.PublishTx flagged the event
// as a duplicate. For meeting_note.recorded specifically there are two
// scenarios where the duplicate must NOT be a no-op:
//
//  1. The underlying row is tombstoned and needs revive consideration
//     (delete then re-record identical content scenario).
//  2. The row is live but the participant→contact resolution may have
//     drifted since the original ingest (the user imported a previously-
//     unmatched anarlog_humans candidate, so the resolved_set_hash
//     would now differ). The carry-forward branch detects whether work
//     is actually needed via hash compare and short-circuits when not.
//
// Returning true unconditionally for live + tombstoned meeting_note
// rows is safe: the handler's carry-forward branch is cheap (single
// UPDATE on the content fields) when nothing changed. Other kinds keep
// the "duplicate is a true no-op" contract.
func (s *IngestService) shouldRunInlineOnDuplicate(ctx context.Context, tx pgx.Tx, env *events.Envelope) (bool, error) {
	if env.Kind != events.KindMeetingNoteRecorded || s.meetingNotes == nil {
		return false, nil
	}
	var p events.MeetingNoteRecordedPayload
	if err := json.Unmarshal(env.Payload, &p); err != nil {
		// Decode failure here is unexpected — the dispatch loop already
		// ran verifyMeetingNoteInvariants which decodes successfully.
		// Surface as an error so the batch rolls back.
		return false, fmt.Errorf("inline-on-duplicate probe: decode payload: %w", err)
	}
	sessionID, parseErr := uuid.Parse(p.SourceID)
	if parseErr != nil {
		return false, fmt.Errorf("inline-on-duplicate probe: parse session UUID: %w", parseErr)
	}
	// Tombstone-aware FOR UPDATE — returns the row whether live or
	// soft-deleted. Either case means the inline handler should run.
	// db.ErrNotFound is the no-prior-row case (e.g. concurrent first
	// insert that lost the race) — in that case the original
	// duplicate-skip semantics apply.
	_, err := s.meetingNotes.GetMeetingNoteBySessionIDForUpdateTx(ctx, tx, sessionID)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, db.ErrNotFound) {
		return false, nil
	}
	return false, fmt.Errorf("inline-on-duplicate probe: lookup row: %w", err)
}

// meetingNoteInputHashRecipe is the canonical (meeting_at, title,
// sorted participant_ids) tuple that the inline handler hashes to
// detect "matching inputs changed" on re-sync.
type meetingNoteInputHashRecipe struct {
	MeetingAt      string   `json:"meeting_at"`
	Title          *string  `json:"title"`
	ParticipantIDs []string `json:"participant_ids"`
}

// computeMeetingNoteInputHash returns the lowercase-hex SHA-256 of a
// stable canonicalization of (meeting_at, title, sorted participant_ids).
// Title is included so a future title-parser does not have to migrate
// hashes. Summary and memo are intentionally excluded — they're content
// fields, not matching inputs.
func computeMeetingNoteInputHash(meetingAt time.Time, title *string, participantIDs []string) (string, error) {
	sorted := append([]string(nil), participantIDs...)
	sort.Strings(sorted)
	recipe := meetingNoteInputHashRecipe{
		MeetingAt:      meetingAt.UTC().Format(time.RFC3339Nano),
		Title:          title,
		ParticipantIDs: sorted,
	}
	raw, err := json.Marshal(recipe)
	if err != nil {
		return "", fmt.Errorf("marshal input hash recipe: %w", err)
	}
	canonical, err := jcs.Transform(raw)
	if err != nil {
		return "", fmt.Errorf("jcs canonicalize input hash recipe: %w", err)
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:]), nil
}

// computeResolvedSetHash returns the lowercase-hex SHA-256 of a
// stable canonicalization of the union of tag-resolved + title-matched
// contact UUIDs. The union captures drift in EITHER resolution path:
// participant→contact (existing) or title-token→contact (added with
// the anarlog title-parsing surface) — a change in either bumps the
// hash and forces carry-forward to fail so the interaction-diff loop
// can reconcile the title-derived `:title:` interactions.
//
// Note: when titleMatchedIDs is empty (no title or no title matches),
// the union reduces to taggedIDs alone and the hash equals the
// pre-title recipe — legacy meeting_note rows with empty titles or
// no title matches keep their carry-forward semantics unchanged.
func computeResolvedSetHash(taggedIDs []uuid.UUID, titleMatchedIDs []uuid.UUID) (string, error) {
	union := make([]string, 0, len(taggedIDs)+len(titleMatchedIDs))
	seen := make(map[uuid.UUID]struct{}, len(taggedIDs)+len(titleMatchedIDs))
	for _, id := range taggedIDs {
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		union = append(union, id.String())
	}
	for _, id := range titleMatchedIDs {
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		union = append(union, id.String())
	}
	sort.Strings(union)
	raw, err := json.Marshal(union)
	if err != nil {
		return "", fmt.Errorf("marshal resolved set: %w", err)
	}
	canonical, err := jcs.Transform(raw)
	if err != nil {
		return "", fmt.Errorf("jcs canonicalize resolved set: %w", err)
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:]), nil
}

// resolvedTag is the (anarlog_human_id, contact_id) pair produced by
// the participant-resolution step. The handler deduplicates by
// contact_id so two anarlog humans mapping to the same CRM contact
// produce exactly one interaction.
type resolvedTag struct {
	AnarlogID string
	ContactID uuid.UUID
}

// resolvedTitle is a title-extracted name token that fuzzy-matched a
// single CRM contact (high-confidence per the collision-gap rule).
// Token preserves original casing for display; NormalizedToken is the
// lowercased form used as the dedup + grouping key. The handler
// deduplicates against resolvedTagged so a tagged human who also
// appears in the title doesn't produce a second interaction.
type resolvedTitle struct {
	Token           string
	NormalizedToken string
	ContactID       uuid.UUID
}

// desiredInteraction is a single (contact_id, source_ref) pair the
// linkage logic decided should be persisted. Computed before the
// re-sync diff so we can compare against the existing set.
type desiredInteraction struct {
	ContactID uuid.UUID
	SourceRef string
}

// handleMeetingNoteRecorded runs the per-event domain logic for a
// meeting_note.recorded envelope inside the per-event savepoint.
//
// Implements the full linkage-detection algorithm per
// .ai/spec/mac-daemon-phase-2-anarlog-matching.md §Linkage detection,
// including Step 3 participant-signal disambiguation: 2+ candidates
// with a strict-max-non-zero overlap against the implied set (tagged
// ∪ title-matched) auto-link; otherwise the row lands on
// conflict_pending with the per-candidate snapshot persisted for the
// resolve-link endpoint to consume. Re-sync diff and the
// revive-on-tombstone branch share the same code path.
//
// Returns (needsAttention, followUps, rejection): needsAttention is
// non-nil when the final linkage_state requires user attention
// (conflict_pending or orphan_needs_review). followUps carries the
// post-commit closures returned by RecordInteractionTx for the new
// session-attributed interactions (so FollowUpManager fires after
// the outer tx commits). rejection != nil rolls back the savepoint.
func (s *IngestService) handleMeetingNoteRecorded(
	ctx context.Context,
	tx pgx.Tx,
	env *events.Envelope,
	authenticatedHostID uuid.UUID,
) (*NeedsAttentionItem, []func(context.Context), *IngestPerEventRejection) {
	if s.meetingNotes == nil || s.calendar == nil || s.interactions == nil ||
		s.identityLookup == nil || s.contactSvc == nil ||
		s.titleMatcher == nil || s.discovery == nil {
		return nil, nil, &IngestPerEventRejection{
			Code:    ingestRejectPayloadInvariant,
			Message: "ingest service was not configured for meeting_note processing",
		}
	}

	// followUps accumulates post-commit closures returned by
	// ContactService.RecordInteractionTx so the dispatch loop can run
	// them after the outer tx commits. FollowUpManager updates
	// contact_task rows + may schedule Todoist work; running it inside
	// the tx would hold the lock across an external HTTP call.
	var followUps []func(context.Context)

	var p events.MeetingNoteRecordedPayload
	if err := json.Unmarshal(env.Payload, &p); err != nil {
		return nil, nil, &IngestPerEventRejection{
			Code:    ingestRejectPayloadInvalid,
			Message: fmt.Sprintf("decode meeting_note.recorded payload: %s", err.Error()),
		}
	}
	sessionID, parseErr := uuid.Parse(p.SourceID)
	if parseErr != nil {
		return nil, nil, &IngestPerEventRejection{
			Code:    ingestRejectPayloadInvalid,
			Message: fmt.Sprintf("parse session UUID: %s", parseErr.Error()),
		}
	}

	// content hash from envelope source_id (already verified upstream
	// by verifyMeetingNoteInvariants → ComputeContentHash match).
	contentHash := env.SourceID[len(env.SourceID)-64:]
	payloadHostID := p.HostID
	_ = payloadHostID // referenced for clarity in payload→insert mapping

	// 1. Acquire row lock + read existing meeting_note row (tombstone-
	//    aware). FOR UPDATE serializes concurrent re-syncs for the same
	//    session UUID.
	prior, getErr := s.meetingNotes.GetMeetingNoteBySessionIDForUpdateTx(ctx, tx, sessionID)
	if getErr != nil && !errors.Is(getErr, db.ErrNotFound) {
		return nil, nil, &IngestPerEventRejection{
			Code:    ingestRejectMeetingNoteUpsertFailed,
			Message: fmt.Sprintf("pre-read meeting_note: %s", getErr.Error()),
		}
	}

	// 2. Host-ownership guard. Mirrors handleExternalContactDeleted.
	//    Non-NULL stored host_id that differs from authenticated host →
	//    warn-log + silent no-op (rejection nil, needs_attention nil).
	if prior != nil && prior.MacHostID != nil && *prior.MacHostID != authenticatedHostID {
		logger.Warn().
			Str("source", env.Source).
			Str("session_id", p.SourceID).
			Str("prior_host_id", prior.MacHostID.String()).
			Str("authenticated_host_id", authenticatedHostID.String()).
			Msg("meeting_note.recorded dropped: row owned by a different host")
		return nil, nil, nil
	}

	// 3. Resolve tagged participants (anarlog_human UUIDs → CRM
	//    contact_ids). Dedupe by contact_id so two anarlog humans
	//    mapping to the same CRM contact produce ONE interaction.
	resolvedTagged := make([]resolvedTag, 0, len(p.ParticipantIDs))
	seenContactIDs := make(map[uuid.UUID]struct{}, len(p.ParticipantIDs))
	for _, pid := range p.ParticipantIDs {
		contactID, lookupErr := s.identityLookup.FindContactIDByAnarlogHumanIDTx(ctx, tx, pid)
		if lookupErr != nil {
			return nil, nil, &IngestPerEventRejection{
				Code:    ingestRejectParticipantResolveFailed,
				Message: fmt.Sprintf("resolve participant %s: %s", pid, lookupErr.Error()),
			}
		}
		if contactID == nil {
			continue
		}
		if _, dup := seenContactIDs[*contactID]; dup {
			continue
		}
		seenContactIDs[*contactID] = struct{}{}
		resolvedTagged = append(resolvedTagged, resolvedTag{AnarlogID: pid, ContactID: *contactID})
	}

	// 4. Extract + match title tokens. Runs UNCONDITIONALLY (i.e., not
	//    gated on carryForward, which doesn't exist yet at this point)
	//    because the title-matched contact_ids feed into the
	//    resolved_set_hash that DETERMINES carryForward. titleMatched
	//    is deduplicated against resolvedTagged so a tagged human who
	//    also appears in the title produces only one interaction.
	titleStr := ""
	if p.Title != nil {
		titleStr = *p.Title
	}
	titleTokens := anarlog.ExtractNameTokens(titleStr)
	titleMatched := make([]resolvedTitle, 0, len(titleTokens))
	titleUnmatched := make([]string, 0, len(titleTokens))
	for _, token := range titleTokens {
		match, mErr := s.titleMatcher.MatchTitleToken(ctx, token)
		if mErr != nil {
			return nil, nil, &IngestPerEventRejection{
				Code:    ingestRejectTitleMatchFailed,
				Message: fmt.Sprintf("match title token %q: %s", token, mErr.Error()),
			}
		}
		if match == nil {
			titleUnmatched = append(titleUnmatched, token)
			continue
		}
		// Dedup against resolvedTagged so the same contact doesn't get
		// two interactions (the tagged-anchor form is canonical; the
		// title-derived form is redundant for that contact).
		alreadyTagged := false
		for _, t := range resolvedTagged {
			if t.ContactID == match.Contact.ID {
				alreadyTagged = true
				break
			}
		}
		if alreadyTagged {
			continue
		}
		titleMatched = append(titleMatched, resolvedTitle{
			Token:           token,
			NormalizedToken: strings.ToLower(token),
			ContactID:       match.Contact.ID,
		})
	}

	// 5. Compute hashes. Stable order: input hash uses sorted
	//    participant_ids; resolved-set hash uses sorted UNION of
	//    tag-resolved + title-matched contact_ids so drift in either
	//    resolution path invalidates carry-forward.
	newInputHash, err := computeMeetingNoteInputHash(p.MeetingAt, p.Title, p.ParticipantIDs)
	if err != nil {
		return nil, nil, &IngestPerEventRejection{
			Code:    ingestRejectMeetingNoteUpsertFailed,
			Message: fmt.Sprintf("compute input hash: %s", err.Error()),
		}
	}
	resolvedContactIDs := make([]uuid.UUID, len(resolvedTagged))
	for i, r := range resolvedTagged {
		resolvedContactIDs[i] = r.ContactID
	}
	titleMatchedIDs := make([]uuid.UUID, len(titleMatched))
	for i, tm := range titleMatched {
		titleMatchedIDs[i] = tm.ContactID
	}
	newResolvedSetHash, err := computeResolvedSetHash(resolvedContactIDs, titleMatchedIDs)
	if err != nil {
		return nil, nil, &IngestPerEventRejection{
			Code:    ingestRejectMeetingNoteUpsertFailed,
			Message: fmt.Sprintf("compute resolved set hash: %s", err.Error()),
		}
	}

	// 5. Decide the branch:
	//    a) prior == nil          → first-insert (run linkage + insert).
	//    b) prior tombstoned      → revive (run linkage + revive update).
	//    c) prior live, hashes
	//       unchanged            → carry-forward (update content fields only).
	//    d) prior live, any hash
	//       changed              → re-link (run linkage + diff interactions).
	revivePath := prior != nil && prior.DeletedAt != nil
	carryForward := prior != nil && !revivePath &&
		prior.InputHash == newInputHash &&
		prior.ResolvedSetHash == newResolvedSetHash

	var (
		finalLinkageState       string
		finalLinkedKind         *string
		finalLinkedID           *uuid.UUID
		desired                 []desiredInteraction
		candidatesLen           int
		coalescedLen            int
		interactionsCreated     int
		interactionsDropped     int
		conflictSnapshot        []repository.ConflictCandidateSummary
		conflictCandidatesBytes []byte
		conflictOverlapTop      int
	)

	if carryForward {
		// Preserve prior linkage. No interaction writes. The
		// conflict_candidates column is preserved verbatim by
		// passing prior.ConflictCandidates through to the update
		// params below; this keeps an existing snapshot intact when
		// a duplicate meeting_note.recorded arrives on a
		// conflict_pending row.
		finalLinkageState = prior.LinkageState
		finalLinkedKind = prior.LinkedKind
		finalLinkedID = prior.LinkedID
	} else {
		// First-insert, revive, or re-link → run linkage fresh.
		windowStart := p.MeetingAt.Add(-linkageWindow)
		windowEnd := p.MeetingAt.Add(linkageWindow)
		candidates, candErr := s.calendar.FindLinkageCandidatesTx(ctx, tx, windowStart, windowEnd)
		if candErr != nil {
			return nil, nil, &IngestPerEventRejection{
				Code:    ingestRejectLinkageQueryFailed,
				Message: fmt.Sprintf("find linkage candidates: %s", candErr.Error()),
			}
		}
		if s.phoneCallLinkage != nil {
			pcCands, pcErr := s.phoneCallLinkage.FindLinkageCandidatesTx(ctx, tx, windowStart, windowEnd)
			if pcErr != nil {
				return nil, nil, &IngestPerEventRejection{
					Code:    ingestRejectLinkageQueryFailed,
					Message: fmt.Sprintf("find phone_call linkage candidates: %s", pcErr.Error()),
				}
			}
			candidates = append(candidates, pcCands...)
		}
		candidatesLen = len(candidates)
		// Post-coalesce count for observability — coalescedLen < candidatesLen
		// means coalescing collapsed a mirrored-meeting / dropped-redial
		// pair. decideLinkage runs the same idempotent pass internally; the
		// redundant call is cheap (window candidate sets are single digits)
		// and side-effect-free.
		coalescedLen = len(coalesceCandidates(candidates))
		finalLinkageState, finalLinkedKind, finalLinkedID, desired, conflictSnapshot = decideLinkage(p, sessionID, candidates, resolvedTagged, titleMatched)
		if len(conflictSnapshot) > 0 {
			// Observability — log the top overlap whether or not we
			// landed on conflict_pending. The auto-resolved branch
			// still benefits from the field (it surfaces "we picked
			// because the winner had N overlapping participants").
			conflictOverlapTop = conflictSnapshot[0].OverlapCount
		}
		if finalLinkageState == repository.LinkageStateConflictPending && len(conflictSnapshot) > 0 {
			marshaled, mErr := json.Marshal(conflictSnapshot)
			if mErr != nil {
				return nil, nil, &IngestPerEventRejection{
					Code:    ingestRejectMeetingNoteUpsertFailed,
					Message: fmt.Sprintf("marshal conflict candidates snapshot: %s", mErr.Error()),
				}
			}
			conflictCandidatesBytes = marshaled
		}
	}

	// 6. Write meeting_note row. Branch on first-insert / revive /
	//    carry-forward / re-link. The repository params are nearly
	//    identical across branches; only the destination method differs.
	//
	// conflict_candidates handling:
	//   - First-insert / revive / re-link: pass conflictCandidatesBytes,
	//     which is the freshly marshaled snapshot when the new state is
	//     conflict_pending and nil otherwise.
	//   - Carry-forward: pass prior.ConflictCandidates verbatim so an
	//     existing snapshot is preserved when a duplicate event arrives.
	updateConflictCandidates := conflictCandidatesBytes
	if carryForward {
		updateConflictCandidates = prior.ConflictCandidates
	}
	params := repository.InsertMeetingNoteParams{
		AnarlogSessionID:   sessionID,
		Title:              p.Title,
		Summary:            p.Summary,
		Memo:               p.Memo,
		Participants:       p.ParticipantIDs,
		MacHostID:          &authenticatedHostID,
		LinkedKind:         finalLinkedKind,
		LinkedID:           finalLinkedID,
		LinkageState:       finalLinkageState,
		InputHash:          newInputHash,
		ResolvedSetHash:    newResolvedSetHash,
		LastContentHash:    &contentHash,
		MeetingAt:          p.MeetingAt,
		ConflictCandidates: conflictCandidatesBytes,
	}
	switch {
	case prior == nil:
		if _, err := s.meetingNotes.InsertMeetingNoteTx(ctx, tx, params); err != nil {
			if !errors.Is(err, db.ErrNotFound) {
				return nil, nil, &IngestPerEventRejection{
					Code:    ingestRejectMeetingNoteUpsertFailed,
					Message: fmt.Sprintf("insert meeting_note: %s", err.Error()),
				}
			}
			// Concurrent first-insert won the race against the partial
			// unique index. Re-read the row (held with FOR UPDATE for
			// the rest of this tx) and fall through to the update path
			// so this event's content + linkage values land via UPDATE
			// instead of erroring as PAYLOAD_INVARIANT.
			racedRow, getErr := s.meetingNotes.GetMeetingNoteBySessionIDForUpdateTx(ctx, tx, sessionID)
			if getErr != nil {
				return nil, nil, &IngestPerEventRejection{
					Code:    ingestRejectMeetingNoteUpsertFailed,
					Message: fmt.Sprintf("re-read meeting_note after concurrent insert: %s", getErr.Error()),
				}
			}
			updateParams := repository.UpdateMeetingNoteOnResyncParams{
				ID:                 racedRow.ID,
				Title:              p.Title,
				Summary:            p.Summary,
				Memo:               p.Memo,
				Participants:       p.ParticipantIDs,
				LinkedKind:         finalLinkedKind,
				LinkedID:           finalLinkedID,
				LinkageState:       finalLinkageState,
				InputHash:          newInputHash,
				ResolvedSetHash:    newResolvedSetHash,
				LastContentHash:    &contentHash,
				MeetingAt:          p.MeetingAt,
				ConflictCandidates: conflictCandidatesBytes,
			}
			if _, err := s.meetingNotes.UpdateMeetingNoteOnResyncTx(ctx, tx, updateParams); err != nil {
				return nil, nil, &IngestPerEventRejection{
					Code:    ingestRejectMeetingNoteUpsertFailed,
					Message: fmt.Sprintf("update meeting_note after concurrent insert: %s", err.Error()),
				}
			}
		}
	case revivePath:
		reviveParams := repository.ReviveMeetingNoteParams{
			ID:                 prior.ID,
			Title:              p.Title,
			Summary:            p.Summary,
			Memo:               p.Memo,
			Participants:       p.ParticipantIDs,
			LinkedKind:         finalLinkedKind,
			LinkedID:           finalLinkedID,
			LinkageState:       finalLinkageState,
			InputHash:          newInputHash,
			ResolvedSetHash:    newResolvedSetHash,
			LastContentHash:    &contentHash,
			MeetingAt:          p.MeetingAt,
			ConflictCandidates: conflictCandidatesBytes,
		}
		if _, err := s.meetingNotes.ReviveMeetingNoteTx(ctx, tx, reviveParams); err != nil {
			return nil, nil, &IngestPerEventRejection{
				Code:    ingestRejectMeetingNoteUpsertFailed,
				Message: fmt.Sprintf("revive meeting_note: %s", err.Error()),
			}
		}
	default:
		updateParams := repository.UpdateMeetingNoteOnResyncParams{
			ID:                 prior.ID,
			Title:              p.Title,
			Summary:            p.Summary,
			Memo:               p.Memo,
			Participants:       p.ParticipantIDs,
			LinkedKind:         finalLinkedKind,
			LinkedID:           finalLinkedID,
			LinkageState:       finalLinkageState,
			InputHash:          newInputHash,
			ResolvedSetHash:    newResolvedSetHash,
			LastContentHash:    &contentHash,
			MeetingAt:          p.MeetingAt,
			ConflictCandidates: updateConflictCandidates,
		}
		if _, err := s.meetingNotes.UpdateMeetingNoteOnResyncTx(ctx, tx, updateParams); err != nil {
			return nil, nil, &IngestPerEventRejection{
				Code:    ingestRejectMeetingNoteUpsertFailed,
				Message: fmt.Sprintf("update meeting_note: %s", err.Error()),
			}
		}
	}

	// 7. Apply interaction writes — carry-forward path skips both
	//    sides; first-insert/revive insert the full desired set;
	//    re-link diffs against the existing live set and adds/drops
	//    accordingly. Calendar/phone-call interactions are NEVER
	//    touched here — we filter by source='anarlog_sessions' +
	//    source_ref prefix.
	if !carryForward {
		sourceRefPrefix := fmt.Sprintf("anarlog:%s:%%", sessionID.String())
		existing, listErr := s.interactions.ListSessionAttributedInteractionsTx(ctx, tx, sourceRefPrefix)
		if listErr != nil {
			return nil, nil, &IngestPerEventRejection{
				Code:    ingestRejectInteractionWriteFailed,
				Message: fmt.Sprintf("list session-attributed interactions: %s", listErr.Error()),
			}
		}
		existingByRef := make(map[string]repository.Interaction, len(existing))
		for _, x := range existing {
			if x.SourceRef != nil {
				existingByRef[*x.SourceRef] = x
			}
		}
		desiredByRef := make(map[string]desiredInteraction, len(desired))
		for _, d := range desired {
			desiredByRef[d.SourceRef] = d
		}
		// to_drop = existing \ desired
		for ref, x := range existingByRef {
			if _, want := desiredByRef[ref]; want {
				continue
			}
			if err := s.interactions.SoftDeleteInteractionTx(ctx, tx, x.ID); err != nil {
				return nil, nil, &IngestPerEventRejection{
					Code:    ingestRejectInteractionWriteFailed,
					Message: fmt.Sprintf("soft-delete obsolete interaction: %s", err.Error()),
				}
			}
			logger.Warn().
				Str("event", "session_re_sync_dropped").
				Str("session_id", p.SourceID).
				Str("source_ref", ref).
				Msg("meeting_note re-sync soft-deleted obsolete session interaction")
			interactionsDropped++
		}
		// in-both: existing source_ref still desired → refresh the
		// content fields (occurred_at + description) when the payload
		// changed so the timeline view reflects the latest meeting_at
		// and title. Routes through ContactService.ExtendInteractionTx
		// which re-applies cadence (last_contacted / contact_by) via
		// the sole-writer path and returns a follow-up closure to run
		// after commit — direct UPDATE would leave cadence stale.
		for ref, d := range desiredByRef {
			x, have := existingByRef[ref]
			if !have {
				continue
			}
			needsRefresh := !x.OccurredAt.Equal(p.MeetingAt) ||
				!stringPtrEqual(x.Description, p.Title)
			if !needsRefresh {
				continue
			}
			refreshFn, err := s.contactSvc.ExtendInteractionTx(ctx, tx, x.ID, d.ContactID, repository.InteractionDirectionMutual, p.MeetingAt, p.Title)
			if err != nil {
				return nil, nil, &IngestPerEventRejection{
					Code:    ingestRejectInteractionWriteFailed,
					Message: fmt.Sprintf("refresh session interaction: %s", err.Error()),
				}
			}
			if refreshFn != nil {
				followUps = append(followUps, refreshFn)
			}
		}
		// to_add = desired \ existing (route through ContactService so
		// cadence + follow-up evaluation fire correctly). Capture the
		// FollowUpFn closures so the dispatch loop can run them after
		// the outer tx commits.
		// Resolve the session venue once for the whole batch (all desired
		// interactions belong to the same session): reuse the linked gcal
		// meeting venue when this note links to an event, else mint a session
		// venue. Best-effort — a resolution error leaves venue_id NULL.
		sessionVenueID, venueErr := resolveAnarlogSessionVenue(ctx, tx, s.venue, sessionID, finalLinkedKind, finalLinkedID)
		if venueErr != nil {
			return nil, nil, &IngestPerEventRejection{
				Code:    ingestRejectInteractionWriteFailed,
				Message: fmt.Sprintf("resolve session venue: %s", venueErr.Error()),
			}
		}
		for ref, d := range desiredByRef {
			if _, have := existingByRef[ref]; have {
				continue
			}
			sourceRef := d.SourceRef
			req := repository.RecordInteractionRequest{
				ContactID:   d.ContactID,
				Source:      repository.InteractionSourceAnarlogSessions,
				SourceRef:   &sourceRef,
				OccurredAt:  p.MeetingAt,
				Description: p.Title,
				Direction:   repository.InteractionDirectionMutual,
				VenueID:     sessionVenueID,
			}
			res, err := s.contactSvc.RecordInteractionTx(ctx, tx, false, req)
			if err != nil {
				return nil, nil, &IngestPerEventRejection{
					Code:    ingestRejectInteractionWriteFailed,
					Message: fmt.Sprintf("record session interaction: %s", err.Error()),
				}
			}
			if res != nil && res.FollowUpFn != nil {
				followUps = append(followUps, res.FollowUpFn)
			}
			interactionsCreated++
		}
	}

	// 8. Discovery upsert for unmatched title tokens. Runs
	//    UNCONDITIONALLY (not gated on !carryForward) so legacy
	//    pre-title-parsing sessions whose hash happens to match under
	//    both old and new recipes still backfill their discovery rows
	//    on first re-ingest. The deterministic source_id (sha256 of
	//    token||session_uuid) makes re-emit a cheap UPDATE refresh on
	//    existing rows.
	for _, token := range titleUnmatched {
		if dErr := s.discovery.UpsertTitleCandidateTx(ctx, tx, sessionID, strings.ToLower(token), token); dErr != nil {
			return nil, nil, &IngestPerEventRejection{
				Code:    ingestRejectTitleDiscoveryUpsertFailed,
				Message: fmt.Sprintf("upsert title candidate %q: %s", token, dErr.Error()),
			}
		}
	}

	// 9. Single-point needs_attention computation. The caller appends
	//    this AFTER savepoint commit so a rollback path cannot leak an
	//    item into the batch accumulator.
	var needs *NeedsAttentionItem
	switch finalLinkageState {
	case repository.LinkageStateConflictPending:
		needs = &NeedsAttentionItem{SessionID: p.SourceID, Reason: NeedsAttentionReasonConflict}
	case repository.LinkageStateOrphanNeedsReview:
		needs = &NeedsAttentionItem{SessionID: p.SourceID, Reason: NeedsAttentionReasonOrphan}
	}

	// 10. Structured log line for observability.
	priorInputHash := ""
	priorResolvedSetHash := ""
	if prior != nil {
		priorInputHash = prior.InputHash
		priorResolvedSetHash = prior.ResolvedSetHash
	}
	linkedKindStr := ""
	linkedIDStr := ""
	if finalLinkedKind != nil {
		linkedKindStr = *finalLinkedKind
	}
	if finalLinkedID != nil {
		linkedIDStr = finalLinkedID.String()
	}
	logger.Info().
		Str("event", "linkage_decision").
		Str("session_id", p.SourceID).
		Int("candidates", candidatesLen).
		Int("coalesced_candidates", coalescedLen).
		Str("linkage_state", finalLinkageState).
		Str("linked_kind", linkedKindStr).
		Str("linked_id", linkedIDStr).
		Int("tagged_total", len(p.ParticipantIDs)).
		Int("tagged_resolved", len(resolvedTagged)).
		Int("tagged_unresolved", len(p.ParticipantIDs)-len(resolvedTagged)).
		Int("interactions_created", interactionsCreated).
		Int("interactions_dropped", interactionsDropped).
		Int("title_tokens_extracted", len(titleTokens)).
		Int("title_tokens_matched", len(titleMatched)).
		Int("title_tokens_unmatched", len(titleUnmatched)).
		Int("conflict_candidates_count", len(conflictSnapshot)).
		Int("conflict_overlap_top", conflictOverlapTop).
		Bool("revive_path", revivePath).
		Bool("carry_forward", carryForward).
		Bool("input_hash_changed", priorInputHash != newInputHash).
		Bool("resolved_set_changed", priorResolvedSetHash != newResolvedSetHash).
		Msg("meeting_note linkage decision")

	return needs, followUps, nil
}

// resolveAnarlogSessionVenue resolves the venue node id for an anarlog session's
// interactions. When the note links to a calendar event (linked_kind='event')
// the session REUSES that event's meeting venue (the only cross-source venue
// merge); otherwise it mints/finds a session venue keyed on the session id.
// Best-effort: returns (nil, nil) when the venue resolver is unwired or the
// linked gcal venue can't be resolved, so the interaction records with a NULL
// venue_id rather than failing. Only a real DB error propagates. Shared by the
// ingest inline handler and MeetingNoteService's resolve-link path.
func resolveAnarlogSessionVenue(
	ctx context.Context, tx pgx.Tx, venue IngestVenueResolver, sessionID uuid.UUID, linkedKind *string, linkedID *uuid.UUID,
) (*uuid.UUID, error) {
	if venue == nil {
		return nil, nil
	}
	// Linked to a calendar event → reuse the gcal meeting venue.
	if linkedKind != nil && *linkedKind == repository.LinkedKindEvent && linkedID != nil {
		venueID, err := venue.ResolveGCalVenueTx(ctx, tx, *linkedID)
		if err != nil {
			return nil, err
		}
		if venueID != nil {
			return venueID, nil
		}
		// Linked event venue couldn't be resolved (event row gone) → fall
		// through to a session venue so the session still gets a container.
	}
	venueID, err := venue.ResolveVenueForInteractionTx(
		ctx, tx, repository.InteractionSourceAnarlogSessions, repository.VenueKindSession, sessionID.String(), "")
	if err != nil {
		return nil, err
	}
	return &venueID, nil
}

// coalesceCandidates collapses semantically-identical linkage candidates
// within a kind so that two equivalents become one, before decideLinkage
// runs its 0/1/2+ classification. Two genuinely-distinct DB rows that
// represent the same real-world interaction — the same calendar meeting
// mirrored across two series/calendars (#370), or a dropped attempt + its
// redial to the same number (#371) — would otherwise force a spurious
// 2+ conflict. Cross-kind candidates are NEVER compared (an event and a
// phone_call in the same window stay two candidates).
//
// Survivors are emitted in the original input order (a single pass over
// the input keeps the kept candidate of each group), so the output is
// deterministic and independent of map iteration order. The function is
// idempotent and side-effect-free: a coalesced slice has no remaining
// coalescible groups, so a second pass is a no-op.
func coalesceCandidates(candidates []repository.LinkageCandidate) []repository.LinkageCandidate {
	if len(candidates) < 2 {
		return candidates
	}

	var events, calls []repository.LinkageCandidate
	for _, c := range candidates {
		switch c.Kind {
		case repository.LinkedKindEvent:
			events = append(events, c)
		case repository.LinkedKindPhoneCall:
			calls = append(calls, c)
		}
	}

	keep := make(map[uuid.UUID]struct{}, len(candidates))
	for id := range coalesceCalendar(events) {
		keep[id] = struct{}{}
	}
	for id := range coalescePhone(calls) {
		keep[id] = struct{}{}
	}

	out := make([]repository.LinkageCandidate, 0, len(candidates))
	for _, c := range candidates {
		// Kinds other than event/phone_call are never grouped (none exist
		// today); pass them through untouched. Grouped kinds survive iff
		// they are their group's chosen representative.
		if c.Kind != repository.LinkedKindEvent && c.Kind != repository.LinkedKindPhoneCall {
			out = append(out, c)
			continue
		}
		if _, ok := keep[c.ID]; ok {
			out = append(out, c)
		}
	}
	return out
}

// coalesceCalendar returns the set of event-candidate IDs that survive
// the #370 same-meeting coalescing rule. Candidates are grouped by
// (NormalizedTitle, OccurredAt); within each group of 2+ the
// representative is the one with the most matched attendees, tie-broken
// by lowest ID. Empty-title candidates carry no semantic identity and are
// never coalesced (each survives on its own). The group key derives from
// the UTC instant so two times that .Equal share a key regardless of
// location.
func coalesceCalendar(events []repository.LinkageCandidate) map[uuid.UUID]struct{} {
	keep := make(map[uuid.UUID]struct{}, len(events))
	groups := make(map[string][]repository.LinkageCandidate)
	for _, c := range events {
		if c.NormalizedTitle == "" {
			keep[c.ID] = struct{}{}
			continue
		}
		key := c.NormalizedTitle + "\x00" + c.OccurredAt.UTC().Format(time.RFC3339Nano)
		groups[key] = append(groups[key], c)
	}
	for _, group := range groups {
		rep := pickCalendarRepresentative(group)
		keep[rep] = struct{}{}
	}
	return keep
}

// pickCalendarRepresentative returns the ID of the representative of a
// calendar group: most matched attendees, then lowest ID. Collects to a
// slice and sorts (never relies on map iteration order).
func pickCalendarRepresentative(group []repository.LinkageCandidate) uuid.UUID {
	ranked := append([]repository.LinkageCandidate(nil), group...)
	sort.Slice(ranked, func(i, j int) bool {
		ai, aj := len(ranked[i].AttendeeContactIDs), len(ranked[j].AttendeeContactIDs)
		if ai != aj {
			return ai > aj
		}
		return ranked[i].ID.String() < ranked[j].ID.String()
	})
	return ranked[0].ID
}

// coalescePhone returns the set of phone_call-candidate IDs that survive
// the #371 dropped-redial coalescing rule. Candidates are partitioned by
// (PeerNormalized, Service, Direction) — all semantically-significant
// dimensions, so opposite-direction or different-service same-number rows
// never collapse. Within a partition, a sort-then-sweep single-linkage
// cluster groups candidates whose consecutive gap is within
// phoneCoalesceWindow; the representative of each cluster is the connected
// (answered) / longest / interaction-linked call, tie-broken by lowest ID.
// Candidates with an empty PeerNormalized carry no key and are never
// coalesced.
func coalescePhone(calls []repository.LinkageCandidate) map[uuid.UUID]struct{} {
	keep := make(map[uuid.UUID]struct{}, len(calls))
	partitions := make(map[string][]repository.LinkageCandidate)
	for _, c := range calls {
		if c.PeerNormalized == "" {
			keep[c.ID] = struct{}{}
			continue
		}
		key := c.PeerNormalized + "\x00" + c.Service + "\x00" + c.Direction
		partitions[key] = append(partitions[key], c)
	}
	for _, partition := range partitions {
		sort.Slice(partition, func(i, j int) bool {
			if !partition[i].OccurredAt.Equal(partition[j].OccurredAt) {
				return partition[i].OccurredAt.Before(partition[j].OccurredAt)
			}
			return partition[i].ID.String() < partition[j].ID.String()
		})
		// Single-linkage sweep: open a new cluster whenever the gap from
		// the PREVIOUS candidate exceeds the window (gap-between-consecutive,
		// not gap-from-anchor) — matches "redial seconds later, possibly
		// several times".
		cluster := []repository.LinkageCandidate{partition[0]}
		flush := func() {
			keep[pickPhoneRepresentative(cluster)] = struct{}{}
		}
		for i := 1; i < len(partition); i++ {
			if partition[i].OccurredAt.Sub(partition[i-1].OccurredAt) > phoneCoalesceWindow {
				flush()
				cluster = []repository.LinkageCandidate{partition[i]}
				continue
			}
			cluster = append(cluster, partition[i])
		}
		flush()
	}
	return keep
}

// pickPhoneRepresentative returns the ID of the representative of a phone
// cluster: prefer answered/connected, then greatest DurationSeconds, then
// a non-nil InteractionID, final tie-break lowest ID. Collects to a slice
// and sorts (never relies on map iteration order).
func pickPhoneRepresentative(cluster []repository.LinkageCandidate) uuid.UUID {
	ranked := append([]repository.LinkageCandidate(nil), cluster...)
	answered := func(c repository.LinkageCandidate) bool {
		return c.Answered != nil && *c.Answered
	}
	sort.Slice(ranked, func(i, j int) bool {
		if ai, aj := answered(ranked[i]), answered(ranked[j]); ai != aj {
			return ai
		}
		if ranked[i].DurationSeconds != ranked[j].DurationSeconds {
			return ranked[i].DurationSeconds > ranked[j].DurationSeconds
		}
		if hi, hj := ranked[i].InteractionID != nil, ranked[j].InteractionID != nil; hi != hj {
			return hi
		}
		return ranked[i].ID.String() < ranked[j].ID.String()
	})
	return ranked[0].ID
}

// decideLinkage implements the spec's linkage-detection algorithm.
// Returns the linkage state, linked_kind/id pointers, the desired
// session-attributed interaction set, AND the conflict-candidate
// snapshot (non-nil only when the resulting state is conflict_pending —
// see disambiguateCandidates).
//
//   - 0 candidates, no tagged humans → orphan_needs_review (no
//     interactions). Title matches DO NOT promote a no-anchor orphan
//     to augmented per spec invariant (tagged humans are the anchor).
//   - 0 candidates, ≥1 resolved tagged human, 0 title-matched →
//     linked_impromptu (one interaction per resolved contact).
//   - 0 candidates, ≥1 resolved tagged human, ≥1 title-matched →
//     orphan_title_augmented (tagged interactions PLUS one
//     `:title:` interaction per title-matched contact).
//   - 1 candidate → linked. A walk-in supplemental interaction is
//     emitted for each resolved tagged contact NOT already in the
//     candidate's intrinsic attendee set (event matched_contact_ids
//     OR phone_call peer per LinkageCandidate.ImpliedAttendeeSet).
//     Title matches DO NOT produce interactions in this state.
//   - 2+ candidates → Step 3 participant-signal disambiguation. If
//     exactly one candidate has the strictly-highest non-zero overlap
//     with the implied contact set (tagged ∪ title-matched), auto-link
//     to it and run Step 5 walk-in supplemental as if there had been
//     one candidate from the start. Otherwise → conflict_pending with
//     the snapshot of the per-candidate overlap table.
func decideLinkage(
	p events.MeetingNoteRecordedPayload,
	sessionID uuid.UUID,
	candidates []repository.LinkageCandidate,
	resolvedTagged []resolvedTag,
	titleMatched []resolvedTitle,
) (state string, linkedKind *string, linkedID *uuid.UUID, desired []desiredInteraction, snapshot []repository.ConflictCandidateSummary) {
	// Collapse semantically-identical candidates within a kind before the
	// 0/1/2+ classification — a mirrored meeting or dropped-redial pair
	// becomes one candidate and flows into the existing auto-link path.
	candidates = coalesceCandidates(candidates)
	switch len(candidates) {
	case 0:
		if len(resolvedTagged) == 0 {
			return repository.LinkageStateOrphanNeedsReview, nil, nil, nil, nil
		}
		out := taggedImpromptuInteractions(sessionID, resolvedTagged)
		if len(titleMatched) > 0 {
			for _, tm := range titleMatched {
				out = append(out, desiredInteraction{
					ContactID: tm.ContactID,
					SourceRef: fmt.Sprintf("anarlog:%s:title:%s", sessionID.String(), tm.ContactID.String()),
				})
			}
			return repository.LinkageStateOrphanTitleAugmented, nil, nil, out, nil
		}
		return repository.LinkageStateLinkedImpromptu, nil, nil, out, nil
	case 1:
		cand := candidates[0]
		walkins := stepFiveWalkins(cand, resolvedTagged, sessionID)
		kind := cand.Kind
		id := cand.ID
		return repository.LinkageStateLinked, &kind, &id, walkins, nil
	default:
		implied := buildImpliedSet(resolvedTagged, titleMatched)
		winner, snap := disambiguateCandidates(candidates, implied)
		if winner != nil {
			walkins := stepFiveWalkins(*winner, resolvedTagged, sessionID)
			kind := winner.Kind
			id := winner.ID
			// Snapshot returned for observability — caller logs but
			// must NOT persist on a linked outcome (the column
			// invariant: NULL for any state other than conflict_pending).
			return repository.LinkageStateLinked, &kind, &id, walkins, snap
		}
		return repository.LinkageStateConflictPending, nil, nil, nil, snap
	}
}

// taggedImpromptuInteractions builds one impromptu desiredInteraction per
// resolved tagged contact with the canonical
// `anarlog:<session>:<contact>` source_ref. Shared by decideLinkage's
// zero-candidate branch and the user-driven "Log as impromptu" resolve so
// the two paths cannot drift in how they shape the tagged interaction set.
// Returns an empty (non-nil) slice for empty input.
func taggedImpromptuInteractions(sessionID uuid.UUID, tagged []resolvedTag) []desiredInteraction {
	out := make([]desiredInteraction, 0, len(tagged))
	for _, r := range tagged {
		out = append(out, desiredInteraction{
			ContactID: r.ContactID,
			SourceRef: fmt.Sprintf("anarlog:%s:%s", sessionID.String(), r.ContactID.String()),
		})
	}
	return out
}

// stepFiveWalkins emits one walk-in supplemental desiredInteraction per
// resolved tagged contact that isn't in the linked candidate's intrinsic
// attendee set. Centralized so the daemon-side `case 1:` branch AND the
// Step 3 auto-link path share the same kind-aware computation
// (LinkageCandidate.ImpliedAttendeeSet is correct for both event and
// phone_call kinds).
func stepFiveWalkins(cand repository.LinkageCandidate, resolvedTagged []resolvedTag, sessionID uuid.UUID) []desiredInteraction {
	attendees := cand.ImpliedAttendeeSet()
	walkins := make([]desiredInteraction, 0)
	for _, r := range resolvedTagged {
		if _, present := attendees[r.ContactID]; present {
			continue
		}
		walkins = append(walkins, desiredInteraction{
			ContactID: r.ContactID,
			SourceRef: fmt.Sprintf("anarlog:%s:walkin:%s", sessionID.String(), r.ContactID.String()),
		})
	}
	return walkins
}

// buildImpliedSet returns the set of contact_ids derived from the
// union of resolvedTagged.ContactID and titleMatched.ContactID per
// spec §Step 3.1. Title matches already deduplicate against
// resolvedTagged upstream (handleMeetingNoteRecorded), so the union
// is effectively just resolvedTagged ∪ titleMatched.
func buildImpliedSet(resolvedTagged []resolvedTag, titleMatched []resolvedTitle) map[uuid.UUID]struct{} {
	implied := make(map[uuid.UUID]struct{}, len(resolvedTagged)+len(titleMatched))
	for _, r := range resolvedTagged {
		implied[r.ContactID] = struct{}{}
	}
	for _, tm := range titleMatched {
		implied[tm.ContactID] = struct{}{}
	}
	return implied
}

// disambiguateCandidates implements Step 3 of the spec's
// linkage-detection algorithm. Returns (winner, snapshot) where:
//
//   - winner is a pointer into the candidates slice when exactly one
//     candidate has the strictly-highest non-zero overlap with the
//     implied contact set; nil otherwise.
//   - snapshot is the full per-candidate overlap table sorted by
//     overlap_count desc, then occurred_at asc as a deterministic
//     tie-breaker.
//
// The caller is expected to gate on len(candidates) >= 2 (the case 0/1
// branches in decideLinkage handle smaller sets); the helper still
// degrades gracefully on smaller inputs.
//
// An empty implied set always yields nil winner — no signal to
// disambiguate with — and returns the snapshot with all overlap_count=0.
func disambiguateCandidates(
	candidates []repository.LinkageCandidate,
	implied map[uuid.UUID]struct{},
) (*repository.LinkageCandidate, []repository.ConflictCandidateSummary) {
	snapshot := make([]repository.ConflictCandidateSummary, 0, len(candidates))
	for i := range candidates {
		c := &candidates[i]
		overlap := 0
		switch c.Kind {
		case repository.LinkedKindEvent:
			for _, aid := range c.AttendeeContactIDs {
				if _, ok := implied[aid]; ok {
					overlap++
				}
			}
		case repository.LinkedKindPhoneCall:
			if c.PeerContactID != nil {
				if _, ok := implied[*c.PeerContactID]; ok {
					overlap = 1
				}
			}
		}
		snapshot = append(snapshot, repository.ConflictCandidateSummary{
			Kind:         c.Kind,
			ID:           c.ID,
			OccurredAt:   c.OccurredAt,
			OverlapCount: overlap,
		})
	}
	sort.SliceStable(snapshot, func(i, j int) bool {
		if snapshot[i].OverlapCount != snapshot[j].OverlapCount {
			return snapshot[i].OverlapCount > snapshot[j].OverlapCount
		}
		return snapshot[i].OccurredAt.Before(snapshot[j].OccurredAt)
	})

	if len(snapshot) == 0 {
		return nil, snapshot
	}
	top := snapshot[0]
	if top.OverlapCount == 0 {
		return nil, snapshot
	}
	if len(snapshot) > 1 && snapshot[1].OverlapCount == top.OverlapCount {
		return nil, snapshot
	}
	for i := range candidates {
		if candidates[i].ID == top.ID {
			return &candidates[i], snapshot
		}
	}
	return nil, snapshot
}

// handleMeetingNoteDeleted runs the per-event domain logic for a
// meeting_note.deleted envelope inside the per-event savepoint.
//
// Behavior:
//   - Unknown session → silent no-op (event-log row preserved).
//   - Already tombstoned → idempotent no-op.
//   - Host-ownership mismatch → warn-log + silent no-op (mirrors
//     handleExternalContactDeleted at service/ingest.go:1037).
//   - Live row + hash-mismatch (and not @unknown sentinel, and stored
//     hash non-NULL) → MEETING_NOTE_DELETE_HASH_MISMATCH.
//   - Live row → soft-delete all session-attributed interactions
//     (source='anarlog_sessions' AND source_ref LIKE 'anarlog:<sid>:%')
//     and soft-delete the meeting_note row. The interaction cascade is
//     explicit because UPDATE deleted_at does NOT trigger ON DELETE
//     CASCADE (CLAUDE.md gotcha).
func (s *IngestService) handleMeetingNoteDeleted(
	ctx context.Context,
	tx pgx.Tx,
	env *events.Envelope,
	authenticatedHostID uuid.UUID,
) *IngestPerEventRejection {
	if s.meetingNotes == nil || s.interactions == nil {
		return &IngestPerEventRejection{
			Code:    ingestRejectPayloadInvariant,
			Message: "ingest service was not configured for meeting_note processing",
		}
	}

	var p events.MeetingNoteDeletedPayload
	if err := json.Unmarshal(env.Payload, &p); err != nil {
		return &IngestPerEventRejection{
			Code:    ingestRejectPayloadInvalid,
			Message: fmt.Sprintf("decode meeting_note.deleted payload: %s", err.Error()),
		}
	}
	sessionID, parseErr := uuid.Parse(p.SourceID)
	if parseErr != nil {
		return &IngestPerEventRejection{
			Code:    ingestRejectPayloadInvalid,
			Message: fmt.Sprintf("parse session UUID: %s", parseErr.Error()),
		}
	}

	prior, getErr := s.meetingNotes.GetMeetingNoteBySessionIDForUpdateTx(ctx, tx, sessionID)
	if getErr != nil {
		if errors.Is(getErr, db.ErrNotFound) {
			// Unknown session — silent no-op.
			return nil
		}
		return &IngestPerEventRejection{
			Code:    ingestRejectMeetingNoteDeleteFailed,
			Message: fmt.Sprintf("pre-read meeting_note: %s", getErr.Error()),
		}
	}
	if prior == nil || prior.DeletedAt != nil {
		// Already tombstoned. Idempotent no-op.
		return nil
	}

	// Host-ownership guard. Mirrors handleExternalContactDeleted.
	if prior.MacHostID != nil && *prior.MacHostID != authenticatedHostID {
		logger.Warn().
			Str("source", env.Source).
			Str("session_id", p.SourceID).
			Str("prior_host_id", prior.MacHostID.String()).
			Str("authenticated_host_id", authenticatedHostID.String()).
			Msg("meeting_note.deleted dropped: row owned by a different host")
		return nil
	}

	// Hash-mismatch check (same three exceptions as
	// handleExternalContactDeleted: @unknown sentinel, NULL stored
	// hash, already-tombstoned — handled above).
	if !strings.HasSuffix(env.SourceID, meetingNoteDeleteUnknownSuffix) && prior.LastContentHash != nil {
		claimed := env.SourceID[len(env.SourceID)-64:]
		if *prior.LastContentHash != claimed {
			return &IngestPerEventRejection{
				Code: ingestRejectMeetingNoteDeleteHashMismatch,
				Message: fmt.Sprintf(
					"delete source_id hash %s does not match stored last_content_hash %s for session %s",
					claimed, *prior.LastContentHash, p.SourceID),
			}
		}
	}

	// Cascade soft-delete to session-attributed interactions. UPDATE
	// deleted_at does NOT trigger ON DELETE CASCADE — must do this
	// explicitly (CLAUDE.md gotcha).
	sourceRefPrefix := fmt.Sprintf("anarlog:%s:%%", sessionID.String())
	existing, listErr := s.interactions.ListSessionAttributedInteractionsTx(ctx, tx, sourceRefPrefix)
	if listErr != nil {
		return &IngestPerEventRejection{
			Code:    ingestRejectMeetingNoteDeleteFailed,
			Message: fmt.Sprintf("list session-attributed interactions: %s", listErr.Error()),
		}
	}
	for _, ix := range existing {
		if err := s.interactions.SoftDeleteInteractionTx(ctx, tx, ix.ID); err != nil {
			return &IngestPerEventRejection{
				Code:    ingestRejectMeetingNoteDeleteFailed,
				Message: fmt.Sprintf("soft-delete session interaction: %s", err.Error()),
			}
		}
		ref := ""
		if ix.SourceRef != nil {
			ref = *ix.SourceRef
		}
		logger.Warn().
			Str("event", "session_tombstoned").
			Str("session_id", p.SourceID).
			Str("source_ref", ref).
			Msg("meeting_note tombstoned cascaded soft-delete to session interaction")
	}

	if err := s.meetingNotes.SoftDeleteMeetingNoteBySessionIDTx(ctx, tx, sessionID); err != nil {
		return &IngestPerEventRejection{
			Code:    ingestRejectMeetingNoteDeleteFailed,
			Message: fmt.Sprintf("soft-delete meeting_note: %s", err.Error()),
		}
	}
	return nil
}

// stringPtrEqual reports whether two *string pointers point to the
// same string value (nil == nil; nil != "x").
func stringPtrEqual(a, b *string) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return *a == *b
}
