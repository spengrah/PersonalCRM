package handlers

import (
	"strings"
	"testing"

	"github.com/go-playground/validator/v10"
	"github.com/stretchr/testify/assert"
)

func TestSeedExternalContactInput_Validation(t *testing.T) {
	v := validator.New()
	tests := []struct {
		name    string
		input   SeedExternalContactInput
		isValid bool
	}{
		{
			name: "valid minimal input (display_name only)",
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
			name: "valid telegram seed with metadata.username and no display_name",
			input: SeedExternalContactInput{
				Source:   "telegram",
				Metadata: map[string]any{"username": "@daledobeck"},
			},
			isValid: true,
		},
		{
			name: "valid with first/last name, no display_name",
			input: SeedExternalContactInput{
				FirstName: "Dale",
				LastName:  "Dobeck",
			},
			isValid: true,
		},
		{
			name: "invalid source rejected",
			input: SeedExternalContactInput{
				DisplayName: "Test User",
				Source:      "bogus",
			},
			isValid: false,
		},
		{
			name: "display_name exceeds max length",
			input: SeedExternalContactInput{
				DisplayName: strings.Repeat("a", 256),
			},
			isValid: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := v.Struct(tt.input)
			if tt.isValid {
				assert.NoError(t, err)
			} else {
				assert.Error(t, err)
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

func TestSeedContactInput_Validation(t *testing.T) {
	tests := []struct {
		name    string
		input   SeedContactInput
		isValid bool
		reason  string
	}{
		{
			name: "valid minimal input",
			input: SeedContactInput{
				FullName: "Test Contact",
			},
			isValid: true,
		},
		{
			name: "valid full input",
			input: SeedContactInput{
				FullName: "Test Contact",
				Location: "San Francisco, CA",
				Notes:    "Met at a conference",
				Cadence:  "weekly",
				Methods: []SeedContactMethodInput{
					{Type: "email", Value: "test@example.com", IsPrimary: true},
				},
				LastContactedDaysAgo: 5,
			},
			isValid: true,
		},
		{
			name: "valid with all cadence types",
			input: SeedContactInput{
				FullName: "Test Contact",
				Cadence:  "biannual",
			},
			isValid: true,
		},
		{
			name: "empty full name",
			input: SeedContactInput{
				FullName: "",
			},
			isValid: false,
			reason:  "full_name is required",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.isValid {
				assert.NotEmpty(t, tt.input.FullName)
			} else {
				assert.Empty(t, tt.input.FullName, tt.reason)
			}
		})
	}
}

func TestSeedContactMethodInput_Validation(t *testing.T) {
	tests := []struct {
		name    string
		input   SeedContactMethodInput
		isValid bool
		reason  string
	}{
		{
			name: "valid email method",
			input: SeedContactMethodInput{
				Type:      "email",
				Value:     "test@example.com",
				IsPrimary: true,
			},
			isValid: true,
		},
		{
			name: "valid phone method",
			input: SeedContactMethodInput{
				Type:  "phone",
				Value: "+1234567890",
			},
			isValid: true,
		},
		{
			name: "valid telegram method",
			input: SeedContactMethodInput{
				Type:  "telegram",
				Value: "@username",
			},
			isValid: true,
		},
		{
			name: "valid discord method",
			input: SeedContactMethodInput{
				Type:  "discord",
				Value: "username#1234",
			},
			isValid: true,
		},
		{
			name: "valid twitter method",
			input: SeedContactMethodInput{
				Type:  "twitter",
				Value: "@handle",
			},
			isValid: true,
		},
		{
			name: "valid signal method",
			input: SeedContactMethodInput{
				Type:  "signal",
				Value: "+1234567890",
			},
			isValid: true,
		},
		{
			name: "valid gchat method",
			input: SeedContactMethodInput{
				Type:  "gchat",
				Value: "user@gmail.com",
			},
			isValid: true,
		},
		{
			name: "valid whatsapp method",
			input: SeedContactMethodInput{
				Type:  "whatsapp",
				Value: "+1234567890",
			},
			isValid: true,
		},
		{
			name: "empty type",
			input: SeedContactMethodInput{
				Type:  "",
				Value: "test@example.com",
			},
			isValid: false,
			reason:  "type is required",
		},
		{
			name: "empty value",
			input: SeedContactMethodInput{
				Type:  "email",
				Value: "",
			},
			isValid: false,
			reason:  "value is required",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.isValid {
				assert.NotEmpty(t, tt.input.Type)
				assert.NotEmpty(t, tt.input.Value)
			} else {
				// Check which field is invalid
				switch tt.reason {
				case "type is required":
					assert.Empty(t, tt.input.Type)
				case "value is required":
					assert.Empty(t, tt.input.Value)
				}
			}
		})
	}
}

func TestSeedContactsRequest_Validation(t *testing.T) {
	tests := []struct {
		name    string
		req     SeedContactsRequest
		isValid bool
		reason  string
	}{
		{
			name: "valid request",
			req: SeedContactsRequest{
				Prefix: "w0-1234567890",
				Contacts: []SeedContactInput{
					{FullName: "Test Contact"},
				},
			},
			isValid: true,
		},
		{
			name: "valid request with multiple contacts",
			req: SeedContactsRequest{
				Prefix: "w1-1234567890",
				Contacts: []SeedContactInput{
					{FullName: "Contact 1"},
					{FullName: "Contact 2"},
					{FullName: "Contact 3"},
				},
			},
			isValid: true,
		},
		{
			name: "empty prefix",
			req: SeedContactsRequest{
				Prefix: "",
				Contacts: []SeedContactInput{
					{FullName: "Test Contact"},
				},
			},
			isValid: false,
			reason:  "prefix is required",
		},
		{
			name: "empty contacts array",
			req: SeedContactsRequest{
				Prefix:   "w0-1234567890",
				Contacts: []SeedContactInput{},
			},
			isValid: false,
			reason:  "contacts array must not be empty",
		},
		{
			name: "nil contacts array",
			req: SeedContactsRequest{
				Prefix:   "w0-1234567890",
				Contacts: nil,
			},
			isValid: false,
			reason:  "contacts array is required",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.isValid {
				assert.NotEmpty(t, tt.req.Prefix)
				assert.NotEmpty(t, tt.req.Contacts)
			} else {
				if tt.reason == "prefix is required" {
					assert.Empty(t, tt.req.Prefix)
				} else {
					assert.Empty(t, tt.req.Contacts)
				}
			}
		})
	}
}
