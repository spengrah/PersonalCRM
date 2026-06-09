package tests

import (
	"context"
	"fmt"
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

	// Migrations are applied once by TestMain.
	cfg := config.TestConfig()
	cfg.Database.URL = databaseURL

	database, err := db.NewDatabase(ctx, cfg.Database)
	if err != nil {
		t.Skipf("Could not connect to database: %v", err)
	}
	defer database.Close()

	repo := repository.NewIdentityRepository(database.Queries)

	// Per-test-run unique suffix so the (identifier, type, source)-keyed
	// accumulating upserts don't collide with a parallel copy and corrupt the
	// exact message_count / ordering assertions below.
	ns := uuid.NewString()[:8]
	src := "test_source_" + ns

	t.Run("UpsertAndGetIdentity", func(t *testing.T) {
		identifier := "test.user." + ns + "@example.com"
		rawIdentifier := "TEST.USER." + ns + "@EXAMPLE.COM"
		displayName := "Test User"

		// Create an identity
		ident, err := repo.Upsert(ctx, repository.UpsertIdentityRequest{
			Identifier:     identifier,
			IdentifierType: identity.IdentifierTypeEmail,
			RawIdentifier:  &rawIdentifier,
			Source:         src,
			MatchType:      repository.MatchTypeUnmatched,
			DisplayName:    &displayName,
			MessageCount:   1,
		})
		require.NoError(t, err)
		require.NotNil(t, ident)

		// Verify fields
		assert.Equal(t, identifier, ident.Identifier)
		assert.Equal(t, identity.IdentifierTypeEmail, ident.IdentifierType)
		assert.Equal(t, src, ident.Source)
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
		found, err = repo.GetByIdentifier(ctx, identity.IdentifierTypeEmail, identifier, src)
		require.NoError(t, err)
		assert.Equal(t, ident.ID, found.ID)

		// Clean up
		err = repo.Delete(ctx, ident.ID)
		require.NoError(t, err)
	})

	t.Run("UpsertIncrementsMessageCount", func(t *testing.T) {
		identifier := "increment.test." + ns + "@example.com"
		// Create identity
		ident, err := repo.Upsert(ctx, repository.UpsertIdentityRequest{
			Identifier:     identifier,
			IdentifierType: identity.IdentifierTypeEmail,
			Source:         src,
			MatchType:      repository.MatchTypeUnmatched,
			MessageCount:   1,
		})
		require.NoError(t, err)
		assert.Equal(t, int32(1), ident.MessageCount)

		// Upsert again - should increment
		ident, err = repo.Upsert(ctx, repository.UpsertIdentityRequest{
			Identifier:     identifier,
			IdentifierType: identity.IdentifierTypeEmail,
			Source:         src,
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
			Identifier:     "link.test." + ns + "@example.com",
			IdentifierType: identity.IdentifierTypeEmail,
			Source:         src,
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
			Identifier:     "unmatched1." + ns + "@example.com",
			IdentifierType: identity.IdentifierTypeEmail,
			Source:         src,
			MatchType:      repository.MatchTypeUnmatched,
			MessageCount:   10,
		})
		require.NoError(t, err)
		defer func() { _ = repo.Delete(ctx, ident1.ID) }()

		ident2, err := repo.Upsert(ctx, repository.UpsertIdentityRequest{
			Identifier:     "unmatched2." + ns + "@example.com",
			IdentifierType: identity.IdentifierTypeEmail,
			Source:         src,
			MatchType:      repository.MatchTypeUnmatched,
			MessageCount:   5,
		})
		require.NoError(t, err)
		defer func() { _ = repo.Delete(ctx, ident2.ID) }()

		// List unmatched
		unmatched, err := repo.ListUnmatched(ctx, 100, 0)
		require.NoError(t, err)

		// Should contain our test identities, sorted by message_count desc.
		// The relative ordering (idx1 < idx2) holds even with other parallel
		// tests' rows interspersed, since ident1 (10 msgs) outranks ident2 (5).
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

		// Create identities linked to contact. The email is namespaced and the
		// phone's source is namespaced so the (identifier, type, source) upsert
		// keys are unique to this test run.
		ident1, err := repo.Upsert(ctx, repository.UpsertIdentityRequest{
			Identifier:     "contact.ident1." + ns + "@example.com",
			IdentifierType: identity.IdentifierTypeEmail,
			Source:         "gmail_" + ns,
			ContactID:      &contact.ID,
			MatchType:      repository.MatchTypeExact,
			MessageCount:   1,
		})
		require.NoError(t, err)
		defer func() { _ = repo.Delete(ctx, ident1.ID) }()

		ident2, err := repo.Upsert(ctx, repository.UpsertIdentityRequest{
			Identifier:     "+15551234567",
			IdentifierType: identity.IdentifierTypePhone,
			Source:         "imessage_" + ns,
			ContactID:      &contact.ID,
			MatchType:      repository.MatchTypeExact,
			MessageCount:   1,
		})
		require.NoError(t, err)
		defer func() { _ = repo.Delete(ctx, ident2.ID) }()

		// List for contact (scoped to this run's unique contact)
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
			Identifier:     "bulk1." + ns + "@example.com",
			IdentifierType: identity.IdentifierTypeEmail,
			Source:         src,
			MatchType:      repository.MatchTypeUnmatched,
			MessageCount:   1,
		})
		require.NoError(t, err)
		defer func() { _ = repo.Delete(ctx, ident1.ID) }()

		ident2, err := repo.Upsert(ctx, repository.UpsertIdentityRequest{
			Identifier:     "bulk2." + ns + "@example.com",
			IdentifierType: identity.IdentifierTypeEmail,
			Source:         src,
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

	// Migrations are applied once by TestMain.
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

	// Per-test-run unique suffix so the matched emails/phones and the discovery/
	// cache sources don't collide with a parallel copy (the cached-match path is
	// order/state-sensitive).
	ns := uuid.NewString()[:8]

	t.Run("MatchOrCreate_DiscoveryMode_NoMatch", func(t *testing.T) {
		result, err := identityService.MatchOrCreate(ctx, service.MatchRequest{
			RawIdentifier: "UNKNOWN." + ns + "@Example.COM",
			Type:          identity.IdentifierTypeEmail,
			Source:        "test_discovery_" + ns,
		})
		require.NoError(t, err)
		require.NotNil(t, result)

		// Should be unmatched
		assert.Nil(t, result.ContactID)
		assert.Equal(t, repository.MatchTypeUnmatched, result.MatchType)
		assert.False(t, result.Cached)

		// Verify normalized
		assert.Equal(t, "unknown."+ns+"@example.com", result.Identity.Identifier)

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
			Type:      string(repository.ContactMethodEmail),
			Value:     "discovery.match." + ns + "@example.com",
			IsPrimary: true,
		})
		require.NoError(t, err)
		defer func() { _ = methodRepo.DeleteContactMethodsByContact(ctx, contact.ID) }()

		// Try to match via identity service
		result, err := identityService.MatchOrCreate(ctx, service.MatchRequest{
			RawIdentifier: "DISCOVERY.MATCH." + ns + "@EXAMPLE.COM",
			Type:          identity.IdentifierTypeEmail,
			Source:        "test_discovery_" + ns,
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
			Type:      string(repository.ContactMethodEmail),
			Value:     "cache.test." + ns + "@example.com",
			IsPrimary: true,
		})
		require.NoError(t, err)
		defer func() { _ = methodRepo.DeleteContactMethodsByContact(ctx, contact.ID) }()

		// First match - should search and cache
		result1, err := identityService.MatchOrCreate(ctx, service.MatchRequest{
			RawIdentifier: "cache.test." + ns + "@example.com",
			Type:          identity.IdentifierTypeEmail,
			Source:        "test_cache_" + ns,
		})
		require.NoError(t, err)
		assert.False(t, result1.Cached)
		assert.Equal(t, contact.ID, *result1.ContactID)
		defer func() { _ = identityRepo.Delete(ctx, result1.Identity.ID) }()

		// Second match - should use cache
		result2, err := identityService.MatchOrCreate(ctx, service.MatchRequest{
			RawIdentifier: "cache.test." + ns + "@example.com",
			Type:          identity.IdentifierTypeEmail,
			Source:        "test_cache_" + ns,
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
			RawIdentifier:  "CONTACT.DRIVEN." + ns + "@EXAMPLE.COM",
			Type:           identity.IdentifierTypeEmail,
			Source:         "gmail_" + ns,
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
		assert.Equal(t, "contact.driven."+ns+"@example.com", result.Identity.Identifier)

		// Clean up
		err = identityRepo.Delete(ctx, result.Identity.ID)
		require.NoError(t, err)
	})

	t.Run("MatchOrCreate_PhoneNormalization", func(t *testing.T) {
		// Per-test-unique 10-digit phone so a parallel copy's contact (with the
		// same normalized phone) can't be matched by mistake. Derive a 7-digit
		// subscriber+exchange from the namespace hash; the 4 format variants all
		// normalize to the same +1<area><number>.
		phoneBase, _ := uniqueTestIDs(t, ns) // >= 9_000_000_000
		sub := phoneBase % 10_000_000        // 7 digits: NXX-XXXX
		area := 200 + int(phoneBase%700)     // valid-ish NANP area code [200, 899]
		exch := sub / 10_000                 // 3 digits
		line := sub % 10_000                 // 4 digits

		// Add contact method with the canonical normalized phone.
		canonical := fmt.Sprintf("+1%03d%03d%04d", area, exch, line)
		contact, err := contactRepo.CreateContact(ctx, repository.CreateContactRequest{
			FullName: "Phone Match Test " + ns,
		})
		require.NoError(t, err)
		defer func() { _ = contactRepo.HardDeleteContact(ctx, contact.ID) }()

		_, err = methodRepo.CreateContactMethod(ctx, repository.CreateContactMethodRequest{
			ContactID: contact.ID,
			Type:      "phone",
			Value:     canonical,
			IsPrimary: true,
		})
		require.NoError(t, err)
		defer func() { _ = methodRepo.DeleteContactMethodsByContact(ctx, contact.ID) }()

		// Try to match with different phone formats — all equivalent to canonical.
		testCases := []string{
			fmt.Sprintf("(%03d) %03d-%04d", area, exch, line),
			fmt.Sprintf("+1 %03d %03d %04d", area, exch, line),
			fmt.Sprintf("%03d.%03d.%04d", area, exch, line),
			fmt.Sprintf("1-%03d-%03d-%04d", area, exch, line),
		}

		for i, phoneFormat := range testCases {
			result, err := identityService.MatchOrCreate(ctx, service.MatchRequest{
				RawIdentifier: phoneFormat,
				Type:          identity.IdentifierTypePhone,
				Source:        fmt.Sprintf("test_phone_%s_%d", ns, i),
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
			RawIdentifier: "manual.link." + ns + "@example.com",
			Type:          identity.IdentifierTypeEmail,
			Source:        "test_manual_" + ns,
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

// TestIdentityService_NormalizationPolicy_Integration exercises the
// MatchOrCreateTx policy parameter against a real database. Each
// sub-test opens its own tx and rolls it back so the valid-input writes
// never persist into the shared personal_crm_test DB.
func TestIdentityService_NormalizationPolicy_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}

	ctx := context.Background()

	// Migrations are applied once by TestMain.
	cfg := config.TestConfig()
	cfg.Database.URL = databaseURL

	database, err := db.NewDatabase(ctx, cfg.Database)
	if err != nil {
		t.Skipf("Could not connect to database: %v", err)
	}
	defer database.Close()

	identityRepo := repository.NewIdentityRepository(database.Queries)
	identityService := service.NewIdentityService(identityRepo)

	t.Run("FailEmpty_EmptyAfterNormalization_ReturnsError", func(t *testing.T) {
		tx, err := database.Pool.Begin(ctx)
		require.NoError(t, err)
		defer func() { _ = tx.Rollback(ctx) }()

		// "+" normalizes to "" for phone.
		result, err := identityService.MatchOrCreateTx(ctx, tx, service.MatchRequest{
			RawIdentifier: "+",
			Type:          identity.IdentifierTypePhone,
			Source:        "test_policy_fail_empty",
		}, service.NormalizationFailEmpty)
		require.Error(t, err)
		require.Contains(t, err.Error(), "empty identifier after normalization")
		require.Nil(t, result)
	})

	t.Run("SkipEmpty_EmptyAfterNormalization_ReturnsNilNil", func(t *testing.T) {
		tx, err := database.Pool.Begin(ctx)
		require.NoError(t, err)
		defer func() { _ = tx.Rollback(ctx) }()

		// Same junk input, SkipEmpty policy: the contract is (nil, nil).
		// The end-to-end "junk leaves no identity row" guarantee is
		// covered by the committed HTTP-stack tests under tests/api/.
		result, err := identityService.MatchOrCreateTx(ctx, tx, service.MatchRequest{
			RawIdentifier: "+",
			Type:          identity.IdentifierTypePhone,
			Source:        "test_policy_skip_empty",
		}, service.NormalizationSkipEmpty)
		require.NoError(t, err)
		require.Nil(t, result)
	})

	t.Run("SkipEmpty_ValidIdentifier_BehavesNormally", func(t *testing.T) {
		tx, err := database.Pool.Begin(ctx)
		require.NoError(t, err)
		defer func() { _ = tx.Rollback(ctx) }()

		// Randomized source + identifier so the in-tx read-back cannot
		// match a stale row left by a prior run in the shared test DB.
		suffix := uuid.NewString()
		rawEmail := "skip-valid-" + suffix + "@example.com"
		source := "test_policy_skip_valid_" + suffix

		result, err := identityService.MatchOrCreateTx(ctx, tx, service.MatchRequest{
			RawIdentifier: rawEmail,
			Type:          identity.IdentifierTypeEmail,
			Source:        source,
		}, service.NormalizationSkipEmpty)
		require.NoError(t, err)
		require.NotNil(t, result, "valid input under SkipEmpty must not no-op")
		require.NotNil(t, result.Identity, "valid input must produce an identity row")

		// Confirm the row exists in-tx before rollback.
		normalized := identity.Normalize(rawEmail, identity.IdentifierTypeEmail)
		readBack, err := identityRepo.GetByIdentifierTx(ctx, tx, identity.IdentifierTypeEmail, normalized, source)
		require.NoError(t, err)
		require.NotNil(t, readBack)
		require.Equal(t, normalized, readBack.Identifier)
	})

	t.Run("FailEmpty_ValidIdentifier_BehavesNormally", func(t *testing.T) {
		tx, err := database.Pool.Begin(ctx)
		require.NoError(t, err)
		defer func() { _ = tx.Rollback(ctx) }()

		suffix := uuid.NewString()
		rawEmail := "fail-valid-" + suffix + "@example.com"
		source := "test_policy_fail_valid_" + suffix

		result, err := identityService.MatchOrCreateTx(ctx, tx, service.MatchRequest{
			RawIdentifier: rawEmail,
			Type:          identity.IdentifierTypeEmail,
			Source:        source,
		}, service.NormalizationFailEmpty)
		require.NoError(t, err)
		require.NotNil(t, result, "valid input under FailEmpty preserves old behavior")
		require.NotNil(t, result.Identity, "valid input must produce an identity row")

		normalized := identity.Normalize(rawEmail, identity.IdentifierTypeEmail)
		readBack, err := identityRepo.GetByIdentifierTx(ctx, tx, identity.IdentifierTypeEmail, normalized, source)
		require.NoError(t, err)
		require.NotNil(t, readBack)
		require.Equal(t, normalized, readBack.Identifier)
	})
}
