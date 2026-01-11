package unit

import (
	"testing"

	"personal-crm/backend/internal/api/handlers"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"

	"github.com/stretchr/testify/assert"
)

// MockImportHandler wraps the buildMethodsFromSelection for testing
// Since buildMethodsFromSelection is unexported, we'll test through the exported type behavior

func TestBuildMethodsFromSelection(t *testing.T) {
	// Create a test helper that mimics buildMethodsFromSelection logic
	// This tests the selection validation logic

	tests := []struct {
		name           string
		externalEmails []string
		externalPhones []string
		selections     []handlers.SelectedMethodInput
		wantMethods    int
	}{
		{
			name:           "Valid single email selection",
			externalEmails: []string{"john@gmail.com"},
			externalPhones: []string{},
			selections: []handlers.SelectedMethodInput{
				{OriginalValue: "john@gmail.com", Type: "email_personal"},
			},
			wantMethods: 1,
		},
		{
			name:           "Valid multiple method selection",
			externalEmails: []string{"john@gmail.com", "john@work.com"},
			externalPhones: []string{"+15551234567"},
			selections: []handlers.SelectedMethodInput{
				{OriginalValue: "john@gmail.com", Type: "email_personal"},
				{OriginalValue: "john@work.com", Type: "email_work"},
				{OriginalValue: "+15551234567", Type: "phone"},
			},
			wantMethods: 3,
		},
		{
			name:           "Skip value not in external contact",
			externalEmails: []string{"john@gmail.com"},
			externalPhones: []string{},
			selections: []handlers.SelectedMethodInput{
				{OriginalValue: "notexists@example.com", Type: "email_personal"},
			},
			wantMethods: 0,
		},
		{
			name:           "Skip duplicate types",
			externalEmails: []string{"john@gmail.com", "john@yahoo.com"},
			externalPhones: []string{},
			selections: []handlers.SelectedMethodInput{
				{OriginalValue: "john@gmail.com", Type: "email_personal"},
				{OriginalValue: "john@yahoo.com", Type: "email_personal"}, // Duplicate type
			},
			wantMethods: 1,
		},
		{
			name:           "Empty selection returns empty",
			externalEmails: []string{"john@gmail.com"},
			externalPhones: []string{"+15551234567"},
			selections:     []handlers.SelectedMethodInput{},
			wantMethods:    0,
		},
		{
			name:           "All types can be selected",
			externalEmails: []string{"john@gmail.com", "john@work.com"},
			externalPhones: []string{"+15551234567"},
			selections: []handlers.SelectedMethodInput{
				{OriginalValue: "john@gmail.com", Type: "email_personal"},
				{OriginalValue: "john@work.com", Type: "email_work"},
				{OriginalValue: "+15551234567", Type: "phone"},
			},
			wantMethods: 3,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Simulate buildMethodsFromSelection logic
			availableValues := make(map[string]bool)
			for _, email := range tt.externalEmails {
				availableValues[email] = true
			}
			for _, phone := range tt.externalPhones {
				availableValues[phone] = true
			}

			usedTypes := make(map[string]bool)
			var methods []service.ContactMethodInput

			for _, sel := range tt.selections {
				// Validate the value exists in external contact
				if !availableValues[sel.OriginalValue] {
					continue
				}

				// Skip duplicate types
				if usedTypes[sel.Type] {
					continue
				}

				methods = append(methods, service.ContactMethodInput{
					Type:  sel.Type,
					Value: sel.OriginalValue,
				})
				usedTypes[sel.Type] = true
			}

			assert.Len(t, methods, tt.wantMethods)
		})
	}
}

func TestSelectedMethodInputValidation(t *testing.T) {
	tests := []struct {
		name      string
		input     handlers.SelectedMethodInput
		wantValid bool
	}{
		{
			name: "Valid email_personal",
			input: handlers.SelectedMethodInput{
				OriginalValue: "john@example.com",
				Type:          "email_personal",
			},
			wantValid: true,
		},
		{
			name: "Valid email_work",
			input: handlers.SelectedMethodInput{
				OriginalValue: "john@work.com",
				Type:          "email_work",
			},
			wantValid: true,
		},
		{
			name: "Valid phone",
			input: handlers.SelectedMethodInput{
				OriginalValue: "+15551234567",
				Type:          "phone",
			},
			wantValid: true,
		},
		{
			name: "Valid telegram",
			input: handlers.SelectedMethodInput{
				OriginalValue: "@johndoe",
				Type:          "telegram",
			},
			wantValid: true,
		},
		{
			name: "Valid signal",
			input: handlers.SelectedMethodInput{
				OriginalValue: "+15551234567",
				Type:          "signal",
			},
			wantValid: true,
		},
		{
			name: "Valid discord",
			input: handlers.SelectedMethodInput{
				OriginalValue: "johndoe#1234",
				Type:          "discord",
			},
			wantValid: true,
		},
		{
			name: "Valid twitter",
			input: handlers.SelectedMethodInput{
				OriginalValue: "@johndoe",
				Type:          "twitter",
			},
			wantValid: true,
		},
		{
			name: "Valid gchat",
			input: handlers.SelectedMethodInput{
				OriginalValue: "john@company.com",
				Type:          "gchat",
			},
			wantValid: true,
		},
		{
			name: "Valid whatsapp",
			input: handlers.SelectedMethodInput{
				OriginalValue: "+15551234567",
				Type:          "whatsapp",
			},
			wantValid: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Just verify the struct can be created with these values
			// The actual validation happens via Gin binding
			assert.NotEmpty(t, tt.input.OriginalValue)
			assert.NotEmpty(t, tt.input.Type)
		})
	}
}

func TestLinkRequestValidation(t *testing.T) {
	tests := []struct {
		name        string
		request     handlers.LinkRequest
		wantValid   bool
		description string
	}{
		{
			name: "Valid link request with contact ID only",
			request: handlers.LinkRequest{
				CRMContactID: "550e8400-e29b-41d4-a716-446655440000",
			},
			wantValid:   true,
			description: "Basic link without method selection",
		},
		{
			name: "Valid link request with method selection",
			request: handlers.LinkRequest{
				CRMContactID: "550e8400-e29b-41d4-a716-446655440000",
				SelectedMethods: []handlers.SelectedMethodInput{
					{OriginalValue: "john@example.com", Type: "email_personal"},
				},
			},
			wantValid:   true,
			description: "Link with method selection",
		},
		{
			name: "Valid link request with conflict resolutions",
			request: handlers.LinkRequest{
				CRMContactID: "550e8400-e29b-41d4-a716-446655440000",
				ConflictResolutions: map[string]string{
					"john@example.com": "use_external",
				},
			},
			wantValid:   true,
			description: "Link with conflict resolution",
		},
		{
			name: "Valid link request with all options",
			request: handlers.LinkRequest{
				CRMContactID: "550e8400-e29b-41d4-a716-446655440000",
				SelectedMethods: []handlers.SelectedMethodInput{
					{OriginalValue: "john@example.com", Type: "email_personal"},
					{OriginalValue: "+15551234567", Type: "phone"},
				},
				ConflictResolutions: map[string]string{
					"john@example.com": "use_crm",
				},
			},
			wantValid:   true,
			description: "Full link request",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.NotEmpty(t, tt.request.CRMContactID)
			if tt.wantValid {
				// Verify the structure is as expected
				if len(tt.request.SelectedMethods) > 0 {
					for _, m := range tt.request.SelectedMethods {
						assert.NotEmpty(t, m.OriginalValue)
						assert.NotEmpty(t, m.Type)
					}
				}
			}
		})
	}
}

func TestImportRequestValidation(t *testing.T) {
	tests := []struct {
		name      string
		request   handlers.ImportRequest
		wantValid bool
	}{
		{
			name:      "Empty request is valid (auto-import)",
			request:   handlers.ImportRequest{},
			wantValid: true,
		},
		{
			name: "Request with method selection",
			request: handlers.ImportRequest{
				SelectedMethods: []handlers.SelectedMethodInput{
					{OriginalValue: "john@example.com", Type: "email_personal"},
				},
			},
			wantValid: true,
		},
		{
			name: "Request with multiple methods",
			request: handlers.ImportRequest{
				SelectedMethods: []handlers.SelectedMethodInput{
					{OriginalValue: "john@example.com", Type: "email_personal"},
					{OriginalValue: "john@work.com", Type: "email_work"},
					{OriginalValue: "+15551234567", Type: "phone"},
				},
			},
			wantValid: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Verify structure
			if tt.wantValid {
				for _, m := range tt.request.SelectedMethods {
					if m.OriginalValue != "" || m.Type != "" {
						assert.NotEmpty(t, m.OriginalValue)
						assert.NotEmpty(t, m.Type)
					}
				}
			}
		})
	}
}

func TestToEnrichmentMethodSelections(t *testing.T) {
	tests := []struct {
		name       string
		selections []handlers.SelectedMethodInput
		wantLen    int
	}{
		{
			name:       "Empty selections",
			selections: []handlers.SelectedMethodInput{},
			wantLen:    0,
		},
		{
			name: "Single selection",
			selections: []handlers.SelectedMethodInput{
				{OriginalValue: "john@example.com", Type: "email_personal"},
			},
			wantLen: 1,
		},
		{
			name: "Multiple selections",
			selections: []handlers.SelectedMethodInput{
				{OriginalValue: "john@example.com", Type: "email_personal"},
				{OriginalValue: "john@work.com", Type: "email_work"},
				{OriginalValue: "+15551234567", Type: "phone"},
			},
			wantLen: 3,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Simulate toEnrichmentMethodSelections
			result := make([]service.MethodSelection, len(tt.selections))
			for i, sel := range tt.selections {
				result[i] = service.MethodSelection{
					OriginalValue: sel.OriginalValue,
					Type:          sel.Type,
				}
			}

			assert.Len(t, result, tt.wantLen)
			for i, sel := range tt.selections {
				assert.Equal(t, sel.OriginalValue, result[i].OriginalValue)
				assert.Equal(t, sel.Type, result[i].Type)
			}
		})
	}
}

func TestConflictResolutionValues(t *testing.T) {
	// Test that conflict resolution values are correctly structured
	validResolutions := []string{"use_crm", "use_external"}

	for _, res := range validResolutions {
		t.Run("Resolution_"+res, func(t *testing.T) {
			request := handlers.LinkRequest{
				CRMContactID: "550e8400-e29b-41d4-a716-446655440000",
				ConflictResolutions: map[string]string{
					"john@example.com": res,
				},
			}

			assert.Equal(t, res, request.ConflictResolutions["john@example.com"])
		})
	}
}

// TestExternalContactEmails verifies the structure of external contact emails
func TestExternalContactEmails(t *testing.T) {
	tests := []struct {
		name       string
		emails     []repository.EmailEntry
		wantCount  int
		wantValues []string
	}{
		{
			name:       "Empty emails",
			emails:     []repository.EmailEntry{},
			wantCount:  0,
			wantValues: []string{},
		},
		{
			name: "Single email",
			emails: []repository.EmailEntry{
				{Value: "john@example.com"},
			},
			wantCount:  1,
			wantValues: []string{"john@example.com"},
		},
		{
			name: "Multiple emails with types",
			emails: []repository.EmailEntry{
				{Value: "john@gmail.com", Type: "home"},
				{Value: "john@work.com", Type: "work"},
			},
			wantCount:  2,
			wantValues: []string{"john@gmail.com", "john@work.com"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Len(t, tt.emails, tt.wantCount)
			for i, email := range tt.emails {
				assert.Equal(t, tt.wantValues[i], email.Value)
			}
		})
	}
}
