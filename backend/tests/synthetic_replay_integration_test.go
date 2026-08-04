package tests

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/synthetic"
	"personal-crm/backend/internal/synthetic/factory"
	"personal-crm/backend/internal/todoist"
	"personal-crm/backend/tests/testsupport"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// These replay integration tests are SLOW-gated (testsupport.RequireLongTests):
// skipped in CI's fast PR gate, run pre-push (LONG_TESTS=1) + locally via
// make test-integration, AND nightly via BACKEND_SLOW_TESTS_REGEX (the
// TestSynthetic name prefix). Each sub-test uses a UNIQUE namespace so
// shared-test-DB reuse cannot collide; teardown is the harness's auto-registered
// quiesce + conditional-cleanup closure (D8).

func newSyntheticDB(t *testing.T) (*db.Database, context.Context) {
	t.Helper()
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set")
	}
	ctx := context.Background()
	require.NoError(t, db.RunMigrations(ctx, databaseURL, getMigrationsPath()))
	cfg := config.TestConfig()
	cfg.Database.URL = databaseURL
	database, err := db.NewDatabase(ctx, cfg.Database)
	require.NoError(t, err)
	t.Cleanup(database.Close)
	return database, ctx
}

// ns builds a unique per-sub-test namespace (sanitized token segment).
func syntheticNS(t *testing.T) string {
	return "r" + uuid.NewString()[:8]
}

func TestSyntheticReplay_SeededSenderSettled(t *testing.T) {
	testsupport.RequireLongTests(t)
	database, ctx := newSyntheticDB(t)

	t.Run("gmail", func(t *testing.T) {
		h := synthetic.NewHarnessForNamespace(t, ctx, database, syntheticNS(t), factory.DefaultSeed)
		gen := h.Generator()
		spec := gen.Contact(factory.WithEmail())
		contact, err := h.SeedContact(ctx, spec)
		require.NoError(t, err)
		res, err := h.ReplayGmail(ctx, contact.ID, gen.GmailMessage(spec, factory.MatchSeeded))
		require.NoError(t, err)
		require.True(t, res.Matched)
		requireInteractionSource(t, ctx, h, contact.ID, "email")
	})

	t.Run("telegram", func(t *testing.T) {
		h := synthetic.NewHarnessForNamespace(t, ctx, database, syntheticNS(t), factory.DefaultSeed)
		gen := h.Generator()
		spec := gen.Contact(factory.WithTelegram())
		contact, err := h.SeedContact(ctx, spec)
		require.NoError(t, err)
		res, err := h.ReplayTelegram(ctx, contact.ID, gen.TelegramMessage(spec, factory.MatchSeeded))
		require.NoError(t, err)
		require.True(t, res.Matched)
		requireInteractionSource(t, ctx, h, contact.ID, "telegram")
	})

	t.Run("gcal", func(t *testing.T) {
		h := synthetic.NewHarnessForNamespace(t, ctx, database, syntheticNS(t), factory.DefaultSeed)
		gen := h.Generator()
		spec := gen.Contact(factory.WithEmail())
		contact, err := h.SeedContact(ctx, spec)
		require.NoError(t, err)
		res, err := h.ReplayGCal(ctx, contact.ID, gen.GCalEvent(spec, factory.MatchSeeded))
		require.NoError(t, err)
		require.True(t, res.Matched)
		requireInteractionSource(t, ctx, h, contact.ID, "gcal")
	})

	t.Run("gchat", func(t *testing.T) {
		h := synthetic.NewHarnessForNamespace(t, ctx, database, syntheticNS(t), factory.DefaultSeed)
		gen := h.Generator()
		spec := gen.Contact(factory.WithEmail())
		contact, err := h.SeedContact(ctx, spec)
		require.NoError(t, err)
		res, err := h.ReplayGChat(ctx, contact.ID, gen.GChatMessage(spec, factory.MatchSeeded))
		require.NoError(t, err)
		require.True(t, res.Matched)
		requireInteractionSource(t, ctx, h, contact.ID, "gchat")
	})

	t.Run("whatsapp", func(t *testing.T) {
		h := synthetic.NewHarnessForNamespace(t, ctx, database, syntheticNS(t), factory.DefaultSeed)
		gen := h.Generator()
		spec := gen.Contact(factory.WithPhone())
		contact, err := h.SeedContact(ctx, spec)
		require.NoError(t, err)
		res, err := h.ReplayWhatsApp(ctx, contact.ID, gen.WhatsAppMessage(spec, factory.MatchSeeded))
		require.NoError(t, err)
		require.True(t, res.Matched)
		requireInteractionSource(t, ctx, h, contact.ID, "whatsapp")
	})

	t.Run("imessage", func(t *testing.T) {
		h := synthetic.NewHarnessForNamespace(t, ctx, database, syntheticNS(t), factory.DefaultSeed)
		gen := h.Generator()
		spec := gen.Contact(factory.WithPhone())
		contact, err := h.SeedContact(ctx, spec)
		require.NoError(t, err)
		imsg, err := gen.IMessage(spec, factory.MatchSeeded, h.MacHostID())
		require.NoError(t, err)
		res, err := h.ReplayIMessage(ctx, contact.ID, imsg)
		require.NoError(t, err)
		require.True(t, res.Matched)
		requireInteractionSource(t, ctx, h, contact.ID, "messages")
	})

	t.Run("mac_contacts", func(t *testing.T) {
		h := synthetic.NewHarnessForNamespace(t, ctx, database, syntheticNS(t), factory.DefaultSeed)
		gen := h.Generator()
		spec := gen.Contact(factory.WithEmail())
		contact, err := h.SeedContact(ctx, spec)
		require.NoError(t, err)
		mc, err := gen.MacContact(spec, factory.MatchSeeded, h.MacHostID())
		require.NoError(t, err)
		res, err := h.ReplayMacContacts(ctx, contact.ID, mc)
		require.NoError(t, err)
		require.True(t, res.Matched)
	})

	t.Run("todoist", func(t *testing.T) {
		h := synthetic.NewHarnessForNamespace(t, ctx, database, syntheticNS(t), factory.DefaultSeed)
		gen := h.Generator()
		spec := gen.Contact(factory.WithEmail(), factory.WithCadence("weekly"))
		contact, err := h.SeedContact(ctx, spec)
		require.NoError(t, err)
		_, err = h.ReplayTodoist(ctx, []uuid.UUID{contact.ID})
		require.NoError(t, err)
	})
}

// TestReplayTodoistRecurringEdit_UnmanagesViaRealPath proves the seed reaches the
// cadence_due `unmanaged` state through the PRODUCTION recurring-edit path
// (provider.Sync → processItem → handleRecurringDetection), not a raw state write.
// It seeds two cadence-bearing contacts, reconciles both to `managed` via
// ReplayTodoist, then runs ReplayTodoistRecurringEdit on ONLY the first. The first
// task must end `unmanaged`; the same-namespace second must stay `managed`, proving
// (a) the recurring edit transitioned only the item it matched by external id, and
// (b) the trailing reconcile was non-destructive to an untouched managed sibling. It
// does NOT prove namespace-scoped confinement — the same-namespace bystander is
// inside the allow-set, so even an unscoped reconcile would leave its already-managed
// task managed. That confinement property is guarded separately below by a
// cross-namespace bystander an unscoped reconcile WOULD visibly mutate. It also
// confirms ReplayTodoist's reconcile finalized the temp id into a Todoist-v1
// alphanumeric external id (cleared pending_temp_id) — the id the recurring edit
// must match on.
func TestReplayTodoistRecurringEdit_UnmanagesViaRealPath(t *testing.T) {
	testsupport.RequireLongTests(t)
	database, ctx := newSyntheticDB(t)

	h := synthetic.NewHarnessForNamespace(t, ctx, database, syntheticNS(t), factory.DefaultSeed)
	gen := h.Generator()

	target, err := h.SeedContact(ctx, gen.Contact(factory.WithEmail(), factory.WithCadence("weekly")))
	require.NoError(t, err)
	bystander, err := h.SeedContact(ctx, gen.Contact(factory.WithEmail(), factory.WithCadence("weekly")))
	require.NoError(t, err)

	_, err = h.ReplayTodoist(ctx, []uuid.UUID{target.ID, bystander.ID})
	require.NoError(t, err)

	// Cross-namespace confinement guard: seed a reconcile-ELIGIBLE cadence contact in a
	// SECOND namespace (normal factory + SeedContact populates contact_by), with NO
	// ReplayTodoist so it has no cadence_due task yet. The recurring edit's trailing Sync
	// reconciles DB-wide; if the namespace-scoped contact-lister were dropped, that
	// reconcile would enumerate this contact via ListContactsWithContactBy, find it due
	// with no task, and CREATE a managed task. Asserting NO task is created is the only
	// test that catches a regression dropping the scope — a mutation an unscoped run
	// would perform (unlike the same-namespace bystander, whose already-managed task
	// stays managed either way).
	hOther := synthetic.NewHarnessForNamespace(t, ctx, database, syntheticNS(t), factory.DefaultSeed)
	crossNS, err := hOther.SeedContact(ctx, hOther.Generator().Contact(factory.WithEmail(), factory.WithCadence("weekly")))
	require.NoError(t, err)

	// Both cadence tasks start managed; the target's temp id is finalized to a
	// Todoist-v1 alphanumeric external id (the id the recurring edit matches on).
	taskRepo := repository.NewContactTaskRepository(database.Queries)
	targetTask, err := taskRepo.GetContactTaskByContactCadenceDue(ctx, target.ID, todoist.SourceName)
	require.NoError(t, err)
	require.Equal(t, repository.ContactTaskStateManaged, targetTask.State, "target starts managed")
	require.Regexp(t, "^[A-Za-z0-9]+$", targetTask.ExternalTaskID, "reconcile finalized the temp id to a Todoist-v1 alphanumeric id")
	require.NotContains(t, targetTask.Metadata, todoist.MetadataKeyPendingTempID, "finalize cleared pending_temp_id")

	// Edit the target's Todoist task to recur — drives the REAL path to unmanaged.
	require.NoError(t, h.ReplayTodoistRecurringEdit(ctx, target.ID))

	targetTask, err = taskRepo.GetContactTaskByContactCadenceDue(ctx, target.ID, todoist.SourceName)
	require.NoError(t, err)
	require.Equal(t, repository.ContactTaskStateUnmanaged, targetTask.State,
		"recurring edit unmanaged the target via handleRecurringDetection")

	bystanderTask, err := taskRepo.GetContactTaskByContactCadenceDue(ctx, bystander.ID, todoist.SourceName)
	require.NoError(t, err)
	require.Equal(t, repository.ContactTaskStateManaged, bystanderTask.State,
		"bystander cadence task untouched by the scoped recurring-edit reconcile")

	// Confinement: the cross-namespace contact must have NO cadence_due task — the
	// DB-wide reconcile did not reach it (namespace scoping held). A dropped scope
	// would have created one.
	_, err = taskRepo.GetContactTaskByContactCadenceDue(ctx, crossNS.ID, todoist.SourceName)
	require.ErrorIs(t, err, db.ErrNotFound,
		"cross-namespace reconcile-eligible contact must get NO task — the scoped reconcile never enumerated it")
}

// TestReplayAssertion_RefreshesKnowledgeCache proves ReplayAssertion drives the
// REAL recompute-from-current-accepted cache path (KnowledgeCacheUpdater.RefreshTx
// in the assert tx), not a nil-only fill or a raw column write. Three cases:
//   - fresh: a cutover birthday assertion on a no-birthday contact populates the
//     derived birthday cache to the asserted value;
//   - supersession: re-asserting a DIFFERENT current birthday recomputes the cache
//     to the new value (an only-fills-NULL bug would leave it at the first value —
//     the cache must track the current-accepted assertion, not a one-time write);
//   - control: a non-cutover text fact returns no error and writes NO cache column,
//     guarding that a non-cutover predicate never reaches RefreshTx (which errors).
//
// Serial (no t.Parallel) to match every sibling in this file — the harness runs a
// live River client, so DB-wide river_job contention argues against parallelism.
func TestReplayAssertion_RefreshesKnowledgeCache(t *testing.T) {
	testsupport.RequireLongTests(t)
	database, ctx := newSyntheticDB(t)

	h := synthetic.NewHarnessForNamespace(t, ctx, database, syntheticNS(t), factory.DefaultSeed)
	gen := h.Generator()
	anchor := gen.Anchor()

	// Fresh: a no-birthday contact receives a cutover birthday assertion; its derived
	// birthday cache must populate to the asserted date A.
	contact, err := h.SeedContact(ctx, gen.Contact(factory.WithEmail()))
	require.NoError(t, err)
	require.Nil(t, contact.Birthday, "seeded contact starts with no birthday")

	dateA := time.Date(anchor.Year()-30, time.March, 3, 0, 0, 0, 0, time.UTC)
	_, err = h.ReplayAssertion(ctx, contact.ID, gen.DateFact("birthday", dateA))
	require.NoError(t, err)

	got, err := h.ContactRepo().GetContact(ctx, contact.ID)
	require.NoError(t, err)
	require.NotNil(t, got.Birthday, "cutover birthday assertion populates the derived birthday cache")
	requireSameDate(t, dateA, *got.Birthday, "fresh birthday cache equals the asserted date A")

	// Supersession: re-assert a DIFFERENT current birthday (date B); birthday is
	// single-cardinality, so the second assertion supersedes the first and RefreshTx
	// recomputes the cache from the new current-accepted row. An only-fills-NULL bug
	// would leave it at date A.
	dateB := time.Date(anchor.Year()-25, time.September, 9, 0, 0, 0, 0, time.UTC)
	_, err = h.ReplayAssertion(ctx, contact.ID, gen.DateFact("birthday", dateB))
	require.NoError(t, err)

	got, err = h.ContactRepo().GetContact(ctx, contact.ID)
	require.NoError(t, err)
	require.NotNil(t, got.Birthday, "birthday cache stays populated after supersession")
	requireSameDate(t, dateB, *got.Birthday, "supersession recomputes the birthday cache to date B, not a one-time fill")

	// Control: a non-cutover text fact must NOT reach RefreshTx (which would error) and
	// must leave every derived cache column NULL.
	control, err := h.SeedContact(ctx, gen.Contact(factory.WithEmail()))
	require.NoError(t, err)
	_, err = h.ReplayAssertion(ctx, control.ID, gen.FactAssertion("job_title"))
	require.NoError(t, err, "a non-cutover assertion must succeed without touching the cache")

	controlGot, err := h.ContactRepo().GetContact(ctx, control.ID)
	require.NoError(t, err)
	require.Nil(t, controlGot.Birthday, "non-cutover assertion leaves birthday cache NULL")
	require.Nil(t, controlGot.Location, "non-cutover assertion leaves location cache NULL")
	require.Nil(t, controlGot.HowMet, "non-cutover assertion leaves how_met cache NULL")
}

// requireSameDate asserts two times fall on the same calendar day (birthday is a
// DATE column, so only Y/M/D are meaningful).
func requireSameDate(t *testing.T, want, got time.Time, msg string) {
	t.Helper()
	wy, wm, wd := want.Date()
	gy, gm, gd := got.UTC().Date()
	require.Equal(t, [3]int{wy, int(wm), wd}, [3]int{gy, int(gm), gd}, msg)
}

func TestSyntheticReplay_UnknownSenderPending(t *testing.T) {
	testsupport.RequireLongTests(t)
	database, ctx := newSyntheticDB(t)

	// Only the four pending-capable sources (D4 matrix).
	t.Run("mac_contacts_unmatched", func(t *testing.T) {
		h := synthetic.NewHarnessForNamespace(t, ctx, database, syntheticNS(t), factory.DefaultSeed)
		gen := h.Generator()
		mc, err := gen.MacContact(gen.Contact(factory.WithEmail()), factory.MatchUnknown, h.MacHostID())
		require.NoError(t, err)
		res, err := h.ReplayMacContacts(ctx, uuid.Nil, mc)
		require.NoError(t, err)
		require.False(t, res.Matched)
	})

	t.Run("imessage_stranded", func(t *testing.T) {
		h := synthetic.NewHarnessForNamespace(t, ctx, database, syntheticNS(t), factory.DefaultSeed)
		gen := h.Generator()
		imsg, err := gen.IMessage(gen.Contact(factory.WithPhone()), factory.MatchUnknown, h.MacHostID())
		require.NoError(t, err)
		res, err := h.ReplayIMessage(ctx, uuid.Nil, imsg)
		require.NoError(t, err)
		require.False(t, res.Matched)
	})

	t.Run("telegram_stranded", func(t *testing.T) {
		h := synthetic.NewHarnessForNamespace(t, ctx, database, syntheticNS(t), factory.DefaultSeed)
		gen := h.Generator()
		res, err := h.ReplayTelegram(ctx, uuid.Nil, gen.TelegramMessage(gen.Contact(factory.WithTelegram()), factory.MatchUnknown))
		require.NoError(t, err)
		require.False(t, res.Matched)
	})

	t.Run("gcal_unmatched_attendee", func(t *testing.T) {
		h := synthetic.NewHarnessForNamespace(t, ctx, database, syntheticNS(t), factory.DefaultSeed)
		gen := h.Generator()
		res, err := h.ReplayGCal(ctx, uuid.Nil, gen.GCalEvent(gen.Contact(factory.WithEmail()), factory.MatchUnknown))
		require.NoError(t, err)
		require.False(t, res.Matched)
	})

	// WhatsApp belongs HERE, not in the match-only test: gmail/gchat write no
	// row for an unknown sender, while matched_contact_id IS NULL is legal for
	// whatsapp and the row IS written — it simply never aggregates.
	t.Run("whatsapp_stranded", func(t *testing.T) {
		h := synthetic.NewHarnessForNamespace(t, ctx, database, syntheticNS(t), factory.DefaultSeed)
		gen := h.Generator()
		target := gen.Contact(factory.WithPhone())
		contact, err := h.SeedContact(ctx, target)
		require.NoError(t, err)

		spec := gen.WhatsAppMessage(gen.Contact(factory.WithPhone()), factory.MatchUnknown)
		res, err := h.ReplayWhatsApp(ctx, uuid.Nil, spec)
		require.NoError(t, err)
		require.False(t, res.Matched)

		exists, err := h.CommsRowExists(ctx, "whatsapp", spec.ExternalID)
		require.NoError(t, err)
		require.True(t, exists, "an unknown WhatsApp peer's message IS stored, just unattached")

		rows, err := h.InteractionRepo().ListContactInteractions(ctx, contact.ID, 100, 0)
		require.NoError(t, err)
		for _, r := range rows {
			require.NotEqual(t, "whatsapp", r.Source, "an unattached whatsapp row must not produce an interaction")
		}
	})
}

func TestSyntheticReplay_UnknownSenderMatchOnly(t *testing.T) {
	testsupport.RequireLongTests(t)
	database, ctx := newSyntheticDB(t)

	// Gmail/GChat unknown sender: match-only (no comms_message row written for
	// the unknown correspondent, hence no interaction, no contact create).
	t.Run("gmail_match_only", func(t *testing.T) {
		h := synthetic.NewHarnessForNamespace(t, ctx, database, syntheticNS(t), factory.DefaultSeed)
		gen := h.Generator()
		spec := gen.GmailMessage(gen.Contact(factory.WithEmail()), factory.MatchUnknown)
		res, err := h.ReplayGmail(ctx, uuid.Nil, spec)
		require.NoError(t, err)
		require.False(t, res.Matched)
		exists, err := h.CommsRowExists(ctx, "email", spec.ExternalID)
		require.NoError(t, err)
		require.False(t, exists, "unknown Gmail correspondent must not produce a comms_message row")
	})

	t.Run("gchat_match_only", func(t *testing.T) {
		h := synthetic.NewHarnessForNamespace(t, ctx, database, syntheticNS(t), factory.DefaultSeed)
		gen := h.Generator()
		spec := gen.GChatMessage(gen.Contact(factory.WithEmail()), factory.MatchUnknown)
		res, err := h.ReplayGChat(ctx, uuid.Nil, spec)
		require.NoError(t, err)
		require.False(t, res.Matched)
		exists, err := h.CommsRowExists(ctx, "gchat", spec.ExternalID)
		require.NoError(t, err)
		require.False(t, exists, "unknown GChat sender must not produce a comms_message row")
	})
}

func TestSyntheticReplay_IdempotentReReplay(t *testing.T) {
	testsupport.RequireLongTests(t)
	database, ctx := newSyntheticDB(t)

	h := synthetic.NewHarnessForNamespace(t, ctx, database, syntheticNS(t), factory.DefaultSeed)
	gen := h.Generator()
	spec := gen.Contact(factory.WithEmail())
	contact, err := h.SeedContact(ctx, spec)
	require.NoError(t, err)

	// Same source payload replayed twice ⇒ stable source-ids dedup to one row.
	msg := gen.GmailMessage(spec, factory.MatchSeeded)
	_, err = h.ReplayGmail(ctx, contact.ID, msg)
	require.NoError(t, err)
	_, err = h.ReplayGmail(ctx, contact.ID, msg)
	require.NoError(t, err)

	rows, err := h.CommsRepo().ListByContact(ctx, contact.ID)
	require.NoError(t, err)
	require.Len(t, rows, 1, "re-replay of the same payload must not add a duplicate comms_message row")

	// WhatsApp shares the same comms_message store and the same (source,
	// external_id) dedup, driven through its own ingest seam.
	waSpec := gen.Contact(factory.WithPhone())
	waContact, err := h.SeedContact(ctx, waSpec)
	require.NoError(t, err)
	waMsg := gen.WhatsAppMessage(waSpec, factory.MatchSeeded)
	_, err = h.ReplayWhatsApp(ctx, waContact.ID, waMsg)
	require.NoError(t, err)
	_, err = h.ReplayWhatsApp(ctx, waContact.ID, waMsg)
	require.NoError(t, err)

	waRows, err := h.CommsRepo().ListByContact(ctx, waContact.ID)
	require.NoError(t, err)
	require.Len(t, waRows, 1, "re-replay of the same whatsapp payload must not add a duplicate row")
}

// requireInteractionSource asserts the contact has at least one interaction with
// the given source after settle.
func requireInteractionSource(t *testing.T, ctx context.Context, h *synthetic.Harness, contactID uuid.UUID, source string) {
	t.Helper()
	rows, err := h.InteractionRepo().ListContactInteractions(ctx, contactID, 100, 0)
	require.NoError(t, err)
	found := false
	for _, r := range rows {
		if r.Source == source {
			found = true
			break
		}
	}
	require.True(t, found, fmt.Sprintf("expected an interaction with source=%s for contact %s", source, contactID))
}
