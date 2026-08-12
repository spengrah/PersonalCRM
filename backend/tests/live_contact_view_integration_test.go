//go:build integration_testdb

package tests

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/contacttask"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/testdb"

	migrate "github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// liveContactColumnOrder is db.Contact's field order (backend/internal/db/models.go),
// which live_contact's projection (migration 078) must match column-for-column
// and in order. Re-read the struct before trusting this list if a column has
// landed since.
var liveContactColumnOrder = []string{
	"id", "full_name", "location", "birthday", "how_met", "cadence",
	"last_contacted", "profile_photo", "deleted_at", "created_at", "updated_at",
	"contact_by", "last_interaction_at", "last_outreach_at", "last_response_at",
}

// TestLiveContactView_MigrationUpDown proves the migration round-trips: 078
// applies (the view exists and its row count already excludes a soft-deleted
// contact), the down migration drops the view, and the down migration leaves
// contact's own rows untouched.
func TestLiveContactView_MigrationUpDown(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}

	ctx := context.Background()
	cloneURL, drop := testdb.NewEphemeralClone(t)
	t.Cleanup(drop)

	cfg := config.TestConfig()
	cfg.Database.URL = cloneURL
	database, err := db.NewDatabase(ctx, cfg.Database)
	require.NoError(t, err)
	t.Cleanup(database.Close)

	m, err := migrate.New(fmt.Sprintf("file://%s", getMigrationsPath()), cloneURL)
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = m.Close() })

	contactRepo := repository.NewContactRepository(database.Queries)
	support := repository.NewSyntheticSupportRepository(database.Queries)

	live, err := contactRepo.CreateContact(ctx, repository.CreateContactRequest{FullName: "LiveContactView Migration Live"})
	require.NoError(t, err)
	deleted, err := contactRepo.CreateContact(ctx, repository.CreateContactRequest{FullName: "LiveContactView Migration Deleted"})
	require.NoError(t, err)
	require.NoError(t, contactRepo.SoftDeleteContact(ctx, deleted.ID))

	// The clone starts at the template's tip, which already includes 078: the
	// view exists as a plain view and its row count already excludes the
	// soft-deleted contact (this clone is a fresh database, so the two seeded
	// contacts are its only rows and the count is exact, not DB-wide).
	kindCount, err := database.Queries.TestCountPlainViews(ctx, "live_contact")
	require.NoError(t, err)
	require.Equal(t, int64(1), kindCount, "live_contact must exist as a plain view at the clone's tip")

	totalContacts, err := support.CountAllRows(ctx, "contact")
	require.NoError(t, err)
	liveContacts, err := support.CountAllRows(ctx, "live_contact")
	require.NoError(t, err)
	require.Equal(t, totalContacts-1, liveContacts, "live_contact must return only the non-deleted contact")

	liveFromDB, err := contactRepo.GetContact(ctx, live.ID)
	require.NoError(t, err)
	assert.Equal(t, "LiveContactView Migration Live", liveFromDB.FullName)

	// Roll 078 down. Steps(-1), not Migrate(<version>): the clone's tip is
	// wherever the migrations directory currently ends (078 here), so rolling
	// back exactly one migration from the tip is robust to gaps or later
	// migrations landing above it, without hardcoding the prior version number
	// (see migration076Env's whatsappFoundationsVersion for the
	// Migrate(<version>) form used where a FIXED intermediate position matters;
	// here only "one migration below the tip" matters).
	require.NoError(t, m.Steps(-1))

	kindCount, err = database.Queries.TestCountPlainViews(ctx, "live_contact")
	require.NoError(t, err)
	assert.Equal(t, int64(0), kindCount, "the down migration drops the view")

	afterDownContacts, err := support.CountAllRows(ctx, "contact")
	require.NoError(t, err)
	assert.Equal(t, totalContacts, afterDownContacts, "the down migration must not touch contact's own rows")

	// Re-apply: the view returns, restored to the same shape.
	require.NoError(t, m.Steps(1))

	kindCount, err = database.Queries.TestCountPlainViews(ctx, "live_contact")
	require.NoError(t, err)
	assert.Equal(t, int64(1), kindCount, "the up migration restores the view")

	cols, err := database.Queries.TestListViewColumns(ctx, "live_contact")
	require.NoError(t, err)
	assert.Len(t, cols, len(liveContactColumnOrder), "the restored view keeps its full column list")
}

// TestLiveContactView_Shape asserts live_contact's KIND (plain, not
// materialized) and its projected column list (names, ordinal order, and
// types) against db.Contact. An EXPLAIN-based plan-equivalence test is not
// buildable under the pinned sqlc version (EXPLAIN's runtime column shape is
// never inferred at generation time), so this catalog assertion guards the
// precondition plan-equivalence actually relies on instead: a plain,
// non-materialized view with an exact, ordered projection is substituted and
// flattened by the planner's subquery pull-up.
func TestLiveContactView_Shape(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	t.Parallel()
	ctx := context.Background()
	database, _ := newSharedTestDB(t, ctx)

	kindCount, err := database.Queries.TestCountPlainViews(ctx, "live_contact")
	require.NoError(t, err)
	require.Equal(t, int64(1), kindCount, "live_contact must exist as a plain (non-materialized) view")

	viewCols, err := database.Queries.TestListViewColumns(ctx, "live_contact")
	require.NoError(t, err)
	require.Len(t, viewCols, len(liveContactColumnOrder), "live_contact must project exactly db.Contact's column count")

	gotNames := make([]string, len(viewCols))
	for i, c := range viewCols {
		gotNames[i] = c.ColumnName
	}
	// Whole ordered slice in one comparison — a per-column membership check
	// would pass even if the order were wrong, and order is half the promise.
	assert.Equal(t, liveContactColumnOrder, gotNames,
		"live_contact's projection must match db.Contact field-for-field, in order")

	baseCols, err := database.Queries.TestListViewColumns(ctx, "contact")
	require.NoError(t, err)
	baseTypes := make(map[string]string, len(baseCols))
	baseNames := make([]string, len(baseCols))
	for i, c := range baseCols {
		baseTypes[c.ColumnName] = c.DataType
		baseNames[i] = c.ColumnName
	}
	for _, c := range viewCols {
		baseType, ok := baseTypes[c.ColumnName]
		require.Truef(t, ok, "view column %s must exist on contact", c.ColumnName)
		assert.Equalf(t, baseType, c.DataType, "view column %s must keep contact's type", c.ColumnName)
	}
	// The loop above only proves every VIEW column exists on contact — it
	// would pass even if contact carried a 16th column the view never picked
	// up (D6-2's drift risk). Compare the complete SETS instead: a column
	// added to contact without a matching migration to the view now fails
	// here, red-first, rather than silently disappearing from every
	// child-table read that joins through live_contact.
	expectedNames := append([]string(nil), liveContactColumnOrder...)
	sort.Strings(expectedNames)
	gotBaseNames := append([]string(nil), baseNames...)
	sort.Strings(gotBaseNames)
	assert.Equal(t, expectedNames, gotBaseNames,
		"contact's full column set must equal live_contact's projected set — no column may exist on one without the other")

	// The KIND and per-column checks above don't rule out a view shape that
	// still blocks the planner's subquery pull-up: a security_barrier view
	// projects the same columns and passes information_schema.views (still a
	// plain, non-materialized relation) while pull-up is disabled, and the
	// same is true of a SELECT DISTINCT/GROUP BY/LIMIT/HAVING/set-op
	// projecting the same 15 names. Assert BOTH catalog facts pull-up
	// actually depends on: no storage option is set (reloptions), and the
	// compiled definition is exactly the simple predicate this migration
	// wrote — not merely "some select over these columns".
	defAndOpts, err := database.Queries.TestGetViewDefAndOptions(ctx, "live_contact")
	require.NoError(t, err)
	assert.Empty(t, defAndOpts.Reloptions,
		"live_contact must carry no storage options (e.g. security_barrier) — any would block subquery pull-up")

	expectedViewDef := "SELECT " + strings.Join(liveContactColumnOrder, ", ") + " FROM contact WHERE (deleted_at IS NULL);"
	assert.Equal(t, expectedViewDef, normalizeSQL(defAndOpts.ViewDefinition),
		"live_contact's compiled definition must be exactly a simple SELECT of these columns from contact with the liveness predicate — a DISTINCT, GROUP BY, LIMIT, HAVING, or set-op would change this text and also block pull-up")
}

// normalizeSQL collapses all whitespace (including the embedded newlines and
// variable indentation PostgreSQL's pretty-printer puts into
// pg_get_viewdef's output) into single spaces, so the comparison depends on
// the SQL's content, not its formatting.
func normalizeSQL(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// liveContactFixture is one live and one soft-deleted contact, seeded through
// the ordinary repository — a handful of rows on the shared package DB, per
// the element's instruction not to reach for the synthetic harness here (it
// starts a live River client and writes outside the caller's control).
type liveContactFixture struct {
	live    *repository.Contact
	deleted *repository.Contact
}

func seedLiveContactFixture(t *testing.T, ctx context.Context, contactRepo *repository.ContactRepository, tag string) liveContactFixture {
	t.Helper()
	token := uuid.NewString()[:8]

	live, err := contactRepo.CreateContact(ctx, repository.CreateContactRequest{
		FullName: "LiveContactView " + tag + " Live " + token,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = contactRepo.HardDeleteContact(context.Background(), live.ID) })

	deleted, err := contactRepo.CreateContact(ctx, repository.CreateContactRequest{
		FullName: "LiveContactView " + tag + " Deleted " + token,
	})
	require.NoError(t, err)
	require.NoError(t, contactRepo.SoftDeleteContact(ctx, deleted.ID))
	t.Cleanup(func() { _ = contactRepo.HardDeleteContact(context.Background(), deleted.ID) })

	return liveContactFixture{live: live, deleted: deleted}
}

// TestLiveContactView_ExcludesSoftDeletedOwners is a regression pin, not a
// driver: today's hand-written predicates already produce these results. The
// actual falsification (removing WHERE deleted_at IS NULL from the view
// definition) is run separately per the element's mandatory falsification
// protocol, not by this test.
func TestLiveContactView_ExcludesSoftDeletedOwners(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	t.Parallel()
	ctx := context.Background()
	database, _ := newSharedTestDB(t, ctx)

	contactRepo := repository.NewContactRepository(database.Queries)
	methodRepo := repository.NewContactMethodRepository(database.Queries)
	commsRepo := repository.NewCommsMessageRepository(database.Queries)

	fx := seedLiveContactFixture(t, ctx, contactRepo, "excl")
	token := uuid.NewString()[:8]
	liveEmail := "live-contact-view-excl-live-" + token + "@example.com"
	deletedEmail := "live-contact-view-excl-deleted-" + token + "@example.com"

	_, err := methodRepo.CreateContactMethod(ctx, repository.CreateContactMethodRequest{
		ContactID: fx.live.ID, Type: string(repository.ContactMethodEmail), Value: liveEmail, IsPrimary: true,
	})
	require.NoError(t, err)
	_, err = methodRepo.CreateContactMethod(ctx, repository.CreateContactMethodRequest{
		ContactID: fx.deleted.ID, Type: string(repository.ContactMethodEmail), Value: deletedEmail, IsPrimary: true,
	})
	require.NoError(t, err)

	liveNorm := repository.NormalizeContactMethodValue(string(repository.ContactMethodEmail), liveEmail)
	deletedNorm := repository.NormalizeContactMethodValue(string(repository.ContactMethodEmail), deletedEmail)

	ids, err := methodRepo.ListCanonicalIdentifiersByType(ctx, []string{string(repository.ContactMethodEmail)})
	require.NoError(t, err)
	assert.Contains(t, ids, liveNorm, "ListCanonicalIdentifiersByType must include the live owner's method")
	assert.NotContains(t, ids, deletedNorm, "ListCanonicalIdentifiersByType must exclude the soft-deleted owner's method")

	emails, err := commsRepo.ListEmailIdentitiesForSync(ctx)
	require.NoError(t, err)
	byValue := make(map[string]uuid.UUID, len(emails))
	for _, e := range emails {
		byValue[e.ValueNormalized] = e.ContactID
	}
	gotLive, ok := byValue[liveNorm]
	assert.True(t, ok, "ListEmailIdentitiesForSync must include the live owner")
	if ok {
		assert.Equal(t, fx.live.ID, gotLive)
	}
	_, stillThere := byValue[deletedNorm]
	assert.False(t, stillThere, "ListEmailIdentitiesForSync must exclude the soft-deleted owner")
}

// TestLiveContactView_RewrittenReadsHonourLiveness covers the remaining six
// rewritten reads not already covered by TestLiveContactView_ExcludesSoftDeletedOwners
// (ListGChatIdentitiesForSync, ListCommsMessagesMissingParticipantNames,
// ListContactTagsWithLiveContact, FindMethodsByNormalizedValue,
// ListManagedContactTasks, SyntheticCountContactMethodsByValueNormalizedPrefix),
// one subtest per read, each excluding a soft-deleted owner's row and
// including a live one.
func TestLiveContactView_RewrittenReadsHonourLiveness(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	t.Parallel()
	ctx := context.Background()
	database, _ := newSharedTestDB(t, ctx)

	contactRepo := repository.NewContactRepository(database.Queries)
	methodRepo := repository.NewContactMethodRepository(database.Queries)
	commsRepo := repository.NewCommsMessageRepository(database.Queries)
	tagRepo := repository.NewTagRepository(database.Queries)
	taskRepo := repository.NewContactTaskRepository(database.Queries)
	identityRepo := repository.NewIdentityRepository(database.Queries)
	support := repository.NewSyntheticSupportRepository(database.Queries)

	t.Run("ListCanonicalIdentifiersByType", func(t *testing.T) {
		t.Parallel()
		fx := seedLiveContactFixture(t, ctx, contactRepo, "rr-canon")
		token := uuid.NewString()[:8]
		liveVal := "live-contact-view-rr-canon-live-" + token + "@example.com"
		deletedVal := "live-contact-view-rr-canon-deleted-" + token + "@example.com"
		_, err := methodRepo.CreateContactMethod(ctx, repository.CreateContactMethodRequest{
			ContactID: fx.live.ID, Type: string(repository.ContactMethodEmail), Value: liveVal, IsPrimary: true,
		})
		require.NoError(t, err)
		_, err = methodRepo.CreateContactMethod(ctx, repository.CreateContactMethodRequest{
			ContactID: fx.deleted.ID, Type: string(repository.ContactMethodEmail), Value: deletedVal, IsPrimary: true,
		})
		require.NoError(t, err)

		liveNorm := repository.NormalizeContactMethodValue(string(repository.ContactMethodEmail), liveVal)
		deletedNorm := repository.NormalizeContactMethodValue(string(repository.ContactMethodEmail), deletedVal)

		got, err := methodRepo.ListCanonicalIdentifiersByType(ctx, []string{string(repository.ContactMethodEmail)})
		require.NoError(t, err)
		assert.Contains(t, got, liveNorm, "ListCanonicalIdentifiersByType must include the live owner's method")
		assert.NotContains(t, got, deletedNorm, "ListCanonicalIdentifiersByType must exclude the soft-deleted owner's method")
	})

	t.Run("ListEmailIdentitiesForSync", func(t *testing.T) {
		t.Parallel()
		fx := seedLiveContactFixture(t, ctx, contactRepo, "rr-email")
		token := uuid.NewString()[:8]
		liveVal := "live-contact-view-rr-email-live-" + token + "@example.com"
		deletedVal := "live-contact-view-rr-email-deleted-" + token + "@example.com"
		_, err := methodRepo.CreateContactMethod(ctx, repository.CreateContactMethodRequest{
			ContactID: fx.live.ID, Type: string(repository.ContactMethodEmail), Value: liveVal, IsPrimary: true,
		})
		require.NoError(t, err)
		_, err = methodRepo.CreateContactMethod(ctx, repository.CreateContactMethodRequest{
			ContactID: fx.deleted.ID, Type: string(repository.ContactMethodEmail), Value: deletedVal, IsPrimary: true,
		})
		require.NoError(t, err)

		liveNorm := repository.NormalizeContactMethodValue(string(repository.ContactMethodEmail), liveVal)
		deletedNorm := repository.NormalizeContactMethodValue(string(repository.ContactMethodEmail), deletedVal)

		rows, err := commsRepo.ListEmailIdentitiesForSync(ctx)
		require.NoError(t, err)
		byValue := make(map[string]uuid.UUID, len(rows))
		for _, r := range rows {
			byValue[r.ValueNormalized] = r.ContactID
		}
		gotLive, ok := byValue[liveNorm]
		assert.True(t, ok, "ListEmailIdentitiesForSync must include the live owner")
		if ok {
			assert.Equal(t, fx.live.ID, gotLive)
		}
		_, stillThere := byValue[deletedNorm]
		assert.False(t, stillThere, "ListEmailIdentitiesForSync must exclude the soft-deleted owner")
	})

	t.Run("ListGChatIdentitiesForSync", func(t *testing.T) {
		t.Parallel()
		fx := seedLiveContactFixture(t, ctx, contactRepo, "rr-gchat")
		token := uuid.NewString()[:8]
		liveVal := "live-contact-view-rr-gchat-live-" + token + "@example.com"
		deletedVal := "live-contact-view-rr-gchat-deleted-" + token + "@example.com"
		_, err := methodRepo.CreateContactMethod(ctx, repository.CreateContactMethodRequest{
			ContactID: fx.live.ID, Type: string(repository.ContactMethodGChat), Value: liveVal, IsPrimary: true,
		})
		require.NoError(t, err)
		_, err = methodRepo.CreateContactMethod(ctx, repository.CreateContactMethodRequest{
			ContactID: fx.deleted.ID, Type: string(repository.ContactMethodGChat), Value: deletedVal, IsPrimary: true,
		})
		require.NoError(t, err)

		liveNorm := repository.NormalizeContactMethodValue(string(repository.ContactMethodGChat), liveVal)
		deletedNorm := repository.NormalizeContactMethodValue(string(repository.ContactMethodGChat), deletedVal)

		rows, err := commsRepo.ListGChatIdentitiesForSync(ctx)
		require.NoError(t, err)
		var foundLive, foundDeleted bool
		for _, r := range rows {
			if r.ValueNormalized == liveNorm {
				foundLive = true
				assert.Equal(t, fx.live.ID, r.ContactID)
			}
			if r.ValueNormalized == deletedNorm {
				foundDeleted = true
			}
		}
		assert.True(t, foundLive, "ListGChatIdentitiesForSync must include the live owner")
		assert.False(t, foundDeleted, "ListGChatIdentitiesForSync must exclude the soft-deleted owner")
	})

	t.Run("ListCommsMessagesMissingParticipantNames", func(t *testing.T) {
		t.Parallel()
		fx := seedLiveContactFixture(t, ctx, contactRepo, "rr-msg")
		token := uuid.NewString()[:8]
		since := accelerated.GetCurrentTime().Add(-time.Minute)

		liveMsg, err := commsRepo.UpsertMessage(ctx, repository.UpsertCommsMessageParams{
			Source: "email", ExternalID: "live-contact-view-rr-msg-live-" + token, Direction: "inbound",
			SentAt: accelerated.GetCurrentTime(), MatchedContactID: fx.live.ID, SourceMetadata: []byte(`{}`),
		})
		require.NoError(t, err)
		t.Cleanup(func() { _ = commsRepo.HardDeleteByContact(context.Background(), fx.live.ID) })

		deletedMsg, err := commsRepo.UpsertMessage(ctx, repository.UpsertCommsMessageParams{
			Source: "email", ExternalID: "live-contact-view-rr-msg-deleted-" + token, Direction: "inbound",
			SentAt: accelerated.GetCurrentTime(), MatchedContactID: fx.deleted.ID, SourceMetadata: []byte(`{}`),
		})
		require.NoError(t, err)
		t.Cleanup(func() { _ = commsRepo.HardDeleteByContact(context.Background(), fx.deleted.ID) })

		rows, err := commsRepo.ListMissingParticipantNames(ctx, since, uuid.Nil, 10000)
		require.NoError(t, err)
		var foundLive, foundDeleted bool
		for _, r := range rows {
			if r.ID == liveMsg.ID {
				foundLive = true
			}
			if r.ID == deletedMsg.ID {
				foundDeleted = true
			}
		}
		assert.True(t, foundLive, "ListCommsMessagesMissingParticipantNames must include the live owner's message")
		assert.False(t, foundDeleted, "ListCommsMessagesMissingParticipantNames must exclude the soft-deleted owner's message")
	})

	t.Run("ListContactTagsWithLiveContact", func(t *testing.T) {
		t.Parallel()
		fx := seedLiveContactFixture(t, ctx, contactRepo, "rr-tag")
		token := uuid.NewString()[:8]
		liveTagID, err := support.InsertTagForMigration(ctx, "live-contact-view-rr-tag-live-"+token, nil)
		require.NoError(t, err)
		deletedTagID, err := support.InsertTagForMigration(ctx, "live-contact-view-rr-tag-deleted-"+token, nil)
		require.NoError(t, err)
		now := accelerated.GetCurrentTime()
		require.NoError(t, support.InsertContactTagAtTime(ctx, fx.live.ID, liveTagID, now))
		require.NoError(t, support.InsertContactTagAtTime(ctx, fx.deleted.ID, deletedTagID, now))

		links, err := tagRepo.ListContactTagsWithLiveContact(ctx)
		require.NoError(t, err)
		var foundLive, foundDeleted bool
		for _, l := range links {
			if l.ContactID == fx.live.ID && l.TagID == liveTagID {
				foundLive = true
			}
			if l.ContactID == fx.deleted.ID && l.TagID == deletedTagID {
				foundDeleted = true
			}
		}
		assert.True(t, foundLive, "ListContactTagsWithLiveContact must include the live owner's tag link")
		assert.False(t, foundDeleted, "ListContactTagsWithLiveContact must exclude the soft-deleted owner's tag link")
	})

	t.Run("FindMethodsByNormalizedValue", func(t *testing.T) {
		t.Parallel()
		fx := seedLiveContactFixture(t, ctx, contactRepo, "rr-find")
		token := uuid.NewString()[:8]
		val := "live-contact-view-rr-find-shared-" + token + "@example.com"
		_, err := methodRepo.CreateContactMethod(ctx, repository.CreateContactMethodRequest{
			ContactID: fx.live.ID, Type: string(repository.ContactMethodEmail), Value: val, IsPrimary: true,
		})
		require.NoError(t, err)
		_, err = methodRepo.CreateContactMethod(ctx, repository.CreateContactMethodRequest{
			ContactID: fx.deleted.ID, Type: string(repository.ContactMethodEmail), Value: val, IsPrimary: true,
		})
		require.NoError(t, err)

		norm := repository.NormalizeContactMethodValue(string(repository.ContactMethodEmail), val)
		matches, err := identityRepo.FindContactMethodsByValue(ctx, []string{string(repository.ContactMethodEmail)}, norm)
		require.NoError(t, err)
		var foundLive, foundDeleted bool
		for _, m := range matches {
			if m.ContactID == fx.live.ID {
				foundLive = true
				assert.Equal(t, fx.live.FullName, m.ContactName)
			}
			if m.ContactID == fx.deleted.ID {
				foundDeleted = true
			}
		}
		assert.True(t, foundLive, "FindMethodsByNormalizedValue must include the live owner")
		assert.False(t, foundDeleted, "FindMethodsByNormalizedValue must exclude the soft-deleted owner")
	})

	t.Run("ListManagedContactTasks", func(t *testing.T) {
		t.Parallel()
		fx := seedLiveContactFixture(t, ctx, contactRepo, "rr-task")
		token := uuid.NewString()[:8]
		provider := "live-contact-view-rr-task-" + token
		liveTask, err := taskRepo.CreateContactTask(ctx, repository.CreateContactTaskRequest{
			ContactID: fx.live.ID, Provider: provider, Kind: contacttask.KindReachOut,
			Lifecycle: contacttask.LifecycleCadenceDue, ExternalTaskID: "live-contact-view-rr-task-live-" + token,
			State: string(repository.ContactTaskStateManaged),
		})
		require.NoError(t, err)
		deletedTask, err := taskRepo.CreateContactTask(ctx, repository.CreateContactTaskRequest{
			ContactID: fx.deleted.ID, Provider: provider, Kind: contacttask.KindReachOut,
			Lifecycle: contacttask.LifecycleCadenceDue, ExternalTaskID: "live-contact-view-rr-task-deleted-" + token,
			State: string(repository.ContactTaskStateManaged),
		})
		require.NoError(t, err)

		tasks, err := taskRepo.ListManagedContactTasks(ctx, provider)
		require.NoError(t, err)
		var foundLive, foundDeleted bool
		for _, tk := range tasks {
			if tk.ID == liveTask.ID {
				foundLive = true
				assert.Equal(t, fx.live.FullName, tk.FullName)
			}
			if tk.ID == deletedTask.ID {
				foundDeleted = true
			}
		}
		assert.True(t, foundLive, "ListManagedContactTasks must include the live owner's task")
		assert.False(t, foundDeleted, "ListManagedContactTasks must exclude the soft-deleted owner's task")
	})

	t.Run("SyntheticCountContactMethodsByValueNormalizedPrefix", func(t *testing.T) {
		t.Parallel()
		fx := seedLiveContactFixture(t, ctx, contactRepo, "rr-phone")
		// A purely NUMERIC token: the normalization trigger strips non-digit
		// characters, so a hex (uuid) token would silently change the digit
		// sequence and break the LIKE-prefix match this subtest depends on.
		token := strconv.Itoa(100000 + rand.Intn(899999))
		prefix := "+1555" + token
		liveVal := prefix + "0001"
		deletedVal := prefix + "0002"
		_, err := methodRepo.CreateContactMethod(ctx, repository.CreateContactMethodRequest{
			ContactID: fx.live.ID, Type: string(repository.ContactMethodPhone), Value: liveVal, IsPrimary: true,
		})
		require.NoError(t, err)
		_, err = methodRepo.CreateContactMethod(ctx, repository.CreateContactMethodRequest{
			ContactID: fx.deleted.ID, Type: string(repository.ContactMethodPhone), Value: deletedVal, IsPrimary: true,
		})
		require.NoError(t, err)

		count, err := support.CountContactMethodsByValueNormalizedPrefix(ctx, prefix)
		require.NoError(t, err)
		assert.Equal(t, int64(1), count, "only the live owner's phone shares the prefix")
	})
}

// TestLiveContactView_UpdateGuardsHonourLiveness proves the two EXISTS-shaped
// UPDATE guards — UpdateContactMethodValue and SetContactMethodPrimary — each
// succeed for a live owner and no-op for a soft-deleted one. Neither guard had
// integration coverage of the blocked branch previously (verified by grep), so
// this is a genuine red-first for that branch, not just a regression pin.
func TestLiveContactView_UpdateGuardsHonourLiveness(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	t.Parallel()
	ctx := context.Background()
	database, _ := newSharedTestDB(t, ctx)

	contactRepo := repository.NewContactRepository(database.Queries)
	methodRepo := repository.NewContactMethodRepository(database.Queries)

	t.Run("UpdateContactMethodValue", func(t *testing.T) {
		t.Parallel()
		fx := seedLiveContactFixture(t, ctx, contactRepo, "guard-update")
		token := uuid.NewString()[:8]
		liveMethod, err := methodRepo.CreateContactMethod(ctx, repository.CreateContactMethodRequest{
			ContactID: fx.live.ID, Type: string(repository.ContactMethodEmail),
			Value: "live-contact-view-guard-live-old-" + token + "@example.com", IsPrimary: true,
		})
		require.NoError(t, err)
		deletedMethod, err := methodRepo.CreateContactMethod(ctx, repository.CreateContactMethodRequest{
			ContactID: fx.deleted.ID, Type: string(repository.ContactMethodEmail),
			Value: "live-contact-view-guard-deleted-old-" + token + "@example.com", IsPrimary: true,
		})
		require.NoError(t, err)

		newLiveVal := "live-contact-view-guard-live-new-" + token + "@example.com"
		require.NoError(t, methodRepo.UpdateContactMethod(ctx, liveMethod.ID,
			repository.UpdateContactMethodRequest{Type: string(repository.ContactMethodEmail), Value: newLiveVal}))
		liveMethods, err := methodRepo.ListContactMethodsByContact(ctx, fx.live.ID)
		require.NoError(t, err)
		require.Len(t, liveMethods, 1)
		assert.Equal(t, newLiveVal, liveMethods[0].Value, "the guard must let a live owner's method update through")

		// UpdateContactMethodValue is a :one query (RETURNING *), so the guard's
		// blocked branch surfaces as pgx.ErrNoRows on the zero-row UPDATE, not a
		// silent success — unlike SetContactMethodPrimary below, which is :exec.
		newDeletedVal := "live-contact-view-guard-deleted-new-" + token + "@example.com"
		err = methodRepo.UpdateContactMethod(ctx, deletedMethod.ID,
			repository.UpdateContactMethodRequest{Type: string(repository.ContactMethodEmail), Value: newDeletedVal})
		assert.ErrorIs(t, err, pgx.ErrNoRows, "the guard must reject a soft-deleted owner's method as not found")
		deletedMethods, err := methodRepo.ListContactMethodsByContact(ctx, fx.deleted.ID)
		require.NoError(t, err)
		require.Len(t, deletedMethods, 1)
		assert.NotEqual(t, newDeletedVal, deletedMethods[0].Value, "the guard must leave the soft-deleted owner's value unchanged")
	})

	t.Run("SetContactMethodPrimary", func(t *testing.T) {
		t.Parallel()
		fx := seedLiveContactFixture(t, ctx, contactRepo, "guard-primary")
		token := uuid.NewString()[:8]
		liveMethod, err := methodRepo.CreateContactMethod(ctx, repository.CreateContactMethodRequest{
			ContactID: fx.live.ID, Type: string(repository.ContactMethodEmail),
			Value: "live-contact-view-guard-primary-live-" + token + "@example.com", IsPrimary: false,
		})
		require.NoError(t, err)
		deletedMethod, err := methodRepo.CreateContactMethod(ctx, repository.CreateContactMethodRequest{
			ContactID: fx.deleted.ID, Type: string(repository.ContactMethodEmail),
			Value: "live-contact-view-guard-primary-deleted-" + token + "@example.com", IsPrimary: false,
		})
		require.NoError(t, err)

		require.NoError(t, methodRepo.SetPrimary(ctx, liveMethod.ID, true))
		liveMethods, err := methodRepo.ListContactMethodsByContact(ctx, fx.live.ID)
		require.NoError(t, err)
		require.Len(t, liveMethods, 1)
		assert.True(t, liveMethods[0].IsPrimary, "the guard must let a live owner's primary flag flip")

		require.NoError(t, methodRepo.SetPrimary(ctx, deletedMethod.ID, true))
		deletedMethods, err := methodRepo.ListContactMethodsByContact(ctx, fx.deleted.ID)
		require.NoError(t, err)
		require.Len(t, deletedMethods, 1)
		assert.False(t, deletedMethods[0].IsPrimary, "the guard must no-op for a soft-deleted owner's primary flag")
	})
}
