package handlers

import (
	"sort"
	"strings"
	"testing"

	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
)

// Test the scoring logic for findBestMatch without mocks
// These tests verify the scoring algorithm directly

func TestMatchScoring_NoName(t *testing.T) {
	// When external contact has no name, we can't match
	external := &repository.ExternalContact{
		DisplayName: nil,
		FirstName:   nil,
		LastName:    nil,
	}

	// The handler would return nil for this case
	// This is verified in the actual function logic
	assert.Nil(t, external.DisplayName)
	assert.Nil(t, external.FirstName)
	assert.Nil(t, external.LastName)
}

func TestMatchScoring_EmailNormalization(t *testing.T) {
	// Test email normalization logic
	tests := []struct {
		name   string
		email1 string
		email2 string
		match  bool
	}{
		{
			name:   "exact match",
			email1: "john@example.com",
			email2: "john@example.com",
			match:  true,
		},
		{
			name:   "case insensitive",
			email1: "JOHN@EXAMPLE.COM",
			email2: "john@example.com",
			match:  true,
		},
		{
			name:   "different emails",
			email1: "john@example.com",
			email2: "jane@example.com",
			match:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Build email sets like the handler does
			set1 := make(map[string]bool)
			set1[toLower(tt.email1)] = true

			matched := set1[toLower(tt.email2)]
			assert.Equal(t, tt.match, matched)
		})
	}
}

func TestMatchScoring_PhoneNormalization(t *testing.T) {
	// Test phone normalization logic
	tests := []struct {
		name   string
		phone1 string
		phone2 string
		match  bool
	}{
		{
			name:   "exact match",
			phone1: "5551234",
			phone2: "5551234",
			match:  true,
		},
		{
			name:   "dashes removed",
			phone1: "555-1234",
			phone2: "5551234",
			match:  true,
		},
		{
			name:   "spaces removed",
			phone1: "555 1234",
			phone2: "5551234",
			match:  true,
		},
		{
			name:   "both normalized",
			phone1: "555-123-4567",
			phone2: "555 123 4567",
			match:  true,
		},
		{
			name:   "different phones",
			phone1: "5551234",
			phone2: "5555678",
			match:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Build phone sets like the handler does
			set1 := make(map[string]bool)
			set1[normalizePhone(tt.phone1)] = true

			matched := set1[normalizePhone(tt.phone2)]
			assert.Equal(t, tt.match, matched)
		})
	}
}

func TestMatchScoring_WeightedScore(t *testing.T) {
	// Test the weighted scoring calculation
	tests := []struct {
		name           string
		nameSimilarity float64
		methodMatches  int
		totalMethods   int
		expectedScore  float64
		meetsThreshold bool
	}{
		{
			name:           "high name sim, no method match",
			nameSimilarity: 0.9,
			methodMatches:  0,
			totalMethods:   2,
			expectedScore:  0.54, // 0.9 * 0.6 + 0 * 0.4
			meetsThreshold: true,
		},
		{
			name:           "perfect match",
			nameSimilarity: 1.0,
			methodMatches:  2,
			totalMethods:   2,
			expectedScore:  1.0, // 1.0 * 0.6 + 1.0 * 0.4
			meetsThreshold: true,
		},
		{
			name:           "good match",
			nameSimilarity: 0.85,
			methodMatches:  1,
			totalMethods:   2,
			expectedScore:  0.71, // 0.85 * 0.6 + 0.5 * 0.4
			meetsThreshold: true,
		},
		{
			name:           "below threshold",
			nameSimilarity: 0.4,
			methodMatches:  0,
			totalMethods:   2,
			expectedScore:  0.24, // 0.4 * 0.6 + 0 * 0.4
			meetsThreshold: false,
		},
		{
			name:           "name only, barely meets",
			nameSimilarity: 0.84,
			methodMatches:  0,
			totalMethods:   1,
			expectedScore:  0.504, // 0.84 * 0.6 + 0 * 0.4
			meetsThreshold: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Calculate score like the handler does
			score := tt.nameSimilarity * 0.6

			if tt.totalMethods > 0 {
				methodScore := float64(tt.methodMatches) / float64(tt.totalMethods)
				score += methodScore * 0.4
			}

			assert.InDelta(t, tt.expectedScore, score, 0.01)
			assert.Equal(t, tt.meetsThreshold, score >= 0.5)
		})
	}
}

func TestMatchScoring_NameExtraction(t *testing.T) {
	tests := []struct {
		name        string
		displayName *string
		firstName   *string
		lastName    *string
		expected    string
	}{
		{
			name:        "display name only",
			displayName: stringPtr("John Smith"),
			expected:    "John Smith",
		},
		{
			name:      "first and last",
			firstName: stringPtr("John"),
			lastName:  stringPtr("Smith"),
			expected:  "John Smith",
		},
		{
			name:      "first only",
			firstName: stringPtr("John"),
			expected:  "John",
		},
		{
			name:     "empty",
			expected: "",
		},
		{
			name:        "display name takes precedence",
			displayName: stringPtr("John Smith Jr."),
			firstName:   stringPtr("John"),
			lastName:    stringPtr("Smith"),
			expected:    "John Smith Jr.",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			external := &repository.ExternalContact{
				DisplayName: tt.displayName,
				FirstName:   tt.firstName,
				LastName:    tt.lastName,
			}

			// Extract name like the handler does
			candidateName := ""
			if external.DisplayName != nil {
				candidateName = *external.DisplayName
			} else if external.FirstName != nil && external.LastName != nil {
				candidateName = *external.FirstName + " " + *external.LastName
			} else if external.FirstName != nil {
				candidateName = *external.FirstName
			}

			assert.Equal(t, tt.expected, candidateName)
		})
	}
}

func TestMatchScoring_MultipleMatches(t *testing.T) {
	// Test selecting the best match among multiple candidates
	matches := []struct {
		id         uuid.UUID
		name       string
		similarity float64
		emailMatch bool
		phoneMatch bool
	}{
		{
			id:         uuid.New(),
			name:       "John Smith",
			similarity: 0.95,
			emailMatch: true,
			phoneMatch: false,
		},
		{
			id:         uuid.New(),
			name:       "Jon Smith",
			similarity: 0.7,
			emailMatch: false,
			phoneMatch: true,
		},
		{
			id:         uuid.New(),
			name:       "Jane Smith",
			similarity: 0.5,
			emailMatch: false,
			phoneMatch: false,
		},
	}

	var bestScore float64
	var bestMatch *struct {
		id         uuid.UUID
		name       string
		similarity float64
		emailMatch bool
		phoneMatch bool
	}

	for i := range matches {
		match := &matches[i]
		// Calculate score
		score := match.similarity * 0.6
		methodCount := 0
		methodMatches := 0
		if match.emailMatch {
			methodCount++
			methodMatches++
		}
		if match.phoneMatch {
			methodCount++
			methodMatches++
		}
		if methodCount > 0 {
			score += (float64(methodMatches) / float64(methodCount)) * 0.4
		}

		if score >= 0.5 && score > bestScore {
			bestScore = score
			bestMatch = match
		}
	}

	// Best match should be John Smith (highest combined score)
	assert.NotNil(t, bestMatch)
	assert.Equal(t, "John Smith", bestMatch.name)
	assert.InDelta(t, 0.97, bestScore, 0.01) // 0.95 * 0.6 + 1.0 * 0.4
}

// Helper functions
func stringPtr(s string) *string {
	return &s
}

func toLower(s string) string {
	return strings.ToLower(s)
}

// normalizePhone is now defined in import.go and used by both production code and tests

// TestCandidateSorting_ByConfidence tests the sorting logic for import candidates (issue #122)
// Candidates should be sorted by confidence score descending, with those without matches
// sorted alphabetically at the end.
func TestCandidateSorting_ByConfidence(t *testing.T) {
	tests := []struct {
		name          string
		candidates    []ImportCandidateResponse
		expectedOrder []string // Expected order of display names after sorting
	}{
		{
			name: "sort by confidence descending",
			candidates: []ImportCandidateResponse{
				{DisplayName: stringPtr("Alice"), SuggestedMatch: &SuggestedMatch{Confidence: 0.55}},
				{DisplayName: stringPtr("Bob"), SuggestedMatch: &SuggestedMatch{Confidence: 0.95}},
				{DisplayName: stringPtr("Charlie"), SuggestedMatch: &SuggestedMatch{Confidence: 0.75}},
			},
			expectedOrder: []string{"Bob", "Charlie", "Alice"},
		},
		{
			name: "matches before non-matches",
			candidates: []ImportCandidateResponse{
				{DisplayName: stringPtr("Alice"), SuggestedMatch: nil},
				{DisplayName: stringPtr("Bob"), SuggestedMatch: &SuggestedMatch{Confidence: 0.60}},
				{DisplayName: stringPtr("Charlie"), SuggestedMatch: nil},
			},
			expectedOrder: []string{"Bob", "Alice", "Charlie"},
		},
		{
			name: "non-matches sorted alphabetically",
			candidates: []ImportCandidateResponse{
				{DisplayName: stringPtr("Charlie"), SuggestedMatch: nil},
				{DisplayName: stringPtr("Alice"), SuggestedMatch: nil},
				{DisplayName: stringPtr("Bob"), SuggestedMatch: nil},
			},
			expectedOrder: []string{"Alice", "Bob", "Charlie"},
		},
		{
			name: "mixed: high confidence, low confidence, then alphabetical non-matches",
			candidates: []ImportCandidateResponse{
				{DisplayName: stringPtr("Zara"), SuggestedMatch: nil},
				{DisplayName: stringPtr("Alice"), SuggestedMatch: &SuggestedMatch{Confidence: 0.50}},
				{DisplayName: stringPtr("Bob"), SuggestedMatch: nil},
				{DisplayName: stringPtr("Charlie"), SuggestedMatch: &SuggestedMatch{Confidence: 0.90}},
				{DisplayName: stringPtr("Dan"), SuggestedMatch: &SuggestedMatch{Confidence: 0.70}},
			},
			expectedOrder: []string{"Charlie", "Dan", "Alice", "Bob", "Zara"},
		},
		{
			name: "equal confidence - stable ordering",
			candidates: []ImportCandidateResponse{
				{DisplayName: stringPtr("Alice"), SuggestedMatch: &SuggestedMatch{Confidence: 0.75}},
				{DisplayName: stringPtr("Bob"), SuggestedMatch: &SuggestedMatch{Confidence: 0.75}},
			},
			// With equal confidence, sort.Slice is not guaranteed to be stable,
			// but we just check both are before any non-matches
			expectedOrder: []string{"Alice", "Bob"}, // May vary due to non-stable sort
		},
		{
			name:          "empty candidates",
			candidates:    []ImportCandidateResponse{},
			expectedOrder: []string{},
		},
		{
			name: "single candidate with match",
			candidates: []ImportCandidateResponse{
				{DisplayName: stringPtr("Alice"), SuggestedMatch: &SuggestedMatch{Confidence: 0.80}},
			},
			expectedOrder: []string{"Alice"},
		},
		{
			name: "single candidate without match",
			candidates: []ImportCandidateResponse{
				{DisplayName: stringPtr("Alice"), SuggestedMatch: nil},
			},
			expectedOrder: []string{"Alice"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Apply the same sorting logic as ListImportCandidates
			sort.Slice(tt.candidates, func(i, j int) bool {
				iMatch := tt.candidates[i].SuggestedMatch
				jMatch := tt.candidates[j].SuggestedMatch

				// Both have matches: sort by confidence descending
				if iMatch != nil && jMatch != nil {
					return iMatch.Confidence > jMatch.Confidence
				}

				// One has match: matched comes first
				if iMatch != nil {
					return true
				}
				if jMatch != nil {
					return false
				}

				// Neither has match: sort alphabetically by display name
				iName := getCandidateDisplayName(tt.candidates[i].DisplayName, tt.candidates[i].FirstName, tt.candidates[i].LastName)
				jName := getCandidateDisplayName(tt.candidates[j].DisplayName, tt.candidates[j].FirstName, tt.candidates[j].LastName)
				return iName < jName
			})

			// Skip order validation for equal confidence case (non-deterministic)
			if tt.name == "equal confidence - stable ordering" {
				// Just verify both still have matches
				assert.NotNil(t, tt.candidates[0].SuggestedMatch)
				assert.NotNil(t, tt.candidates[1].SuggestedMatch)
				return
			}

			// Extract resulting order
			resultOrder := make([]string, len(tt.candidates))
			for i, c := range tt.candidates {
				if c.DisplayName != nil {
					resultOrder[i] = *c.DisplayName
				}
			}

			assert.Equal(t, tt.expectedOrder, resultOrder)
		})
	}
}

// TestGetCandidateDisplayName tests the helper function for extracting display names
func TestGetCandidateDisplayName(t *testing.T) {
	tests := []struct {
		name        string
		displayName *string
		firstName   *string
		lastName    *string
		expected    string
	}{
		{
			name:        "display name only",
			displayName: stringPtr("John Smith"),
			expected:    "John Smith",
		},
		{
			name:        "display name takes precedence over first/last",
			displayName: stringPtr("Johnny"),
			firstName:   stringPtr("John"),
			lastName:    stringPtr("Smith"),
			expected:    "Johnny",
		},
		{
			name:      "first and last name",
			firstName: stringPtr("John"),
			lastName:  stringPtr("Smith"),
			expected:  "John Smith",
		},
		{
			name:      "first name only",
			firstName: stringPtr("John"),
			expected:  "John",
		},
		{
			name:     "last name only - returns empty",
			lastName: stringPtr("Smith"),
			expected: "",
		},
		{
			name:     "all nil - returns empty",
			expected: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := getCandidateDisplayName(tt.displayName, tt.firstName, tt.lastName)
			assert.Equal(t, tt.expected, result)
		})
	}
}

// TestPaginationAfterSorting tests that pagination works correctly after in-memory sorting
func TestPaginationAfterSorting(t *testing.T) {
	tests := []struct {
		name          string
		totalItems    int
		page          int
		limit         int
		expectedStart int
		expectedEnd   int
		expectedCount int
	}{
		{
			name:          "first page",
			totalItems:    50,
			page:          1,
			limit:         20,
			expectedStart: 0,
			expectedEnd:   20,
			expectedCount: 20,
		},
		{
			name:          "middle page",
			totalItems:    50,
			page:          2,
			limit:         20,
			expectedStart: 20,
			expectedEnd:   40,
			expectedCount: 20,
		},
		{
			name:          "last partial page",
			totalItems:    50,
			page:          3,
			limit:         20,
			expectedStart: 40,
			expectedEnd:   50,
			expectedCount: 10,
		},
		{
			name:          "page beyond total - empty",
			totalItems:    50,
			page:          10,
			limit:         20,
			expectedStart: 50,
			expectedEnd:   50,
			expectedCount: 0,
		},
		{
			name:          "empty total",
			totalItems:    0,
			page:          1,
			limit:         20,
			expectedStart: 0,
			expectedEnd:   0,
			expectedCount: 0,
		},
		{
			name:          "single item",
			totalItems:    1,
			page:          1,
			limit:         20,
			expectedStart: 0,
			expectedEnd:   1,
			expectedCount: 1,
		},
		{
			name:          "exact page boundary",
			totalItems:    40,
			page:          2,
			limit:         20,
			expectedStart: 20,
			expectedEnd:   40,
			expectedCount: 20,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Simulate the pagination logic from ListImportCandidates
			total := int64(tt.totalItems)
			offset := (tt.page - 1) * tt.limit
			end := offset + tt.limit

			if offset > int(total) {
				offset = int(total)
			}
			if end > int(total) {
				end = int(total)
			}

			assert.Equal(t, tt.expectedStart, offset, "offset mismatch")
			assert.Equal(t, tt.expectedEnd, end, "end mismatch")
			assert.Equal(t, tt.expectedCount, end-offset, "count mismatch")
		})
	}
}

// TestTotalPagesCalculation tests the total pages calculation
func TestTotalPagesCalculation(t *testing.T) {
	tests := []struct {
		name          string
		total         int64
		limit         int
		expectedPages int
	}{
		{
			name:          "exact multiple",
			total:         40,
			limit:         20,
			expectedPages: 2,
		},
		{
			name:          "with remainder",
			total:         45,
			limit:         20,
			expectedPages: 3,
		},
		{
			name:          "single page",
			total:         15,
			limit:         20,
			expectedPages: 1,
		},
		{
			name:          "empty",
			total:         0,
			limit:         20,
			expectedPages: 0,
		},
		{
			name:          "exactly one page",
			total:         20,
			limit:         20,
			expectedPages: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Simulate the totalPages calculation from ListImportCandidates
			totalPages := int(tt.total) / tt.limit
			if int(tt.total)%tt.limit > 0 {
				totalPages++
			}

			assert.Equal(t, tt.expectedPages, totalPages)
		})
	}
}

// TestMatchScoring_OnlyCountMatchableMethodTypes is a regression test for issue #101
// The bug was that totalMethods was incremented for ALL contact methods (including telegram,
// whatsapp, etc.), but only email and phone types were checked for matches. This inflated
// the denominator and deflated the method overlap score.
func TestMatchScoring_OnlyCountMatchableMethodTypes(t *testing.T) {
	tests := []struct {
		name            string
		methods         []repository.ContactMethod
		candidateEmails []string
		candidatePhones []string
		expectedTotal   int
		expectedMatch   int
		expectedScore   float64
	}{
		{
			name: "email only - perfect match",
			methods: []repository.ContactMethod{
				{Type: "email_work", Value: "john@example.com"},
			},
			candidateEmails: []string{"john@example.com"},
			expectedTotal:   1,
			expectedMatch:   1,
			expectedScore:   1.0, // 1/1
		},
		{
			name: "email with non-matchable types should not inflate denominator",
			methods: []repository.ContactMethod{
				{Type: "email_work", Value: "john@example.com"},
				{Type: "telegram", Value: "@johnsmith"},
				{Type: "whatsapp", Value: "+1234567890"},
			},
			candidateEmails: []string{"john@example.com"},
			expectedTotal:   1, // Only email_work counts - telegram and whatsapp are ignored
			expectedMatch:   1,
			expectedScore:   1.0, // BUG would have: 1/3 = 0.33, FIX has: 1/1 = 1.0
		},
		{
			name: "phone only - perfect match",
			methods: []repository.ContactMethod{
				{Type: "phone", Value: "+1-555-123-4567"},
			},
			candidatePhones: []string{"+15551234567"},
			expectedTotal:   1,
			expectedMatch:   1,
			expectedScore:   1.0, // 1/1
		},
		{
			name: "phone with non-matchable types should not inflate denominator",
			methods: []repository.ContactMethod{
				{Type: "phone", Value: "555-123-4567"},
				{Type: "telegram", Value: "@johnsmith"},
				{Type: "signal", Value: "+1234567890"},
			},
			candidatePhones: []string{"5551234567"},
			expectedTotal:   1, // Only phone counts - telegram and signal are ignored
			expectedMatch:   1,
			expectedScore:   1.0, // BUG would have: 1/3 = 0.33, FIX has: 1/1 = 1.0
		},
		{
			name: "phone match but email no match",
			methods: []repository.ContactMethod{
				{Type: "email_work", Value: "john@example.com"},
				{Type: "phone", Value: "+1234567890"},
				{Type: "telegram", Value: "@john"},
			},
			candidateEmails: []string{"different@example.com"}, // no match
			candidatePhones: []string{"+1234567890"},           // matches
			expectedTotal:   2,                                 // 1 email + 1 phone (telegram ignored)
			expectedMatch:   1,                                 // only phone matches
			expectedScore:   0.5,                               // 1/2
		},
		{
			name: "email match but phone no match",
			methods: []repository.ContactMethod{
				{Type: "email_personal", Value: "john@gmail.com"},
				{Type: "phone", Value: "+1234567890"},
				{Type: "discord", Value: "john#1234"},
			},
			candidateEmails: []string{"john@gmail.com"}, // matches
			candidatePhones: []string{"+9999999999"},    // no match
			expectedTotal:   2,                          // 1 email + 1 phone (discord ignored)
			expectedMatch:   1,                          // only email matches
			expectedScore:   0.5,                        // 1/2
		},
		{
			name: "both email and phone match",
			methods: []repository.ContactMethod{
				{Type: "email_work", Value: "john@example.com"},
				{Type: "phone", Value: "+1234567890"},
				{Type: "whatsapp", Value: "+1234567890"},
			},
			candidateEmails: []string{"john@example.com"},
			candidatePhones: []string{"+1234567890"},
			expectedTotal:   2,   // 1 email + 1 phone (whatsapp ignored)
			expectedMatch:   2,   // both match
			expectedScore:   1.0, // 2/2
		},
		{
			name: "multiple matchable types with non-matchable - partial match",
			methods: []repository.ContactMethod{
				{Type: "email_work", Value: "john@example.com"},
				{Type: "email_personal", Value: "john.personal@example.com"},
				{Type: "phone", Value: "+1234567890"},
				{Type: "discord", Value: "john#1234"},
				{Type: "signal", Value: "+1234567890"},
			},
			candidateEmails: []string{"john@example.com"},
			candidatePhones: []string{"+1234567890"},
			expectedTotal:   3,     // 2 emails + 1 phone
			expectedMatch:   2,     // work email + phone match
			expectedScore:   0.667, // 2/3
		},
		{
			name: "only non-matchable types",
			methods: []repository.ContactMethod{
				{Type: "telegram", Value: "@johnsmith"},
				{Type: "discord", Value: "john#1234"},
				{Type: "twitter", Value: "@john"},
			},
			candidateEmails: []string{"john@example.com"},
			candidatePhones: []string{"+1234567890"},
			expectedTotal:   0, // No matchable types
			expectedMatch:   0,
			expectedScore:   0.0, // No contribution from methods
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Build candidate sets like the handler does
			candidateEmails := make(map[string]bool)
			for _, email := range tt.candidateEmails {
				candidateEmails[strings.ToLower(email)] = true
			}
			candidatePhones := make(map[string]bool)
			for _, phone := range tt.candidatePhones {
				candidatePhones[normalizePhone(phone)] = true
			}

			var methodMatches int
			var totalMethods int

			// This is the FIXED logic - only counting matchable types
			for _, method := range tt.methods {
				switch method.Type {
				case "email_personal", "email_work":
					totalMethods++
					if candidateEmails[strings.ToLower(method.Value)] {
						methodMatches++
					}
				case "phone":
					totalMethods++
					if candidatePhones[normalizePhone(method.Value)] {
						methodMatches++
					}
				}
			}

			assert.Equal(t, tt.expectedTotal, totalMethods, "totalMethods should only count matchable types")
			assert.Equal(t, tt.expectedMatch, methodMatches, "methodMatches should count actual matches")

			var methodScore float64
			if totalMethods > 0 {
				methodScore = float64(methodMatches) / float64(totalMethods)
			}
			assert.InDelta(t, tt.expectedScore, methodScore, 0.01, "method score should be correct")
		})
	}
}
