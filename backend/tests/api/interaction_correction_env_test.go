package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/api"
	"personal-crm/backend/internal/api/handlers"
	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/consumer"
	"personal-crm/backend/internal/consumer/consumerjobs"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/events"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"
	"personal-crm/backend/internal/synthetic"
	"personal-crm/backend/internal/synthetic/factory"
	"personal-crm/backend/internal/todoist"
	"personal-crm/backend/tests/testsupport"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/stretchr/testify/require"
)

// correctionEnv serves the interaction correction routes (POST
// /api/v1/interactions/merge, /merge/preview, /:id/removals) and GET
// /api/v1/contacts/:id over real services on one database. Observations are
// recorded through ContactService's recording path. Follow-up tasks run
// through the real FollowUpManager; Todoist is faked at its client boundary
// (todoist.Client) and its op jobs are enqueued on a River client that is
// never started, so a test runs them explicitly with the real op worker.
type correctionEnv struct {
	ctx          context.Context
	database     *db.Database
	harness      *synthetic.Harness
	router       *gin.Engine
	contacts     *service.ContactService
	contactRepo  *repository.ContactRepository
	interactions *repository.InteractionRepository
	assertions   *repository.AssertionRepository
	asserts      *service.AssertService
	nodes        *repository.NodeRepository
	tasks        *repository.ContactTaskRepository
	events       *repository.EventRepository
	bus          *events.Bus
	cadence      *consumer.CadenceUpdater
	riverClient  *river.Client[pgx.Tx]
	opWorker     *consumer.TodoistTaskOpWorker
	recorder     *consumer.InteractionRecorder
	todoist      *correctionTodoist
	base         time.Time
}

// newCorrectionEnv builds the env on the shared package database.
func newCorrectionEnv(t *testing.T) *correctionEnv {
	t.Helper()
	return newCorrectionEnvOn(t, testsupport.OpenDatabase(t))
}

// newCorrectionEnvOn builds the env on database.
func newCorrectionEnvOn(t *testing.T, database *db.Database) *correctionEnv {
	t.Helper()
	ctx := t.Context()
	cfg := config.TestConfig()

	workers := river.NewWorkers()
	river.AddWorker(workers, &apiTestCadenceShim{})
	river.AddWorker(workers, &apiTestFollowUpShim{})
	river.AddWorker(workers, &apiKnowledgeCacheNoopWorker{})
	opShim := &correctionOpShim{}
	river.AddWorker(workers, opShim)
	riverClient, err := river.NewClient(riverpgxv5.New(database.Pool), &river.Config{
		Queues:   map[string]river.QueueConfig{river.QueueDefault: {MaxWorkers: 1}},
		Workers:  workers,
		TestOnly: true,
	})
	require.NoError(t, err)
	eventRepo := repository.NewEventRepository(database.Queries)
	bus := events.NewBus(database.Pool, riverClient, eventRepo)

	contactRepo := repository.NewContactRepository(database.Queries)
	contactRepo.SetPool(database.Pool)
	interactionRepo := repository.NewInteractionRepository(database.Queries)
	taskRepo := repository.NewContactTaskRepository(database.Queries)
	claimRepo := repository.NewEventConsumerClaimRepository(database.Queries)
	assertionRepo := repository.NewAssertionRepository(database.Queries)
	nodeRepo := repository.NewNodeRepository(database.Queries)

	fake := &correctionTodoist{}
	settings := func(context.Context) (*todoist.Settings, string, error) {
		return &todoist.Settings{ProjectID: "proj-correction", LabelName: "followup", IntegrationInstanceID: "inst-correction"}, "token", nil
	}
	cadenceUpdater := consumer.NewCadenceUpdater(claimRepo, contactRepo, database.Queries, consumer.CadenceModeCutover, false, cfg.Watchdog)
	followUp := consumer.NewFollowUpManager(consumer.FollowUpModeCutover, claimRepo, contactRepo, taskRepo, taskRepo, interactionRepo,
		riverClient, settings, cfg.CORS.FrontendURL, cfg.Watchdog)
	assertSvc, cache := buildKnowledgeDepsForAPITest(t, database, bus)
	contactService := service.NewContactService(database, contactRepo, repository.NewContactMethodRepository(database.Queries),
		interactionRepo, taskRepo, nil, nil, cadenceUpdater, assertSvc, cache, followUp)
	corrections := service.NewInteractionCorrectionService(database.Pool, interactionRepo, nodeRepo, assertionRepo,
		contactRepo, assertSvc, cadenceUpdater, followUp, bus)
	opWorker := consumer.NewTodoistTaskOpWorker(consumer.FollowUpModeCutover, taskRepo, settings,
		func(string) todoist.Client { return fake }, riverClient, database.Pool)
	opShim.real = opWorker

	router := gin.New()
	router.Use(api.RequestIDMiddleware())
	v1 := router.Group("/api/v1")
	handlers.RegisterContactRoutes(v1, handlers.ContactRouteDeps{
		Contact:       handlers.NewContactHandler(contactService),
		Interaction:   &handlers.InteractionHandler{},
		Correction:    handlers.NewInteractionCorrectionHandler(corrections),
		Note:          &handlers.NoteHandler{},
		ContactMethod: &handlers.ContactMethodHandler{},
	})

	return &correctionEnv{
		ctx:          ctx,
		database:     database,
		harness:      synthetic.NewHarnessForNamespace(t, ctx, database, "x"+uuid.NewString()[:8], factory.DefaultSeed),
		router:       router,
		contacts:     contactService,
		contactRepo:  contactRepo,
		interactions: interactionRepo,
		assertions:   assertionRepo,
		asserts:      assertSvc,
		nodes:        nodeRepo,
		tasks:        taskRepo,
		events:       eventRepo,
		bus:          bus,
		cadence:      cadenceUpdater,
		riverClient:  riverClient,
		opWorker:     opWorker,
		recorder: consumer.NewInteractionRecorder(contactService, repository.NewStagingProcessorRegistry(nil), bus, cadenceUpdater, followUp,
			repository.NewCalendarEventRepository(database.Queries)),
		todoist: fake,
		base:    accelerated.GetCurrentTime().UTC().Add(-48 * time.Hour).Truncate(time.Second),
	}
}

// correctionOpShim registers the Todoist op kind; jobs never run on their own.
type correctionOpShim struct {
	river.WorkerDefaults[consumerjobs.TodoistTaskOpArgs]
	real *consumer.TodoistTaskOpWorker
}

func (w *correctionOpShim) Work(ctx context.Context, j *river.Job[consumerjobs.TodoistTaskOpArgs]) error {
	return w.real.Work(ctx, j)
}

// apiTodoistOpNoop registers the Todoist op kind on a River client that is
// never started.
type apiTodoistOpNoop struct {
	river.WorkerDefaults[consumerjobs.TodoistTaskOpArgs]
}

func (*apiTodoistOpNoop) Work(context.Context, *river.Job[consumerjobs.TodoistTaskOpArgs]) error {
	return nil
}

// correctionTodoist is Todoist faked at its client boundary. While failing,
// every Sync returns an error. It records every command it accepted, and
// delivers items once, on the next Sync that reads items.
type correctionTodoist struct {
	failing  bool
	accepted []todoist.SyncCommand
	items    []todoist.SyncItem
}

func (f *correctionTodoist) QuickAdd(context.Context, string, string) (*todoist.QuickAddTask, error) {
	return nil, errTodoistUnreachable
}

func (f *correctionTodoist) Sync(_ context.Context, _ string, resourceTypes []string, cmds []todoist.SyncCommand) (*todoist.SyncResponse, error) {
	if f.failing {
		return nil, errTodoistUnreachable
	}
	f.accepted = append(f.accepted, cmds...)
	tempMap := map[string]string{}
	for _, c := range cmds {
		if c.Type == "item_add" {
			tempMap[c.TempID] = "real-" + c.TempID
		}
	}
	resp := &todoist.SyncResponse{TempIDMap: tempMap}
	if slices.Contains(resourceTypes, "items") {
		resp.Items, f.items = f.items, nil
	}
	return resp, nil
}

// cadenceSync is the real Todoist cadence sync (CadenceSyncProvider.Sync) over
// the env, with Todoist faked at its client boundary. It reconciles every
// contact with a next-contact date, so it runs on a database the test owns.
type cadenceSync struct {
	e        *correctionEnv
	provider *todoist.CadenceSyncProvider
	account  string
}

const correctionTodoistLabel = "crm"

// newCadenceSync builds the cadence sync over e, whose database is the
// isolated database with config cfg.
func newCadenceSync(e *correctionEnv, cfg *config.Config) *cadenceSync {
	return &cadenceSync{
		e: e,
		provider: todoist.NewCadenceSyncProvider(correctionOAuth{}, e.tasks, e.contactRepo,
			repository.NewSyncRepositoryWithPool(e.database.Queries, e.database.Pool), cfg, e.bus, e.cadence, e.database.Pool,
			func(string) todoist.Client { return e.todoist }, e.riverClient, true),
		account: e.harness.Generator().Prefix() + "todoist",
	}
}

// reconcile runs one cadence sync.
func (c *cadenceSync) reconcile(t *testing.T) {
	t.Helper()
	_, err := c.provider.Sync(c.e.ctx, &repository.SyncState{
		Source:    repository.InteractionSourceTodoist,
		AccountID: &c.account,
		Metadata: map[string]any{
			todoist.MetadataKeyProjectID: "proj-correction",
			todoist.MetadataKeyLabelID:   "label-correction",
			todoist.MetadataKeyLabelName: correctionTodoistLabel,
		},
	}, nil)
	require.NoError(t, err)
}

// cadenceTask returns the contact's cadence-due task, after a reconcile that
// creates it when it is missing.
func (c *cadenceSync) cadenceTask(t *testing.T, contactID uuid.UUID) *repository.ContactTask {
	t.Helper()
	c.reconcile(t)
	task, err := c.e.tasks.GetContactTaskByContactCadenceDue(c.e.ctx, contactID, todoist.SourceName)
	require.NoError(t, err)
	require.NotEmpty(t, task.ExternalTaskID)
	return task
}

// editDeadline makes a Todoist deadline edit on the contact's cadence task:
// the user moves the task's deadline to date in Todoist, and the next sync
// reads it.
func (c *cadenceSync) editDeadline(t *testing.T, contactID uuid.UUID, date time.Time) {
	t.Helper()
	c.deliver(t, contactID, &todoist.SyncDate{Date: date.Format(todoist.DateFormat)})
}

// skipInTodoist skips the contact's cadence task from Todoist: the user removes
// its deadline, and the next sync reads it.
func (c *cadenceSync) skipInTodoist(t *testing.T, contactID uuid.UUID) {
	t.Helper()
	c.deliver(t, contactID, nil)
}

func (c *cadenceSync) deliver(t *testing.T, contactID uuid.UUID, deadline *todoist.SyncDate) {
	t.Helper()
	task := c.cadenceTask(t, contactID)
	c.e.todoist.items = []todoist.SyncItem{{
		ID:        task.ExternalTaskID,
		ProjectID: "proj-correction",
		Content:   "cadence task",
		Deadline:  deadline,
		Labels:    []string{correctionTodoistLabel},
		UpdatedAt: uuid.NewString(),
	}}
	c.reconcile(t)
}

// correctionOAuth is the Todoist OAuth token provider faked for the cadence
// sync.
type correctionOAuth struct{}

func (correctionOAuth) GetAccessToken(context.Context, string) (string, error) { return "token", nil }
func (correctionOAuth) HasAnyAccount(context.Context) bool                     { return true }

var errTodoistUnreachable = &todoistUnreachableError{}

type todoistUnreachableError struct{}

func (*todoistUnreachableError) Error() string { return "todoist unreachable" }

// at returns the env's base time plus h hours.
func (e *correctionEnv) at(h int) time.Time { return e.base.Add(time.Duration(h) * time.Hour) }

// contact seeds a synthetic contact created 60 days ago with the given
// cadence ("" for none).
func (e *correctionEnv) contact(t *testing.T, cadence string) uuid.UUID {
	t.Helper()
	opts := []factory.ContactOption{factory.WithCreatedAge(60 * 24 * time.Hour)}
	if cadence != "" {
		opts = append(opts, factory.WithCadence(cadence))
	}
	c, err := e.harness.SeedContact(e.ctx, e.harness.Generator().Contact(opts...))
	require.NoError(t, err)
	return c.ID
}

// record records one observation through the recording path.
func (e *correctionEnv) record(t *testing.T, contactID uuid.UUID, source, ref, direction string, occurredAt time.Time) *repository.Interaction {
	t.Helper()
	req := repository.RecordInteractionRequest{
		ContactID:  contactID,
		Source:     source,
		OccurredAt: occurredAt,
		Direction:  direction,
	}
	if ref != "" {
		req.SourceRef = &ref
	}
	ix, err := e.contacts.RecordInteraction(e.ctx, req)
	require.NoError(t, err)
	return ix
}

// event stores a past calendar event through the real calendar sync (the
// synthetic GCal replay, with no matched attendee) and returns the ref its
// observations carry (the calendar_event id): every attendee's observation of
// one stored event shares its interaction node.
func (e *correctionEnv) event(t *testing.T) string {
	t.Helper()
	spec := e.harness.Generator().GCalEvent(factory.ContactSpec{}, factory.MatchUnknown)
	_, err := e.harness.ReplayGCal(e.ctx, uuid.Nil, spec)
	require.NoError(t, err)
	ev, err := e.harness.CalendarEventRepo().GetByGcalID(e.ctx, spec.GcalEventID, "primary", spec.AccountID)
	require.NoError(t, err)
	return ev.ID.String()
}

// meeting records a contact's observation of calendar event ref (see event).
func (e *correctionEnv) meeting(t *testing.T, contactID uuid.UUID, ref string, occurredAt time.Time) uuid.UUID {
	t.Helper()
	ix := e.record(t, contactID, repository.InteractionSourceGCal, ref, repository.InteractionDirectionMutual, occurredAt)
	require.NotNil(t, ix.InteractionNodeID)
	return *ix.InteractionNodeID
}

// call records a contact's observation of phone call ref in direction.
func (e *correctionEnv) call(t *testing.T, contactID uuid.UUID, ref, direction string, occurredAt time.Time) uuid.UUID {
	t.Helper()
	ix := e.record(t, contactID, repository.InteractionSourcePhoneCalls, ref, direction, occurredAt)
	require.NotNil(t, ix.InteractionNodeID)
	return *ix.InteractionNodeID
}

// ref returns a source ref unique to this test.
func (e *correctionEnv) ref(prefix string) string { return prefix + "-" + uuid.NewString() }

// post sends a JSON body to path and returns the response.
func (e *correctionEnv) post(t *testing.T, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	e.router.ServeHTTP(w, req)
	return w
}

// mergeBody is the merge and preview request body.
func mergeBody(contactID uuid.UUID, ids ...uuid.UUID) map[string]any {
	raw := make([]string, len(ids))
	for i, id := range ids {
		raw[i] = id.String()
	}
	return map[string]any{"interaction_ids": raw, "contact_id": contactID.String()}
}

// mergeAnswer is the merge and preview answer's data.
type mergeAnswer struct {
	InteractionID uuid.UUID `json:"interaction_id"`
	Participation struct {
		Direction  string    `json:"direction"`
		OccurredAt time.Time `json:"occurred_at"`
	} `json:"participation"`
}

// merge posts a merge and requires 200, returning its answer.
func (e *correctionEnv) merge(t *testing.T, contactID uuid.UUID, ids ...uuid.UUID) mergeAnswer {
	t.Helper()
	w := e.post(t, "/api/v1/interactions/merge", mergeBody(contactID, ids...))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	return decodeMergeAnswer(t, w)
}

// preview posts a merge preview and requires 200, returning its answer.
func (e *correctionEnv) preview(t *testing.T, contactID uuid.UUID, ids ...uuid.UUID) mergeAnswer {
	t.Helper()
	w := e.post(t, "/api/v1/interactions/merge/preview", mergeBody(contactID, ids...))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	return decodeMergeAnswer(t, w)
}

func decodeMergeAnswer(t *testing.T, w *httptest.ResponseRecorder) mergeAnswer {
	t.Helper()
	var envelope struct {
		Success bool        `json:"success"`
		Data    mergeAnswer `json:"data"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &envelope))
	require.True(t, envelope.Success)
	return envelope.Data
}

// removalBody is a removal request body naming contacts.
func removalBody(reason string, contactIDs ...uuid.UUID) map[string]any {
	raw := make([]string, len(contactIDs))
	for i, id := range contactIDs {
		raw[i] = id.String()
	}
	return map[string]any{"scope": map[string]any{"contact_ids": raw}, "reason": reason}
}

// everyoneBody is a removal request body for everyone.
func everyoneBody(reason string) map[string]any {
	return map[string]any{"scope": map[string]any{"everyone": true}, "reason": reason}
}

// remove posts a removal and requires 204 with no body.
func (e *correctionEnv) remove(t *testing.T, id uuid.UUID, body map[string]any) {
	t.Helper()
	w := e.post(t, "/api/v1/interactions/"+id.String()+"/removals", body)
	require.Equal(t, http.StatusNoContent, w.Code, w.Body.String())
	require.Empty(t, w.Body.String())
}

// participation is the contact's effective participation (arc seam S4).
func (e *correctionEnv) participation(t *testing.T, contactID uuid.UUID) []repository.EffectiveParticipation {
	t.Helper()
	eps, err := e.interactions.ListEffectiveParticipation(e.ctx, contactID)
	require.NoError(t, err)
	return eps
}

// correctable returns the contact's effective participations that have an
// interaction node.
func (e *correctionEnv) correctable(t *testing.T, contactID uuid.UUID) []repository.EffectiveParticipation {
	t.Helper()
	var out []repository.EffectiveParticipation
	for _, ep := range e.participation(t, contactID) {
		if ep.InteractionNodeID != nil {
			out = append(out, ep)
		}
	}
	return out
}

// contactFields is the cadence part of GET /api/v1/contacts/:id.
type contactFields struct {
	LastContacted      *string `json:"last_contacted"`
	LastInteractionAt  *string `json:"last_interaction_at"`
	LastOutreachAt     *string `json:"last_outreach_at"`
	LastResponseAt     *string `json:"last_response_at"`
	ContactBy          *string `json:"contact_by"`
	AwaitingReply      bool    `json:"awaiting_reply"`
	AwaitingReplyUntil *string `json:"awaiting_reply_until"`
	LastSkippedAt      *string `json:"last_skipped_at"`
	UndoSkipAvailable  bool    `json:"undo_skip_available"`
}

// fields reads the contact's cadence fields through GET /api/v1/contacts/:id.
func (e *correctionEnv) fields(t *testing.T, contactID uuid.UUID) contactFields {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/contacts/"+contactID.String(), nil)
	w := httptest.NewRecorder()
	e.router.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var envelope struct {
		Data contactFields `json:"data"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &envelope))
	return envelope.Data
}

// correctionAssertions returns the accepted correction assertions under
// predicate touching node.
func (e *correctionEnv) correctionAssertions(t *testing.T, node uuid.UUID, predicate string) []repository.Assertion {
	t.Helper()
	var out []repository.Assertion
	require.NoError(t, pgx.BeginTxFunc(e.ctx, e.database.Pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		all, err := e.assertions.ListAssertionsTouchingNodeTx(e.ctx, tx, node)
		for _, a := range all {
			if a.PredicateKey == predicate && a.Status == repository.AssertionStatusAccepted {
				out = append(out, a)
			}
		}
		return err
	}))
	return out
}

// locator returns the one provenance locator of a correction assertion.
func (e *correctionEnv) locator(t *testing.T, assertionID uuid.UUID) repository.Provenance {
	t.Helper()
	prov, err := e.assertions.ListProvenance(e.ctx, assertionID)
	require.NoError(t, err)
	require.Len(t, prov, 1)
	return prov[0]
}

// correctionEvent returns the interaction.corrected event a correction act
// published; the act's id is the source id of its assertions' locator.
func (e *correctionEnv) correctionEvent(t *testing.T, act string) events.InteractionCorrectedPayload {
	t.Helper()
	env, err := e.events.FindEventBySource(e.ctx, "interaction_correction", act)
	require.NoError(t, err)
	require.Equal(t, events.KindInteractionCorrected, env.Kind)
	var p events.InteractionCorrectedPayload
	require.NoError(t, events.Unmarshal(env, &p))
	return p
}

// correctionEventCount counts every interaction.corrected event; meaningful
// only on a database the test owns.
func (e *correctionEnv) correctionEventCount(t *testing.T) int64 {
	t.Helper()
	n, err := e.events.CountBySource(e.ctx, "interaction_correction")
	require.NoError(t, err)
	return n
}

// rfc3339 renders a time as the contact payload does.
func rfc3339(at time.Time) string { return at.UTC().Format(time.RFC3339Nano) }
