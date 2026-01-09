package matching

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
		{"mixed case", "John.Doe@Example.COM", "john.doe@example.com"},
		{"trim whitespace", "  john@example.com  ", "john@example.com"},
		{"already normalized", "john@example.com", "john@example.com"},
		{"empty", "", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, NormalizeEmail(tt.input))
		})
	}
}

func TestNormalizePhoneLoose(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{"dashes removed", "555-1234", "5551234"},
		{"spaces removed", "555 1234", "5551234"},
		{"parentheses removed", "(555) 123-4567", "5551234567"},
		{"leading plus preserved", "+1 (555) 123-4567", "+15551234567"},
		{"empty", "", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, NormalizePhoneLoose(tt.input))
		})
	}
}

func TestNormalizePhoneE164(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{"US number with dashes", "555-123-4567", "+15551234567"},
		{"US number with 1 prefix", "1-555-123-4567", "+15551234567"},
		{"US number with +1 prefix", "+1-555-123-4567", "+15551234567"},
		{"international with plus", "+44 20 7946 0958", "+442079460958"},
		{"international without plus", "44 20 7946 0958", "+442079460958"},
		{"empty", "", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, NormalizePhoneE164(tt.input))
		})
	}
}
