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

type contactMatchFinder interface {
	FindSimilarContacts(ctx context.Context, name string, threshold float64, limit int32) ([]repository.ContactMatch, error)
	FindSimilarContactsBatch(ctx context.Context, inputs []repository.BatchContactInput, threshold float64, limitPerCandidate int32) ([]repository.BatchContactMatch, error)
}

// ImportMatchService encapsulates matching logic for import candidates.
type ImportMatchService struct {
	contactRepo contactMatchFinder
}

// NewImportMatchService creates a new import match service.
func NewImportMatchService(contactRepo contactMatchFinder) *ImportMatchService {
	return &ImportMatchService{contactRepo: contactRepo}
}

// FindBestMatch finds the best matching CRM contact for an external contact.
// Returns a suggested match if confidence >= matching.ImportConfig.ConfidenceThreshold.
func (s *ImportMatchService) FindBestMatch(ctx context.Context, external *repository.ExternalContact) (*ImportSuggestedMatch, error) {
	candidateName := extractCandidateName(external)
	if candidateName == "" {
		return nil, nil
	}

	matches, err := s.contactRepo.FindSimilarContacts(ctx, candidateName, matching.ImportConfig.MinSimilarityThreshold, 5)
	if err != nil {
		logger.Warn().Err(err).Str("name", candidateName).Msg("failed to find similar contacts")
		return nil, err
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

	return bestMatch, nil
}

// FindBestMatchesBatch finds best matches for multiple external contacts in one batch.
// Returns matches in the same order as inputs, with nil for candidates with no match.
func (s *ImportMatchService) FindBestMatchesBatch(
	ctx context.Context,
	externals []*repository.ExternalContact,
) ([]*ImportSuggestedMatch, error) {
	// Build batch input with valid names only
	var batchInputs []repository.BatchContactInput
	indexMap := make(map[string]int) // candidate_id -> index in externals

	for i, external := range externals {
		candidateName := extractCandidateName(external)
		if candidateName == "" {
			continue
		}

		candidateID := external.ID.String()
		batchInputs = append(batchInputs, repository.BatchContactInput{
			CandidateID:   candidateID,
			CandidateName: candidateName,
		})
		indexMap[candidateID] = i
	}

	// Early return if no valid candidates
	if len(batchInputs) == 0 {
		return make([]*ImportSuggestedMatch, len(externals)), nil
	}

	// Execute batch query
	batchMatches, err := s.contactRepo.FindSimilarContactsBatch(
		ctx,
		batchInputs,
		matching.ImportConfig.MinSimilarityThreshold,
		5,
	)
	if err != nil {
		logger.Warn().Err(err).Msg("failed to find similar contacts in batch")
		return nil, err
	}

	// Prepare normalized contact methods for each candidate (by candidate ID)
	type methodMaps struct {
		emails map[string]bool
		phones map[string]bool
	}
	candidateMethodMaps := make(map[string]methodMaps)

	for _, external := range externals {
		candidateID := external.ID.String()

		emails := make(map[string]bool)
		for _, email := range external.Emails {
			emails[matching.NormalizeEmail(email.Value)] = true
		}

		phones := make(map[string]bool)
		for _, phone := range external.Phones {
			phones[matching.NormalizePhoneLoose(phone.Value)] = true
		}

		candidateMethodMaps[candidateID] = methodMaps{emails: emails, phones: phones}
	}

	// Process matches and compute scores
	results := make([]*ImportSuggestedMatch, len(externals))

	for _, batchMatch := range batchMatches {
		methodMap := candidateMethodMaps[batchMatch.CandidateID]

		var bestMatch *ImportSuggestedMatch
		var bestScore float64

		for _, match := range batchMatch.Matches {
			methodMatches, totalMethods := countMethodOverlap(
				match.Contact.Methods,
				methodMap.emails,
				methodMap.phones,
			)
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

		// Map back to original index
		if idx, exists := indexMap[batchMatch.CandidateID]; exists {
			results[idx] = bestMatch
		}
	}

	return results, nil
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
		case "email":
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
