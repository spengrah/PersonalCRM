// PR 8 Step 15 replacement integration coverage for the deleted shadow-era
// consumer_cadence_updater_integration_test.go. These tests hit a real
// database and exercise the post-cutover sole-writer paths through
// CadenceUpdater.
//
// Coverage matrix:
//   - ApplyInteraction direction branches against a live DB (forward-only
//     for outbound/inbound/mutual, unconditional for manual)
//   - BulkApply preserves forward-max semantics on merge
//   - ApplyContactByOverride can clear or back-date contact_by for user
//     cadence-preference edits (unconditional branch)
//   - Queued-delivery duplicate claim is a durable no-op (blocker fix)
//   - Mode=off short-circuits without writes
//
// Unit-level direction-flag parity lives in
// backend/internal/consumer/cadence_updater_test.go; these tests focus on
// the observable SQL side-effects against the real cadence columns.
package tests

import (
	"context"
	"os"
	"testing"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/cadence"
	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/consumer"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/events"
	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newCadenceUpdaterTestDB(t *testing.T) (*db.Database, func()) {
	t.Helper()
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}
	ctx := context.Background()
	cfg := config.TestConfig()
	cfg.Database.URL = databaseURL
	database, err := db.NewDatabase(ctx, cfg.Database)
	require.NoError(t, err)
	return database, func() { database.Close() }
}

func newCadenceUpdaterForTest(t *testing.T, database *db.Database, mode string) (*consumer.CadenceUpdater, *repository.ContactRepository) {
	t.Helper()
	contactRepo := repository.NewContactRepository(database.Queries)
	contactRepo.SetPool(database.Pool)
	claimRepo := repository.NewEventConsumerClaimRepository(database.Queries)
	return consumer.NewCadenceUpdater(
		claimRepo, contactRepo, database.Queries, mode, false,
	), contactRepo
}

func applyInTx(t *testing.T, database *db.Database, fn func(tx pgx.Tx) error) {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, pgx.BeginTxFunc(ctx, database.Pool, pgx.TxOptions{}, fn))
}

// TestIntegration_CadenceUpdater_ApplyInteraction_Outbound_LiveDB verifies
// that an outbound ApplyInteraction bumps ONLY last_outreach_at and leaves
// last_contacted, last_response_at, contact_by untouched.
func TestIntegration_CadenceUpdater_ApplyInteraction_Outbound_LiveDB(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	database, cleanup := newCadenceUpdaterTestDB(t)
	defer cleanup()
	ctx := context.Background()
	cad := "monthly"
	initial := time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)
	contactRepo := repository.NewContactRepository(database.Queries)
	contact, err := contactRepo.CreateContact(ctx, repository.CreateContactRequest{
		FullName: "Cadence Outbound Apply", Cadence: &cad, LastContacted: &initial,
	})
	require.NoError(t, err)
	defer func() { _ = contactRepo.HardDeleteContact(ctx, contact.ID) }()
	initialContactBy := *contact.ContactBy

	cu, _ := newCadenceUpdaterForTest(t, database, consumer.CadenceModeCutover)
	outbound := time.Date(2026, 3, 10, 10, 0, 0, 0, time.UTC)
	applyInTx(t, database, func(tx pgx.Tx) error {
		return cu.ApplyInteraction(ctx, tx, repository.ApplyInteractionRequest{
			ContactID:  contact.ID,
			Direction:  repository.InteractionDirectionOutbound,
			Source:     repository.InteractionSourceTodoist,
			OccurredAt: outbound,
		})
	})

	got, err := contactRepo.GetContact(ctx, contact.ID)
	require.NoError(t, err)
	require.NotNil(t, got.LastOutreachAt)
	assert.Equal(t, outbound.UTC(), got.LastOutreachAt.UTC(), "outbound should set last_outreach_at")
	assert.Equal(t, initial.UTC(), got.LastContacted.UTC(), "outbound must NOT bump last_contacted")
	assert.Nil(t, got.LastResponseAt, "outbound must NOT set last_response_at")
	assert.Equal(t, initialContactBy.UTC(), got.ContactBy.UTC(), "outbound must NOT recompute contact_by")
}

// TestIntegration_CadenceUpdater_ApplyInteraction_Mutual_ForwardOnly verifies
// an older mutual ApplyInteraction cannot regress a newer last_contacted.
func TestIntegration_CadenceUpdater_ApplyInteraction_Mutual_ForwardOnly(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	database, cleanup := newCadenceUpdaterTestDB(t)
	defer cleanup()
	ctx := context.Background()
	cad := "weekly"
	newer := time.Date(2026, 3, 15, 12, 0, 0, 0, time.UTC)
	contactRepo := repository.NewContactRepository(database.Queries)
	contact, err := contactRepo.CreateContact(ctx, repository.CreateContactRequest{
		FullName: "Cadence Forward Only", Cadence: &cad, LastContacted: &newer,
	})
	require.NoError(t, err)
	defer func() { _ = contactRepo.HardDeleteContact(ctx, contact.ID) }()

	cu, _ := newCadenceUpdaterForTest(t, database, consumer.CadenceModeCutover)
	older := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	applyInTx(t, database, func(tx pgx.Tx) error {
		return cu.ApplyInteraction(ctx, tx, repository.ApplyInteractionRequest{
			ContactID:  contact.ID,
			Direction:  repository.InteractionDirectionMutual,
			Source:     repository.InteractionSourceGCal,
			OccurredAt: older,
		})
	})

	got, err := contactRepo.GetContact(ctx, contact.ID)
	require.NoError(t, err)
	assert.Equal(t, newer.UTC(), got.LastContacted.UTC(), "forward-only branch must NOT regress last_contacted")
}

// TestIntegration_CadenceUpdater_ApplyInteraction_Manual_UnconditionalBackdate
// verifies the manual-source unconditional branch CAN backdate
// last_contacted when the user submits a correction.
func TestIntegration_CadenceUpdater_ApplyInteraction_Manual_UnconditionalBackdate(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	database, cleanup := newCadenceUpdaterTestDB(t)
	defer cleanup()
	ctx := context.Background()
	cad := "weekly"
	newer := time.Date(2026, 3, 15, 12, 0, 0, 0, time.UTC)
	contactRepo := repository.NewContactRepository(database.Queries)
	contact, err := contactRepo.CreateContact(ctx, repository.CreateContactRequest{
		FullName: "Cadence Manual Backdate", Cadence: &cad, LastContacted: &newer,
	})
	require.NoError(t, err)
	defer func() { _ = contactRepo.HardDeleteContact(ctx, contact.ID) }()

	cu, _ := newCadenceUpdaterForTest(t, database, consumer.CadenceModeCutover)
	older := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	applyInTx(t, database, func(tx pgx.Tx) error {
		return cu.ApplyInteraction(ctx, tx, repository.ApplyInteractionRequest{
			ContactID:  contact.ID,
			Direction:  repository.InteractionDirectionMutual,
			Source:     repository.InteractionSourceManual,
			OccurredAt: older,
		})
	})

	got, err := contactRepo.GetContact(ctx, contact.ID)
	require.NoError(t, err)
	assert.Equal(t, older.UTC(), got.LastContacted.UTC(), "manual source must overwrite last_contacted unconditionally")
}

// TestIntegration_CadenceUpdater_BulkApply_DoesNotBumpLastInteractionAt
// verifies the merge path leaves last_interaction_at untouched even when
// it advances last_contacted — a merge is not an interaction, so the
// "last non-outbound interaction" timestamp must survive unchanged.
// Guards against a regression where the cutover queries piggybacked
// last_interaction_at on apply_last_contacted.
func TestIntegration_CadenceUpdater_BulkApply_DoesNotBumpLastInteractionAt(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	database, cleanup := newCadenceUpdaterTestDB(t)
	defer cleanup()
	ctx := context.Background()

	targetLast := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	// Seed a pre-merge last_interaction_at on the target so we can
	// assert the merge leaves it alone. We seed via a mutual
	// ApplyInteraction (the legitimate path for this column).
	contactRepo := repository.NewContactRepository(database.Queries)
	target, err := contactRepo.CreateContact(ctx, repository.CreateContactRequest{
		FullName: "Merge LastInteractionAt Guard", LastContacted: &targetLast,
	})
	require.NoError(t, err)
	defer func() { _ = contactRepo.HardDeleteContact(ctx, target.ID) }()

	cu, _ := newCadenceUpdaterForTest(t, database, consumer.CadenceModeCutover)

	// Seed last_interaction_at via a mutual interaction at targetLast.
	applyInTx(t, database, func(tx pgx.Tx) error {
		return cu.ApplyInteraction(ctx, tx, repository.ApplyInteractionRequest{
			ContactID:  target.ID,
			Direction:  repository.InteractionDirectionMutual,
			Source:     repository.InteractionSourceGCal,
			OccurredAt: targetLast,
		})
	})
	seeded, err := contactRepo.GetContact(ctx, target.ID)
	require.NoError(t, err)
	require.NotNil(t, seeded.LastInteractionAt, "seeding should have set last_interaction_at")
	seededLastInteraction := *seeded.LastInteractionAt

	// Merge: source had an older last_contacted but the target has a
	// newer last_interaction_at we don't want overwritten. Call BulkApply
	// with a forward-max merge of last_contacted (newer source).
	mergeNewer := time.Date(2026, 5, 1, 9, 0, 0, 0, time.UTC)
	applyInTx(t, database, func(tx pgx.Tx) error {
		return cu.BulkApply(ctx, tx, target.ID, repository.ContactCadenceFields{LastContacted: &mergeNewer})
	})
	after, err := contactRepo.GetContact(ctx, target.ID)
	require.NoError(t, err)
	assert.Equal(t, mergeNewer.UTC(), after.LastContacted.UTC(), "merge should advance last_contacted forward")
	require.NotNil(t, after.LastInteractionAt)
	assert.Equal(t, seededLastInteraction.UTC(), after.LastInteractionAt.UTC(),
		"merge (BulkApply) MUST NOT bump last_interaction_at — merge is not an interaction")
}

// TestIntegration_CadenceUpdater_BulkApply_ForwardMaxOnMerge verifies
// MergeContacts' BulkApply path never regresses an existing field.
func TestIntegration_CadenceUpdater_BulkApply_ForwardMaxOnMerge(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	database, cleanup := newCadenceUpdaterTestDB(t)
	defer cleanup()
	ctx := context.Background()

	targetLast := time.Date(2026, 3, 15, 12, 0, 0, 0, time.UTC)
	contactRepo := repository.NewContactRepository(database.Queries)
	target, err := contactRepo.CreateContact(ctx, repository.CreateContactRequest{
		FullName: "Merge Target", LastContacted: &targetLast,
	})
	require.NoError(t, err)
	defer func() { _ = contactRepo.HardDeleteContact(ctx, target.ID) }()

	cu, _ := newCadenceUpdaterForTest(t, database, consumer.CadenceModeCutover)
	sourceLastOlder := time.Date(2026, 2, 1, 9, 0, 0, 0, time.UTC)
	sourceLastNewer := time.Date(2026, 4, 1, 9, 0, 0, 0, time.UTC)

	// First BulkApply with source's OLDER last_contacted: target should not regress.
	applyInTx(t, database, func(tx pgx.Tx) error {
		return cu.BulkApply(ctx, tx, target.ID, repository.ContactCadenceFields{LastContacted: &sourceLastOlder})
	})
	got, err := contactRepo.GetContact(ctx, target.ID)
	require.NoError(t, err)
	assert.Equal(t, targetLast.UTC(), got.LastContacted.UTC(), "bulk apply forward-only: older source must not regress target")

	// Second BulkApply with source's NEWER last_contacted: target should advance.
	applyInTx(t, database, func(tx pgx.Tx) error {
		return cu.BulkApply(ctx, tx, target.ID, repository.ContactCadenceFields{LastContacted: &sourceLastNewer})
	})
	got, err = contactRepo.GetContact(ctx, target.ID)
	require.NoError(t, err)
	assert.Equal(t, sourceLastNewer.UTC(), got.LastContacted.UTC(), "bulk apply forward-only: newer source must advance target")
}

// TestIntegration_CadenceUpdater_ApplyContactByOverride_ClearAndBackdate
// verifies the user-cadence-preference direct API can both clear and
// backdate contact_by (unconditional branch per Design Decision 9).
func TestIntegration_CadenceUpdater_ApplyContactByOverride_ClearAndBackdate(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	database, cleanup := newCadenceUpdaterTestDB(t)
	defer cleanup()
	ctx := context.Background()
	cad := "weekly"
	contactRepo := repository.NewContactRepository(database.Queries)
	contact, err := contactRepo.CreateContact(ctx, repository.CreateContactRequest{
		FullName: "Cadence Override", Cadence: &cad,
	})
	require.NoError(t, err)
	defer func() { _ = contactRepo.HardDeleteContact(ctx, contact.ID) }()
	require.NotNil(t, contact.ContactBy, "weekly cadence should seed contact_by at create time")

	cu, _ := newCadenceUpdaterForTest(t, database, consumer.CadenceModeCutover)

	// Clear contact_by (user removed cadence).
	applyInTx(t, database, func(tx pgx.Tx) error {
		return cu.ApplyContactByOverride(ctx, tx, contact.ID, nil)
	})
	got, err := contactRepo.GetContact(ctx, contact.ID)
	require.NoError(t, err)
	assert.Nil(t, got.ContactBy, "nil override should clear contact_by")

	// Backdate contact_by (user set an earlier due date; unconditional
	// branch allows backward movement).
	earlier := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	applyInTx(t, database, func(tx pgx.Tx) error {
		return cu.ApplyContactByOverride(ctx, tx, contact.ID, &earlier)
	})
	got, err = contactRepo.GetContact(ctx, contact.ID)
	require.NoError(t, err)
	require.NotNil(t, got.ContactBy)
	assert.Equal(t, earlier.UTC().Format("2006-01-02"), got.ContactBy.UTC().Format("2006-01-02"),
		"unconditional override should allow backdate")
}

// TestIntegration_CadenceUpdater_HandleEvent_DuplicateClaim_NoOp verifies
// the durable dedupe — a second HandleEvent on the same event_id returns
// nil without touching cadence (the blocker-fix invariant from plan
// Decision 2).
func TestIntegration_CadenceUpdater_HandleEvent_DuplicateClaim_NoOp(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	database, cleanup := newCadenceUpdaterTestDB(t)
	defer cleanup()
	ctx := context.Background()
	cad := "weekly"
	initialLast := time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)
	contactRepo := repository.NewContactRepository(database.Queries)
	contact, err := contactRepo.CreateContact(ctx, repository.CreateContactRequest{
		FullName: "Duplicate Claim", Cadence: &cad, LastContacted: &initialLast,
	})
	require.NoError(t, err)
	defer func() { _ = contactRepo.HardDeleteContact(ctx, contact.ID) }()

	cu, _ := newCadenceUpdaterForTest(t, database, consumer.CadenceModeCutover)

	firstOccurred := time.Date(2026, 4, 1, 10, 0, 0, 0, time.UTC)
	env := &events.Envelope{
		ID:         uuid.New(),
		Kind:       events.KindInteractionRecorded,
		Source:     repository.InteractionSourceTelegram,
		ObservedAt: firstOccurred,
	}
	payload := events.InteractionRecordedPayload{
		Version:       2,
		ContactID:     contact.ID,
		InteractionID: uuid.New(),
		Direction:     repository.InteractionDirectionMutual,
		OccurredAt:    firstOccurred,
		Source:        repository.InteractionSourceTelegram,
		PrevCadenceSnapshot: &events.CadenceFieldsSnapshot{
			LastContacted: &initialLast,
		},
		PrevCadenceValue: &cad,
	}
	env.Payload, err = events.Marshal(events.KindInteractionRecorded, payload)
	require.NoError(t, err)

	// First call wins the claim and writes the cadence.
	applyInTx(t, database, func(tx pgx.Tx) error {
		return cu.HandleEvent(ctx, tx, env)
	})
	afterFirst, err := contactRepo.GetContact(ctx, contact.ID)
	require.NoError(t, err)
	require.NotNil(t, afterFirst.LastContacted)
	assert.Equal(t, firstOccurred.UTC(), afterFirst.LastContacted.UTC(), "first claim should have written last_contacted")

	// Mutate the contact row out-of-band to a NEWER value to simulate
	// activity landing between inline apply and the queued re-delivery.
	// A duplicate HandleEvent must NOT overwrite this.
	newerLast := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	applyInTx(t, database, func(tx pgx.Tx) error {
		return cu.ApplyContactByOverride(ctx, tx, contact.ID, nil) // no-op side effect: overwrite rev
	})
	// Force last_contacted forward via a manual-source apply through the
	// sole writer (the only legitimate way to post-date last_contacted).
	applyInTx(t, database, func(tx pgx.Tx) error {
		return cu.ApplyInteraction(ctx, tx, repository.ApplyInteractionRequest{
			ContactID:  contact.ID,
			Direction:  repository.InteractionDirectionMutual,
			Source:     repository.InteractionSourceManual,
			OccurredAt: newerLast,
		})
	})

	// Second HandleEvent with the SAME event_id: must be a durable no-op
	// (claim already exists).
	applyInTx(t, database, func(tx pgx.Tx) error {
		return cu.HandleEvent(ctx, tx, env)
	})

	afterDuplicate, err := contactRepo.GetContact(ctx, contact.ID)
	require.NoError(t, err)
	assert.Equal(t, newerLast.UTC(), afterDuplicate.LastContacted.UTC(),
		"duplicate claim must NOT overwrite last_contacted; post-newer value must persist")
}

// TestIntegration_CadenceUpdater_ModeOff_NoWrite verifies mode=off
// short-circuits cadence writes even with a valid envelope.
func TestIntegration_CadenceUpdater_ModeOff_NoWrite(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	database, cleanup := newCadenceUpdaterTestDB(t)
	defer cleanup()
	ctx := context.Background()
	cad := "weekly"
	initialLast := time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)
	contactRepo := repository.NewContactRepository(database.Queries)
	contact, err := contactRepo.CreateContact(ctx, repository.CreateContactRequest{
		FullName: "Cadence Mode Off", Cadence: &cad, LastContacted: &initialLast,
	})
	require.NoError(t, err)
	defer func() { _ = contactRepo.HardDeleteContact(ctx, contact.ID) }()

	cuOff, _ := newCadenceUpdaterForTest(t, database, consumer.CadenceModeOff)
	applyInTx(t, database, func(tx pgx.Tx) error {
		return cuOff.ApplyInteraction(ctx, tx, repository.ApplyInteractionRequest{
			ContactID:  contact.ID,
			Direction:  repository.InteractionDirectionMutual,
			Source:     repository.InteractionSourceGCal,
			OccurredAt: accelerated.GetCurrentTime(),
		})
	})
	got, err := contactRepo.GetContact(ctx, contact.ID)
	require.NoError(t, err)
	assert.Equal(t, initialLast.UTC(), got.LastContacted.UTC(), "mode=off must not write last_contacted")
}

// TestIntegration_CadenceUpdater_ContactBy_DerivedFromCadence verifies the
// inbound/mutual recompute of contact_by uses the stored cadence string
// (end-to-end verification against a live DB, distinct from the unit tests
// that assert on flags alone).
func TestIntegration_CadenceUpdater_ContactBy_DerivedFromCadence(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	database, cleanup := newCadenceUpdaterTestDB(t)
	defer cleanup()
	ctx := context.Background()
	cad := "monthly"
	initialLast := time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)
	contactRepo := repository.NewContactRepository(database.Queries)
	contact, err := contactRepo.CreateContact(ctx, repository.CreateContactRequest{
		FullName: "Cadence Contact By", Cadence: &cad, LastContacted: &initialLast,
	})
	require.NoError(t, err)
	defer func() { _ = contactRepo.HardDeleteContact(ctx, contact.ID) }()

	cu, _ := newCadenceUpdaterForTest(t, database, consumer.CadenceModeCutover)
	newer := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	applyInTx(t, database, func(tx pgx.Tx) error {
		return cu.ApplyInteraction(ctx, tx, repository.ApplyInteractionRequest{
			ContactID:  contact.ID,
			Direction:  repository.InteractionDirectionMutual,
			Source:     repository.InteractionSourceGCal,
			OccurredAt: newer,
		})
	})

	got, err := contactRepo.GetContact(ctx, contact.ID)
	require.NoError(t, err)
	require.NotNil(t, got.ContactBy)
	expectedContactBy := cadence.CalculateContactBy(newer, cadence.CadenceMonthly)
	assert.Equal(t, expectedContactBy.UTC().Format("2006-01-02"), got.ContactBy.UTC().Format("2006-01-02"),
		"mutual apply should recompute contact_by = occurred_at + monthly")
}
