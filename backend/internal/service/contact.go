package service

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/cadence"
	"personal-crm/backend/internal/consumer/consumerjobs"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/events"
	"personal-crm/backend/internal/logger"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/todoist"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
)

type ContactMethodInput struct {
	Type      string
	Value     string
	IsPrimary bool
}

type OverdueContact struct {
	Contact         repository.Contact
	DaysOverdue     int
	NextDueDate     time.Time
	SuggestedAction string
}

// FollowUpConsumer is the subset of *consumer.FollowUpManager
// ContactService depends on post-cutover. ApplyInteraction is the
// direct-invoke path for non-bus callers (Todoist completion wrapper,
// Promote/ExtendInteraction). All remote effects leave via
// todoist_task_op jobs enqueued in the caller's tx, so no post-commit
// closure is returned.
//
// Interface lives here (not on the consumer) so tests can stub
// follow-up behavior without building the full consumer graph.
type FollowUpConsumer interface {
	ApplyInteraction(ctx context.Context, tx pgx.Tx, req repository.ApplyInteractionRequest) error
}

// cadenceWriter abstracts the direct-invoke write surface of
// *consumer.CadenceUpdater that ContactService needs post-cutover.
// Defined as a narrow interface so tests can stub it without importing
// the consumer package graph, and so the service layer's dependency
// stays unambiguous.
type cadenceWriter interface {
	// ApplyInteraction applies the cadence side-effects of
	// ExtendInteraction + PromoteInteractionToMutual (direction-conditional,
	// forward-only for non-manual, unconditional for manual).
	ApplyInteraction(ctx context.Context, tx pgx.Tx, req repository.ApplyInteractionRequest) error
	// BulkApply applies per-field forward-max cadence writes for merge.
	BulkApply(ctx context.Context, tx pgx.Tx, contactID uuid.UUID, fields repository.ContactCadenceFields) error
	// ApplyContactByOverride applies a user-driven cadence edit. Takes
	// the unconditional branch so cadence clears or backdates work.
	ApplyContactByOverride(ctx context.Context, tx pgx.Tx, contactID uuid.UUID, contactBy *time.Time) error
}

type ContactService struct {
	database          *db.Database
	contactRepo       *repository.ContactRepository
	contactMethodRepo *repository.ContactMethodRepository
	interactionRepo   *repository.InteractionRepository
	contactTaskRepo   *repository.ContactTaskRepository
	// followUp is the FollowUpManager consumer — the sole writer of
	// contact_task.kind='follow_up' lifecycle post-cutover. Passed to
	// NewContactService. Nil-tolerant: when nil, applyFollowUpInlineTx
	// no-ops and non-bus follow-up work is silently skipped. Non-bus
	// callers (Todoist completion path, Promote/Extend) route through
	// followUp.ApplyInteraction inline in the caller's tx via
	// applyFollowUpInlineTx.
	followUp FollowUpConsumer
	// bus is the event bus used to publish contact_methods.added on
	// method-adding mutations. Required in production wiring (main.go).
	// Nil-safe for tests that don't exercise the rematch dispatch path —
	// the publisher short-circuits when nil and leaves rematchJobID as
	// uuid.Nil in the response.
	bus *events.Bus
	// rematchRegistry is the narrow contract for registering an in-memory
	// job entry synchronously so GET /rematch/jobs/:id works immediately
	// after publish. Implemented by *RematchService. Required alongside
	// bus in production; nil-safe for tests.
	rematchRegistry RematchRegistry
	// cadence is the direct-invoke writer (the sole writer of the four
	// cadence columns post-cutover). Passed to NewContactService.
	// When nil, MergeContacts / ExtendInteraction /
	// PromoteInteractionToMutual / cadence-edit UpdateContact paths
	// return an error rather than silently no-op-ing on a code path
	// that was supposed to mutate cadence state.
	cadence cadenceWriter
	// knowledge emits the lives_in/birthday/how_met assertions for
	// location/birthday/how_met and refreshes the derived cache columns
	// inline (the authority flip — the assertion store is the source of
	// truth, the columns are a cache). Built by NewContactService from the
	// (assertSvc, knowledgeCache) pair when both are provided; left nil
	// when neither is (a half-set pair is a constructor-time panic). When
	// nil, CreateContact / UpdateContact / MergeContacts return an error
	// rather than silently dropping a location/birthday/how_met write (the
	// columns are no longer written by the contact SQL).
	knowledge *knowledgeWriter
	// commsRepo carries the savepoint-wrapped comms_message merge repoint
	// (dedup soft-delete + repoint with one scoped retry). Constructed
	// internally from database.Queries — no new constructor arg.
	commsRepo *repository.CommsMessageRepository
	// taskCloseEnqueuer inserts the durable Todoist close job for automated
	// contact_task rows the merge closes with a REAL external id. Injected
	// via SetTaskCloseEnqueuer after construction (a cross-block dependency:
	// the river client is decided later). taskCloseConfigured tracks "setter called"
	// separately from taskCloseRemoteEnabled: setter never called + eligible
	// refs exist → error (wiring bug); called with remoteEnabled=false
	// (follow-up mode 'off', the documented completion-disabled emergency
	// override) → WARN + skip enqueue; enabled → enqueue.
	taskCloseEnqueuer      repository.JobEnqueuer
	taskCloseConfigured    bool
	taskCloseRemoteEnabled bool
	// mergeCommitBarrier, when non-nil, is invoked after the in-tx
	// attribution repoints and immediately before the merge tx commit.
	// Test-only hook (InjectMergeCommitBarrierForTest) that makes the
	// concurrent-ingest race deterministic: a test commits a raced
	// source-attributed row from a second connection inside the hook, and
	// the post-commit second pass must repoint it. Nil in production.
	mergeCommitBarrier func()
}

// NewContactService constructs a ContactService. bus and rematchRegistry
// are required for the event-bus rematch path in production:
// CreateContact / UpdateContact / RescanRematch publish
// contact_methods.added through bus and seed the in-memory job entry via
// rematchRegistry. Tests that don't exercise rematch may pass nil for
// both — the publisher silently skips when bus is nil.
//
// The promoted consumer dependencies (cadence, assertSvc+knowledgeCache,
// followUp) carry the nil semantics their former setters had:
//   - cadence: nil ⇒ Merge/Extend/Promote/cadence-edit paths error (they
//     must mutate cadence state; a nil writer is a wiring bug on those paths).
//   - assertSvc + knowledgeCache: both-or-neither. Both non-nil ⇒ the
//     knowledge writer is built; both nil ⇒ knowledge stays nil and the
//     create/update/merge knowledge guards error. A HALF-set pair is a
//     wiring bug and panics at construction (the writer needs both).
//   - followUp: nil-tolerant ⇒ non-bus follow-up work is silently skipped.
func NewContactService(
	database *db.Database,
	contactRepo *repository.ContactRepository,
	contactMethodRepo *repository.ContactMethodRepository,
	interactionRepo *repository.InteractionRepository,
	contactTaskRepo *repository.ContactTaskRepository,
	bus *events.Bus,
	rematchRegistry RematchRegistry,
	cadence cadenceWriter,
	assertSvc *AssertService,
	knowledgeCache knowledgeCacheRefresher,
	followUp FollowUpConsumer,
) *ContactService {
	return &ContactService{
		database:          database,
		contactRepo:       contactRepo,
		contactMethodRepo: contactMethodRepo,
		interactionRepo:   interactionRepo,
		contactTaskRepo:   contactTaskRepo,
		commsRepo:         repository.NewCommsMessageRepository(database.Queries),
		bus:               bus,
		rematchRegistry:   rematchRegistry,
		cadence:           cadence,
		knowledge:         buildKnowledgeWriter(assertSvc, knowledgeCache),
		followUp:          followUp,
	}
}

// buildKnowledgeWriter enforces the both-or-neither knowledge-pair rule shared
// by NewContactService and NewEnrichmentService: both non-nil builds the
// writer, both nil leaves it nil (the "not wired" guards fire), a half-set pair
// panics loudly at construction (a wiring bug — the writer cannot persist
// location/birthday/how_met without both the assert service and the cache).
func buildKnowledgeWriter(assertSvc *AssertService, knowledgeCache knowledgeCacheRefresher) *knowledgeWriter {
	switch {
	case assertSvc != nil && knowledgeCache != nil:
		return newKnowledgeWriter(assertSvc, knowledgeCache)
	case assertSvc == nil && knowledgeCache == nil:
		return nil
	default:
		panic("service: knowledge writer requires both assertSvc and knowledgeCache (or neither)")
	}
}

// SetTaskCloseEnqueuer injects the river job enqueuer MergeContacts uses to
// schedule remote Todoist closes for the source contact's automated tasks.
// remoteCloseEnabled reflects the follow-up mode gate (cutover only): the
// close worker refuses to run outside cutover, so enqueuing in mode 'off'
// would only manufacture failing jobs — MergeContacts skips the enqueue with
// a WARN instead (that mode's documented contract is "completion disabled").
// Calling the setter at all marks the dependency as configured; a merge that
// closes an enqueue-eligible task without the setter ever being called
// errors, surfacing the wiring bug instead of stranding a remote task.
func (s *ContactService) SetTaskCloseEnqueuer(e repository.JobEnqueuer, remoteCloseEnabled bool) {
	s.taskCloseEnqueuer = e
	s.taskCloseConfigured = true
	s.taskCloseRemoteEnabled = remoteCloseEnabled
}

// InjectMergeCommitBarrierForTest installs the test-only pre-commit barrier
// for MergeContacts (see the mergeCommitBarrier field). Must only be called
// before the service is used concurrently; no synchronization is performed.
func (s *ContactService) InjectMergeCommitBarrierForTest(fn func()) {
	s.mergeCommitBarrier = fn
}

// InjectBusForTest swaps the event bus reference after construction.
// Integration tests have a chicken-and-egg dependency where the bus
// needs the ContactService (via InteractionRecorder) AND the
// ContactService needs the bus (to publish contact_methods.added).
// Production main.go resolves this by reordering construction — tests
// can't do that cleanly across many fixtures, so this setter exists
// for test-only bus injection. Must only be called before the
// service is used concurrently; no synchronization is performed.
func (s *ContactService) InjectBusForTest(bus *events.Bus) {
	s.bus = bus
}

// HasPendingFollowUp checks if a contact has a pending follow-up task
func (s *ContactService) HasPendingFollowUp(ctx context.Context, contactID uuid.UUID) (bool, error) {
	_, err := s.contactTaskRepo.FindPendingFollowUp(ctx, contactID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func (s *ContactService) GetContact(ctx context.Context, id uuid.UUID) (*repository.Contact, error) {
	contact, err := s.contactRepo.GetContact(ctx, id)
	if err != nil {
		return nil, err
	}

	if err := s.attachMethods(ctx, contact); err != nil {
		return nil, err
	}

	return contact, nil
}

// ListContactsPage returns one page of contacts (optionally searched via
// params.Query) plus the total count for the same filters.
func (s *ContactService) ListContactsPage(ctx context.Context, params repository.ListContactsParams) ([]repository.Contact, int64, error) {
	contacts, err := s.contactRepo.ListContacts(ctx, params)
	if err != nil {
		return nil, 0, err
	}

	if err := s.attachMethodsToContacts(ctx, contacts); err != nil {
		return nil, 0, err
	}

	total, err := s.contactRepo.CountContacts(ctx, params)
	if err != nil {
		return nil, 0, err
	}

	return contacts, total, nil
}

// ListContactIDs retrieves a list of contact IDs with optional sorting and search.
// This is a lightweight method for navigation purposes (e.g., keyboard navigation between contacts).
func (s *ContactService) ListContactIDs(ctx context.Context, params repository.ListContactIDsParams) ([]uuid.UUID, error) {
	return s.contactRepo.ListContactIDs(ctx, params)
}

func (s *ContactService) CreateContact(ctx context.Context, req repository.CreateContactRequest, methods []ContactMethodInput) (contact *repository.Contact, jobID uuid.UUID, err error) {
	if s.knowledge == nil {
		// Authority-flip invariant: location/birthday/how_met are no longer
		// written by the contact SQL — they flow from the assertion store via
		// the knowledge writer. Refusing to operate without it prevents a
		// silent drop of those fields.
		return nil, uuid.Nil, errors.New("create contact: knowledge writer not wired (pass assertSvc+knowledgeCache to NewContactService)")
	}
	tx, err := s.database.Pool.Begin(ctx)
	if err != nil {
		return nil, uuid.Nil, err
	}
	defer func() {
		if rollbackErr := tx.Rollback(ctx); rollbackErr != nil && !errors.Is(rollbackErr, pgx.ErrTxClosed) {
			if err == nil {
				err = rollbackErr
			}
		}
	}()

	txQueries := db.New(tx)
	contactRepo := repository.NewContactRepository(txQueries)
	contactMethodRepo := repository.NewContactMethodRepository(txQueries)

	// ContactRepository.CreateContact inserts the person node and the contact
	// in one statement (node.id == contact.id), so the pair commits or rolls
	// back together in this tx with no separate node insert here.
	contact, err = contactRepo.CreateContact(ctx, req)
	if err != nil {
		return nil, uuid.Nil, err
	}

	// Authority flip: persist location/birthday/how_met as user assertions in
	// the same tx (the contact SQL no longer writes those columns) and refresh
	// the derived cache columns inline so the returned contact reflects them on
	// commit. A live user create stamps knowledge_from = now (no override).
	if err = s.knowledge.assertCreate(ctx, tx, contact.ID,
		knowledgeFieldValues{Location: req.Location, Birthday: req.Birthday, HowMet: req.HowMet},
		knowledgeFieldProvenance{
			SourceKind:     repository.SourceKindUser,
			ProducerKind:   repository.ProducerKindUser,
			SourceIDPrefix: "edit",
		}); err != nil {
		return nil, uuid.Nil, fmt.Errorf("assert contact knowledge: %w", err)
	}

	// The cache refresh above is a second write on the contact row, so the
	// struct returned by CreateContact (which never carried location/birthday/
	// how_met) is stale for those columns. Re-fetch inside the tx so the caller
	// receives the committed values.
	contact, err = contactRepo.GetContact(ctx, contact.ID)
	if err != nil {
		return nil, uuid.Nil, fmt.Errorf("refetch contact after knowledge assert: %w", err)
	}

	createdMethods, err := createContactMethods(ctx, contactMethodRepo, contact.ID, methods)
	if err != nil {
		return nil, uuid.Nil, err
	}

	// Publish contact_methods.added inside the tx so event row + river
	// consumer job land atomically with the method rows (spec §4). A tx
	// rollback rolls back all three. jobID returns only after Commit,
	// so clients never see a jobID for work that didn't ship.
	//
	// Filter to eligible methods first: if no registered rematch handler
	// matches any new method, we skip publishing entirely and return
	// uuid.Nil — matches the pre-cutover StartRematchForContact contract
	// where unhandled methods produced no job id.
	var eligibleMethods []Method
	if len(createdMethods) > 0 && s.rematchRegistry != nil {
		eligibleMethods = s.rematchRegistry.EligibleMethods(toRematchMethods(createdMethods))
	}
	if len(eligibleMethods) > 0 && s.bus != nil {
		jobID = uuid.New()
		env, marshalErr := buildContactMethodsAddedEnvelope("manual", contact.ID, rematchMethodsToRefs(eligibleMethods), jobID)
		if marshalErr != nil {
			return nil, uuid.Nil, marshalErr
		}
		if err := s.bus.PublishTx(ctx, tx, env); err != nil {
			return nil, uuid.Nil, fmt.Errorf("publish contact_methods.added: %w", err)
		}
	}

	if err = tx.Commit(ctx); err != nil {
		return nil, uuid.Nil, err
	}

	assignMethods(contact, createdMethods)
	// RegisterPending seeds the in-memory entry so GET
	// /rematch/jobs/:id returns it immediately. Idempotent; safe even
	// though the bus may have deduped a publisher retry.
	if jobID != uuid.Nil && s.rematchRegistry != nil {
		s.rematchRegistry.RegisterPending(jobID, contact.ID, eligibleMethods)
	}
	return contact, jobID, nil
}

// UpdateContact updates a contact's scalar profile fields.
//
// It does NOT touch contact methods. The method-replacing branch this function
// used to carry made absence mean "delete", so a client saving from a stale
// read destroyed every method it had never seen. Methods are mutated through
// ContactMethodService.ApplyOperations, which takes operations and therefore
// cannot express a removal the caller did not name.
func (s *ContactService) UpdateContact(ctx context.Context, id uuid.UUID, req repository.UpdateContactRequest) (contact *repository.Contact, err error) {
	if s.cadence == nil {
		// Sole-writer invariant: contact_by recomputation on cadence
		// edits must route through CadenceUpdater. Refusing to operate
		// without it prevents a silent rollback to the direct-path
		// behavior.
		return nil, errors.New("update contact: cadence updater not wired (pass cadence to NewContactService)")
	}
	if s.knowledge == nil {
		// Authority-flip invariant: location/birthday/how_met flow from the
		// assertion store, not the contact SQL.
		return nil, errors.New("update contact: knowledge writer not wired (pass assertSvc+knowledgeCache to NewContactService)")
	}
	tx, err := s.database.Pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() {
		if rollbackErr := tx.Rollback(ctx); rollbackErr != nil && !errors.Is(rollbackErr, pgx.ErrTxClosed) {
			if err == nil {
				err = rollbackErr
			}
		}
	}()

	txQueries := db.New(tx)
	contactRepo := repository.NewContactRepository(txQueries)
	contactMethodRepo := repository.NewContactMethodRepository(txQueries)
	nodeRepo := repository.NewNodeRepository(txQueries)

	existingContact, err := contactRepo.GetContact(ctx, id)
	if err != nil {
		return nil, err
	}

	// Contact_by recomputation is a user-cadence-preference edit and
	// must route through CadenceUpdater. The profile-only UpdateContact
	// query below writes the new cadence string; contact_by is then
	// recomputed from (new cadence, existing last_contacted ||
	// created_at) and applied via ApplyContactByOverride in the same
	// tx. The unconditional branch inside ApplyContactByOverride
	// accepts backdate/clear semantics, which is required for "user
	// switched from weekly to annual" or "user removed cadence
	// entirely".
	var newContactBy *time.Time
	if req.Cadence != nil && *req.Cadence != "" {
		if cadenceType, parseErr := cadence.ParseCadence(*req.Cadence); parseErr == nil {
			base := existingContact.CreatedAt
			if existingContact.LastContacted != nil {
				base = *existingContact.LastContacted
			}
			t := cadence.CalculateContactBy(base, cadenceType)
			newContactBy = &t
		}
	}
	// The DTO's ContactBy field is ignored post-cutover — contact_by is
	// always recomputed from the cadence string to preserve the
	// invariant "cadence drives contact_by". Zero it explicitly on the
	// DTO so a future profile-only query extension can't accidentally
	// forward a stale value.
	req.ContactBy = nil

	if _, err = contactRepo.UpdateContact(ctx, id, req); err != nil {
		return nil, err
	}

	// Keep the person node's display label synced with the contact's name.
	// UpdateContact always writes full_name in this same tx, so we sync the
	// node to req.FullName UNCONDITIONALLY — gating on a pre-read name compare
	// would let a stale-read non-rename update leave node and contact divergent
	// under concurrency. A redundant no-op UPDATE on a non-rename edit is cheap.
	if err = nodeRepo.UpdateNodeCanonicalLabelTx(ctx, tx, id, req.FullName); err != nil {
		return nil, fmt.Errorf("sync person node label: %w", err)
	}

	// Authority flip: reconcile location/birthday/how_met against the contact's
	// pre-update cache values — assert a new/changed value, close a cleared
	// slot — then refresh the cache columns inline. existingContact's cache
	// columns reflect the current-accepted assertions, so the next/existing diff
	// drives the supersession-vs-closure decision.
	if err = s.knowledge.applyUpdate(ctx, tx, id,
		knowledgeFieldValues{Location: req.Location, Birthday: req.Birthday, HowMet: req.HowMet},
		knowledgeFieldValues{Location: existingContact.Location, Birthday: existingContact.Birthday, HowMet: existingContact.HowMet},
		knowledgeFieldProvenance{
			SourceKind:     repository.SourceKindUser,
			ProducerKind:   repository.ProducerKindUser,
			SourceIDPrefix: "edit",
		}); err != nil {
		return nil, fmt.Errorf("apply contact knowledge: %w", err)
	}

	if err := s.cadence.ApplyContactByOverride(ctx, tx, id, newContactBy); err != nil {
		return nil, fmt.Errorf("apply cadence contact_by override: %w", err)
	}
	// ApplyContactByOverride is a second UPDATE on the contact row, so
	// the profile-only UpdateContact's RETURNING values (notably
	// updated_at AND contact_by) are stale by the time we'd use them.
	// The knowledge cache refresh above is likewise a separate write on
	// location/birthday/how_met. Re-fetch inside the tx so the struct the
	// caller receives matches the committed row bit-for-bit.
	contact, err = contactRepo.GetContact(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("refetch contact after cadence override: %w", err)
	}

	// Methods are read back only so the returned contact carries them. Nothing
	// here writes a method, so there is no diff to publish and no rematch job to
	// mint: a rematch is triggered by newly-present method values, which now
	// arrive exclusively through ContactMethodService.ApplyOperations.
	updatedMethods, err := contactMethodRepo.ListContactMethodsByContact(ctx, id)
	if err != nil {
		return nil, err
	}
	sortContactMethods(updatedMethods)

	if err = tx.Commit(ctx); err != nil {
		return nil, err
	}

	assignMethods(contact, updatedMethods)
	return contact, nil
}

// DeleteContact soft-deletes a contact and propagates the tombstone to its
// person node (node.id == contact.id), so graph reads — which filter
// node.deleted_at IS NULL — drop the contact's assertions from live results
// while retaining them in the table. The two soft-deletes commit atomically.
func (s *ContactService) DeleteContact(ctx context.Context, id uuid.UUID) (err error) {
	if _, err := s.contactRepo.GetContact(ctx, id); err != nil {
		return err
	}

	tx, err := s.database.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() {
		if rb := tx.Rollback(ctx); rb != nil && !errors.Is(rb, pgx.ErrTxClosed) && err == nil {
			err = rb
		}
	}()

	if err = s.contactRepo.SoftDeleteContactTx(ctx, tx, id); err != nil {
		return fmt.Errorf("soft delete contact: %w", err)
	}
	// node.id == contact.id; soft-delete the person node so its assertions drop
	// from live graph reads. SoftDeleteNodeTx re-derives its querier from tx, so
	// the base-queries repo is fine here. Idempotent if the node is already
	// tombstoned (a merged-away source, say).
	nodeRepo := repository.NewNodeRepository(s.database.Queries)
	if err = nodeRepo.SoftDeleteNodeTx(ctx, tx, id); err != nil {
		return fmt.Errorf("soft delete person node: %w", err)
	}
	return tx.Commit(ctx)
}

// RecordInteraction creates an interaction record and updates contact fields based on direction.
// Handles source-aware deduplication:
//   - For sources with source_ref (gcal, todoist): dedup by source_ref
//   - For manual sources (no source_ref): dedup by 30-minute time window
//
// Direction-conditional updates:
//   - outbound: updates only last_outreach_at (does NOT reset last_contacted/contact_by)
//   - inbound: updates last_contacted, last_interaction_at, last_response_at, contact_by
//   - mutual: updates all fields + contact_by
//
// Follow-up management (best-effort, non-blocking):
//   - outbound: creates or refreshes a follow-up Todoist task
//   - inbound/mutual: completes any pending follow-up task
//
// Non-tx wrapper. Opens a short-lived tx via BeginTxFunc and delegates to
// RecordInteractionTx. Used by the Todoist completion path and internal
// service callers that don't own a tx. The event-bus consumer and
// manual-UI handler use
// RecordInteractionTx directly so they can share the outer tx (spec
// §3.4.1 atomicity contract).
func (s *ContactService) RecordInteraction(ctx context.Context, req repository.RecordInteractionRequest) (*repository.Interaction, error) {
	var res *RecordInteractionResult
	err := pgx.BeginTxFunc(ctx, s.database.Pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		var txErr error
		// Non-event-bus wrapper: no paired interaction.recorded event
		// will be published, so publishesEvent=false. The direct
		// CadenceUpdater.ApplyInteraction path is used instead of the
		// bus-path inline HandleEvent.
		res, txErr = s.RecordInteractionTx(ctx, tx, false, req)
		return txErr
	})
	if err != nil {
		return nil, err
	}
	// Follow-up (and its op enqueue) already ran inside the tx above; no
	// post-commit work remains.
	return res.Interaction, nil
}

// RecordInteractionResult is an alias for repository.RecordInteractionResult.
// The canonical definition lives in repository so the consumer's
// interactionWriter interface can reference it without importing the
// service package. See RecordInteractionTx for the construction contract.
type RecordInteractionResult = repository.RecordInteractionResult

// RecordInteractionTx is the tx-threaded variant of RecordInteraction. The
// caller owns the tx (river worker's BeginTxFunc, manual handler's
// BeginTxFunc). Dedup, contact existence check, interaction insert, and
// cadence UPDATEs all run inside the caller's tx (spec §3.4.1).
//
// The `publishesEvent` flag tells the service whether the caller intends to
// publish an interaction.recorded event after commit and therefore
// needs the pre-cadence snapshot populated on the result (for the V2
// payload). Event-bus consumer callers pass true; the non-tx
// RecordInteraction wrapper passes false and takes the direct-invoke
// cadence path instead.
//
// Returns a *RecordInteractionResult with named fields (prior shape had 7
// positional returns). See the field doc comments for per-field
// nilability contracts. On error the result is nil and the caller
// should roll back the tx.
func (s *ContactService) RecordInteractionTx(
	ctx context.Context, tx pgx.Tx, publishesEvent bool, req repository.RecordInteractionRequest,
) (*RecordInteractionResult, error) {
	// 1. Default direction to "mutual" if empty (backward compat).
	if req.Direction == "" {
		req.Direction = repository.InteractionDirectionMutual
	}

	// 2. Source-aware deduplication (tx-aware variants).
	if req.SourceRef != nil {
		existing, err := s.interactionRepo.FindBySourceRefTx(ctx, tx, req.ContactID, req.Source, *req.SourceRef)
		if err != nil && !errors.Is(err, db.ErrNotFound) {
			return nil, fmt.Errorf("check existing interaction by source_ref: %w", err)
		}
		if existing != nil {
			logger.Debug().
				Str("contactId", req.ContactID.String()).
				Str("source", req.Source).
				Str("sourceRef", *req.SourceRef).
				Msg("skipping duplicate interaction (same source_ref)")
			return &RecordInteractionResult{Interaction: existing, IsReplay: true}, nil
		}
	} else {
		existing, err := s.interactionRepo.FindInWindowTx(ctx, tx, req.ContactID, req.Source, req.Direction, req.OccurredAt, 30*time.Minute)
		if err != nil && !errors.Is(err, db.ErrNotFound) {
			return nil, fmt.Errorf("check existing interaction in window: %w", err)
		}
		if existing != nil {
			logger.Debug().
				Str("contactId", req.ContactID.String()).
				Str("existingSource", existing.Source).
				Str("newSource", req.Source).
				Msg("skipping duplicate interaction within 30-min window")
			return &RecordInteractionResult{Interaction: existing, IsReplay: true}, nil
		}
	}

	// 3. Verify contact exists (avoids FK violation returning unhelpful error).
	contact, err := s.contactRepo.GetContactTx(ctx, tx, req.ContactID)
	if err != nil {
		return nil, err // propagates db.ErrNotFound
	}

	// 4. Capture pre-cadence snapshot + cadence-at-emit from the
	// in-memory contact BEFORE the write. This is the pre-image the
	// interaction.recorded V2 payload carries so CadenceUpdater can
	// replay against a deterministic prev snapshot.
	prevSnap := repository.ContactCadenceFieldsFromContact(contact)
	var cadenceAtEmit *string
	if contact.Cadence != nil && *contact.Cadence != "" {
		c := *contact.Cadence
		cadenceAtEmit = &c
	}

	// 5. Create interaction record in the caller's tx.
	interaction, err := s.interactionRepo.CreateInteractionTx(ctx, tx, repository.CreateInteractionRequest(req))
	if err != nil {
		return nil, fmt.Errorf("create interaction: %w", err)
	}

	// 6. Direct-path cadence write. When publishesEvent=false the caller is
	// NOT going through the event bus (non-bus wrappers: Todoist action /
	// cadence task completion, and service-layer tests). No
	// interaction.recorded will be emitted, so InteractionRecorder's
	// inline CadenceUpdater.HandleEvent dispatch won't fire and cadence
	// columns would otherwise silently stop updating. Route through
	// CadenceUpdater.ApplyInteraction so this path stays under the
	// sole-writer invariant. publishesEvent=true callers (the
	// InteractionRecorder) still rely on the recorder's inline
	// HandleEvent path + event-id claim for dedupe across inline +
	// queued delivery.
	if !publishesEvent {
		if s.cadence == nil {
			return nil, errors.New("record interaction: cadence updater not wired (pass cadence to NewContactService)")
		}
		if err := s.cadence.ApplyInteraction(ctx, tx, repository.ApplyInteractionRequest{
			ContactID:  req.ContactID,
			Direction:  req.Direction,
			Source:     req.Source,
			OccurredAt: req.OccurredAt,
		}); err != nil {
			return nil, fmt.Errorf("apply interaction cadence: %w", err)
		}
	}

	// 7. Derive the follow-up closure. Cadence writes happen via either
	// the recorder's inline CadenceUpdater.HandleEvent (publishesEvent true)
	// or the direct ApplyInteraction call above (publishesEvent false).
	// Follow-up runs the same way: publishesEvent=true callers rely on the
	// recorder's inline FollowUpManager.HandleEvent; the publishesEvent=false
	// path (Todoist completion wrapper) applies it INLINE here, in the
	// caller's tx, so the op enqueue commits atomically with the interaction
	// (I4 — no best-effort post-commit closure).
	if !publishesEvent {
		if err := s.applyFollowUpInlineTx(ctx, tx, interaction); err != nil {
			return nil, err
		}
	}

	res := &RecordInteractionResult{
		Interaction: interaction,
		IsReplay:    false,
	}
	if publishesEvent {
		// The caller will publish interaction.recorded after this returns;
		// populate the V2 payload snapshot + cadence-at-emit fields so
		// the bus event carries the pre-image for downstream consumers.
		res.PrevCadence = &prevSnap
		res.CadenceAtEmit = cadenceAtEmit
	}
	return res, nil
}

// PromoteInteractionToMutual updates an outbound interaction to mutual (reply bridging)
// and applies the resulting contact field updates and follow-up completion.
//
// Non-tx wrapper. See PromoteInteractionToMutualTx for the tx-threaded variant.
func (s *ContactService) PromoteInteractionToMutual(ctx context.Context, interactionID, contactID uuid.UUID, replyAt time.Time) error {
	return pgx.BeginTxFunc(ctx, s.database.Pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		return s.PromoteInteractionToMutualTx(ctx, tx, interactionID, contactID, replyAt)
	})
}

// PromoteInteractionToMutualTx is the tx-threaded variant. Caller owns the tx.
// Cadence + follow-up effects (including the follow-up's op enqueue) run
// inline in that tx; there is no post-commit work.
func (s *ContactService) PromoteInteractionToMutualTx(
	ctx context.Context, tx pgx.Tx, interactionID, contactID uuid.UUID, replyAt time.Time,
) error {
	if s.cadence == nil {
		return errors.New("promote interaction: cadence updater not wired (pass cadence to NewContactService)")
	}
	updated, err := s.updateInteractionDirectionTx(ctx, tx, interactionID, repository.InteractionDirectionMutual, replyAt)
	if err != nil {
		return fmt.Errorf("update interaction direction: %w", err)
	}
	// Route cadence writes through CadenceUpdater so the sole-writer
	// invariant holds. Promote does NOT emit interaction.recorded and
	// therefore does not claim an event — ApplyInteraction is the
	// direct-invoke path that bypasses the claim store.
	if err := s.cadence.ApplyInteraction(ctx, tx, repository.ApplyInteractionRequest{
		ContactID:  contactID,
		Direction:  repository.InteractionDirectionMutual,
		Source:     updated.Source,
		OccurredAt: replyAt,
	}); err != nil {
		return fmt.Errorf("apply promote cadence: %w", err)
	}
	return s.applyFollowUpInlineTx(ctx, tx, updated)
}

// ExtendInteraction extends an existing interaction's timestamp/description (incremental
// coalescing) and re-applies contact field effects for the updated timestamp.
//
// Non-tx wrapper. See ExtendInteractionTx for the tx-threaded variant.
func (s *ContactService) ExtendInteraction(ctx context.Context, interactionID, contactID uuid.UUID, direction string, occurredAt time.Time, description *string) error {
	return pgx.BeginTxFunc(ctx, s.database.Pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		return s.ExtendInteractionTx(ctx, tx, interactionID, contactID, direction, occurredAt, description)
	})
}

// ExtendInteractionTx is the tx-threaded variant. Caller owns the tx. Note
// that direction is a caller-supplied argument because UpdateInteractionTimestamp
// does not change direction on the row — the caller knows which direction
// applies to the session being extended.
func (s *ContactService) ExtendInteractionTx(
	ctx context.Context, tx pgx.Tx, interactionID, contactID uuid.UUID, direction string, occurredAt time.Time, description *string,
) error {
	if s.cadence == nil {
		return errors.New("extend interaction: cadence updater not wired (pass cadence to NewContactService)")
	}
	updated, err := s.updateInteractionTimestampTx(ctx, tx, interactionID, occurredAt, description)
	if err != nil {
		return fmt.Errorf("update interaction timestamp: %w", err)
	}
	// UpdateInteractionTimestamp did not change the row's Direction column —
	// the persisted Direction may be outbound/inbound/mutual from a prior
	// write. The follow-up apply reads the row's Direction, which for
	// same-direction coalescing equals the caller-supplied direction. Guard
	// against surprises by overriding the row's Direction in-memory with the
	// caller's intent before applying effects.
	updated.Direction = direction
	// Route cadence writes through CadenceUpdater. Extend does NOT emit
	// interaction.recorded; ApplyInteraction is the direct-invoke path.
	if err := s.cadence.ApplyInteraction(ctx, tx, repository.ApplyInteractionRequest{
		ContactID:  contactID,
		Direction:  direction,
		Source:     updated.Source,
		OccurredAt: occurredAt,
	}); err != nil {
		return fmt.Errorf("apply extend cadence: %w", err)
	}
	return s.applyFollowUpInlineTx(ctx, tx, updated)
}

// updateInteractionDirectionTx / updateInteractionTimestampTx are thin
// tx-threaded wrappers around the InteractionRepository methods used by
// the promote/extend tx paths above. They live here because the repo
// already has tx-aware variants for the primary create/find operations;
// adding their peers is a mechanical mirror and avoids introducing a
// separate helper file.
func (s *ContactService) updateInteractionDirectionTx(
	ctx context.Context, tx pgx.Tx, id uuid.UUID, direction string, occurredAt time.Time,
) (*repository.Interaction, error) {
	return s.interactionRepo.UpdateInteractionDirectionTx(ctx, tx, id, direction, occurredAt)
}

func (s *ContactService) updateInteractionTimestampTx(
	ctx context.Context, tx pgx.Tx, id uuid.UUID, occurredAt time.Time, description *string,
) (*repository.Interaction, error) {
	return s.interactionRepo.UpdateInteractionTimestampTx(ctx, tx, id, occurredAt, description)
}

// applyFollowUpInlineTx routes non-bus follow-up work through
// FollowUpManager.ApplyInteraction INSIDE the caller's tx. Used by the
// non-bus callers (RecordInteractionTx for Todoist completion, Promote/
// Extend) where no interaction.recorded event is published and therefore
// the recorder's inline dispatch never fires. ApplyInteraction performs
// its own direction dispatch (unknown directions no-op) and enqueues any
// op job in the same tx, so the follow-up write commits atomically with
// the interaction — no best-effort post-commit closure (I4). No-op when
// the follow-up consumer isn't wired (tests).
func (s *ContactService) applyFollowUpInlineTx(ctx context.Context, tx pgx.Tx, interaction *repository.Interaction) error {
	if s.followUp == nil || interaction == nil {
		return nil
	}
	if err := s.followUp.ApplyInteraction(ctx, tx, repository.ApplyInteractionRequest{
		ContactID:  interaction.ContactID,
		Direction:  interaction.Direction,
		Source:     interaction.Source,
		OccurredAt: interaction.OccurredAt,
	}); err != nil {
		return fmt.Errorf("apply follow-up: %w", err)
	}
	return nil
}

// ListOverdueContacts retrieves contacts whose contact_by date is in the past.
// In production, uses the persistent contact_by field with database-level filtering.
// In testing mode with accelerated cadences, falls back to in-memory calculation
// since the DATE column loses precision needed for sub-day cadences.
func (s *ContactService) ListOverdueContacts(ctx context.Context) ([]OverdueContact, error) {
	now := accelerated.GetCurrentTime()

	// In testing mode, use in-memory calculation because the DATE column
	// loses precision needed for accelerated (sub-day) cadences
	if cadence.IsTestingMode() {
		return s.listOverdueContactsInMemory(ctx, now)
	}

	// In production, use the persistent contact_by field with database filtering
	today := cadence.Today(now)
	contacts, err := s.contactRepo.ListOverdueContacts(ctx, today, 1000)
	if err != nil {
		return nil, err
	}

	overdueContacts := make([]OverdueContact, 0, len(contacts))

	for _, contact := range contacts {
		// Attach methods to each contact
		if err := s.attachMethods(ctx, &contact); err != nil {
			return nil, err
		}

		// contact_by is guaranteed non-nil since the query filters for it
		daysOverdue := cadence.GetContactByOverdueDays(*contact.ContactBy, now)
		suggestedAction := suggestedActionForOverdueDays(daysOverdue)

		overdueContacts = append(overdueContacts, OverdueContact{
			Contact:         contact,
			DaysOverdue:     daysOverdue,
			NextDueDate:     *contact.ContactBy, // contact_by is the next due date
			SuggestedAction: suggestedAction,
		})
	}

	// Sort by most overdue first (highest days_overdue)
	sort.Slice(overdueContacts, func(i, j int) bool {
		return overdueContacts[i].DaysOverdue > overdueContacts[j].DaysOverdue
	})

	return overdueContacts, nil
}

// listOverdueContactsInMemory computes overdue status using last_contacted + cadence_duration.
// This is the fallback for testing mode where DATE precision is insufficient.
func (s *ContactService) listOverdueContactsInMemory(ctx context.Context, now time.Time) ([]OverdueContact, error) {
	// Fetch all contacts with a cadence set
	allContacts, err := s.contactRepo.ListContactsWithContactBy(ctx, 1000)
	if err != nil {
		return nil, err
	}

	overdueContacts := make([]OverdueContact, 0)

	for _, contact := range allContacts {
		// Skip contacts without cadence
		if contact.Cadence == nil || *contact.Cadence == "" {
			continue
		}

		cadenceType, err := cadence.ParseCadence(*contact.Cadence)
		if err != nil {
			continue
		}

		// Calculate overdue using in-memory timestamp comparison
		if !cadence.IsOverdueWithConfig(cadenceType, contact.LastContacted, contact.CreatedAt, now) {
			continue
		}

		// Attach methods
		if err := s.attachMethods(ctx, &contact); err != nil {
			return nil, err
		}

		// Calculate days overdue
		daysOverdue := cadence.GetOverdueDaysWithConfig(cadenceType, contact.LastContacted, contact.CreatedAt, now)
		suggestedAction := suggestedActionForOverdueDays(daysOverdue)
		nextDueDate := cadence.CalculateNextDueDateWithConfig(cadenceType, contact.LastContacted, contact.CreatedAt)

		overdueContacts = append(overdueContacts, OverdueContact{
			Contact:         contact,
			DaysOverdue:     daysOverdue,
			NextDueDate:     nextDueDate,
			SuggestedAction: suggestedAction,
		})
	}

	// Sort by most overdue first (highest days_overdue)
	sort.Slice(overdueContacts, func(i, j int) bool {
		return overdueContacts[i].DaysOverdue > overdueContacts[j].DaysOverdue
	})

	return overdueContacts, nil
}

func (s *ContactService) attachMethods(ctx context.Context, contact *repository.Contact) error {
	methods, err := s.contactMethodRepo.ListContactMethodsByContact(ctx, contact.ID)
	if err != nil {
		return err
	}

	sortContactMethods(methods)
	assignMethods(contact, methods)
	return nil
}

func (s *ContactService) attachMethodsToContacts(ctx context.Context, contacts []repository.Contact) error {
	for i := range contacts {
		if err := s.attachMethods(ctx, &contacts[i]); err != nil {
			return err
		}
	}
	return nil
}

func createContactMethods(ctx context.Context, repo *repository.ContactMethodRepository, contactID uuid.UUID, methods []ContactMethodInput) ([]repository.ContactMethod, error) {
	created := make([]repository.ContactMethod, 0, len(methods))

	for _, method := range methods {
		createdMethod, err := repo.CreateContactMethod(ctx, repository.CreateContactMethodRequest{
			ContactID: contactID,
			Type:      method.Type,
			Value:     method.Value,
			IsPrimary: method.IsPrimary,
		})
		if err != nil {
			return nil, err
		}
		created = append(created, *createdMethod)
	}

	sortContactMethods(created)
	return created, nil
}

func assignMethods(contact *repository.Contact, methods []repository.ContactMethod) {
	contact.Methods = methods
	contact.PrimaryMethod = findPrimaryMethod(methods)
}

func findPrimaryMethod(methods []repository.ContactMethod) *repository.ContactMethod {
	for i := range methods {
		if methods[i].IsPrimary {
			return &methods[i]
		}
	}
	return nil
}

func sortContactMethods(methods []repository.ContactMethod) {
	sort.SliceStable(methods, func(i, j int) bool {
		if methods[i].IsPrimary != methods[j].IsPrimary {
			return methods[i].IsPrimary
		}

		priorityI := contactMethodPriority(methods[i].Type)
		priorityJ := contactMethodPriority(methods[j].Type)
		if priorityI != priorityJ {
			return priorityI < priorityJ
		}

		if methods[i].CreatedAt.IsZero() || methods[j].CreatedAt.IsZero() {
			return methods[i].CreatedAt.Before(methods[j].CreatedAt)
		}

		return methods[i].CreatedAt.Before(methods[j].CreatedAt)
	})
}

func contactMethodPriority(methodType string) int {
	switch methodType {
	case string(repository.ContactMethodEmail):
		return 1
	case string(repository.ContactMethodPhone):
		return 2
	case string(repository.ContactMethodWhatsApp):
		return 3
	case string(repository.ContactMethodTelegram):
		return 4
	case string(repository.ContactMethodSignal):
		return 5
	case string(repository.ContactMethodDiscord):
		return 6
	case string(repository.ContactMethodTwitter):
		return 7
	case string(repository.ContactMethodGChat):
		return 8
	default:
		return 99
	}
}

func suggestedActionForOverdueDays(daysOverdue int) string {
	switch {
	case daysOverdue <= 2:
		return "Send a quick check-in message"
	case daysOverdue <= 7:
		return "Schedule a call or coffee"
	case daysOverdue <= 30:
		return "Send a meaningful update about your life"
	default:
		return "Reconnect with something specific and personal"
	}
}

// MergePreview contains information about what will be merged
type MergePreview struct {
	SourceContact          *repository.Contact `json:"source_contact"`
	TargetContact          *repository.Contact `json:"target_contact"`
	MethodsToTransfer      int64               `json:"methods_to_transfer"`
	DuplicateMethods       int64               `json:"duplicate_methods"`
	NotesToTransfer        int64               `json:"notes_to_transfer"`
	InteractionsToTransfer int64               `json:"interactions_to_transfer"`
	CalendarEventsToUpdate int64               `json:"calendar_events_to_update"`
}

// MergeContactsRequest contains the options for merging contacts
type MergeContactsRequest struct {
	// SourceContactID is the contact that will be archived after merge
	SourceContactID uuid.UUID `json:"source_contact_id"`
	// TargetContactID is the contact that will receive the merged data
	TargetContactID uuid.UUID `json:"target_contact_id"`
	// FieldSelections specifies which contact's value to use for conflicting fields
	FieldSelections MergeFieldSelections `json:"field_selections"`
	// NewName is the name to use for the merged contact (optional, defaults to target's name)
	NewName *string `json:"new_name,omitempty"`
}

// MergeFieldSelections specifies which contact's value to use for each field
type MergeFieldSelections struct {
	// Cadence: "source" or "target" (default: target)
	Cadence string `json:"cadence,omitempty"`
	// Location: "source" or "target" (default: target)
	Location string `json:"location,omitempty"`
	// Birthday: "source" or "target" (default: target)
	Birthday string `json:"birthday,omitempty"`
}

// GetMergePreview returns a preview of what will happen when merging two contacts
func (s *ContactService) GetMergePreview(ctx context.Context, sourceID, targetID uuid.UUID) (*MergePreview, error) {
	// Verify both contacts exist
	sourceContact, err := s.GetContact(ctx, sourceID)
	if err != nil {
		return nil, fmt.Errorf("source contact: %w", err)
	}

	targetContact, err := s.GetContact(ctx, targetID)
	if err != nil {
		return nil, fmt.Errorf("target contact: %w", err)
	}

	// Cannot merge contact with itself
	if sourceID == targetID {
		return nil, errors.New("cannot merge contact with itself")
	}

	// Get counts for preview
	sourceMethods, err := s.database.Queries.CountMergeContactMethods(ctx, sourceID)
	if err != nil {
		return nil, fmt.Errorf("count source methods: %w", err)
	}

	// Find duplicate methods
	duplicates, err := s.database.Queries.FindDuplicateContactMethods(ctx, db.FindDuplicateContactMethodsParams{
		SourceContactID: sourceID,
		TargetContactID: targetID,
	})
	if err != nil {
		return nil, fmt.Errorf("find duplicate methods: %w", err)
	}

	sourceNotes, err := s.database.Queries.CountMergeNotes(ctx, sourceID)
	if err != nil {
		return nil, fmt.Errorf("count source notes: %w", err)
	}

	sourceInteractions, err := s.database.Queries.CountMergeInteractions(ctx, sourceID)
	if err != nil {
		return nil, fmt.Errorf("count source interactions: %w", err)
	}

	sourceCalendarEvents, err := s.database.Queries.CountMergeCalendarEvents(ctx, sourceID)
	if err != nil {
		return nil, fmt.Errorf("count source calendar events: %w", err)
	}

	return &MergePreview{
		SourceContact:          sourceContact,
		TargetContact:          targetContact,
		MethodsToTransfer:      sourceMethods - int64(len(duplicates)),
		DuplicateMethods:       int64(len(duplicates)),
		NotesToTransfer:        sourceNotes,
		InteractionsToTransfer: sourceInteractions,
		CalendarEventsToUpdate: sourceCalendarEvents,
	}, nil
}

// MergeContacts merges the source contact into the target contact.
// All related entities are transferred to the target, and the source is soft-deleted.
func (s *ContactService) MergeContacts(ctx context.Context, req MergeContactsRequest) (mergedContact *repository.Contact, err error) {
	// Verify both contacts exist before starting transaction
	sourceContact, err := s.contactRepo.GetContact(ctx, req.SourceContactID)
	if err != nil {
		return nil, fmt.Errorf("source contact: %w", err)
	}

	targetContact, err := s.contactRepo.GetContact(ctx, req.TargetContactID)
	if err != nil {
		return nil, fmt.Errorf("target contact: %w", err)
	}

	// Cannot merge contact with itself
	if req.SourceContactID == req.TargetContactID {
		return nil, errors.New("cannot merge contact with itself")
	}

	// Start transaction
	tx, err := s.database.Pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() {
		if rollbackErr := tx.Rollback(ctx); rollbackErr != nil && !errors.Is(rollbackErr, pgx.ErrTxClosed) {
			if err == nil {
				err = rollbackErr
			}
		}
	}()

	txQueries := db.New(tx)
	sourceUUID := req.SourceContactID
	targetUUID := req.TargetContactID

	// 1. Delete duplicate contact methods (same normalized value and type)
	if err := txQueries.DeleteDuplicateContactMethods(ctx, db.DeleteDuplicateContactMethodsParams{
		SourceContactID: sourceUUID,
		TargetContactID: targetUUID,
	}); err != nil {
		return nil, fmt.Errorf("delete duplicate contact methods: %w", err)
	}

	// 1b. Demote the source's primary methods when the target already has a
	// primary, regardless of method type — idx_contact_method_primary is a
	// unique partial index on (contact_id) WHERE is_primary = true, so an
	// undemoted source primary of any type would collide on transfer.
	if err := txQueries.DemoteSourcePrimaryMethods(ctx, db.DemoteSourcePrimaryMethodsParams{
		SourceContactID: sourceUUID,
		TargetContactID: targetUUID,
	}); err != nil {
		return nil, fmt.Errorf("demote source primary methods: %w", err)
	}

	// 2. Transfer remaining contact methods
	if err := txQueries.TransferContactMethods(ctx, db.TransferContactMethodsParams{
		SourceContactID: sourceUUID,
		TargetContactID: targetUUID,
	}); err != nil {
		return nil, fmt.Errorf("transfer contact methods: %w", err)
	}

	// 3. Merge notepad notes (combine content if both exist)
	notepadCategory := string(repository.NoteCategoryNotepad)
	sourceNotepad, err := txQueries.GetContactNoteByCategory(ctx, db.GetContactNoteByCategoryParams{
		ContactID: sourceUUID,
		Category:  &notepadCategory,
	})
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("get source notepad: %w", err)
		}
		// sqlc :one returns a non-nil pointer even on ErrNoRows; nil it so the
		// no-notepad case doesn't flow through the combine path below.
		sourceNotepad = nil
	}

	if sourceNotepad != nil {
		// Delete the source notepad first regardless of content — otherwise
		// TransferNotes repoints it onto the target and collides with the
		// one-notepad-per-contact unique index when the target has one too.
		if err := txQueries.DeleteContactNoteByCategory(ctx, db.DeleteContactNoteByCategoryParams{
			ContactID: sourceUUID,
			Category:  &notepadCategory,
		}); err != nil {
			return nil, fmt.Errorf("delete source notepad: %w", err)
		}

		// An empty-body source notepad (e.g. minted by the pre-fix merge bug)
		// contributes nothing: leave the target's notepad state untouched.
		if sourceNotepad.Body != "" {
			targetNotepad, err := txQueries.GetContactNoteByCategory(ctx, db.GetContactNoteByCategoryParams{
				ContactID: targetUUID,
				Category:  &notepadCategory,
			})
			if err != nil {
				if !errors.Is(err, pgx.ErrNoRows) {
					return nil, fmt.Errorf("get target notepad: %w", err)
				}
				targetNotepad = nil
			}

			// Determine combined content
			var combinedBody string
			if targetNotepad != nil && targetNotepad.Body != "" {
				// Both have notepads - combine with separator
				combinedBody = targetNotepad.Body + "\n\n---\n\n" + sourceNotepad.Body
			} else {
				// Only source has notepad content
				combinedBody = sourceNotepad.Body
			}

			// Upsert combined content to target
			if _, err := txQueries.UpsertContactNoteByCategory(ctx, db.UpsertContactNoteByCategoryParams{
				ContactID: targetUUID,
				Body:      combinedBody,
				Category:  &notepadCategory,
			}); err != nil {
				return nil, fmt.Errorf("upsert merged notepad: %w", err)
			}
		}
	}

	// Transfer any remaining notes (non-notepad notes for future use)
	if err := txQueries.TransferNotes(ctx, db.TransferNotesParams{
		SourceContactID: sourceUUID,
		TargetContactID: targetUUID,
	}); err != nil {
		return nil, fmt.Errorf("transfer notes: %w", err)
	}

	// 4. Transfer interactions
	if err := txQueries.TransferInteractions(ctx, db.TransferInteractionsParams{
		SourceContactID: sourceUUID,
		TargetContactID: targetUUID,
	}); err != nil {
		return nil, fmt.Errorf("transfer interactions: %w", err)
	}

	// 5. Update calendar events
	if err := txQueries.ReplaceContactInCalendarEvents(ctx, db.ReplaceContactInCalendarEventsParams{
		SourceContactID: sourceUUID,
		TargetContactID: targetUUID,
	}); err != nil {
		return nil, fmt.Errorf("replace contact in calendar events: %w", err)
	}

	// 6. Deduplicate calendar event contact arrays
	if err := txQueries.DeduplicateCalendarEventContacts(ctx, targetUUID); err != nil {
		return nil, fmt.Errorf("deduplicate calendar event contacts: %w", err)
	}

	// 6b. Re-point identity + attribution state. Soft-deleting the source
	// below fires no FK cascades, so every one of these references would
	// otherwise dangle at a live-but-tombstoned row: the external_identity
	// cache would keep attributing future inbound events from the source's
	// handles to the dead contact, and already-committed staging rows
	// (including unprocessed ones) would strand against it.
	if err := txQueries.RepointIdentitiesToContact(ctx, db.RepointIdentitiesToContactParams{
		SourceContactID: &sourceUUID,
		TargetContactID: &targetUUID,
	}); err != nil {
		return nil, fmt.Errorf("repoint external identities: %w", err)
	}
	if err := txQueries.RepointExternalContactsToContact(ctx, db.RepointExternalContactsToContactParams{
		SourceContactID: &sourceUUID,
		TargetContactID: &targetUUID,
	}); err != nil {
		return nil, fmt.Errorf("repoint external contacts: %w", err)
	}
	if err := txQueries.RepointMessagesMessageContact(ctx, db.RepointMessagesMessageContactParams{
		SourceContactID: &sourceUUID,
		TargetContactID: &targetUUID,
	}); err != nil {
		return nil, fmt.Errorf("repoint messages_message staging rows: %w", err)
	}
	if err := txQueries.RepointTelegramMessageContact(ctx, db.RepointTelegramMessageContactParams{
		SourceContactID: &sourceUUID,
		TargetContactID: &targetUUID,
	}); err != nil {
		return nil, fmt.Errorf("repoint telegram_message staging rows: %w", err)
	}
	if err := txQueries.RepointPhoneCallContact(ctx, db.RepointPhoneCallContactParams{
		SourceContactID: &sourceUUID,
		TargetContactID: &targetUUID,
	}); err != nil {
		return nil, fmt.Errorf("repoint phone_call staging rows: %w", err)
	}
	// comms_message needs the savepoint-wrapped two-step (dedup soft-delete
	// then repoint): its dedup unique index includes matched_contact_id, so
	// an email fanned out to both contacts collides on a bare repoint.
	if err := s.commsRepo.RepointContactForMergeTx(ctx, tx, req.SourceContactID, req.TargetContactID); err != nil {
		return nil, fmt.Errorf("repoint comms_message staging rows: %w", err)
	}

	// 6c. contact_task, split by lifecycle: manual rows (user content) follow
	// the survivor in every state (collision-free — no unique index covers
	// lifecycle='manual'). Automated rows (cadence_due/followup_loop)
	// transfer to the survivor whenever no unique index blocks the move; a
	// transfer that loses a race to a concurrent insert falls back to
	// closing rather than failing the merge. The rest close, with a durable
	// remote close only for rows carrying a real external id. No row is ever
	// deleted by a merge — orphan cleanup of merge-closed automated tasks is
	// tracked separately (issue #811).
	if err := txQueries.RepointManualContactTasksToContact(ctx, db.RepointManualContactTasksToContactParams{
		SourceContactID: sourceUUID,
		TargetContactID: targetUUID,
	}); err != nil {
		return nil, fmt.Errorf("repoint manual contact tasks: %w", err)
	}
	transferredRefs, err := s.contactTaskRepo.TransferAutomatedTasksForMergeTx(ctx, tx, req.SourceContactID, req.TargetContactID)
	if err != nil {
		return nil, fmt.Errorf("transfer automated contact tasks: %w", err)
	}
	logger.Debug().
		Str("source_contact_id", req.SourceContactID.String()).
		Str("target_contact_id", req.TargetContactID.String()).
		Int("transferred_tasks", len(transferredRefs)).
		Msg("merge: transferred automated contact tasks to survivor")
	closedRefs, err := s.contactTaskRepo.CompleteLiveTasksForContactTx(ctx, tx, req.SourceContactID, todoist.MetadataKeyPendingTempID)
	if err != nil {
		return nil, fmt.Errorf("close live contact tasks: %w", err)
	}
	// Remote-close enqueue for rows with a REAL external id (non-empty and
	// not a pending temp id — mirrors every existing close path's
	// isPendingTempID gate; the close worker has no temp-id guard of its
	// own). Temp-ID and empty-ID rows are closed locally only: their remote
	// cleanup is owned by the create worker's close-while-pending branch
	// (empty id) and the state-aware temp-mapping finalize (temp id).
	var eligibleRefs []repository.CompletedTaskRef
	for _, ref := range closedRefs {
		if ref.ExternalTaskID != "" && ref.PendingTempID != ref.ExternalTaskID {
			eligibleRefs = append(eligibleRefs, ref)
		}
	}
	if len(eligibleRefs) > 0 {
		switch {
		case !s.taskCloseConfigured:
			return nil, errors.New("merge contacts: task close enqueuer not wired (call SetTaskCloseEnqueuer)")
		case !s.taskCloseRemoteEnabled:
			logger.Warn().
				Str("source_contact_id", req.SourceContactID.String()).
				Int("closed_tasks", len(eligibleRefs)).
				Msg("merge: remote task close disabled (follow-up mode off); tasks closed locally only")
		default:
			for _, ref := range eligibleRefs {
				if _, err := s.taskCloseEnqueuer.InsertTx(ctx, tx, consumerjobs.TodoistFollowUpCloseJobArgs{ContactTaskID: ref.ID}, &river.InsertOpts{MaxAttempts: 10}); err != nil {
					return nil, fmt.Errorf("enqueue todoist close for merged task %s: %w", ref.ID, err)
				}
			}
		}
	}

	// 7. Update target contact with field selections and optional new
	// name. This write is profile-only (name/location/birthday/cadence/
	// photo/how_met) and NEVER touches the four cadence columns.
	// Cadence columns are applied in the next step via
	// cadenceUpdater.BulkApply so the sole-writer invariant holds.
	txContactRepo := repository.NewContactRepository(txQueries)
	updateReq := buildMergeUpdateRequest(targetContact, sourceContact, req)
	mergedContact, err = txContactRepo.UpdateContact(ctx, req.TargetContactID, updateReq)
	if err != nil {
		return nil, fmt.Errorf("update target contact: %w", err)
	}

	// Keep the target's person node label synced with the merged name
	// (node.id == contact.id). The UpdateContact above always writes full_name
	// in this same tx, so sync the node UNCONDITIONALLY to avoid divergence.
	nodeRepo := repository.NewNodeRepository(txQueries)
	if err := nodeRepo.UpdateNodeCanonicalLabelTx(ctx, tx, req.TargetContactID, updateReq.FullName); err != nil {
		return nil, fmt.Errorf("sync target person node label: %w", err)
	}

	if s.knowledge == nil {
		return nil, errors.New("merge contacts: knowledge writer not wired (pass assertSvc+knowledgeCache to NewContactService)")
	}

	// 7-graph. Graph merge FIRST: tombstone the source person node
	// (merged_into=target, deleted_at=now) and re-point every assertion touching
	// the source onto the target node (D9). This migrates the source contact's OWN
	// knowledge edges/facts onto the survivor. It runs BEFORE the field-selection
	// apply below so the user's per-field merge choice is the LAST writer and wins:
	// a re-pointed source value can supersede the target's prior, but the chosen
	// value (asserted next) then supersedes it in turn, and the inline cache
	// refresh reflects the final state. Where the user kept the target's value the
	// re-pointed same-value source row collapses via proposition identity.
	if err := s.knowledge.mergeNodes(ctx, tx, req.SourceContactID, req.TargetContactID); err != nil {
		return nil, fmt.Errorf("merge source node into target: %w", err)
	}

	// Authority flip: the merged profile's location/birthday/how_met (target-or-
	// source per field selection) are persisted as assertions on the target node,
	// then the cache columns refresh inline. updateReq carries the chosen values;
	// targetContact carries the pre-merge values, so the diff drives
	// supersession (a source-preferred field) vs corroboration (target kept).
	if err := s.knowledge.applyUpdate(ctx, tx, req.TargetContactID,
		knowledgeFieldValues{Location: updateReq.Location, Birthday: updateReq.Birthday, HowMet: updateReq.HowMet},
		knowledgeFieldValues{Location: targetContact.Location, Birthday: targetContact.Birthday, HowMet: targetContact.HowMet},
		knowledgeFieldProvenance{
			SourceKind:     repository.SourceKindUser,
			ProducerKind:   repository.ProducerKindUser,
			SourceIDPrefix: "merge",
		}); err != nil {
		return nil, fmt.Errorf("apply merged contact knowledge: %w", err)
	}

	// 7a. Forward-max merged cadence columns through
	// CadenceUpdater.BulkApply. The merged cadence string may come from
	// the source (when FieldSelections.Cadence == "source"), so
	// contact_by is derived from the CHOSEN cadence value — not from
	// the pre-merge target.cadence, which might have been overwritten
	// above.
	if s.cadence == nil {
		return nil, errors.New("merge contacts: cadence updater not wired (pass cadence to NewContactService)")
	}
	mergedFields := buildMergeCadenceFields(targetContact, sourceContact, updateReq.Cadence)
	if err := s.cadence.BulkApply(ctx, tx, req.TargetContactID, mergedFields); err != nil {
		return nil, fmt.Errorf("bulk apply merged cadence: %w", err)
	}

	// BulkApply is a second UPDATE on the contact row, so the profile-
	// only UpdateContact's RETURNING values (updated_at + the four
	// cadence columns) are stale. Re-fetch inside the tx so the struct
	// the caller receives matches the committed row bit-for-bit.
	mergedContact, err = txContactRepo.GetContact(ctx, req.TargetContactID)
	if err != nil {
		return nil, fmt.Errorf("refetch merged contact after bulk apply: %w", err)
	}

	// 8. Soft delete source contact
	if err := txQueries.SoftDeleteContact(ctx, sourceUUID); err != nil {
		return nil, fmt.Errorf("soft delete source contact: %w", err)
	}

	// Test-only commit barrier: lets a race test commit a source-attributed
	// row from a second connection between the in-tx repoints above and the
	// commit below, deterministically exercising the post-commit second
	// pass. Nil in production.
	if s.mergeCommitBarrier != nil {
		s.mergeCommitBarrier()
	}

	// Commit transaction
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}

	// Post-commit second pass over the attribution repoints. The merge tx
	// runs at READ COMMITTED and only tombstones the source at commit, so a
	// concurrently racing ingest can read the pre-merge identity row (cache
	// hit → source) and commit a source-attributed row AFTER the in-tx
	// repoint statements took their snapshots. Re-running the idempotent
	// repoints once post-commit catches every raced row committed before
	// this pass. Best-effort: the merge itself committed, so failures are
	// logged at ERROR and do not fail the merge. (Residual: a raced ingest
	// committing after this pass strands at most one row — staging rows
	// self-heal via the identity liveness guard on the next event;
	// external_contact is the documented indefinite residual, fixed
	// manually via the import UI.)
	s.repointMergedAttributionSecondPass(ctx, req.SourceContactID, req.TargetContactID)

	// Attach methods to the merged contact
	if err := s.attachMethods(ctx, mergedContact); err != nil {
		return nil, err
	}

	return mergedContact, nil
}

// repointMergedAttributionSecondPass re-runs the merge's identity +
// external_contact + staging repoints once via the non-tx path. Every
// statement is an idempotent "WHERE ... = source" update, so re-running after
// commit is safe; comms_message reuses the savepoint-wrapped two-step inside
// a short transaction. Errors are logged at ERROR and swallowed — see the
// call site in MergeContacts.
func (s *ContactService) repointMergedAttributionSecondPass(ctx context.Context, sourceID, targetID uuid.UUID) {
	q := s.database.Queries

	logSecondPassErr := func(step string, err error) {
		logger.Error().Err(err).
			Str("source_contact_id", sourceID.String()).
			Str("target_contact_id", targetID.String()).
			Str("step", step).
			Msg("merge attribution second pass failed; raced rows may remain attributed to the merged-away contact")
	}

	if err := q.RepointIdentitiesToContact(ctx, db.RepointIdentitiesToContactParams{
		SourceContactID: &sourceID, TargetContactID: &targetID,
	}); err != nil {
		logSecondPassErr("external_identity", err)
	}
	if err := q.RepointExternalContactsToContact(ctx, db.RepointExternalContactsToContactParams{
		SourceContactID: &sourceID, TargetContactID: &targetID,
	}); err != nil {
		logSecondPassErr("external_contact", err)
	}
	if err := q.RepointMessagesMessageContact(ctx, db.RepointMessagesMessageContactParams{
		SourceContactID: &sourceID, TargetContactID: &targetID,
	}); err != nil {
		logSecondPassErr("messages_message", err)
	}
	if err := q.RepointTelegramMessageContact(ctx, db.RepointTelegramMessageContactParams{
		SourceContactID: &sourceID, TargetContactID: &targetID,
	}); err != nil {
		logSecondPassErr("telegram_message", err)
	}
	if err := q.RepointPhoneCallContact(ctx, db.RepointPhoneCallContactParams{
		SourceContactID: &sourceID, TargetContactID: &targetID,
	}); err != nil {
		logSecondPassErr("phone_call", err)
	}
	if err := pgx.BeginTxFunc(ctx, s.database.Pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		return s.commsRepo.RepointContactForMergeTx(ctx, tx, sourceID, targetID)
	}); err != nil {
		logSecondPassErr("comms_message", err)
	}
}

// buildMergeUpdateRequest creates an UpdateContactRequest based on field selections
func buildMergeUpdateRequest(targetContact, sourceContact *repository.Contact, req MergeContactsRequest) repository.UpdateContactRequest {
	updateReq := repository.UpdateContactRequest{
		FullName:     targetContact.FullName,
		Location:     targetContact.Location,
		Birthday:     targetContact.Birthday,
		HowMet:       targetContact.HowMet,
		Cadence:      targetContact.Cadence,
		ProfilePhoto: targetContact.ProfilePhoto,
	}

	// Override name if provided
	if req.NewName != nil && *req.NewName != "" {
		updateReq.FullName = *req.NewName
	}

	// Apply field selections
	if req.FieldSelections.Location == "source" && sourceContact.Location != nil {
		updateReq.Location = sourceContact.Location
	}
	if req.FieldSelections.Birthday == "source" && sourceContact.Birthday != nil {
		updateReq.Birthday = sourceContact.Birthday
	}
	if req.FieldSelections.Cadence == "source" && sourceContact.Cadence != nil {
		updateReq.Cadence = sourceContact.Cadence
	}

	// Notes are handled separately in MergeContacts using the note table

	return updateReq
}

// buildMergeCadenceFields computes the per-field forward-max cadence
// values for MergeContacts. Timestamp columns take max(source, target);
// contact_by is derived from the CHOSEN merged cadence string + the
// merged max(last_contacted) — not from either pre-merge value, so a
// merge that elects the source's cadence string gets a freshly-computed
// contact_by rather than preserving target's stale one.
//
// Returns the fields struct as-is; CadenceUpdater.BulkApply routes on
// the forward-only branch so a source-preferring merge can never move
// target state backward.
func buildMergeCadenceFields(targetContact, sourceContact *repository.Contact, mergedCadence *string) repository.ContactCadenceFields {
	fields := repository.ContactCadenceFields{
		LastContacted:  maxTimePtr(targetContact.LastContacted, sourceContact.LastContacted),
		LastOutreachAt: maxTimePtr(targetContact.LastOutreachAt, sourceContact.LastOutreachAt),
		LastResponseAt: maxTimePtr(targetContact.LastResponseAt, sourceContact.LastResponseAt),
	}
	// Derive merged contact_by from the CHOSEN cadence (which may be the
	// source's after field-selection). Base = merged last_contacted or
	// the newer created_at. Parse failures fall through to nil.
	if mergedCadence != nil && *mergedCadence != "" {
		if cadenceType, err := cadence.ParseCadence(*mergedCadence); err == nil {
			var base time.Time
			switch {
			case fields.LastContacted != nil:
				base = *fields.LastContacted
			case targetContact.CreatedAt.After(sourceContact.CreatedAt):
				base = targetContact.CreatedAt
			default:
				base = sourceContact.CreatedAt
			}
			t := cadence.CalculateContactBy(base, cadenceType)
			fields.ContactBy = &t
		}
	}
	return fields
}

// maxTimePtr returns a pointer to the strictly-later of a and b.
// nil-nil returns nil; otherwise the non-nil side wins.
func maxTimePtr(a, b *time.Time) *time.Time {
	switch {
	case a == nil && b == nil:
		return nil
	case a == nil:
		return b
	case b == nil:
		return a
	case b.After(*a):
		return b
	default:
		return a
	}
}
