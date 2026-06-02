// End-to-end coverage for GmailRematchHandler (Gmail integration phase 4).
// These tests drive the REAL handler against a REAL events.Bus + database.Pool
// and the REAL EmailInteractionConsumer (so derived interactions are asserted,
// not just published events), with a FAKE gmailFetcher + injected me-set + a
// stub accountLister (no OAuth touched). They prove the new-address backfill
// seam:
//   - a new address scan publishes email.* events that the consumer turns into
//     interactions with the right direction + cadence columns;
//   - match-only (an unknown participant never creates a contact);
//   - fan-out to every contact sharing the address;
//   - across multiple accounts the per-account `seen` set is NOT hoisted (each
//     account is scanned; the interaction derives exactly once via dedup;
//     provenance merges both accounts);
//   - the rematch scan creates NO external_sync_state row (no cursor rewind);
//   - the scan query floors at the backfill since-date and uses the
//     single-address OR-group shape.
//
// Addresses are placeholders; timestamps are accelerated.GetCurrentTime()-safe
// (the localNoonAnchor helper from the email suite); all assertions go through
// repositories (no raw SQL).
package tests

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/events"
	"personal-crm/backend/internal/google"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	gmailapi "google.golang.org/api/gmail/v1"
)

// Compile-time proof that GmailRematchHandler satisfies the rematch handler
// interface phase 5 will register it under. The handler is unwired in phase 4,
// so this is the only build site exercising the interface boundary until then.
var _ service.RematchHandler = (*google.GmailRematchHandler)(nil)

// gmailRematchEnv bundles the real handler + provider + email consumer harness.
type gmailRematchEnv struct {
	ctx             context.Context
	database        *db.Database
	provider        *google.GmailSyncProvider
	handler         *google.GmailRematchHandler
	accounts        *stubAccountLister
	bus             *events.Bus
	commsRepo       *repository.CommsMessageRepository
	contactRepo     *repository.ContactRepository
	methodRepo      *repository.ContactMethodRepository
	interactionRepo *repository.InteractionRepository
	identityRepo    *repository.IdentityRepository
	eventRepo       *repository.EventRepository
	syncRepo        *repository.SyncRepository
}

// stubAccountLister satisfies google's accountLister seam with canned account
// ids — no OAuth. Each id becomes an OAuthCredentialStatus with AccountID set.
type stubAccountLister struct {
	accountIDs []string
}

func (s *stubAccountLister) ListAccounts(_ context.Context) ([]repository.OAuthCredentialStatus, error) {
	out := make([]repository.OAuthCredentialStatus, 0, len(s.accountIDs))
	for _, id := range s.accountIDs {
		out = append(out, repository.OAuthCredentialStatus{AccountID: id})
	}
	return out, nil
}

func newGmailRematchEnv(t *testing.T, accountIDs []string) *gmailRematchEnv {
	t.Helper()
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
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

	commsRepo := repository.NewCommsMessageRepository(database.Queries)
	contactRepo := repository.NewContactRepository(database.Queries)
	contactRepo.SetPool(database.Pool)
	methodRepo := repository.NewContactMethodRepository(database.Queries)
	interactionRepo := repository.NewInteractionRepository(database.Queries)
	identityRepo := repository.NewIdentityRepository(database.Queries)
	contactTaskRepo := repository.NewContactTaskRepository(database.Queries)
	eventRepo := repository.NewEventRepository(database.Queries)
	syncRepo := repository.NewSyncRepository(database.Queries)
	contactService := service.NewContactService(database, contactRepo, methodRepo, interactionRepo, contactTaskRepo, nil, nil)

	// Real email consumer wired on the live bus so the handler's published
	// email.* events are turned into interactions (not just logged).
	bus, _ := setupTestEventBusForEmail(t, ctx, database, contactService)

	provider := google.NewGmailSyncProvider(nil, commsRepo, bus, database.Pool)
	accounts := &stubAccountLister{accountIDs: accountIDs}
	handler := google.NewGmailRematchHandler(provider, accounts, commsRepo)

	return &gmailRematchEnv{
		ctx:             ctx,
		database:        database,
		provider:        provider,
		handler:         handler,
		accounts:        accounts,
		bus:             bus,
		commsRepo:       commsRepo,
		contactRepo:     contactRepo,
		methodRepo:      methodRepo,
		interactionRepo: interactionRepo,
		identityRepo:    identityRepo,
		eventRepo:       eventRepo,
		syncRepo:        syncRepo,
	}
}

func (e *gmailRematchEnv) newContactWithEmail(t *testing.T, name, email string) *repository.Contact {
	t.Helper()
	cadence := "weekly"
	contact, err := e.contactRepo.CreateContact(e.ctx, repository.CreateContactRequest{
		FullName: name,
		Cadence:  &cadence,
	})
	require.NoError(t, err)
	_, err = e.methodRepo.CreateContactMethod(e.ctx, repository.CreateContactMethodRequest{
		ContactID: contact.ID,
		Type:      "email",
		Value:     email,
		IsPrimary: true,
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = e.commsRepo.HardDeleteByContact(e.ctx, contact.ID)
		_ = e.interactionRepo.HardDeleteInteractionsBySourceRefPrefix(e.ctx, repository.InteractionSourceEmail, contact.ID.String()+":%")
		_ = e.contactRepo.SoftDeleteContact(e.ctx, contact.ID)
	})
	return contact
}

func (e *gmailRematchEnv) cleanupEvents(t *testing.T, externalID string) {
	t.Helper()
	t.Cleanup(func() {
		_ = e.eventRepo.HardDeleteEventsBySourceAndSourceIDPrefix(e.ctx, "email", externalID+"%")
	})
}

// inject wires the fake store as the provider's fetcher + sets the me-set.
func (e *gmailRematchEnv) inject(store *recordingMessageStore, meSet map[string]struct{}) {
	e.provider.SetFetcherFactoryForTest(google.NewFakeGmailFetcherFactoryForTest(store.fetcherFuncs()))
	e.provider.SetMeSetForTest(meSet)
}

// recordingMessageStore is a query-agnostic gmailFetcher that (a) returns all
// registered messages for every non-empty query, (b) records the query strings
// it received, and (c) counts GetMessage calls per id. The per-id counter is
// the per-account-`seen` regression gate: a hoisted/global seen set would skip
// account B's GetMessage and the count would be 1 instead of (num accounts).
type recordingMessageStore struct {
	messages []*gmailapi.Message
	getCalls map[string]int
	queries  []string
}

func newRecordingMessageStore(messages []*gmailapi.Message) *recordingMessageStore {
	return &recordingMessageStore{messages: messages, getCalls: map[string]int{}}
}

func (s *recordingMessageStore) fetcherFuncs() google.FakeGmailFetcherFuncs {
	return google.FakeGmailFetcherFuncs{
		ListMessageIDs: func(_ context.Context, query, pageToken string) ([]google.GmailMessageRefForTest, string, error) {
			if pageToken != "" {
				return nil, "", nil
			}
			s.queries = append(s.queries, query)
			refs := make([]google.GmailMessageRefForTest, 0, len(s.messages))
			for _, m := range s.messages {
				refs = append(refs, google.GmailMessageRefForTest{ID: m.Id, ThreadID: m.ThreadId})
			}
			return refs, "", nil
		},
		GetMessage: func(_ context.Context, id string) (*gmailapi.Message, error) {
			s.getCalls[id]++
			for _, m := range s.messages {
				if m.Id == id {
					return m, nil
				}
			}
			return nil, nil
		},
	}
}

func (e *gmailRematchEnv) waitForCommsProcessed(t *testing.T, externalID string, contactID uuid.UUID) *repository.CommsMessage {
	t.Helper()
	deadline := accelerated.GetCurrentTime().Add(defaultInteractionWaitTimeout)
	for accelerated.GetCurrentTime().Before(deadline) {
		msg, err := e.commsRepo.GetMessage(e.ctx, repository.InteractionSourceEmail, externalID, contactID)
		if err == nil && msg.InteractionID != nil && msg.ProcessedAt != nil {
			return msg
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for comms_message %s to be processed for contact %s", externalID, contactID)
	return nil
}

// listEmailInteractions returns the contact's live email interactions.
func (e *gmailRematchEnv) listEmailInteractions(t *testing.T, contactID uuid.UUID) []repository.Interaction {
	t.Helper()
	rows, err := e.interactionRepo.ListContactInteractions(e.ctx, contactID, 100, 0)
	require.NoError(t, err)
	out := make([]repository.Interaction, 0, len(rows))
	for _, r := range rows {
		if r.Source == repository.InteractionSourceEmail {
			out = append(out, r)
		}
	}
	return out
}

// waitForLastContacted polls until the contact's last_contacted is set.
func (e *gmailRematchEnv) waitForLastContacted(t *testing.T, contactID uuid.UUID) *repository.Contact {
	t.Helper()
	deadline := accelerated.GetCurrentTime().Add(defaultInteractionWaitTimeout)
	for accelerated.GetCurrentTime().Before(deadline) {
		c, err := e.contactRepo.GetContact(e.ctx, contactID)
		require.NoError(t, err)
		if c.LastContacted != nil && c.LastOutreachAt != nil {
			return c
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for last_contacted + last_outreach_at on contact %s", contactID)
	return nil
}

// --- Scenario 1: new-address scan derives interactions ----------------------

func TestGmailRematch_NewAddressScan_DerivesInteractions(t *testing.T) {
	e := newGmailRematchEnv(t, []string{"acct-" + uuid.NewString()[:8] + "@example.com"})
	suffix := uuid.NewString()[:8]
	me := e.accounts.accountIDs[0]
	addrA := "a-" + suffix + "@example.com"
	contactA := e.newContactWithEmail(t, "Contact A "+suffix, addrA)

	inExt := "in-" + suffix + "@example.com"
	outExt := "out-" + suffix + "@example.com"
	e.cleanupEvents(t, inExt)
	e.cleanupEvents(t, outExt)

	sentAt := localNoonAnchor()
	inbound := gmailMsg("g-in", "thr-in", addrA, []string{me}, nil, nil, "In", "hi", "<"+inExt+">", sentAt.UnixMilli())
	outbound := gmailMsg("g-out", "thr-out", me, []string{addrA}, nil, nil, "Out", "yo", "<"+outExt+">", sentAt.Add(time.Hour).UnixMilli())
	e.inject(newRecordingMessageStore([]*gmailapi.Message{inbound, outbound}), map[string]struct{}{me: {}})

	matched, err := e.handler.Rematch(e.ctx, contactA.ID, addrA)
	require.NoError(t, err)
	require.Equal(t, 2, matched, "one inbound + one outbound row persisted")

	// Content rows exist for both directions.
	rows, err := e.commsRepo.ListByContact(e.ctx, contactA.ID)
	require.NoError(t, err)
	require.Len(t, rows, 2)

	// Events published.
	inEnv, err := e.eventRepo.FindEventBySource(e.ctx, "email", inExt+":"+contactA.ID.String())
	require.NoError(t, err)
	require.Equal(t, events.KindEmailReceived, inEnv.Kind)
	outEnv, err := e.eventRepo.FindEventBySource(e.ctx, "email", outExt+":"+contactA.ID.String())
	require.NoError(t, err)
	require.Equal(t, events.KindEmailSent, outEnv.Kind)

	// Consumer derives interactions; cadence columns updated.
	e.waitForCommsProcessed(t, inExt, contactA.ID)
	e.waitForCommsProcessed(t, outExt, contactA.ID)

	interactions := e.listEmailInteractions(t, contactA.ID)
	require.NotEmpty(t, interactions, "consumer derived at least one interaction")

	c := e.waitForLastContacted(t, contactA.ID)
	require.NotNil(t, c.LastContacted, "inbound bumps last_contacted")
	require.NotNil(t, c.LastOutreachAt, "outbound bumps last_outreach_at")
}

// --- Scenario 2: match-only (no contact creation) ---------------------------

func TestGmailRematch_MatchOnly_NoContactCreated(t *testing.T) {
	e := newGmailRematchEnv(t, []string{"acct-" + uuid.NewString()[:8] + "@example.com"})
	suffix := uuid.NewString()[:8]
	me := e.accounts.accountIDs[0]
	addrA := "a-" + suffix + "@example.com"
	unknown := "unknown-" + suffix + "@example.com"
	contactA := e.newContactWithEmail(t, "Contact A "+suffix, addrA)

	e.cleanupEvents(t, "mo-"+suffix+"@example.com")
	// Message between "me" and an UNKNOWN address only (A is not a participant).
	msg := gmailMsg("g-mo", "thr", unknown, []string{me}, nil, nil, "S", "body", "<mo-"+suffix+"@example.com>", localNoonAnchor().UnixMilli())
	e.inject(newRecordingMessageStore([]*gmailapi.Message{msg}), map[string]struct{}{me: {}})

	// Rematch for A's address, but the message has no A participant → no rows.
	matched, err := e.handler.Rematch(e.ctx, contactA.ID, addrA)
	require.NoError(t, err)
	require.Equal(t, 0, matched)

	rows, err := e.commsRepo.ListByContact(e.ctx, contactA.ID)
	require.NoError(t, err)
	require.Empty(t, rows, "no qualifying rows for A")

	// The unknown address never produced a contact_method/contact.
	matches, err := e.identityRepo.FindContactMethodsByValue(e.ctx, []string{"email"}, unknown)
	require.NoError(t, err)
	require.Empty(t, matches, "unknown address must not have produced a contact")
}

// --- Scenario 3: fan-out to multiple contacts sharing the address -----------

func TestGmailRematch_FanOut_SharedAddress(t *testing.T) {
	e := newGmailRematchEnv(t, []string{"acct-" + uuid.NewString()[:8] + "@example.com"})
	suffix := uuid.NewString()[:8]
	me := e.accounts.accountIDs[0]
	shared := "shared-" + suffix + "@example.com"
	c1 := e.newContactWithEmail(t, "Contact One "+suffix, shared)
	c2 := e.newContactWithEmail(t, "Contact Two "+suffix, shared)

	ext := "fan-" + suffix + "@example.com"
	e.cleanupEvents(t, ext)
	msg := gmailMsg("g-fan", "thr", shared, []string{me}, nil, nil, "S", "body", "<"+ext+">", localNoonAnchor().UnixMilli())
	e.inject(newRecordingMessageStore([]*gmailapi.Message{msg}), map[string]struct{}{me: {}})

	matched, err := e.handler.Rematch(e.ctx, c1.ID, shared)
	require.NoError(t, err)
	require.Equal(t, 2, matched, "one row per contact sharing the address")

	// Two content rows (one per contact), two events.
	rows1, err := e.commsRepo.ListByContact(e.ctx, c1.ID)
	require.NoError(t, err)
	require.Len(t, rows1, 1)
	rows2, err := e.commsRepo.ListByContact(e.ctx, c2.ID)
	require.NoError(t, err)
	require.Len(t, rows2, 1)

	_, err = e.eventRepo.FindEventBySource(e.ctx, "email", ext+":"+c1.ID.String())
	require.NoError(t, err)
	_, err = e.eventRepo.FindEventBySource(e.ctx, "email", ext+":"+c2.ID.String())
	require.NoError(t, err)

	// Both contacts get a derived interaction.
	e.waitForCommsProcessed(t, ext, c1.ID)
	e.waitForCommsProcessed(t, ext, c2.ID)
	require.NotEmpty(t, e.listEmailInteractions(t, c1.ID))
	require.NotEmpty(t, e.listEmailInteractions(t, c2.ID))
}

// --- Scenario 4: across multiple accounts (per-account `seen` not hoisted) --

func TestGmailRematch_MultipleAccounts_PerAccountSeen(t *testing.T) {
	suffix := uuid.NewString()[:8]
	accountX := "x-" + suffix + "@example.com"
	accountY := "y-" + suffix + "@example.com"
	e := newGmailRematchEnv(t, []string{accountX, accountY})
	addrA := "a-" + suffix + "@example.com"
	contactA := e.newContactWithEmail(t, "Contact A "+suffix, addrA)

	ext := "xacct-" + suffix + "@example.com"
	e.cleanupEvents(t, ext)
	// The SAME Message-ID observed in both mailboxes: same gmail message id so
	// the fetcher's per-id counter measures cross-account scanning. Both
	// accounts share the me-set so cross-account provenance detection fires.
	sentAt := localNoonAnchor()
	msg := gmailMsg("g-shared", "thr", addrA, []string{accountX, accountY}, nil, nil, "S", "body", "<"+ext+">", sentAt.UnixMilli())
	store := newRecordingMessageStore([]*gmailapi.Message{msg})
	e.inject(store, map[string]struct{}{accountX: {}, accountY: {}})

	matched, err := e.handler.Rematch(e.ctx, contactA.ID, addrA)
	require.NoError(t, err)
	require.Equal(t, 2, matched, "both accounts persist a row (provenance merges; interaction dedups)")

	// REGRESSION GATE: GetMessage("g-shared") fired once PER ACCOUNT (= 2),
	// proving `seen` is per-ScanIdentifier/per-account and NOT hoisted. A
	// global seen set would skip account Y and the count would be 1.
	require.Equal(t, 2, store.getCalls["g-shared"], "message body fetched once per account (per-account seen)")

	// Exactly one content row, provenance merged across BOTH accounts.
	rows, err := e.commsRepo.ListByContact(e.ctx, contactA.ID)
	require.NoError(t, err)
	require.Len(t, rows, 1, "cross-account same Message-ID collapses to one content row")

	got, err := e.commsRepo.GetMessage(e.ctx, "email", ext, contactA.ID)
	require.NoError(t, err)
	require.ElementsMatch(t, []string{accountX, accountY}, observedAccounts(t, got.SourceMetadata))

	// Interaction derived exactly once (event (source, source_id) dedup).
	e.waitForCommsProcessed(t, ext, contactA.ID)
	time.Sleep(300 * time.Millisecond)
	require.Len(t, e.listEmailInteractions(t, contactA.ID), 1, "cross-account same Message-ID derives one interaction")
}

// --- Scenario 5: no cursor side-effects -------------------------------------

func TestGmailRematch_NoCursorSideEffects(t *testing.T) {
	account := "acct-" + uuid.NewString()[:8] + "@example.com"
	e := newGmailRematchEnv(t, []string{account})
	suffix := uuid.NewString()[:8]
	addrA := "a-" + suffix + "@example.com"
	contactA := e.newContactWithEmail(t, "Contact A "+suffix, addrA)

	ext := "nocur-" + suffix + "@example.com"
	e.cleanupEvents(t, ext)
	msg := gmailMsg("g-nocur", "thr", addrA, []string{account}, nil, nil, "S", "body", "<"+ext+">", localNoonAnchor().UnixMilli())
	e.inject(newRecordingMessageStore([]*gmailapi.Message{msg}), map[string]struct{}{account: {}})

	_, err := e.handler.Rematch(e.ctx, contactA.ID, addrA)
	require.NoError(t, err)

	// The rematch scan must NOT have created an external_sync_state row.
	_, err = e.syncRepo.GetSyncStateBySource(e.ctx, "email", &account)
	require.ErrorIs(t, err, db.ErrNotFound, "rematch must not create/touch an external_sync_state cursor row")
}

// --- Scenario 6: backfill floor honored + single-address OR-group -----------

func TestGmailRematch_BackfillFloorAndQueryShape(t *testing.T) {
	account := "acct-" + uuid.NewString()[:8] + "@example.com"
	e := newGmailRematchEnv(t, []string{account})
	suffix := uuid.NewString()[:8]
	addrA := "a-" + suffix + "@example.com"
	contactA := e.newContactWithEmail(t, "Contact A "+suffix, addrA)

	ext := "shape-" + suffix + "@example.com"
	e.cleanupEvents(t, ext)
	msg := gmailMsg("g-shape", "thr", addrA, []string{account}, nil, nil, "S", "body", "<"+ext+">", localNoonAnchor().UnixMilli())
	store := newRecordingMessageStore([]*gmailapi.Message{msg})
	e.inject(store, map[string]struct{}{account: {}})

	_, err := e.handler.Rematch(e.ctx, contactA.ID, addrA)
	require.NoError(t, err)

	require.NotEmpty(t, store.queries, "the scan must have issued at least one list query")
	backfillEpoch := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Unix()
	for _, q := range store.queries {
		require.Contains(t, q, "after:"+strconv.FormatInt(backfillEpoch, 10), "query floors at the default backfill since-date")
		require.Contains(t, q, "category:primary", "query restricts to the primary category")
		require.Contains(t, q, "from:"+addrA, "single-address OR-group includes from:")
		require.Contains(t, q, "to:"+addrA, "single-address OR-group includes to:")
		require.Contains(t, q, "cc:"+addrA, "single-address OR-group includes cc:")
		require.Contains(t, q, "bcc:"+addrA, "single-address OR-group includes bcc:")
		// One address → exactly one OR-group, so no second group separator.
		require.False(t, strings.Contains(q, ") OR ("), "single address yields exactly one OR-group")
	}
}
