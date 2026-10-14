package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/api"
	"personal-crm/backend/internal/events"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"
	"personal-crm/backend/tests/testsupport"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// These tests observe interaction-node identity (IXN-015, KNW-036, ING-045) at
// the recording entry points that serve the ingest API and the manual API:
// POST /api/v1/ingest/events (calls, Anarlog sessions), POST
// /api/v1/meeting-notes/:id/resolve-link, and POST
// /api/v1/contacts/:id/interactions. Each assertion reads the stored
// observation's interaction node and checks it is a registry node of type
// 'interaction'.

// ixnNode returns the interaction node an observation resolved to, and asserts
// the node is a live registry node of type 'interaction'.
func ixnNode(t *testing.T, nodes *repository.NodeRepository, ix repository.Interaction) uuid.UUID {
	t.Helper()
	require.NotNil(t, ix.InteractionNodeID, "observation %s (source %s) must resolve to an interaction node", ix.ID, ix.Source)
	node, err := nodes.GetNode(t.Context(), *ix.InteractionNodeID)
	require.NoError(t, err)
	require.Equal(t, "interaction", node.Type)
	return node.ID
}

// ixnSessionRows returns the session's live interactions keyed by source_ref.
func ixnSessionRows(t *testing.T, env *meetingNoteIngestEnv, sessionUUID string) map[string]repository.Interaction {
	t.Helper()
	out := map[string]repository.Interaction{}
	for _, ix := range listSessionInteractions(t, env, sessionUUID) {
		require.NotNil(t, ix.SourceRef)
		out[*ix.SourceRef] = ix
	}
	return out
}

// ixnPostSession posts one meeting_note.recorded event and requires acceptance.
func ixnPostSession(t *testing.T, env *meetingNoteIngestEnv, sessionUUID string, meetingAt time.Time, title string, participants []string) {
	t.Helper()
	ev := buildMNRecordedEvent(t, env.pairedHostID, sessionUUID, meetingAt, title, participants)
	w := postMNIngest(t, env, map[string]any{"events": []any{ev}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	resp := parseMNIngestResp(t, w)
	require.Equal(t, 1, resp.Accepted, "errors=%+v", resp.Errors)
}

// spec: IXN-015.one-node-per-observation
func TestInteractionNode_CallResolvesToNodeOfItsOwn(t *testing.T) {
	testsupport.RequireDatabase(t)
	t.Parallel()
	env := setupPhoneCallIngestEnv(t)
	ctx := t.Context()
	nodes := repository.NewNodeRepository(env.database.Queries)

	callA := env.sourcePrefix + "node-a"
	callB := env.sourcePrefix + "node-b"
	for _, id := range []string{callA, callB} {
		ev := buildCallEvent(t, events.KindCallReceived, env.pairedHostID, id, env.seededPhone, nil)
		w := postIngestCall(t, env, map[string]any{"events": []any{ev}})
		require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
		require.Equal(t, 1, parseIngestResp(t, w).Accepted)
	}

	rowA, err := env.interactionRpo.FindBySourceRef(ctx, env.seededContact, "phone_calls", callA)
	require.NoError(t, err)
	rowB, err := env.interactionRpo.FindBySourceRef(ctx, env.seededContact, "phone_calls", callB)
	require.NoError(t, err)
	require.NotEqual(t, ixnNode(t, nodes, *rowA), ixnNode(t, nodes, *rowB), "each call is an interaction of its own")
}

// spec: ING-045.call-reingest-keeps-node
func TestInteractionNode_CallReIngestedKeepsItsNode(t *testing.T) {
	testsupport.RequireDatabase(t)
	t.Parallel()
	env := setupPhoneCallIngestEnv(t)
	ctx := t.Context()
	nodes := repository.NewNodeRepository(env.database.Queries)

	callID := env.sourcePrefix + "node-reingest"
	ev := buildCallEvent(t, events.KindCallReceived, env.pairedHostID, callID, env.seededPhone, nil)
	w := postIngestCall(t, env, map[string]any{"events": []any{ev}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	first, err := env.interactionRpo.FindBySourceRef(ctx, env.seededContact, "phone_calls", callID)
	require.NoError(t, err)
	before := ixnNode(t, nodes, *first)

	w = postIngestCall(t, env, map[string]any{"events": []any{ev}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	again, err := env.interactionRpo.FindBySourceRef(ctx, env.seededContact, "phone_calls", callID)
	require.NoError(t, err)
	require.Equal(t, first.ID, again.ID)
	require.Equal(t, before, ixnNode(t, nodes, *again))
}

// A session's tagged, title-matched and walk-in participants all key to the
// session, so a row of any form resolves to the node the session already has.
// spec: IXN-015.shared-per-source-object
func TestInteractionNode_AnarlogTaggedTitleAndWalkinShareSessionNode(t *testing.T) {
	env := setupMeetingNoteIngestEnv(t)
	nodes := repository.NewNodeRepository(env.database.Queries)
	suffix := strings.TrimSuffix(strings.TrimPrefix(env.sourceIDPrefix, "mn-ingest-"), "-")
	anarlogA := env.seedAnarlogHumanResolvingTo(t, fmt.Sprintf("mn-test-0-%s@example.invalid", suffix))
	tokenA := newTitleToken(t, env)
	tokenB := newTitleToken(t, env)
	_, err := env.contactRepo.UpdateContact(t.Context(), env.contactA, repository.UpdateContactRequest{FullName: tokenA + " Smith"})
	require.NoError(t, err)
	contactB := seedTitleContact(t, env, tokenB+" Jones")

	sessionUUID := env.newSessionUUID()
	meetingAt := env.uniqueWindowBase(700)
	title := tokenA + " / " + tokenB + " 1:1"

	// Tagged and title-matched participants of an unlinked session.
	ixnPostSession(t, env, sessionUUID, meetingAt, title, []string{anarlogA})
	rows := ixnSessionRows(t, env, sessionUUID)
	taggedRef := "anarlog:" + sessionUUID + ":" + env.contactA.String()
	titleRef := "anarlog:" + sessionUUID + ":title:" + contactB.String()
	require.Len(t, rows, 2)
	sessionNode := ixnNode(t, nodes, rows[taggedRef])
	require.Equal(t, sessionNode, ixnNode(t, nodes, rows[titleRef]))

	// The session then links to a calendar event the tagged contact is not on,
	// which records the tagged contact as a walk-in.
	env.seedCalendarEventInWindow(t, meetingAt, []uuid.UUID{contactB})
	ixnPostSession(t, env, sessionUUID, meetingAt, title+" linked", []string{anarlogA})
	rows = ixnSessionRows(t, env, sessionUUID)
	walkinRef := "anarlog:" + sessionUUID + ":walkin:" + env.contactA.String()
	require.Contains(t, rows, walkinRef)
	require.Equal(t, sessionNode, ixnNode(t, nodes, rows[walkinRef]))
}

// A participant whose key moves from the walk-in form to the tagged and title
// forms (the old row dropped, a new row written) resolves to the session's node.
// spec: ING-045.anarlog-key-move-keeps-node
func TestInteractionNode_AnarlogParticipantKeyMovesKeepsSessionNode(t *testing.T) {
	env := setupMeetingNoteIngestEnv(t)
	nodes := repository.NewNodeRepository(env.database.Queries)
	suffix := strings.TrimSuffix(strings.TrimPrefix(env.sourceIDPrefix, "mn-ingest-"), "-")
	anarlogA := env.seedAnarlogHumanResolvingTo(t, fmt.Sprintf("mn-test-0-%s@example.invalid", suffix))
	tokenA := newTitleToken(t, env)
	tokenB := newTitleToken(t, env)
	_, err := env.contactRepo.UpdateContact(t.Context(), env.contactA, repository.UpdateContactRequest{FullName: tokenA + " Smith"})
	require.NoError(t, err)
	contactB := seedTitleContact(t, env, tokenB+" Jones")

	sessionUUID := env.newSessionUUID()
	meetingAt := env.uniqueWindowBase(701)
	title := tokenA + " / " + tokenB + " 1:1"

	// Linked to an event the tagged contact is not on: walk-in form.
	eventID := env.seedCalendarEventInWindow(t, meetingAt, []uuid.UUID{contactB})
	ixnPostSession(t, env, sessionUUID, meetingAt, title, []string{anarlogA})
	walkinRef := "anarlog:" + sessionUUID + ":walkin:" + env.contactA.String()
	rows := ixnSessionRows(t, env, sessionUUID)
	require.Len(t, rows, 1)
	walkin := rows[walkinRef]
	sessionNode := ixnNode(t, nodes, walkin)

	// The event goes away; the session re-syncs as unlinked: tagged and title forms.
	require.NoError(t, env.calendarRepo.TestHardDeleteByID(t.Context(), eventID))
	ixnPostSession(t, env, sessionUUID, meetingAt, title+" unlinked", []string{anarlogA})
	rows = ixnSessionRows(t, env, sessionUUID)
	taggedRef := "anarlog:" + sessionUUID + ":" + env.contactA.String()
	titleRef := "anarlog:" + sessionUUID + ":title:" + contactB.String()
	require.Len(t, rows, 2)
	require.NotContains(t, rows, walkinRef, "the walk-in row is dropped")
	require.NotEqual(t, walkin.ID, rows[taggedRef].ID, "the tagged row is a new row")
	require.Equal(t, sessionNode, ixnNode(t, nodes, rows[taggedRef]))
	require.Equal(t, sessionNode, ixnNode(t, nodes, rows[titleRef]))
}

// A session re-linked by ingest to a different calendar event keeps the node of
// the retained row, and the row moves to the new event's venue.
// spec: ING-045.anarlog-relink-keeps-node
func TestInteractionNode_AnarlogRelinkedByIngestKeepsNode(t *testing.T) {
	env := setupMeetingNoteIngestEnv(t)
	nodes := repository.NewNodeRepository(env.database.Queries)
	suffix := strings.TrimSuffix(strings.TrimPrefix(env.sourceIDPrefix, "mn-ingest-"), "-")
	anarlogA := env.seedAnarlogHumanResolvingTo(t, fmt.Sprintf("mn-test-0-%s@example.invalid", suffix))
	anarlogB := env.seedAnarlogHumanResolvingTo(t, fmt.Sprintf("mn-test-1-%s@example.invalid", suffix))

	sessionUUID := env.newSessionUUID()
	meetingAt := env.uniqueWindowBase(702)
	walkinRef := "anarlog:" + sessionUUID + ":walkin:" + env.contactA.String()

	// Linked to the only candidate, which the tagged contact is not on.
	env.seedCalendarEventInWindow(t, meetingAt, []uuid.UUID{env.contactC})
	ixnPostSession(t, env, sessionUUID, meetingAt, "Relink", []string{anarlogA})
	before := ixnSessionRows(t, env, sessionUUID)[walkinRef]
	beforeNode := ixnNode(t, nodes, before)
	require.NotNil(t, before.VenueID)

	// A second candidate appears and now wins on overlap with the tagged set.
	env.seedCalendarEventInWindow(t, meetingAt.Add(5*time.Minute), []uuid.UUID{env.contactB})
	ixnPostSession(t, env, sessionUUID, meetingAt, "Relink again", []string{anarlogA, anarlogB})
	after := ixnSessionRows(t, env, sessionUUID)[walkinRef]
	require.Equal(t, before.ID, after.ID, "the walk-in row is retained")
	require.NotNil(t, after.VenueID)
	require.NotEqual(t, *before.VenueID, *after.VenueID, "the retained row moved to the new event's venue")
	require.Equal(t, beforeNode, ixnNode(t, nodes, after))
}

// A session re-linked by the user's link resolution resolves its new rows to the
// node the session had before the link was in doubt.
// spec: ING-045.anarlog-relink-keeps-node
func TestInteractionNode_AnarlogRelinkedByLinkResolutionKeepsNode(t *testing.T) {
	env := setupMeetingNoteIngestEnv(t)
	nodes := repository.NewNodeRepository(env.database.Queries)
	suffix := strings.TrimSuffix(strings.TrimPrefix(env.sourceIDPrefix, "mn-ingest-"), "-")
	anarlogA := env.seedAnarlogHumanResolvingTo(t, fmt.Sprintf("mn-test-0-%s@example.invalid", suffix))

	sessionUUID := env.newSessionUUID()
	meetingAt := env.uniqueWindowBase(703)

	// Unlinked: one tagged row.
	ixnPostSession(t, env, sessionUUID, meetingAt, "Resolve", []string{anarlogA})
	taggedRef := "anarlog:" + sessionUUID + ":" + env.contactA.String()
	sessionNode := ixnNode(t, nodes, ixnSessionRows(t, env, sessionUUID)[taggedRef])

	// Two equal candidates appear: the link is in doubt and the row is dropped.
	eventB := env.seedCalendarEventInWindow(t, meetingAt, []uuid.UUID{env.contactB})
	env.seedCalendarEventInWindow(t, meetingAt.Add(5*time.Minute), []uuid.UUID{env.contactC})
	ixnPostSession(t, env, sessionUUID, meetingAt, "Resolve in doubt", []string{anarlogA})
	row := findMeetingNoteRow(t, env, sessionUUID)
	require.Equal(t, repository.LinkageStateConflictPending, row.LinkageState)
	require.Empty(t, ixnSessionRows(t, env, sessionUUID))

	// The user links one candidate; the tagged contact is a walk-in of it.
	w := postResolveLink(t, env, row.ID.String(), map[string]any{"action": "link", "kind": "event", "id": eventB.String()})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	walkinRef := "anarlog:" + sessionUUID + ":walkin:" + env.contactA.String()
	rows := ixnSessionRows(t, env, sessionUUID)
	require.Contains(t, rows, walkinRef)
	require.Equal(t, sessionNode, ixnNode(t, nodes, rows[walkinRef]))
}

// spec: IXN-015.manual-own-node
func TestInteractionNode_ManualEntries(t *testing.T) {
	testsupport.RequireDatabase(t)
	t.Parallel()
	router, cleanup := setupInteractionTestRouter(t)
	defer cleanup()
	database := testsupport.OpenDatabase(t)
	interactions := repository.NewInteractionRepository(database.Queries)
	nodes := repository.NewNodeRepository(database.Queries)

	contactID := createInteractionTestContact(t, router, "Node Manual User "+uuid.NewString()[:8])
	defer deleteInteractionTestContact(t, router, contactID)

	post := func(at time.Time) uuid.UUID {
		t.Helper()
		body, err := json.Marshal(map[string]string{"occurred_at": at.Format(time.RFC3339)})
		require.NoError(t, err)
		req, err := http.NewRequest("POST", "/api/v1/contacts/"+contactID+"/interactions", bytes.NewBuffer(body))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		require.Equal(t, http.StatusCreated, w.Code, "body: %s", w.Body.String())
		var resp api.APIResponse
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
		return uuid.MustParse(resp.Data.(map[string]any)["id"].(string))
	}
	nodeOf := func(id uuid.UUID) uuid.UUID {
		t.Helper()
		row, err := interactions.GetInteraction(t.Context(), id)
		require.NoError(t, err)
		return ixnNode(t, nodes, *row)
	}

	base := accelerated.GetCurrentTime().Add(-48 * time.Hour)
	first := post(base)
	firstNode := nodeOf(first)

	// A second entry outside the 30-minute dedup window is another observation
	// with a node of its own.
	second := post(base.Add(-3 * time.Hour))
	require.NotEqual(t, first, second)
	require.NotEqual(t, firstNode, nodeOf(second))

	// A dedup-window hit returns the first entry, and so its node.
	hit := post(base.Add(10 * time.Minute))
	require.Equal(t, first, hit)
	require.Equal(t, firstNode, nodeOf(hit))
}

// An iMessage session (source "messages") is a message burst: created through
// the ingest API and aggregation engine, extended by a later message and
// promoted to mutual by a reply, it resolves to no interaction node.
// spec: IXN-015.bursts-have-no-node
func TestInteractionNode_IMessageBurstSessionHasNoNode(t *testing.T) {
	t.Parallel()
	env := setupRawMessageE2E(t)
	jobs := repository.NewSyntheticSupportRepository(env.database.Queries)
	// The ingest handler enqueues one aggregation job per contact, and a job
	// still queued or running absorbs a newer one; each message therefore waits
	// for the queue to drain before the next arrives.
	waitForIdleQueue := func() {
		t.Helper()
		require.Eventually(t, func() bool {
			n, err := jobs.CountNonFinalRiverJobs(env.ctx)
			return err == nil && n == 0
		}, 10*time.Second, 20*time.Millisecond, "river jobs drain")
	}

	sentA := "node-sent-a-" + uuid.NewString()
	postRawMessageE2E(t, env, events.KindRawMessageSent, sentA)
	created := pollProcessedRawMessage(t, env, sentA)
	session, err := env.interactionRepo.GetInteraction(env.ctx, *created.InteractionID)
	require.NoError(t, err)
	require.Nil(t, session.InteractionNodeID, "a created burst session resolves to no interaction node")
	waitForIdleQueue()

	sentB := "node-sent-b-" + uuid.NewString()
	postRawMessageE2E(t, env, events.KindRawMessageSent, sentB)
	extended := pollProcessedRawMessage(t, env, sentB)
	require.Equal(t, session.ID, *extended.InteractionID, "the later message extends the session")
	session, err = env.interactionRepo.GetInteraction(env.ctx, session.ID)
	require.NoError(t, err)
	require.Nil(t, session.InteractionNodeID, "an extended burst session resolves to no interaction node")
	waitForIdleQueue()

	reply := "node-reply-" + uuid.NewString()
	postRawMessageE2E(t, env, events.KindRawMessageReceived, reply)
	promoted := pollProcessedRawMessage(t, env, reply)
	require.Equal(t, session.ID, *promoted.InteractionID, "the reply promotes the session")
	session, err = env.interactionRepo.GetInteraction(env.ctx, session.ID)
	require.NoError(t, err)
	require.Equal(t, repository.InteractionDirectionMutual, session.Direction)
	require.Nil(t, session.InteractionNodeID, "a session promoted to mutual resolves to no interaction node")
}

// After a contact merge, the observations moved to the surviving contact keep
// their nodes, and Anarlog re-processing that writes a new row for the surviving
// contact resolves to the moved row's node.
// spec: ING-045.after-merge-new-row-same-node, IXN-015.assigned-node-kept
func TestInteractionNode_AnarlogAfterContactMergeResolvesToMovedRowNode(t *testing.T) {
	env := setupMeetingNoteIngestEnv(t)
	nodes := repository.NewNodeRepository(env.database.Queries)
	suffix := strings.TrimSuffix(strings.TrimPrefix(env.sourceIDPrefix, "mn-ingest-"), "-")
	anarlogA := env.seedAnarlogHumanResolvingTo(t, fmt.Sprintf("mn-test-0-%s@example.invalid", suffix))

	sessionUUID := env.newSessionUUID()
	meetingAt := env.uniqueWindowBase(704)
	ixnPostSession(t, env, sessionUUID, meetingAt, "Before merge", []string{anarlogA})
	sourceRef := "anarlog:" + sessionUUID + ":" + env.contactA.String()
	movedBefore := ixnSessionRows(t, env, sessionUUID)[sourceRef]
	nodeBefore := ixnNode(t, nodes, movedBefore)

	assertSvc, cache := buildKnowledgeDepsForAPITest(t, env.database, nil)
	mergeSvc := service.NewContactService(env.database, env.contactRepo,
		repository.NewContactMethodRepository(env.database.Queries), env.interactionRep,
		repository.NewContactTaskRepository(env.database.Queries), nil, nil,
		buildCadenceUpdaterForAPITest(t, env.database), assertSvc, cache, nil)
	_, err := mergeSvc.MergeContacts(t.Context(), service.MergeContactsRequest{SourceContactID: env.contactA, TargetContactID: env.contactB})
	require.NoError(t, err)
	moved, err := env.interactionRep.FindBySourceRef(t.Context(), env.contactB, repository.InteractionSourceAnarlogSessions, sourceRef)
	require.NoError(t, err)
	require.Equal(t, nodeBefore, ixnNode(t, nodes, *moved), "the moved observation keeps its node")

	// The session is re-processed: the tagged human now resolves to the
	// surviving contact, whose row is new.
	ixnPostSession(t, env, sessionUUID, meetingAt, "After merge", []string{anarlogA})
	survivorRef := "anarlog:" + sessionUUID + ":" + env.contactB.String()
	rows := ixnSessionRows(t, env, sessionUUID)
	require.Contains(t, rows, survivorRef)
	require.NotEqual(t, moved.ID, rows[survivorRef].ID, "the surviving contact's observation is a new row")
	require.Equal(t, nodeBefore, ixnNode(t, nodes, rows[survivorRef]))
}
