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

func TestMapIdentifierTypeToContactMethodTypes(t *testing.T) {
	tests := []struct {
		name     string
		idType   IdentifierType
		expected []ContactMethodType
	}{
		{
			name:     "email maps to both email_personal and email_work",
			idType:   IdentifierTypeEmail,
			expected: []ContactMethodType{ContactMethodTypeEmailPersonal, ContactMethodTypeEmailWork},
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
			name:     "imessage email maps to both email types",
			idType:   IdentifierTypeIMessageEmail,
			expected: []ContactMethodType{ContactMethodTypeEmailPersonal, ContactMethodTypeEmailWork},
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
