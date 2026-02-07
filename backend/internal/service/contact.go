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

type ContactService struct {
	database          *db.Database
	contactRepo       *repository.ContactRepository
	contactMethodRepo *repository.ContactMethodRepository
	interactionRepo   *repository.InteractionRepository
}

func NewContactService(database *db.Database, contactRepo *repository.ContactRepository, contactMethodRepo *repository.ContactMethodRepository, interactionRepo *repository.InteractionRepository) *ContactService {
	return &ContactService{
		database:          database,
		contactRepo:       contactRepo,
		contactMethodRepo: contactMethodRepo,
		interactionRepo:   interactionRepo,
	}
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

	total, err := s.contactRepo.CountContacts(ctx, params.CadenceFilter)
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

	total, err := s.contactRepo.CountSearchContacts(ctx, params.Query, params.CadenceFilter)
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

func (s *ContactService) CreateContact(ctx context.Context, req repository.CreateContactRequest, methods []ContactMethodInput) (contact *repository.Contact, err error) {
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

	contact, err = contactRepo.CreateContact(ctx, req)
	if err != nil {
		return nil, err
	}

	createdMethods, err := createContactMethods(ctx, contactMethodRepo, contact.ID, methods)
	if err != nil {
		return nil, err
	}

	if err = tx.Commit(ctx); err != nil {
		return nil, err
	}

	assignMethods(contact, createdMethods)
	return contact, nil
}

func (s *ContactService) UpdateContact(ctx context.Context, id uuid.UUID, req repository.UpdateContactRequest, methods []ContactMethodInput, replaceMethods bool) (contact *repository.Contact, err error) {
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

	existingContact, err := contactRepo.GetContact(ctx, id)
	if err != nil {
		return nil, err
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
		return nil, err
	}

	var updatedMethods []repository.ContactMethod
	if replaceMethods {
		if err := contactMethodRepo.DeleteContactMethodsByContact(ctx, id); err != nil {
			return nil, err
		}

		updatedMethods, err = createContactMethods(ctx, contactMethodRepo, id, methods)
		if err != nil {
			return nil, err
		}
	} else {
		updatedMethods, err = contactMethodRepo.ListContactMethodsByContact(ctx, id)
		if err != nil {
			return nil, err
		}
		sortContactMethods(updatedMethods)
	}

	if err = tx.Commit(ctx); err != nil {
		return nil, err
	}

	assignMethods(contact, updatedMethods)
	return contact, nil
}

func (s *ContactService) DeleteContact(ctx context.Context, id uuid.UUID) error {
	_, err := s.contactRepo.GetContact(ctx, id)
	if err != nil {
		return err
	}

	return s.contactRepo.SoftDeleteContact(ctx, id)
}

// UpdateContactLastContacted updates the last contacted date for a contact.
// If lastContacted is nil, the current time is used.
// Also records an interaction and updates contact_by based on the new last_contacted date and cadence.
func (s *ContactService) UpdateContactLastContacted(ctx context.Context, id uuid.UUID, lastContacted *time.Time) (*repository.Contact, error) {
	// Use provided date or default to current time
	dateToSet := accelerated.GetCurrentTime()
	if lastContacted != nil {
		dateToSet = *lastContacted
	}

	// Record interaction (handles dedup, last_contacted update, and contact_by recalculation)
	_, err := s.RecordInteraction(ctx, repository.RecordInteractionRequest{
		ContactID:  id,
		Source:     repository.InteractionSourceManual,
		OccurredAt: dateToSet,
	})
	if err != nil {
		return nil, fmt.Errorf("record interaction: %w", err)
	}

	contact, err := s.contactRepo.GetContact(ctx, id)
	if err != nil {
		return nil, err
	}

	if err := s.attachMethods(ctx, contact); err != nil {
		return nil, err
	}

	return contact, nil
}

// RecordInteraction creates an interaction record and updates last_contacted/contact_by.
// Handles source-aware deduplication:
//   - For sources with source_ref (gcal, todoist): dedup by source_ref
//   - For manual sources (no source_ref): dedup by 30-minute time window
//
// Uses forward-only semantics for last_contacted: only updates if occurred_at > current last_contacted.
func (s *ContactService) RecordInteraction(ctx context.Context, req repository.RecordInteractionRequest) (*repository.Interaction, error) {
	// 1. Source-aware deduplication
	if req.SourceRef != nil {
		existing, err := s.interactionRepo.FindBySourceRef(ctx, req.ContactID, req.Source, *req.SourceRef)
		if err != nil && !errors.Is(err, db.ErrNotFound) {
			return nil, fmt.Errorf("check existing interaction by source_ref: %w", err)
		}
		if existing != nil {
			logger.Debug().
				Str("contactId", req.ContactID.String()).
				Str("source", req.Source).
				Str("sourceRef", *req.SourceRef).
				Msg("skipping duplicate interaction (same source_ref)")
			return existing, nil
		}
	} else {
		// Manual source: use 30-minute time window
		existing, err := s.interactionRepo.FindInWindow(ctx, req.ContactID, req.Source, req.OccurredAt, 30*time.Minute)
		if err != nil && !errors.Is(err, db.ErrNotFound) {
			return nil, fmt.Errorf("check existing interaction in window: %w", err)
		}
		if existing != nil {
			logger.Debug().
				Str("contactId", req.ContactID.String()).
				Str("existingSource", existing.Source).
				Str("newSource", req.Source).
				Msg("skipping duplicate interaction within 30-min window")
			return existing, nil
		}
	}

	// 2. Verify contact exists (avoids FK violation returning unhelpful error)
	contact, err := s.contactRepo.GetContact(ctx, req.ContactID)
	if err != nil {
		return nil, err // propagates ErrNotFound
	}

	// 3. Create interaction record
	interaction, err := s.interactionRepo.CreateInteraction(ctx, repository.CreateInteractionRequest(req))
	if err != nil {
		return nil, fmt.Errorf("create interaction: %w", err)
	}

	// 4. Update last_contacted
	// Manual source always updates (user correction). Automated sources use forward-only semantics.
	isManual := req.Source == repository.InteractionSourceManual
	shouldUpdate := isManual || contact.LastContacted == nil || req.OccurredAt.After(*contact.LastContacted)
	if shouldUpdate {
		// Calculate contact_by based on cadence
		var contactBy *time.Time
		if contact.Cadence != nil && *contact.Cadence != "" {
			if cadenceType, err := cadence.ParseCadence(*contact.Cadence); err == nil {
				contactByTime := cadence.CalculateContactBy(req.OccurredAt, cadenceType)
				contactBy = &contactByTime
			}
		}

		if err := s.contactRepo.UpdateContactLastContacted(ctx, req.ContactID, req.OccurredAt, contactBy); err != nil {
			logger.Warn().Err(err).
				Str("contactId", req.ContactID.String()).
				Msg("failed to update last_contacted from interaction")
		}
	}

	return interaction, nil
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
