package service

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"personal-crm/backend/internal/identity"
	"personal-crm/backend/internal/logger"
	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

// MethodSelection represents a user-selected method for enrichment
type MethodSelection struct {
	OriginalValue string
	Type          string
	IsPrimary     bool
}

// EnrichmentService handles contact enrichment from external sources
type EnrichmentService struct {
	contactRepo    *repository.ContactRepository
	methodRepo     *repository.ContactMethodRepository
	enrichmentRepo *repository.EnrichmentRepository
}

// NewEnrichmentService creates a new enrichment service
func NewEnrichmentService(
	contactRepo *repository.ContactRepository,
	methodRepo *repository.ContactMethodRepository,
	enrichmentRepo *repository.EnrichmentRepository,
) *EnrichmentService {
	return &EnrichmentService{
		contactRepo:    contactRepo,
		methodRepo:     methodRepo,
		enrichmentRepo: enrichmentRepo,
	}
}

// EnrichContactFromExternal enriches a CRM contact with data from an external contact.
// Only fills in missing fields - never overwrites existing data.
func (s *EnrichmentService) EnrichContactFromExternal(
	ctx context.Context,
	crmContactID uuid.UUID,
	external *repository.ExternalContact,
) error {
	// Get current contact
	contact, err := s.contactRepo.GetContact(ctx, crmContactID)
	if err != nil {
		return err
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

	// Enrich birthday if CRM contact has none
	if contact.Birthday == nil && external.Birthday != nil {
		updateReq.Birthday = external.Birthday
		needsUpdate = true
		s.recordEnrichment(ctx, crmContactID, external, "birthday", external.Birthday.Format("2006-01-02"))
	}

	// Enrich location from addresses if CRM contact has none
	if contact.Location == nil && len(external.Addresses) > 0 && external.Addresses[0].Formatted != "" {
		location := external.Addresses[0].Formatted
		updateReq.Location = &location
		needsUpdate = true
		s.recordEnrichment(ctx, crmContactID, external, "location", location)
	}

	// Apply updates to contact if any enrichment occurred
	if needsUpdate {
		if _, err := s.contactRepo.UpdateContact(ctx, crmContactID, updateReq); err != nil {
			logger.Warn().Err(err).Msg("failed to update contact with enrichments")
		}
	}

	// Enrich contact methods (emails, phones)
	if err := s.enrichContactMethods(ctx, contact, external); err != nil {
		logger.Warn().Err(err).Msg("failed to enrich contact methods")
	}

	return nil
}

// EnrichContactFromExternalWithSelections enriches a CRM contact with user-selected methods.
// Unlike EnrichContactFromExternal, this uses explicit method selections and conflict resolutions.
// If cadence is provided, it will update the contact's cadence.
// If name is provided, it will update the contact's full name.
func (s *EnrichmentService) EnrichContactFromExternalWithSelections(
	ctx context.Context,
	crmContactID uuid.UUID,
	external *repository.ExternalContact,
	selectedMethods []MethodSelection,
	conflictResolutions map[string]string,
	cadence *string,
	name *string,
) error {
	// Get current contact
	contact, err := s.contactRepo.GetContact(ctx, crmContactID)
	if err != nil {
		return err
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

	// Enrich birthday if CRM contact has none
	if contact.Birthday == nil && external.Birthday != nil {
		updateReq.Birthday = external.Birthday
		needsUpdate = true
		s.recordEnrichment(ctx, crmContactID, external, "birthday", external.Birthday.Format("2006-01-02"))
	}

	// Enrich location from addresses if CRM contact has none
	if contact.Location == nil && len(external.Addresses) > 0 && external.Addresses[0].Formatted != "" {
		location := external.Addresses[0].Formatted
		updateReq.Location = &location
		needsUpdate = true
		s.recordEnrichment(ctx, crmContactID, external, "location", location)
	}

	// Update cadence if provided
	if cadence != nil {
		updateReq.Cadence = cadence
		needsUpdate = true
	}

	// Update name if provided
	if name != nil && strings.TrimSpace(*name) != "" {
		trimmedName := strings.TrimSpace(*name)
		updateReq.FullName = trimmedName
		needsUpdate = true
	}

	// Apply updates to contact if any enrichment occurred
	if needsUpdate {
		if _, err := s.contactRepo.UpdateContact(ctx, crmContactID, updateReq); err != nil {
			logger.Warn().Err(err).Msg("failed to update contact with enrichments")
		}
	}

	// Enrich contact methods using selections
	if err := s.enrichContactMethodsWithSelections(ctx, contact, external, selectedMethods, conflictResolutions); err != nil {
		return err
	}

	return nil
}

// enrichContactMethodsWithSelections adds methods based on user selection and conflict resolution
func (s *EnrichmentService) enrichContactMethodsWithSelections(
	ctx context.Context,
	contact *repository.Contact,
	external *repository.ExternalContact,
	selectedMethods []MethodSelection,
	conflictResolutions map[string]string,
) error {
	// conflictResolutions is kept for API compatibility; type conflicts are no longer applicable.
	_ = conflictResolutions

	// Get existing methods
	existingMethods, err := s.methodRepo.ListContactMethodsByContact(ctx, contact.ID)
	if err != nil {
		return err
	}

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

		newMethod, err := s.methodRepo.CreateContactMethod(ctx, repository.CreateContactMethodRequest{
			ContactID: contact.ID,
			Type:      sel.Type,
			Value:     storedValue,
			IsPrimary: false, // We'll update primary status separately
		})
		if err != nil {
			if isUniqueViolation(err) {
				// Concurrent insert won the race — refetch the contact's methods
				// to recover the raced row's ID. Required when sel.IsPrimary is
				// set, otherwise the user's primary selection is silently dropped.
				if sel.IsPrimary {
					raced, refetchErr := s.methodRepo.ListContactMethodsByContact(ctx, contact.ID)
					if refetchErr == nil {
						for i := range raced {
							if methodDedupKey(raced[i].Type, raced[i].Value) == selKey {
								existingPrimaryMethodID = &raced[i].ID
								break
							}
						}
					}
				}
				existingKeys[selKey] = true
				continue
			}
			methodErrors = append(methodErrors, fmt.Sprintf("failed to add method %s: %v", storedValue, err))
			continue
		}

		// Track if this new method should be primary
		if sel.IsPrimary {
			newPrimaryMethodID = &newMethod.ID
		}

		s.recordEnrichment(ctx, contact.ID, external,
			"method:"+sel.Type+":"+identity.Normalize(storedValue, mapMethodTypeToIdentifier(sel.Type)),
			storedValue)
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
				if err := s.methodRepo.SetPrimary(ctx, m.ID, false); err != nil {
					return fmt.Errorf("failed to clear existing primary method: %w", err)
				}
			}
		}
		// Set new primary
		if err := s.methodRepo.SetPrimary(ctx, *primaryMethodID, true); err != nil {
			return fmt.Errorf("failed to set primary method: %w", err)
		}
	}

	// Return error if any method operations failed
	if len(methodErrors) > 0 {
		return fmt.Errorf("method enrichment errors: %s", strings.Join(methodErrors, "; "))
	}

	return nil
}

// enrichContactMethods adds missing contact methods from external contact.
// Uses BuildMethodsFromExternal as the single source of truth for emitting
// source-specific methods (emails, phones, telegram username). Dedup is
// type-scoped normalized to match the DB unique index on
// (contact_id, type, value_normalized).
func (s *EnrichmentService) enrichContactMethods(
	ctx context.Context,
	contact *repository.Contact,
	external *repository.ExternalContact,
) error {
	existingMethods, err := s.methodRepo.ListContactMethodsByContact(ctx, contact.ID)
	if err != nil {
		return err
	}

	existingSet := make(map[string]bool)
	for _, m := range existingMethods {
		existingSet[methodDedupKey(m.Type, m.Value)] = true
	}

	for _, input := range BuildMethodsFromExternal(external) {
		key := methodDedupKey(input.Type, input.Value)
		if existingSet[key] {
			continue
		}
		_, err := s.methodRepo.CreateContactMethod(ctx, repository.CreateContactMethodRequest{
			ContactID: contact.ID,
			Type:      input.Type,
			Value:     input.Value,
			IsPrimary: false, // Never set primary for enriched methods
		})
		if err != nil {
			if isUniqueViolation(err) {
				// Concurrent insert — already there, treat as success
				existingSet[key] = true
				continue
			}
			logger.Warn().Err(err).
				Str("type", input.Type).
				Str("value", input.Value).
				Msg("failed to add method from enrichment")
			continue
		}
		s.recordEnrichment(ctx, contact.ID, external,
			"method:"+input.Type+":"+identity.Normalize(input.Value, mapMethodTypeToIdentifier(input.Type)),
			input.Value)
		existingSet[key] = true
	}
	return nil
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

// SyncMethodsFromExternal adds any missing contact methods from an
// ExternalContact to the given CRM contact. Unlike EnrichContactFromExternal,
// it does NOT touch profile fields (photo, birthday, location, name, cadence).
// Intended for auto-match flows where silent profile overwrites are undesirable.
//
// Audit rows are written via recordEnrichment. Idempotent: duplicate methods
// (either via normalized-value dedup or PG unique-violation race) are no-ops.
func (s *EnrichmentService) SyncMethodsFromExternal(
	ctx context.Context,
	crmContactID uuid.UUID,
	external *repository.ExternalContact,
) error {
	contact, err := s.contactRepo.GetContact(ctx, crmContactID)
	if err != nil {
		return fmt.Errorf("get contact for method sync: %w", err)
	}
	return s.enrichContactMethods(ctx, contact, external)
}

// HasEnrichment checks if a field has been enriched for a contact
func (s *EnrichmentService) HasEnrichment(ctx context.Context, contactID uuid.UUID, field string) (bool, error) {
	return s.enrichmentRepo.HasEnrichment(ctx, contactID, field)
}

// ListEnrichments returns all enrichments for a contact
func (s *EnrichmentService) ListEnrichments(ctx context.Context, contactID uuid.UUID) ([]repository.ContactEnrichment, error) {
	return s.enrichmentRepo.ListForContact(ctx, contactID)
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
