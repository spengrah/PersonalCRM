package unit

import (
	"strings"
	"testing"

	"personal-crm/backend/internal/api/handlers"

	"github.com/go-playground/validator/v10"
	"github.com/stretchr/testify/assert"
)

var validate *validator.Validate

func init() {
	validate = validator.New()
}

// TestContactMethodValidation_Type tests method type validation
func TestContactMethodValidation_Type(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		method    handlers.ContactMethodRequest
		wantError bool
	}{
		{"Valid email personal", handlers.ContactMethodRequest{Type: "email", Value: "john@example.com"}, false},
		{"Valid phone", handlers.ContactMethodRequest{Type: "phone", Value: "+1-555-0123"}, false},
		{"Missing type", handlers.ContactMethodRequest{Type: "", Value: "john@example.com"}, true},
		{"Invalid type", handlers.ContactMethodRequest{Type: "fax", Value: "123"}, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validate.Struct(tt.method)

			if tt.wantError {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

// TestContactMethodValidation_Value tests method value validation
func TestContactMethodValidation_Value(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		method    handlers.ContactMethodRequest
		wantError bool
	}{
		{"Valid value", handlers.ContactMethodRequest{Type: "email", Value: "john@example.com"}, false},
		{"Empty value", handlers.ContactMethodRequest{Type: "email", Value: ""}, true},
		{"Max length 255", handlers.ContactMethodRequest{Type: "phone", Value: strings.Repeat("1", 255)}, false},
		{"Exceeds max length", handlers.ContactMethodRequest{Type: "phone", Value: strings.Repeat("1", 256)}, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validate.Struct(tt.method)

			if tt.wantError {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

// Helper function to create string pointers
func strPtr(s string) *string {
	return &s
}
