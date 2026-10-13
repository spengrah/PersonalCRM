package tests

import (
	"context"
	"testing"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/consumer"
	"personal-crm/backend/internal/contacttask"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/events"
	"personal-crm/backend/internal/google"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"
	"personal-crm/backend/internal/synthetic/factory"
	"personal-crm/backend/internal/todoist"
	"personal-crm/backend/tests/testsupport"

	"github.com/google/uuid"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/stretchr/testify/require"
	calendarapi "google.golang.org/api/calendar/v3"
	gmailapi "google.golang.org/api/gmail/v1"
)

// These tests observe interaction-node identity (IXN-015, KNW-036, ING-045) at
// the sync runs: CalendarSyncProvider.Sync with Google faked at its client
// boundary, the calendar rematch, GmailSyncProvider.Sync and
// CadenceSyncProvider.Sync. Each recording runs through the live
// InteractionRecorder (or the live email consumer), so the node an observation
// resolves to is read from the row the consumer wrote.

// nodeOfInteraction returns the interaction node an observation resolved to,
// and asserts it is a live registry node of type 'interaction'.
func nodeOfInteraction(t *testing.T, nodes *repository.NodeRepository, ix *repository.Interaction) uuid.UUID {
	t.Helper()
	require.NotNil(t, ix.InteractionNodeID, "observation %s (source %s) must resolve to an interaction node", ix.ID, ix.Source)
	node, err := nodes.GetNode(t.Context(), *ix.InteractionNodeID)
	require.NoError(t, err)
	require.Equal(t, "interaction", node.Type)
	return node.ID
}

type nodeSyncEnv struct {
	ctx             context.Context
	database        *db.Database
	gen             *factory.Generator
	bus             *events.Bus
	contactService  *service.ContactService
	contactRepo     *repository.ContactRepository
	interactionRepo *repository.InteractionRepository
	calendarRepo    *repository.CalendarEventRepository
	nodes           *repository.NodeRepository
	cadenceUpdater  *consumer.CadenceUpdater
}

// newNodeSyncEnv wires a live bus with the InteractionRecorder worker on an
// isolated database clone.
func newNodeSyncEnv(t *testing.T) *nodeSyncEnv {
	t.Helper()
	e, start := newUnstartedNodeSyncEnv(t)
	start(e.ctx)
	return e
}

// newUnstartedNodeSyncEnv is newNodeSyncEnv with the workers not started yet.
func newUnstartedNodeSyncEnv(t *testing.T) (*nodeSyncEnv, func(ctx context.Context)) {
	t.Helper()
	testsupport.RequireDatabase(t)
	ctx := t.Context()
	database, _ := newIsolatedRiverTestDB(t, ctx)

	contactRepo := repository.NewContactRepository(database.Queries)
	contactRepo.SetPool(database.Pool)
	methodRepo := repository.NewContactMethodRepository(database.Queries)
	interactionRepo := repository.NewInteractionRepository(database.Queries)
	taskRepo := repository.NewContactTaskRepository(database.Queries)
	cadenceUpdater := buildCadenceUpdaterForTest(t, database)
	assertSvc, cache := buildKnowledgeDeps(t, database, nil)
	contactService := service.NewContactService(database, contactRepo, methodRepo, interactionRepo, taskRepo, nil, nil,
		cadenceUpdater, assertSvc, cache, nil)
	bus, start := newUnstartedRecorderAndDeclineBus(t, database, contactService, contactRepo, interactionRepo, cadenceUpdater)
	gen, _ := migrationGenerator(t)

	return &nodeSyncEnv{
		ctx:             ctx,
		database:        database,
		gen:             gen,
		bus:             bus,
		contactService:  contactService,
		contactRepo:     contactRepo,
		interactionRepo: interactionRepo,
		calendarRepo:    repository.NewCalendarEventRepository(database.Queries),
		nodes:           repository.NewNodeRepository(database.Queries),
		cadenceUpdater:  cadenceUpdater,
	}, start
}

// newUnstartedRecorderAndDeclineBus is setupTestEventBus plus the live calendar
// decline handler (so a declined event removes the observation as it does in
// production), with the workers not started. A test can enqueue jobs and release
// them together; the returned func starts the workers.
func newUnstartedRecorderAndDeclineBus(
	t *testing.T,
	database *db.Database,
	contactService *service.ContactService,
	contactRepo *repository.ContactRepository,
	interactionRepo *repository.InteractionRepository,
	cadenceUpdater *consumer.CadenceUpdater,
) (*events.Bus, func(ctx context.Context)) {
	t.Helper()
	workers := river.NewWorkers()
	recorderShim := &deferredRecorderWorker{}
	declineShim := &deferredDeclineWorker{}
	river.AddWorker(workers, recorderShim)
	river.AddWorker(workers, declineShim)
	river.AddWorker(workers, &cadenceUpdaterNoopWorker{})
	river.AddWorker(workers, &followUpManagerNoopWorker{})
	river.AddWorker(workers, &emailInteractionNoopWorker{})
	river.AddWorker(workers, &knowledgeCacheNoopWorker{})
	client, err := river.NewClient(riverpgxv5.New(database.Pool), &river.Config{
		Queues:   map[string]river.QueueConfig{river.QueueDefault: {MaxWorkers: 4}},
		Workers:  workers,
		TestOnly: true,
	})
	require.NoError(t, err)
	bus := events.NewBus(database.Pool, client, repository.NewEventRepository(database.Queries))
	recorder := consumer.NewInteractionRecorder(contactService, repository.NewStagingProcessorRegistry(nil), bus, cadenceUpdater, nil, repository.NewCalendarEventRepository(database.Queries))
	recorderShim.real = consumer.NewInteractionRecorderWorker(bus, database.Pool, recorder, nil)
	declineShim.real = consumer.NewCalendarDeclineHandlerWorker(bus, database.Pool, consumer.NewCalendarDeclineHandler(interactionRepo, contactRepo, config.TestConfig().Watchdog))
	start := func(ctx context.Context) {
		require.NoError(t, client.Start(ctx))
		t.Cleanup(func() {
			stopCtx, stopCancel := context.WithTimeout(context.WithoutCancel(t.Context()), 10*time.Second)
			defer stopCancel()
			_ = client.Stop(stopCtx)
		})
	}
	return bus, start
}

// newContactWithEmail seeds a contact carrying one email method and returns it
// with the spec (the email calendar attendees and mail participants match on).
func (e *nodeSyncEnv) newContactWithEmail(t *testing.T) (*repository.Contact, factory.ContactSpec) {
	t.Helper()
	spec := e.gen.Contact(factory.WithEmail())
	methods := make([]service.ContactMethodInput, 0, len(spec.Methods))
	for _, m := range spec.Methods {
		methods = append(methods, service.ContactMethodInput{Type: m.Type, Value: m.Value, IsPrimary: m.IsPrimary})
	}
	contact, _, err := e.contactService.CreateContact(e.ctx, repository.CreateContactRequest{FullName: spec.FullName}, methods)
	require.NoError(t, err)
	return contact, spec
}

// syncCalendar runs one CalendarSyncProvider.Sync whose faked Google client
// returns the given events. A non-empty cursor makes it an incremental sync.
func (e *nodeSyncEnv) syncCalendar(t *testing.T, accountID, cursor string, evs ...*calendarapi.Event) {
	t.Helper()
	identity := service.NewIdentityService(repository.NewIdentityRepository(e.database.Queries))
	externals := repository.NewExternalContactRepository(e.database.Queries)
	provider := google.NewCalendarSyncProvider(nil, e.calendarRepo, e.contactRepo, identity, externals, e.bus, e.database.Pool)
	provider.SetFetcherFactoryForTest(google.NewFakeCalendarFetcherFactoryForTest(google.FakeCalendarFetcherFuncs{
		ListEvents: func(_ context.Context, _ string, _ google.CalendarListOpts) ([]*calendarapi.Event, string, string, error) {
			return evs, "", "node-test-cursor", nil
		},
	}))
	state := &repository.SyncState{Source: repository.InteractionSourceGCal, AccountID: &accountID}
	if cursor != "" {
		state.SyncCursor = &cursor
	}
	_, err := provider.Sync(e.ctx, state, nil)
	require.NoError(t, err)
}

// gcalObservation waits for the contact's gcal observation of the stored event.
func (e *nodeSyncEnv) gcalObservation(t *testing.T, ev factory.GCalEventSpec, contactID uuid.UUID) *repository.Interaction {
	t.Helper()
	stored, err := e.calendarRepo.GetByGcalID(e.ctx, ev.GcalEventID, "primary", ev.AccountID)
	require.NoError(t, err)
	return waitForInteractionBySourceRef(t, e.ctx, e.interactionRepo, contactID, repository.InteractionSourceGCal, stored.ID.String(), defaultInteractionWaitTimeout)
}

// withAttendee returns the event with one more accepted attendee.
func withAttendee(ev factory.GCalEventSpec, email string) factory.GCalEventSpec {
	ev.Event.Attendees = append(ev.Event.Attendees, &calendarapi.EventAttendee{Email: email, ResponseStatus: "accepted"})
	return ev
}

// spec: IXN-015.shared-per-source-object
func TestInteractionNode_CalendarEventAttendeesShareNode(t *testing.T) {
	t.Parallel()
	e := newNodeSyncEnv(t)
	contactA, specA := e.newContactWithEmail(t)
	contactB, specB := e.newContactWithEmail(t)

	ev := withAttendee(e.gen.GCalEvent(specA, factory.MatchSeeded), specB.Email)
	e.syncCalendar(t, ev.AccountID, "", ev.Event)

	nodeA := nodeOfInteraction(t, e.nodes, e.gcalObservation(t, ev, contactA.ID))
	nodeB := nodeOfInteraction(t, e.nodes, e.gcalObservation(t, ev, contactB.ID))
	require.Equal(t, nodeA, nodeB, "both attendees' observations of one event share its node")
}

// spec: IXN-015.overlapping-events-stay-two
func TestInteractionNode_OverlappingCalendarEventsStayTwoNodes(t *testing.T) {
	t.Parallel()
	e := newNodeSyncEnv(t)
	contactA, specA := e.newContactWithEmail(t)
	contactB, specB := e.newContactWithEmail(t)

	shape := factory.GCalEventShape{StartOffset: -3 * time.Hour, Duration: 90 * time.Minute}
	first := withAttendee(e.gen.GCalEventWithShape(specA, factory.MatchSeeded, shape), specB.Email)
	second := withAttendee(e.gen.GCalEventWithShape(specA, factory.MatchSeeded, shape), specB.Email)
	e.syncCalendar(t, first.AccountID, "", first.Event, second.Event)

	for _, contactID := range []uuid.UUID{contactA.ID, contactB.ID} {
		firstNode := nodeOfInteraction(t, e.nodes, e.gcalObservation(t, first, contactID))
		secondNode := nodeOfInteraction(t, e.nodes, e.gcalObservation(t, second, contactID))
		require.NotEqual(t, firstNode, secondNode, "overlapping events with the same attendees stay two interactions (IXN-009)")
	}
}

// A declined event that is accepted again is a new stored event row, and its
// new observation resolves to the node of the observations it had before.
// spec: ING-045.declined-accepted-again
func TestInteractionNode_CalendarDeclinedThenAcceptedKeepsNode(t *testing.T) {
	t.Parallel()
	e := newNodeSyncEnv(t)
	contact, spec := e.newContactWithEmail(t)

	ev := e.gen.GCalEvent(spec, factory.MatchSeeded)
	e.syncCalendar(t, ev.AccountID, "", ev.Event)
	before := e.gcalObservation(t, ev, contact.ID)
	beforeNode := nodeOfInteraction(t, e.nodes, before)

	// Declined: the sync deletes the stored event and publishes calendar.declined,
	// and the decline handler soft-deletes the observation.
	e.syncCalendar(t, ev.AccountID, "node-test-cursor", cancelledStubFor(ev.GcalEventID))
	waitForInteractionGone(t, e, contact.ID, before.SourceRef)

	// Accepted again.
	e.syncCalendar(t, ev.AccountID, "", ev.Event)
	after := e.gcalObservation(t, ev, contact.ID)
	require.NotEqual(t, before.ID, after.ID, "the accepted-again observation is a new row")
	require.Equal(t, beforeNode, nodeOfInteraction(t, e.nodes, after))
}

func waitForInteractionGone(t *testing.T, e *nodeSyncEnv, contactID uuid.UUID, sourceRef *string) {
	t.Helper()
	require.NotNil(t, sourceRef)
	deadline := accelerated.GetCurrentTime().Add(defaultInteractionWaitTimeout)
	for accelerated.GetCurrentTime().Before(deadline) {
		if _, err := e.interactionRepo.FindBySourceRef(e.ctx, contactID, repository.InteractionSourceGCal, *sourceRef); err != nil {
			require.ErrorIs(t, err, db.ErrNotFound)
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for the declined observation %s to go", *sourceRef)
}

// A calendar rematch that matches one more attendee of an already recorded
// event resolves the new attendee's observation to the event's node.
// spec: ING-045.calendar-rematch-shares-node
func TestInteractionNode_CalendarRematchAddedAttendeeSharesNode(t *testing.T) {
	t.Parallel()
	env := setupRematchEnv(t)
	nodes := repository.NewNodeRepository(env.database.Queries)
	contactA := env.newContact(t)
	contactB := env.newContact(t)

	accountID := "rematch-node-" + uuid.NewString()
	emailA := "node-a-" + uuid.NewString()[:8] + "@example.com"
	emailB := "node-b-" + uuid.NewString()[:8] + "@example.com"
	t.Cleanup(func() { _ = env.calendarRepo.DeleteEventsByAccount(env.ctx, accountID) })

	title := "Rematch node meeting"
	resp := "accepted"
	end := accelerated.GetCurrentTime().Add(-2 * time.Hour)
	event, err := env.calendarRepo.Upsert(env.ctx, repository.UpsertCalendarEventRequest{
		GcalEventID:     "rematch-node-event-" + uuid.NewString(),
		GcalCalendarID:  "primary",
		GoogleAccountID: accountID,
		Title:           &title,
		StartTime:       end.Add(-time.Hour),
		EndTime:         end,
		Status:          "confirmed",
		UserResponse:    &resp,
		Attendees: []repository.Attendee{
			{Email: emailA, DisplayName: "Attendee A"},
			{Email: emailB, DisplayName: "Attendee B"},
		},
		MatchedContactIDs: []uuid.UUID{},
		SyncedAt:          accelerated.GetCurrentTime(),
	})
	require.NoError(t, err)
	ref := event.ID.String()

	jobA := env.rematchSvc.StartRematchForContact(contactA.ID, []service.Method{{Type: "email", Value: emailA}})
	require.Equal(t, service.JobStatusCompleted, waitForRematchJob(t, env.rematchSvc, jobA).Status)
	first := waitForInteractionBySourceRef(t, env.ctx, env.interactionRepo, contactA.ID, repository.InteractionSourceGCal, ref, defaultInteractionWaitTimeout)
	firstNode := nodeOfInteraction(t, nodes, first)

	// The second attendee is matched later, after the event was recorded.
	jobB := env.rematchSvc.StartRematchForContact(contactB.ID, []service.Method{{Type: "email", Value: emailB}})
	require.Equal(t, service.JobStatusCompleted, waitForRematchJob(t, env.rematchSvc, jobB).Status)
	second := waitForInteractionBySourceRef(t, env.ctx, env.interactionRepo, contactB.ID, repository.InteractionSourceGCal, ref, defaultInteractionWaitTimeout)
	require.Equal(t, firstNode, nodeOfInteraction(t, nodes, second))
}

// Several attendees' observations of one event, recorded at the same moment,
// all resolve to one node and every recording succeeds.
// The recorder jobs are held on the event's row lock until released together.
// spec: IXN-015.shared-per-source-object
func TestInteractionNode_CalendarAttendeesRecordedConcurrentlyShareNode(t *testing.T) {
	t.Parallel()
	e, start := newUnstartedNodeSyncEnv(t)

	const attendees = 6
	contacts := make([]*repository.Contact, attendees)
	specs := make([]factory.ContactSpec, attendees)
	for i := range contacts {
		contacts[i], specs[i] = e.newContactWithEmail(t)
	}
	ev := e.gen.GCalEvent(specs[0], factory.MatchSeeded)
	for _, spec := range specs[1:] {
		ev = withAttendee(ev, spec.Email)
	}

	// The sync stores the event and enqueues one calendar.attended job per
	// attendee; no worker is running yet.
	e.syncCalendar(t, ev.AccountID, "", ev.Event)
	stored, err := e.calendarRepo.GetByGcalID(e.ctx, ev.GcalEventID, "primary", ev.AccountID)
	require.NoError(t, err)
	require.Len(t, stored.MatchedContactIDs, attendees)

	// Hold the event row so every recorder blocks on its lock, then release them
	// together into the observation step.
	tx, err := e.database.Pool.Begin(e.ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(context.WithoutCancel(e.ctx)) }()
	_, err = e.calendarRepo.TestGetByIDForUpdateNoWaitTx(e.ctx, tx, stored.ID)
	require.NoError(t, err)
	start(e.ctx)
	time.Sleep(500 * time.Millisecond)
	require.NoError(t, tx.Commit(e.ctx))

	var first uuid.UUID
	for i, contact := range contacts {
		node := nodeOfInteraction(t, e.nodes, e.gcalObservation(t, ev, contact.ID))
		if i == 0 {
			first = node
		}
		require.Equal(t, first, node)
	}
}

// nodeMailEnv runs GmailSyncProvider.Sync with Gmail faked at its client
// boundary, against the live email interaction consumer.
type nodeMailEnv struct {
	*emailEnv
	provider *google.GmailSyncProvider
	nodes    *repository.NodeRepository
	me       string
}

func newNodeMailEnv(t *testing.T) *nodeMailEnv {
	t.Helper()
	e := newEmailEnv(t, t.Context())
	return &nodeMailEnv{
		emailEnv: e,
		provider: google.NewGmailSyncProvider(nil, e.commsRepo, e.bus, e.database.Pool),
		nodes:    repository.NewNodeRepository(e.database.Queries),
		me:       e.gen.Prefix() + "me@synthetic.example",
	}
}

// newMailContact seeds a contact carrying one email address and returns it with
// the address.
func (e *nodeMailEnv) newMailContact(t *testing.T) (*repository.Contact, string) {
	t.Helper()
	spec := e.gen.Contact(factory.WithEmail())
	methods := make([]service.ContactMethodInput, 0, len(spec.Methods))
	for _, m := range spec.Methods {
		methods = append(methods, service.ContactMethodInput{Type: m.Type, Value: m.Value, IsPrimary: m.IsPrimary})
	}
	contact, _, err := e.contactService.CreateContact(e.ctx, repository.CreateContactRequest{FullName: spec.FullName}, methods)
	require.NoError(t, err)
	return contact, spec.Email
}

// sync runs one full-sweep Gmail sync over the given messages.
func (e *nodeMailEnv) sync(t *testing.T, messages ...*gmailapi.Message) {
	t.Helper()
	e.provider.SetFetcherFactoryForTest(google.NewFakeGmailFetcherFactoryForTest(newFakeMessageStore(messages).fetcherFuncs()))
	e.provider.SetMeSetForTest(map[string]struct{}{e.me: {}})
	_, err := e.provider.Sync(e.ctx, syncState(e.me, ""), nil)
	require.NoError(t, err)
}

// observation waits for the contact's email observation of the thread on the
// local day of sentAt.
func (e *nodeMailEnv) observation(t *testing.T, contactID uuid.UUID, thread string, sentAt time.Time) *repository.Interaction {
	t.Helper()
	return waitForInteractionBySourceRef(t, e.ctx, e.interactionRepo, contactID, repository.InteractionSourceEmail,
		sourceRefFor(contactID, thread, sentAt), defaultInteractionWaitTimeout)
}

// mail builds an inbound message (from the contact's address to the account).
func (e *nodeMailEnv) mail(id, thread, from string, to []string, sentAt time.Time) *gmailapi.Message {
	return gmailMsg(id, thread, from, to, nil, nil, "subject "+id, "body", "<"+e.gen.Prefix()+id+"@synthetic.example>", sentAt.UnixMilli())
}

var nodeMailDay = time.Date(2023, 11, 15, 12, 0, 0, 0, time.Local)

// Two contacts' observations of one thread on one day share a node; the same
// thread on another day is another node.
// spec: IXN-015.email-thread-day
func TestInteractionNode_EmailThreadDayBucketsShareNode(t *testing.T) {
	t.Parallel()
	e := newNodeMailEnv(t)
	contactA, addrA := e.newMailContact(t)
	contactB, addrB := e.newMailContact(t)
	day := nodeMailDay
	prev := day.AddDate(0, 0, -1)

	e.sync(t,
		e.mail("a-day", "thr-buckets", addrA, []string{e.me}, day),
		e.mail("b-day", "thr-buckets", addrB, []string{e.me}, day.Add(time.Hour)),
		e.mail("a-prev", "thr-buckets", addrA, []string{e.me}, prev),
	)

	aDay := nodeOfInteraction(t, e.nodes, e.observation(t, contactA.ID, "thr-buckets", day))
	bDay := nodeOfInteraction(t, e.nodes, e.observation(t, contactB.ID, "thr-buckets", day))
	aPrev := nodeOfInteraction(t, e.nodes, e.observation(t, contactA.ID, "thr-buckets", prev))
	require.Equal(t, aDay, bDay, "two contacts on one thread and day share a node")
	require.NotEqual(t, aDay, aPrev, "the same thread on another day is another node")
}

// A thread gaining a message on the same day, by extension and by promotion to
// mutual, keeps the node of its observation.
// spec: ING-045.email-gaining-message-keeps-node
func TestInteractionNode_EmailThreadGainingMessageKeepsNode(t *testing.T) {
	t.Parallel()
	e := newNodeMailEnv(t)
	contactA, addrA := e.newMailContact(t)
	day := nodeMailDay

	first := e.mail("g-1", "thr-gain", addrA, []string{e.me}, day)
	e.sync(t, first)
	before := e.observation(t, contactA.ID, "thr-gain", day)
	beforeNode := nodeOfInteraction(t, e.nodes, before)

	// A later message in the same direction extends it.
	later := e.mail("g-2", "thr-gain", addrA, []string{e.me}, day.Add(time.Hour))
	e.sync(t, first, later)
	e.waitForCommsProcessed(t, e.gen.Prefix()+"g-2@synthetic.example", contactA.ID, defaultInteractionWaitTimeout)
	extended := e.observation(t, contactA.ID, "thr-gain", day)
	require.Equal(t, before.ID, extended.ID)
	require.Equal(t, beforeNode, nodeOfInteraction(t, e.nodes, extended))

	// A reply in the other direction promotes it to mutual.
	reply := e.mail("g-3", "thr-gain", e.me, []string{addrA}, day.Add(2*time.Hour))
	e.sync(t, first, later, reply)
	e.waitForCommsProcessed(t, e.gen.Prefix()+"g-3@synthetic.example", contactA.ID, defaultInteractionWaitTimeout)
	promoted := e.observation(t, contactA.ID, "thr-gain", day)
	require.Equal(t, repository.InteractionDirectionMutual, promoted.Direction)
	require.Equal(t, before.ID, promoted.ID)
	require.Equal(t, beforeNode, nodeOfInteraction(t, e.nodes, promoted))
}

// After a contact merge, the moved observation keeps its node, and mail the
// surviving contact then gets on the same thread and day writes a new row that
// resolves to that node.
// spec: ING-045.after-merge-new-row-same-node, IXN-015.assigned-node-kept
func TestInteractionNode_EmailAfterContactMergeResolvesToMovedRowNode(t *testing.T) {
	t.Parallel()
	e := newNodeMailEnv(t)
	source, sourceAddr := e.newMailContact(t)
	target, _ := e.newMailContact(t)
	day := nodeMailDay

	first := e.mail("m-1", "thr-merge", sourceAddr, []string{e.me}, day)
	e.sync(t, first)
	movedBefore := e.observation(t, source.ID, "thr-merge", day)
	nodeBefore := nodeOfInteraction(t, e.nodes, movedBefore)

	_, err := e.contactService.MergeContacts(e.ctx, service.MergeContactsRequest{SourceContactID: source.ID, TargetContactID: target.ID})
	require.NoError(t, err)
	moved, err := e.interactionRepo.FindBySourceRef(e.ctx, target.ID, repository.InteractionSourceEmail, *movedBefore.SourceRef)
	require.NoError(t, err)
	require.Equal(t, nodeBefore, nodeOfInteraction(t, e.nodes, moved), "the moved observation keeps its node")

	// The source's address now belongs to the surviving contact.
	second := e.mail("m-2", "thr-merge", sourceAddr, []string{e.me}, day.Add(time.Hour))
	e.sync(t, first, second)
	fresh := e.observation(t, target.ID, "thr-merge", day)
	require.NotEqual(t, moved.ID, fresh.ID, "the surviving contact's mail writes a new row")
	require.Equal(t, nodeBefore, nodeOfInteraction(t, e.nodes, fresh))
}

type nodeTestTodoistOAuth struct{}

func (nodeTestTodoistOAuth) GetAccessToken(_ context.Context, _ string) (string, error) {
	return "node-test-token", nil
}
func (nodeTestTodoistOAuth) HasAnyAccount(_ context.Context) bool { return true }

// nodeTestTodoistClient is Todoist faked at its client boundary: the incremental
// sync returns the configured items; item_add commands are finalized.
type nodeTestTodoistClient struct{ items []todoist.SyncItem }

func (nodeTestTodoistClient) QuickAdd(_ context.Context, _ string, _ string) (*todoist.QuickAddTask, error) {
	return &todoist.QuickAddTask{}, nil
}

func (c nodeTestTodoistClient) Sync(_ context.Context, _ string, _ []string, commands []todoist.SyncCommand) (*todoist.SyncResponse, error) {
	resp := &todoist.SyncResponse{SyncToken: "node-test-sync-token", FullSync: true, Items: c.items}
	for _, cmd := range commands {
		if cmd.TempID != "" {
			if resp.TempIDMap == nil {
				resp.TempIDMap = map[string]string{}
			}
			resp.TempIDMap[cmd.TempID] = "node" + cmd.TempID
		}
	}
	return resp, nil
}

// Each Todoist task completion resolves to a node of its own.
// spec: IXN-015.one-node-per-observation
func TestInteractionNode_TodoistTaskCompletionsResolveToOwnNodes(t *testing.T) {
	t.Parallel()
	e := newNodeSyncEnv(t)
	cadence := "monthly"
	taskRepo := repository.NewContactTaskRepository(e.database.Queries)

	var items []todoist.SyncItem
	contacts := make([]*repository.Contact, 2)
	taskIDs := make([]string, 2)
	for i := range contacts {
		contact, err := e.contactRepo.CreateContact(e.ctx, repository.CreateContactRequest{FullName: e.gen.Prefix() + "todoist-node-" + uuid.NewString()[:8], Cadence: &cadence})
		require.NoError(t, err)
		contacts[i] = contact
		taskIDs[i] = "td-node-" + uuid.NewString()[:8]
		_, err = taskRepo.CreateContactTask(e.ctx, repository.CreateContactTaskRequest{
			ContactID:      contact.ID,
			Provider:       todoist.SourceName,
			Kind:           contacttask.KindReachOut,
			Lifecycle:      contacttask.LifecycleCadenceDue,
			ExternalTaskID: taskIDs[i],
			State:          string(repository.ContactTaskStateManaged),
			Metadata:       map[string]any{},
		})
		require.NoError(t, err)
		items = append(items, todoist.SyncItem{ID: taskIDs[i], Checked: true})
	}

	provider := todoist.NewCadenceSyncProvider(
		nodeTestTodoistOAuth{}, taskRepo, e.contactRepo,
		repository.NewSyncRepositoryWithPool(e.database.Queries, e.database.Pool),
		config.TestConfig(), e.bus, e.cadenceUpdater, e.database.Pool,
		func(string) todoist.Client { return nodeTestTodoistClient{items: items} },
		nil, false,
	)
	accountID := e.gen.Prefix() + "todoist"
	_, err := provider.Sync(e.ctx, &repository.SyncState{
		Source:    repository.InteractionSourceTodoist,
		AccountID: &accountID,
		Metadata: map[string]any{
			todoist.MetadataKeyProjectID: e.gen.Prefix() + "project",
			todoist.MetadataKeyLabelID:   e.gen.Prefix() + "label",
		},
	}, nil)
	require.NoError(t, err)

	first := waitForInteractionBySourceRef(t, e.ctx, e.interactionRepo, contacts[0].ID, repository.InteractionSourceTodoist, taskIDs[0], defaultInteractionWaitTimeout)
	second := waitForInteractionBySourceRef(t, e.ctx, e.interactionRepo, contacts[1].ID, repository.InteractionSourceTodoist, taskIDs[1], defaultInteractionWaitTimeout)
	require.NotEqual(t, nodeOfInteraction(t, e.nodes, first), nodeOfInteraction(t, e.nodes, second))
}
