package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"personal-crm/backend/internal/cadence"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/events"
	"personal-crm/backend/internal/identity"
	"personal-crm/backend/internal/logger"
	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// MethodSelection represents a user-selected method for enrichment
type MethodSelection struct {
	OriginalValue string
	Type          string
	IsPrimary     bool
}

// EnrichmentService handles contact enrichment from external sources.
// When the link/import flow supplies an explicit cadence override, the
// cadence mutation routes through CadenceUpdater.ApplyContactByOverride
// so contact cadence writes stay under the sole-writer invariant.
// Inferred-enrichment paths (no cadence) continue to use the profile-
// only UpdateContact query and never touch cadence columns.
type EnrichmentService struct {
	database        *db.Database
	contactRepo     *repository.ContactRepository
	methodRepo      *repository.ContactMethodRepository
	enrichmentRepo  *repository.EnrichmentRepository
	bus             *events.Bus
	rematchRegistry RematchRegistry
	cadence         cadenceWriter
	// knowledge persists inferred location/birthday/how_met as assertions and
	// refreshes the derived cache columns inline. Post-cutover the contact SQL
	// no longer writes those columns, so an enrichment that infers them MUST
	// route through this writer or the value silently vanishes. Injected via
	// SetKnowledgeWriter; when unset, an enrichment that would set one of those
	// three fields returns an error rather than dropping it.
	knowledge *knowledgeWriter
}

// NewEnrichmentService creates a new enrichment service. database is
// required post-cutover so the cadence-override path can open its own
// tx for the profile-update + ApplyContactByOverride pair. bus +
// rematchRegistry are required for the event-bus rematch path:
// EnrichContactFromExternal* publish contact_methods.added through bus
// and seed the in-memory job entry via rematchRegistry. Tests that
// don't exercise rematch may pass nil for both — the publisher silently
// skips when bus is nil.
func NewEnrichmentService(
	database *db.Database,
	contactRepo *repository.ContactRepository,
	methodRepo *repository.ContactMethodRepository,
	enrichmentRepo *repository.EnrichmentRepository,
	bus *events.Bus,
	rematchRegistry RematchRegistry,
) *EnrichmentService {
	return &EnrichmentService{
		database:        database,
		contactRepo:     contactRepo,
		methodRepo:      methodRepo,
		enrichmentRepo:  enrichmentRepo,
		bus:             bus,
		rematchRegistry: rematchRegistry,
	}
}

// SetCadenceUpdater injects the cadence writer. Required for the
// cadence-present link/import override path. When unset, cadence-
// present enrichment calls return an error rather than silently
// skipping the sole-writer invariant.
func (s *EnrichmentService) SetCadenceUpdater(c cadenceWriter) {
	s.cadence = c
}

// SetKnowledgeWriter injects the assertion-store knowledge writer. Required for
// any enrichment that infers location/birthday/how_met (those columns are no
// longer written by the contact SQL). Inferred enrichment is stamped with agent
// provenance (producer=agent, source_kind=agent_session) since it is derived
// from external contact data, not a direct user edit.
func (s *EnrichmentService) SetKnowledgeWriter(assertSvc *AssertService, cache knowledgeCacheRefresher) {
	s.knowledge = newKnowledgeWriter(assertSvc, cache)
}

// InjectBusForTest swaps the event bus reference after construction.
// Integration tests have a chicken-and-egg dependency where the bus
// needs the ContactService (via InteractionRecorder) AND the services
// need the bus. Production main.go resolves this by reordering
// construction; tests call this post-construction. Must only be called
// before the service is used concurrently.
func (s *EnrichmentService) InjectBusForTest(bus *events.Bus) {
	s.bus = bus
}

// EnrichContactFromExternal enriches a CRM contact with data from an external contact.
// Only fills in missing fields - never overwrites existing data. Returns a rematch
// job ID (uuid.Nil when no handlers match any newly-added method).
func (s *EnrichmentService) EnrichContactFromExternal(
	ctx context.Context,
	crmContactID uuid.UUID,
	external *repository.ExternalContact,
) (uuid.UUID, error) {
	// Get current contact
	contact, err := s.contactRepo.GetContact(ctx, crmContactID)
	if err != nil {
		return uuid.Nil, err
	}

	// Track what needs updating
	needsUpdate := false
	updateReq := repository.UpdateContactRequest{
		FullName:     contact.FullName,
		Location:     contact.Location,
		Birthday:     contact.Birthday,
		HowMet:       contact.HowMet,
		Cadence:      contact.Cadence,
		ProfilePhoto: contact.ProfilePhoto,
	}

	// inferred holds the location/birthday/how_met values this enrichment newly
	// derives. Post-cutover those columns are NOT written by the contact SQL —
	// they flow from the assertion store — so each inferred field becomes an
	// agent-provenance assertion below. Fields the contact already has stay nil
	// here (enrichment only fills empty fields), so no spurious assertion fires.
	var inferred knowledgeFieldValues

	// Enrich profile photo if CRM contact has none
	if contact.ProfilePhoto == nil && external.PhotoURL != nil && *external.PhotoURL != "" {
		updateReq.ProfilePhoto = external.PhotoURL
		needsUpdate = true
		s.recordEnrichment(ctx, crmContactID, external, "profile_photo", *external.PhotoURL)
	}

	// Enrich birthday if CRM contact has none
	if contact.Birthday == nil && external.Birthday != nil {
		updateReq.Birthday = external.Birthday
		inferred.Birthday = external.Birthday
		needsUpdate = true
		s.recordEnrichment(ctx, crmContactID, external, "birthday", external.Birthday.Format("2006-01-02"))
	}

	// Enrich location from addresses if CRM contact has none
	if contact.Location == nil && len(external.Addresses) > 0 && external.Addresses[0].Formatted != "" {
		location := external.Addresses[0].Formatted
		updateReq.Location = &location
		inferred.Location = &location
		needsUpdate = true
		s.recordEnrichment(ctx, crmContactID, external, "location", location)
	}

	if (inferred.Location != nil || inferred.Birthday != nil) && s.knowledge == nil {
		return uuid.Nil, errors.New("enrichment: inferred location/birthday but knowledge writer not wired")
	}

	// Apply updates to contact if any enrichment occurred. This path never
	// renames (no name input — updateReq.FullName stays == contact.FullName),
	// but UpdateContact still writes full_name, so the node label sync rides the
	// same tx and is UNCONDITIONAL — keeping contact and node in lockstep even
	// if the written name clobbers a concurrent rename.
	if needsUpdate {
		txErr := pgx.BeginTxFunc(ctx, s.database.Pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
			txQueries := db.New(tx)
			if _, txErr := repository.NewContactRepository(txQueries).UpdateContact(ctx, crmContactID, updateReq); txErr != nil {
				return fmt.Errorf("update contact profile: %w", txErr)
			}
			if txErr := s.assertInferredKnowledge(ctx, tx, crmContactID, inferred); txErr != nil {
				return txErr
			}
			return repository.NewNodeRepository(txQueries).UpdateNodeCanonicalLabelTx(ctx, tx, crmContactID, updateReq.FullName)
		})
		if txErr != nil {
			// When the enrichment inferred a location/birthday, a failed tx means
			// that value was dropped — the cache columns are no longer written by
			// the profile UPDATE, so the assertion store is the ONLY home for it.
			// Surface that as an error instead of logging-and-continuing (which
			// would report enrichment success while silently losing the field).
			// A photo/cadence-only enrichment (no inferred knowledge) keeps the
			// historical best-effort warn so an unrelated profile-write hiccup
			// doesn't fail the whole link/import.
			if inferred.Location != nil || inferred.Birthday != nil {
				return uuid.Nil, fmt.Errorf("apply inferred contact knowledge: %w", txErr)
			}
			logger.Warn().Err(txErr).Msg("failed to update contact with enrichments")
		}
	}

	// Method enrichment + per-method audit inserts + contact_methods.added
	// publish all share a single tx for atomicity. Propagating the tx
	// error is required — silent swallow would let the caller see a
	// uuid.Nil jobID while the rolled-back method rows leave the contact
	// unchanged.
	jobID, addedMethods, err := s.enrichMethodsAndPublish(ctx, contact, external, "")
	if err != nil {
		return uuid.Nil, fmt.Errorf("enrich contact methods: %w", err)
	}
	if jobID != uuid.Nil && s.rematchRegistry != nil {
		s.rematchRegistry.RegisterPending(jobID, crmContactID, addedMethods)
	}
	return jobID, nil
}

// enrichMethodsAndPublish wraps the method-enrichment loop, per-method
// audit inserts, and the contact_methods.added event publish in a
// single pgx.Tx so they commit or roll back together. Returns a
// non-Nil jobID iff at least one method was added and s.bus is wired;
// otherwise jobID=uuid.Nil. When selectedMethods is non-empty (link/
// import WithSelections flow), the method-loop uses user selections;
// otherwise the full BuildMethodsFromExternal set is enriched.
//
// The caller registers the in-memory pending entry via
// rematchRegistry.RegisterPending post-commit.
func (s *EnrichmentService) enrichMethodsAndPublish(
	ctx context.Context,
	contact *repository.Contact,
	external *repository.ExternalContact,
	source string,
) (uuid.UUID, []Method, error) {
	var (
		added []Method
		jobID uuid.UUID
	)
	effectiveSource := source
	if effectiveSource == "" {
		// Default source for the EnrichContactFromExternal path matches
		// the external contact's provenance (gcontacts, telegram, etc.).
		effectiveSource = external.Source
	}
	var eligible []Method
	txErr := pgx.BeginTxFunc(ctx, s.database.Pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		txQueries := db.New(tx)
		txMethodRepo := repository.NewContactMethodRepository(txQueries)
		txEnrichmentRepo := repository.NewEnrichmentRepository(txQueries)

		var err error
		added, err = s.enrichContactMethodsTx(ctx, tx, txMethodRepo, txEnrichmentRepo, contact, external)
		if err != nil {
			return err
		}
		if len(added) == 0 {
			return nil
		}
		// Filter to handler-eligible methods: skip publish when no
		// registered rematch handler matches any added method.
		if s.rematchRegistry != nil {
			eligible = s.rematchRegistry.EligibleMethods(added)
		}
		if len(eligible) == 0 {
			return nil
		}
		if s.bus == nil {
			return nil
		}
		jobID = uuid.New()
		refs := rematchMethodsToRefs(eligible)
		env, err := buildContactMethodsAddedEnvelope(effectiveSource, contact.ID, refs, jobID)
		if err != nil {
			return err
		}
		return s.bus.PublishTx(ctx, tx, env)
	})
	if txErr != nil {
		return uuid.Nil, nil, txErr
	}
	// Caller passes eligible methods into RegisterPending so the
	// in-memory job entry matches what the consumer will run.
	return jobID, eligible, nil
}

// EnrichContactFromExternalWithSelections enriches a CRM contact with user-selected methods.
// Unlike EnrichContactFromExternal, this uses explicit method selections and conflict resolutions.
// If cadence is provided, it will update the contact's cadence.
// If name is provided, it will update the contact's full name.
// Returns a rematch job ID (uuid.Nil when no handlers match any added method).
func (s *EnrichmentService) EnrichContactFromExternalWithSelections(
	ctx context.Context,
	crmContactID uuid.UUID,
	external *repository.ExternalContact,
	selectedMethods []MethodSelection,
	conflictResolutions map[string]string,
	cadenceArg *string,
	name *string,
) (uuid.UUID, error) {
	// Get current contact
	contact, err := s.contactRepo.GetContact(ctx, crmContactID)
	if err != nil {
		return uuid.Nil, err
	}

	// Track what needs updating
	needsUpdate := false
	updateReq := repository.UpdateContactRequest{
		FullName:     contact.FullName,
		Location:     contact.Location,
		Birthday:     contact.Birthday,
		HowMet:       contact.HowMet,
		Cadence:      contact.Cadence,
		ProfilePhoto: contact.ProfilePhoto,
	}

	// Enrich profile photo if CRM contact has none
	if contact.ProfilePhoto == nil && external.PhotoURL != nil && *external.PhotoURL != "" {
		updateReq.ProfilePhoto = external.PhotoURL
		needsUpdate = true
		s.recordEnrichment(ctx, crmContactID, external, "profile_photo", *external.PhotoURL)
	}

	// inferred holds the location/birthday this enrichment newly derives; each
	// becomes an agent-provenance assertion (the cache columns are no longer
	// written by the contact SQL).
	var inferred knowledgeFieldValues

	// Enrich birthday if CRM contact has none
	if contact.Birthday == nil && external.Birthday != nil {
		updateReq.Birthday = external.Birthday
		inferred.Birthday = external.Birthday
		needsUpdate = true
		s.recordEnrichment(ctx, crmContactID, external, "birthday", external.Birthday.Format("2006-01-02"))
	}

	// Enrich location from addresses if CRM contact has none
	if contact.Location == nil && len(external.Addresses) > 0 && external.Addresses[0].Formatted != "" {
		location := external.Addresses[0].Formatted
		updateReq.Location = &location
		inferred.Location = &location
		needsUpdate = true
		s.recordEnrichment(ctx, crmContactID, external, "location", location)
	}

	if (inferred.Location != nil || inferred.Birthday != nil) && s.knowledge == nil {
		return uuid.Nil, errors.New("enrichment: inferred location/birthday but knowledge writer not wired")
	}

	// Update cadence if provided. Explicit cadence overrides from the
	// link/import flow are treated as user-supplied manual cadence
	// edits and must route through CadenceUpdater.ApplyContactByOverride
	// so contact_by recomputation stays under the sole-writer invariant.
	// cadencePresent gates the tx-wrapped path below.
	cadencePresent := cadenceArg != nil
	if cadencePresent {
		updateReq.Cadence = cadenceArg
		needsUpdate = true
	}

	// Update name if provided
	if name != nil && strings.TrimSpace(*name) != "" {
		trimmedName := strings.TrimSpace(*name)
		updateReq.FullName = trimmedName
		needsUpdate = true
	}

	// Apply updates to contact if any enrichment occurred. UpdateContact always
	// writes full_name, so the contact update + the person node label sync
	// (node.id == contact.id) ALWAYS share one tx and the node is synced
	// UNCONDITIONALLY to the written name — gating on a pre-read name compare
	// would let a stale-read enrichment write back an old name while leaving the
	// node on a concurrent rename's value, diverging the two.
	if needsUpdate {
		var newContactBy *time.Time
		if cadencePresent {
			if s.cadence == nil {
				return uuid.Nil, errors.New("enrichment: cadence override requested but CadenceUpdater not wired")
			}
			newContactBy = deriveContactByFromCadence(contact, cadenceArg)
		}
		txErr := pgx.BeginTxFunc(ctx, s.database.Pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
			// Profile-only write inside the tx so the cadence string is visible
			// to ApplyContactByOverride's downstream reads.
			txQueries := db.New(tx)
			if _, txErr := repository.NewContactRepository(txQueries).UpdateContact(ctx, crmContactID, updateReq); txErr != nil {
				return fmt.Errorf("update contact profile: %w", txErr)
			}
			if txErr := s.assertInferredKnowledge(ctx, tx, crmContactID, inferred); txErr != nil {
				return txErr
			}
			if txErr := repository.NewNodeRepository(txQueries).UpdateNodeCanonicalLabelTx(ctx, tx, crmContactID, updateReq.FullName); txErr != nil {
				return fmt.Errorf("sync person node label: %w", txErr)
			}
			if cadencePresent {
				return s.cadence.ApplyContactByOverride(ctx, tx, crmContactID, newContactBy)
			}
			return nil
		})
		if txErr != nil {
			// A failed tx drops an inferred location/birthday (the cache columns
			// are no longer written by the profile UPDATE — the assertion store is
			// the only home). Surface it instead of reporting false success. A
			// photo/cadence/name-only enrichment keeps the historical best-effort
			// warn so an unrelated profile-write hiccup doesn't fail link/import.
			if inferred.Location != nil || inferred.Birthday != nil {
				return uuid.Nil, fmt.Errorf("apply inferred contact knowledge: %w", txErr)
			}
			logger.Warn().Err(txErr).Msg("failed to update contact with enrichments")
		}
	}

	// Enrich contact methods using selections + publish inside a tx so
	// method inserts + audits + event all commit or roll back together
	// (spec §4 atomicity). Propagates tx error to caller — the handler
	// (LinkContact) decides whether to surface as HTTP 500 vs conflict.
	jobID, addedMethods, err := s.enrichMethodsAndPublishWithSelections(
		ctx, contact, external, selectedMethods, conflictResolutions, "manual",
	)
	if err != nil {
		return uuid.Nil, err
	}
	if jobID != uuid.Nil && s.rematchRegistry != nil {
		s.rematchRegistry.RegisterPending(jobID, crmContactID, addedMethods)
	}
	return jobID, nil
}

// enrichMethodsAndPublishWithSelections is the selection-driven
// counterpart to enrichMethodsAndPublish. Wraps the user-selected
// method-insert loop, per-method audit inserts, and event publish in a
// single pgx.Tx (spec §4 atomicity). Source defaults to "manual"
// (user-initiated link/import).
func (s *EnrichmentService) enrichMethodsAndPublishWithSelections(
	ctx context.Context,
	contact *repository.Contact,
	external *repository.ExternalContact,
	selectedMethods []MethodSelection,
	conflictResolutions map[string]string,
	source string,
) (uuid.UUID, []Method, error) {
	var (
		added []Method
		jobID uuid.UUID
	)
	effectiveSource := source
	if effectiveSource == "" {
		effectiveSource = "manual"
	}
	var eligible []Method
	txErr := pgx.BeginTxFunc(ctx, s.database.Pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		txQueries := db.New(tx)
		txMethodRepo := repository.NewContactMethodRepository(txQueries)
		txEnrichmentRepo := repository.NewEnrichmentRepository(txQueries)

		var err error
		added, err = s.enrichContactMethodsWithSelectionsTx(
			ctx, tx, txMethodRepo, txEnrichmentRepo, contact, external, selectedMethods, conflictResolutions,
		)
		if err != nil {
			return err
		}
		if len(added) == 0 {
			return nil
		}
		// Filter to handler-eligible methods: skip publish when no
		// registered rematch handler matches any added method.
		if s.rematchRegistry != nil {
			eligible = s.rematchRegistry.EligibleMethods(added)
		}
		if len(eligible) == 0 {
			return nil
		}
		if s.bus == nil {
			return nil
		}
		jobID = uuid.New()
		refs := rematchMethodsToRefs(eligible)
		env, err := buildContactMethodsAddedEnvelope(effectiveSource, contact.ID, refs, jobID)
		if err != nil {
			return err
		}
		return s.bus.PublishTx(ctx, tx, env)
	})
	if txErr != nil {
		return uuid.Nil, nil, txErr
	}
	return jobID, eligible, nil
}

// enrichContactMethodsWithSelectionsTx adds methods based on user
// selection and conflict resolution inside the caller's tx.
// Per-method audit inserts share the tx so they commit with the
// method rows (spec §4 atomicity).
//
// Each CreateContactMethod call runs inside a nested savepoint so a
// unique-violation (concurrent-insert race) can be rolled back without
// aborting the outer tx (CLAUDE.md gotcha: "pgx.Tx insert hitting a
// unique-violation aborts the outer tx"). On savepoint rollback, the
// caller refetches via the tx-scoped repo to recover the raced row's
// ID for primary-method handling.
func (s *EnrichmentService) enrichContactMethodsWithSelectionsTx(
	ctx context.Context,
	tx pgx.Tx,
	txMethodRepo *repository.ContactMethodRepository,
	txEnrichmentRepo *repository.EnrichmentRepository,
	contact *repository.Contact,
	external *repository.ExternalContact,
	selectedMethods []MethodSelection,
	conflictResolutions map[string]string,
) ([]Method, error) {
	// conflictResolutions is kept for API compatibility; type conflicts are no longer applicable.
	_ = conflictResolutions

	// Get existing methods (tx-scoped)
	existingMethods, err := txMethodRepo.ListContactMethodsByContact(ctx, contact.ID)
	if err != nil {
		return nil, err
	}

	added := make([]Method, 0)

	// Build maps for existing methods, keyed as (type + ":" + normalized).
	// Type-scoped keys mirror the DB unique index (contact_id, type, value_normalized)
	// so cross-type collisions (e.g., telegram:foo vs twitter:foo) stay distinct.
	existingKeys := make(map[string]bool)
	existingMethodByKey := make(map[string]*repository.ContactMethod)
	for i := range existingMethods {
		m := &existingMethods[i]
		key := methodDedupKey(m.Type, m.Value)
		existingKeys[key] = true
		existingMethodByKey[key] = m
	}

	// Build admissible-external lookup via the shared helper, keyed by
	// methodDedupKey. Because the helper canonicalizes telegram handles
	// (stripping leading '@'), both "@handle" and "handle" from the
	// frontend produce the same dedup key and are both admitted.
	externalKeys := make(map[string]bool)
	for _, m := range BuildMethodsFromExternal(external) {
		externalKeys[methodDedupKey(m.Type, m.Value)] = true
	}

	// Collect errors for reporting
	var methodErrors []string

	// Track if any selection has IsPrimary set
	var newPrimaryMethodID *uuid.UUID
	var existingPrimaryMethodID *uuid.UUID

	// Process selected methods
	for _, sel := range selectedMethods {
		selKey := methodDedupKey(sel.Type, sel.OriginalValue)

		// Validate the value exists in external contact (type-scoped normalized)
		if !externalKeys[selKey] {
			methodErrors = append(methodErrors, fmt.Sprintf("value %q not found in external contact", sel.OriginalValue))
			continue
		}

		if existingKeys[selKey] {
			// Method already exists - check if we need to update primary status
			if sel.IsPrimary {
				if existingMethod := existingMethodByKey[selKey]; existingMethod != nil {
					existingPrimaryMethodID = &existingMethod.ID
				}
			}
			continue // Already have this value
		}

		// Canonicalize the stored value to match storage convention across paths:
		//   - telegram/twitter/discord: strip leading '@' and trim whitespace (bare handle)
		//   - email/phone: preserve as-is
		// Mirrors buildMethodsAuto's import-new behavior so link-flow storage
		// is consistent with import-new storage.
		storedValue := canonicalizeMethodValue(sel.Type, sel.OriginalValue)

		// Savepoint-wrapped insert so a unique-violation from a
		// concurrent writer rolls back only the nested tx, leaving the
		// outer tx live for subsequent inserts (CLAUDE.md gotcha).
		newMethod, raced, err := insertContactMethodSavepoint(ctx, tx, repository.CreateContactMethodRequest{
			ContactID: contact.ID,
			Type:      sel.Type,
			Value:     storedValue,
			IsPrimary: false, // We'll update primary status separately
		})
		if err != nil {
			methodErrors = append(methodErrors, fmt.Sprintf("failed to add method %s: %v", storedValue, err))
			continue
		}
		if raced {
			// Concurrent insert won — refetch to recover the row for
			// primary-method handling.
			if sel.IsPrimary {
				racedRows, refetchErr := txMethodRepo.ListContactMethodsByContact(ctx, contact.ID)
				if refetchErr != nil {
					methodErrors = append(methodErrors,
						fmt.Sprintf("recover raced primary for %s: %v", storedValue, refetchErr))
				} else {
					var recovered bool
					for i := range racedRows {
						if methodDedupKey(racedRows[i].Type, racedRows[i].Value) == selKey {
							existingPrimaryMethodID = &racedRows[i].ID
							recovered = true
							break
						}
					}
					if !recovered {
						methodErrors = append(methodErrors,
							fmt.Sprintf("raced row for %s not visible after refetch — primary selection dropped", storedValue))
					}
				}
			}
			existingKeys[selKey] = true
			continue
		}

		// Track if this new method should be primary
		if sel.IsPrimary {
			newPrimaryMethodID = &newMethod.ID
		}

		added = append(added, Method{Type: newMethod.Type, Value: newMethod.ValueNormalized})
		if auditErr := s.recordEnrichmentTx(ctx, txEnrichmentRepo, contact.ID, external,
			"method:"+sel.Type+":"+identity.Normalize(storedValue, mapMethodTypeToIdentifier(sel.Type)),
			storedValue); auditErr != nil {
			return nil, auditErr
		}
		existingKeys[selKey] = true
	}

	// Handle primary method updates - first clear any existing primary, then set the new one.
	// We determine which method should be primary:
	// - If a new method is marked primary, it takes precedence
	// - Otherwise, if an existing method is marked primary, use that
	// This ensures the user's explicit selection is honored.
	primaryMethodID := newPrimaryMethodID
	if primaryMethodID == nil {
		primaryMethodID = existingPrimaryMethodID
	}

	if primaryMethodID != nil {
		// Clear existing primary methods first - fail if this fails to prevent
		// multiple primary methods per contact
		for i := range existingMethods {
			m := &existingMethods[i]
			if m.IsPrimary && m.ID != *primaryMethodID {
				if err := txMethodRepo.SetPrimary(ctx, m.ID, false); err != nil {
					return added, fmt.Errorf("failed to clear existing primary method: %w", err)
				}
			}
		}
		// Set new primary
		if err := txMethodRepo.SetPrimary(ctx, *primaryMethodID, true); err != nil {
			return added, fmt.Errorf("failed to set primary method: %w", err)
		}
	}

	// Per-selection method errors (e.g., "value not found in external
	// contact") are logged but NOT returned as a tx-failing error —
	// returning would roll back successfully-inserted methods and drop
	// the entire link operation. Link/import paths expect partial
	// success: valid selections commit, invalid ones log and skip.
	if len(methodErrors) > 0 {
		logger.Warn().
			Str("contact_id", contact.ID.String()).
			Str("errors", strings.Join(methodErrors, "; ")).
			Msg("enrichment: one or more selected methods could not be inserted; partial success")
	}

	return added, nil
}

// enrichContactMethodsTx adds missing contact methods from the external
// contact inside the caller's tx. Per-method audit inserts share the
// tx so they commit with the method rows (spec §4 atomicity).
//
// Each CreateContactMethod runs inside a nested savepoint — a
// unique-violation from a concurrent writer rolls back only the
// savepoint, leaving the outer tx live for subsequent inserts
// (CLAUDE.md gotcha: pgx.Tx + 23505 aborts the outer tx).
//
// Uses BuildMethodsFromExternal as the single source of truth for
// emitting source-specific methods (emails, phones, telegram
// username). Dedup is type-scoped normalized to match the DB unique
// index on (contact_id, type, value_normalized).
func (s *EnrichmentService) enrichContactMethodsTx(
	ctx context.Context,
	tx pgx.Tx,
	txMethodRepo *repository.ContactMethodRepository,
	txEnrichmentRepo *repository.EnrichmentRepository,
	contact *repository.Contact,
	external *repository.ExternalContact,
) ([]Method, error) {
	existingMethods, err := txMethodRepo.ListContactMethodsByContact(ctx, contact.ID)
	if err != nil {
		return nil, err
	}

	existingSet := make(map[string]bool)
	for _, m := range existingMethods {
		existingSet[methodDedupKey(m.Type, m.Value)] = true
	}

	added := make([]Method, 0)

	for _, input := range BuildMethodsFromExternal(external) {
		key := methodDedupKey(input.Type, input.Value)
		if existingSet[key] {
			continue
		}
		created, raced, err := insertContactMethodSavepoint(ctx, tx, repository.CreateContactMethodRequest{
			ContactID: contact.ID,
			Type:      input.Type,
			Value:     input.Value,
			IsPrimary: false, // Never set primary for enriched methods
		})
		if err != nil {
			// Do NOT log input.Value — a raw email/phone is PII under this
			// repo's model. Type + error class is enough to diagnose; the
			// method value is recoverable from the external_contact row.
			logger.Warn().Err(err).
				Str("type", input.Type).
				Msg("failed to add method from enrichment")
			continue
		}
		if raced {
			// Concurrent insert — already there, treat as success
			existingSet[key] = true
			continue
		}
		added = append(added, Method{Type: created.Type, Value: created.ValueNormalized})
		if auditErr := s.recordEnrichmentTx(ctx, txEnrichmentRepo, contact.ID, external,
			"method:"+input.Type+":"+identity.Normalize(input.Value, mapMethodTypeToIdentifier(input.Type)),
			input.Value); auditErr != nil {
			return nil, auditErr
		}
		existingSet[key] = true
	}
	return added, nil
}

// methodDedupKey returns the dedup key used to compare an incoming method
// against existing methods on a contact. Mirrors the DB unique index shape
// (contact_id, type, value_normalized) — without type scoping, cross-type
// duplicates like telegram:foo vs twitter:foo would collide incorrectly
// because they normalize to the same handle but live in different type buckets.
func methodDedupKey(methodType, value string) string {
	return methodType + ":" + identity.Normalize(value, mapMethodTypeToIdentifier(methodType))
}

// isUniqueViolation returns true if err is a PostgreSQL unique_violation (23505).
// Used to treat concurrent-insert races as idempotent no-ops.
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// recordEnrichment records that a field was enriched from an external source.
// Passes ExternalContactID=nil when external.ID is the zero UUID so matcher
// paths that synthesize an *ExternalContact (no persisted row for
// below-threshold peers) produce SQL NULL in the audit, not a zero UUID.
//
// Used for profile-field audits (profile_photo, birthday, location) which
// run OUTSIDE the method-enrichment tx. The tx-scoped counterpart
// recordEnrichmentTx handles per-method audits.
func (s *EnrichmentService) recordEnrichment(
	ctx context.Context,
	contactID uuid.UUID,
	external *repository.ExternalContact,
	field string,
	value string,
) {
	var externalContactID *uuid.UUID
	if external.ID != uuid.Nil {
		externalContactID = &external.ID
	}
	_, err := s.enrichmentRepo.Create(ctx, repository.CreateEnrichmentRequest{
		ContactID:         contactID,
		Source:            external.Source,
		AccountID:         external.AccountID,
		Field:             field,
		ExternalContactID: externalContactID,
		OriginalValue:     &value,
	})
	if err != nil {
		logger.Warn().Err(err).Str("field", field).Msg("failed to record enrichment")
	}
}

// recordEnrichmentTx is the tx-scoped counterpart to recordEnrichment.
// Audit rows share fate with the method rows + event row from the
// caller's pgx.Tx (spec §4 atomicity). Uses the tx-scoped
// EnrichmentRepository built on db.New(tx).
//
// Unlike recordEnrichment which log-swallows insert errors, this
// variant returns the error to its caller — an audit-insert failure
// inside the tx aborts the whole flow so method rows + event + audit
// all roll back together.
func (s *EnrichmentService) recordEnrichmentTx(
	ctx context.Context,
	txEnrichmentRepo *repository.EnrichmentRepository,
	contactID uuid.UUID,
	external *repository.ExternalContact,
	field string,
	value string,
) error {
	var externalContactID *uuid.UUID
	if external.ID != uuid.Nil {
		externalContactID = &external.ID
	}
	_, err := txEnrichmentRepo.Create(ctx, repository.CreateEnrichmentRequest{
		ContactID:         contactID,
		Source:            external.Source,
		AccountID:         external.AccountID,
		Field:             field,
		ExternalContactID: externalContactID,
		OriginalValue:     &value,
	})
	if err != nil {
		return fmt.Errorf("record enrichment (tx) for field %s: %w", field, err)
	}
	return nil
}

// insertContactMethodSavepoint wraps CreateContactMethod in a nested
// pgx savepoint so a unique-violation (concurrent-insert race) rolls
// back only the inner savepoint, leaving the outer tx live for
// subsequent inserts (CLAUDE.md gotcha: a 23505 inside pgx.Tx aborts
// the outer tx without this).
//
// Returns (method, raced=false, nil) on a clean insert,
//
//	(nil, raced=true,  nil) on a unique-violation (caller treats as
//	                        existing-row success),
//	(nil, false,       err) on any other error.
func insertContactMethodSavepoint(
	ctx context.Context,
	tx pgx.Tx,
	req repository.CreateContactMethodRequest,
) (*repository.ContactMethod, bool, error) {
	sp, err := tx.Begin(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("begin savepoint for contact_method insert: %w", err)
	}
	// Use the savepoint-scoped queries so the INSERT runs inside the
	// nested tx. Repository is cheap to construct.
	spQueries := db.New(sp)
	spMethodRepo := repository.NewContactMethodRepository(spQueries)
	created, err := spMethodRepo.CreateContactMethod(ctx, req)
	if err != nil {
		_ = sp.Rollback(ctx)
		if isUniqueViolation(err) {
			return nil, true, nil
		}
		return nil, false, err
	}
	if commitErr := sp.Commit(ctx); commitErr != nil {
		return nil, false, fmt.Errorf("commit savepoint for contact_method insert: %w", commitErr)
	}
	return created, false, nil
}

// SyncMethodsFromExternal adds any missing contact methods from an
// ExternalContact to the given CRM contact. Unlike EnrichContactFromExternal,
// it does NOT touch profile fields (photo, birthday, location, name, cadence).
// Intended for auto-match flows where silent profile overwrites are undesirable.
//
// Audit rows share a tx with the method inserts (spec §4 atomicity).
// Matcher paths don't publish contact_methods.added events — rematch
// is only dispatched from the HTTP-facing Enrich* entry points
// (Appendix A spec divergence, unchanged since #182).
//
// Idempotent: duplicate methods (normalized-value dedup OR savepoint-
// wrapped unique-violation) are no-ops.
func (s *EnrichmentService) SyncMethodsFromExternal(
	ctx context.Context,
	crmContactID uuid.UUID,
	external *repository.ExternalContact,
) error {
	contact, err := s.contactRepo.GetContact(ctx, crmContactID)
	if err != nil {
		return fmt.Errorf("get contact for method sync: %w", err)
	}
	return pgx.BeginTxFunc(ctx, s.database.Pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		txQueries := db.New(tx)
		txMethodRepo := repository.NewContactMethodRepository(txQueries)
		txEnrichmentRepo := repository.NewEnrichmentRepository(txQueries)
		_, innerErr := s.enrichContactMethodsTx(ctx, tx, txMethodRepo, txEnrichmentRepo, contact, external)
		return innerErr
	})
}

// HasEnrichment checks if a field has been enriched for a contact
func (s *EnrichmentService) HasEnrichment(ctx context.Context, contactID uuid.UUID, field string) (bool, error) {
	return s.enrichmentRepo.HasEnrichment(ctx, contactID, field)
}

// ListEnrichments returns all enrichments for a contact
func (s *EnrichmentService) ListEnrichments(ctx context.Context, contactID uuid.UUID) ([]repository.ContactEnrichment, error) {
	return s.enrichmentRepo.ListForContact(ctx, contactID)
}

// deriveContactByFromCadence computes the next contact_by date from a
// cadence string + the contact's existing last_contacted (fallback:
// created_at). Returns nil when cadence is nil, empty, or unparseable —
// those cases collapse to "clear contact_by" on the unconditional branch.
func deriveContactByFromCadence(contact *repository.Contact, cadenceArg *string) *time.Time {
	if contact == nil || cadenceArg == nil || *cadenceArg == "" {
		return nil
	}
	cadenceType, err := cadence.ParseCadence(*cadenceArg)
	if err != nil {
		return nil
	}
	base := contact.CreatedAt
	if contact.LastContacted != nil {
		base = *contact.LastContacted
	}
	t := cadence.CalculateContactBy(base, cadenceType)
	return &t
}

// assertInferredKnowledge persists the enrichment-inferred location/birthday as
// agent-provenance assertions inside the caller's tx and refreshes the derived
// cache columns inline. No-op when nothing was inferred. The provenance is
// producer=agent / source_kind=agent_session (the values come from external
// contact data, not a direct user edit), keyed deterministically so a re-run
// corroborates rather than duplicates.
func (s *EnrichmentService) assertInferredKnowledge(ctx context.Context, tx pgx.Tx, contactID uuid.UUID, inferred knowledgeFieldValues) error {
	if inferred.Location == nil && inferred.Birthday == nil && inferred.HowMet == nil {
		return nil
	}
	return s.knowledge.assertCreate(ctx, tx, contactID, inferred, knowledgeFieldProvenance{
		SourceKind:     repository.SourceKindAgentSession,
		ProducerKind:   repository.ProducerKindAgent,
		SourceIDPrefix: "enrichment",
	})
}

// mapMethodTypeToIdentifier maps contact method type to identity type for normalization
func mapMethodTypeToIdentifier(methodType string) identity.IdentifierType {
	switch methodType {
	case string(repository.ContactMethodEmail):
		return identity.IdentifierTypeEmail
	case string(repository.ContactMethodPhone), string(repository.ContactMethodSignal):
		return identity.IdentifierTypePhone
	case string(repository.ContactMethodGChat):
		return identity.IdentifierTypeEmail
	case string(repository.ContactMethodTelegram):
		return identity.IdentifierTypeTelegram
	case string(repository.ContactMethodWhatsApp):
		return identity.IdentifierTypeWhatsApp
	case string(repository.ContactMethodDiscord), string(repository.ContactMethodTwitter):
		return identity.IdentifierTypeTelegram
	default:
		return identity.IdentifierTypeEmail
	}
}
