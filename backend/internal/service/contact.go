package service

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/cadence"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/logger"
	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
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
// ContactService depends on post-cutover. HandleEvent is exposed for
// completeness (ContactService itself doesn't call it — the
// InteractionRecorder does), and ApplyInteraction is the direct-invoke
// path for non-bus callers (Todoist completion wrapper,
// Promote/ExtendInteraction). Both methods return a post-commit
// closure so the refresh path's Todoist item_update runs outside the
// caller's tx per core.md rule 153.
//
// Interface lives here (not on the consumer) so tests can stub
// follow-up behavior without building the full consumer graph.
type FollowUpConsumer interface {
	ApplyInteraction(ctx context.Context, tx pgx.Tx, req repository.ApplyInteractionRequest) (postCommit func(context.Context), err error)
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
	// contact_task.kind='follow_up' lifecycle post-cutover. Injected via
	// SetFollowUpConsumer after construction to avoid a circular dep
	// with consumer wiring. Non-bus callers (Todoist completion path,
	// Promote/Extend) route through followUp.ApplyInteraction inside
	// deriveFollowUpClosure.
	followUp   FollowUpConsumer
	rematchSvc *RematchService
	// cadence is the direct-invoke writer (the sole writer of the four
	// cadence columns post-cutover). Injected via SetCadenceUpdater
	// after construction to avoid a circular dep with consumer wiring.
	// When unset, MergeContacts / ExtendInteraction /
	// PromoteInteractionToMutual / cadence-edit UpdateContact paths
	// return an error rather than silently no-op-ing on a code path
	// that was supposed to mutate cadence state.
	cadence cadenceWriter
}

func NewContactService(database *db.Database, contactRepo *repository.ContactRepository, contactMethodRepo *repository.ContactMethodRepository, interactionRepo *repository.InteractionRepository, contactTaskRepo *repository.ContactTaskRepository) *ContactService {
	return &ContactService{
		database:          database,
		contactRepo:       contactRepo,
		contactMethodRepo: contactMethodRepo,
		interactionRepo:   interactionRepo,
		contactTaskRepo:   contactTaskRepo,
	}
}

// SetFollowUpConsumer injects the FollowUpManager consumer. Matches
// SetCadenceUpdater's setter-after-construction pattern so main.go
// can build the service and consumer independently and wire them
// together once both exist. Non-bus callers (Todoist completion,
// Promote/Extend) require this dependency to be set — otherwise
// deriveFollowUpClosure returns nil and no follow-up work fires.
func (s *ContactService) SetFollowUpConsumer(fm FollowUpConsumer) {
	s.followUp = fm
}

// SetRematchService injects the rematch service. Safe to leave unset — CreateContact
// and UpdateContact return uuid.Nil as the jobID when nil.
func (s *ContactService) SetRematchService(r *RematchService) {
	s.rematchSvc = r
}

// SetCadenceUpdater injects the cadence writer. Main.go wires this
// after constructing both the service and the consumer.CadenceUpdater;
// the deferred wire-in avoids a circular construction dependency.
// Non-event-bus cadence entry points (MergeContacts, ExtendInteraction,
// PromoteInteractionToMutual, user-driven cadence edits in
// UpdateContact) require this dependency to be set — otherwise they
// return an error rather than silently no-op-ing on a code path that
// was supposed to mutate cadence state.
func (s *ContactService) SetCadenceUpdater(c cadenceWriter) {
	s.cadence = c
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

// Deprecated: Use ListContactsPage when pagination metadata is needed.
func (s *ContactService) ListContacts(ctx context.Context, params repository.ListContactsParams) ([]repository.Contact, error) {
	contacts, err := s.contactRepo.ListContacts(ctx, params)
	if err != nil {
		return nil, err
	}

	if err := s.attachMethodsToContacts(ctx, contacts); err != nil {
		return nil, err
	}

	return contacts, nil
}

func (s *ContactService) ListContactsPage(ctx context.Context, params repository.ListContactsParams) ([]repository.Contact, int64, error) {
	contacts, err := s.contactRepo.ListContacts(ctx, params)
	if err != nil {
		return nil, 0, err
	}

	if err := s.attachMethodsToContacts(ctx, contacts); err != nil {
		return nil, 0, err
	}

	total, err := s.contactRepo.CountContacts(ctx, params.CadenceFilter, params.FollowupFilter)
	if err != nil {
		return nil, 0, err
	}

	return contacts, total, nil
}

// Deprecated: Use SearchContactsPage when pagination metadata is needed.
func (s *ContactService) SearchContacts(ctx context.Context, params repository.SearchContactsParams) ([]repository.Contact, error) {
	contacts, err := s.contactRepo.SearchContacts(ctx, params)
	if err != nil {
		return nil, err
	}

	if err := s.attachMethodsToContacts(ctx, contacts); err != nil {
		return nil, err
	}

	return contacts, nil
}

func (s *ContactService) SearchContactsPage(ctx context.Context, params repository.SearchContactsParams) ([]repository.Contact, int64, error) {
	contacts, err := s.contactRepo.SearchContacts(ctx, params)
	if err != nil {
		return nil, 0, err
	}

	if err := s.attachMethodsToContacts(ctx, contacts); err != nil {
		return nil, 0, err
	}

	total, err := s.contactRepo.CountSearchContacts(ctx, params.Query, params.CadenceFilter, params.FollowupFilter)
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

	contact, err = contactRepo.CreateContact(ctx, req)
	if err != nil {
		return nil, uuid.Nil, err
	}

	createdMethods, err := createContactMethods(ctx, contactMethodRepo, contact.ID, methods)
	if err != nil {
		return nil, uuid.Nil, err
	}

	if err = tx.Commit(ctx); err != nil {
		return nil, uuid.Nil, err
	}

	assignMethods(contact, createdMethods)
	if s.rematchSvc != nil && len(createdMethods) > 0 {
		jobID = s.rematchSvc.StartRematchForContact(contact.ID, toRematchMethods(createdMethods))
	}
	return contact, jobID, nil
}

func (s *ContactService) UpdateContact(ctx context.Context, id uuid.UUID, req repository.UpdateContactRequest, methods []ContactMethodInput, replaceMethods bool) (contact *repository.Contact, jobID uuid.UUID, err error) {
	if s.cadence == nil {
		// Sole-writer invariant: contact_by recomputation on cadence
		// edits must route through CadenceUpdater. Refusing to operate
		// without it prevents a silent rollback to the direct-path
		// behavior.
		return nil, uuid.Nil, errors.New("update contact: cadence updater not wired (call SetCadenceUpdater)")
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

	existingContact, err := contactRepo.GetContact(ctx, id)
	if err != nil {
		return nil, uuid.Nil, err
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
		return nil, uuid.Nil, err
	}

	if err := s.cadence.ApplyContactByOverride(ctx, tx, id, newContactBy); err != nil {
		return nil, uuid.Nil, fmt.Errorf("apply cadence contact_by override: %w", err)
	}
	// ApplyContactByOverride is a second UPDATE on the contact row, so
	// the profile-only UpdateContact's RETURNING values (notably
	// updated_at AND contact_by) are stale by the time we'd use them.
	// Re-fetch inside the tx so the struct the caller receives matches
	// the committed row bit-for-bit.
	contact, err = contactRepo.GetContact(ctx, id)
	if err != nil {
		return nil, uuid.Nil, fmt.Errorf("refetch contact after cadence override: %w", err)
	}

	var (
		updatedMethods []repository.ContactMethod
		existingBefore []repository.ContactMethod
	)
	if replaceMethods {
		existingBefore, err = contactMethodRepo.ListContactMethodsByContact(ctx, id)
		if err != nil {
			return nil, uuid.Nil, err
		}
		if err := contactMethodRepo.DeleteContactMethodsByContact(ctx, id); err != nil {
			return nil, uuid.Nil, err
		}

		updatedMethods, err = createContactMethods(ctx, contactMethodRepo, id, methods)
		if err != nil {
			return nil, uuid.Nil, err
		}
	} else {
		updatedMethods, err = contactMethodRepo.ListContactMethodsByContact(ctx, id)
		if err != nil {
			return nil, uuid.Nil, err
		}
		sortContactMethods(updatedMethods)
	}

	if err = tx.Commit(ctx); err != nil {
		return nil, uuid.Nil, err
	}

	assignMethods(contact, updatedMethods)
	if replaceMethods && s.rematchSvc != nil {
		newlyAdded := diffNewMethods(existingBefore, updatedMethods)
		if len(newlyAdded) > 0 {
			jobID = s.rematchSvc.StartRematchForContact(id, newlyAdded)
		}
	}
	return contact, jobID, nil
}

func (s *ContactService) DeleteContact(ctx context.Context, id uuid.UUID) error {
	_, err := s.contactRepo.GetContact(ctx, id)
	if err != nil {
		return err
	}

	return s.contactRepo.SoftDeleteContact(ctx, id)
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
// RecordInteractionTx. Used by Todoist (PR 11) and internal service callers
// that don't own a tx. The event-bus consumer and manual-UI handler use
// RecordInteractionTx directly so they can share the outer tx (spec §3.4.1
// atomicity contract; plan Decision 4a).
func (s *ContactService) RecordInteraction(ctx context.Context, req repository.RecordInteractionRequest) (*repository.Interaction, error) {
	var res *RecordInteractionResult
	err := pgx.BeginTxFunc(ctx, s.database.Pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		var txErr error
		// Non-event-bus wrapper: no paired interaction.recorded event
		// will be published, so withShadow=false — shadow drain is
		// skipped entirely.
		res, txErr = s.RecordInteractionTx(ctx, tx, false, req)
		return txErr
	})
	if err != nil {
		return nil, err
	}
	if res.FollowUpFn != nil {
		res.FollowUpFn(ctx)
	}
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
// The `withShadow` flag tells the service whether the caller intends to
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
	ctx context.Context, tx pgx.Tx, withShadow bool, req repository.RecordInteractionRequest,
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
		existing, err := s.interactionRepo.FindInWindowTx(ctx, tx, req.ContactID, req.Source, req.OccurredAt, 30*time.Minute)
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

	// 4. Capture pre-cadence snapshot + cadence-at-emit from the in-memory
	// contact BEFORE the write. This is the pre-image that both the direct
	// path and the payload-carried consumer prev use (plan Decision 2a).
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

	// 6. Direct-path cadence write. When withShadow=false the caller is
	// NOT going through the event bus (non-bus wrappers: Todoist action /
	// cadence task completion, and service-layer tests). No
	// interaction.recorded will be emitted, so InteractionRecorder's
	// inline CadenceUpdater.HandleEvent dispatch won't fire and cadence
	// columns would otherwise silently stop updating. Route through
	// CadenceUpdater.ApplyInteraction so this path stays under the
	// sole-writer invariant. withShadow=true callers (the
	// InteractionRecorder) still rely on the recorder's inline
	// HandleEvent path + event-id claim for dedupe across inline +
	// queued delivery.
	if !withShadow {
		if s.cadence == nil {
			return nil, errors.New("record interaction: cadence updater not wired (call SetCadenceUpdater)")
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
	// the recorder's inline CadenceUpdater.HandleEvent (withShadow true)
	// or the direct ApplyInteraction call above (withShadow false). The
	// FollowUpManager consumer runs inline in the recorder on the
	// withShadow=true path (post-commit closure for refresh path only);
	// the withShadow=false path (Todoist completion wrapper) still
	// needs a post-commit closure that calls
	// FollowUpManager.ApplyInteraction on a fresh tx.
	var followUpFn func(context.Context)
	if !withShadow {
		followUpFn = s.deriveFollowUpClosure(contact, interaction)
	}

	res := &RecordInteractionResult{
		Interaction: interaction,
		IsReplay:    false,
		FollowUpFn:  followUpFn,
	}
	if withShadow {
		// withShadow's name is a historical artifact — post-cutover it
		// means "populate the V2 payload snapshot + cadence-at-emit
		// fields so the bus event carries the pre-image for downstream
		// consumers".
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
	var postCommit func(context.Context)
	err := pgx.BeginTxFunc(ctx, s.database.Pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		var txErr error
		postCommit, txErr = s.PromoteInteractionToMutualTx(ctx, tx, interactionID, contactID, replyAt)
		return txErr
	})
	if err != nil {
		return err
	}
	if postCommit != nil {
		postCommit(ctx)
	}
	return nil
}

// PromoteInteractionToMutualTx is the tx-threaded variant. Caller owns the tx.
// The returned postCommit closure (nil-safe) captures follow-up-manager work
// that should fire AFTER the tx commits (plan Decision 4 + 8).
func (s *ContactService) PromoteInteractionToMutualTx(
	ctx context.Context, tx pgx.Tx, interactionID, contactID uuid.UUID, replyAt time.Time,
) (func(context.Context), error) {
	if s.cadence == nil {
		return nil, errors.New("promote interaction: cadence updater not wired (call SetCadenceUpdater)")
	}
	updated, err := s.updateInteractionDirectionTx(ctx, tx, interactionID, repository.InteractionDirectionMutual, replyAt)
	if err != nil {
		return nil, fmt.Errorf("update interaction direction: %w", err)
	}
	contact, err := s.contactRepo.GetContactTx(ctx, tx, contactID)
	if err != nil {
		return nil, fmt.Errorf("get contact for promotion: %w", err)
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
		return nil, fmt.Errorf("apply promote cadence: %w", err)
	}
	return s.deriveFollowUpClosure(contact, updated), nil
}

// ExtendInteraction extends an existing interaction's timestamp/description (incremental
// coalescing) and re-applies contact field effects for the updated timestamp.
//
// Non-tx wrapper. See ExtendInteractionTx for the tx-threaded variant.
func (s *ContactService) ExtendInteraction(ctx context.Context, interactionID, contactID uuid.UUID, direction string, occurredAt time.Time, description *string) error {
	var postCommit func(context.Context)
	err := pgx.BeginTxFunc(ctx, s.database.Pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		var txErr error
		postCommit, txErr = s.ExtendInteractionTx(ctx, tx, interactionID, contactID, direction, occurredAt, description)
		return txErr
	})
	if err != nil {
		return err
	}
	if postCommit != nil {
		postCommit(ctx)
	}
	return nil
}

// ExtendInteractionTx is the tx-threaded variant. Caller owns the tx. Note
// that direction is a caller-supplied argument because UpdateInteractionTimestamp
// does not change direction on the row — the caller knows which direction
// applies to the session being extended.
func (s *ContactService) ExtendInteractionTx(
	ctx context.Context, tx pgx.Tx, interactionID, contactID uuid.UUID, direction string, occurredAt time.Time, description *string,
) (func(context.Context), error) {
	if s.cadence == nil {
		return nil, errors.New("extend interaction: cadence updater not wired (call SetCadenceUpdater)")
	}
	updated, err := s.updateInteractionTimestampTx(ctx, tx, interactionID, occurredAt, description)
	if err != nil {
		return nil, fmt.Errorf("update interaction timestamp: %w", err)
	}
	contact, err := s.contactRepo.GetContactTx(ctx, tx, contactID)
	if err != nil {
		return nil, fmt.Errorf("get contact for extension: %w", err)
	}
	// UpdateInteractionTimestamp did not change the row's Direction column —
	// the persisted Direction may be outbound/inbound/mutual from a prior
	// write. deriveFollowUpClosure reads the persisted row's Direction,
	// which for same-direction coalescing equals the caller-supplied
	// direction. Guard against surprises by overriding the row's
	// Direction in-memory with the caller's intent before applying effects.
	updated.Direction = direction
	// Route cadence writes through CadenceUpdater. Extend does NOT emit
	// interaction.recorded; ApplyInteraction is the direct-invoke path.
	if err := s.cadence.ApplyInteraction(ctx, tx, repository.ApplyInteractionRequest{
		ContactID:  contactID,
		Direction:  direction,
		Source:     updated.Source,
		OccurredAt: occurredAt,
	}); err != nil {
		return nil, fmt.Errorf("apply extend cadence: %w", err)
	}
	return s.deriveFollowUpClosure(contact, updated), nil
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

// deriveFollowUpClosure builds the nil-safe post-commit closure that
// routes follow-up work through FollowUpManager.ApplyInteraction. Used
// by non-bus callers (non-tx RecordInteraction wrapper for Todoist
// completion, Promote/Extend) where no interaction.recorded event is
// published and therefore the recorder's inline dispatch never fires.
// The closure opens a fresh tx on the pool, invokes ApplyInteraction
// inside it, and runs the returned inner post-commit closure (refresh
// path only) after the fresh tx commits.
func (s *ContactService) deriveFollowUpClosure(contact *repository.Contact, interaction *repository.Interaction) func(context.Context) {
	if contact == nil || interaction == nil || s.followUp == nil {
		return nil
	}
	direction := interaction.Direction
	switch direction {
	case repository.InteractionDirectionOutbound,
		repository.InteractionDirectionInbound,
		repository.InteractionDirectionMutual:
		// ok
	default:
		return nil
	}

	contactID := contact.ID
	contactSource := interaction.Source
	occ := interaction.OccurredAt
	dir := direction
	fm := s.followUp
	pool := s.database.Pool

	return func(postCtx context.Context) {
		req := repository.ApplyInteractionRequest{
			ContactID:  contactID,
			Direction:  dir,
			Source:     contactSource,
			OccurredAt: occ,
		}
		var innerPostCommit func(context.Context)
		err := pgx.BeginTxFunc(postCtx, pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
			pc, applyErr := fm.ApplyInteraction(postCtx, tx, req)
			if applyErr != nil {
				return applyErr
			}
			innerPostCommit = pc
			return nil
		})
		if err != nil {
			logger.Warn().Err(err).
				Str("contactId", contactID.String()).
				Str("direction", dir).
				Msg("failed to apply follow-up via consumer")
			return
		}
		// Run the nested post-commit closure OUTSIDE the fresh tx —
		// this is where the Todoist item_update (refresh path) runs.
		// On failure, the closure itself handles enqueueing a
		// TodoistFollowUpRefreshJob for river-managed retry.
		if innerPostCommit != nil {
			innerPostCommit(postCtx)
		}
	}
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
	sourceMethods, err := s.database.Queries.CountMergeContactMethods(ctx, uuidToPgUUID(sourceID))
	if err != nil {
		return nil, fmt.Errorf("count source methods: %w", err)
	}

	// Find duplicate methods
	duplicates, err := s.database.Queries.FindDuplicateContactMethods(ctx, db.FindDuplicateContactMethodsParams{
		SourceContactID: uuidToPgUUID(sourceID),
		TargetContactID: uuidToPgUUID(targetID),
	})
	if err != nil {
		return nil, fmt.Errorf("find duplicate methods: %w", err)
	}

	sourceNotes, err := s.database.Queries.CountMergeNotes(ctx, uuidToPgUUID(sourceID))
	if err != nil {
		return nil, fmt.Errorf("count source notes: %w", err)
	}

	sourceInteractions, err := s.database.Queries.CountMergeInteractions(ctx, uuidToPgUUID(sourceID))
	if err != nil {
		return nil, fmt.Errorf("count source interactions: %w", err)
	}

	sourceCalendarEvents, err := s.database.Queries.CountMergeCalendarEvents(ctx, uuidToPgUUID(sourceID))
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
	sourceUUID := uuidToPgUUID(req.SourceContactID)
	targetUUID := uuidToPgUUID(req.TargetContactID)

	// 1. Delete duplicate contact methods (same normalized value and type)
	if err := txQueries.DeleteDuplicateContactMethods(ctx, db.DeleteDuplicateContactMethodsParams{
		SourceContactID: sourceUUID,
		TargetContactID: targetUUID,
	}); err != nil {
		return nil, fmt.Errorf("delete duplicate contact methods: %w", err)
	}

	// 1b. Demote source's primary methods when target already has primaries for those types
	// This prevents violation of the unique partial index on (contact_id, type) WHERE is_primary = true
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
	notepadCategory := pgtype.Text{String: string(repository.NoteCategoryNotepad), Valid: true}
	sourceNotepad, err := txQueries.GetContactNoteByCategory(ctx, db.GetContactNoteByCategoryParams{
		ContactID: sourceUUID,
		Category:  notepadCategory,
	})
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("get source notepad: %w", err)
	}

	if sourceNotepad != nil {
		// Source has a notepad - need to handle it
		targetNotepad, err := txQueries.GetContactNoteByCategory(ctx, db.GetContactNoteByCategoryParams{
			ContactID: targetUUID,
			Category:  notepadCategory,
		})
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("get target notepad: %w", err)
		}

		// Delete source notepad first (so TransferNotes won't create duplicate)
		if err := txQueries.DeleteContactNoteByCategory(ctx, db.DeleteContactNoteByCategoryParams{
			ContactID: sourceUUID,
			Category:  notepadCategory,
		}); err != nil {
			return nil, fmt.Errorf("delete source notepad: %w", err)
		}

		// Determine combined content
		var combinedBody string
		if targetNotepad != nil && targetNotepad.Body != "" {
			// Both have notepads - combine with separator
			combinedBody = targetNotepad.Body + "\n\n---\n\n" + sourceNotepad.Body
		} else {
			// Only source has notepad
			combinedBody = sourceNotepad.Body
		}

		// Upsert combined content to target
		if _, err := txQueries.UpsertContactNoteByCategory(ctx, db.UpsertContactNoteByCategoryParams{
			ContactID: targetUUID,
			Body:      combinedBody,
			Category:  notepadCategory,
		}); err != nil {
			return nil, fmt.Errorf("upsert merged notepad: %w", err)
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

	// 5. Delete connections between source and target (would be self-referential after merge)
	if err := txQueries.DeleteDuplicateConnections(ctx, db.DeleteDuplicateConnectionsParams{
		SourceContactID: sourceUUID,
		TargetContactID: targetUUID,
	}); err != nil {
		return nil, fmt.Errorf("delete duplicate connections: %w", err)
	}

	// 5b. Delete source's connections to third parties that target already connects to
	// This prevents duplicate rows after transfer when both connect to the same person
	if err := txQueries.DeleteDuplicateThirdPartyConnectionsA(ctx, db.DeleteDuplicateThirdPartyConnectionsAParams{
		SourceContactID: sourceUUID,
		TargetContactID: targetUUID,
	}); err != nil {
		return nil, fmt.Errorf("delete duplicate third party connections (contact_a): %w", err)
	}

	if err := txQueries.DeleteDuplicateThirdPartyConnectionsB(ctx, db.DeleteDuplicateThirdPartyConnectionsBParams{
		SourceContactID: sourceUUID,
		TargetContactID: targetUUID,
	}); err != nil {
		return nil, fmt.Errorf("delete duplicate third party connections (contact_b): %w", err)
	}

	// 6. Transfer connections (both directions)
	if err := txQueries.TransferConnectionsAsContactA(ctx, db.TransferConnectionsAsContactAParams{
		SourceContactID: sourceUUID,
		TargetContactID: targetUUID,
	}); err != nil {
		return nil, fmt.Errorf("transfer connections as contact_a: %w", err)
	}

	if err := txQueries.TransferConnectionsAsContactB(ctx, db.TransferConnectionsAsContactBParams{
		SourceContactID: sourceUUID,
		TargetContactID: targetUUID,
	}); err != nil {
		return nil, fmt.Errorf("transfer connections as contact_b: %w", err)
	}

	// 7. Update calendar events
	if err := txQueries.ReplaceContactInCalendarEvents(ctx, db.ReplaceContactInCalendarEventsParams{
		SourceContactID: sourceUUID,
		TargetContactID: targetUUID,
	}); err != nil {
		return nil, fmt.Errorf("replace contact in calendar events: %w", err)
	}

	// 8. Deduplicate calendar event contact arrays
	if err := txQueries.DeduplicateCalendarEventContacts(ctx, targetUUID); err != nil {
		return nil, fmt.Errorf("deduplicate calendar event contacts: %w", err)
	}

	// 9. Update target contact with field selections and optional new
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

	// 9a. Forward-max merged cadence columns through
	// CadenceUpdater.BulkApply. The merged cadence string may come from
	// the source (when FieldSelections.Cadence == "source"), so
	// contact_by is derived from the CHOSEN cadence value — not from
	// the pre-merge target.cadence, which might have been overwritten
	// above.
	if s.cadence == nil {
		return nil, errors.New("merge contacts: cadence updater not wired (call SetCadenceUpdater)")
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

	// 10. Soft delete source contact
	if err := txQueries.SoftDeleteContact(ctx, sourceUUID); err != nil {
		return nil, fmt.Errorf("soft delete source contact: %w", err)
	}

	// Commit transaction
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}

	// Attach methods to the merged contact
	if err := s.attachMethods(ctx, mergedContact); err != nil {
		return nil, err
	}

	return mergedContact, nil
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

// Helper to convert uuid to pgtype.UUID (used in merge operations)
func uuidToPgUUID(id uuid.UUID) pgtype.UUID {
	return pgtype.UUID{Bytes: id, Valid: true}
}
