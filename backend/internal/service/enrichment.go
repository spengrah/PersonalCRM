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
func (s *EnrichmentService) EnrichContactFromExternalWithSelections(
	ctx context.Context,
	crmContactID uuid.UUID,
	external *repository.ExternalContact,
	selectedMethods []MethodSelection,
	conflictResolutions map[string]string,
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
	// Get existing methods
	existingMethods, err := s.methodRepo.ListContactMethodsByContact(ctx, contact.ID)
	if err != nil {
		return err
	}

	// Build maps for existing methods
	existingByType := make(map[string]*repository.ContactMethod)
	existingNormalized := make(map[string]bool)
	for i := range existingMethods {
		m := &existingMethods[i]
		existingByType[m.Type] = m
		normalized := identity.Normalize(m.Value, mapMethodTypeToIdentifier(m.Type))
		existingNormalized[normalized] = true
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
			continue // Already have this value
		}

		// Check if type slot is taken
		if existingMethod, exists := existingByType[sel.Type]; exists {
			// Type conflict - check resolution
			resolution := conflictResolutions[sel.OriginalValue]
			if resolution == "use_external" {
				// Replace CRM value with external value
				err := s.methodRepo.UpdateContactMethod(ctx, existingMethod.ID, repository.UpdateContactMethodRequest{
					Value: sel.OriginalValue,
				})
				if err != nil {
					methodErrors = append(methodErrors, fmt.Sprintf("failed to update method %s: %v", sel.OriginalValue, err))
					continue
				}
				s.recordEnrichment(ctx, contact.ID, external, "method:"+sel.Type+":replaced", sel.OriginalValue)
			}
			// If resolution is "use_crm" or empty, keep existing value (no action)
			continue
		}

		// Type slot is available - add the method
		_, err := s.methodRepo.CreateContactMethod(ctx, repository.CreateContactMethodRequest{
			ContactID: contact.ID,
			Type:      sel.Type,
			Value:     sel.OriginalValue,
			IsPrimary: false,
		})
		if err != nil {
			methodErrors = append(methodErrors, fmt.Sprintf("failed to add method %s: %v", sel.OriginalValue, err))
			continue
		}

		s.recordEnrichment(ctx, contact.ID, external, "method:"+sel.Type+":"+normalized, sel.OriginalValue)
		existingNormalized[normalized] = true
		existingByType[sel.Type] = nil // Mark type as taken (we don't need the actual method)
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

	// Build set of normalized existing values and types
	existingSet := make(map[string]bool)
	existingTypes := make(map[string]bool)
	for _, m := range existingMethods {
		normalized := identity.Normalize(m.Value, mapMethodTypeToIdentifier(m.Type))
		existingSet[normalized] = true
		existingTypes[m.Type] = true
	}

	// Add missing emails
	var conflicts []string
	for _, email := range external.Emails {
		normalized := identity.Normalize(email.Value, identity.IdentifierTypeEmail)
		if existingSet[normalized] {
			continue // Already have this email
		}

		// Determine type based on email type from Google
		methodType := string(repository.ContactMethodEmailPersonal)
		if strings.Contains(strings.ToLower(email.Type), "work") {
			methodType = string(repository.ContactMethodEmailWork)
		}

		// Check if this type is already taken
		if existingTypes[methodType] {
			conflicts = append(conflicts, email.Value+" (type "+methodType+" already exists)")
			continue
		}

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
		existingTypes[methodType] = true
	}

	// Add missing phones
	for _, phone := range external.Phones {
		normalized := identity.Normalize(phone.Value, identity.IdentifierTypePhone)
		if existingSet[normalized] {
			continue // Already have this phone
		}

		methodType := string(repository.ContactMethodPhone)

		// Check if phone type is already taken
		if existingTypes[methodType] {
			conflicts = append(conflicts, phone.Value+" (phone already exists)")
			continue
		}

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
		existingTypes[methodType] = true
	}

	// Return error if there were conflicts
	if len(conflicts) > 0 {
		return fmt.Errorf("contact method conflicts: %s", strings.Join(conflicts, "; "))
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
	case string(repository.ContactMethodEmailPersonal), string(repository.ContactMethodEmailWork):
		return identity.IdentifierTypeEmail
	case string(repository.ContactMethodPhone):
		return identity.IdentifierTypePhone
	case string(repository.ContactMethodTelegram):
		return identity.IdentifierTypeTelegram
	case string(repository.ContactMethodWhatsApp):
		return identity.IdentifierTypeWhatsApp
	default:
		return identity.IdentifierTypeEmail
	}
}
