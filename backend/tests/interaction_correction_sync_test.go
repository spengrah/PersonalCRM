package tests

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"personal-crm/backend/internal/api"
	"personal-crm/backend/internal/api/handlers"
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

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/stretchr/testify/require"
)

// These tests make corrections through the correction routes and then
// re-process the corrected source objects through their sync entry points:
// CalendarSyncProvider.Sync and GmailSyncProvider.Sync with Google faked at its
// client boundary, and the calendar decline handler the calendar sync drives.

// correctionRoutes serves the interaction correction routes on database,
// recomputing cadence and follow-ups through cadence and followUp.
func correctionRoutes(t *testing.T, database *db.Database, cadence *consumer.CadenceUpdater, followUp *consumer.FollowUpManager) *gin.Engine {
	t.Helper()
	bus := newInsertOnlyCorrectionBus(t, database)
	assertSvc, _ := buildKnowledgeDeps(t, database, bus)
	contactRepo := repository.NewContactRepository(database.Queries)
	contactRepo.SetPool(database.Pool)
	corrections := service.NewInteractionCorrectionService(database.Pool, repository.NewInteractionRepository(database.Queries),
		repository.NewNodeRepository(database.Queries), repository.NewAssertionRepository(database.Queries), contactRepo,
		assertSvc, cadence, followUp, bus)
	router := gin.New()
	router.Use(api.RequestIDMiddleware())
	handler := handlers.NewInteractionCorrectionHandler(corrections)
	router.POST("/api/v1/interactions/merge", handler.MergeInteractions)
	router.POST("/api/v1/interactions/:id/removals", handler.RemoveParticipations)
	return router
}

// newInsertOnlyCorrectionBus is a bus whose River client is never started.
func newInsertOnlyCorrectionBus(t *testing.T, database *db.Database) *events.Bus {
	t.Helper()
	workers := river.NewWorkers()
	river.AddWorker(workers, &knowledgeCacheNoopWorker{})
	river.AddWorker(workers, &cadenceUpdaterNoopWorker{})
	river.AddWorker(workers, &followUpManagerNoopWorker{})
	river.AddWorker(workers, &followUpTestNoopOp{})
	client, err := river.NewClient(riverpgxv5.New(database.Pool), &river.Config{
		Queues:   map[string]river.QueueConfig{river.QueueDefault: {MaxWorkers: 1}},
		Workers:  workers,
		TestOnly: true,
	})
	require.NoError(t, err)
	return events.NewBus(database.Pool, client, repository.NewEventRepository(database.Queries))
}

// unconfiguredFollowUp is a cutover FollowUpManager on an instance that never
// connected Todoist.
func unconfiguredFollowUp(t *testing.T, database *db.Database) *consumer.FollowUpManager {
	t.Helper()
	contactRepo := repository.NewContactRepository(database.Queries)
	taskRepo := repository.NewContactTaskRepository(database.Queries)
	return consumer.NewFollowUpManager(consumer.FollowUpModeCutover, repository.NewEventConsumerClaimRepository(database.Queries),
		contactRepo, taskRepo, taskRepo, repository.NewInteractionRepository(database.Queries),
		newInsertOnlyCorrectionBusClient(t, database),
		func(context.Context) (*todoist.Settings, string, error) {
			return nil, "", consumer.ErrTodoistUnconfigured
		},
		"", config.TestConfig().Watchdog)
}

// newInsertOnlyCorrectionBusClient is a never-started River client accepting
// the Todoist op kind.
func newInsertOnlyCorrectionBusClient(t *testing.T, database *db.Database) *river.Client[pgx.Tx] {
	t.Helper()
	workers := river.NewWorkers()
	river.AddWorker(workers, &followUpTestNoopOp{})
	client, err := river.NewClient(riverpgxv5.New(database.Pool), &river.Config{
		Queues:   map[string]river.QueueConfig{river.QueueDefault: {MaxWorkers: 1}},
		Workers:  workers,
		TestOnly: true,
	})
	require.NoError(t, err)
	return client
}

// postCorrection posts a correction body and requires the status.
func postCorrection(t *testing.T, router *gin.Engine, path string, body any, status int) {
	t.Helper()
	raw, err := json.Marshal(body)
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	require.Equal(t, status, w.Code, w.Body.String())
}

func mergeRequest(contactID uuid.UUID, ids ...uuid.UUID) map[string]any {
	raw := make([]string, len(ids))
	for i, id := range ids {
		raw[i] = id.String()
	}
	return map[string]any{"interaction_ids": raw, "contact_id": contactID.String()}
}

func removeContacts(reason string, ids ...uuid.UUID) map[string]any {
	raw := make([]string, len(ids))
	for i, id := range ids {
		raw[i] = id.String()
	}
	return map[string]any{"scope": map[string]any{"contact_ids": raw}, "reason": reason}
}

func removeEveryone(reason string) map[string]any {
	return map[string]any{"scope": map[string]any{"everyone": true}, "reason": reason}
}

func removalPath(node uuid.UUID) string { return "/api/v1/interactions/" + node.String() + "/removals" }

// cadenceOf reads the contact's cadence fields.
func cadenceOf(t *testing.T, contacts *repository.ContactRepository, id uuid.UUID) repository.Contact {
	t.Helper()
	c, err := contacts.GetContact(t.Context(), id)
	require.NoError(t, err)
	c.UpdatedAt = time.Time{}
	return *c
}

// effective returns the contact's correctable effective participations.
func effective(t *testing.T, interactions *repository.InteractionRepository, contactID uuid.UUID) []repository.EffectiveParticipation {
	t.Helper()
	eps, err := interactions.ListEffectiveParticipation(t.Context(), contactID)
	require.NoError(t, err)
	var out []repository.EffectiveParticipation
	for _, ep := range eps {
		if ep.InteractionNodeID != nil {
			out = append(out, ep)
		}
	}
	return out
}

func TestInteractionCorrection_HoldsThroughCalendarSync(t *testing.T) {
	t.Parallel()

	t.Run("a re-sync of an event merged into another keeps one participation and moves no cadence field", func(t *testing.T) {
		// spec: ING-040.no-sync-restores-correction
		e := newNodeSyncEnv(t)
		router := correctionRoutes(t, e.database, e.cadenceUpdater, unconfiguredFollowUp(t, e.database))
		contactA, specA := e.newContactWithEmail(t)
		contactB, specB := e.newContactWithEmail(t)
		shape := factory.GCalEventShape{StartOffset: -3 * time.Hour, Duration: time.Hour}
		first := withAttendee(e.gen.GCalEventWithShape(specA, factory.MatchSeeded, shape), specB.Email)
		second := withAttendee(e.gen.GCalEventWithShape(specA, factory.MatchSeeded, shape), specB.Email)
		e.syncCalendar(t, first.AccountID, "", first.Event, second.Event)
		nodeFirst := nodeOfInteraction(t, e.nodes, e.gcalObservation(t, first, contactA.ID))
		nodeSecond := nodeOfInteraction(t, e.nodes, e.gcalObservation(t, second, contactA.ID))
		e.gcalObservation(t, first, contactB.ID)
		e.gcalObservation(t, second, contactB.ID)
		postCorrection(t, router, "/api/v1/interactions/merge", mergeRequest(contactA.ID, nodeFirst, nodeSecond), http.StatusOK)
		before := map[uuid.UUID]repository.Contact{contactA.ID: cadenceOf(t, e.contactRepo, contactA.ID), contactB.ID: cadenceOf(t, e.contactRepo, contactB.ID)}

		first.Event.Summary += " (moved)"
		e.syncCalendar(t, first.AccountID, "", first.Event, second.Event)

		for id, fields := range before {
			eps := effective(t, e.interactionRepo, id)
			require.Len(t, eps, 1)
			require.Equal(t, nodeFirst, *eps[0].InteractionNodeID)
			require.Equal(t, fields, cadenceOf(t, e.contactRepo, id))
		}
	})

	t.Run("a sync adding an attendee to an event removed for everyone stores an observation that has no effect", func(t *testing.T) {
		// spec: ING-040.excluded-observation-recorded-without-effect, IXN-018.everyone-covers-later-observed
		e := newNodeSyncEnv(t)
		router := correctionRoutes(t, e.database, e.cadenceUpdater, unconfiguredFollowUp(t, e.database))
		contactA, specA := e.newContactWithEmail(t)
		contactC, specC := e.newContactWithEmail(t)
		ev := e.gen.GCalEvent(specA, factory.MatchSeeded)
		e.syncCalendar(t, ev.AccountID, "", ev.Event)
		node := nodeOfInteraction(t, e.nodes, e.gcalObservation(t, ev, contactA.ID))
		postCorrection(t, router, removalPath(node), removeEveryone(service.RemovalReasonDidNotHappen), http.StatusNoContent)
		beforeC := cadenceOf(t, e.contactRepo, contactC.ID)

		withC := withAttendee(ev, specC.Email)
		e.syncCalendar(t, ev.AccountID, "", withC.Event)

		stored := e.gcalObservation(t, ev, contactC.ID)
		require.Equal(t, node, nodeOfInteraction(t, e.nodes, stored), "the attendee's observation is stored on the removed event")
		require.Empty(t, effective(t, e.interactionRepo, contactC.ID))
		require.Equal(t, beforeC, cadenceOf(t, e.contactRepo, contactC.ID))
		tasks, err := repository.NewContactTaskRepository(e.database.Queries).ListContactTasksByContact(e.ctx, contactC.ID)
		require.NoError(t, err)
		require.Empty(t, tasks)
	})

	t.Run("a decline after a removal does not restore the removed observation's effect", func(t *testing.T) {
		// spec: ING-040.no-sync-restores-correction
		e := newNodeSyncEnv(t)
		router := correctionRoutes(t, e.database, e.cadenceUpdater, unconfiguredFollowUp(t, e.database))
		contact, spec := e.newContactWithEmail(t)
		older := e.gen.GCalEventWithShape(spec, factory.MatchSeeded, factory.GCalEventShape{StartOffset: -30 * time.Hour, Duration: time.Hour})
		removed := e.gen.GCalEventWithShape(spec, factory.MatchSeeded, factory.GCalEventShape{StartOffset: -3 * time.Hour, Duration: time.Hour})
		e.syncCalendar(t, older.AccountID, "", older.Event)
		e.syncCalendar(t, removed.AccountID, "", removed.Event)
		olderObs := e.gcalObservation(t, older, contact.ID)
		removedNode := nodeOfInteraction(t, e.nodes, e.gcalObservation(t, removed, contact.ID))
		postCorrection(t, router, removalPath(removedNode), removeContacts(service.RemovalReasonDidNotAttend, contact.ID), http.StatusNoContent)
		require.True(t, cadenceOf(t, e.contactRepo, contact.ID).LastContacted.Equal(olderObs.OccurredAt))

		e.syncCalendar(t, older.AccountID, "node-test-cursor", cancelledStubFor(older.GcalEventID))
		waitForInteractionGone(t, e, contact.ID, olderObs.SourceRef)

		after := cadenceOf(t, e.contactRepo, contact.ID)
		require.Nil(t, after.LastContacted, "the removed observation is no remaining participation")
		require.Nil(t, after.LastInteractionAt)
	})
}

// correctionMailEnv runs GmailSyncProvider.Sync against the live email
// consumer, with a ContactService whose follow-up manager has Todoist
// configured, so an extension or promotion that took effect would show in
// the contact's follow-up tasks. Its mail is dated on nodeMailDay, which the
// faked Gmail sweep returns.
type correctionMailEnv struct {
	*nodeMailEnv
	router *gin.Engine
	tasks  *repository.ContactTaskRepository
}

func newCorrectionMailEnv(t *testing.T) *correctionMailEnv {
	t.Helper()
	ctx := t.Context()
	database, _ := newIsolatedRiverTestDB(t, ctx)
	contactRepo := repository.NewContactRepository(database.Queries)
	contactRepo.SetPool(database.Pool)
	interactionRepo := repository.NewInteractionRepository(database.Queries)
	taskRepo := repository.NewContactTaskRepository(database.Queries)
	commsRepo := repository.NewCommsMessageRepository(database.Queries)
	cadenceUpdater := buildCadenceUpdaterForTest(t, database)
	followUp := consumer.NewFollowUpManager(consumer.FollowUpModeCutover, repository.NewEventConsumerClaimRepository(database.Queries),
		contactRepo, taskRepo, taskRepo, interactionRepo, newInsertOnlyCorrectionBusClient(t, database),
		func(context.Context) (*todoist.Settings, string, error) {
			return &todoist.Settings{ProjectID: "proj", LabelName: "followup", IntegrationInstanceID: "inst"}, "token", nil
		}, "", config.TestConfig().Watchdog)
	assertSvc, cache := buildKnowledgeDeps(t, database, nil)
	contactService := service.NewContactService(database, contactRepo, repository.NewContactMethodRepository(database.Queries),
		interactionRepo, taskRepo, nil, nil, cadenceUpdater, assertSvc, cache, followUp)
	bus, emailConsumer := setupTestEventBusForEmail(t, ctx, database, contactService)
	gen, _ := migrationGenerator(t)
	mail := &emailEnv{
		ctx: ctx, database: database, gen: gen, bus: bus, emailConsumer: emailConsumer, contactRepo: contactRepo,
		interactionRepo: interactionRepo, commsRepo: commsRepo, contactService: contactService,
	}
	return &correctionMailEnv{
		nodeMailEnv: &nodeMailEnv{
			emailEnv: mail,
			provider: google.NewGmailSyncProvider(nil, commsRepo, bus, database.Pool),
			nodes:    repository.NewNodeRepository(database.Queries),
			me:       gen.Prefix() + "me@synthetic.example",
		},
		router: correctionRoutes(t, database, cadenceUpdater, followUp),
		tasks:  taskRepo,
	}
}

// newCadenceMailContact seeds a contact with a monthly cadence and one email
// address.
func (e *correctionMailEnv) newCadenceMailContact(t *testing.T) (*repository.Contact, string) {
	t.Helper()
	spec := e.gen.Contact(factory.WithEmail(), factory.WithCadence("monthly"), factory.WithCreatedAge(60*24*time.Hour))
	methods := make([]service.ContactMethodInput, 0, len(spec.Methods))
	for _, m := range spec.Methods {
		methods = append(methods, service.ContactMethodInput{Type: m.Type, Value: m.Value, IsPrimary: m.IsPrimary})
	}
	contact, _, err := e.contactService.CreateContact(e.ctx, repository.CreateContactRequest{
		FullName: spec.FullName, Cadence: spec.Cadence, CreatedAt: spec.CreatedAt,
	}, methods)
	require.NoError(t, err)
	return contact, spec.Email
}

// liveFollowUps returns the contact's live follow-up tasks.
func (e *correctionMailEnv) liveFollowUps(t *testing.T, contactID uuid.UUID) []repository.ContactTask {
	t.Helper()
	tasks, err := e.tasks.ListContactTasksByContact(e.ctx, contactID)
	require.NoError(t, err)
	var out []repository.ContactTask
	for _, task := range tasks {
		if task.Lifecycle == contacttask.LifecycleFollowUpLoop && task.State != repository.ContactTaskStateCompleted {
			out = append(out, task)
		}
	}
	return out
}

func TestInteractionCorrection_HoldsThroughGmailSync(t *testing.T) {
	t.Parallel()

	t.Run("a removed thread-day gaining same-day mail by extension and promotion has no cadence or follow-up effect", func(t *testing.T) {
		// spec: ING-040.no-sync-restores-correction, ING-040.excluded-observation-recorded-without-effect
		e := newCorrectionMailEnv(t)
		contact, addr := e.newCadenceMailContact(t)
		day := nodeMailDay
		first := e.mail("r-1", "thr-removed", addr, []string{e.me}, day.Add(time.Hour))
		e.sync(t, first)
		threadDay := e.observation(t, contact.ID, "thr-removed", day)
		ref := "call-" + uuid.NewString()
		_, err := e.contactService.RecordInteraction(e.ctx, repository.RecordInteractionRequest{
			ContactID: contact.ID, Source: repository.InteractionSourcePhoneCalls, SourceRef: &ref,
			Direction: repository.InteractionDirectionOutbound, OccurredAt: day,
		})
		require.NoError(t, err)
		require.Len(t, e.liveFollowUps(t, contact.ID), 1)
		postCorrection(t, e.router, removalPath(*threadDay.InteractionNodeID), removeContacts(service.RemovalReasonDuplicate, contact.ID), http.StatusNoContent)
		before := cadenceOf(t, e.contactRepo, contact.ID)
		followUps := e.liveFollowUps(t, contact.ID)
		require.Len(t, followUps, 1, "the outbound still awaits a reply")

		extend := e.mail("r-2", "thr-removed", addr, []string{e.me}, day.Add(2*time.Hour))
		e.sync(t, first, extend)
		e.waitForCommsProcessed(t, e.gen.Prefix()+"r-2@synthetic.example", contact.ID, defaultInteractionWaitTimeout)
		promote := e.mail("r-3", "thr-removed", e.me, []string{addr}, day.Add(3*time.Hour))
		e.sync(t, first, extend, promote)
		e.waitForCommsProcessed(t, e.gen.Prefix()+"r-3@synthetic.example", contact.ID, defaultInteractionWaitTimeout)

		stored := e.observation(t, contact.ID, "thr-removed", day)
		require.Equal(t, threadDay.ID, stored.ID)
		require.Equal(t, repository.InteractionDirectionMutual, stored.Direction, "the raw evidence is still recorded")
		require.Equal(t, before, cadenceOf(t, e.contactRepo, contact.ID))
		require.Equal(t, followUps, e.liveFollowUps(t, contact.ID), "a promotion to mutual of a removed observation closes no follow-up")
		for _, ep := range effective(t, e.interactionRepo, contact.ID) {
			require.NotEqual(t, *threadDay.InteractionNodeID, *ep.InteractionNodeID)
		}
	})

	t.Run("extending one member of a merged interaction applies the merged participation at its new latest time", func(t *testing.T) {
		// spec: ING-040.merged-member-applies-merged-participation
		e := newCorrectionMailEnv(t)
		contact, addr := e.newCadenceMailContact(t)
		day := nodeMailDay
		ref := "call-" + uuid.NewString()
		call, err := e.contactService.RecordInteraction(e.ctx, repository.RecordInteractionRequest{
			ContactID: contact.ID, Source: repository.InteractionSourcePhoneCalls, SourceRef: &ref,
			Direction: repository.InteractionDirectionOutbound, OccurredAt: day,
		})
		require.NoError(t, err)
		first := e.mail("m-1", "thr-merged", addr, []string{e.me}, day.Add(time.Hour))
		e.sync(t, first)
		threadDay := e.observation(t, contact.ID, "thr-merged", day)
		postCorrection(t, e.router, "/api/v1/interactions/merge", mergeRequest(contact.ID, *call.InteractionNodeID, *threadDay.InteractionNodeID), http.StatusOK)

		later := day.Add(5 * time.Hour)
		e.sync(t, first, e.mail("m-2", "thr-merged", addr, []string{e.me}, later))
		e.waitForCommsProcessed(t, e.gen.Prefix()+"m-2@synthetic.example", contact.ID, defaultInteractionWaitTimeout)

		got := cadenceOf(t, e.contactRepo, contact.ID)
		require.True(t, got.LastContacted.Equal(later))
		require.True(t, got.LastResponseAt.Equal(later))
		require.True(t, got.LastOutreachAt.Equal(later), "the merged participation is mutual: the outbound member's direction counts")
	})
}
