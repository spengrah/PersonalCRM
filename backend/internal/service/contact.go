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

// followUpManager manages follow-up Todoist tasks for outbound interactions
type followUpManager interface {
	CreateOrRefreshFollowUp(ctx context.Context, contact repository.Contact, outreachAt time.Time) error
	CompleteFollowUp(ctx context.Context, contactID uuid.UUID) error
}

// cadenceShadowObserver records direct-path cadence observations for the
// PR 7 shadow-mode bake. Kept as a single-method interface so tests can
// stub it without the full repo. Production wiring injects
// *repository.CadenceShadowObservationRepository (RecordDirect matches
// the signature).
//
// The tx argument is typically nil — the direct-path observer calls this
// from a post-commit closure that runs OUTSIDE the caller's authoritative
// tx (plan Decision 4). Passing tx=nil signals the repo to open its own
// short-lived tx on the configured pool.
type cadenceShadowObserver interface {
	RecordDirect(ctx context.Context, tx pgx.Tx, obs repository.CadenceShadowObservation) error
}

type ContactService struct {
	database          *db.Database
	contactRepo       *repository.ContactRepository
	contactMethodRepo *repository.ContactMethodRepository
	interactionRepo   *repository.InteractionRepository
	contactTaskRepo   *repository.ContactTaskRepository
	followUpMgr       followUpManager
	rematchSvc        *RematchService
	// cadenceShadow is the PR 7 direct-path observer for cadence-column
	// shadow writes. Nil in default builds / when
	// EVENT_BUS_CADENCE_MODE=off. When non-nil, applyInteractionEffectsFromRow
	// queues a post-commit closure that captures the post-image and records
	// an observation via the own-tx repo method. See plan Decision 6.
	cadenceShadow cadenceShadowObserver
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

// SetFollowUpManager injects the follow-up manager after construction (resolves circular dependency)
func (s *ContactService) SetFollowUpManager(fm followUpManager) {
	s.followUpMgr = fm
}

// SetRematchService injects the rematch service. Safe to leave unset — CreateContact
// and UpdateContact return uuid.Nil as the jobID when nil.
func (s *ContactService) SetRematchService(r *RematchService) {
	s.rematchSvc = r
}

// SetCadenceShadowObserver wires the PR 7 direct-path cadence shadow
// observer. Nil-safe: when not set, applyInteractionEffectsFromRow
// short-circuits the shadow-capture branch. Called from main.go when
// EVENT_BUS_CADENCE_MODE is shadow or cutover (post-PR-7).
func (s *ContactService) SetCadenceShadowObserver(obs cadenceShadowObserver) {
	s.cadenceShadow = obs
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

	// Calculate contact_by based on cadence change
	// If cadence is cleared, contact_by should be NULL
	// If cadence is set/changed, recompute contact_by from last_contacted || created_at
	if req.Cadence == nil || *req.Cadence == "" {
		// Cadence is being cleared - set contact_by to NULL
		req.ContactBy = nil
	} else {
		// Cadence is being set/changed - compute contact_by
		if cadenceType, err := cadence.ParseCadence(*req.Cadence); err == nil {
			base := existingContact.CreatedAt
			if existingContact.LastContacted != nil {
				base = *existingContact.LastContacted
			}
			contactByTime := cadence.CalculateContactBy(base, cadenceType)
			req.ContactBy = &contactByTime
		}
	}

	contact, err = contactRepo.UpdateContact(ctx, id, req)
	if err != nil {
		return nil, uuid.Nil, err
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
	var (
		interaction *repository.Interaction
		postCommit  func(context.Context)
	)
	err := pgx.BeginTxFunc(ctx, s.database.Pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		var txErr error
		// Non-event-bus wrapper: no paired interaction.recorded event
		// will be published, so withShadow=false — shadow drain is
		// skipped entirely.
		interaction, _, _, _, postCommit, _, txErr = s.RecordInteractionTx(ctx, tx, false, req)
		return txErr
	})
	if err != nil {
		return nil, err
	}
	if postCommit != nil {
		postCommit(ctx)
	}
	return interaction, nil
}

// RecordInteractionTx is the tx-threaded variant of RecordInteraction. The
// caller owns the tx (river worker's BeginTxFunc, manual handler's
// BeginTxFunc). Dedup, contact existence check, interaction insert, and
// cadence UPDATEs all run inside the caller's tx (spec §3.4.1).
//
// The `withShadow` flag tells the service whether the caller intends to
// publish an interaction.recorded event after commit and therefore wants
// the PR 7 direct-path cadence shadow drain queued. Event-bus consumer
// callers pass true; the non-tx RecordInteraction wrapper passes false.
// Plan Decisions 4 + 6.
//
// Returns:
//   - interaction:    the persisted row (either freshly-inserted OR the
//     existing dedup-hit row).
//   - isReplay:       true if FindBySourceRefTx / FindInWindowTx returned
//     an existing row; false on fresh insert. Consumers branch on this
//     to skip interaction.recorded emit on replay (spec §3.4.1).
//   - prevCadence:    pre-cadence snapshot captured in-memory from the
//     contact row BEFORE the authoritative UPDATE. Non-nil on fresh
//     writes when `withShadow` is true (so InteractionRecorder can
//     populate V2 InteractionRecordedPayload.PrevCadenceSnapshot per
//     plan Decision 2a). Nil on replay and when withShadow=false.
//   - cadenceAtEmit:  value of contact.cadence at capture time (may be
//     nil if the contact has no cadence). Non-nil only when
//     withShadow=true and cadence is set. Populates PrevCadenceValue.
//   - followUpFn:     follow-up-manager closure on fresh writes with a
//     configured manager. Nil otherwise. Caller invokes AFTER tx commit.
//   - shadowDrainFn:  PR 7 cadence shadow observation drain. Non-nil on
//     fresh writes when the shadow observer is wired. Caller invokes
//     AFTER the interaction.recorded event is published, passing the
//     recordedEnv.ID so direct and consumer observations share a
//     matching event_id (plan Decision 6, fix for the JOIN key).
//   - err:            wrapped error. Caller should rollback tx.
func (s *ContactService) RecordInteractionTx(
	ctx context.Context, tx pgx.Tx, withShadow bool, req repository.RecordInteractionRequest,
) (*repository.Interaction, bool, *repository.ContactCadenceFields, *string, func(context.Context), repository.CadenceShadowDrainFn, error) {
	// 1. Default direction to "mutual" if empty (backward compat).
	if req.Direction == "" {
		req.Direction = repository.InteractionDirectionMutual
	}

	// 2. Source-aware deduplication (tx-aware variants).
	if req.SourceRef != nil {
		existing, err := s.interactionRepo.FindBySourceRefTx(ctx, tx, req.ContactID, req.Source, *req.SourceRef)
		if err != nil && !errors.Is(err, db.ErrNotFound) {
			return nil, false, nil, nil, nil, nil, fmt.Errorf("check existing interaction by source_ref: %w", err)
		}
		if existing != nil {
			logger.Debug().
				Str("contactId", req.ContactID.String()).
				Str("source", req.Source).
				Str("sourceRef", *req.SourceRef).
				Msg("skipping duplicate interaction (same source_ref)")
			return existing, true, nil, nil, nil, nil, nil
		}
	} else {
		existing, err := s.interactionRepo.FindInWindowTx(ctx, tx, req.ContactID, req.Source, req.OccurredAt, 30*time.Minute)
		if err != nil && !errors.Is(err, db.ErrNotFound) {
			return nil, false, nil, nil, nil, nil, fmt.Errorf("check existing interaction in window: %w", err)
		}
		if existing != nil {
			logger.Debug().
				Str("contactId", req.ContactID.String()).
				Str("existingSource", existing.Source).
				Str("newSource", req.Source).
				Msg("skipping duplicate interaction within 30-min window")
			return existing, true, nil, nil, nil, nil, nil
		}
	}

	// 3. Verify contact exists (avoids FK violation returning unhelpful error).
	contact, err := s.contactRepo.GetContactTx(ctx, tx, req.ContactID)
	if err != nil {
		return nil, false, nil, nil, nil, nil, err // propagates db.ErrNotFound
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
		return nil, false, nil, nil, nil, nil, fmt.Errorf("create interaction: %w", err)
	}

	// 6. Direction-conditional cadence writes + shadow observation drain
	// queue. shadowQueue is a per-call local slice (thread-safe because
	// each RecordInteractionTx invocation has its own slice). We only
	// pass the slice pointer when the caller wants shadow tracking —
	// otherwise the helper short-circuits.
	var shadowQueue []repository.CadenceShadowDrainFn
	var queuePtr *[]repository.CadenceShadowDrainFn
	var prevPtr *repository.ContactCadenceFields
	if withShadow {
		shadowQueue = make([]repository.CadenceShadowDrainFn, 0, 1)
		queuePtr = &shadowQueue
		prevPtr = &prevSnap
	}
	followUpFn := s.applyInteractionEffectsFromRow(ctx, tx, contact, interaction, queuePtr, prevPtr)

	// 7. Build the caller-facing drain. Combine all queued shadow closures
	// into a single repository.CadenceShadowDrainFn so the caller passes eventID
	// once and every observation in the queue inherits it.
	shadowDrainFn := combineShadowDrain(shadowQueue)

	if !withShadow {
		return interaction, false, nil, nil, followUpFn, nil, nil
	}
	return interaction, false, &prevSnap, cadenceAtEmit, followUpFn, shadowDrainFn, nil
}

// combineShadowDrain collapses a slice of repository.CadenceShadowDrainFn closures
// into a single function that drains all of them with the same eventID.
// Returns nil when the slice is empty. Each closure runs under defer-
// recover so one panic doesn't strand the others.
func combineShadowDrain(queue []repository.CadenceShadowDrainFn) repository.CadenceShadowDrainFn {
	if len(queue) == 0 {
		return nil
	}
	return func(ctx context.Context, eventID uuid.UUID) {
		for _, fn := range queue {
			runShadowWithRecover(ctx, fn, eventID, "cadence shadow post-commit")
		}
	}
}

// runShadowWithRecover invokes fn and recovers from any panic so the
// caller can continue with the next closure.
func runShadowWithRecover(ctx context.Context, fn repository.CadenceShadowDrainFn, eventID uuid.UUID, label string) {
	defer func() {
		if r := recover(); r != nil {
			logger.Error().
				Interface("panic", r).
				Str("label", label).
				Str("event_id", eventID.String()).
				Msg("shadow drain closure panicked; recovering to continue queue drain")
		}
	}()
	fn(ctx, eventID)
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
	updated, err := s.updateInteractionDirectionTx(ctx, tx, interactionID, repository.InteractionDirectionMutual, replyAt)
	if err != nil {
		return nil, fmt.Errorf("update interaction direction: %w", err)
	}
	contact, err := s.contactRepo.GetContactTx(ctx, tx, contactID)
	if err != nil {
		return nil, fmt.Errorf("get contact for promotion: %w", err)
	}
	// Promote does not create a new interaction.recorded event — no
	// paired eventID and therefore no shadow observation (plan Decision
	// 6a). Pass nil for shadowQueue + prev; the helper's shadow-capture
	// branch short-circuits on nil.
	return s.applyInteractionEffectsFromRow(ctx, tx, contact, updated, nil, nil), nil
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
	// write. applyInteractionEffectsFromRow reads the persisted row's
	// Direction, which for same-direction coalescing equals the caller-
	// supplied direction. Guard against surprises by overriding the row's
	// Direction in-memory with the caller's intent before applying effects.
	updated.Direction = direction
	// Extend does not create a new interaction.recorded event — no
	// paired eventID and therefore no shadow observation (plan Decision
	// 6a).
	return s.applyInteractionEffectsFromRow(ctx, tx, contact, updated, nil, nil), nil
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

// applyInteractionEffectsFromRow handles direction-conditional contact field
// updates (in-tx) and captures follow-up-manager work in a returned closure
// for the caller to fire AFTER the tx commits (plan Decisions 4 + 8).
//
// The helper reads `Direction`, `OccurredAt`, and `Source` from the persisted
// `interaction` row — not from in-flight request args — per spec §5 PR 6
// line 845. This eliminates drift between request-time values and persisted-
// row values on dedup-hit replays and out-of-order updates.
//
// PR 7 extension: eventID + shadowQueue + prev support the direct-path
// cadence shadow observer (plan Decision 6). When all three are non-nil
// AND the observer is wired, the helper queues a post-commit closure
// that opens its OWN short-lived tx, SELECTs the post-image, and writes
// a writer='direct' shadow observation. Callers without a paired event
// (ExtendInteraction / PromoteInteractionToMutual) pass nil for all
// three and the shadow-capture branch short-circuits.
//
// Returns a nil-safe post-commit closure (the follow-up manager call).
// Nil means no follow-up work is warranted. Non-nil closures capture
// the contact, direction, and occurredAt by value so they're stable
// across tx commit.
func (s *ContactService) applyInteractionEffectsFromRow(
	ctx context.Context, tx pgx.Tx,
	contact *repository.Contact, interaction *repository.Interaction,
	shadowQueue *[]repository.CadenceShadowDrainFn, prev *repository.ContactCadenceFields,
) func(context.Context) {
	if contact == nil || interaction == nil {
		return nil
	}
	contactID := contact.ID
	direction := interaction.Direction
	occurredAt := interaction.OccurredAt
	isManual := interaction.Source == repository.InteractionSourceManual

	// applyContactBy tracks the authoritative decision — whether the
	// direct path actually recomputed contact_by for this write.
	// Combines the direction-rule permission (outbound never touches
	// contact_by) with the time-gate (prev.LastContacted vs occurredAt)
	// and the cadence-set check. Consumer replays this bit-for-bit.
	_, _, _, directionAllowsContactBy := repository.CadenceApplyFlagsByDirection(direction)
	applyContactBy := directionAllowsContactBy && repository.ShouldApplyContactBy(
		contact.LastContacted, occurredAt, isManual,
		contact.Cadence != nil && *contact.Cadence != "",
	)
	var nextContactBy *time.Time
	if applyContactBy && contact.Cadence != nil && *contact.Cadence != "" {
		if cadenceType, err := cadence.ParseCadence(*contact.Cadence); err == nil {
			t := cadence.CalculateContactBy(occurredAt, cadenceType)
			nextContactBy = &t
		} else {
			// Parse failure = no contact_by update; correct applyContactBy
			// so the shadow observation matches direct's effective behavior.
			applyContactBy = false
		}
	}

	switch direction {
	case repository.InteractionDirectionOutbound:
		if err := s.contactRepo.UpdateContactOutreachAtTx(ctx, tx, contactID, occurredAt, isManual); err != nil {
			logger.Warn().Err(err).
				Str("contactId", contactID.String()).
				Msg("failed to update last_outreach_at from outbound interaction")
		}

	case repository.InteractionDirectionInbound:
		if err := s.contactRepo.UpdateContactResponseFieldsTx(ctx, tx, contactID, occurredAt, nextContactBy, isManual); err != nil {
			logger.Warn().Err(err).
				Str("contactId", contactID.String()).
				Msg("failed to update contact fields from inbound interaction")
		}

	default: // mutual
		if err := s.contactRepo.UpdateContactMutualFieldsTx(ctx, tx, contactID, occurredAt, nextContactBy, isManual); err != nil {
			logger.Warn().Err(err).
				Str("contactId", contactID.String()).
				Msg("failed to update contact fields from mutual interaction")
		}
	}

	// PR 7 shadow-capture branch. Only fires when the observer is wired
	// AND the caller supplied a paired shadowQueue + prev
	// (RecordInteractionTx with an event-bus envelope). Extend/Promote
	// pass nil and short-circuit here — documented coverage gap in plan
	// Decision 6a + acceptance scope. The eventID is NOT bound here:
	// it will be supplied by the caller (InteractionRecorder) at drain
	// time, AFTER bus.PublishTx populates the interaction.recorded
	// envelope's ID — that ID is what both direct and consumer
	// observations must share for the post-bake FULL OUTER JOIN to
	// resolve.
	if s.cadenceShadow != nil && shadowQueue != nil && prev != nil {
		s.queueCadenceShadowObservation(
			contactID, interaction.Source, direction, occurredAt, isManual,
			*prev, applyContactBy, shadowQueue,
		)
	}

	// Capture follow-up work for post-commit invocation. The closure
	// snapshots contact-by-value so it's stable if the caller mutates the
	// contact pointer later.
	if s.followUpMgr == nil {
		return nil
	}
	switch direction {
	case repository.InteractionDirectionOutbound, repository.InteractionDirectionInbound, repository.InteractionDirectionMutual:
		contactSnapshot := *contact
		fm := s.followUpMgr
		dir := direction
		occ := occurredAt
		return func(postCtx context.Context) {
			switch dir {
			case repository.InteractionDirectionOutbound:
				if err := fm.CreateOrRefreshFollowUp(postCtx, contactSnapshot, occ); err != nil {
					logger.Warn().Err(err).
						Str("contactId", contactID.String()).
						Str("direction", dir).
						Msg("failed to create/refresh follow-up task")
				}
			case repository.InteractionDirectionInbound, repository.InteractionDirectionMutual:
				if err := fm.CompleteFollowUp(postCtx, contactID); err != nil {
					logger.Warn().Err(err).
						Str("contactId", contactID.String()).
						Str("direction", dir).
						Msg("failed to complete follow-up task")
				}
			}
		}
	}
	return nil
}

// queueCadenceShadowObservation appends a drain closure to shadowQueue
// that writes a direct-path cadence shadow observation. The closure
// runs OUTSIDE the caller's authoritative tx — it opens its own short-
// lived tx via the repo (plan Decision 4). All captured state is by
// value; the eventID is bound at drain time by the caller.
//
// Observation assembly replays the per-column apply-flag matrix (from
// direction rules) so the shadow table records exactly what direct
// path would have written. The consumer's closure (in the worker tx)
// uses the SAME flags — byte-for-byte parity is what powers the
// post-bake divergence query.
func (s *ContactService) queueCadenceShadowObservation(
	contactID uuid.UUID, source, direction string, occurredAt time.Time, isManual bool,
	prev repository.ContactCadenceFields, applyContactBy bool,
	shadowQueue *[]repository.CadenceShadowDrainFn,
) {
	branch := repository.CadenceShadowBranchForward
	if isManual {
		branch = repository.CadenceShadowBranchUnconditional
	}
	applyLastContacted, applyLastOutreachAt, applyLastResponseAt, _ := repository.CadenceApplyFlagsByDirection(direction)

	// Capture values by copy — no pointer aliasing into the closure.
	contactIDCopy := contactID
	sourceCopy := source
	directionCopy := direction
	occurredAtCopy := occurredAt
	prevCopy := prev
	applyContactByCopy := applyContactBy
	observer := s.cadenceShadow

	*shadowQueue = append(*shadowQueue, func(postCtx context.Context, eventID uuid.UUID) {
		if observer == nil {
			return
		}
		post, err := s.contactRepo.SnapshotContactCadenceFields(postCtx, contactIDCopy)
		if errors.Is(err, db.ErrNotFound) {
			logger.Debug().
				Str("event_id", eventID.String()).
				Str("contact_id", contactIDCopy.String()).
				Msg("shadow: contact soft-deleted before direct-path observation; skipping")
			return
		}
		if err != nil {
			logger.Error().Err(err).
				Str("event_id", eventID.String()).
				Str("contact_id", contactIDCopy.String()).
				Msg("shadow: direct-path post-image snapshot failed")
			return
		}
		// Build next-image respecting apply flags (nil for apply-false columns).
		obs := repository.CadenceShadowObservation{
			EventID:             eventID,
			ContactID:           contactIDCopy,
			Source:              sourceCopy,
			Direction:           directionCopy,
			Branch:              branch,
			OccurredAt:          occurredAtCopy,
			PrevLastContacted:   prevCopy.LastContacted,
			PrevLastOutreachAt:  prevCopy.LastOutreachAt,
			PrevLastResponseAt:  prevCopy.LastResponseAt,
			PrevContactBy:       prevCopy.ContactBy,
			ApplyLastContacted:  applyLastContacted,
			ApplyLastOutreachAt: applyLastOutreachAt,
			ApplyLastResponseAt: applyLastResponseAt,
			ApplyContactBy:      applyContactByCopy,
		}
		// Next values come from the live post-image re-read. Set nil for
		// apply-flag-false columns so the shadow row encodes "direct did
		// not touch this column" rather than capturing the unchanged post-
		// value.
		if applyLastContacted {
			obs.NextLastContacted = post.LastContacted
		}
		if applyLastOutreachAt {
			obs.NextLastOutreachAt = post.LastOutreachAt
		}
		if applyLastResponseAt {
			obs.NextLastResponseAt = post.LastResponseAt
		}
		if applyContactByCopy {
			// When applyContactBy is true direct should have set contact_by
			// (per the forward-only SQL). Use the re-read value to match
			// actual DB state.
			obs.NextContactBy = post.ContactBy
		}
		if err := observer.RecordDirect(postCtx, nil /*own-tx*/, obs); err != nil {
			logger.Error().Err(err).
				Str("event_id", eventID.String()).
				Str("contact_id", contactIDCopy.String()).
				Msg("shadow: direct-path observation insert failed")
		}
	})
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

	// 9. Update target contact with field selections and optional new name
	txContactRepo := repository.NewContactRepository(txQueries)
	updateReq := buildMergeUpdateRequest(targetContact, sourceContact, req)
	mergedContact, err = txContactRepo.UpdateContact(ctx, req.TargetContactID, updateReq)
	if err != nil {
		return nil, fmt.Errorf("update target contact: %w", err)
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

// Helper to convert uuid to pgtype.UUID (used in merge operations)
func uuidToPgUUID(id uuid.UUID) pgtype.UUID {
	return pgtype.UUID{Bytes: id, Valid: true}
}
