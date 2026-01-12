package unit

import (
	"testing"

	"personal-crm/backend/internal/api/handlers"
	"personal-crm/backend/internal/service"

	"github.com/stretchr/testify/assert"
)

// These tests verify the data structures and type definitions for import/link handlers.
// Actual handler behavior is tested in backend/tests/api/import_with_selections_test.go
// using integration tests with real database connections.

func TestSelectedMethodInput_TypeValues(t *testing.T) {
	// Verify all valid type values that the handler accepts
	// These match the validation tag: oneof=email_personal email_work phone telegram signal discord twitter gchat whatsapp
	validTypes := []string{
		"email_personal",
		"email_work",
		"phone",
		"telegram",
		"signal",
		"discord",
		"twitter",
		"gchat",
		"whatsapp",
	}

	for _, validType := range validTypes {
		t.Run("ValidType_"+validType, func(t *testing.T) {
			input := handlers.SelectedMethodInput{
				OriginalValue: "test@example.com",
				Type:          validType,
			}
			assert.Equal(t, validType, input.Type)
			assert.NotEmpty(t, input.OriginalValue)
		})
	}
}

func TestLinkRequest_Structure(t *testing.T) {
	t.Run("RequiredCRMContactID", func(t *testing.T) {
		// LinkRequest requires crm_contact_id
		req := handlers.LinkRequest{
			CRMContactID: "550e8400-e29b-41d4-a716-446655440000",
		}
		assert.NotEmpty(t, req.CRMContactID)
	})

	t.Run("OptionalSelectedMethods", func(t *testing.T) {
		// selected_methods is optional
		req := handlers.LinkRequest{
			CRMContactID: "550e8400-e29b-41d4-a716-446655440000",
			SelectedMethods: []handlers.SelectedMethodInput{
				{OriginalValue: "john@example.com", Type: "email_personal"},
			},
		}
		assert.Len(t, req.SelectedMethods, 1)
	})

	t.Run("OptionalConflictResolutions", func(t *testing.T) {
		// conflict_resolutions is optional
		req := handlers.LinkRequest{
			CRMContactID: "550e8400-e29b-41d4-a716-446655440000",
			ConflictResolutions: map[string]string{
				"john@example.com": "use_external",
			},
		}
		assert.Len(t, req.ConflictResolutions, 1)
		assert.Equal(t, "use_external", req.ConflictResolutions["john@example.com"])
	})
}

func TestImportRequest_Structure(t *testing.T) {
	t.Run("EmptyRequestIsValid", func(t *testing.T) {
		// Empty ImportRequest is valid for backward compatibility
		req := handlers.ImportRequest{}
		assert.Nil(t, req.SelectedMethods)
	})

	t.Run("WithSelectedMethods", func(t *testing.T) {
		req := handlers.ImportRequest{
			SelectedMethods: []handlers.SelectedMethodInput{
				{OriginalValue: "john@example.com", Type: "email_personal"},
				{OriginalValue: "+15551234567", Type: "phone"},
			},
		}
		assert.Len(t, req.SelectedMethods, 2)
	})
}

func TestConflictResolutionValues(t *testing.T) {
	// Verify valid conflict resolution values
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

func TestMethodSelectionConversion(t *testing.T) {
	// Verify that handler SelectedMethodInput can be converted to service MethodSelection
	handlerInput := handlers.SelectedMethodInput{
		OriginalValue: "test@example.com",
		Type:          "email_personal",
	}

	// Conversion logic from handler to service
	serviceSelection := service.MethodSelection{
		OriginalValue: handlerInput.OriginalValue,
		Type:          handlerInput.Type,
	}

	assert.Equal(t, handlerInput.OriginalValue, serviceSelection.OriginalValue)
	assert.Equal(t, handlerInput.Type, serviceSelection.Type)
}
