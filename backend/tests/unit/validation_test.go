package unit

import (
	"strings"
	"testing"

	"personal-crm/backend/internal/api/handlers"

	"github.com/go-playground/validator/v10"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var validate *validator.Validate

func init() {
	validate = validator.New()
}

// TestContactValidation_FullName tests FullName validation
func TestContactValidation_FullName(t *testing.T) {
	t.Parallel()

	type Contact struct {
		FullName string `validate:"required,min=1,max=255"`
	}

	tests := []struct {
		name      string
		fullName  string
		wantError bool
	}{
		{"Valid name", "John Doe", false},
		{"Empty name fails", "", true},
		{"Single character valid", "A", false},
		{"Max length 255 valid", strings.Repeat("a", 255), false},
		{"Exceeds max length", strings.Repeat("a", 256), true},
		{"Unicode characters valid", "José García", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			contact := Contact{FullName: tt.fullName}
			err := validate.Struct(contact)

			if tt.wantError {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
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

// TestContactValidation_Location tests Location validation
func TestContactValidation_Location(t *testing.T) {
	t.Parallel()

	type Contact struct {
		Location *string `validate:"omitempty,max=255"`
	}

	tests := []struct {
		name      string
		location  *string
		wantError bool
	}{
		{"Valid location", strPtr("San Francisco, CA"), false},
		{"Nil location valid", nil, false},
		{"Max length 255", strPtr(strings.Repeat("a", 255)), false},
		{"Exceeds max length", strPtr(strings.Repeat("a", 256)), true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			contact := Contact{Location: tt.location}
			err := validate.Struct(contact)

			if tt.wantError {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

// TestContactValidation_HowMet tests HowMet validation
func TestContactValidation_HowMet(t *testing.T) {
	t.Parallel()

	type Contact struct {
		HowMet *string `validate:"omitempty,max=500"`
	}

	tests := []struct {
		name      string
		howMet    *string
		wantError bool
	}{
		{"Valid how met", strPtr("Met at tech conference"), false},
		{"Nil how met valid", nil, false},
		{"Max length 500", strPtr(strings.Repeat("a", 500)), false},
		{"Exceeds max length", strPtr(strings.Repeat("a", 501)), true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			contact := Contact{HowMet: tt.howMet}
			err := validate.Struct(contact)

			if tt.wantError {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

// TestContactValidation_Cadence tests Cadence validation
func TestContactValidation_Cadence(t *testing.T) {
	t.Parallel()

	type Contact struct {
		Cadence *string `validate:"omitempty,oneof=weekly biweekly monthly quarterly biannual annual"`
	}

	tests := []struct {
		name      string
		cadence   *string
		wantError bool
	}{
		{"Valid weekly", strPtr("weekly"), false},
		{"Valid biweekly", strPtr("biweekly"), false},
		{"Valid monthly", strPtr("monthly"), false},
		{"Valid quarterly", strPtr("quarterly"), false},
		{"Valid biannual", strPtr("biannual"), false},
		{"Valid annual", strPtr("annual"), false},
		{"Nil cadence valid", nil, false},
		{"Invalid cadence", strPtr("daily"), true},
		{"Empty string", strPtr(""), true},
		{"Case sensitive - uppercase fails", strPtr("WEEKLY"), true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			contact := Contact{Cadence: tt.cadence}
			err := validate.Struct(contact)

			if tt.wantError {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

// TestContactValidation_Notes tests Notes validation
func TestContactValidation_Notes(t *testing.T) {
	t.Parallel()

	type Contact struct {
		Notes *string `validate:"omitempty,max=2000"`
	}

	tests := []struct {
		name      string
		notes     *string
		wantError bool
	}{
		{"Valid notes", strPtr("Met at a conference. Works in tech."), false},
		{"Nil notes valid", nil, false},
		{"Empty string valid", strPtr(""), false},
		{"Max length 2000", strPtr(strings.Repeat("a", 2000)), false},
		{"Exceeds max length", strPtr(strings.Repeat("a", 2001)), true},
		{"Multiline notes valid", strPtr("Line 1\nLine 2\nLine 3"), false},
		{"Notes with special characters", strPtr("Notes with émojis 🎉 and symbols @#$%"), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			contact := Contact{Notes: tt.notes}
			err := validate.Struct(contact)

			if tt.wantError {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

// TestContactValidation_ProfilePhoto tests ProfilePhoto URL validation
func TestContactValidation_ProfilePhoto(t *testing.T) {
	t.Parallel()

	type Contact struct {
		ProfilePhoto *string `validate:"omitempty,url,max=500"`
	}

	tests := []struct {
		name      string
		photo     *string
		wantError bool
	}{
		{"Valid HTTP URL", strPtr("http://example.com/photo.jpg"), false},
		{"Valid HTTPS URL", strPtr("https://example.com/photo.jpg"), false},
		{"Nil photo valid", nil, false},
		{"Invalid URL - no scheme", strPtr("example.com/photo.jpg"), true},
		{"Invalid URL - malformed", strPtr("not a url"), true},
		{"Max length 500", strPtr("https://example.com/" + strings.Repeat("a", 476) + ".jpg"), false},    // 19 + 476 + 4 = 499
		{"Exceeds max length", strPtr("https://example.com/" + strings.Repeat("a", 482) + ".jpg"), true}, // 19 + 482 + 4 = 505
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			contact := Contact{ProfilePhoto: tt.photo}
			err := validate.Struct(contact)

			if tt.wantError {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

// TestQueryValidation_Page tests Page validation
func TestQueryValidation_Page(t *testing.T) {
	t.Parallel()

	type Query struct {
		Page int `validate:"omitempty,min=1"`
	}

	tests := []struct {
		name      string
		page      int
		wantError bool
	}{
		{"Valid page 1", 1, false},
		{"Valid page 100", 100, false},
		{"Zero treated as omitted", 0, false}, // omitempty with int treats 0 as empty
		{"Negative fails", -1, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			query := Query{Page: tt.page}
			err := validate.Struct(query)

			if tt.wantError {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

// TestQueryValidation_Limit tests Limit validation
func TestQueryValidation_Limit(t *testing.T) {
	t.Parallel()

	type Query struct {
		Limit int `validate:"omitempty,min=1,max=1000"`
	}

	tests := []struct {
		name      string
		limit     int
		wantError bool
	}{
		{"Valid limit 1", 1, false},
		{"Valid limit 20", 20, false},
		{"Valid limit 1000", 1000, false},
		{"Zero treated as omitted", 0, false},
		{"Exceeds max", 1001, true},
		{"Negative fails", -1, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			query := Query{Limit: tt.limit}
			err := validate.Struct(query)

			if tt.wantError {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

// TestQueryValidation_Search tests Search validation
func TestQueryValidation_Search(t *testing.T) {
	t.Parallel()

	type Query struct {
		Search string `validate:"omitempty,max=255"`
	}

	tests := []struct {
		name      string
		search    string
		wantError bool
	}{
		{"Valid search", "john", false},
		{"Empty search valid", "", false},
		{"Max length 255", strings.Repeat("a", 255), false},
		{"Exceeds max length", strings.Repeat("a", 256), true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			query := Query{Search: tt.search}
			err := validate.Struct(query)

			if tt.wantError {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

// TestQueryValidation_Sort tests Sort field validation
func TestQueryValidation_Sort(t *testing.T) {
	t.Parallel()

	type Query struct {
		Sort string `validate:"omitempty,oneof=name location birthday last_contacted last_response_at contact_by cadence"`
	}

	tests := []struct {
		name      string
		sort      string
		wantError bool
	}{
		{"Valid sort - name", "name", false},
		{"Valid sort - location", "location", false},
		{"Valid sort - birthday", "birthday", false},
		{"Valid sort - last_contacted", "last_contacted", false},
		{"Valid sort - last_response_at", "last_response_at", false},
		{"Valid sort - contact_by", "contact_by", false},
		{"Valid sort - cadence", "cadence", false},
		{"Empty sort valid", "", false},
		{"Invalid sort field", "invalid", true},
		{"Case sensitive", "Name", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			query := Query{Sort: tt.sort}
			err := validate.Struct(query)

			if tt.wantError {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

// TestQueryValidation_Order tests Order validation
func TestQueryValidation_Order(t *testing.T) {
	t.Parallel()

	type Query struct {
		Order string `validate:"omitempty,oneof=asc desc"`
	}

	tests := []struct {
		name      string
		order     string
		wantError bool
	}{
		{"Valid order - asc", "asc", false},
		{"Valid order - desc", "desc", false},
		{"Empty order valid", "", false},
		{"Invalid order", "invalid", true},
		{"Case sensitive", "ASC", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			query := Query{Order: tt.order}
			err := validate.Struct(query)

			if tt.wantError {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

// TestComplexValidation_MultipleFields tests validation with multiple fields
func TestComplexValidation_MultipleFields(t *testing.T) {
	t.Parallel()

	type Contact struct {
		FullName     string  `validate:"required,min=1,max=255"`
		Cadence      *string `validate:"omitempty,oneof=weekly biweekly monthly quarterly biannual annual"`
		ProfilePhoto *string `validate:"omitempty,url,max=500"`
	}

	tests := []struct {
		name       string
		contact    Contact
		wantError  bool
		errorCount int // Expected number of validation errors
	}{
		{
			name: "All valid",
			contact: Contact{
				FullName:     "John Doe",
				Cadence:      strPtr("monthly"),
				ProfilePhoto: strPtr("https://example.com/photo.jpg"),
			},
			wantError:  false,
			errorCount: 0,
		},
		{
			name: "Missing required field",
			contact: Contact{
				FullName:     "",
				Cadence:      strPtr("monthly"),
				ProfilePhoto: strPtr("https://example.com/photo.jpg"),
			},
			wantError:  true,
			errorCount: 1,
		},
		{
			name: "Multiple invalid fields",
			contact: Contact{
				FullName:     "",
				Cadence:      strPtr("daily"),
				ProfilePhoto: strPtr("not-a-url"),
			},
			wantError:  true,
			errorCount: 3, // FullName required, Cadence invalid value, ProfilePhoto invalid URL
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validate.Struct(tt.contact)

			if tt.wantError {
				require.Error(t, err)
				validationErrors, ok := err.(validator.ValidationErrors)
				require.True(t, ok)
				assert.Equal(t, tt.errorCount, len(validationErrors))
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
