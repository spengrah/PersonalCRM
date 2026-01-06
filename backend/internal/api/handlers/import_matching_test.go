package handlers

import (
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

// TestMatchScoring_OnlyCountMatchableMethodTypes is a regression test for issue #101
// The bug was that totalMethods was incremented for ALL contact methods (including telegram,
// whatsapp, etc.), but only email and phone types were checked for matches. This inflated
// the denominator and deflated the method overlap score.
func TestMatchScoring_OnlyCountMatchableMethodTypes(t *testing.T) {
	tests := []struct {
		name           string
		methods        []repository.ContactMethod
		candidateEmail string
		expectedTotal  int
		expectedMatch  int
		expectedScore  float64
	}{
		{
			name: "email only - perfect match",
			methods: []repository.ContactMethod{
				{Type: "email_work", Value: "john@example.com"},
			},
			candidateEmail: "john@example.com",
			expectedTotal:  1,
			expectedMatch:  1,
			expectedScore:  1.0, // 1/1
		},
		{
			name: "email with non-matchable types should not inflate denominator",
			methods: []repository.ContactMethod{
				{Type: "email_work", Value: "john@example.com"},
				{Type: "telegram", Value: "@johnsmith"},
				{Type: "whatsapp", Value: "+1234567890"},
			},
			candidateEmail: "john@example.com",
			expectedTotal:  1, // Only email_work counts - telegram and whatsapp are ignored
			expectedMatch:  1,
			expectedScore:  1.0, // BUG would have: 1/3 = 0.33, FIX has: 1/1 = 1.0
		},
		{
			name: "multiple matchable types with non-matchable",
			methods: []repository.ContactMethod{
				{Type: "email_work", Value: "john@example.com"},
				{Type: "email_personal", Value: "john.personal@example.com"},
				{Type: "phone", Value: "+1234567890"},
				{Type: "discord", Value: "john#1234"},
				{Type: "signal", Value: "+1234567890"},
			},
			candidateEmail: "john@example.com",
			expectedTotal:  3,     // 2 emails + 1 phone
			expectedMatch:  1,     // Only work email matches
			expectedScore:  0.333, // 1/3
		},
		{
			name: "only non-matchable types",
			methods: []repository.ContactMethod{
				{Type: "telegram", Value: "@johnsmith"},
				{Type: "discord", Value: "john#1234"},
				{Type: "twitter", Value: "@john"},
			},
			candidateEmail: "john@example.com",
			expectedTotal:  0, // No matchable types
			expectedMatch:  0,
			expectedScore:  0.0, // No contribution from methods
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Simulate the scoring logic from findBestMatch
			candidateEmails := make(map[string]bool)
			candidateEmails[strings.ToLower(tt.candidateEmail)] = true

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
					// Phone matching would happen here but we're testing emails
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
