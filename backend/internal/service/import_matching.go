package service

import (
	"context"

	"personal-crm/backend/internal/logger"
	"personal-crm/backend/internal/matching"
	"personal-crm/backend/internal/repository"
)

// ImportSuggestedMatch represents a suggested CRM contact match for an import candidate.
type ImportSuggestedMatch struct {
	ContactID   string
	ContactName string
	Confidence  float64
}

// ImportMatchService encapsulates matching logic for import candidates.
type ImportMatchService struct {
	contactRepo *repository.ContactRepository
}

// NewImportMatchService creates a new import match service.
func NewImportMatchService(contactRepo *repository.ContactRepository) *ImportMatchService {
	return &ImportMatchService{contactRepo: contactRepo}
}

// FindBestMatch finds the best matching CRM contact for an external contact.
// Returns a suggested match if confidence >= matching.ImportConfig.ConfidenceThreshold, otherwise nil.
func (s *ImportMatchService) FindBestMatch(ctx context.Context, external *repository.ExternalContact) *ImportSuggestedMatch {
	candidateName := extractCandidateName(external)
	if candidateName == "" {
		return nil
	}

	matches, err := s.contactRepo.FindSimilarContacts(ctx, candidateName, matching.ImportConfig.MinSimilarityThreshold, 5)
	if err != nil {
		logger.Warn().Err(err).Str("name", candidateName).Msg("failed to find similar contacts")
		return nil
	}

	candidateEmails := make(map[string]bool)
	for _, email := range external.Emails {
		candidateEmails[matching.NormalizeEmail(email.Value)] = true
	}
	candidatePhones := make(map[string]bool)
	for _, phone := range external.Phones {
		candidatePhones[matching.NormalizePhoneLoose(phone.Value)] = true
	}

	var bestMatch *ImportSuggestedMatch
	var bestScore float64

	for _, match := range matches {
		methodMatches, totalMethods := countMethodOverlap(match.Contact.Methods, candidateEmails, candidatePhones)
		score := matching.ImportConfig.Score(match.Similarity, methodMatches, totalMethods)

		if score >= matching.ImportConfig.ConfidenceThreshold && score > bestScore {
			bestScore = score
			bestMatch = &ImportSuggestedMatch{
				ContactID:   match.Contact.ID.String(),
				ContactName: match.Contact.FullName,
				Confidence:  score,
			}
		}
	}

	return bestMatch
}

func extractCandidateName(external *repository.ExternalContact) string {
	if external.DisplayName != nil {
		return *external.DisplayName
	}
	if external.FirstName != nil && external.LastName != nil {
		return *external.FirstName + " " + *external.LastName
	}
	if external.FirstName != nil {
		return *external.FirstName
	}
	return ""
}

func countMethodOverlap(
	methods []repository.ContactMethod,
	candidateEmails map[string]bool,
	candidatePhones map[string]bool,
) (int, int) {
	var methodMatches int
	var totalMethods int

	for _, method := range methods {
		switch method.Type {
		case "email_personal", "email_work":
			totalMethods++
			if candidateEmails[matching.NormalizeEmail(method.Value)] {
				methodMatches++
			}
		case "phone":
			totalMethods++
			if candidatePhones[matching.NormalizePhoneLoose(method.Value)] {
				methodMatches++
			}
		}
	}

	return methodMatches, totalMethods
}
