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
//   - cursor edge cases (all-bystander and empty windows advance; hard failure
//     leaves cursor unchanged);
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
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/events"
	"personal-crm/backend/internal/google"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"
	"personal-crm/backend/internal/synthetic/factory"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"
	gmailapi "google.golang.org/api/gmail/v1"
)

// gmailProviderEnv bundles a real provider + bus + repos for the sweep tests.
type gmailProviderEnv struct {
	ctx            context.Context
	database       *db.Database
	gen            *factory.Generator
	provider       *google.GmailSyncProvider
	bus            *events.Bus
	commsRepo      *repository.CommsMessageRepository
	contactRepo    *repository.ContactRepository
	contactService *service.ContactService
	identityRepo   *repository.IdentityRepository
	eventRepo      *repository.EventRepository
}

func newGmailProviderEnv(t *testing.T) *gmailProviderEnv {
	t.Helper()
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	// Per-test isolated clone: the live consumer drains a private river_job.
	ctx := context.Background()
	database, _ := newIsolatedRiverTestDB(t, ctx)

	commsRepo := repository.NewCommsMessageRepository(database.Queries)
	contactRepo := repository.NewContactRepository(database.Queries)
	contactRepo.SetPool(database.Pool)
	methodRepo := repository.NewContactMethodRepository(database.Queries)
	identityRepo := repository.NewIdentityRepository(database.Queries)
	interactionRepo := repository.NewInteractionRepository(database.Queries)
	contactTaskRepo := repository.NewContactTaskRepository(database.Queries)
	eventRepo := repository.NewEventRepository(database.Queries)
	// nil bus + nil rematch: a single-tx multi-method seed write, no River client.
	cadenceUpdater := buildCadenceUpdaterForTest(t, database)
	assertSvc, cache := buildKnowledgeDeps(t, database, nil)
	contactService := service.NewContactService(database, contactRepo, methodRepo, interactionRepo, contactTaskRepo, nil, nil,
		cadenceUpdater, assertSvc, cache, nil)

	// Live bus via the shared harness. email.* kinds enqueue an
	// email_interaction_consumer job, drained by the harness's no-op email
	// worker — these tests assert on the durable event-log row + content
	// rows, not the derived interaction.
	bus := setupTestEventBus(t, ctx, database, contactService)

	provider := google.NewGmailSyncProvider(nil, commsRepo, bus, database.Pool)

	gen, _ := migrationGenerator(t)

	return &gmailProviderEnv{
		ctx:            ctx,
		database:       database,
		gen:            gen,
		provider:       provider,
		bus:            bus,
		commsRepo:      commsRepo,
		contactRepo:    contactRepo,
		contactService: contactService,
		identityRepo:   identityRepo,
		eventRepo:      eventRepo,
	}
}

// newEmailContact seeds a namespaced contact carrying one email method and
// returns it plus that email (the value the sweep matches a fetched message's
// participants against). Cleanup hard-deletes the contact's content rows before
// the contact itself (FK-child rows must go first).
func (e *gmailProviderEnv) newEmailContact(t *testing.T) (*repository.Contact, string) {
	t.Helper()
	spec := e.gen.Contact(factory.WithEmail())
	contact := e.seedSpec(t, spec)
	return contact, spec.Email
}

// seedSpec writes a factory ContactSpec through the env's nil-bus
// ContactService.CreateContact (the sanctioned single-tx multi-method write
// path) and registers content-then-contact cleanup.
func (e *gmailProviderEnv) seedSpec(t *testing.T, spec factory.ContactSpec) *repository.Contact {
	t.Helper()
	methods := make([]service.ContactMethodInput, 0, len(spec.Methods))
	for _, m := range spec.Methods {
		methods = append(methods, service.ContactMethodInput{Type: m.Type, Value: m.Value, IsPrimary: m.IsPrimary})
	}
	contact, _, err := e.contactService.CreateContact(e.ctx, repository.CreateContactRequest{
		FullName: spec.FullName,
		Cadence:  spec.Cadence,
	}, methods)
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = e.commsRepo.HardDeleteByContact(e.ctx, contact.ID)
		_ = e.contactRepo.HardDeleteContact(e.ctx, contact.ID)
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
				if !fakeMessageMatchesQuery(m, query) {
					continue
				}
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

func fakeMessageMatchesQuery(msg *gmailapi.Message, query string) bool {
	secs := msg.InternalDate / 1000
	if after, ok := fakeQueryEpoch(query, "after:"); ok && secs < after {
		return false
	}
	if before, ok := fakeQueryEpoch(query, "before:"); ok && secs > before {
		return false
	}
	return true
}

func fakeQueryEpoch(query, prefix string) (int64, bool) {
	for _, part := range strings.Fields(query) {
		if !strings.HasPrefix(part, prefix) {
			continue
		}
		v, err := strconv.ParseInt(strings.TrimPrefix(part, prefix), 10, 64)
		return v, err == nil
	}
	return 0, false
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
	st := &repository.SyncState{
		Source:    "email",
		AccountID: &accountID,
		Metadata:  map[string]any{"backfill_since": "2023-11-01"},
	}
	if cursor != "" {
		st.SyncCursor = &cursor
	}
	return st
}

type gmailCursorForTest struct {
	Version          int      `json:"v"`
	CompletedThrough int64    `json:"completed_through"`
	BoundaryHashes   []string `json:"boundary_hashes"`
}

func decodeGmailCursor(t *testing.T, cursor string) gmailCursorForTest {
	t.Helper()
	var decoded gmailCursorForTest
	require.NoError(t, json.Unmarshal([]byte(cursor), &decoded))
	require.Equal(t, 2, decoded.Version)
	return decoded
}

func expectCursorAtLeast(t *testing.T, cursor string, minEpoch int64) gmailCursorForTest {
	t.Helper()
	decoded := decodeGmailCursor(t, cursor)
	require.GreaterOrEqual(t, decoded.CompletedThrough, minEpoch)
	return decoded
}

// --- full sweep → content rows + events ---

func TestGmailProvider_FullSweep_ContentAndEvents(t *testing.T) {
	t.Parallel()
	e := newGmailProviderEnv(t)
	prefix := e.gen.Prefix()
	me := prefix + "me@synthetic.example"
	contactA, addrA := e.newEmailContact(t)
	contactB, addrB := e.newEmailContact(t)

	inExt := prefix + "in@synthetic.example"
	outExt := prefix + "out@synthetic.example"
	inMsgID := "<" + inExt + ">"
	outMsgID := "<" + outExt + ">"
	e.cleanupEvents(t, inExt)
	e.cleanupEvents(t, outExt)

	inbound := gmailMsg("g-in", "thr-in", addrA, []string{me}, nil, nil, "Inbound subj", "hello from A", inMsgID, 1700000100000)
	outbound := gmailMsg("g-out", "thr-out", me, []string{addrB}, nil, nil, "Outbound subj", "hi to B", outMsgID, 1700000200000)
	e.inject(newFakeMessageStore([]*gmailapi.Message{inbound, outbound}), map[string]struct{}{me: {}})

	result, err := e.provider.Sync(e.ctx, syncState(me, ""), nil)
	require.NoError(t, err)
	require.Equal(t, 2, result.ItemsProcessed)
	require.Equal(t, 2, result.ItemsMatched)
	expectCursorAtLeast(t, result.NewCursor, 1700000200)

	aRows, err := e.commsRepo.ListByContact(e.ctx, contactA.ID)
	require.NoError(t, err)
	require.Len(t, aRows, 1)
	require.Equal(t, "inbound", aRows[0].Direction)
	require.Equal(t, inExt, aRows[0].ExternalID)
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

	inEnv, err := e.eventRepo.FindEventBySource(e.ctx, "email", inExt+":"+contactA.ID.String())
	require.NoError(t, err)
	require.Equal(t, events.KindEmailReceived, inEnv.Kind)
	var inPayload events.EmailEventPayload
	require.NoError(t, events.Unmarshal(inEnv, &inPayload))
	require.Equal(t, contactA.ID, inPayload.ContactID)
	require.Equal(t, "inbound", inPayload.Direction)
	require.Equal(t, "thr-in", inPayload.ThreadID)

	outEnv, err := e.eventRepo.FindEventBySource(e.ctx, "email", outExt+":"+contactB.ID.String())
	require.NoError(t, err)
	require.Equal(t, events.KindEmailSent, outEnv.Kind)
}

// --- cross-chunk seen dedup at sweep level ---

func TestGmailProvider_CrossChunkSeenDedup(t *testing.T) {
	t.Parallel()
	e := newGmailProviderEnv(t)
	prefix := e.gen.Prefix()
	me := prefix + "me@synthetic.example"

	// Many known contacts force the provider to build MORE THAN ONE OR-chunk;
	// the query-agnostic fake returns the single spanning message for every
	// chunk query, so the per-id seen set is what prevents a second body fetch.
	const n = 250
	var addrs []string
	for i := 0; i < n; i++ {
		_, addr := e.newEmailContact(t)
		addrs = append(addrs, addr)
	}

	spanExt := prefix + "span@synthetic.example"
	msgID := "<" + spanExt + ">"
	e.cleanupEvents(t, spanExt)
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
	t.Parallel()
	e := newGmailProviderEnv(t)
	prefix := e.gen.Prefix()
	me := prefix + "me@synthetic.example"
	unknown := prefix + "unknown@synthetic.example"
	contactA, addrA := e.newEmailContact(t)

	e.cleanupEvents(t, prefix+"mo@synthetic.example")
	msg := gmailMsg("g-mo", "thr", addrA, []string{me, unknown}, nil, nil, "S", "body", "<"+prefix+"mo@synthetic.example>", 1700000400000)
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
	t.Parallel()
	e := newGmailProviderEnv(t)
	prefix := e.gen.Prefix()
	me := prefix + "me@synthetic.example"
	contactA, addrA := e.newEmailContact(t)

	idemExt := prefix + "idem@synthetic.example"
	e.cleanupEvents(t, idemExt)
	msg := gmailMsg("g-idem", "thr", addrA, []string{me}, nil, nil, "S", "body", "<"+idemExt+">", 1700000500000)
	e.inject(newFakeMessageStore([]*gmailapi.Message{msg}), map[string]struct{}{me: {}})

	_, err := e.provider.Sync(e.ctx, syncState(me, ""), nil)
	require.NoError(t, err)
	_, err = e.provider.Sync(e.ctx, syncState(me, ""), nil)
	require.NoError(t, err)

	aRows, err := e.commsRepo.ListByContact(e.ctx, contactA.ID)
	require.NoError(t, err)
	require.Len(t, aRows, 1, "second sweep must not add a duplicate content row")

	got, err := e.commsRepo.GetMessage(e.ctx, "email", idemExt, contactA.ID)
	require.NoError(t, err)
	require.Equal(t, []string{me}, observedAccounts(t, got.SourceMetadata))
}

// --- publish-before-mutate ---

func TestGmailProvider_PublishBeforeMutate_RollsBack(t *testing.T) {
	t.Parallel()
	e := newGmailProviderEnv(t)
	prefix := e.gen.Prefix()
	me := prefix + "me@synthetic.example"
	contactA, addrA := e.newEmailContact(t)

	e.cleanupEvents(t, prefix+"pbm@synthetic.example")
	msg := gmailMsg("g-pbm", "thr", addrA, []string{me}, nil, nil, "S", "body", "<"+prefix+"pbm@synthetic.example>", 1700000600000)
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
	t.Parallel()
	e := newGmailProviderEnv(t)
	prefix := e.gen.Prefix()
	me := prefix + "me@synthetic.example"
	contactA, addrA := e.newEmailContact(t)

	gmailID := "g-nomsgid-" + prefix
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
	t.Parallel()
	e := newGmailProviderEnv(t)
	prefix := e.gen.Prefix()
	me := prefix + "me@synthetic.example"
	contactA, addrA := e.newEmailContact(t)

	e.cleanupEvents(t, prefix+"by@synthetic.example")
	// A is only co-Cc'd by a third party; thread not to/from me → bystander.
	msg := gmailMsg("g-by", "thr", prefix+"third@synthetic.example",
		[]string{prefix + "other@synthetic.example"}, []string{addrA}, nil, "S", "body", "<"+prefix+"by@synthetic.example>", 1700000800000)
	e.inject(newFakeMessageStore([]*gmailapi.Message{msg}), map[string]struct{}{me: {}})

	result, err := e.provider.Sync(e.ctx, syncState(me, ""), nil)
	require.NoError(t, err)
	require.Equal(t, 1, result.ItemsProcessed)
	require.Equal(t, 0, result.ItemsMatched)
	expectCursorAtLeast(t, result.NewCursor, 1700000800)

	aRows, err := e.commsRepo.ListByContact(e.ctx, contactA.ID)
	require.NoError(t, err)
	require.Empty(t, aRows)
}

func TestGmailProvider_ZeroFetchedAdvancesCompletedWindows(t *testing.T) {
	t.Parallel()
	e := newGmailProviderEnv(t)
	prefix := e.gen.Prefix()
	me := prefix + "me@synthetic.example"
	_, _ = e.newEmailContact(t)

	e.inject(newFakeMessageStore(nil), map[string]struct{}{me: {}})

	// Empty but fully scanned windows are proven complete and advance.
	priorEpoch := int64(1699999999)
	result, err := e.provider.Sync(e.ctx, syncState(me, fmt.Sprintf("%d", priorEpoch)), nil)
	require.NoError(t, err)
	decoded := decodeGmailCursor(t, result.NewCursor)
	require.Greater(t, decoded.CompletedThrough, priorEpoch)

	// With an empty prior cursor, the metadata backfill floor is upgraded into
	// the v2 cursor and advanced through completed empty windows.
	result, err = e.provider.Sync(e.ctx, syncState(me, ""), nil)
	require.NoError(t, err)
	expectedBackfill := time.Date(2023, 11, 1, 0, 0, 0, 0, time.UTC).Unix()
	decoded = decodeGmailCursor(t, result.NewCursor)
	require.Greater(t, decoded.CompletedThrough, expectedBackfill)
}

func TestGmailProvider_CatchUpScansSuccessiveWindowsInOneRun(t *testing.T) {
	t.Parallel()
	e := newGmailProviderEnv(t)
	prefix := e.gen.Prefix()
	me := prefix + "me@synthetic.example"
	contactA, addrA := e.newEmailContact(t)

	start := accelerated.GetCurrentTime().UTC().Add(-28 * 24 * time.Hour).Truncate(time.Second).Unix()
	msgEpochs := []int64{
		start + int64((1 * time.Hour).Seconds()),
		start + int64((8 * 24 * time.Hour).Seconds()),
		start + int64((15 * 24 * time.Hour).Seconds()),
	}
	var messages []*gmailapi.Message
	for i, epoch := range msgEpochs {
		ext := fmt.Sprintf("%scatchup-%d@synthetic.example", prefix, i)
		e.cleanupEvents(t, ext)
		messages = append(messages, gmailMsg(
			fmt.Sprintf("g-catchup-%d", i),
			"thr",
			addrA,
			[]string{me},
			nil,
			nil,
			"S",
			"body",
			"<"+ext+">",
			epoch*1000,
		))
	}

	tooNewExt := prefix + "catchup-new@synthetic.example"
	e.cleanupEvents(t, tooNewExt)
	tooNewEpoch := accelerated.GetCurrentTime().UTC().Add(-1 * time.Minute).Unix()
	messages = append(messages, gmailMsg("g-catchup-new", "thr", addrA, []string{me}, nil, nil, "S", "body", "<"+tooNewExt+">", tooNewEpoch*1000))

	e.inject(newFakeMessageStore(messages), map[string]struct{}{me: {}})

	beforeSafe := accelerated.GetCurrentTime().UTC().Add(-10 * time.Minute).Unix()
	result, err := e.provider.Sync(e.ctx, syncState(me, fmt.Sprintf("%d", start)), nil)
	afterSafe := accelerated.GetCurrentTime().UTC().Add(-10 * time.Minute).Unix()
	require.NoError(t, err)
	require.Equal(t, 3, result.ItemsProcessed)
	require.Equal(t, 3, result.ItemsMatched)
	cursor := decodeGmailCursor(t, result.NewCursor)
	require.GreaterOrEqual(t, cursor.CompletedThrough, beforeSafe)
	require.LessOrEqual(t, cursor.CompletedThrough, afterSafe)

	rows, err := e.commsRepo.ListByContact(e.ctx, contactA.ID)
	require.NoError(t, err)
	require.Len(t, rows, 3, "all eligible messages below the final cursor are inserted")
	for _, row := range rows {
		require.NotEqual(t, tooNewExt, row.ExternalID, "message inside the safety lag must not be ingested yet")
	}
}

func TestGmailProvider_BoundaryOverlapSkipsOnlySeenMessageIDs(t *testing.T) {
	t.Parallel()
	e := newGmailProviderEnv(t)
	prefix := e.gen.Prefix()
	me := prefix + "me@synthetic.example"
	contactA, addrA := e.newEmailContact(t)

	start := accelerated.GetCurrentTime().UTC().Add(-21 * 24 * time.Hour).Truncate(time.Second).Unix()
	boundaryEpoch := start + int64((7 * 24 * time.Hour).Seconds())
	ext := prefix + "boundary@synthetic.example"
	e.cleanupEvents(t, ext)
	msg := gmailMsg("g-boundary-"+prefix, "thr", addrA, []string{me}, nil, nil, "S", "body", "<"+ext+">", boundaryEpoch*1000)
	store := newFakeMessageStore([]*gmailapi.Message{msg})
	e.inject(store, map[string]struct{}{me: {}})

	result, err := e.provider.Sync(e.ctx, syncState(me, fmt.Sprintf("%d", start)), nil)
	require.NoError(t, err)
	require.Equal(t, 1, result.ItemsProcessed)
	require.Equal(t, 1, result.ItemsMatched)
	require.Equal(t, 1, store.getCalls[msg.Id], "boundary replay should be skipped by Gmail message id hash")

	rows, err := e.commsRepo.ListByContact(e.ctx, contactA.ID)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, ext, rows[0].ExternalID)
}

func TestGmailProvider_HardFailureLeavesCursorUnchanged(t *testing.T) {
	t.Parallel()
	e := newGmailProviderEnv(t)
	prefix := e.gen.Prefix()
	me := prefix + "me@synthetic.example"
	contactA, addrA := e.newEmailContact(t)

	e.cleanupEvents(t, prefix+"hf@synthetic.example")
	msg := gmailMsg("g-hf", "thr", addrA, []string{me}, nil, nil, "S", "body", "<"+prefix+"hf@synthetic.example>", 1700000900000)
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
	expectCursorAtLeast(t, result.NewCursor, 1700000900)
}

// --- nil-account guard ---

func TestGmailProvider_NilAccount_Errors(t *testing.T) {
	t.Parallel()
	e := newGmailProviderEnv(t)
	state := &repository.SyncState{Source: "email"}
	result, err := e.provider.Sync(e.ctx, state, nil)
	require.Error(t, err)
	require.Nil(t, result)
}

// --- cross-account provenance set-union MERGE ---

func TestGmailProvider_CrossAccountProvenanceMerge(t *testing.T) {
	t.Parallel()
	e := newGmailProviderEnv(t)
	prefix := e.gen.Prefix()
	accountX := prefix + "x@synthetic.example"
	accountY := prefix + "y@synthetic.example"
	contactA, addrA := e.newEmailContact(t)

	xacctExt := prefix + "xacct@synthetic.example"
	e.cleanupEvents(t, xacctExt)
	msgID := "<" + xacctExt + ">"

	// Same RFC822 Message-ID observed in both mailboxes, with a different
	// per-mailbox gmail id each sweep. Both accounts share the same "me" set so
	// cross-account detection works.
	gmailX := "gmail-x-" + prefix
	gmailY := "gmail-y-" + prefix
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

	got, err := e.commsRepo.GetMessage(e.ctx, "email", xacctExt, contactA.ID)
	require.NoError(t, err)
	require.ElementsMatch(t, []string{accountX, accountY}, observedAccounts(t, got.SourceMetadata))
	gmailIDs := accountGmailIDs(t, got.SourceMetadata)
	require.Equal(t, gmailX, gmailIDs[accountX])
	require.Equal(t, gmailY, gmailIDs[accountY])
}

// --- onboarding empty-cursor backfill window (spec §3.2) --------------------

// TestGmailProvider_Onboarding_EmptyCursor_BackfillSince drives the full
// onboarding seam through Sync end-to-end: a nil SyncCursor + a backfill_since
// metadata override must (a) start the proven window at the override epoch, (b)
// persist a qualifying message dated after the floor, (c) advance the cursor to
// a completed window, and (d) be idempotent on a second sweep with the returned
// cursor.
func TestGmailProvider_Onboarding_EmptyCursor_BackfillSince(t *testing.T) {
	t.Parallel()
	e := newGmailProviderEnv(t)
	prefix := e.gen.Prefix()
	me := prefix + "me@synthetic.example"
	contactA, addrA := e.newEmailContact(t)

	ext := prefix + "onb@synthetic.example"
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
	expectCursorAtLeast(t, result.NewCursor, msgEpochSecs)

	// The first query is widened around the override floor; exact inclusion is
	// enforced with internalDate after fetching.
	require.NotEmpty(t, capturedQueries)
	firstAfter, ok := fakeQueryEpoch(capturedQueries[0], "after:")
	require.True(t, ok, "first query should include after:")
	require.Equal(t, overrideEpoch-int64((48*time.Hour)/time.Second), firstAfter)

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
