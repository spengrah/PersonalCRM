// Package service provides business-logic wrappers around repositories.
// IngestService owns the single-tx batch-publish semantics for the HTTP
// ingestion endpoint introduced in PR 4 of the event-bus-foundation spec.
package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/consumer/consumerjobs"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/events"
	"personal-crm/backend/internal/identity"
	"personal-crm/backend/internal/logger"
	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
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
)

// ErrHostRevokedDuringBatch is returned by IngestBatch when the
// tx-internal host-liveness re-check (SELECT ... FOR UPDATE on
// mac_host) finds the host revoked between auth-middleware validation
// and the batch's lock acquire. The handler translates this to
// 401 UNKNOWN_HOST, matching the cursor-commit precedent in
// MacHostHandler.CommitCursor.
var ErrHostRevokedDuringBatch = errors.New("host revoked during ingest batch")

// allowedExternalContactSources is the set of envelope.Source values the
// external_contact.* inline handler accepts. Mismatches surface as
// PAYLOAD_INVARIANT. Extend when a new daemon-side source ships.
var allowedExternalContactSources = map[string]struct{}{
	"icloud_contacts": {},
}

// reExternalContactUpsertSourceID matches the envelope SourceID shape
// for external_contact.upserted events: `<entity_uuid>@<sha256-hex>`.
// The entity portion may not contain `@`; the hash is exactly 64 hex
// characters (SHA-256). Compiled once at package load.
var reExternalContactUpsertSourceID = regexp.MustCompile(`^[^@]+@[a-f0-9]{64}$`)

// reExternalContactDeleteSourceID matches the envelope SourceID shape
// for external_contact.deleted events:
// `<entity_uuid>@deleted@<sha256-hex>` or `<entity_uuid>@deleted@unknown`.
var reExternalContactDeleteSourceID = regexp.MustCompile(`^[^@]+@deleted@([a-f0-9]{64}|unknown)$`)

// IngestPerEventRejection is a per-event domain rejection emitted by the
// service-layer inline handlers. Index is the caller-original index from
// originalIndices, NOT the compacted in-service position — so the HTTP
// handler can surface it directly in the response's errors[] field.
type IngestPerEventRejection struct {
	Index   int
	Code    string
	Message string
}

// JobInsertTxer is the narrow surface for transactional River inserts.
// Concrete is *river.Client[pgx.Tx]. Interface keeps the service
// testable.
type JobInsertTxer interface {
	InsertTx(ctx context.Context, tx pgx.Tx, args river.JobArgs, opts *river.InsertOpts) (*rivertype.JobInsertResult, error)
}

// IdentityMatcher is the narrow surface for the per-event identity
// match call. Concrete is *IdentityService.
type IdentityMatcher interface {
	MatchOrCreateTx(ctx context.Context, tx pgx.Tx, req MatchRequest) (*MatchResult, error)
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

// HostLivenessChecker is the narrow surface IngestService needs to
// acquire the row-level write lock on the authed host's mac_host row
// inside the batch tx. Concrete is *repository.MacHostRepository. The
// SELECT ... FOR UPDATE the method runs blocks any concurrent revoke
// of the same row until the batch tx commits or rolls back. Returns
// db.ErrNotFound when the host has been revoked.
type HostLivenessChecker interface {
	GetActiveHostByIDForUpdateTx(ctx context.Context, tx pgx.Tx, id uuid.UUID) (*repository.MacHost, error)
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
func NewIngestService(
	database *db.Database,
	bus *events.Bus,
	identityMatcher IdentityMatcher,
	messages MessagesUpserter,
	riverClient JobInsertTxer,
	externalContacts ExternalContactWriter,
	hostLiveness HostLivenessChecker,
) *IngestService {
	return &IngestService{
		database:         database,
		bus:              bus,
		identity:         identityMatcher,
		messages:         messages,
		riverClient:      riverClient,
		externalContacts: externalContacts,
		hostLiveness:     hostLiveness,
	}
}

// hostAuthAllowedKinds is the single source of truth for which event
// kinds may be submitted via the host-auth path. Daemon-emitted kinds
// extend the set as they land (currently raw_message.* for Messages
// staging + external_contact.* for the iCloud Contacts watcher).
//
// Symmetric: kinds in this set are REJECTED on the global-API-key
// path. These kinds have exactly one producer — the Mac daemon — so
// an internal Pi publisher writing one would either be a bug or a
// hostile actor with the global key.
var hostAuthAllowedKinds = map[events.Kind]struct{}{
	events.KindRawMessageReceived:      {},
	events.KindRawMessageSent:          {},
	events.KindExternalContactUpserted: {},
	events.KindExternalContactDeleted:  {},
}

// isHostOnlyKind reports whether the kind is in the host-auth
// allowlist (i.e., daemon-emitted only). Used on BOTH sides of the
// auth dispatch:
//   - on the host-auth path, kinds NOT in this set are rejected as
//     "host-auth doesn't accept this kind"
//   - on the global-API-key path, kinds IN this set are rejected as
//     "this kind requires host-auth"
func isHostOnlyKind(k events.Kind) bool {
	_, ok := hostAuthAllowedKinds[k]
	return ok
}

// isRawMessageKind reports whether the kind is one of the
// raw_message.* daemon-emitted events. Used by the dispatch loop to
// pick between handleRawMessage and the external_contact handlers.
func isRawMessageKind(k events.Kind) bool {
	return k == events.KindRawMessageReceived || k == events.KindRawMessageSent
}

// isExternalContactKind reports whether the kind is one of the
// external_contact.* daemon-emitted events.
func isExternalContactKind(k events.Kind) bool {
	return k == events.KindExternalContactUpserted || k == events.KindExternalContactDeleted
}

// pendingAggregateKey is the (source, contactID) pair used to dedupe
// aggregator enqueues within a single batch.
type pendingAggregateKey struct {
	Source    string
	ContactID uuid.UUID
}

// IngestBatch persists envs in one pgx transaction. Returns
// (accepted, duplicate, perEventRejections, err):
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
//   - err: any unexpected DB/infrastructure failure (begin-tx, publish-
//     tx, savepoint commit, end-of-batch aggregator enqueue, outer
//     commit). The whole tx rolls back in this case; counts are zero
//     and perEventRejections is nil on error return.
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
) (accepted, duplicate int, perEventRejections []IngestPerEventRejection, err error) {
	if len(envs) == 0 {
		return 0, 0, nil, nil
	}
	if len(originalIndices) != len(envs) {
		return 0, 0, nil, fmt.Errorf(
			"ingest: originalIndices length %d does not match envs length %d",
			len(originalIndices), len(envs))
	}

	for i, env := range envs {
		if env == nil {
			return 0, 0, nil, fmt.Errorf("ingest: envelope at index %d is nil", i)
		}
		if env.ID != uuid.Nil {
			return 0, 0, nil, fmt.Errorf(
				"ingest: envelope at index %d has a pre-assigned ID; IngestBatch "+
					"requires ID=uuid.Nil so the duplicate sentinel works", i)
		}
	}

	tx, err := s.database.Pool.Begin(ctx)
	if err != nil {
		return 0, 0, nil, fmt.Errorf("begin tx: %w", err)
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
				return 0, 0, nil, ErrHostRevokedDuringBatch
			}
			return 0, 0, nil, fmt.Errorf("re-validate host liveness: %w", livenessErr)
		}
	}

	pendingAggregate := make(map[pendingAggregateKey]struct{})

	for i, env := range envs {
		originalIdx := originalIndices[i]

		// Step 1 — host-auth allowlist enforcement.
		//   * Host-only kinds (raw_message.*, external_contact.*) are
		//     allowed ONLY on the host-auth path (hostID != nil).
		//   * Other kinds (Pi internal publishers) are allowed ONLY on
		//     the global-key path (hostID == nil).
		if hostID != nil {
			if !isHostOnlyKind(env.Kind) {
				perEventRejections = append(perEventRejections, IngestPerEventRejection{
					Index:   originalIdx,
					Code:    ingestRejectUnsupportedHostAuthKind,
					Message: fmt.Sprintf("kind %q not allowed on host-auth path; daemon only emits raw_message.* and external_contact.*", env.Kind),
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
		// envelope/payload pair.
		if isRawMessageKind(env.Kind) {
			if rejection := verifyRawMessageInvariants(env, *hostID); rejection != nil {
				rejection.Index = originalIdx
				perEventRejections = append(perEventRejections, *rejection)
				continue
			}
		} else if isExternalContactKind(env.Kind) {
			if rejection := verifyExternalContactInvariants(env, *hostID); rejection != nil {
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
			return 0, 0, nil, fmt.Errorf("begin savepoint for index %d (original %d): %w", i, originalIdx, spErr)
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
				return 0, 0, nil, fmt.Errorf("publish event index %d (original %d): %w (rollback: %v)", i, originalIdx, pubErr, rbErr)
			}
			return 0, 0, nil, fmt.Errorf("publish event index %d (original %d): %w", i, originalIdx, pubErr)
		}

		isDuplicate := env.ID == uuid.Nil

		// Step 5 — inline handler for daemon-emitted kinds. On
		// duplicate (Step 4 detected a dedup hit) we SKIP the inline
		// handler entirely: re-running identity-match + re-upserting
		// staging on every duplicate would spuriously bump
		// external_identity.last_seen_at and re-add the row to
		// pendingAggregate (raw_message), or re-revive a row whose
		// content hash hasn't actually changed (external_contact).
		// The guard makes a duplicate re-submit a true no-op.
		if !isDuplicate {
			switch {
			case isRawMessageKind(env.Kind):
				contactID, rejection := s.handleRawMessage(ctx, sp, env, *hostID)
				if rejection != nil {
					rejection.Index = originalIdx
					perEventRejections = append(perEventRejections, *rejection)
					if rbErr := sp.Rollback(ctx); rbErr != nil {
						return 0, 0, nil, fmt.Errorf("rollback savepoint for rejected index %d (original %d): %w", i, originalIdx, rbErr)
					}
					continue
				}
				if contactID != nil {
					pendingAggregate[pendingAggregateKey{Source: env.Source, ContactID: *contactID}] = struct{}{}
				}
			case env.Kind == events.KindExternalContactUpserted:
				if rejection := s.handleExternalContactUpserted(ctx, sp, env, *hostID); rejection != nil {
					rejection.Index = originalIdx
					perEventRejections = append(perEventRejections, *rejection)
					if rbErr := sp.Rollback(ctx); rbErr != nil {
						return 0, 0, nil, fmt.Errorf("rollback savepoint for rejected index %d (original %d): %w", i, originalIdx, rbErr)
					}
					continue
				}
			case env.Kind == events.KindExternalContactDeleted:
				if rejection := s.handleExternalContactDeleted(ctx, sp, env, *hostID); rejection != nil {
					rejection.Index = originalIdx
					perEventRejections = append(perEventRejections, *rejection)
					if rbErr := sp.Rollback(ctx); rbErr != nil {
						return 0, 0, nil, fmt.Errorf("rollback savepoint for rejected index %d (original %d): %w", i, originalIdx, rbErr)
					}
					continue
				}
			}
		}

		if commitErr := sp.Commit(ctx); commitErr != nil {
			return 0, 0, nil, fmt.Errorf("commit savepoint %d (original %d): %w", i, originalIdx, commitErr)
		}

		if isDuplicate {
			duplicate++
		} else {
			accepted++
		}
	}

	// Step 6 — end-of-batch aggregator-enqueue. Each enqueue is in the
	// OUTER tx (not a savepoint) — a failure here rolls the whole
	// batch back (R4 rationale: partial-enqueue stranding is worse than
	// a daemon retry).
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
			return 0, 0, nil, fmt.Errorf("enqueue messaging aggregate (contact=%s source=%s): %w",
				pair.ContactID, pair.Source, insErr)
		}
	}

	if commitErr := tx.Commit(ctx); commitErr != nil {
		return 0, 0, nil, fmt.Errorf("commit: %w", commitErr)
	}
	return accepted, duplicate, perEventRejections, nil
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
	matchResult, err := s.identity.MatchOrCreateTx(ctx, tx, matchReq)
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
	if env.Source != "messages" {
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
//  3. env.Source is in allowedExternalContactSources.
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
	if _, ok := allowedExternalContactSources[env.Source]; !ok {
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
// Returns nil on success; rejection != nil rolls back the savepoint.
func (s *IngestService) handleExternalContactUpserted(
	ctx context.Context,
	tx pgx.Tx,
	env *events.Envelope,
	hostID uuid.UUID,
) *IngestPerEventRejection {
	if s.identity == nil || s.externalContacts == nil {
		return &IngestPerEventRejection{
			Code:    ingestRejectPayloadInvariant,
			Message: "ingest service was not configured for external_contact processing",
		}
	}

	var p events.ExternalContactUpsertedPayload
	if err := json.Unmarshal(env.Payload, &p); err != nil {
		return &IngestPerEventRejection{
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
		return &IngestPerEventRejection{
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
		return &IngestPerEventRejection{
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
			return &IngestPerEventRejection{
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
	for _, em := range p.Emails {
		if em.Value == "" {
			continue
		}
		result, err := s.identity.MatchOrCreateTx(ctx, tx, MatchRequest{
			RawIdentifier: em.Value,
			Type:          identity.IdentifierTypeEmail,
			Source:        env.Source,
			DisplayName:   p.DisplayName,
		})
		if err != nil {
			return &IngestPerEventRejection{
				Code:    ingestRejectIdentityMatchFailed,
				Message: fmt.Sprintf("identity match (email): %s", err.Error()),
			}
		}
		if canSetMatchOnRow && !matched && result != nil && result.ContactID != nil {
			if _, err := s.externalContacts.UpdateMatchTx(ctx, tx,
				external.ID, result.ContactID, repository.MatchStatusMatched); err != nil {
				return &IngestPerEventRejection{
					Code:    ingestRejectExternalContactUpdateMatchFailed,
					Message: fmt.Sprintf("update external_contact match (email): %s", err.Error()),
				}
			}
			matched = true
		}
	}
	for _, ph := range p.Phones {
		if ph.Value == "" {
			continue
		}
		result, err := s.identity.MatchOrCreateTx(ctx, tx, MatchRequest{
			RawIdentifier: ph.Value,
			Type:          identity.IdentifierTypePhone,
			Source:        env.Source,
			DisplayName:   p.DisplayName,
		})
		if err != nil {
			return &IngestPerEventRejection{
				Code:    ingestRejectIdentityMatchFailed,
				Message: fmt.Sprintf("identity match (phone): %s", err.Error()),
			}
		}
		if canSetMatchOnRow && !matched && result != nil && result.ContactID != nil {
			if _, err := s.externalContacts.UpdateMatchTx(ctx, tx,
				external.ID, result.ContactID, repository.MatchStatusMatched); err != nil {
				return &IngestPerEventRejection{
					Code:    ingestRejectExternalContactUpdateMatchFailed,
					Message: fmt.Sprintf("update external_contact match (phone): %s", err.Error()),
				}
			}
			matched = true
		}
	}
	return nil
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
	// NULL prior.HostID = legacy row (pre-migration 052 or non-mac source)
	// → pass through so the self-heal-on-upsert path keeps working.
	// Silent no-op (not a rejection) so a stranded post-re-pair delete in
	// the daemon's event queue doesn't stall the cursor.
	if prior.HostID != nil && *prior.HostID != authenticatedHostID {
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
