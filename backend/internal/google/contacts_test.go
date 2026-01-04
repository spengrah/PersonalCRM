package google

import (
	"testing"
	"time"

	"personal-crm/backend/internal/repository"

	"github.com/stretchr/testify/assert"
	"google.golang.org/api/people/v1"
)

func TestContactsProvider_Config(t *testing.T) {
	provider := NewContactsProvider(nil, nil, nil, nil)
	config := provider.Config()

	assert.Equal(t, ContactsSourceName, config.Name)
	assert.Equal(t, "Google Contacts", config.DisplayName)
	assert.Equal(t, repository.SyncStrategyFetchAll, config.Strategy)
	assert.True(t, config.SupportsMultiAccount)
	assert.True(t, config.SupportsDiscovery)
	assert.Equal(t, ContactsDefaultInterval, config.DefaultInterval)
}

func TestContactsProvider_ConvertPersonToRequest(t *testing.T) {
	provider := NewContactsProvider(nil, nil, nil, nil)
	accountID := "test@gmail.com"

	tests := []struct {
		name     string
		person   *people.Person
		expected repository.UpsertExternalContactRequest
	}{
		{
			name: "full contact with all fields",
			person: &people.Person{
				ResourceName: "people/123456",
				Etag:         "etag123",
				Names: []*people.Name{
					{
						DisplayName: "John Doe",
						GivenName:   "John",
						FamilyName:  "Doe",
					},
				},
				EmailAddresses: []*people.EmailAddress{
					{
						Value: "john@example.com",
						Type:  "work",
						Metadata: &people.FieldMetadata{
							Primary: true,
						},
					},
					{
						Value: "john.doe@personal.com",
						Type:  "home",
					},
				},
				PhoneNumbers: []*people.PhoneNumber{
					{
						Value: "+1-555-123-4567",
						Type:  "mobile",
					},
				},
				Addresses: []*people.Address{
					{
						FormattedValue: "123 Main St, City, State 12345",
						Type:           "home",
					},
				},
				Organizations: []*people.Organization{
					{
						Name:  "Acme Corp",
						Title: "Software Engineer",
					},
				},
				Birthdays: []*people.Birthday{
					{
						Date: &people.Date{
							Year:  1990,
							Month: 6,
							Day:   15,
						},
					},
				},
				Photos: []*people.Photo{
					{
						Url: "https://example.com/photo.jpg",
					},
				},
			},
			expected: repository.UpsertExternalContactRequest{
				Source:   ContactsSourceName,
				SourceID: "people/123456",
			},
		},
		{
			name: "minimal contact with only name",
			person: &people.Person{
				ResourceName: "people/789",
				Names: []*people.Name{
					{
						DisplayName: "Jane Smith",
						GivenName:   "Jane",
						FamilyName:  "Smith",
					},
				},
			},
			expected: repository.UpsertExternalContactRequest{
				Source:   ContactsSourceName,
				SourceID: "people/789",
			},
		},
		{
			name: "contact with only email",
			person: &people.Person{
				ResourceName: "people/abc",
				EmailAddresses: []*people.EmailAddress{
					{
						Value: "unknown@example.com",
						Type:  "other",
					},
				},
			},
			expected: repository.UpsertExternalContactRequest{
				Source:   ContactsSourceName,
				SourceID: "people/abc",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := provider.convertPersonToRequest(tt.person, accountID)

			// Check basic fields
			assert.Equal(t, tt.expected.Source, result.Source)
			assert.Equal(t, tt.expected.SourceID, result.SourceID)
			assert.Equal(t, &accountID, result.AccountID)

			// Check names if present
			if len(tt.person.Names) > 0 {
				assert.NotNil(t, result.DisplayName)
				assert.Equal(t, tt.person.Names[0].DisplayName, *result.DisplayName)
				if tt.person.Names[0].GivenName != "" {
					assert.Equal(t, tt.person.Names[0].GivenName, *result.FirstName)
				}
				if tt.person.Names[0].FamilyName != "" {
					assert.Equal(t, tt.person.Names[0].FamilyName, *result.LastName)
				}
			}

			// Check emails
			assert.Equal(t, len(tt.person.EmailAddresses), len(result.Emails))
			for i, email := range tt.person.EmailAddresses {
				assert.Equal(t, email.Value, result.Emails[i].Value)
				assert.Equal(t, email.Type, result.Emails[i].Type)
			}

			// Check phones
			assert.Equal(t, len(tt.person.PhoneNumbers), len(result.Phones))

			// Check addresses
			assert.Equal(t, len(tt.person.Addresses), len(result.Addresses))

			// Check organization
			if len(tt.person.Organizations) > 0 {
				if tt.person.Organizations[0].Name != "" {
					assert.NotNil(t, result.Organization)
					assert.Equal(t, tt.person.Organizations[0].Name, *result.Organization)
				}
				if tt.person.Organizations[0].Title != "" {
					assert.NotNil(t, result.JobTitle)
					assert.Equal(t, tt.person.Organizations[0].Title, *result.JobTitle)
				}
			}

			// Check birthday
			if len(tt.person.Birthdays) > 0 && tt.person.Birthdays[0].Date != nil {
				date := tt.person.Birthdays[0].Date
				if date.Year > 0 && date.Month > 0 && date.Day > 0 {
					assert.NotNil(t, result.Birthday)
					expectedDate := time.Date(int(date.Year), time.Month(date.Month), int(date.Day), 0, 0, 0, 0, time.UTC)
					assert.Equal(t, expectedDate, *result.Birthday)
				}
			}

			// Check photo
			if len(tt.person.Photos) > 0 && tt.person.Photos[0].Url != "" {
				assert.NotNil(t, result.PhotoURL)
				assert.Equal(t, tt.person.Photos[0].Url, *result.PhotoURL)
			}

			// Check etag
			if tt.person.Etag != "" {
				assert.NotNil(t, result.Etag)
				assert.Equal(t, tt.person.Etag, *result.Etag)
			}

			// Check synced_at is set
			assert.NotNil(t, result.SyncedAt)
		})
	}
}

func TestContactsProvider_ConvertPersonToRequest_EmptyPerson(t *testing.T) {
	provider := NewContactsProvider(nil, nil, nil, nil)
	accountID := "test@gmail.com"

	person := &people.Person{
		ResourceName: "people/empty",
	}

	result := provider.convertPersonToRequest(person, accountID)

	assert.Equal(t, ContactsSourceName, result.Source)
	assert.Equal(t, "people/empty", result.SourceID)
	assert.Equal(t, &accountID, result.AccountID)
	assert.Nil(t, result.DisplayName)
	assert.Nil(t, result.FirstName)
	assert.Nil(t, result.LastName)
	assert.Empty(t, result.Emails)
	assert.Empty(t, result.Phones)
	assert.Empty(t, result.Addresses)
	assert.Nil(t, result.Organization)
	assert.Nil(t, result.JobTitle)
	assert.Nil(t, result.Birthday)
	assert.Nil(t, result.PhotoURL)
}

func TestContactsProvider_ConvertPersonToRequest_PartialBirthday(t *testing.T) {
	provider := NewContactsProvider(nil, nil, nil, nil)
	accountID := "test@gmail.com"

	// Birthday with only month and day (no year) should not be set
	person := &people.Person{
		ResourceName: "people/partial-bday",
		Birthdays: []*people.Birthday{
			{
				Date: &people.Date{
					Year:  0, // No year
					Month: 6,
					Day:   15,
				},
			},
		},
	}

	result := provider.convertPersonToRequest(person, accountID)
	assert.Nil(t, result.Birthday, "Birthday should be nil when year is 0")
}

func TestContactsProvider_ConvertPersonToRequest_EmailPrimary(t *testing.T) {
	provider := NewContactsProvider(nil, nil, nil, nil)
	accountID := "test@gmail.com"

	person := &people.Person{
		ResourceName: "people/email-test",
		EmailAddresses: []*people.EmailAddress{
			{
				Value: "primary@example.com",
				Type:  "work",
				Metadata: &people.FieldMetadata{
					Primary: true,
				},
			},
			{
				Value:    "secondary@example.com",
				Type:     "home",
				Metadata: nil,
			},
		},
	}

	result := provider.convertPersonToRequest(person, accountID)

	assert.Len(t, result.Emails, 2)
	assert.True(t, result.Emails[0].Primary)
	assert.False(t, result.Emails[1].Primary)
}
