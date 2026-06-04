package identity

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestNormalizeEmail(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "lowercase",
			input:    "John.Doe@Example.COM",
			expected: "john.doe@example.com",
		},
		{
			name:     "trim whitespace",
			input:    "  john@example.com  ",
			expected: "john@example.com",
		},
		{
			name:     "already normalized",
			input:    "john@example.com",
			expected: "john@example.com",
		},
		{
			name:     "empty string",
			input:    "",
			expected: "",
		},
		{
			name:     "unicode email",
			input:    "JÖHN@EXAMPLE.COM",
			expected: "jöhn@example.com",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := Normalize(tt.input, IdentifierTypeEmail)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestNormalizePhone(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "US number with dashes",
			input:    "555-123-4567",
			expected: "+15551234567",
		},
		{
			name:     "US number with parentheses",
			input:    "(555) 123-4567",
			expected: "+15551234567",
		},
		{
			name:     "US number with spaces",
			input:    "555 123 4567",
			expected: "+15551234567",
		},
		{
			name:     "US number with +1 prefix",
			input:    "+1-555-123-4567",
			expected: "+15551234567",
		},
		{
			name:     "US number with 1 prefix",
			input:    "1-555-123-4567",
			expected: "+15551234567",
		},
		{
			name:     "international number with +",
			input:    "+44 20 7946 0958",
			expected: "+442079460958",
		},
		{
			name:     "international number without +",
			input:    "44 20 7946 0958",
			expected: "+442079460958",
		},
		{
			name:     "empty string",
			input:    "",
			expected: "",
		},
		{
			name:     "whitespace only",
			input:    "   ",
			expected: "",
		},
		{
			name:     "already E.164 format",
			input:    "+15551234567",
			expected: "+15551234567",
		},
		{
			name:     "German number",
			input:    "+49 30 12345678",
			expected: "+493012345678",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := Normalize(tt.input, IdentifierTypePhone)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestNormalizeTelegram(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "with @ prefix",
			input:    "@JohnDoe",
			expected: "johndoe",
		},
		{
			name:     "without @ prefix",
			input:    "JohnDoe",
			expected: "johndoe",
		},
		{
			name:     "with whitespace",
			input:    "  @johndoe  ",
			expected: "johndoe",
		},
		{
			name:     "already lowercase",
			input:    "johndoe",
			expected: "johndoe",
		},
		{
			name:     "empty string",
			input:    "",
			expected: "",
		},
		{
			name:     "numbers in handle",
			input:    "@John123",
			expected: "john123",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := Normalize(tt.input, IdentifierTypeTelegram)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestNormalizeIMessageEmail(t *testing.T) {
	result := Normalize("John@iCloud.COM", IdentifierTypeIMessageEmail)
	assert.Equal(t, "john@icloud.com", result)
}

func TestNormalizeIMessagePhone(t *testing.T) {
	result := Normalize("+1 (555) 123-4567", IdentifierTypeIMessagePhone)
	assert.Equal(t, "+15551234567", result)
}

func TestNormalizeWhatsApp(t *testing.T) {
	result := Normalize("+1 555 123 4567", IdentifierTypeWhatsApp)
	assert.Equal(t, "+15551234567", result)
}

// TestNormalizeGChat asserts a Google Chat sender address normalizes
// identically to an email (lowercase + trim) — GChat addresses ARE emails,
// matching the contact_method trigger's gchat handling exactly.
func TestNormalizeGChat(t *testing.T) {
	tests := []struct {
		name  string
		input string
	}{
		{"mixed case", "John.Doe@Example.COM"},
		{"leading/trailing whitespace", "  john.doe@example.com  "},
		{"already normalized", "john.doe@example.com"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gchat := Normalize(tt.input, IdentifierTypeGChat)
			email := Normalize(tt.input, IdentifierTypeEmail)
			assert.Equal(t, email, gchat,
				"gchat normalization must match email normalization")
		})
	}
	assert.Equal(t, "john.doe@example.com",
		Normalize("John.Doe@Example.COM", IdentifierTypeGChat))
}

// TestMapIdentifierTypeToContactMethodTypes_GChatReturnsNil is a regression
// guard: IdentifierTypeGChat is intentionally absent from the mapping (§5.X.7
// DEFERRED). The ListGChatIdentitiesForSync sqlc query is the single source of
// truth for the (gchat, email) dual-source set; a mapping branch here would be
// dead code and a second encoding that could drift. If someone adds a gchat
// MatchOrCreate caller later, they must also revisit this rationale.
func TestMapIdentifierTypeToContactMethodTypes_GChatReturnsNil(t *testing.T) {
	result := MapIdentifierTypeToContactMethodTypes(IdentifierTypeGChat)
	assert.Nil(t, result)
}

func TestNormalizeAnarlogHuman(t *testing.T) {
	// Anarlog UUIDs are already canonical; Normalize trims whitespace
	// and lowercases so case-mixed daemon emits compare equal to stored.
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "canonical uuid",
			input:    "fe0e4dd9-30b3-4c2b-9c80-6c7eb1f44a91",
			expected: "fe0e4dd9-30b3-4c2b-9c80-6c7eb1f44a91",
		},
		{
			name:     "uppercase uuid",
			input:    "FE0E4DD9-30B3-4C2B-9C80-6C7EB1F44A91",
			expected: "fe0e4dd9-30b3-4c2b-9c80-6c7eb1f44a91",
		},
		{
			name:     "with whitespace",
			input:    "  fe0e4dd9-30b3-4c2b-9c80-6c7eb1f44a91  ",
			expected: "fe0e4dd9-30b3-4c2b-9c80-6c7eb1f44a91",
		},
		{
			name:     "empty string",
			input:    "",
			expected: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := Normalize(tt.input, IdentifierTypeAnarlogHuman)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestMapIdentifierTypeToContactMethodTypes_AnarlogHumanReturnsNil(t *testing.T) {
	// Anarlog UUIDs do not search against contact_method rows directly
	// — the contact_id linkage comes from the associated
	// external_contact's email/phone match path. Returning nil keeps
	// the identity-service search path short-circuiting.
	result := MapIdentifierTypeToContactMethodTypes(IdentifierTypeAnarlogHuman)
	assert.Nil(t, result)
}

func TestMapIdentifierTypeToContactMethodTypes(t *testing.T) {
	tests := []struct {
		name     string
		idType   IdentifierType
		expected []ContactMethodType
	}{
		{
			name:     "email maps to email",
			idType:   IdentifierTypeEmail,
			expected: []ContactMethodType{ContactMethodTypeEmail},
		},
		{
			name:     "phone",
			idType:   IdentifierTypePhone,
			expected: []ContactMethodType{ContactMethodTypePhone},
		},
		{
			name:     "telegram",
			idType:   IdentifierTypeTelegram,
			expected: []ContactMethodType{ContactMethodTypeTelegram},
		},
		{
			name:     "imessage email maps to email",
			idType:   IdentifierTypeIMessageEmail,
			expected: []ContactMethodType{ContactMethodTypeEmail},
		},
		{
			name:     "imessage phone",
			idType:   IdentifierTypeIMessagePhone,
			expected: []ContactMethodType{ContactMethodTypePhone},
		},
		{
			name:     "whatsapp",
			idType:   IdentifierTypeWhatsApp,
			expected: []ContactMethodType{ContactMethodTypeWhatsApp, ContactMethodTypePhone},
		},
		{
			name:     "unknown type",
			idType:   IdentifierType("unknown"),
			expected: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := MapIdentifierTypeToContactMethodTypes(tt.idType)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestDetectIdentifierType(t *testing.T) {
	tests := []struct {
		name       string
		identifier string
		expected   IdentifierType
	}{
		{
			name:       "email with @",
			identifier: "john@example.com",
			expected:   IdentifierTypeEmail,
		},
		{
			name:       "phone with +",
			identifier: "+15551234567",
			expected:   IdentifierTypePhone,
		},
		{
			name:       "phone without + mostly digits",
			identifier: "555-123-4567",
			expected:   IdentifierTypePhone,
		},
		{
			name:       "ambiguous defaults to email",
			identifier: "johndoe",
			expected:   IdentifierTypeEmail,
		},
		{
			name:       "phone number",
			identifier: "5551234567",
			expected:   IdentifierTypePhone,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := DetectIdentifierType(tt.identifier)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestNormalizePhoneEdgeCases(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "only non-digits",
			input:    "abc-def-ghij",
			expected: "",
		},
		{
			name:     "short number",
			input:    "123",
			expected: "+123",
		},
		{
			name:     "seven digits",
			input:    "1234567",
			expected: "+1234567",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := Normalize(tt.input, IdentifierTypePhone)
			assert.Equal(t, tt.expected, result)
		})
	}
}
