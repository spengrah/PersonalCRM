package tests

import (
	"context"
	"fmt"
	"os"
	"sort"
	"testing"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// jsonbGINEnv bundles the dependencies a JSONB GIN index integration test
// needs. The fixture repository wraps test-only sqlc bindings that allow
// inserting raw JSONB shapes (NULL, scalar, missing keys) that the typed
// production paths cannot produce.
type jsonbGINEnv struct {
	ctx          context.Context
	database     *db.Database
	externalRepo *repository.ExternalContactRepository
	calendarRepo *repository.CalendarEventRepository
	fixtureRepo  *repository.TestJSONBFixturesRepository
	suffix       string
	sourceIDPfx  string
	gcalIDPfx    string
}

func setupJSONBGINEnv(t *testing.T) *jsonbGINEnv {
	t.Helper()
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set")
	}

	cfg := config.TestConfig()
	cfg.Database.URL = databaseURL

	ctx := context.Background()
	database, err := db.NewDatabase(ctx, cfg.Database)
	require.NoError(t, err)
	t.Cleanup(func() { database.Close() })

	suffix := uuid.NewString()
	sourceIDPfx := "jsonb_gin_test_" + suffix + "_"
	gcalIDPfx := "jsonb_gin_test_" + suffix + "_"

	env := &jsonbGINEnv{
		ctx:          ctx,
		database:     database,
		externalRepo: repository.NewExternalContactRepository(database.Queries),
		calendarRepo: repository.NewCalendarEventRepository(database.Queries),
		fixtureRepo:  repository.NewTestJSONBFixturesRepository(database.Queries),
		suffix:       suffix,
		sourceIDPfx:  sourceIDPfx,
		gcalIDPfx:    gcalIDPfx,
	}

	t.Cleanup(func() {
		// Best-effort cleanup. Ignore errors so a failing test still cleans
		// up the rest. Each fixture row has the per-test suffix in its
		// identifying column so this never touches another test's data.
		_ = env.fixtureRepo.DeleteExternalContactsBySourceIDPrefix(context.Background(), sourceIDPfx)
		_ = env.fixtureRepo.DeleteCalendarEventsByGcalEventIDPrefix(context.Background(), gcalIDPfx)
	})

	return env
}

// sourceIDFor returns a per-test-tagged source_id used as the fixture row's
// identifying column. The cleanup hook deletes by prefix.
func (e *jsonbGINEnv) sourceIDFor(label string) string {
	return e.sourceIDPfx + label
}

// gcalIDFor returns a per-test-tagged gcal_event_id used as the fixture
// row's identifying column.
func (e *jsonbGINEnv) gcalIDFor(label string) string {
	return e.gcalIDPfx + label
}

// TestFindExternalContactsByNormalizedEmail_Behavior covers cases 1–10 from
// the plan: behavioral correctness of the GIN-backed query against
// every JSONB shape that exists in prod plus the defensive shapes that
// don't (NULL JSONB column, non-array JSONB).
func TestFindExternalContactsByNormalizedEmail_Behavior(t *testing.T) {
	env := setupJSONBGINEnv(t)

	t.Run("Case01_MixedCaseStored_LowercaseQuery_Matches", func(t *testing.T) {
		stored := []byte(fmt.Sprintf(`[{"value":"User_%s@Domain.invalid"}]`, env.suffix))
		_, err := env.fixtureRepo.InsertExternalContactRawEmails(env.ctx, "google", env.sourceIDFor("case01"), "Case 01", stored)
		require.NoError(t, err)

		// Query uses the lowercase form of the stored value so it matches the
		// stored mixed-case email after both sides are LOWER()ed.
		queryEmail := fmt.Sprintf("user_%s@domain.invalid", env.suffix)
		results, err := env.externalRepo.FindByNormalizedEmail(env.ctx, queryEmail)
		require.NoError(t, err)
		// Filter to fixtures owned by this test (DB is shared across runs).
		matches := filterExternalBySourceIDPrefix(results, env.sourceIDPfx)
		assert.Len(t, matches, 1, "expected exactly 1 match for case01")
	})

	t.Run("Case02_LowercaseStored_MixedCaseQuery_Matches", func(t *testing.T) {
		stored := []byte(fmt.Sprintf(`[{"value":"foo_%s@bar.invalid"}]`, env.suffix))
		_, err := env.fixtureRepo.InsertExternalContactRawEmails(env.ctx, "google", env.sourceIDFor("case02"), "Case 02", stored)
		require.NoError(t, err)

		queryEmail := fmt.Sprintf("Foo_%s@Bar.invalid", env.suffix)
		results, err := env.externalRepo.FindByNormalizedEmail(env.ctx, queryEmail)
		require.NoError(t, err)
		matches := filterExternalBySourceIDPrefix(results, env.sourceIDPfx)
		assert.Len(t, matches, 1, "expected exactly 1 match for case02")
	})

	t.Run("Case03_MultipleEntries_OneMatches", func(t *testing.T) {
		stored := []byte(fmt.Sprintf(`[{"value":"a_%s@x.invalid"},{"value":"b_%s@y.invalid"}]`, env.suffix, env.suffix))
		_, err := env.fixtureRepo.InsertExternalContactRawEmails(env.ctx, "google", env.sourceIDFor("case03"), "Case 03", stored)
		require.NoError(t, err)

		queryEmail := fmt.Sprintf("b_%s@y.invalid", env.suffix)
		results, err := env.externalRepo.FindByNormalizedEmail(env.ctx, queryEmail)
		require.NoError(t, err)
		matches := filterExternalBySourceIDPrefix(results, env.sourceIDPfx)
		assert.Len(t, matches, 1, "expected exactly 1 match for case03")
	})

	t.Run("Case04_EmptyArray_NoMatch", func(t *testing.T) {
		_, err := env.fixtureRepo.InsertExternalContactRawEmails(env.ctx, "google", env.sourceIDFor("case04"), "Case 04", []byte(`[]`))
		require.NoError(t, err)

		queryEmail := fmt.Sprintf("nobody_%s@x.invalid", env.suffix)
		results, err := env.externalRepo.FindByNormalizedEmail(env.ctx, queryEmail)
		require.NoError(t, err)
		matches := filterExternalBySourceIDPrefix(results, env.sourceIDPfx)
		assert.Empty(t, matches, "expected zero matches for case04")
	})

	t.Run("Case05_NullJSONBColumn_NoMatchNoError", func(t *testing.T) {
		// Pass nil bytes → SQL NULL in the emails column. The STRICT helper
		// short-circuits to NULL → row excluded by WHERE.
		_, err := env.fixtureRepo.InsertExternalContactRawEmails(env.ctx, "google", env.sourceIDFor("case05"), "Case 05", nil)
		require.NoError(t, err)

		queryEmail := fmt.Sprintf("anything_%s@x.invalid", env.suffix)
		results, err := env.externalRepo.FindByNormalizedEmail(env.ctx, queryEmail)
		require.NoError(t, err, "STRICT helper must not raise on NULL JSONB column")
		matches := filterExternalBySourceIDPrefix(results, env.sourceIDPfx)
		assert.Empty(t, matches)
	})

	t.Run("Case06_DuplicateOfId_Excluded", func(t *testing.T) {
		// Two rows share the same email; the second is marked as a duplicate
		// of the first via UpdateExternalContactDuplicate. Only the first
		// (non-duplicate) row should be returned.
		shared := fmt.Sprintf("dupe_%s@x.invalid", env.suffix)
		stored := []byte(fmt.Sprintf(`[{"value":"%s"}]`, shared))

		first, err := env.fixtureRepo.InsertExternalContactRawEmails(env.ctx, "google", env.sourceIDFor("case06_a"), "Case 06 A", stored)
		require.NoError(t, err)
		second, err := env.fixtureRepo.InsertExternalContactRawEmails(env.ctx, "google", env.sourceIDFor("case06_b"), "Case 06 B", stored)
		require.NoError(t, err)

		require.NoError(t, env.externalRepo.MarkAsDuplicate(env.ctx, uuid.UUID(second.ID.Bytes), uuid.UUID(first.ID.Bytes)))

		results, err := env.externalRepo.FindByNormalizedEmail(env.ctx, shared)
		require.NoError(t, err)
		matches := filterExternalBySourceIDPrefix(results, env.sourceIDPfx)
		assert.Len(t, matches, 1, "duplicate should be excluded")
	})

	t.Run("Case07_MultipleDistinctRows_ReturnedInCreatedAtOrder", func(t *testing.T) {
		shared := fmt.Sprintf("shared_%s@x.invalid", env.suffix)
		stored := []byte(fmt.Sprintf(`[{"value":"%s"}]`, shared))

		first, err := env.fixtureRepo.InsertExternalContactRawEmails(env.ctx, "google", env.sourceIDFor("case07_a"), "Case 07 A", stored)
		require.NoError(t, err)
		// Sleep one ms to disambiguate created_at ordering.
		time.Sleep(2 * time.Millisecond)
		second, err := env.fixtureRepo.InsertExternalContactRawEmails(env.ctx, "google", env.sourceIDFor("case07_b"), "Case 07 B", stored)
		require.NoError(t, err)

		results, err := env.externalRepo.FindByNormalizedEmail(env.ctx, shared)
		require.NoError(t, err)
		matches := filterExternalBySourceIDPrefix(results, env.sourceIDPfx)
		require.Len(t, matches, 2, "both non-duplicate rows should be returned")
		// Ordered by created_at, so first inserted should come first.
		assert.Equal(t, uuid.UUID(first.ID.Bytes), matches[0].ID)
		assert.Equal(t, uuid.UUID(second.ID.Bytes), matches[1].ID)
	})

	t.Run("Case08_ElementMissingValueKey_OtherElementMatches", func(t *testing.T) {
		stored := []byte(fmt.Sprintf(`[{"type":"home"},{"value":"only_%s@here.invalid"}]`, env.suffix))
		_, err := env.fixtureRepo.InsertExternalContactRawEmails(env.ctx, "google", env.sourceIDFor("case08"), "Case 08", stored)
		require.NoError(t, err)

		hit := fmt.Sprintf("only_%s@here.invalid", env.suffix)
		results, err := env.externalRepo.FindByNormalizedEmail(env.ctx, hit)
		require.NoError(t, err)
		matches := filterExternalBySourceIDPrefix(results, env.sourceIDPfx)
		assert.Len(t, matches, 1, "the element with the value key should match")

		// The other element's "type" field must not be findable as a value.
		miss, err := env.externalRepo.FindByNormalizedEmail(env.ctx, "home")
		require.NoError(t, err)
		// "home" must not match this fixture row even though one element has
		// type=home — the indexed projection is over the 'value' key only.
		homeMatches := filterExternalBySourceIDPrefix(miss, env.sourceIDPfx)
		assert.Empty(t, homeMatches, "type=home must not match a value=... lookup")
	})

	t.Run("Case09_AllElementsMissingKey_NoMatch", func(t *testing.T) {
		stored := []byte(`[{"type":"home"},{"type":"work"}]`)
		_, err := env.fixtureRepo.InsertExternalContactRawEmails(env.ctx, "google", env.sourceIDFor("case09"), "Case 09", stored)
		require.NoError(t, err)

		results, err := env.externalRepo.FindByNormalizedEmail(env.ctx, "home")
		require.NoError(t, err)
		matches := filterExternalBySourceIDPrefix(results, env.sourceIDPfx)
		assert.Empty(t, matches)
	})

	t.Run("Case10_NonArrayJSONB_NoMatchNoError", func(t *testing.T) {
		// Defensive: production code cannot produce a non-array JSONB but the
		// helper's jsonb_typeof guard must keep the new query safe in case
		// of schema drift. The legacy form would raise here — this is the
		// stale-sqlc tripwire for FindExternalContactsByNormalizedEmail.
		stored := []byte(`{"foo":"bar"}`)
		_, err := env.fixtureRepo.InsertExternalContactRawEmails(env.ctx, "google", env.sourceIDFor("case10"), "Case 10", stored)
		require.NoError(t, err)

		queryEmail := fmt.Sprintf("anything_%s@x.invalid", env.suffix)
		results, err := env.externalRepo.FindByNormalizedEmail(env.ctx, queryEmail)
		require.NoError(t, err, "non-array JSONB must not raise; helper guards via jsonb_typeof")
		matches := filterExternalBySourceIDPrefix(results, env.sourceIDPfx)
		assert.Empty(t, matches)
	})
}

// TestFindEventsByAttendeeEmailUnmatchedForContact_Behavior covers cases
// 11–16: the GIN-backed query for calendar_event.attendees.
func TestFindEventsByAttendeeEmailUnmatchedForContact_Behavior(t *testing.T) {
	env := setupJSONBGINEnv(t)
	now := accelerated.GetCurrentTime().UTC()

	t.Run("Case11_AttendeeMatches_ContactNotInMatched", func(t *testing.T) {
		email := fmt.Sprintf("user11_%s@x.invalid", env.suffix)
		stored := []byte(fmt.Sprintf(`[{"email":"%s"}]`, email))
		_, err := env.fixtureRepo.InsertCalendarEventRawAttendees(
			env.ctx,
			env.gcalIDFor("case11"), "primary", "acct11_"+env.suffix,
			now, now.Add(time.Hour),
			"confirmed", stored, nil,
		)
		require.NoError(t, err)

		freshContactID := uuid.New()
		results, err := env.calendarRepo.FindEventsByAttendeeEmailUnmatchedForContact(env.ctx, email, freshContactID)
		require.NoError(t, err)
		matches := filterCalendarByGcalIDPrefix(results, env.gcalIDPfx)
		assert.Len(t, matches, 1)
	})

	t.Run("Case12_ContactAlreadyInMatched_Excluded", func(t *testing.T) {
		email := fmt.Sprintf("user12_%s@x.invalid", env.suffix)
		stored := []byte(fmt.Sprintf(`[{"email":"%s"}]`, email))
		alreadyMatched := uuid.New()

		_, err := env.fixtureRepo.InsertCalendarEventRawAttendees(
			env.ctx,
			env.gcalIDFor("case12"), "primary", "acct12_"+env.suffix,
			now, now.Add(time.Hour),
			"confirmed", stored, []uuid.UUID{alreadyMatched},
		)
		require.NoError(t, err)

		results, err := env.calendarRepo.FindEventsByAttendeeEmailUnmatchedForContact(env.ctx, email, alreadyMatched)
		require.NoError(t, err)
		matches := filterCalendarByGcalIDPrefix(results, env.gcalIDPfx)
		assert.Empty(t, matches, "already-matched contact must be excluded")
	})

	t.Run("Case13_StatusCancelled_Excluded", func(t *testing.T) {
		email := fmt.Sprintf("user13_%s@x.invalid", env.suffix)
		stored := []byte(fmt.Sprintf(`[{"email":"%s"}]`, email))

		_, err := env.fixtureRepo.InsertCalendarEventRawAttendees(
			env.ctx,
			env.gcalIDFor("case13"), "primary", "acct13_"+env.suffix,
			now, now.Add(time.Hour),
			"cancelled", stored, nil,
		)
		require.NoError(t, err)

		results, err := env.calendarRepo.FindEventsByAttendeeEmailUnmatchedForContact(env.ctx, email, uuid.New())
		require.NoError(t, err)
		matches := filterCalendarByGcalIDPrefix(results, env.gcalIDPfx)
		assert.Empty(t, matches)
	})

	t.Run("Case14_EmptyAttendeesArray_NoMatch", func(t *testing.T) {
		_, err := env.fixtureRepo.InsertCalendarEventRawAttendees(
			env.ctx,
			env.gcalIDFor("case14"), "primary", "acct14_"+env.suffix,
			now, now.Add(time.Hour),
			"confirmed", []byte(`[]`), nil,
		)
		require.NoError(t, err)

		results, err := env.calendarRepo.FindEventsByAttendeeEmailUnmatchedForContact(
			env.ctx, fmt.Sprintf("anything_%s@x.invalid", env.suffix), uuid.New())
		require.NoError(t, err)
		matches := filterCalendarByGcalIDPrefix(results, env.gcalIDPfx)
		assert.Empty(t, matches)
	})

	t.Run("Case15_NullAttendeesColumn_NoMatchNoError", func(t *testing.T) {
		_, err := env.fixtureRepo.InsertCalendarEventRawAttendees(
			env.ctx,
			env.gcalIDFor("case15"), "primary", "acct15_"+env.suffix,
			now, now.Add(time.Hour),
			"confirmed", nil, nil,
		)
		require.NoError(t, err)

		results, err := env.calendarRepo.FindEventsByAttendeeEmailUnmatchedForContact(
			env.ctx, fmt.Sprintf("anything_%s@x.invalid", env.suffix), uuid.New())
		require.NoError(t, err, "STRICT helper must not raise on NULL JSONB column")
		matches := filterCalendarByGcalIDPrefix(results, env.gcalIDPfx)
		assert.Empty(t, matches)
	})

	t.Run("Case16_NonArrayAttendeesJSONB_NoMatchNoError", func(t *testing.T) {
		// Stale-sqlc tripwire for FindEventsByAttendeeEmailUnmatchedForContact:
		// new query returns no rows, legacy SQL would raise.
		stored := []byte(`{"foo":"bar"}`)
		_, err := env.fixtureRepo.InsertCalendarEventRawAttendees(
			env.ctx,
			env.gcalIDFor("case16"), "primary", "acct16_"+env.suffix,
			now, now.Add(time.Hour),
			"confirmed", stored, nil,
		)
		require.NoError(t, err)

		results, err := env.calendarRepo.FindEventsByAttendeeEmailUnmatchedForContact(
			env.ctx, fmt.Sprintf("anything_%s@x.invalid", env.suffix), uuid.New())
		require.NoError(t, err, "non-array JSONB must not raise")
		matches := filterCalendarByGcalIDPrefix(results, env.gcalIDPfx)
		assert.Empty(t, matches)
	})
}

// TestParity_NewVsLegacyForms_ExternalContact (case 17) populates a set of
// array-shape fixtures and asserts the new GIN-backed query and the
// permanent legacy-shape sqlc query return the same row IDs for the same
// lookup parameter. Excludes shapes (NULL, non-array) on which the legacy
// form raises or where parity is trivially satisfied.
func TestParity_NewVsLegacyForms_ExternalContact(t *testing.T) {
	env := setupJSONBGINEnv(t)

	// Lookup target — a value that case A and case C both contain.
	target := fmt.Sprintf("parity_target_%s@x.invalid", env.suffix)

	// Fixture A: stores the target in mixed case.
	storedA := []byte(fmt.Sprintf(`[{"value":"PARITY_target_%s@X.INVALID"}]`, env.suffix))
	_, err := env.fixtureRepo.InsertExternalContactRawEmails(env.ctx, "google", env.sourceIDFor("parity_a"), "Parity A", storedA)
	require.NoError(t, err)

	// Fixture B: stores a different email — should not match either form.
	storedB := []byte(fmt.Sprintf(`[{"value":"other_%s@x.invalid"}]`, env.suffix))
	_, err = env.fixtureRepo.InsertExternalContactRawEmails(env.ctx, "google", env.sourceIDFor("parity_b"), "Parity B", storedB)
	require.NoError(t, err)

	// Fixture C: target email plus an extra entry (covers multi-element shape).
	storedC := []byte(fmt.Sprintf(`[{"value":"first_%s@x.invalid"},{"value":"%s"}]`, env.suffix, target))
	_, err = env.fixtureRepo.InsertExternalContactRawEmails(env.ctx, "google", env.sourceIDFor("parity_c"), "Parity C", storedC)
	require.NoError(t, err)

	// Fixture D: empty array — neither form should match.
	_, err = env.fixtureRepo.InsertExternalContactRawEmails(env.ctx, "google", env.sourceIDFor("parity_d"), "Parity D", []byte(`[]`))
	require.NoError(t, err)

	newRows, err := env.externalRepo.FindByNormalizedEmail(env.ctx, target)
	require.NoError(t, err)
	legacyRows, err := env.fixtureRepo.FindExternalContactsByNormalizedEmailLegacy(env.ctx, target)
	require.NoError(t, err)

	newIDs := externalIDsForPrefix(newRows, env.sourceIDPfx)
	legacyIDs := dbExternalIDsForPrefix(legacyRows, env.sourceIDPfx)
	assert.Equal(t, newIDs, legacyIDs, "new and legacy queries must return identical IDs for array-shape fixtures")
}

// TestParity_NewVsLegacyForms_CalendarEvent (case 18) — same shape parity
// check for the calendar_event query.
func TestParity_NewVsLegacyForms_CalendarEvent(t *testing.T) {
	env := setupJSONBGINEnv(t)
	now := accelerated.GetCurrentTime().UTC()
	target := fmt.Sprintf("parity_evt_%s@x.invalid", env.suffix)
	freshContactID := uuid.New()

	// Fixture A: target email present, status=confirmed, contact not matched.
	storedA := []byte(fmt.Sprintf(`[{"email":"%s"}]`, target))
	_, err := env.fixtureRepo.InsertCalendarEventRawAttendees(
		env.ctx,
		env.gcalIDFor("parity_a"), "primary", "acct_pa_"+env.suffix,
		now, now.Add(time.Hour), "confirmed", storedA, nil,
	)
	require.NoError(t, err)

	// Fixture B: target email present BUT status=cancelled (filter excludes).
	_, err = env.fixtureRepo.InsertCalendarEventRawAttendees(
		env.ctx,
		env.gcalIDFor("parity_b"), "primary", "acct_pb_"+env.suffix,
		now, now.Add(time.Hour), "cancelled", storedA, nil,
	)
	require.NoError(t, err)

	// Fixture C: contact already matched — must be excluded.
	storedC := []byte(fmt.Sprintf(`[{"email":"%s"}]`, target))
	_, err = env.fixtureRepo.InsertCalendarEventRawAttendees(
		env.ctx,
		env.gcalIDFor("parity_c"), "primary", "acct_pc_"+env.suffix,
		now, now.Add(time.Hour), "confirmed", storedC, []uuid.UUID{freshContactID},
	)
	require.NoError(t, err)

	// Fixture D: empty attendees.
	_, err = env.fixtureRepo.InsertCalendarEventRawAttendees(
		env.ctx,
		env.gcalIDFor("parity_d"), "primary", "acct_pd_"+env.suffix,
		now, now.Add(time.Hour), "confirmed", []byte(`[]`), nil,
	)
	require.NoError(t, err)

	newRows, err := env.calendarRepo.FindEventsByAttendeeEmailUnmatchedForContact(env.ctx, target, freshContactID)
	require.NoError(t, err)
	legacyRows, err := env.fixtureRepo.FindEventsByAttendeeEmailUnmatchedForContactLegacy(env.ctx, target, freshContactID)
	require.NoError(t, err)

	newIDs := calendarIDsForPrefix(newRows, env.gcalIDPfx)
	legacyIDs := dbCalendarIDsForPrefix(legacyRows, env.gcalIDPfx)
	assert.Equal(t, newIDs, legacyIDs, "new and legacy queries must return identical IDs for array-shape fixtures")
}

// filterExternalBySourceIDPrefix narrows a result set to rows whose source_id
// starts with the per-test prefix. Necessary because the test DB is shared
// across runs and other tests may have left rows behind.
func filterExternalBySourceIDPrefix(rows []repository.ExternalContact, prefix string) []repository.ExternalContact {
	out := make([]repository.ExternalContact, 0, len(rows))
	for _, r := range rows {
		if hasPrefix(r.SourceID, prefix) {
			out = append(out, r)
		}
	}
	return out
}

func filterCalendarByGcalIDPrefix(rows []repository.CalendarEvent, prefix string) []repository.CalendarEvent {
	out := make([]repository.CalendarEvent, 0, len(rows))
	for _, r := range rows {
		if hasPrefix(r.GcalEventID, prefix) {
			out = append(out, r)
		}
	}
	return out
}

func hasPrefix(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}

// externalIDsForPrefix returns sorted UUIDs from a repo result set, filtered
// to rows owned by this test (by source_id prefix).
func externalIDsForPrefix(rows []repository.ExternalContact, prefix string) []uuid.UUID {
	ids := make([]uuid.UUID, 0, len(rows))
	for _, r := range rows {
		if hasPrefix(r.SourceID, prefix) {
			ids = append(ids, r.ID)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i].String() < ids[j].String() })
	return ids
}

func calendarIDsForPrefix(rows []repository.CalendarEvent, prefix string) []uuid.UUID {
	ids := make([]uuid.UUID, 0, len(rows))
	for _, r := range rows {
		if hasPrefix(r.GcalEventID, prefix) {
			ids = append(ids, r.ID)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i].String() < ids[j].String() })
	return ids
}

// dbExternalIDsForPrefix is the equivalent of externalIDsForPrefix for the
// generated db.ExternalContact type returned by the legacy parity query.
func dbExternalIDsForPrefix(rows []*db.ExternalContact, prefix string) []uuid.UUID {
	ids := make([]uuid.UUID, 0, len(rows))
	for _, r := range rows {
		if r == nil || !r.ID.Valid {
			continue
		}
		if hasPrefix(r.SourceID, prefix) {
			ids = append(ids, uuid.UUID(r.ID.Bytes))
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i].String() < ids[j].String() })
	return ids
}

func dbCalendarIDsForPrefix(rows []*db.CalendarEvent, prefix string) []uuid.UUID {
	ids := make([]uuid.UUID, 0, len(rows))
	for _, r := range rows {
		if r == nil || !r.ID.Valid {
			continue
		}
		if hasPrefix(r.GcalEventID, prefix) {
			ids = append(ids, uuid.UUID(r.ID.Bytes))
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i].String() < ids[j].String() })
	return ids
}
