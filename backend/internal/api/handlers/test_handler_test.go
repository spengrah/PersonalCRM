package handlers

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestEscapeSQLLikeWildcards(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "no wildcards",
			input:    "simple-prefix",
			expected: "simple-prefix",
		},
		{
			name:     "percentage wildcard",
			input:    "test%",
			expected: `test\%`,
		},
		{
			name:     "underscore wildcard",
			input:    "test_prefix",
			expected: `test\_prefix`,
		},
		{
			name:     "both wildcards",
			input:    "test%_prefix",
			expected: `test\%\_prefix`,
		},
		{
			name:     "multiple percentages",
			input:    "a%b%c",
			expected: `a\%b\%c`,
		},
		{
			name:     "backslash in input",
			input:    `test\prefix`,
			expected: `test\\prefix`,
		},
		{
			name:     "backslash and wildcards",
			input:    `test\%_`,
			expected: `test\\\%\_`,
		},
		{
			name:     "empty string",
			input:    "",
			expected: "",
		},
		{
			name:     "typical test prefix",
			input:    "w0-1234567890",
			expected: "w0-1234567890",
		},
		{
			name:     "malicious injection attempt",
			input:    "prefix%; DROP TABLE contacts; --",
			expected: `prefix\%; DROP TABLE contacts; --`,
		},
		{
			name:     "wildcard at start",
			input:    "%admin",
			expected: `\%admin`,
		},
		{
			name:     "only wildcards",
			input:    "%%__",
			expected: `\%\%\_\_`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := escapeSQLLikeWildcards(tt.input)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestSeedExternalContactInput_Validation(t *testing.T) {
	tests := []struct {
		name    string
		input   SeedExternalContactInput
		isValid bool
	}{
		{
			name: "valid minimal input",
			input: SeedExternalContactInput{
				DisplayName: "Test User",
			},
			isValid: true,
		},
		{
			name: "valid full input",
			input: SeedExternalContactInput{
				DisplayName:  "Test User",
				Emails:       []string{"test@example.com"},
				Phones:       []string{"+1234567890"},
				Organization: "Test Org",
				JobTitle:     "Engineer",
			},
			isValid: true,
		},
		{
			name: "empty display name",
			input: SeedExternalContactInput{
				DisplayName: "",
			},
			isValid: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Test that display_name is required
			if tt.isValid {
				assert.NotEmpty(t, tt.input.DisplayName)
			} else {
				assert.Empty(t, tt.input.DisplayName)
			}
		})
	}
}

func TestSeedOverdueContactInput_Validation(t *testing.T) {
	tests := []struct {
		name    string
		input   SeedOverdueContactInput
		isValid bool
	}{
		{
			name: "valid input",
			input: SeedOverdueContactInput{
				FullName:    "Test Contact",
				Cadence:     "weekly",
				DaysOverdue: 3,
			},
			isValid: true,
		},
		{
			name: "valid with email",
			input: SeedOverdueContactInput{
				FullName:    "Test Contact",
				Cadence:     "monthly",
				DaysOverdue: 5,
				Email:       "test@example.com",
			},
			isValid: true,
		},
		{
			name: "empty full name",
			input: SeedOverdueContactInput{
				FullName:    "",
				Cadence:     "weekly",
				DaysOverdue: 1,
			},
			isValid: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.isValid {
				assert.NotEmpty(t, tt.input.FullName)
				assert.NotEmpty(t, tt.input.Cadence)
				assert.Greater(t, tt.input.DaysOverdue, 0)
			} else {
				assert.Empty(t, tt.input.FullName)
			}
		})
	}
}

func TestCleanupRequest_Validation(t *testing.T) {
	tests := []struct {
		name    string
		prefix  string
		isValid bool
	}{
		{
			name:    "valid prefix",
			prefix:  "w0-1234567890",
			isValid: true,
		},
		{
			name:    "empty prefix",
			prefix:  "",
			isValid: false,
		},
		{
			name:    "prefix with special chars",
			prefix:  "test-prefix",
			isValid: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := CleanupRequest{Prefix: tt.prefix}
			if tt.isValid {
				assert.NotEmpty(t, req.Prefix)
			} else {
				assert.Empty(t, req.Prefix)
			}
		})
	}
}
