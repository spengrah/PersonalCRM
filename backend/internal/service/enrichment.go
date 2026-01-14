package service

import (
	"context"
	"fmt"
	"strings"

	"personal-crm/backend/internal/identity"
	"personal-crm/backend/internal/logger"
	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
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

	// Build maps for existing methods (value -> method ID)
	existingNormalized := make(map[string]bool)
	existingMethodByNormalized := make(map[string]*repository.ContactMethod)
	for i := range existingMethods {
		m := &existingMethods[i]
		normalized := identity.Normalize(m.Value, mapMethodTypeToIdentifier(m.Type))
		existingNormalized[normalized] = true
		existingMethodByNormalized[normalized] = m
	}

	// Build map of available values from external contact
	externalValues := make(map[string]bool)
	for _, email := range external.Emails {
		externalValues[email.Value] = true
	}
	for _, phone := range external.Phones {
		externalValues[phone.Value] = true
	}

	// Collect errors for reporting
	var methodErrors []string

	// Track if any selection has IsPrimary set
	var newPrimaryMethodID *uuid.UUID
	var existingPrimaryMethodID *uuid.UUID

	// Process selected methods
	for _, sel := range selectedMethods {
		// Validate the value exists in external contact
		if !externalValues[sel.OriginalValue] {
			methodErrors = append(methodErrors, fmt.Sprintf("value %q not found in external contact", sel.OriginalValue))
			continue
		}

		// Check if value is already in CRM (normalized)
		identType := mapMethodTypeToIdentifier(sel.Type)
		normalized := identity.Normalize(sel.OriginalValue, identType)
		if existingNormalized[normalized] {
			// Method already exists - check if we need to update primary status
			if sel.IsPrimary {
				existingMethod := existingMethodByNormalized[normalized]
				if existingMethod != nil {
					existingPrimaryMethodID = &existingMethod.ID
				}
			}
			continue // Already have this value
		}

		// Add the method
		newMethod, err := s.methodRepo.CreateContactMethod(ctx, repository.CreateContactMethodRequest{
			ContactID: contact.ID,
			Type:      sel.Type,
			Value:     sel.OriginalValue,
			IsPrimary: false, // We'll update primary status separately
		})
		if err != nil {
			methodErrors = append(methodErrors, fmt.Sprintf("failed to add method %s: %v", sel.OriginalValue, err))
			continue
		}

		// Track if this new method should be primary
		if sel.IsPrimary {
			newPrimaryMethodID = &newMethod.ID
		}

		s.recordEnrichment(ctx, contact.ID, external, "method:"+sel.Type+":"+normalized, sel.OriginalValue)
		existingNormalized[normalized] = true
	}

	// Handle primary method updates - first clear any existing primary, then set the new one
	primaryMethodID := newPrimaryMethodID
	if primaryMethodID == nil {
		primaryMethodID = existingPrimaryMethodID
	}

	if primaryMethodID != nil {
		// Clear existing primary
		for i := range existingMethods {
			m := &existingMethods[i]
			if m.IsPrimary && m.ID != *primaryMethodID {
				if err := s.methodRepo.SetPrimary(ctx, m.ID, false); err != nil {
					logger.Warn().Err(err).Str("method_id", m.ID.String()).Msg("failed to clear primary status")
				}
			}
		}
		// Set new primary
		if err := s.methodRepo.SetPrimary(ctx, *primaryMethodID, true); err != nil {
			methodErrors = append(methodErrors, fmt.Sprintf("failed to set primary method: %v", err))
		}
	}

	// Return error if any method operations failed
	if len(methodErrors) > 0 {
		return fmt.Errorf("method enrichment errors: %s", strings.Join(methodErrors, "; "))
	}

	return nil
}

// enrichContactMethods adds missing contact methods from external contact
func (s *EnrichmentService) enrichContactMethods(
	ctx context.Context,
	contact *repository.Contact,
	external *repository.ExternalContact,
) error {
	// Get existing methods
	existingMethods, err := s.methodRepo.ListContactMethodsByContact(ctx, contact.ID)
	if err != nil {
		return err
	}

	// Build set of normalized existing values
	existingSet := make(map[string]bool)
	for _, m := range existingMethods {
		normalized := identity.Normalize(m.Value, mapMethodTypeToIdentifier(m.Type))
		existingSet[normalized] = true
	}

	// Add missing emails
	for _, email := range external.Emails {
		normalized := identity.Normalize(email.Value, identity.IdentifierTypeEmail)
		if existingSet[normalized] {
			continue // Already have this email
		}

		methodType := string(repository.ContactMethodEmail)

		_, err := s.methodRepo.CreateContactMethod(ctx, repository.CreateContactMethodRequest{
			ContactID: contact.ID,
			Type:      methodType,
			Value:     email.Value,
			IsPrimary: false, // Never set primary for enriched methods
		})
		if err != nil {
			logger.Warn().Err(err).Str("email", email.Value).Msg("failed to add email from enrichment")
			continue
		}

		s.recordEnrichment(ctx, contact.ID, external, "method:"+methodType+":"+normalized, email.Value)
		existingSet[normalized] = true // Mark as added
	}

	// Add missing phones
	for _, phone := range external.Phones {
		normalized := identity.Normalize(phone.Value, identity.IdentifierTypePhone)
		if existingSet[normalized] {
			continue // Already have this phone
		}

		methodType := string(repository.ContactMethodPhone)

		_, err := s.methodRepo.CreateContactMethod(ctx, repository.CreateContactMethodRequest{
			ContactID: contact.ID,
			Type:      methodType,
			Value:     phone.Value,
			IsPrimary: false,
		})
		if err != nil {
			logger.Warn().Err(err).Str("phone", phone.Value).Msg("failed to add phone from enrichment")
			continue
		}

		s.recordEnrichment(ctx, contact.ID, external, "method:phone:"+normalized, phone.Value)
		existingSet[normalized] = true
	}

	return nil
}

// recordEnrichment records that a field was enriched from an external source
func (s *EnrichmentService) recordEnrichment(
	ctx context.Context,
	contactID uuid.UUID,
	external *repository.ExternalContact,
	field string,
	value string,
) {
	_, err := s.enrichmentRepo.Create(ctx, repository.CreateEnrichmentRequest{
		ContactID:         contactID,
		Source:            external.Source,
		AccountID:         external.AccountID,
		Field:             field,
		ExternalContactID: &external.ID,
		OriginalValue:     &value,
	})
	if err != nil {
		logger.Warn().Err(err).Str("field", field).Msg("failed to record enrichment")
	}
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
