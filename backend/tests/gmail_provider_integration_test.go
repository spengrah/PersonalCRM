// End-to-end coverage for the inert Gmail sync provider, driven through
// the REAL GmailSyncProvider.Sync with a real *events.Bus + database.Pool and a
// FAKE gmailFetcher + injected M-set (no stored OAuth credential needed). The
// in-package google tests exercise the pure helpers + processMessage; these
// prove the full sweep: fetch → resolve → publish-before-mutate upsert →
// cursor advance, including:
//   - content rows + durable email.* event rows land per qualifying (msg, contact);
//   - cross-chunk seen dedup body-fetches a message once;
//   - match-only (unknown participant never creates a contact);
//   - cursor-overlap idempotency (no duplicate rows / events / provenance growth);
//   - publish-before-mutate (a failing PublishTx leaves no content row);
//   - nomsgid fallback persistence;
//   - cursor edge cases (all-bystander advances; zero-fetched preserves; hard
//     failure leaves cursor unchanged);
//   - nil-account guard;
//   - cross-account set-union provenance merge through the provider/upsert path.
//
// email.* kinds route to the email_interaction_consumer job (Gmail phase 3);
// this harness registers a no-op worker for that kind so the provider's
// publishes enqueue legally. These tests assert on the durable event-log row
// + content rows, not the derived interaction (that is the consumer's own
// suite, email_interaction_integration_test.go).
package tests

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/events"
	"personal-crm/backend/internal/google"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"
	gmailapi "google.golang.org/api/gmail/v1"
)

// gmailProviderEnv bundles a real provider + bus + repos for the sweep tests.
type gmailProviderEnv struct {
	ctx          context.Context
	database     *db.Database
	provider     *google.GmailSyncProvider
	bus          *events.Bus
	commsRepo    *repository.CommsMessageRepository
	contactRepo  *repository.ContactRepository
	methodRepo   *repository.ContactMethodRepository
	identityRepo *repository.IdentityRepository
	eventRepo    *repository.EventRepository
}

func newGmailProviderEnv(t *testing.T) *gmailProviderEnv {
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
	identityRepo := repository.NewIdentityRepository(database.Queries)
	interactionRepo := repository.NewInteractionRepository(database.Queries)
	contactTaskRepo := repository.NewContactTaskRepository(database.Queries)
	eventRepo := repository.NewEventRepository(database.Queries)
	contactService := service.NewContactService(database, contactRepo, methodRepo, interactionRepo, contactTaskRepo, nil, nil)

	// Live bus via the shared harness. email.* kinds enqueue an
	// email_interaction_consumer job, drained by the harness's no-op email
	// worker — these tests assert on the durable event-log row + content
	// rows, not the derived interaction.
	bus := setupTestEventBus(t, ctx, database, contactService)

	provider := google.NewGmailSyncProvider(nil, commsRepo, bus, database.Pool)

	return &gmailProviderEnv{
		ctx:          ctx,
		database:     database,
		provider:     provider,
		bus:          bus,
		commsRepo:    commsRepo,
		contactRepo:  contactRepo,
		methodRepo:   methodRepo,
		identityRepo: identityRepo,
		eventRepo:    eventRepo,
	}
}

// newEmailContactWithMethod creates a contact with one email method and
// registers cleanup (hard-delete content + soft-delete contact).
func (e *gmailProviderEnv) newEmailContactWithMethod(t *testing.T, name, email string) *repository.Contact {
	t.Helper()
	contact, err := e.contactRepo.CreateContact(e.ctx, repository.CreateContactRequest{FullName: name})
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
		_ = e.contactRepo.SoftDeleteContact(e.ctx, contact.ID)
	})
	return contact
}

// cleanupEvents registers cleanup of the durable-but-unconsumed event rows for
// a given externalID. The email.* SourceID is "<externalID>:<contactID>", so a
// "<externalID>%" prefix scopes the delete to this test's events.
func (e *gmailProviderEnv) cleanupEvents(t *testing.T, externalID string) {
	t.Helper()
	t.Cleanup(func() {
		_ = e.eventRepo.HardDeleteEventsBySourceAndSourceIDPrefix(e.ctx, "email", externalID+"%")
	})
}

// fakeMessageStore backs a query-agnostic gmailFetcher: every non-empty query
// returns ALL registered message stubs (one page), and GetMessage looks up by
// id. Cross-chunk dedup is exercised by the provider's per-id seen set, which
// we assert via the GetMessage call counter. forcedGetErr makes GetMessage fail
// for specific ids (hard-failure tests).
type fakeMessageStore struct {
	messages     []*gmailapi.Message
	getCalls     map[string]int
	forcedGetErr map[string]struct{}
}

func newFakeMessageStore(messages []*gmailapi.Message) *fakeMessageStore {
	return &fakeMessageStore{
		messages:     messages,
		getCalls:     map[string]int{},
		forcedGetErr: map[string]struct{}{},
	}
}

func (s *fakeMessageStore) fetcherFuncs() google.FakeGmailFetcherFuncs {
	return google.FakeGmailFetcherFuncs{
		ListMessageIDs: func(_ context.Context, query, pageToken string) ([]google.GmailMessageRefForTest, string, error) {
			if pageToken != "" {
				return nil, "", nil
			}
			refs := make([]google.GmailMessageRefForTest, 0, len(s.messages))
			for _, m := range s.messages {
				refs = append(refs, google.GmailMessageRefForTest{ID: m.Id, ThreadID: m.ThreadId})
			}
			return refs, "", nil
		},
		GetMessage: func(_ context.Context, id string) (*gmailapi.Message, error) {
			s.getCalls[id]++
			if _, bad := s.forcedGetErr[id]; bad {
				return nil, fmt.Errorf("forced get error for %s", id)
			}
			for _, m := range s.messages {
				if m.Id == id {
					return m, nil
				}
			}
			return nil, fmt.Errorf("message %s not found", id)
		},
	}
}

// inject wires the store as the provider's fetcher + sets the me-set.
func (e *gmailProviderEnv) inject(store *fakeMessageStore, meSet map[string]struct{}) {
	e.provider.SetFetcherFactoryForTest(google.NewFakeGmailFetcherFactoryForTest(store.fetcherFuncs()))
	e.provider.SetMeSetForTest(meSet)
}

// gmailMsg builds a text/plain *gmail.Message for the fake fetcher.
func gmailMsg(id, threadID, from string, to, cc, bcc []string, subject, body, msgID string, internalMillis int64) *gmailapi.Message {
	headers := []*gmailapi.MessagePartHeader{{Name: "From", Value: from}}
	if len(to) > 0 {
		headers = append(headers, &gmailapi.MessagePartHeader{Name: "To", Value: strings.Join(to, ", ")})
	}
	if len(cc) > 0 {
		headers = append(headers, &gmailapi.MessagePartHeader{Name: "Cc", Value: strings.Join(cc, ", ")})
	}
	if len(bcc) > 0 {
		headers = append(headers, &gmailapi.MessagePartHeader{Name: "Bcc", Value: strings.Join(bcc, ", ")})
	}
	if subject != "" {
		headers = append(headers, &gmailapi.MessagePartHeader{Name: "Subject", Value: subject})
	}
	if msgID != "" {
		headers = append(headers, &gmailapi.MessagePartHeader{Name: "Message-ID", Value: msgID})
	}
	body64 := google.EncodeBase64URLForTest(body)
	return &gmailapi.Message{
		Id:           id,
		ThreadId:     threadID,
		InternalDate: internalMillis,
		Snippet:      "snippet",
		LabelIds:     []string{"INBOX"},
		Payload: &gmailapi.MessagePart{
			MimeType: "text/plain",
			Headers:  headers,
			Body:     &gmailapi.MessagePartBody{Data: body64, Size: int64(len(body))},
		},
	}
}

// failingBus is a busTx stub whose PublishTx always errors — used to assert
// publish-before-mutate (the content upsert must roll back).
type failingBus struct{}

func (failingBus) PublishTx(_ context.Context, _ pgx.Tx, _ *events.Envelope) error {
	return errors.New("forced publish failure")
}

func syncState(accountID, cursor string) *repository.SyncState {
	st := &repository.SyncState{Source: "email", AccountID: &accountID}
	if cursor != "" {
		st.SyncCursor = &cursor
	}
	return st
}

// --- full sweep → content rows + events ---

func TestGmailProvider_FullSweep_ContentAndEvents(t *testing.T) {
	e := newGmailProviderEnv(t)
	suffix := uuid.NewString()[:8]
	me := "me-" + suffix + "@example.com"
	addrA := "a-" + suffix + "@example.com"
	addrB := "b-" + suffix + "@example.com"
	contactA := e.newEmailContactWithMethod(t, "Contact A "+suffix, addrA)
	contactB := e.newEmailContactWithMethod(t, "Contact B "+suffix, addrB)

	inMsgID := "<in-" + suffix + "@example.com>"
	outMsgID := "<out-" + suffix + "@example.com>"
	e.cleanupEvents(t, "in-"+suffix+"@example.com")
	e.cleanupEvents(t, "out-"+suffix+"@example.com")

	inbound := gmailMsg("g-in", "thr-in", addrA, []string{me}, nil, nil, "Inbound subj", "hello from A", inMsgID, 1700000100000)
	outbound := gmailMsg("g-out", "thr-out", me, []string{addrB}, nil, nil, "Outbound subj", "hi to B", outMsgID, 1700000200000)
	e.inject(newFakeMessageStore([]*gmailapi.Message{inbound, outbound}), map[string]struct{}{me: {}})

	result, err := e.provider.Sync(e.ctx, syncState(me, ""), nil)
	require.NoError(t, err)
	require.Equal(t, 2, result.ItemsProcessed)
	require.Equal(t, 2, result.ItemsMatched)
	require.Equal(t, "1700000200", result.NewCursor) // max internalDate (epoch secs)

	aRows, err := e.commsRepo.ListByContact(e.ctx, contactA.ID)
	require.NoError(t, err)
	require.Len(t, aRows, 1)
	require.Equal(t, "inbound", aRows[0].Direction)
	require.Equal(t, "in-"+suffix+"@example.com", aRows[0].ExternalID)
	require.Equal(t, "thr-in", *aRows[0].ThreadID)
	require.Equal(t, "Inbound subj", *aRows[0].Subject)
	require.Equal(t, "hello from A", *aRows[0].Body)
	require.Equal(t, addrA, *aRows[0].PeerNormalized)
	require.Equal(t, me, *aRows[0].AccountID)

	bRows, err := e.commsRepo.ListByContact(e.ctx, contactB.ID)
	require.NoError(t, err)
	require.Len(t, bRows, 1)
	require.Equal(t, "outbound", bRows[0].Direction)
	require.Equal(t, addrB, *bRows[0].PeerNormalized)

	inEnv, err := e.eventRepo.FindEventBySource(e.ctx, "email", "in-"+suffix+"@example.com:"+contactA.ID.String())
	require.NoError(t, err)
	require.Equal(t, events.KindEmailReceived, inEnv.Kind)
	var inPayload events.EmailEventPayload
	require.NoError(t, events.Unmarshal(inEnv, &inPayload))
	require.Equal(t, contactA.ID, inPayload.ContactID)
	require.Equal(t, "inbound", inPayload.Direction)
	require.Equal(t, "thr-in", inPayload.ThreadID)

	outEnv, err := e.eventRepo.FindEventBySource(e.ctx, "email", "out-"+suffix+"@example.com:"+contactB.ID.String())
	require.NoError(t, err)
	require.Equal(t, events.KindEmailSent, outEnv.Kind)
}

// --- cross-chunk seen dedup at sweep level ---

func TestGmailProvider_CrossChunkSeenDedup(t *testing.T) {
	e := newGmailProviderEnv(t)
	suffix := uuid.NewString()[:8]
	me := "me-" + suffix + "@example.com"

	// Many known contacts force the provider to build MORE THAN ONE OR-chunk;
	// the query-agnostic fake returns the single spanning message for every
	// chunk query, so the per-id seen set is what prevents a second body fetch.
	const n = 250
	var addrs []string
	for i := 0; i < n; i++ {
		addr := fmt.Sprintf("c%03d-%s@example.com", i, suffix)
		addrs = append(addrs, addr)
		e.newEmailContactWithMethod(t, fmt.Sprintf("Contact %03d %s", i, suffix), addr)
	}

	msgID := "<span-" + suffix + "@example.com>"
	e.cleanupEvents(t, "span-"+suffix+"@example.com")
	// Inbound from the first known contact to me.
	msg := gmailMsg("g-span", "thr", addrs[0], []string{me}, nil, nil, "S", "body", msgID, 1700000300000)

	store := newFakeMessageStore([]*gmailapi.Message{msg})
	e.inject(store, map[string]struct{}{me: {}})

	// Sanity: the provider must have built >1 chunk for this to be meaningful.
	chunks := google.BuildORChunksForTest(addrs, 0)
	require.Greater(t, len(chunks), 1, "test requires multiple OR-chunks")

	_, err := e.provider.Sync(e.ctx, syncState(me, ""), nil)
	require.NoError(t, err)
	require.Equal(t, 1, store.getCalls["g-span"], "body should be fetched exactly once across chunks")
}

// --- match-only (unknown participant never creates a contact) ---

func TestGmailProvider_MatchOnly_NoContactCreated(t *testing.T) {
	e := newGmailProviderEnv(t)
	suffix := uuid.NewString()[:8]
	me := "me-" + suffix + "@example.com"
	addrA := "a-" + suffix + "@example.com"
	unknown := "unknown-" + suffix + "@example.com"
	contactA := e.newEmailContactWithMethod(t, "Contact A "+suffix, addrA)

	e.cleanupEvents(t, "mo-"+suffix+"@example.com")
	msg := gmailMsg("g-mo", "thr", addrA, []string{me, unknown}, nil, nil, "S", "body", "<mo-"+suffix+"@example.com>", 1700000400000)
	e.inject(newFakeMessageStore([]*gmailapi.Message{msg}), map[string]struct{}{me: {}})

	_, err := e.provider.Sync(e.ctx, syncState(me, ""), nil)
	require.NoError(t, err)

	aRows, err := e.commsRepo.ListByContact(e.ctx, contactA.ID)
	require.NoError(t, err)
	require.Len(t, aRows, 1)

	// The unknown address has no contact_method/contact (look it up by
	// normalized value — assert per-query, not via a global contact count).
	matches, err := e.identityRepo.FindContactMethodsByValue(e.ctx, []string{"email"}, unknown)
	require.NoError(t, err)
	require.Empty(t, matches, "unknown address must not have produced a contact")
}

// --- cursor-overlap idempotency ---

func TestGmailProvider_CursorOverlapIdempotent(t *testing.T) {
	e := newGmailProviderEnv(t)
	suffix := uuid.NewString()[:8]
	me := "me-" + suffix + "@example.com"
	addrA := "a-" + suffix + "@example.com"
	contactA := e.newEmailContactWithMethod(t, "Contact A "+suffix, addrA)

	e.cleanupEvents(t, "idem-"+suffix+"@example.com")
	msg := gmailMsg("g-idem", "thr", addrA, []string{me}, nil, nil, "S", "body", "<idem-"+suffix+"@example.com>", 1700000500000)
	e.inject(newFakeMessageStore([]*gmailapi.Message{msg}), map[string]struct{}{me: {}})

	_, err := e.provider.Sync(e.ctx, syncState(me, ""), nil)
	require.NoError(t, err)
	_, err = e.provider.Sync(e.ctx, syncState(me, ""), nil)
	require.NoError(t, err)

	aRows, err := e.commsRepo.ListByContact(e.ctx, contactA.ID)
	require.NoError(t, err)
	require.Len(t, aRows, 1, "second sweep must not add a duplicate content row")

	got, err := e.commsRepo.GetMessage(e.ctx, "email", "idem-"+suffix+"@example.com", contactA.ID)
	require.NoError(t, err)
	require.Equal(t, []string{me}, observedAccounts(t, got.SourceMetadata))
}

// --- publish-before-mutate ---

func TestGmailProvider_PublishBeforeMutate_RollsBack(t *testing.T) {
	e := newGmailProviderEnv(t)
	suffix := uuid.NewString()[:8]
	me := "me-" + suffix + "@example.com"
	addrA := "a-" + suffix + "@example.com"
	contactA := e.newEmailContactWithMethod(t, "Contact A "+suffix, addrA)

	e.cleanupEvents(t, "pbm-"+suffix+"@example.com")
	msg := gmailMsg("g-pbm", "thr", addrA, []string{me}, nil, nil, "S", "body", "<pbm-"+suffix+"@example.com>", 1700000600000)
	e.inject(newFakeMessageStore([]*gmailapi.Message{msg}), map[string]struct{}{me: {}})

	e.provider.SetBusForTest(failingBus{})
	_, err := e.provider.Sync(e.ctx, syncState(me, ""), nil)
	require.Error(t, err, "a failing PublishTx must surface as a sync error")

	aRows, err := e.commsRepo.ListByContact(e.ctx, contactA.ID)
	require.NoError(t, err)
	require.Empty(t, aRows, "the content upsert must have rolled back with the failed publish")
}

// --- nomsgid fallback persistence ---

func TestGmailProvider_NomsgidFallback(t *testing.T) {
	e := newGmailProviderEnv(t)
	suffix := uuid.NewString()[:8]
	me := "me-" + suffix + "@example.com"
	addrA := "a-" + suffix + "@example.com"
	contactA := e.newEmailContactWithMethod(t, "Contact A "+suffix, addrA)

	gmailID := "g-nomsgid-" + suffix
	expectedExternal := "nomsgid:" + me + ":" + gmailID
	e.cleanupEvents(t, expectedExternal)
	msg := gmailMsg(gmailID, "thr", addrA, []string{me}, nil, nil, "S", "body", "", 1700000700000)
	e.inject(newFakeMessageStore([]*gmailapi.Message{msg}), map[string]struct{}{me: {}})

	_, err := e.provider.Sync(e.ctx, syncState(me, ""), nil)
	require.NoError(t, err)

	aRows, err := e.commsRepo.ListByContact(e.ctx, contactA.ID)
	require.NoError(t, err)
	require.Len(t, aRows, 1)
	require.Equal(t, expectedExternal, aRows[0].ExternalID)

	env, err := e.eventRepo.FindEventBySource(e.ctx, "email", expectedExternal+":"+contactA.ID.String())
	require.NoError(t, err)
	require.Equal(t, events.KindEmailReceived, env.Kind)
}

// --- cursor edge cases ---

func TestGmailProvider_AllBystanderSweepAdvancesCursor(t *testing.T) {
	e := newGmailProviderEnv(t)
	suffix := uuid.NewString()[:8]
	me := "me-" + suffix + "@example.com"
	addrA := "a-" + suffix + "@example.com"
	contactA := e.newEmailContactWithMethod(t, "Contact A "+suffix, addrA)

	e.cleanupEvents(t, "by-"+suffix+"@example.com")
	// A is only co-Cc'd by a third party; thread not to/from me → bystander.
	msg := gmailMsg("g-by", "thr", "third-"+suffix+"@example.com",
		[]string{"other-" + suffix + "@example.com"}, []string{addrA}, nil, "S", "body", "<by-"+suffix+"@example.com>", 1700000800000)
	e.inject(newFakeMessageStore([]*gmailapi.Message{msg}), map[string]struct{}{me: {}})

	result, err := e.provider.Sync(e.ctx, syncState(me, ""), nil)
	require.NoError(t, err)
	require.Equal(t, 1, result.ItemsProcessed)
	require.Equal(t, 0, result.ItemsMatched)
	require.Equal(t, "1700000800", result.NewCursor, "processed-but-not-persisted message still advances the cursor")

	aRows, err := e.commsRepo.ListByContact(e.ctx, contactA.ID)
	require.NoError(t, err)
	require.Empty(t, aRows)
}

func TestGmailProvider_ZeroFetchedPreservesCursor(t *testing.T) {
	e := newGmailProviderEnv(t)
	suffix := uuid.NewString()[:8]
	me := "me-" + suffix + "@example.com"
	addrA := "a-" + suffix + "@example.com"
	_ = e.newEmailContactWithMethod(t, "Contact A "+suffix, addrA)

	e.inject(newFakeMessageStore(nil), map[string]struct{}{me: {}})

	// With a prior cursor, the same value is re-written verbatim.
	result, err := e.provider.Sync(e.ctx, syncState(me, "1699999999"), nil)
	require.NoError(t, err)
	require.Equal(t, "1699999999", result.NewCursor)

	// With an empty prior cursor (onboarding, nothing fetched), the
	// backfill_since epoch is written (never empty → no NULL-clear).
	result, err = e.provider.Sync(e.ctx, syncState(me, ""), nil)
	require.NoError(t, err)
	expectedBackfill := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Unix()
	require.Equal(t, fmt.Sprintf("%d", expectedBackfill), result.NewCursor)
}

func TestGmailProvider_HardFailureLeavesCursorUnchanged(t *testing.T) {
	e := newGmailProviderEnv(t)
	suffix := uuid.NewString()[:8]
	me := "me-" + suffix + "@example.com"
	addrA := "a-" + suffix + "@example.com"
	contactA := e.newEmailContactWithMethod(t, "Contact A "+suffix, addrA)

	e.cleanupEvents(t, "hf-"+suffix+"@example.com")
	msg := gmailMsg("g-hf", "thr", addrA, []string{me}, nil, nil, "S", "body", "<hf-"+suffix+"@example.com>", 1700000900000)
	store := newFakeMessageStore([]*gmailapi.Message{msg})
	store.forcedGetErr["g-hf"] = struct{}{}
	e.inject(store, map[string]struct{}{me: {}})

	priorCursor := "1699999000"
	result, err := e.provider.Sync(e.ctx, syncState(me, priorCursor), nil)
	require.Error(t, err)
	require.Equal(t, priorCursor, result.NewCursor, "hard failure must NOT advance the cursor")

	aRows, err := e.commsRepo.ListByContact(e.ctx, contactA.ID)
	require.NoError(t, err)
	require.Empty(t, aRows)

	// Clearing the failure: the re-run advances normally.
	delete(store.forcedGetErr, "g-hf")
	result, err = e.provider.Sync(e.ctx, syncState(me, priorCursor), nil)
	require.NoError(t, err)
	require.Equal(t, "1700000900", result.NewCursor)
}

// --- nil-account guard ---

func TestGmailProvider_NilAccount_Errors(t *testing.T) {
	e := newGmailProviderEnv(t)
	state := &repository.SyncState{Source: "email"}
	result, err := e.provider.Sync(e.ctx, state, nil)
	require.Error(t, err)
	require.Nil(t, result)
}

// --- cross-account provenance set-union MERGE ---

func TestGmailProvider_CrossAccountProvenanceMerge(t *testing.T) {
	e := newGmailProviderEnv(t)
	suffix := uuid.NewString()[:8]
	accountX := "x-" + suffix + "@example.com"
	accountY := "y-" + suffix + "@example.com"
	addrA := "a-" + suffix + "@example.com"
	contactA := e.newEmailContactWithMethod(t, "Contact A "+suffix, addrA)

	e.cleanupEvents(t, "xacct-"+suffix+"@example.com")
	msgID := "<xacct-" + suffix + "@example.com>"

	// Same RFC822 Message-ID observed in both mailboxes, with a different
	// per-mailbox gmail id each sweep. Both accounts share the same "me" set so
	// cross-account detection works.
	gmailX := "gmail-x-" + suffix
	gmailY := "gmail-y-" + suffix
	msgX := gmailMsg(gmailX, "thr", addrA, []string{accountX}, nil, nil, "S", "body", msgID, 1700001000000)
	msgY := gmailMsg(gmailY, "thr", addrA, []string{accountY}, nil, nil, "S", "body", msgID, 1700001000000)
	meSet := map[string]struct{}{accountX: {}, accountY: {}}

	// Sweep account X.
	e.inject(newFakeMessageStore([]*gmailapi.Message{msgX}), meSet)
	_, err := e.provider.Sync(e.ctx, syncState(accountX, ""), nil)
	require.NoError(t, err)

	// Sweep account Y.
	e.inject(newFakeMessageStore([]*gmailapi.Message{msgY}), meSet)
	_, err = e.provider.Sync(e.ctx, syncState(accountY, ""), nil)
	require.NoError(t, err)

	// Exactly one content row, provenance merged across both accounts.
	rows, err := e.commsRepo.ListByContact(e.ctx, contactA.ID)
	require.NoError(t, err)
	require.Len(t, rows, 1)

	got, err := e.commsRepo.GetMessage(e.ctx, "email", "xacct-"+suffix+"@example.com", contactA.ID)
	require.NoError(t, err)
	require.ElementsMatch(t, []string{accountX, accountY}, observedAccounts(t, got.SourceMetadata))
	gmailIDs := accountGmailIDs(t, got.SourceMetadata)
	require.Equal(t, gmailX, gmailIDs[accountX])
	require.Equal(t, gmailY, gmailIDs[accountY])
}

// --- onboarding empty-cursor backfill window (spec §3.2) --------------------

// TestGmailProvider_Onboarding_EmptyCursor_BackfillSince drives the full
// onboarding seam through Sync end-to-end: a nil SyncCursor + a backfill_since
// metadata override must (a) floor the scan query at the override epoch, (b)
// persist a qualifying message dated after the floor, (c) advance the cursor to
// the message's internalDate seconds, and (d) be idempotent on a second sweep
// with the returned cursor. Complements (does not duplicate) the resolveAfterFloor
// / computeNewCursor helper unit tests, which exercise the functions in isolation.
func TestGmailProvider_Onboarding_EmptyCursor_BackfillSince(t *testing.T) {
	e := newGmailProviderEnv(t)
	suffix := uuid.NewString()[:8]
	me := "me-" + suffix + "@example.com"
	addrA := "a-" + suffix + "@example.com"
	contactA := e.newEmailContactWithMethod(t, "Contact A "+suffix, addrA)

	ext := "onb-" + suffix + "@example.com"
	e.cleanupEvents(t, ext)

	// backfill_since override floors the scan at 2026-03-01.
	override := "2026-03-01"
	overrideEpoch := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC).Unix()
	// A message dated AFTER the floor must be scanned and persisted.
	msgEpochSecs := time.Date(2026, 3, 15, 9, 0, 0, 0, time.UTC).Unix()
	msg := gmailMsg("g-onb", "thr", addrA, []string{me}, nil, nil, "S", "body", "<"+ext+">", msgEpochSecs*1000)

	// A query-recording fetcher proves the metadata override flows into the
	// after:<epoch> floor (the query-agnostic fakeMessageStore returns the
	// message regardless of query, so we capture the query explicitly here).
	var capturedQueries []string
	e.provider.SetFetcherFactoryForTest(google.NewFakeGmailFetcherFactoryForTest(google.FakeGmailFetcherFuncs{
		ListMessageIDs: func(_ context.Context, query, pageToken string) ([]google.GmailMessageRefForTest, string, error) {
			if pageToken != "" {
				return nil, "", nil
			}
			capturedQueries = append(capturedQueries, query)
			return []google.GmailMessageRefForTest{{ID: msg.Id, ThreadID: msg.ThreadId}}, "", nil
		},
		GetMessage: func(_ context.Context, id string) (*gmailapi.Message, error) {
			if id == msg.Id {
				return msg, nil
			}
			return nil, fmt.Errorf("message %s not found", id)
		},
	}))
	e.provider.SetMeSetForTest(map[string]struct{}{me: {}})

	// Onboarding: nil cursor + backfill_since metadata override.
	state := &repository.SyncState{
		Source:    "email",
		AccountID: &me,
		Metadata:  map[string]any{"backfill_since": override},
	}

	result, err := e.provider.Sync(e.ctx, state, nil)
	require.NoError(t, err)
	require.Equal(t, 1, result.ItemsProcessed)
	require.Equal(t, 1, result.ItemsMatched)
	require.Equal(t, fmt.Sprintf("%d", msgEpochSecs), result.NewCursor, "cursor advances to the message internalDate seconds")

	// Query floored at the override epoch, not the default 2026-01-01.
	require.NotEmpty(t, capturedQueries)
	for _, q := range capturedQueries {
		require.Contains(t, q, fmt.Sprintf("after:%d", overrideEpoch), "scan floors at the backfill_since override")
	}

	// Content row + event persisted.
	rows, err := e.commsRepo.ListByContact(e.ctx, contactA.ID)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, ext, rows[0].ExternalID)
	_, err = e.eventRepo.FindEventBySource(e.ctx, "email", ext+":"+contactA.ID.String())
	require.NoError(t, err)

	// Second sweep with the returned cursor is idempotent (no duplicate row).
	state2 := &repository.SyncState{
		Source:     "email",
		AccountID:  &me,
		SyncCursor: &result.NewCursor,
		Metadata:   map[string]any{"backfill_since": override},
	}
	_, err = e.provider.Sync(e.ctx, state2, nil)
	require.NoError(t, err)
	rows, err = e.commsRepo.ListByContact(e.ctx, contactA.ID)
	require.NoError(t, err)
	require.Len(t, rows, 1, "second sweep must not add a duplicate content row")
}
