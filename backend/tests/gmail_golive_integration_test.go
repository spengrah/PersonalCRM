// Go-live acceptance for the Gmail integration (phase 5, spec §8 item 5). This
// is the end-to-end proof that flipping the feature on actually works: a
// GmailSyncProvider registered in a real ProviderRegistry, an enabled
// (email, account) external_sync_state created exactly as the boot
// reconciliation creates it, and the sweep driven through the
// SyncService.RunAccountSync worker entry point (NOT a direct provider.Sync
// call) so the registered-provider + state-fetch + cursor-write path is
// exercised. The REAL EmailInteractionConsumer + CadenceUpdater are wired (via
// the shared email harness) so derived interactions + cadence are asserted, not
// just published events.
//
// It proves: provider registered → scheduler-driven sweep → content rows +
// interactions + cadence (last_contacted for inbound, last_outreach_at for
// outbound), no contact created for an unknown participant, and the cursor
// advanced so a second sweep is incremental.
//
// The tick-enumeration / ListDueAccounts → enqueue chain is covered by the
// scheduler's own tests; re-driving it here would be redundant. The fetcher +
// me-set are injected fakes (no OAuth); addresses are placeholders (NO PII).
package tests

import (
	"context"
	"os"
	"testing"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/google"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"
	syncpkg "personal-crm/backend/internal/sync"
	"personal-crm/backend/internal/synthetic/factory"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	gmailapi "google.golang.org/api/gmail/v1"
)

// TestGmailGoLive_RegisteredProvider_SchedulerSweep_ProducesInteractionsAndCadence
// is the phase-5 go-live acceptance test.
func TestGmailGoLive_RegisteredProvider_SchedulerSweep_ProducesInteractionsAndCadence(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}
	ctx := context.Background()
	database, _ := newEventBusTestDB(t, ctx)

	commsRepo := repository.NewCommsMessageRepository(database.Queries)
	contactRepo := repository.NewContactRepository(database.Queries)
	contactRepo.SetPool(database.Pool)
	methodRepo := repository.NewContactMethodRepository(database.Queries)
	interactionRepo := repository.NewInteractionRepository(database.Queries)
	eventRepo := repository.NewEventRepository(database.Queries)
	syncRepo := repository.NewSyncRepository(database.Queries)
	identityRepo := repository.NewIdentityRepository(database.Queries)
	contactTaskRepo := repository.NewContactTaskRepository(database.Queries)

	// nil bus + nil rematch on this ContactService: the email consumer
	// publishes interaction.recorded through the live bus the harness builds.
	contactService := service.NewContactService(database, contactRepo, methodRepo, interactionRepo, contactTaskRepo, nil, nil)

	// Live bus with the REAL EmailInteractionConsumer + CadenceUpdater wired
	// (FollowUpManager in off-mode to drain) — derived interactions + cadence
	// are observable.
	bus, _ := setupTestEventBusForEmail(t, ctx, database, contactService)

	gen, _ := migrationGenerator(t)
	prefix := gen.Prefix()
	account := prefix + "me@synthetic.example"

	// Build + register the Gmail provider in a real registry, exactly as
	// main.go does (cutover). Inject a fake fetcher + me-set so no OAuth is
	// touched. nil oauthService is safe: the fetcher + me-set seams are
	// overridden, so the provider never dereferences it.
	provider := google.NewGmailSyncProvider(nil, commsRepo, bus, database.Pool)

	// Known contact + email method. The provider loads its own known-contact
	// map from contact_method rows via ListEmailIdentitiesForSync. Seeded via the
	// nil-bus ContactService (single-tx contact+method write, no River client).
	spec := gen.Contact(factory.WithEmail(), factory.WithCadence("weekly"))
	contact, _, err := contactService.CreateContact(ctx, repository.CreateContactRequest{
		FullName: spec.FullName,
		Cadence:  spec.Cadence,
	}, []service.ContactMethodInput{{Type: "email", Value: spec.Email, IsPrimary: true}})
	require.NoError(t, err)
	addr := spec.Email

	// Unknown participant the sweep must NOT turn into a contact.
	unknown := prefix + "stranger@synthetic.example"

	// External IDs are stored UNBRACKETED (the provider strips the RFC822
	// Message-ID's surrounding <>), so query/assert with the unbracketed form
	// and pass the bracketed form into the message header.
	inExt := prefix + "golive-in@synthetic.example"
	outExt := prefix + "golive-out@synthetic.example"
	unkExt := prefix + "golive-unk@synthetic.example"

	t.Cleanup(func() {
		_ = commsRepo.HardDeleteByContact(ctx, contact.ID)
		_ = interactionRepo.HardDeleteInteractionsBySourceRefPrefix(ctx, repository.InteractionSourceEmail, contact.ID.String()+":%")
		_ = eventRepo.HardDeleteEventsBySourceAndSourceIDPrefix(ctx, "email", prefix+"golive-")
		_ = syncRepo.DeleteSyncStatesByAccountID(ctx, account)
		_ = contactRepo.HardDeleteContact(ctx, contact.ID)
	})

	sentAt := gmailPastNoonAnchor()
	inbound := gmailMsg("glv-in", "thr-in", addr, []string{account}, nil, nil, "In", "hi", "<"+inExt+">", sentAt.UnixMilli())
	outbound := gmailMsg("glv-out", "thr-out", account, []string{addr}, nil, nil, "Out", "yo", "<"+outExt+">", sentAt.Add(time.Hour).UnixMilli())
	// Message between "me" and an unknown address only — a bystander to the
	// known contact, must not create a contact.
	unknownMsg := gmailMsg("glv-unk", "thr-unk", unknown, []string{account}, nil, nil, "U", "body", "<"+unkExt+">", sentAt.Add(2*time.Hour).UnixMilli())

	store := newFakeMessageStore([]*gmailapi.Message{inbound, outbound, unknownMsg})
	provider.SetFetcherFactoryForTest(google.NewFakeGmailFetcherFactoryForTest(store.fetcherFuncs()))
	provider.SetMeSetForTest(map[string]struct{}{account: {}})

	registry := syncpkg.NewProviderRegistry()
	registry.Register(provider)

	// Create the enabled (email, account) state exactly as the boot
	// reconciliation would (enabled, contact_driven, backfill_since, empty
	// cursor, NULL next_sync_at).
	stateLister := &reconcileStubLister{accounts: []repository.OAuthCredentialStatus{
		credWithScope(account, gmailReadonlyScopeForTest),
	}}
	reconcileSvc := service.NewSyncService(syncRepo, contactRepo, registry)
	reconcileSvc.SetEmailAccountLister(stateLister)
	require.NoError(t, reconcileSvc.ReconcileEmailSyncStates(ctx))

	// Sanity: the reconciliation created the enabled state the sweep fetches.
	acct := account
	created, err := syncRepo.GetSyncStateBySource(ctx, "email", &acct)
	require.NoError(t, err)
	require.True(t, created.Enabled)
	require.Nil(t, created.SyncCursor, "cursor empty before first sweep")

	// Anchor the backfill floor a few days behind the fixture so one sweep's
	// catch-up always reaches it, independent of the calendar date the suite
	// runs on. With the default 2026-01-01 floor, the fixed per-run window cap
	// would eventually stop covering "yesterday" as wall-clock advances.
	md := created.Metadata
	if md == nil {
		md = map[string]any{}
	}
	md["backfill_since"] = sentAt.Add(-72 * time.Hour).Format("2006-01-02")
	_, err = syncRepo.UpdateSyncStateMetadata(ctx, created.ID, md)
	require.NoError(t, err)

	// Drive the sweep through the worker entry point: RunAccountSync fetches
	// fresh state from the registry-backed provider, runs Sync, and writes the
	// advanced cursor via UpdateSyncStateSuccess.
	sweepSvc := service.NewSyncService(syncRepo, contactRepo, registry)
	require.NoError(t, sweepSvc.RunAccountSync(ctx, "email", &acct))

	// Content rows exist for both qualifying directions (inbound + outbound),
	// none for the unknown bystander.
	rows, err := commsRepo.ListByContact(ctx, contact.ID)
	require.NoError(t, err)
	require.Len(t, rows, 2, "one inbound + one outbound content row for the known contact")

	// Wait for the async consumer to derive interactions + apply cadence.
	waitForCommsProcessedByExternalID(t, ctx, commsRepo, inExt, contact.ID)
	waitForCommsProcessedByExternalID(t, ctx, commsRepo, outExt, contact.ID)

	interactions := listEmailInteractionsFor(t, ctx, interactionRepo, contact.ID)
	require.NotEmpty(t, interactions, "consumer derived at least one interaction")

	// Cadence: inbound bumps last_contacted; outbound bumps last_outreach_at.
	c := waitForBothCadence(t, ctx, contactRepo, contact.ID)
	require.NotNil(t, c.LastContacted, "inbound bumps last_contacted")
	require.NotNil(t, c.LastOutreachAt, "outbound bumps last_outreach_at")

	// Match-only: the unknown participant never produced a contact_method.
	unknownMatches, err := identityRepo.FindContactMethodsByValue(ctx, []string{"email"}, unknown)
	require.NoError(t, err)
	require.Empty(t, unknownMatches, "unknown participant must not create a contact")

	// Cursor advanced off empty → a second RunAccountSync is incremental.
	after, err := syncRepo.GetSyncStateBySource(ctx, "email", &acct)
	require.NoError(t, err)
	require.NotNil(t, after.SyncCursor, "cursor advanced after the sweep")
	require.NotEmpty(t, *after.SyncCursor)
}

// waitForCommsProcessedByExternalID polls until the comms_message row for
// (externalID, contact) is linked to an interaction (interaction_id +
// processed_at set), via the repository.
func waitForCommsProcessedByExternalID(t *testing.T, ctx context.Context, commsRepo *repository.CommsMessageRepository, externalID string, contactID uuid.UUID) {
	t.Helper()
	deadline := accelerated.GetCurrentTime().Add(defaultInteractionWaitTimeout)
	for accelerated.GetCurrentTime().Before(deadline) {
		msg, err := commsRepo.GetMessage(ctx, repository.InteractionSourceEmail, externalID, contactID)
		if err == nil && msg.InteractionID != nil && msg.ProcessedAt != nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for comms_message %s to be processed for contact %s", externalID, contactID)
}

// listEmailInteractionsFor returns the contact's live email interactions.
func listEmailInteractionsFor(t *testing.T, ctx context.Context, interactionRepo *repository.InteractionRepository, contactID uuid.UUID) []repository.Interaction {
	t.Helper()
	rows, err := interactionRepo.ListContactInteractions(ctx, contactID, 100, 0)
	require.NoError(t, err)
	out := make([]repository.Interaction, 0, len(rows))
	for _, r := range rows {
		if r.Source == repository.InteractionSourceEmail {
			out = append(out, r)
		}
	}
	return out
}

// waitForBothCadence polls until both last_contacted and last_outreach_at are set.
func waitForBothCadence(t *testing.T, ctx context.Context, contactRepo *repository.ContactRepository, contactID uuid.UUID) *repository.Contact {
	t.Helper()
	deadline := accelerated.GetCurrentTime().Add(defaultInteractionWaitTimeout)
	for accelerated.GetCurrentTime().Before(deadline) {
		c, err := contactRepo.GetContact(ctx, contactID)
		require.NoError(t, err)
		if c.LastContacted != nil && c.LastOutreachAt != nil {
			return c
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for last_contacted + last_outreach_at on contact %s", contactID)
	return nil
}
