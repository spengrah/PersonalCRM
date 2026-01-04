package service

import (
	"context"
	"strings"

	"personal-crm/backend/internal/identity"
	"personal-crm/backend/internal/logger"
	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
)

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

		// Determine type based on email type from Google
		methodType := string(repository.ContactMethodEmailPersonal)
		if strings.Contains(strings.ToLower(email.Type), "work") {
			methodType = string(repository.ContactMethodEmailWork)
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
	}

	// Add missing phones
	for _, phone := range external.Phones {
		normalized := identity.Normalize(phone.Value, identity.IdentifierTypePhone)
		if existingSet[normalized] {
			continue // Already have this phone
		}

		_, err := s.methodRepo.CreateContactMethod(ctx, repository.CreateContactMethodRequest{
			ContactID: contact.ID,
			Type:      string(repository.ContactMethodPhone),
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
