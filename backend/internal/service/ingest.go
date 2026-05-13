// Package service provides business-logic wrappers around repositories.
// IngestService owns the single-tx batch-publish semantics for the HTTP
// ingestion endpoint introduced in PR 4 of the event-bus-foundation spec.
package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"personal-crm/backend/internal/consumer/consumerjobs"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/events"
	"personal-crm/backend/internal/identity"
	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
)

// Per-event rejection codes surfaced through perEventRejections so the
// HTTP handler can translate them into the response's errors[] array.
const (
	ingestRejectUnsupportedHostAuthKind = "UNSUPPORTED_HOST_AUTH_KIND"
	ingestRejectRawMessageRequiresHost  = "RAW_MESSAGE_REQUIRES_HOST_AUTH"
	ingestRejectPayloadInvariant        = "PAYLOAD_INVARIANT"
	ingestRejectPayloadInvalid          = "PAYLOAD_INVALID"
	ingestRejectIdentityMatchFailed     = "IDENTITY_MATCH_FAILED"
	ingestRejectStagingUpsertFailed     = "STAGING_UPSERT_FAILED"
)

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
	database    *db.Database
	bus         *events.Bus
	identity    IdentityMatcher
	messages    MessagesUpserter
	riverClient JobInsertTxer
}

// NewIngestService builds an IngestService. The non-raw-message
// dependencies (identity, messages, riverClient) may be nil — in that
// case any raw_message.* envelope is per-event REJECTED with
// PAYLOAD_INVARIANT explaining the missing wiring. Production
// constructs all four; unit tests can pass nils for the irrelevant ones.
func NewIngestService(
	database *db.Database,
	bus *events.Bus,
	identityMatcher IdentityMatcher,
	messages MessagesUpserter,
	riverClient JobInsertTxer,
) *IngestService {
	return &IngestService{
		database:    database,
		bus:         bus,
		identity:    identityMatcher,
		messages:    messages,
		riverClient: riverClient,
	}
}

// hostAuthAllowedKinds is the single source of truth for which event
// kinds may be submitted via the host-auth path. Currently only the
// daemon-emitted raw_message.* kinds are allowed; new daemon-emitted
// kinds extend the set as they land.
//
// Symmetric: kinds in this set are REJECTED on the global-API-key
// path. Daemon-emitted raw_message.* is the ONLY producer path;
// an internal Pi publisher writing a raw_message would either be a
// bug or a hostile actor with the global key.
var hostAuthAllowedKinds = map[events.Kind]struct{}{
	events.KindRawMessageReceived: {},
	events.KindRawMessageSent:     {},
}

// isHostAuthAllowedKind reports whether the kind is permitted on the
// host-auth path.
func isHostAuthAllowedKind(k events.Kind) bool {
	_, ok := hostAuthAllowedKinds[k]
	return ok
}

// isRawMessageKind reports whether the kind is a daemon-emitted
// raw_message.* event. Used for the symmetric global-key rejection.
func isRawMessageKind(k events.Kind) bool {
	return k == events.KindRawMessageReceived || k == events.KindRawMessageSent
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

	pendingAggregate := make(map[pendingAggregateKey]struct{})

	for i, env := range envs {
		originalIdx := originalIndices[i]

		// Step 1 — host-auth allowlist enforcement.
		//   * raw_message.* allowed ONLY on the host-auth path (hostID != nil).
		//   * other kinds allowed ONLY on the global-key path (hostID == nil).
		if hostID != nil {
			if !isHostAuthAllowedKind(env.Kind) {
				perEventRejections = append(perEventRejections, IngestPerEventRejection{
					Index:   originalIdx,
					Code:    ingestRejectUnsupportedHostAuthKind,
					Message: fmt.Sprintf("kind %q not allowed on host-auth path; daemon only emits raw_message.*", env.Kind),
				})
				continue
			}
		} else if isRawMessageKind(env.Kind) {
			perEventRejections = append(perEventRejections, IngestPerEventRejection{
				Index:   originalIdx,
				Code:    ingestRejectRawMessageRequiresHost,
				Message: "raw_message.* events require X-Mac-Host-ID auth path",
			})
			continue
		}

		// Step 2 — payload-invariant cross-checks (raw_message only).
		// Done BEFORE opening the savepoint so a clean failure doesn't
		// even open one. The handler's pre-validation already enforced
		// required-field presence; we cross-check the envelope/payload
		// pair.
		if isRawMessageKind(env.Kind) {
			if rejection := verifyRawMessageInvariants(env, *hostID); rejection != nil {
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

		// Step 5 — inline handler for raw_message kinds. On duplicate
		// (Step 4 detected a dedup hit) we SKIP the inline handler
		// entirely: re-running identity-match + re-upserting staging on
		// every duplicate would spuriously bump
		// external_identity.last_seen_at and re-add the row to
		// pendingAggregate. Both are wasteful; the guard makes a
		// duplicate re-submit a true no-op.
		if !isDuplicate && isRawMessageKind(env.Kind) {
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
			UniqueOpts: river.UniqueOpts{ByArgs: true},
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
