package service

import (
	"context"
	"strconv"
	"strings"

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

// usernameMatchGap is the minimum score gap (top-1 minus runner-up) required
// for a username-derived top match to be surfaced as a suggestion. Tighter
// gaps mean the top match is ambiguous vs. common handles (e.g., "alex"),
// and the UI shouldn't guess.
const usernameMatchGap = 0.15

// candidateSearchTerm is a single name-search string derived from an external
// contact. FromUsername is true when the term came from a Telegram handle
// rather than a display or first+last name.
type candidateSearchTerm struct {
	Text         string
	FromUsername bool
}

// candidateSearchTerms returns up to two name-search terms for an external
// contact: the primary (display/first+last) name, and — for Telegram — a
// normalized username term. Callers use the per-term score max to pick the
// best match.
func candidateSearchTerms(external *repository.ExternalContact) []candidateSearchTerm {
	var terms []candidateSearchTerm
	if name := extractCandidateName(external); name != "" {
		terms = append(terms, candidateSearchTerm{Text: name, FromUsername: false})
	}
	if external.Source == "telegram" {
		if raw, ok := external.Metadata["username"].(string); ok {
			if normalized, usable := matching.NormalizeHandleForNameMatch(raw); usable {
				if len(terms) == 0 || !strings.EqualFold(terms[0].Text, normalized) {
					terms = append(terms, candidateSearchTerm{Text: normalized, FromUsername: true})
				}
			}
		}
	}
	return terms
}

// methodMaps holds normalized contact methods for a single external contact,
// keyed for fast overlap counting against CRM contact methods.
type methodMaps struct {
	emails map[string]bool
	phones map[string]bool
}

// buildMethodMaps pre-normalizes email/phone methods for every external
// contact so score computation doesn't re-normalize per match.
func buildMethodMaps(externals []*repository.ExternalContact) map[string]methodMaps {
	out := make(map[string]methodMaps, len(externals))
	for _, external := range externals {
		emails := make(map[string]bool)
		for _, email := range external.Emails {
			emails[matching.NormalizeEmail(email.Value)] = true
		}
		phones := make(map[string]bool)
		for _, phone := range external.Phones {
			phones[matching.NormalizePhoneLoose(phone.Value)] = true
		}
		out[external.ID.String()] = methodMaps{emails: emails, phones: phones}
	}
	return out
}

// contactScore tracks the best score seen for a single (candidate, contact)
// pair across all search terms for that candidate.
type contactScore struct {
	contactID    string
	contactName  string
	score        float64
	fromUsername bool
}

// applyExactHandleBonus adds the method-weight bonus when the matching term
// came from a username AND the username and contact full_name collapse to the
// same strict-equality form. Capped at 1.0.
func applyExactHandleBonus(score float64, fromUsername bool, usernameTerm, fullName string) float64 {
	if !fromUsername {
		return score
	}
	handleStrict := matching.NormalizeForExactHandleMatch(usernameTerm)
	nameStrict := matching.NormalizeForExactHandleMatch(fullName)
	if handleStrict == "" || handleStrict != nameStrict {
		return score
	}
	score += matching.ImportConfig.MethodWeight
	if score > 1.0 {
		score = 1.0
	}
	return score
}

// selectBestSuggestion picks a suggestion from the per-contact score map for
// one candidate. Returns nil if no top match or if the top match is
// username-derived and too close to the runner-up (ambiguous handle).
func selectBestSuggestion(byContact map[string]*contactScore) *ImportSuggestedMatch {
	var top1, top2 *contactScore
	for _, cs := range byContact {
		switch {
		case top1 == nil || cs.score > top1.score:
			top2 = top1
			top1 = cs
		case top2 == nil || cs.score > top2.score:
			top2 = cs
		}
	}
	if top1 == nil {
		return nil
	}
	if top1.fromUsername && top2 != nil && top1.score-top2.score < usernameMatchGap {
		return nil
	}
	return &ImportSuggestedMatch{
		ContactID:   top1.contactID,
		ContactName: top1.contactName,
		Confidence:  top1.score,
	}
}

// FindBestMatch finds the best matching CRM contact for an external contact.
// Returns a suggested match if confidence >= matching.ImportConfig.ConfidenceThreshold.
func (s *ImportMatchService) FindBestMatch(ctx context.Context, external *repository.ExternalContact) (*ImportSuggestedMatch, error) {
	terms := candidateSearchTerms(external)
	if len(terms) == 0 {
		return nil, nil
	}

	methodMap := buildMethodMaps([]*repository.ExternalContact{external})[external.ID.String()]

	var usernameText string
	for _, term := range terms {
		if term.FromUsername {
			usernameText = term.Text
			break
		}
	}

	byContact := make(map[string]*contactScore)
	for _, term := range terms {
		matches, err := s.contactRepo.FindSimilarContacts(ctx, term.Text, matching.ImportConfig.MinSimilarityThreshold, 5)
		if err != nil {
			logger.Warn().Err(err).Str("name", term.Text).Msg("failed to find similar contacts")
			return nil, err
		}
		for _, m := range matches {
			methodMatches, totalMethods := countMethodOverlap(m.Contact.Methods, methodMap.emails, methodMap.phones)
			score := matching.ImportConfig.Score(m.Similarity, methodMatches, totalMethods)
			score = applyExactHandleBonus(score, term.FromUsername, usernameText, m.Contact.FullName)
			if score < matching.ImportConfig.ConfidenceThreshold {
				continue
			}
			contactID := m.Contact.ID.String()
			existing := byContact[contactID]
			if existing == nil || score > existing.score {
				byContact[contactID] = &contactScore{
					contactID:    contactID,
					contactName:  m.Contact.FullName,
					score:        score,
					fromUsername: term.FromUsername,
				}
			}
		}
	}

	return selectBestSuggestion(byContact), nil
}

// FindBestMatchesBatch finds best matches for multiple external contacts in one batch.
// Returns matches in the same order as inputs, with nil for candidates with no match.
func (s *ImportMatchService) FindBestMatchesBatch(
	ctx context.Context,
	externals []*repository.ExternalContact,
) ([]*ImportSuggestedMatch, error) {
	type termMeta struct {
		candidateIdx int
		fromUsername bool
	}

	termIndex := make(map[string]termMeta)
	var batchInputs []repository.BatchContactInput
	usernameTerm := make(map[int]string) // candidateIdx -> normalized username term

	for i, external := range externals {
		terms := candidateSearchTerms(external)
		if len(terms) > 0 {
			logger.Debug().
				Str("candidate_id", external.ID.String()).
				Int("term_count", len(terms)).
				Msg("generated search terms for candidate")
		}
		for ti, term := range terms {
			compositeID := external.ID.String() + "|" + strconv.Itoa(ti)
			batchInputs = append(batchInputs, repository.BatchContactInput{
				CandidateID:   compositeID,
				CandidateName: term.Text,
			})
			termIndex[compositeID] = termMeta{candidateIdx: i, fromUsername: term.FromUsername}
			if term.FromUsername {
				usernameTerm[i] = term.Text
			}
		}
	}

	if len(batchInputs) == 0 {
		return make([]*ImportSuggestedMatch, len(externals)), nil
	}

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

	candidateMethodMaps := buildMethodMaps(externals)

	perCandidate := make(map[int]map[string]*contactScore)

	for _, bm := range batchMatches {
		meta, ok := termIndex[bm.CandidateID]
		if !ok {
			continue
		}
		ext := externals[meta.candidateIdx]
		methodMap := candidateMethodMaps[ext.ID.String()]

		for _, m := range bm.Matches {
			methodMatches, totalMethods := countMethodOverlap(
				m.Contact.Methods, methodMap.emails, methodMap.phones,
			)
			score := matching.ImportConfig.Score(m.Similarity, methodMatches, totalMethods)
			score = applyExactHandleBonus(score, meta.fromUsername, usernameTerm[meta.candidateIdx], m.Contact.FullName)

			if score < matching.ImportConfig.ConfidenceThreshold {
				continue
			}
			contactID := m.Contact.ID.String()
			byContact := perCandidate[meta.candidateIdx]
			if byContact == nil {
				byContact = make(map[string]*contactScore)
				perCandidate[meta.candidateIdx] = byContact
			}
			existing := byContact[contactID]
			if existing == nil || score > existing.score {
				byContact[contactID] = &contactScore{
					contactID:    contactID,
					contactName:  m.Contact.FullName,
					score:        score,
					fromUsername: meta.fromUsername,
				}
			}
		}
	}

	results := make([]*ImportSuggestedMatch, len(externals))
	for idx, byContact := range perCandidate {
		results[idx] = selectBestSuggestion(byContact)
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
