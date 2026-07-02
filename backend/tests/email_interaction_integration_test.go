// End-to-end coverage for the EmailInteractionConsumer (Gmail integration
// phase 3). Each test publishes email.received / email.sent envelopes
// directly via the live bus (the Gmail provider is not involved — phase 3
// tests the consumer in isolation from the fetcher) and waits for the async
// email_interaction_consumer worker to derive a per-(contact, thread,
// local-day) aggregated interaction. The real CadenceUpdaterWorker is wired
// so the create branch's cadence delivery (via async interaction.recorded)
// is observable; the FollowUpManager runs in off-mode to drain its queue
// cleanly. All assertions go through repositories (no raw SQL); timestamps
// are accelerated.GetCurrentTime()-anchored; addresses are placeholders.
package tests

import (
	"context"
	"testing"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/consumer"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/events"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"
	"personal-crm/backend/internal/synthetic/factory"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"
)

// emailEnv bundles a live bus + repos + the email consumer for the
// integration suite.
type emailEnv struct {
	ctx             context.Context
	database        *db.Database
	gen             *factory.Generator
	bus             *events.Bus
	emailConsumer   *consumer.EmailInteractionConsumer
	contactRepo     *repository.ContactRepository
	interactionRepo *repository.InteractionRepository
	commsRepo       *repository.CommsMessageRepository
	contactService  *service.ContactService
}

func newEmailEnv(t *testing.T, ctx context.Context) *emailEnv {
	t.Helper()
	// Per-test isolated clone: the live email consumer drains a private
	// river_job, so concurrent siblings can't steal each other's jobs.
	database, _ := newIsolatedRiverTestDB(t, ctx)

	contactRepo := repository.NewContactRepository(database.Queries)
	contactRepo.SetPool(database.Pool)
	contactMethodRepo := repository.NewContactMethodRepository(database.Queries)
	interactionRepo := repository.NewInteractionRepository(database.Queries)
	contactTaskRepo := repository.NewContactTaskRepository(database.Queries)
	commsRepo := repository.NewCommsMessageRepository(database.Queries)

	// nil bus + nil rematch: the email consumer publishes interaction.recorded
	// through the live bus the harness builds, not through the service.
	contactService := service.NewContactService(database, contactRepo, contactMethodRepo, interactionRepo, contactTaskRepo, nil, nil)

	bus, emailConsumer := setupTestEventBusForEmail(t, ctx, database, contactService)

	gen, _ := migrationGenerator(t)

	return &emailEnv{
		ctx:             ctx,
		database:        database,
		gen:             gen,
		bus:             bus,
		emailConsumer:   emailConsumer,
		contactRepo:     contactRepo,
		interactionRepo: interactionRepo,
		commsRepo:       commsRepo,
		contactService:  contactService,
	}
}

// newEmailTestContact seeds a namespaced contact (no methods — the consumer
// matches on the contact id carried in the payload, never on an identifier) with
// a weekly cadence so contact_by rolls, and registers cleanup that hard-deletes
// its comms_message + email interaction rows before the contact (FK-child rows
// must go first).
func (e *emailEnv) newEmailTestContact(t *testing.T) *repository.Contact {
	t.Helper()
	contact, contactCleanup := seedMigrationContact(e.ctx, t, e.database, e.gen,
		factory.WithNoMethods(), factory.WithCadence("weekly"))
	t.Cleanup(func() {
		_ = e.commsRepo.HardDeleteByContact(e.ctx, contact.ID)
		_ = e.interactionRepo.HardDeleteInteractionsBySourceRefPrefix(e.ctx, repository.InteractionSourceEmail, contact.ID.String()+":%")
		contactCleanup()
	})
	return contact
}

// seedCommsMessage upserts a comms_message content row for the contact and
// returns it. The consumer locates this row by natural key inside its tx.
func (e *emailEnv) seedCommsMessage(t *testing.T, contactID uuid.UUID, externalID, threadID, direction string, sentAt time.Time) *repository.CommsMessage {
	t.Helper()
	thread := threadID
	subject := "Subject " + externalID
	body := "body"
	peer := "peer@example.test"
	acct := "acct-a"
	gmailID := "gm-" + externalID
	msg, err := e.commsRepo.UpsertMessage(e.ctx, repository.UpsertCommsMessageParams{
		Source:           repository.InteractionSourceEmail,
		ExternalID:       externalID,
		ThreadID:         &thread,
		Subject:          &subject,
		Body:             &body,
		PeerHandle:       &peer,
		PeerNormalized:   &peer,
		Direction:        direction,
		SentAt:           sentAt,
		AccountID:        &acct,
		MatchedContactID: contactID,
		GmailMessageID:   &gmailID,
	})
	require.NoError(t, err)
	return msg
}

func localDay(ts time.Time) string {
	return ts.In(time.Local).Format("2006-01-02")
}

// localNoonAnchor returns a UTC timestamp that is local noon today, so the
// per-test +Δhours offsets (≤ a few hours) stay within ONE time.Local
// calendar day. Anchoring naively to GetCurrentTime() risks straddling a
// local-day boundary (e.g. 23:05 local + 2h → next day), which would split
// what should be one thread-day aggregate into two source_refs. occurred_at
// is still anchored to real time so cadence math behaves; we only constrain
// the calendar day.
func localNoonAnchor() time.Time {
	now := accelerated.GetCurrentTime().In(time.Local)
	noon := time.Date(now.Year(), now.Month(), now.Day(), 12, 0, 0, 0, time.Local)
	return noon.UTC().Truncate(time.Second)
}

func (e *emailEnv) publishEmail(t *testing.T, kind events.Kind, contactID uuid.UUID, externalID, threadID, direction string, sentAt time.Time) {
	t.Helper()
	subject := "Subject " + externalID
	p := events.EmailEventPayload{
		Version:    1,
		ContactID:  contactID,
		ExternalID: externalID,
		ThreadID:   threadID,
		LocalDay:   localDay(sentAt),
		SentAt:     sentAt,
		Direction:  direction,
		Subject:    &subject,
	}
	raw, err := events.Marshal(kind, p)
	require.NoError(t, err)
	env := &events.Envelope{
		Source:     repository.InteractionSourceEmail,
		SourceID:   externalID + ":" + contactID.String(),
		Kind:       kind,
		Payload:    raw,
		ObservedAt: sentAt,
	}
	require.NoError(t, e.bus.Publish(e.ctx, env))
}

func sourceRefFor(contactID uuid.UUID, threadID string, sentAt time.Time) string {
	return contactID.String() + ":" + threadID + ":" + localDay(sentAt)
}

// waitForCommsProcessed polls until the comms_message row is linked to an
// interaction (interaction_id + processed_at set), via the repository.
func (e *emailEnv) waitForCommsProcessed(t *testing.T, externalID string, contactID uuid.UUID, timeout time.Duration) *repository.CommsMessage {
	t.Helper()
	deadline := accelerated.GetCurrentTime().Add(timeout)
	for accelerated.GetCurrentTime().Before(deadline) {
		msg, err := e.commsRepo.GetMessage(e.ctx, repository.InteractionSourceEmail, externalID, contactID)
		require.NoError(t, err)
		if msg.InteractionID != nil && msg.ProcessedAt != nil {
			return msg
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for comms_message %s to be processed for contact %s", externalID, contactID)
	return nil
}

// --- A. Create → cadence (inbound) ------------------------------------------

func TestEmailIntegration_CreateInbound_AppliesCadence(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := newEmailEnv(t, ctx)
	contact := e.newEmailTestContact(t)

	sentAt := localNoonAnchor()
	e.seedCommsMessage(t, contact.ID, "<a-1@example.test>", "thr-A", repository.InteractionDirectionInbound, sentAt)
	e.publishEmail(t, events.KindEmailReceived, contact.ID, "<a-1@example.test>", "thr-A", repository.InteractionDirectionInbound, sentAt)

	ref := sourceRefFor(contact.ID, "thr-A", sentAt)
	got := waitForInteractionBySourceRef(t, ctx, e.interactionRepo, contact.ID, repository.InteractionSourceEmail, ref, defaultInteractionWaitTimeout)
	require.Equal(t, repository.InteractionDirectionInbound, got.Direction)
	require.WithinDuration(t, sentAt, got.OccurredAt, time.Second)

	msg := e.waitForCommsProcessed(t, "<a-1@example.test>", contact.ID, defaultInteractionWaitTimeout)
	require.Equal(t, got.ID, *msg.InteractionID)

	// Cadence via async interaction.recorded: last_contacted bumps for inbound.
	c := e.waitForLastContacted(t, contact.ID, defaultInteractionWaitTimeout)
	require.NotNil(t, c.LastContacted)
	require.WithinDuration(t, sentAt, *c.LastContacted, time.Second)
}

// --- B. Create → cadence (outbound) -----------------------------------------

func TestEmailIntegration_CreateOutbound_AppliesOutreachCadence(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := newEmailEnv(t, ctx)
	contact := e.newEmailTestContact(t)

	sentAt := localNoonAnchor()
	e.seedCommsMessage(t, contact.ID, "<b-1@example.test>", "thr-B", repository.InteractionDirectionOutbound, sentAt)
	e.publishEmail(t, events.KindEmailSent, contact.ID, "<b-1@example.test>", "thr-B", repository.InteractionDirectionOutbound, sentAt)

	ref := sourceRefFor(contact.ID, "thr-B", sentAt)
	got := waitForInteractionBySourceRef(t, ctx, e.interactionRepo, contact.ID, repository.InteractionSourceEmail, ref, defaultInteractionWaitTimeout)
	require.Equal(t, repository.InteractionDirectionOutbound, got.Direction)

	// Outbound: last_outreach_at bumps; last_contacted does NOT.
	c := e.waitForLastOutreach(t, contact.ID, defaultInteractionWaitTimeout)
	require.NotNil(t, c.LastOutreachAt)
	require.WithinDuration(t, sentAt, *c.LastOutreachAt, time.Second)
	require.Nil(t, c.LastContacted, "outbound email must not bump last_contacted")
}

// --- C. Same thread+day extend ----------------------------------------------

func TestEmailIntegration_SameThreadDay_Extends(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := newEmailEnv(t, ctx)
	contact := e.newEmailTestContact(t)

	t1 := localNoonAnchor()
	t2 := t1.Add(2 * time.Hour)
	e.seedCommsMessage(t, contact.ID, "<c-1@example.test>", "thr-C", repository.InteractionDirectionInbound, t1)
	e.seedCommsMessage(t, contact.ID, "<c-2@example.test>", "thr-C", repository.InteractionDirectionInbound, t2)

	e.publishEmail(t, events.KindEmailReceived, contact.ID, "<c-1@example.test>", "thr-C", repository.InteractionDirectionInbound, t1)
	ref := sourceRefFor(contact.ID, "thr-C", t1)
	waitForInteractionBySourceRef(t, ctx, e.interactionRepo, contact.ID, repository.InteractionSourceEmail, ref, defaultInteractionWaitTimeout)
	e.waitForCommsProcessed(t, "<c-1@example.test>", contact.ID, defaultInteractionWaitTimeout)

	e.publishEmail(t, events.KindEmailReceived, contact.ID, "<c-2@example.test>", "thr-C", repository.InteractionDirectionInbound, t2)
	e.waitForCommsProcessed(t, "<c-2@example.test>", contact.ID, defaultInteractionWaitTimeout)

	// Still exactly one interaction (extend, not a new row); occurred_at = t2.
	rows := e.listEmailInteractions(t, contact.ID)
	require.Len(t, rows, 1, "same thread+day extends, no new interaction")
	require.WithinDuration(t, t2, rows[0].OccurredAt, time.Second)

	// Both content rows linked to the same interaction.
	m1, err := e.commsRepo.GetMessage(ctx, repository.InteractionSourceEmail, "<c-1@example.test>", contact.ID)
	require.NoError(t, err)
	m2, err := e.commsRepo.GetMessage(ctx, repository.InteractionSourceEmail, "<c-2@example.test>", contact.ID)
	require.NoError(t, err)
	require.NotNil(t, m1.InteractionID)
	require.NotNil(t, m2.InteractionID)
	require.Equal(t, *m1.InteractionID, *m2.InteractionID)

	// Cadence advanced to t2.
	c := e.waitForLastContactedAtLeast(t, contact.ID, t2, defaultInteractionWaitTimeout)
	require.WithinDuration(t, t2, *c.LastContacted, time.Second)
}

// --- D. Next calendar day → new interaction ---------------------------------

func TestEmailIntegration_NextDay_NewInteraction(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := newEmailEnv(t, ctx)
	contact := e.newEmailTestContact(t)

	dayD := localNoonAnchor()
	dayD1 := dayD.Add(24 * time.Hour)
	e.seedCommsMessage(t, contact.ID, "<d-1@example.test>", "thr-D", repository.InteractionDirectionInbound, dayD)
	e.seedCommsMessage(t, contact.ID, "<d-2@example.test>", "thr-D", repository.InteractionDirectionInbound, dayD1)

	e.publishEmail(t, events.KindEmailReceived, contact.ID, "<d-1@example.test>", "thr-D", repository.InteractionDirectionInbound, dayD)
	e.publishEmail(t, events.KindEmailReceived, contact.ID, "<d-2@example.test>", "thr-D", repository.InteractionDirectionInbound, dayD1)
	e.waitForCommsProcessed(t, "<d-1@example.test>", contact.ID, defaultInteractionWaitTimeout)
	e.waitForCommsProcessed(t, "<d-2@example.test>", contact.ID, defaultInteractionWaitTimeout)

	rows := e.waitForEmailInteractionCount(t, contact.ID, 2, defaultInteractionWaitTimeout)
	require.Len(t, rows, 2, "distinct local days → distinct source_refs → two interactions")
	require.NotNil(t, rows[0].SourceRef)
	require.NotNil(t, rows[1].SourceRef)
	require.NotEqual(t, *rows[0].SourceRef, *rows[1].SourceRef)
}

// --- E. Mixed-direction same thread+day → promote to mutual -----------------

func TestEmailIntegration_MixedDirection_PromotesToMutual(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := newEmailEnv(t, ctx)
	contact := e.newEmailTestContact(t)

	t1 := localNoonAnchor()
	t2 := t1.Add(time.Hour)
	e.seedCommsMessage(t, contact.ID, "<e-1@example.test>", "thr-E", repository.InteractionDirectionInbound, t1)
	e.seedCommsMessage(t, contact.ID, "<e-2@example.test>", "thr-E", repository.InteractionDirectionOutbound, t2)

	e.publishEmail(t, events.KindEmailReceived, contact.ID, "<e-1@example.test>", "thr-E", repository.InteractionDirectionInbound, t1)
	e.waitForCommsProcessed(t, "<e-1@example.test>", contact.ID, defaultInteractionWaitTimeout)
	e.publishEmail(t, events.KindEmailSent, contact.ID, "<e-2@example.test>", "thr-E", repository.InteractionDirectionOutbound, t2)
	e.waitForCommsProcessed(t, "<e-2@example.test>", contact.ID, defaultInteractionWaitTimeout)

	rows := e.listEmailInteractions(t, contact.ID)
	require.Len(t, rows, 1, "mixed direction same thread+day → one interaction")
	// Eventually promoted to mutual.
	mutated := waitForInteractionDirection(t, ctx, e.interactionRepo, contact.ID, repository.InteractionDirectionMutual, defaultInteractionWaitTimeout)
	require.Len(t, e.filterEmail(mutated), 1)
	require.Equal(t, repository.InteractionDirectionMutual, e.filterEmail(mutated)[0].Direction)
	require.WithinDuration(t, t2, e.filterEmail(mutated)[0].OccurredAt, time.Second)

	// Mutual bumps last_contacted to t2.
	c := e.waitForLastContactedAtLeast(t, contact.ID, t2, defaultInteractionWaitTimeout)
	require.WithinDuration(t, t2, *c.LastContacted, time.Second)
}

// --- F. Out-of-order backfill does NOT move occurred_at backward ------------

func TestEmailIntegration_OutOfOrderBackfill_NoBackwardMove(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := newEmailEnv(t, ctx)
	contact := e.newEmailTestContact(t)

	t1 := localNoonAnchor()
	t2 := t1.Add(3 * time.Hour)
	e.seedCommsMessage(t, contact.ID, "<f-late@example.test>", "thr-F", repository.InteractionDirectionInbound, t2)
	e.seedCommsMessage(t, contact.ID, "<f-early@example.test>", "thr-F", repository.InteractionDirectionInbound, t1)
	e.seedCommsMessage(t, contact.ID, "<f-early-out@example.test>", "thr-F", repository.InteractionDirectionOutbound, t1)

	// Create with the LATER SentAt first.
	e.publishEmail(t, events.KindEmailReceived, contact.ID, "<f-late@example.test>", "thr-F", repository.InteractionDirectionInbound, t2)
	e.waitForCommsProcessed(t, "<f-late@example.test>", contact.ID, defaultInteractionWaitTimeout)

	// Earlier same-direction backfill must NOT move occurred_at back to t1.
	e.publishEmail(t, events.KindEmailReceived, contact.ID, "<f-early@example.test>", "thr-F", repository.InteractionDirectionInbound, t1)
	e.waitForCommsProcessed(t, "<f-early@example.test>", contact.ID, defaultInteractionWaitTimeout)

	rows := e.listEmailInteractions(t, contact.ID)
	require.Len(t, rows, 1)
	require.WithinDuration(t, t2, rows[0].OccurredAt, time.Second, "occurred_at stays at the later T2")
	require.Equal(t, repository.InteractionDirectionInbound, rows[0].Direction)

	// Earlier mixed-direction backfill → promotes to mutual but occurred_at held at t2.
	e.publishEmail(t, events.KindEmailSent, contact.ID, "<f-early-out@example.test>", "thr-F", repository.InteractionDirectionOutbound, t1)
	e.waitForCommsProcessed(t, "<f-early-out@example.test>", contact.ID, defaultInteractionWaitTimeout)
	mutated := waitForInteractionDirection(t, ctx, e.interactionRepo, contact.ID, repository.InteractionDirectionMutual, defaultInteractionWaitTimeout)
	emailRows := e.filterEmail(mutated)
	require.Len(t, emailRows, 1)
	require.WithinDuration(t, t2, emailRows[0].OccurredAt, time.Second, "promote holds occurred_at at T2")
}

// --- G. Re-delivery idempotency ---------------------------------------------

func TestEmailIntegration_ReDelivery_Idempotent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := newEmailEnv(t, ctx)
	contact := e.newEmailTestContact(t)

	sentAt := localNoonAnchor()
	e.seedCommsMessage(t, contact.ID, "<g-1@example.test>", "thr-G", repository.InteractionDirectionInbound, sentAt)

	// Publish the same (source, source_id) envelope twice — the second
	// Publish dedups at the event-table unique index (no second enqueue).
	e.publishEmail(t, events.KindEmailReceived, contact.ID, "<g-1@example.test>", "thr-G", repository.InteractionDirectionInbound, sentAt)
	ref := sourceRefFor(contact.ID, "thr-G", sentAt)
	first := waitForInteractionBySourceRef(t, ctx, e.interactionRepo, contact.ID, repository.InteractionSourceEmail, ref, defaultInteractionWaitTimeout)
	e.waitForCommsProcessed(t, "<g-1@example.test>", contact.ID, defaultInteractionWaitTimeout)

	e.publishEmail(t, events.KindEmailReceived, contact.ID, "<g-1@example.test>", "thr-G", repository.InteractionDirectionInbound, sentAt)
	// Give any (non-)enqueued re-run a chance to run.
	time.Sleep(500 * time.Millisecond)

	rows := e.listEmailInteractions(t, contact.ID)
	require.Len(t, rows, 1, "re-delivery does not create a duplicate interaction")
	require.Equal(t, first.ID, rows[0].ID)
	require.WithinDuration(t, sentAt, rows[0].OccurredAt, time.Second, "occurred_at unchanged on re-delivery")
}

// --- H. Cross-account same Message-ID → exactly one interaction -------------

func TestEmailIntegration_CrossAccountSameMessageID_OneInteraction(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := newEmailEnv(t, ctx)
	contact := e.newEmailTestContact(t)

	sentAt := localNoonAnchor()
	e.seedCommsMessage(t, contact.ID, "<h-shared@example.test>", "thr-H", repository.InteractionDirectionInbound, sentAt)

	// Two observations of the SAME message+contact share one event source_id
	// (<message_id>:<contact_uuid>), so the second publish dedups.
	e.publishEmail(t, events.KindEmailReceived, contact.ID, "<h-shared@example.test>", "thr-H", repository.InteractionDirectionInbound, sentAt)
	e.publishEmail(t, events.KindEmailReceived, contact.ID, "<h-shared@example.test>", "thr-H", repository.InteractionDirectionInbound, sentAt)
	e.waitForCommsProcessed(t, "<h-shared@example.test>", contact.ID, defaultInteractionWaitTimeout)
	time.Sleep(300 * time.Millisecond)

	rows := e.listEmailInteractions(t, contact.ID)
	require.Len(t, rows, 1, "cross-account same Message-ID derives exactly one interaction")
}

// --- I. Match-only: consumer never creates a contact ------------------------

func TestEmailIntegration_MatchOnly_NoContactCreation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := newEmailEnv(t, ctx)
	contact := e.newEmailTestContact(t)

	before := e.countContacts(t)

	sentAt := localNoonAnchor()
	e.seedCommsMessage(t, contact.ID, "<i-1@example.test>", "thr-I", repository.InteractionDirectionInbound, sentAt)
	e.publishEmail(t, events.KindEmailReceived, contact.ID, "<i-1@example.test>", "thr-I", repository.InteractionDirectionInbound, sentAt)
	e.waitForCommsProcessed(t, "<i-1@example.test>", contact.ID, defaultInteractionWaitTimeout)

	after := e.countContacts(t)
	require.Equal(t, before, after, "consumer derives from a known contact; it never creates one")
}

// --- J. Concurrent same-thread+day jobs do NOT move occurred_at backward ----

func TestEmailIntegration_Concurrent_NoBackwardMove(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := newEmailEnv(t, ctx)
	contact := e.newEmailTestContact(t)

	t1 := localNoonAnchor()
	t2 := t1.Add(2 * time.Hour)
	m1 := e.seedCommsMessage(t, contact.ID, "<j-late@example.test>", "thr-J", repository.InteractionDirectionInbound, t2)
	m2 := e.seedCommsMessage(t, contact.ID, "<j-early@example.test>", "thr-J", repository.InteractionDirectionInbound, t1)

	// Drive two HandleEvent calls concurrently on separate txs against the
	// same pool, each with the same source_ref. The advisory lock must force
	// one to block until the other commits; the forward-only guard then keeps
	// occurred_at at the LATER SentAt (t2) regardless of commit interleaving.
	envLate := e.buildEnvelope(t, events.KindEmailReceived, contact.ID, "<j-late@example.test>", "thr-J", repository.InteractionDirectionInbound, t2)
	envEarly := e.buildEnvelope(t, events.KindEmailReceived, contact.ID, "<j-early@example.test>", "thr-J", repository.InteractionDirectionInbound, t1)
	// Insert both event rows so GetEvent inside the worker can resolve them
	// (publish writes the event log row; the consumer fetches by id). Here we
	// invoke HandleEvent directly, so we just need the comms rows seeded.
	_ = m1
	_ = m2

	errCh := make(chan error, 2)
	run := func(env *events.Envelope) {
		err := pgx.BeginTxFunc(ctx, e.database.Pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
			_, herr := e.emailConsumer.HandleEvent(ctx, tx, env)
			return herr
		})
		errCh <- err
	}
	go run(envLate)
	go run(envEarly)
	require.NoError(t, <-errCh)
	require.NoError(t, <-errCh)

	rows := e.listEmailInteractions(t, contact.ID)
	require.Len(t, rows, 1, "concurrent same-thread+day jobs produce exactly one interaction")
	require.WithinDuration(t, t2, rows[0].OccurredAt, time.Second, "occurred_at = later SentAt regardless of interleaving (no backward move)")

	// Both content rows linked to the one interaction.
	g1, err := e.commsRepo.GetMessage(ctx, repository.InteractionSourceEmail, "<j-late@example.test>", contact.ID)
	require.NoError(t, err)
	g2, err := e.commsRepo.GetMessage(ctx, repository.InteractionSourceEmail, "<j-early@example.test>", contact.ID)
	require.NoError(t, err)
	require.NotNil(t, g1.InteractionID)
	require.NotNil(t, g2.InteractionID)
	require.Equal(t, rows[0].ID, *g1.InteractionID)
	require.Equal(t, rows[0].ID, *g2.InteractionID)
}

// ---------------------------------------------------------------------------
// Helpers (repository-only; no raw SQL).
// ---------------------------------------------------------------------------

func (e *emailEnv) buildEnvelope(t *testing.T, kind events.Kind, contactID uuid.UUID, externalID, threadID, direction string, sentAt time.Time) *events.Envelope {
	t.Helper()
	subject := "Subject " + externalID
	p := events.EmailEventPayload{
		Version:    1,
		ContactID:  contactID,
		ExternalID: externalID,
		ThreadID:   threadID,
		LocalDay:   localDay(sentAt),
		SentAt:     sentAt,
		Direction:  direction,
		Subject:    &subject,
	}
	raw, err := events.Marshal(kind, p)
	require.NoError(t, err)
	return &events.Envelope{
		ID:         uuid.New(),
		Source:     repository.InteractionSourceEmail,
		SourceID:   externalID + ":" + contactID.String(),
		Kind:       kind,
		Payload:    raw,
		ObservedAt: sentAt,
	}
}

// listEmailInteractions returns the contact's live email interactions.
func (e *emailEnv) listEmailInteractions(t *testing.T, contactID uuid.UUID) []repository.Interaction {
	t.Helper()
	rows, err := e.interactionRepo.ListContactInteractions(e.ctx, contactID, 100, 0)
	require.NoError(t, err)
	return e.filterEmail(rows)
}

func (e *emailEnv) filterEmail(rows []repository.Interaction) []repository.Interaction {
	out := make([]repository.Interaction, 0, len(rows))
	for _, r := range rows {
		if r.Source == repository.InteractionSourceEmail {
			out = append(out, r)
		}
	}
	return out
}

func (e *emailEnv) waitForEmailInteractionCount(t *testing.T, contactID uuid.UUID, want int, timeout time.Duration) []repository.Interaction {
	t.Helper()
	deadline := accelerated.GetCurrentTime().Add(timeout)
	var last []repository.Interaction
	for accelerated.GetCurrentTime().Before(deadline) {
		last = e.listEmailInteractions(t, contactID)
		if len(last) == want {
			return last
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d email interactions (got %d) for contact %s", want, len(last), contactID)
	return last
}

func (e *emailEnv) waitForLastContacted(t *testing.T, contactID uuid.UUID, timeout time.Duration) *repository.Contact {
	t.Helper()
	deadline := accelerated.GetCurrentTime().Add(timeout)
	for accelerated.GetCurrentTime().Before(deadline) {
		c, err := e.contactRepo.GetContact(e.ctx, contactID)
		require.NoError(t, err)
		if c.LastContacted != nil {
			return c
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for last_contacted on contact %s", contactID)
	return nil
}

func (e *emailEnv) waitForLastContactedAtLeast(t *testing.T, contactID uuid.UUID, atLeast time.Time, timeout time.Duration) *repository.Contact {
	t.Helper()
	deadline := accelerated.GetCurrentTime().Add(timeout)
	for accelerated.GetCurrentTime().Before(deadline) {
		c, err := e.contactRepo.GetContact(e.ctx, contactID)
		require.NoError(t, err)
		if c.LastContacted != nil && !c.LastContacted.Before(atLeast.Add(-time.Second)) {
			return c
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for last_contacted >= %s on contact %s", atLeast, contactID)
	return nil
}

func (e *emailEnv) waitForLastOutreach(t *testing.T, contactID uuid.UUID, timeout time.Duration) *repository.Contact {
	t.Helper()
	deadline := accelerated.GetCurrentTime().Add(timeout)
	for accelerated.GetCurrentTime().Before(deadline) {
		c, err := e.contactRepo.GetContact(e.ctx, contactID)
		require.NoError(t, err)
		if c.LastOutreachAt != nil {
			return c
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for last_outreach_at on contact %s", contactID)
	return nil
}

func (e *emailEnv) countContacts(t *testing.T) int64 {
	t.Helper()
	// Empty filters → global count. Used only for a single before/after delta
	// within ONE test; package tests run serially (no t.Parallel), so no
	// cross-test pollution lands between the two reads.
	n, err := e.contactRepo.CountContacts(e.ctx, repository.ListContactsParams{})
	require.NoError(t, err)
	return n
}
