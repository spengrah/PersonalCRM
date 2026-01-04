package tests

import (
	"context"
	"os"
	"testing"

	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/identity"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestIdentityRepository_Integration tests the identity repository with a real database
func TestIdentityRepository_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}

	ctx := context.Background()

	// Run migrations first
	migrationsPath := getMigrationsPath()
	if err := db.RunMigrations(databaseURL, migrationsPath); err != nil {
		t.Fatalf("Failed to run migrations: %v", err)
	}

	cfg := config.TestConfig()
	cfg.Database.URL = databaseURL

	database, err := db.NewDatabase(ctx, cfg.Database)
	if err != nil {
		t.Skipf("Could not connect to database: %v", err)
	}
	defer database.Close()

	repo := repository.NewIdentityRepository(database.Queries)

	t.Run("UpsertAndGetIdentity", func(t *testing.T) {
		rawIdentifier := "TEST.USER@EXAMPLE.COM"
		displayName := "Test User"

		// Create an identity
		ident, err := repo.Upsert(ctx, repository.UpsertIdentityRequest{
			Identifier:     "test.user@example.com",
			IdentifierType: identity.IdentifierTypeEmail,
			RawIdentifier:  &rawIdentifier,
			Source:         "test_source",
			MatchType:      repository.MatchTypeUnmatched,
			DisplayName:    &displayName,
			MessageCount:   1,
		})
		require.NoError(t, err)
		require.NotNil(t, ident)

		// Verify fields
		assert.Equal(t, "test.user@example.com", ident.Identifier)
		assert.Equal(t, identity.IdentifierTypeEmail, ident.IdentifierType)
		assert.Equal(t, "test_source", ident.Source)
		assert.Equal(t, repository.MatchTypeUnmatched, ident.MatchType)
		assert.Equal(t, "Test User", *ident.DisplayName)
		assert.Equal(t, int32(1), ident.MessageCount)
		assert.NotEqual(t, uuid.Nil, ident.ID)

		// Get by ID
		found, err := repo.GetByID(ctx, ident.ID)
		require.NoError(t, err)
		assert.Equal(t, ident.ID, found.ID)
		assert.Equal(t, ident.Identifier, found.Identifier)

		// Get by identifier
		found, err = repo.GetByIdentifier(ctx, identity.IdentifierTypeEmail, "test.user@example.com", "test_source")
		require.NoError(t, err)
		assert.Equal(t, ident.ID, found.ID)

		// Clean up
		err = repo.Delete(ctx, ident.ID)
		require.NoError(t, err)
	})

	t.Run("UpsertIncrementsMessageCount", func(t *testing.T) {
		// Create identity
		ident, err := repo.Upsert(ctx, repository.UpsertIdentityRequest{
			Identifier:     "increment.test@example.com",
			IdentifierType: identity.IdentifierTypeEmail,
			Source:         "test_source",
			MatchType:      repository.MatchTypeUnmatched,
			MessageCount:   1,
		})
		require.NoError(t, err)
		assert.Equal(t, int32(1), ident.MessageCount)

		// Upsert again - should increment
		ident, err = repo.Upsert(ctx, repository.UpsertIdentityRequest{
			Identifier:     "increment.test@example.com",
			IdentifierType: identity.IdentifierTypeEmail,
			Source:         "test_source",
			MatchType:      repository.MatchTypeUnmatched,
			MessageCount:   5,
		})
		require.NoError(t, err)
		assert.Equal(t, int32(6), ident.MessageCount)

		// Clean up
		err = repo.Delete(ctx, ident.ID)
		require.NoError(t, err)
	})

	t.Run("LinkAndUnlinkIdentity", func(t *testing.T) {
		// Create a contact first
		contactRepo := repository.NewContactRepository(database.Queries)
		contact, err := contactRepo.CreateContact(ctx, repository.CreateContactRequest{
			FullName: "Link Test Contact",
		})
		require.NoError(t, err)
		defer func() { _ = contactRepo.HardDeleteContact(ctx, contact.ID) }()

		// Create an unmatched identity
		ident, err := repo.Upsert(ctx, repository.UpsertIdentityRequest{
			Identifier:     "link.test@example.com",
			IdentifierType: identity.IdentifierTypeEmail,
			Source:         "test_source",
			MatchType:      repository.MatchTypeUnmatched,
			MessageCount:   1,
		})
		require.NoError(t, err)
		defer func() { _ = repo.Delete(ctx, ident.ID) }()

		assert.Nil(t, ident.ContactID)

		// Link to contact
		confidence := 1.0
		linked, err := repo.LinkToContact(ctx, repository.LinkIdentityRequest{
			IdentityID:      ident.ID,
			ContactID:       contact.ID,
			MatchType:       repository.MatchTypeManual,
			MatchConfidence: &confidence,
		})
		require.NoError(t, err)
		assert.NotNil(t, linked.ContactID)
		assert.Equal(t, contact.ID, *linked.ContactID)
		assert.Equal(t, repository.MatchTypeManual, linked.MatchType)

		// Unlink
		unlinked, err := repo.UnlinkFromContact(ctx, ident.ID)
		require.NoError(t, err)
		assert.Nil(t, unlinked.ContactID)
		assert.Equal(t, repository.MatchTypeUnmatched, unlinked.MatchType)
	})

	t.Run("ListUnmatched", func(t *testing.T) {
		// Create some unmatched identities
		ident1, err := repo.Upsert(ctx, repository.UpsertIdentityRequest{
			Identifier:     "unmatched1@example.com",
			IdentifierType: identity.IdentifierTypeEmail,
			Source:         "test_source",
			MatchType:      repository.MatchTypeUnmatched,
			MessageCount:   10,
		})
		require.NoError(t, err)
		defer func() { _ = repo.Delete(ctx, ident1.ID) }()

		ident2, err := repo.Upsert(ctx, repository.UpsertIdentityRequest{
			Identifier:     "unmatched2@example.com",
			IdentifierType: identity.IdentifierTypeEmail,
			Source:         "test_source",
			MatchType:      repository.MatchTypeUnmatched,
			MessageCount:   5,
		})
		require.NoError(t, err)
		defer func() { _ = repo.Delete(ctx, ident2.ID) }()

		// List unmatched
		unmatched, err := repo.ListUnmatched(ctx, 100, 0)
		require.NoError(t, err)

		// Should contain our test identities, sorted by message_count desc
		found1, found2 := false, false
		var idx1, idx2 int
		for i, u := range unmatched {
			if u.ID == ident1.ID {
				found1 = true
				idx1 = i
			}
			if u.ID == ident2.ID {
				found2 = true
				idx2 = i
			}
		}
		assert.True(t, found1, "ident1 should be in unmatched list")
		assert.True(t, found2, "ident2 should be in unmatched list")
		assert.Less(t, idx1, idx2, "ident1 (10 msgs) should come before ident2 (5 msgs)")

		// Count unmatched
		count, err := repo.CountUnmatched(ctx)
		require.NoError(t, err)
		assert.GreaterOrEqual(t, count, int64(2))
	})

	t.Run("ListForContact", func(t *testing.T) {
		// Create a contact
		contactRepo := repository.NewContactRepository(database.Queries)
		contact, err := contactRepo.CreateContact(ctx, repository.CreateContactRequest{
			FullName: "List For Contact Test",
		})
		require.NoError(t, err)
		defer func() { _ = contactRepo.HardDeleteContact(ctx, contact.ID) }()

		// Create identities linked to contact
		ident1, err := repo.Upsert(ctx, repository.UpsertIdentityRequest{
			Identifier:     "contact.ident1@example.com",
			IdentifierType: identity.IdentifierTypeEmail,
			Source:         "gmail",
			ContactID:      &contact.ID,
			MatchType:      repository.MatchTypeExact,
			MessageCount:   1,
		})
		require.NoError(t, err)
		defer func() { _ = repo.Delete(ctx, ident1.ID) }()

		ident2, err := repo.Upsert(ctx, repository.UpsertIdentityRequest{
			Identifier:     "+15551234567",
			IdentifierType: identity.IdentifierTypePhone,
			Source:         "imessage",
			ContactID:      &contact.ID,
			MatchType:      repository.MatchTypeExact,
			MessageCount:   1,
		})
		require.NoError(t, err)
		defer func() { _ = repo.Delete(ctx, ident2.ID) }()

		// List for contact
		identities, err := repo.ListForContact(ctx, contact.ID)
		require.NoError(t, err)
		assert.Len(t, identities, 2)
	})

	t.Run("BulkLinkToContact", func(t *testing.T) {
		// Create a contact
		contactRepo := repository.NewContactRepository(database.Queries)
		contact, err := contactRepo.CreateContact(ctx, repository.CreateContactRequest{
			FullName: "Bulk Link Test",
		})
		require.NoError(t, err)
		defer func() { _ = contactRepo.HardDeleteContact(ctx, contact.ID) }()

		// Create unmatched identities
		ident1, err := repo.Upsert(ctx, repository.UpsertIdentityRequest{
			Identifier:     "bulk1@example.com",
			IdentifierType: identity.IdentifierTypeEmail,
			Source:         "test_source",
			MatchType:      repository.MatchTypeUnmatched,
			MessageCount:   1,
		})
		require.NoError(t, err)
		defer func() { _ = repo.Delete(ctx, ident1.ID) }()

		ident2, err := repo.Upsert(ctx, repository.UpsertIdentityRequest{
			Identifier:     "bulk2@example.com",
			IdentifierType: identity.IdentifierTypeEmail,
			Source:         "test_source",
			MatchType:      repository.MatchTypeUnmatched,
			MessageCount:   1,
		})
		require.NoError(t, err)
		defer func() { _ = repo.Delete(ctx, ident2.ID) }()

		// Bulk link
		confidence := 1.0
		err = repo.BulkLinkToContact(ctx, []uuid.UUID{ident1.ID, ident2.ID}, contact.ID, repository.MatchTypeManual, &confidence)
		require.NoError(t, err)

		// Verify both are linked
		identities, err := repo.ListForContact(ctx, contact.ID)
		require.NoError(t, err)
		assert.Len(t, identities, 2)
	})
}

// TestIdentityService_Integration tests identity matching with real database
func TestIdentityService_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}

	ctx := context.Background()

	migrationsPath := getMigrationsPath()
	if err := db.RunMigrations(databaseURL, migrationsPath); err != nil {
		t.Fatalf("Failed to run migrations: %v", err)
	}

	cfg := config.TestConfig()
	cfg.Database.URL = databaseURL

	database, err := db.NewDatabase(ctx, cfg.Database)
	if err != nil {
		t.Skipf("Could not connect to database: %v", err)
	}
	defer database.Close()

	identityRepo := repository.NewIdentityRepository(database.Queries)
	contactRepo := repository.NewContactRepository(database.Queries)
	methodRepo := repository.NewContactMethodRepository(database.Queries)
	identityService := service.NewIdentityService(identityRepo)

	t.Run("MatchOrCreate_DiscoveryMode_NoMatch", func(t *testing.T) {
		result, err := identityService.MatchOrCreate(ctx, service.MatchRequest{
			RawIdentifier: "UNKNOWN@Example.COM",
			Type:          identity.IdentifierTypeEmail,
			Source:        "test_discovery",
		})
		require.NoError(t, err)
		require.NotNil(t, result)

		// Should be unmatched
		assert.Nil(t, result.ContactID)
		assert.Equal(t, repository.MatchTypeUnmatched, result.MatchType)
		assert.False(t, result.Cached)

		// Verify normalized
		assert.Equal(t, "unknown@example.com", result.Identity.Identifier)

		// Clean up
		err = identityRepo.Delete(ctx, result.Identity.ID)
		require.NoError(t, err)
	})

	t.Run("MatchOrCreate_DiscoveryMode_ExactMatch", func(t *testing.T) {
		// Create a contact with email
		contact, err := contactRepo.CreateContact(ctx, repository.CreateContactRequest{
			FullName: "Discovery Match Test",
		})
		require.NoError(t, err)
		defer func() { _ = contactRepo.HardDeleteContact(ctx, contact.ID) }()

		// Add contact method
		_, err = methodRepo.CreateContactMethod(ctx, repository.CreateContactMethodRequest{
			ContactID: contact.ID,
			Type:      string(repository.ContactMethodEmailPersonal),
			Value:     "discovery.match@example.com",
			IsPrimary: true,
		})
		require.NoError(t, err)
		defer func() { _ = methodRepo.DeleteContactMethodsByContact(ctx, contact.ID) }()

		// Try to match via identity service
		result, err := identityService.MatchOrCreate(ctx, service.MatchRequest{
			RawIdentifier: "DISCOVERY.MATCH@EXAMPLE.COM",
			Type:          identity.IdentifierTypeEmail,
			Source:        "test_discovery",
		})
		require.NoError(t, err)
		require.NotNil(t, result)

		// Should match the contact
		assert.NotNil(t, result.ContactID)
		assert.Equal(t, contact.ID, *result.ContactID)
		assert.Equal(t, repository.MatchTypeExact, result.MatchType)
		assert.False(t, result.Cached)

		// Clean up
		err = identityRepo.Delete(ctx, result.Identity.ID)
		require.NoError(t, err)
	})

	t.Run("MatchOrCreate_DiscoveryMode_CachedMatch", func(t *testing.T) {
		// Create a contact
		contact, err := contactRepo.CreateContact(ctx, repository.CreateContactRequest{
			FullName: "Cache Test Contact",
		})
		require.NoError(t, err)
		defer func() { _ = contactRepo.HardDeleteContact(ctx, contact.ID) }()

		// Add contact method
		_, err = methodRepo.CreateContactMethod(ctx, repository.CreateContactMethodRequest{
			ContactID: contact.ID,
			Type:      string(repository.ContactMethodEmailPersonal),
			Value:     "cache.test@example.com",
			IsPrimary: true,
		})
		require.NoError(t, err)
		defer func() { _ = methodRepo.DeleteContactMethodsByContact(ctx, contact.ID) }()

		// First match - should search and cache
		result1, err := identityService.MatchOrCreate(ctx, service.MatchRequest{
			RawIdentifier: "cache.test@example.com",
			Type:          identity.IdentifierTypeEmail,
			Source:        "test_cache",
		})
		require.NoError(t, err)
		assert.False(t, result1.Cached)
		assert.Equal(t, contact.ID, *result1.ContactID)
		defer func() { _ = identityRepo.Delete(ctx, result1.Identity.ID) }()

		// Second match - should use cache
		result2, err := identityService.MatchOrCreate(ctx, service.MatchRequest{
			RawIdentifier: "cache.test@example.com",
			Type:          identity.IdentifierTypeEmail,
			Source:        "test_cache",
		})
		require.NoError(t, err)
		assert.True(t, result2.Cached)
		assert.Equal(t, contact.ID, *result2.ContactID)
	})

	t.Run("MatchOrCreate_ContactDrivenMode", func(t *testing.T) {
		// Create a contact
		contact, err := contactRepo.CreateContact(ctx, repository.CreateContactRequest{
			FullName: "Contact Driven Test",
		})
		require.NoError(t, err)
		defer func() { _ = contactRepo.HardDeleteContact(ctx, contact.ID) }()

		// Use contact-driven mode with KnownContactID
		result, err := identityService.MatchOrCreate(ctx, service.MatchRequest{
			RawIdentifier:  "CONTACT.DRIVEN@EXAMPLE.COM",
			Type:           identity.IdentifierTypeEmail,
			Source:         "gmail",
			KnownContactID: &contact.ID,
		})
		require.NoError(t, err)
		require.NotNil(t, result)

		// Should link directly without searching
		assert.NotNil(t, result.ContactID)
		assert.Equal(t, contact.ID, *result.ContactID)
		assert.Equal(t, repository.MatchTypeExact, result.MatchType)
		assert.False(t, result.Cached)

		// Verify normalized
		assert.Equal(t, "contact.driven@example.com", result.Identity.Identifier)

		// Clean up
		err = identityRepo.Delete(ctx, result.Identity.ID)
		require.NoError(t, err)
	})

	t.Run("MatchOrCreate_PhoneNormalization", func(t *testing.T) {
		// Create a contact with phone
		contact, err := contactRepo.CreateContact(ctx, repository.CreateContactRequest{
			FullName: "Phone Match Test",
		})
		require.NoError(t, err)
		defer func() { _ = contactRepo.HardDeleteContact(ctx, contact.ID) }()

		// Add contact method with normalized phone
		_, err = methodRepo.CreateContactMethod(ctx, repository.CreateContactMethodRequest{
			ContactID: contact.ID,
			Type:      "phone",
			Value:     "+15551234567",
			IsPrimary: true,
		})
		require.NoError(t, err)
		defer func() { _ = methodRepo.DeleteContactMethodsByContact(ctx, contact.ID) }()

		// Try to match with different phone formats
		testCases := []string{
			"(555) 123-4567",
			"+1 555 123 4567",
			"555.123.4567",
			"1-555-123-4567",
		}

		for _, phoneFormat := range testCases {
			result, err := identityService.MatchOrCreate(ctx, service.MatchRequest{
				RawIdentifier: phoneFormat,
				Type:          identity.IdentifierTypePhone,
				Source:        "test_phone_" + phoneFormat[:3],
			})
			require.NoError(t, err, "failed for format: %s", phoneFormat)

			// Should match despite different formats
			assert.NotNil(t, result.ContactID, "should match for format: %s", phoneFormat)
			assert.Equal(t, contact.ID, *result.ContactID, "should match correct contact for format: %s", phoneFormat)

			// Clean up
			_ = identityRepo.Delete(ctx, result.Identity.ID)
		}
	})

	t.Run("ManualLinkUnlink", func(t *testing.T) {
		// Create a contact
		contact, err := contactRepo.CreateContact(ctx, repository.CreateContactRequest{
			FullName: "Manual Link Test",
		})
		require.NoError(t, err)
		defer func() { _ = contactRepo.HardDeleteContact(ctx, contact.ID) }()

		// Create unmatched identity
		result, err := identityService.MatchOrCreate(ctx, service.MatchRequest{
			RawIdentifier: "manual.link@example.com",
			Type:          identity.IdentifierTypeEmail,
			Source:        "test_manual",
		})
		require.NoError(t, err)
		assert.Nil(t, result.ContactID)
		defer func() { _ = identityRepo.Delete(ctx, result.Identity.ID) }()

		// Manually link
		linked, err := identityService.LinkIdentity(ctx, result.Identity.ID, contact.ID)
		require.NoError(t, err)
		assert.NotNil(t, linked.ContactID)
		assert.Equal(t, contact.ID, *linked.ContactID)
		assert.Equal(t, repository.MatchTypeManual, linked.MatchType)

		// Unlink
		unlinked, err := identityService.UnlinkIdentity(ctx, result.Identity.ID)
		require.NoError(t, err)
		assert.Nil(t, unlinked.ContactID)
		assert.Equal(t, repository.MatchTypeUnmatched, unlinked.MatchType)
	})
}
