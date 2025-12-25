package unit

import (
	"strings"
	"testing"

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

// TestContactValidation_Email tests Email validation
func TestContactValidation_Email(t *testing.T) {
	type Contact struct {
		Email *string `validate:"omitempty,email,max=255"`
	}

	tests := []struct {
		name      string
		email     *string
		wantError bool
	}{
		{"Valid email", strPtr("john@example.com"), false},
		{"Nil email valid (omitempty)", nil, false},
		{"Invalid email format", strPtr("not-an-email"), true},
		{"Missing @ symbol", strPtr("john.example.com"), true},
		{"Missing domain", strPtr("john@"), true},
		{"Valid with subdomain", strPtr("john@mail.example.com"), false},
		{"Valid with + sign", strPtr("john+test@example.com"), false},
		{"Max length 255", strPtr(strings.Repeat("a", 244) + "@test.com"), false},    // 244 + 1 + 8 + 1 + 3 = 257, actually let's be more careful
		{"Exceeds max length", strPtr(strings.Repeat("a", 247) + "@test.com"), true}, // Should exceed 255
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			contact := Contact{Email: tt.email}
			err := validate.Struct(contact)

			if tt.wantError {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

// TestContactValidation_Phone tests Phone validation
func TestContactValidation_Phone(t *testing.T) {
	type Contact struct {
		Phone *string `validate:"omitempty,max=50"`
	}

	tests := []struct {
		name      string
		phone     *string
		wantError bool
	}{
		{"Valid phone", strPtr("+1-555-0123"), false},
		{"Nil phone valid (omitempty)", nil, false},
		{"Max length 50", strPtr(strings.Repeat("1", 50)), false},
		{"Exceeds max length", strPtr(strings.Repeat("1", 51)), true},
		{"Various formats allowed", strPtr("(555) 123-4567"), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			contact := Contact{Phone: tt.phone}
			err := validate.Struct(contact)

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

// TestContactValidation_ProfilePhoto tests ProfilePhoto URL validation
func TestContactValidation_ProfilePhoto(t *testing.T) {
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
	type Query struct {
		Sort string `validate:"omitempty,oneof=name location birthday last_contacted"`
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

// TestReminderValidation_Title tests Reminder title validation
func TestReminderValidation_Title(t *testing.T) {
	type Reminder struct {
		Title string `validate:"required,max=255"`
	}

	tests := []struct {
		name      string
		title     string
		wantError bool
	}{
		{"Valid title", "Follow up with John", false},
		{"Empty title fails", "", true},
		{"Max length 255", strings.Repeat("a", 255), false},
		{"Exceeds max length", strings.Repeat("a", 256), true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reminder := Reminder{Title: tt.title}
			err := validate.Struct(reminder)

			if tt.wantError {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

// TestReminderValidation_Description tests Reminder description validation
func TestReminderValidation_Description(t *testing.T) {
	type Reminder struct {
		Description *string `validate:"omitempty,max=1000"`
	}

	tests := []struct {
		name      string
		desc      *string
		wantError bool
	}{
		{"Valid description", strPtr("Check in about project"), false},
		{"Nil description valid", nil, false},
		{"Max length 1000", strPtr(strings.Repeat("a", 1000)), false},
		{"Exceeds max length", strPtr(strings.Repeat("a", 1001)), true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reminder := Reminder{Description: tt.desc}
			err := validate.Struct(reminder)

			if tt.wantError {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

// TestTimeEntryValidation_Description tests TimeEntry description validation
func TestTimeEntryValidation_Description(t *testing.T) {
	type TimeEntry struct {
		Description string `validate:"required,max=500"`
	}

	tests := []struct {
		name      string
		desc      string
		wantError bool
	}{
		{"Valid description", "Worked on API development", false},
		{"Empty description fails", "", true},
		{"Max length 500", strings.Repeat("a", 500), false},
		{"Exceeds max length", strings.Repeat("a", 501), true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			entry := TimeEntry{Description: tt.desc}
			err := validate.Struct(entry)

			if tt.wantError {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

// TestTimeEntryValidation_Project tests TimeEntry project validation
func TestTimeEntryValidation_Project(t *testing.T) {
	type TimeEntry struct {
		Project *string `validate:"omitempty,max=100"`
	}

	tests := []struct {
		name      string
		project   *string
		wantError bool
	}{
		{"Valid project", strPtr("PersonalCRM"), false},
		{"Nil project valid", nil, false},
		{"Max length 100", strPtr(strings.Repeat("a", 100)), false},
		{"Exceeds max length", strPtr(strings.Repeat("a", 101)), true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			entry := TimeEntry{Project: tt.project}
			err := validate.Struct(entry)

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
	type Contact struct {
		FullName string  `validate:"required,min=1,max=255"`
		Email    *string `validate:"omitempty,email,max=255"`
		Cadence  *string `validate:"omitempty,oneof=weekly biweekly monthly quarterly biannual annual"`
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
				FullName: "John Doe",
				Email:    strPtr("john@example.com"),
				Cadence:  strPtr("monthly"),
			},
			wantError:  false,
			errorCount: 0,
		},
		{
			name: "Missing required field",
			contact: Contact{
				FullName: "",
				Email:    strPtr("john@example.com"),
				Cadence:  strPtr("monthly"),
			},
			wantError:  true,
			errorCount: 1,
		},
		{
			name: "Multiple invalid fields",
			contact: Contact{
				FullName: "",
				Email:    strPtr("not-an-email"),
				Cadence:  strPtr("daily"),
			},
			wantError:  true,
			errorCount: 3, // FullName required, Email invalid format, Cadence invalid value
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
