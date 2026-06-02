// End-to-end coverage for GmailRematchHandler.
// These tests drive the REAL handler against a REAL events.Bus + database.Pool
// and the REAL EmailInteractionConsumer (so derived interactions are asserted,
// not just published events), with a FAKE gmailFetcher + injected me-set + the
// REAL SyncRepository seeded with enabled email sync states (no OAuth touched).
// They prove the new-address backfill seam:
//   - a new address scan publishes email.* events that the consumer turns into
//     interactions with the right direction + cadence columns;
//   - match-only (an unknown participant never creates a contact);
//   - fan-out to every contact sharing the address;
//   - across multiple accounts the per-account `seen` set is NOT hoisted (each
//     account is scanned; the interaction derives exactly once via dedup;
//     provenance merges both accounts);
//   - the rematch scan creates NO external_sync_state row (no cursor rewind);
//   - the scan query floors at the backfill since-date and uses the
//     single-address OR-group shape;
//   - only ENABLED, non-disabled email accounts are scanned (no enabled state
//     → no-op); any enabled-account scan failure surfaces an error;
//   - each enabled account is scanned with its OWN metadata["backfill_since"].
//
// SHARED-DB CAUTION: ListEnabledSyncStates returns ALL enabled email states in
// the shared personal_crm_test DB, so the handler scans every enabled email
// account, not just this test's. Mitigations: each scenario seeds a per-test
// random account id, the fake fetcher is account-agnostic (so a stray enabled
// account only re-scans the same fake messages, collapsed by comms_message
// upsert dedup keyed by (externalID, contactID)), every seeded state is
// hard-deleted in t.Cleanup, and assertions are scoped to the test's OWN
// freshly-created contact's rows — never a global state count.
//
// Addresses are placeholders; timestamps are accelerated.GetCurrentTime()-safe
// (the localNoonAnchor helper from the email suite); all assertions go through
// repositories (no raw SQL).
package tests

import (
	"context"
	"errors"
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
// interface it is registered under.
var _ service.RematchHandler = (*google.GmailRematchHandler)(nil)

// gmailRematchEnv bundles the real handler + provider + email consumer harness.
type gmailRematchEnv struct {
	ctx             context.Context
	database        *db.Database
	provider        *google.GmailSyncProvider
	handler         *google.GmailRematchHandler
	accountIDs      []string
	bus             *events.Bus
	commsRepo       *repository.CommsMessageRepository
	contactRepo     *repository.ContactRepository
	methodRepo      *repository.ContactMethodRepository
	interactionRepo *repository.InteractionRepository
	identityRepo    *repository.IdentityRepository
	eventRepo       *repository.EventRepository
	syncRepo        *repository.SyncRepository
}

// newGmailRematchEnv builds the harness and seeds an ENABLED email sync state
// (backfill_since=2026-01-01) for each accountID so the handler's enablement
// gate scans them. Pass nil/empty to seed nothing (the no-op gate test).
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
	handler := google.NewGmailRematchHandler(provider, syncRepo, commsRepo)

	env := &gmailRematchEnv{
		ctx:             ctx,
		database:        database,
		provider:        provider,
		handler:         handler,
		accountIDs:      accountIDs,
		bus:             bus,
		commsRepo:       commsRepo,
		contactRepo:     contactRepo,
		methodRepo:      methodRepo,
		interactionRepo: interactionRepo,
		identityRepo:    identityRepo,
		eventRepo:       eventRepo,
		syncRepo:        syncRepo,
	}

	// Seed an enabled email sync state per account so the handler's enablement
	// gate scans them (default backfill_since).
	for _, id := range accountIDs {
		env.seedEnabledEmailState(t, id, "2026-01-01")
	}
	return env
}

// seedEnabledEmailState creates an ENABLED (email, accountID) sync state with
// metadata["backfill_since"]=backfillSince via the repository (sqlc-backed, no
// raw SQL) and registers a hard-delete cleanup so the shared test DB does not
// accumulate enabled email states that pollute other tests.
func (e *gmailRematchEnv) seedEnabledEmailState(t *testing.T, accountID, backfillSince string) {
	t.Helper()
	acct := accountID
	_, err := e.syncRepo.CreateSyncState(e.ctx, repository.CreateSyncStateRequest{
		Source:    "email",
		AccountID: &acct,
		Enabled:   true,
		Strategy:  repository.SyncStrategyContactDriven,
		Metadata:  map[string]any{"backfill_since": backfillSince},
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = e.syncRepo.DeleteSyncStatesByAccountID(e.ctx, accountID)
	})
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
	me := e.accountIDs[0]
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
	me := e.accountIDs[0]
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
	me := e.accountIDs[0]
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

// The rematch reads the enabled email sync state (the enablement gate) but must
// never WRITE one: no cursor advance, no last_sync/last_successful_sync bump
// (spec §3.3 — steady-state cursors are not rewound). The state is seeded by
// the harness (the gate requires it); we assert its cursor/sync fields stay
// untouched after a rematch that DID scan + persist rows.
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

	matched, err := e.handler.Rematch(e.ctx, contactA.ID, addrA)
	require.NoError(t, err)
	require.Equal(t, 1, matched, "the rematch did scan and persist a row")

	// The seeded state's cursor + sync timestamps are untouched: the rematch
	// does NOT advance or rewind the steady-state cursor.
	st, err := e.syncRepo.GetSyncStateBySource(e.ctx, "email", &account)
	require.NoError(t, err)
	require.Nil(t, st.SyncCursor, "rematch must not write a cursor")
	require.Nil(t, st.LastSyncAt, "rematch must not bump last_sync_at")
	require.Nil(t, st.LastSuccessfulSyncAt, "rematch must not bump last_successful_sync_at")
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

// --- Scenario 7: enablement gate — no enabled email state → no-op -----------

// TestGmailRematch_NoEnabledState_NoOp proves the P0 enablement gate: when the
// account has no enabled, non-disabled email sync state, the rematch is a no-op
// — it never builds a fetcher (so the scan never runs) and writes nothing. Two
// sub-cases: (a) no state at all, (b) a status='disabled' state.
func TestGmailRematch_NoEnabledState_NoOp(t *testing.T) {
	t.Run("no_state_at_all", func(t *testing.T) {
		// Seed NO email state for this env (nil account list).
		e := newGmailRematchEnv(t, nil)
		suffix := uuid.NewString()[:8]
		me := "acct-" + suffix + "@example.com"
		addrA := "a-" + suffix + "@example.com"
		contactA := e.newContactWithEmail(t, "Contact A "+suffix, addrA)

		// A fetcher that WOULD return a qualifying message if it were ever
		// consulted — the gate must prevent that.
		msg := gmailMsg("g-gate-a", "thr", addrA, []string{me}, nil, nil, "S", "body", "<gate-a-"+suffix+"@example.com>", localNoonAnchor().UnixMilli())
		store := newRecordingMessageStore([]*gmailapi.Message{msg})
		e.inject(store, map[string]struct{}{me: {}})

		matched, err := e.handler.Rematch(e.ctx, contactA.ID, addrA)
		require.NoError(t, err)
		require.Equal(t, 0, matched, "no enabled email state → no matches")
		require.Empty(t, store.queries, "fetcher must never be consulted (gate ran before any scan)")
		require.Empty(t, store.getCalls, "no message body fetched")

		rows, err := e.commsRepo.ListByContact(e.ctx, contactA.ID)
		require.NoError(t, err)
		require.Empty(t, rows, "no qualifying rows written for the contact")
	})

	t.Run("disabled_state", func(t *testing.T) {
		// Seed an email state for this account, then DISABLE it via the
		// enable/disable path. ListEnabledSyncStates filters status='disabled'.
		e := newGmailRematchEnv(t, nil)
		suffix := uuid.NewString()[:8]
		me := "acct-" + suffix + "@example.com"
		addrA := "a-" + suffix + "@example.com"
		contactA := e.newContactWithEmail(t, "Contact A "+suffix, addrA)

		acct := me
		created, err := e.syncRepo.CreateSyncState(e.ctx, repository.CreateSyncStateRequest{
			Source:    "email",
			AccountID: &acct,
			Enabled:   true,
			Status:    repository.SyncStatusDisabled,
			Strategy:  repository.SyncStrategyContactDriven,
			Metadata:  map[string]any{"backfill_since": "2026-01-01"},
		})
		require.NoError(t, err)
		t.Cleanup(func() { _ = e.syncRepo.DeleteSyncStatesByAccountID(e.ctx, acct) })

		// Belt-and-suspenders: also flip enabled=false so neither gate clause
		// admits it (ListEnabledSyncStates requires enabled=TRUE AND
		// status!='disabled').
		_, err = e.syncRepo.UpdateSyncStateEnabled(e.ctx, created.ID, false)
		require.NoError(t, err)

		msg := gmailMsg("g-gate-d", "thr", addrA, []string{me}, nil, nil, "S", "body", "<gate-d-"+suffix+"@example.com>", localNoonAnchor().UnixMilli())
		store := newRecordingMessageStore([]*gmailapi.Message{msg})
		e.inject(store, map[string]struct{}{me: {}})

		matched, err := e.handler.Rematch(e.ctx, contactA.ID, addrA)
		require.NoError(t, err)
		require.Equal(t, 0, matched, "disabled email state → no matches")
		require.Empty(t, store.queries, "disabled account must not be scanned")

		rows, err := e.commsRepo.ListByContact(e.ctx, contactA.ID)
		require.NoError(t, err)
		require.Empty(t, rows, "no qualifying rows written for the contact")
	})
}

// --- Scenario 8: partial failure → returns error (River retries) ------------

// TestGmailRematch_PartialFailure_ReturnsError proves the P1 fail-on-any-error
// behavior: two enabled accounts X and Y, X's scan errors, Y's succeeds. The
// handler returns a non-nil error (so River retries the whole idempotent job)
// while preserving Y's partial progress in `matched`. The error carries the
// failing OWN-mailbox account id RAW (operator triage) and must NOT contain the
// third-party contact address being rematched.
func TestGmailRematch_PartialFailure_ReturnsError(t *testing.T) {
	suffix := uuid.NewString()[:8]
	accountX := "x-" + suffix + "@example.com"
	accountY := "y-" + suffix + "@example.com"
	e := newGmailRematchEnv(t, []string{accountX, accountY})
	addrA := "a-" + suffix + "@example.com"
	contactA := e.newContactWithEmail(t, "Contact A "+suffix, addrA)

	ext := "pf-" + suffix + "@example.com"
	e.cleanupEvents(t, ext)
	msg := gmailMsg("g-pf", "thr", addrA, []string{accountY}, nil, nil, "S", "body", "<"+ext+">", localNoonAnchor().UnixMilli())

	// Account-keyed fetcher factory: account X always fails its list call;
	// account Y returns the qualifying message and records its query.
	yStore := newRecordingMessageStore([]*gmailapi.Message{msg})
	listErr := errors.New("simulated gmail list outage")
	e.provider.SetFetcherFactoryForTest(google.NewFakeGmailFetcherFactoryByAccountForTest(func(accountID string) google.FakeGmailFetcherFuncs {
		if accountID == accountX {
			return google.FakeGmailFetcherFuncs{
				ListMessageIDs: func(_ context.Context, _, _ string) ([]google.GmailMessageRefForTest, string, error) {
					return nil, "", listErr
				},
				GetMessage: func(_ context.Context, _ string) (*gmailapi.Message, error) { return nil, nil },
			}
		}
		return yStore.fetcherFuncs()
	}))
	e.provider.SetMeSetForTest(map[string]struct{}{accountX: {}, accountY: {}})

	matched, err := e.handler.Rematch(e.ctx, contactA.ID, addrA)
	require.Error(t, err, "any enabled-account scan failure must surface an error so River retries")
	require.Equal(t, 1, matched, "account Y's successful row is still counted (partial progress preserved)")

	// PII posture: own-mailbox account id RAW for triage, NO third-party address.
	require.Contains(t, err.Error(), accountX, "error names the failing own-mailbox account id (raw, for triage)")
	require.NotContains(t, err.Error(), addrA, "error must NOT leak the third-party contact address")

	// Account Y's row was persisted despite X's failure.
	rows, err := e.commsRepo.ListByContact(e.ctx, contactA.ID)
	require.NoError(t, err)
	require.Len(t, rows, 1, "account Y's qualifying row persisted")
}

// --- Scenario 9: per-account backfill_since honored -------------------------

// TestGmailRematch_PerAccountBackfillSince proves the P2 fix: each enabled
// account is scanned with its OWN metadata["backfill_since"], not a hardcoded
// default. The inverse of TestGmailRematch_BackfillFloorAndQueryShape.
func TestGmailRematch_PerAccountBackfillSince(t *testing.T) {
	t.Run("explicit_backfill_since", func(t *testing.T) {
		// Seed NO default state; create one with an explicit non-default floor.
		e := newGmailRematchEnv(t, nil)
		suffix := uuid.NewString()[:8]
		account := "acct-" + suffix + "@example.com"
		e.seedEnabledEmailState(t, account, "2026-03-15")

		addrA := "a-" + suffix + "@example.com"
		contactA := e.newContactWithEmail(t, "Contact A "+suffix, addrA)
		ext := "bf-" + suffix + "@example.com"
		e.cleanupEvents(t, ext)
		msg := gmailMsg("g-bf", "thr", addrA, []string{account}, nil, nil, "S", "body", "<"+ext+">", localNoonAnchor().UnixMilli())
		store := newRecordingMessageStore([]*gmailapi.Message{msg})
		e.inject(store, map[string]struct{}{account: {}})

		_, err := e.handler.Rematch(e.ctx, contactA.ID, addrA)
		require.NoError(t, err)

		require.NotEmpty(t, store.queries, "the scan must have issued at least one list query")
		wantEpoch := time.Date(2026, 3, 15, 0, 0, 0, 0, time.UTC).Unix()
		defaultEpoch := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Unix()
		for _, q := range store.queries {
			require.Contains(t, q, "after:"+strconv.FormatInt(wantEpoch, 10), "query floors at the per-account backfill_since")
			require.NotContains(t, q, "after:"+strconv.FormatInt(defaultEpoch, 10), "must NOT use the hardcoded default floor")
		}
	})

	t.Run("absent_backfill_since_falls_back_to_default", func(t *testing.T) {
		// An enabled state with empty metadata falls back to 2026-01-01 through
		// the new per-state wiring (proves backfillSinceEpoch's default path).
		e := newGmailRematchEnv(t, nil)
		suffix := uuid.NewString()[:8]
		account := "acct-" + suffix + "@example.com"
		acct := account
		_, err := e.syncRepo.CreateSyncState(e.ctx, repository.CreateSyncStateRequest{
			Source:    "email",
			AccountID: &acct,
			Enabled:   true,
			Strategy:  repository.SyncStrategyContactDriven,
			// No metadata → no backfill_since key.
		})
		require.NoError(t, err)
		t.Cleanup(func() { _ = e.syncRepo.DeleteSyncStatesByAccountID(e.ctx, acct) })

		addrA := "a-" + suffix + "@example.com"
		contactA := e.newContactWithEmail(t, "Contact A "+suffix, addrA)
		ext := "bfd-" + suffix + "@example.com"
		e.cleanupEvents(t, ext)
		msg := gmailMsg("g-bfd", "thr", addrA, []string{account}, nil, nil, "S", "body", "<"+ext+">", localNoonAnchor().UnixMilli())
		store := newRecordingMessageStore([]*gmailapi.Message{msg})
		e.inject(store, map[string]struct{}{account: {}})

		_, err = e.handler.Rematch(e.ctx, contactA.ID, addrA)
		require.NoError(t, err)

		require.NotEmpty(t, store.queries, "the scan must have issued at least one list query")
		defaultEpoch := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Unix()
		for _, q := range store.queries {
			require.Contains(t, q, "after:"+strconv.FormatInt(defaultEpoch, 10), "absent backfill_since falls back to the default floor")
		}
	})
}
