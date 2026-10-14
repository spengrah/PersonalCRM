package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/api/handlers"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/events"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// groupIngestEnv drives the whole pipeline for group chats: the ingest POST,
// the aggregator, the recorder and the content read, on an isolated database.
type groupIngestEnv struct {
	*rawMessageE2EEnv
	members []*repository.Contact
	phones  map[uuid.UUID]string
	seq     int
}

func setupGroupIngest(t *testing.T) *groupIngestEnv {
	t.Helper()
	env := setupRawMessageE2E(t)
	queries := env.database.Queries
	content := service.NewInteractionContentService(env.interactionRepo, repository.NewCommsMessageRepository(queries),
		repository.NewTelegramMessageRepository(queries), env.messagesRepo, repository.NewMeetingNoteRepository(queries),
		repository.NewCalendarEventRepository(queries), repository.NewPhoneCallRepository(queries), env.contactRepo)
	handlers.RegisterContactRoutes(env.router.Group("/api/v1"), handlers.ContactRouteDeps{
		Contact: &handlers.ContactHandler{}, Interaction: handlers.NewInteractionHandler(nil, content),
		Note: &handlers.NoteHandler{}, ContactMethod: &handlers.ContactMethodHandler{},
	})
	return &groupIngestEnv{rawMessageE2EEnv: env, phones: map[uuid.UUID]string{}}
}

const strangerHandle = "+15559990001"

// newMember creates a contact with a weekly cadence and one phone method.
func (g *groupIngestEnv) newMember(t *testing.T) *repository.Contact {
	t.Helper()
	g.seq++
	phone := fmt.Sprintf("+1555200%04d", g.seq)
	contact, err := g.contactRepo.CreateContact(t.Context(), repository.CreateContactRequest{
		FullName: fmt.Sprintf("Group Ingest Member %d", g.seq),
		Cadence:  strPtr("weekly"),
	})
	require.NoError(t, err)
	_, err = g.contactMethods.CreateContactMethod(t.Context(), repository.CreateContactMethodRequest{
		ContactID: contact.ID, Type: "phone", Value: phone,
	})
	require.NoError(t, err)
	g.members = append(g.members, contact)
	g.phones[contact.ID] = phone
	return contact
}

// rawMsg is one raw_message event.
type rawMsg struct {
	kind   events.Kind
	guid   string
	chat   string
	handle string
	group  bool
	sentAt time.Time
	text   string
}

func (g *groupIngestEnv) newGuid() string { return "group-ingest-" + uuid.NewString() }

// received is an inbound group message from the handle.
func (g *groupIngestEnv) received(chat, handle string, sentAt time.Time) rawMsg {
	return rawMsg{kind: events.KindRawMessageReceived, guid: g.newGuid(), chat: chat, handle: handle, group: true, sentAt: sentAt, text: "inbound " + uuid.NewString()}
}

// sent is the user's own group message as the daemon publishes it: no peer.
func (g *groupIngestEnv) sent(chat string, sentAt time.Time) rawMsg {
	return rawMsg{kind: events.KindRawMessageSent, guid: g.newGuid(), chat: chat, handle: "", group: true, sentAt: sentAt, text: "outbound " + uuid.NewString()}
}

func (g *groupIngestEnv) eventFor(t *testing.T, m rawMsg) map[string]any {
	t.Helper()
	p := events.RawMessageReceivedPayload{
		Version: 1, HostID: g.pair.HostID, Source: "messages", Guid: m.guid, ChatID: m.chat,
		PeerHandle: m.handle, Text: &m.text, MessageType: "text", IsGroup: m.group, SentAt: m.sentAt,
	}
	var raw json.RawMessage
	var err error
	if m.kind == events.KindRawMessageSent {
		raw, err = events.Marshal(m.kind, events.RawMessageSentPayload(p))
	} else {
		raw, err = events.Marshal(m.kind, p)
	}
	require.NoError(t, err)
	return map[string]any{"source": "messages", "source_id": m.guid, "kind": string(m.kind), "payload": raw, "observed_at": accelerated.GetCurrentTime()}
}

func (g *groupIngestEnv) hostRequest(method, path string, body []byte) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Mac-Host-ID", g.pair.HostID.String())
	req.Header.Set("Authorization", "Bearer "+g.pair.APIKey)
	w := httptest.NewRecorder()
	g.router.ServeHTTP(w, req)
	return w
}

// post sends the messages as one ingest batch and returns the answer.
func (g *groupIngestEnv) post(t *testing.T, msgs ...rawMsg) handlers.IngestResponse {
	t.Helper()
	evs := make([]any, len(msgs))
	for i, m := range msgs {
		evs[i] = g.eventFor(t, m)
	}
	body, err := json.Marshal(map[string]any{"events": evs})
	require.NoError(t, err)
	w := g.hostRequest(http.MethodPost, "/api/v1/ingest/events", body)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	return parseIngestResp(t, w)
}

// postAccepted is post that requires every message to be accepted.
func (g *groupIngestEnv) postAccepted(t *testing.T, msgs ...rawMsg) {
	t.Helper()
	resp := g.post(t, msgs...)
	require.Equal(t, len(msgs), resp.Accepted, "errors: %+v", resp.Errors)
	require.Zero(t, resp.Rejected, "errors: %+v", resp.Errors)
}

// enrich posts enrichment items on the host path.
func (g *groupIngestEnv) enrich(t *testing.T, items ...map[string]any) enrichmentResponse {
	t.Helper()
	body, err := json.Marshal(map[string]any{"items": items})
	require.NoError(t, err)
	w := g.hostRequest(http.MethodPost, "/api/v1/host/"+g.pair.HostID.String()+"/messages/enrichment", body)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	var res enrichmentResponse
	readData(t, w, &res)
	return res
}

// settle waits until the aggregator and every job it caused for the members
// have finished.
func (g *groupIngestEnv) settle(t *testing.T) {
	t.Helper()
	ids := []string{g.contactID.String()}
	for _, m := range g.members {
		ids = append(ids, m.ID.String())
	}
	stable := 0
	for range 600 {
		// Aggregate jobs first: they enqueue the event jobs counted second.
		aggregate, err := g.database.Queries.SyntheticCountUnfinalizedMessagingAggregateJobs(t.Context(),
			db.SyntheticCountUnfinalizedMessagingAggregateJobsParams{Source: "messages", ContactIds: ids})
		require.NoError(t, err)
		eventJobs, err := g.database.Queries.SyntheticCountUnfinalizedRiverJobsForEventsByContacts(t.Context(), ids)
		require.NoError(t, err)
		if aggregate == 0 && eventJobs == 0 {
			stable++
			if stable == 3 {
				return
			}
		} else {
			stable = 0
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("the aggregator and its jobs did not settle within 15s")
}

func (g *groupIngestEnv) row(t *testing.T, guid string) *repository.MessagesMessage {
	t.Helper()
	row, err := g.messagesRepo.GetMessage(t.Context(), guid)
	require.NoError(t, err)
	return row
}

func (g *groupIngestEnv) interactions(t *testing.T, contactID uuid.UUID) []repository.Interaction {
	t.Helper()
	rows, err := g.interactionRepo.ListContactInteractions(t.Context(), contactID, 50, 0)
	require.NoError(t, err)
	return rows
}

func (g *groupIngestEnv) contact(t *testing.T, contactID uuid.UUID) *repository.Contact {
	t.Helper()
	c, err := g.contactRepo.GetContact(t.Context(), contactID)
	require.NoError(t, err)
	return c
}

// burst settles the member's messages (posted together as one inbound burst) and
// returns the interaction they produced.
func (g *groupIngestEnv) burst(t *testing.T, chat string, member *repository.Contact, times ...time.Time) (*repository.Interaction, []string) {
	t.Helper()
	msgs := make([]rawMsg, len(times))
	guids := make([]string, len(times))
	for i, sentAt := range times {
		msgs[i] = g.received(chat, g.phones[member.ID], sentAt)
		guids[i] = msgs[i].guid
	}
	g.postAccepted(t, msgs...)
	g.settle(t)
	linked := g.row(t, guids[0]).InteractionID
	require.NotNil(t, linked, "the member's messages aggregate into one interaction")
	interaction, err := g.interactionRepo.GetInteraction(t.Context(), *linked)
	require.NoError(t, err)
	return interaction, guids
}

func (g *groupIngestEnv) interaction(t *testing.T, id uuid.UUID) *repository.Interaction {
	t.Helper()
	i, err := g.interactionRepo.GetInteraction(t.Context(), id)
	require.NoError(t, err)
	return i
}

// content reads the drill-down messages of an interaction in its own
// contact's participation context.
func (g *groupIngestEnv) content(t *testing.T, id uuid.UUID) []contentRow {
	t.Helper()
	path := timelineContentPath(g.interaction(t, id))
	w := httptest.NewRecorder()
	g.router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	var data struct {
		Members []struct {
			Messages []contentRow `json:"messages"`
		} `json:"members"`
	}
	readData(t, w, &data)
	var rows []contentRow
	for _, m := range data.Members {
		rows = append(rows, m.Messages...)
	}
	return rows
}

func (g *groupIngestEnv) bodies(t *testing.T, id uuid.UUID) []string {
	t.Helper()
	rows := g.content(t, id)
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.Body
	}
	return out
}

// state is everything the user's group message must leave alone for a member
// it does not promote.
type memberState struct {
	contact      *repository.Contact
	interactions []repository.Interaction
	tasks        []repository.ContactTask
}

func (g *groupIngestEnv) stateOf(t *testing.T, member *repository.Contact) memberState {
	t.Helper()
	tasks, err := repository.NewContactTaskRepository(g.database.Queries).ListContactTasksByContact(t.Context(), member.ID)
	require.NoError(t, err)
	return memberState{contact: g.contact(t, member.ID), interactions: g.interactions(t, member.ID), tasks: tasks}
}

// cadenceOf is the cadence-bearing part of a contact, which two contacts that
// went through the same interactions share.
type cadenceFields struct {
	LastContacted, ContactBy, LastInteractionAt, LastOutreachAt, LastResponseAt, AwaitingReplyUntil *time.Time
	LastSkippedAt, LastSkippedContactBy                                                             *time.Time
	LastSkipReason                                                                                  *string
}

func cadenceOf(c *repository.Contact) cadenceFields {
	return cadenceFields{c.LastContacted, c.ContactBy, c.LastInteractionAt, c.LastOutreachAt, c.LastResponseAt,
		c.AwaitingReplyUntil, c.LastSkippedAt, c.LastSkippedContactBy, c.LastSkipReason}
}

// promotedControl records what the existing promotion of an inbound burst to
// mutual at its own occurred_at does to a member's cadence fields, on a control
// member whose burst has the same times in a chat of its own.
func (g *groupIngestEnv) promotedControl(t *testing.T, times ...time.Time) cadenceFields {
	t.Helper()
	control := g.newMember(t)
	burst, _ := g.burst(t, "control-"+uuid.NewString(), control, times...)
	require.NoError(t, g.contactSvc.PromoteInteractionToMutual(t.Context(), burst.ID, control.ID, burst.OccurredAt))
	return cadenceOf(g.contact(t, control.ID))
}

func chatName(t *testing.T) string { return "group-chat-" + uuid.NewString() }

// spec: ING-041.non-contact-message-stored
func TestGroupIngest_NonContactMessageIsStoredUnmatchedAndUnlinkedAndCreatesNoContact(t *testing.T) {
	g := setupGroupIngest(t)
	chat := chatName(t)
	member := g.newMember(t)
	g.burst(t, chat, member, at(0), at(120))
	contactsBefore, err := g.contactRepo.CountContacts(t.Context(), repository.ListContactsParams{})
	require.NoError(t, err)
	stranger := g.received(chat, strangerHandle, at(60))

	g.postAccepted(t, stranger)
	g.settle(t)

	row := g.row(t, stranger.guid)
	require.Equal(t, strangerHandle, row.PeerHandle, "the sender's handle is the stored sender")
	require.Equal(t, chat, row.ChatGuid)
	require.True(t, row.IsGroupChat)
	require.False(t, row.IsOutgoing)
	require.Nil(t, row.MatchedContactID)
	require.Nil(t, row.InteractionID)
	contactsAfter, err := g.contactRepo.CountContacts(t.Context(), repository.ListContactsParams{})
	require.NoError(t, err)
	require.Equal(t, contactsBefore, contactsAfter, "no contact is created")
}

// spec: ING-041.non-contact-message-credits-nothing
// spec: ING-041.evidence-within-existing-windows
func TestGroupIngest_NonContactMessageChangesNoBurstContactOrTaskAndIsEvidenceOnlyInsideWindows(t *testing.T) {
	g := setupGroupIngest(t)
	chat := chatName(t)
	member := g.newMember(t)
	burst, _ := g.burst(t, chat, member, at(0), at(120))
	linkedBefore, err := g.messagesRepo.ListByInteractionIDs(t.Context(), []uuid.UUID{burst.ID})
	require.NoError(t, err)
	before := g.stateOf(t, member)
	inside, outside := g.received(chat, strangerHandle, at(60)), g.received(chat, strangerHandle, at(500))

	g.postAccepted(t, inside, outside)
	g.settle(t)

	require.Equal(t, before, g.stateOf(t, member), "the member's cadence fields, awaiting-reply window, skip state, interactions and tasks are unchanged")
	linkedAfter, err := g.messagesRepo.ListByInteractionIDs(t.Context(), []uuid.UUID{burst.ID})
	require.NoError(t, err)
	require.Equal(t, linkedBefore, linkedAfter, "the burst's linked messages are unchanged")
	after := g.interaction(t, burst.ID)
	require.True(t, burst.OccurredAt.Equal(after.OccurredAt))
	bodies := g.bodies(t, burst.ID)
	require.Contains(t, bodies, inside.text, "a non-contact message inside the window is in the burst's content")
	require.NotContains(t, bodies, outside.text, "a non-contact message outside every window is in no interaction's content")
	require.Nil(t, g.row(t, inside.guid).InteractionID)
	require.Nil(t, g.row(t, outside.guid).InteractionID)
}

// spec: ING-041.outbound-group-message-unattributed
func TestGroupIngest_UsersGroupMessageIsStoredUnattributedWhateverPeerThePayloadNames(t *testing.T) {
	g := setupGroupIngest(t)
	chat := chatName(t)
	member := g.newMember(t)
	empty := g.sent(chat, at(10))
	named := g.sent(chat, at(20))
	named.handle = g.phones[member.ID]
	before := g.stateOf(t, member)

	g.postAccepted(t, empty, named)
	g.settle(t)

	for _, m := range []rawMsg{empty, named} {
		row := g.row(t, m.guid)
		require.True(t, row.IsOutgoing)
		require.True(t, row.IsGroupChat)
		require.Empty(t, row.PeerHandle, "an empty sender handle")
		require.Nil(t, row.PeerNormalized, "no normalized peer")
		require.Nil(t, row.MatchedContactID, "no matched contact")
		require.Nil(t, row.InteractionID)
	}
	require.Equal(t, before, g.stateOf(t, member), "the named member gains no outbound burst and no outreach credit")
}

// spec: ING-041.drill-down-labels-senders-honestly
func TestGroupIngest_DrillDownLabelsANonContactBySenderAndTheUserAsMe(t *testing.T) {
	g := setupGroupIngest(t)
	chat := chatName(t)
	member := g.newMember(t)
	burst, _ := g.burst(t, chat, member, at(0), at(120))
	stranger, mine := g.received(chat, strangerHandle, at(30)), g.sent(chat, at(60))

	g.postAccepted(t, stranger, mine)
	g.settle(t)

	rows := g.content(t, burst.ID)
	require.Equal(t, strangerHandle, senderOf(t, rows, stranger.text).Sender)
	reply := senderOf(t, rows, mine.text)
	require.Equal(t, "Me", reply.Sender)
	require.True(t, reply.IsOutgoing)
}

// linkedRowsOf reads the live rows linked to the interaction.
func (g *groupIngestEnv) linkedRowsOf(t *testing.T, id uuid.UUID) []repository.MessagesMessage {
	t.Helper()
	rows, err := g.messagesRepo.ListByInteractionIDs(t.Context(), []uuid.UUID{id})
	require.NoError(t, err)
	return rows
}

// spec: ING-043.promotes-containing-inbound-burst
// spec: ING-043.keeps-burst-window-and-time
// spec: ING-043.starts-no-burst
func TestGroupIngest_UsersMessageInsideAnInboundBurstPromotesItToMutualAsTheExistingPromotionDoes(t *testing.T) {
	g := setupGroupIngest(t)
	chat := chatName(t)
	member := g.newMember(t)
	burst, _ := g.burst(t, chat, member, at(0), at(120))
	linkedBefore := g.linkedRowsOf(t, burst.ID)
	require.Equal(t, repository.InteractionDirectionInbound, burst.Direction)
	wantCadence := g.promotedControl(t, at(0), at(120))

	g.postAccepted(t, g.sent(chat, at(60)))

	promoted := g.interaction(t, burst.ID)
	require.Equal(t, repository.InteractionDirectionMutual, promoted.Direction)
	require.True(t, burst.OccurredAt.Equal(promoted.OccurredAt), "occurred_at is unchanged")
	require.Equal(t, linkedBefore, g.linkedRowsOf(t, burst.ID), "the linked messages and so the window are unchanged")
	require.Equal(t, wantCadence, cadenceOf(g.contact(t, member.ID)), "the cadence effect is that of the existing promotion at occurred_at")
	require.Len(t, g.interactions(t, member.ID), 1, "the message itself creates no interaction")
}

// spec: ING-043.no-credit-without-containing-burst
// spec: ING-043.starts-no-burst
func TestGroupIngest_UsersMessageOutsideAMembersWindowsChangesNothingForThatMember(t *testing.T) {
	g := setupGroupIngest(t)
	chat := chatName(t)
	containing, other, bystander := g.newMember(t), g.newMember(t), g.newMember(t)
	g.burst(t, chat, containing, at(0), at(120))
	otherBurst, _ := g.burst(t, chat, other, at(1000), at(1100))
	otherBefore, bystanderBefore := g.stateOf(t, other), g.stateOf(t, bystander)

	g.postAccepted(t, g.sent(chat, at(60)))

	require.Equal(t, otherBefore, g.stateOf(t, other), "a member whose bursts do not contain it gains no interaction, credit or cadence change")
	require.Equal(t, repository.InteractionDirectionInbound, g.interaction(t, otherBurst.ID).Direction)
	require.Equal(t, bystanderBefore, g.stateOf(t, bystander), "a member with no burst gains none")
	require.Empty(t, g.interactions(t, bystander.ID))
}

// spec: ING-043.no-credit-without-containing-burst
func TestGroupIngest_UsersMessageInsideNoWindowStartsNoBurstAndChangesNoCadence(t *testing.T) {
	g := setupGroupIngest(t)
	chat := chatName(t)
	member := g.newMember(t)
	burst, _ := g.burst(t, chat, member, at(0), at(120))
	idle := g.newMember(t)
	memberBefore, idleBefore := g.stateOf(t, member), g.stateOf(t, idle)

	g.postAccepted(t, g.sent(chat, at(5000)), g.sent(chatName(t), at(60)))
	g.settle(t)

	require.Equal(t, memberBefore, g.stateOf(t, member))
	require.Equal(t, idleBefore, g.stateOf(t, idle))
	require.Equal(t, repository.InteractionDirectionInbound, g.interaction(t, burst.ID).Direction)
	require.Empty(t, g.interactions(t, idle.ID))
}

// spec: ING-043.promotes-containing-inbound-burst
func TestGroupIngest_UsersMessageInsideTwoMembersWindowsPromotesBoth(t *testing.T) {
	g := setupGroupIngest(t)
	chat := chatName(t)
	a, b := g.newMember(t), g.newMember(t)
	burstA, _ := g.burst(t, chat, a, at(0), at(120))
	burstB, _ := g.burst(t, chat, b, at(30), at(90))

	g.postAccepted(t, g.sent(chat, at(60)))

	require.Equal(t, repository.InteractionDirectionMutual, g.interaction(t, burstA.ID).Direction)
	require.Equal(t, repository.InteractionDirectionMutual, g.interaction(t, burstB.ID).Direction)
}

// spec: ING-043.non-inbound-burst-not-promoted
func TestGroupIngest_UsersMessageLeavesMutualAndOutboundBurstsAlone(t *testing.T) {
	g := setupGroupIngest(t)
	chat := chatName(t)
	replied, outboundOnly := g.newMember(t), g.newMember(t)
	burst, _ := g.burst(t, chat, replied, at(0), at(120))
	g.postAccepted(t, g.sent(chat, at(60)))
	require.Equal(t, repository.InteractionDirectionMutual, g.interaction(t, burst.ID).Direction)
	// An outbound-only burst of a member, as the daemon attributed the user's
	// group messages before they became unattributed.
	outbound := g.seedOutboundBurst(t, chat, outboundOnly, at(0), at(120))
	mutualBefore, outboundBefore := g.stateOf(t, replied), g.stateOf(t, outboundOnly)

	g.postAccepted(t, g.sent(chat, at(70)), g.sent(chat, at(80)))
	g.settle(t)

	require.Equal(t, mutualBefore, g.stateOf(t, replied), "a burst already mutual is unchanged, its cadence fields too")
	require.Equal(t, outboundBefore, g.stateOf(t, outboundOnly), "an outbound-only burst stays outbound")
	require.Equal(t, repository.InteractionDirectionOutbound, g.interaction(t, outbound.ID).Direction)
}

// seedOutboundBurst stores attributed outbound group rows and links them to an
// outbound interaction of the member.
func (g *groupIngestEnv) seedOutboundBurst(t *testing.T, chat string, member *repository.Contact, times ...time.Time) *repository.Interaction {
	t.Helper()
	ids := make([]uuid.UUID, len(times))
	var first string
	for i, sentAt := range times {
		guid := g.newGuid()
		if i == 0 {
			first = guid
		}
		row, err := g.messagesRepo.UpsertMessage(t.Context(), repository.UpsertMessagesMessageParams{
			Guid: guid, ChatGuid: chat, PeerHandle: g.phones[member.ID], PeerNormalized: strPtr(g.phones[member.ID]),
			Text: strPtr("legacy outbound"), MessageType: "text", SentAt: sentAt, IsOutgoing: true, IsGroupChat: true,
			MatchedContactID: &member.ID, MacHostID: &g.pair.HostID,
		})
		require.NoError(t, err)
		ids[i] = row.ID
	}
	ref := "messages:" + chat + ":" + first
	interaction, err := g.interactionRepo.CreateInteraction(t.Context(), repository.CreateInteractionRequest{
		ContactID: member.ID, Source: repository.InteractionSourceMessages, SourceRef: &ref,
		OccurredAt: times[len(times)-1], Direction: repository.InteractionDirectionOutbound,
	})
	require.NoError(t, err)
	require.NoError(t, g.messagesRepo.MarkMessagesProcessed(t.Context(), ids, interaction.ID))
	return interaction
}

// spec: ING-043.promotes-containing-inbound-burst
func TestGroupIngest_WindowBoundsAreInclusiveToTheSecond(t *testing.T) {
	g := setupGroupIngest(t)
	member := g.newMember(t)
	cases := map[string]struct {
		at      time.Time
		promote bool
	}{
		"earliest message time": {at(100), true},
		"latest message time":   {at(200), true},
		"one second before":     {at(99), false},
		"one second after":      {at(201), false},
	}
	bursts := map[string]*repository.Interaction{}
	chats := map[string]string{}
	for name := range cases {
		chats[name] = chatName(t)
		bursts[name], _ = g.burst(t, chats[name], member, at(100), at(200))
	}

	for name, c := range cases {
		g.postAccepted(t, g.sent(chats[name], c.at))
	}

	for name, c := range cases {
		want := repository.InteractionDirectionInbound
		if c.promote {
			want = repository.InteractionDirectionMutual
		}
		require.Equal(t, want, g.interaction(t, bursts[name].ID).Direction, name)
	}
}

// spec: ING-043.promotion-order-independent
func TestGroupIngest_AMemberMessageThatExtendsTheBurstAroundAnEarlierUsersMessagePromotesIt(t *testing.T) {
	g := setupGroupIngest(t)
	chat := chatName(t)
	member := g.newMember(t)
	burst, _ := g.burst(t, chat, member, at(0))
	g.postAccepted(t, g.sent(chat, at(60)))
	require.Equal(t, repository.InteractionDirectionInbound, g.interaction(t, burst.ID).Direction, "the window [0,0] does not contain it yet")

	g.postAccepted(t, g.received(chat, g.phones[member.ID], at(120)))
	g.settle(t)

	promoted := g.interaction(t, burst.ID)
	require.Equal(t, repository.InteractionDirectionMutual, promoted.Direction)
	require.True(t, at(120).Equal(promoted.OccurredAt), "the extended burst keeps the occurred_at the extension gave it")
	require.Len(t, g.interactions(t, member.ID), 1)
}

// spec: ING-043.promotion-order-independent
func TestGroupIngest_AMemberBurstCreatedAroundAnEarlierUsersMessageEndsMutual(t *testing.T) {
	g := setupGroupIngest(t)
	chat := chatName(t)
	member := g.newMember(t)
	g.postAccepted(t, g.sent(chat, at(60)))
	require.Empty(t, g.interactions(t, member.ID))
	wantCadence := g.promotedControl(t, at(0), at(120))

	burst, _ := g.burst(t, chat, member, at(0), at(120))

	require.Equal(t, repository.InteractionDirectionMutual, burst.Direction)
	require.True(t, at(120).Equal(burst.OccurredAt))
	require.Equal(t, wantCadence, cadenceOf(g.contact(t, member.ID)))
}

// spec: ING-043.one-to-one-outbound-unchanged
func TestGroupIngest_OutboundOneToOneMessageStillRecordsAnOutboundInteractionWithItsOutreachEffect(t *testing.T) {
	g := setupGroupIngest(t)
	member := g.newMember(t)
	msg := g.sent("dm-"+uuid.NewString(), at(10))
	msg.group, msg.handle = false, g.phones[member.ID]

	g.postAccepted(t, msg)
	g.settle(t)

	row := g.row(t, msg.guid)
	require.Equal(t, g.phones[member.ID], row.PeerHandle)
	require.NotNil(t, row.MatchedContactID)
	require.NotNil(t, row.InteractionID)
	require.Equal(t, repository.InteractionDirectionOutbound, g.interaction(t, *row.InteractionID).Direction)
	require.NotNil(t, g.contact(t, member.ID).LastOutreachAt)
}

// spec: ING-043.no-credit-without-containing-burst
func TestGroupIngest_AnIngestedUsersMessageAgainIsADuplicateAndChangesNothing(t *testing.T) {
	g := setupGroupIngest(t)
	chat := chatName(t)
	member := g.newMember(t)
	g.burst(t, chat, member, at(0), at(120))
	mine := g.sent(chat, at(60))
	g.postAccepted(t, mine)
	g.settle(t)
	before := g.stateOf(t, member)
	rowBefore := g.row(t, mine.guid)

	again := g.post(t, mine)

	require.Equal(t, 1, again.Duplicate)
	require.Zero(t, again.Accepted)
	require.Equal(t, before, g.stateOf(t, member))
	require.Equal(t, rowBefore, g.row(t, mine.guid))
}

// spec: ING-043.one-to-one-outbound-unchanged
func TestGroupIngest_EmptyPeerHandleIsAcceptedOnlyForTheUsersGroupMessage(t *testing.T) {
	g := setupGroupIngest(t)
	chat := chatName(t)
	member := g.newMember(t)
	inboundGroup := g.received(chat, "", at(10))
	inboundOneToOne := g.received("dm-"+uuid.NewString(), "", at(10))
	inboundOneToOne.group = false
	outboundOneToOne := g.sent("dm-"+uuid.NewString(), at(10))
	outboundOneToOne.group = false
	outboundGroup := g.sent(chat, at(10))
	before := g.stateOf(t, member)

	resp := g.post(t, inboundGroup, inboundOneToOne, outboundOneToOne, outboundGroup)

	require.Equal(t, 1, resp.Accepted, "only the user's group message")
	require.Equal(t, 3, resp.Rejected)
	for _, e := range resp.Errors {
		require.Equal(t, "MISSING_FIELD", e.Code)
	}
	require.Equal(t, []int{0, 1, 2}, []int{resp.Errors[0].Index, resp.Errors[1].Index, resp.Errors[2].Index})
	require.Equal(t, before, g.stateOf(t, member))
}

// spec: ING-042.no-cadence-or-event-effect
func TestGroupIngest_AnAddedUsersMessageDoesNotPromoteTheBurstWhenAMemberMessageLaterExtendsIt(t *testing.T) {
	g := setupGroupIngest(t)
	chat := chatName(t)
	member := g.newMember(t)
	burst, _ := g.burst(t, chat, member, at(0), at(120))
	added := g.newGuid()
	require.Equal(t, []string{"added"}, g.enrich(t, addItem(added, chat, nil, "outbound", at(60), strPtr("late copy"))).outcomes())
	require.Equal(t, repository.InteractionDirectionInbound, g.interaction(t, burst.ID).Direction)

	g.postAccepted(t, g.received(chat, g.phones[member.ID], at(180)))
	g.settle(t)

	extended := g.interaction(t, burst.ID)
	require.Equal(t, repository.InteractionDirectionInbound, extended.Direction, "the added message is no reply")
	require.True(t, at(180).Equal(extended.OccurredAt))
	require.Nil(t, g.row(t, added).InteractionID)
}

// spec: ING-042.no-cadence-or-event-effect
func TestGroupIngest_AnAddedMessageFromAContactIsNeverAttachedToThatContactByLaterAggregation(t *testing.T) {
	g := setupGroupIngest(t)
	chat := chatName(t)
	member := g.newMember(t)
	burst, _ := g.burst(t, chat, member, at(0), at(120))
	linkedBefore := g.linkedRowsOf(t, burst.ID)
	before := g.stateOf(t, member)
	added := g.newGuid()

	res := g.enrich(t, addItem(added, chat, strPtr(g.phones[member.ID]), "inbound", at(60), strPtr("missed one")))
	require.Equal(t, []string{"added"}, res.outcomes())

	require.Equal(t, before, g.stateOf(t, member), "no interaction, cadence field or follow-up task changes")
	g.postAccepted(t, g.received(chat, g.phones[member.ID], at(180)))
	g.settle(t)
	row := g.row(t, added)
	require.Nil(t, row.MatchedContactID, "aggregation does not pick the added message up")
	require.Nil(t, row.InteractionID)
	require.Len(t, g.linkedRowsOf(t, burst.ID), len(linkedBefore)+1, "only the member's new message joined the burst")
	require.Contains(t, g.bodies(t, burst.ID), "missed one", "the added message is evidence inside the window")
}

// spec: ING-042.no-cadence-or-event-effect
func TestGroupIngest_AClearedOutboundRowNeverPromotesTheBurstsThatContainIt(t *testing.T) {
	g := setupGroupIngest(t)
	chat := chatName(t)
	sender, member := g.newMember(t), g.newMember(t)
	outbound := g.seedOutboundBurst(t, chat, sender, at(60))
	rows := g.linkedRowsOf(t, outbound.ID)
	require.Len(t, rows, 1)
	burst, _ := g.burst(t, chat, member, at(0), at(120))

	res := g.enrich(t, clearItem(rows[0].Guid))
	require.Equal(t, []string{"cleared"}, res.outcomes())
	g.postAccepted(t, g.received(chat, g.phones[member.ID], at(180)))
	g.settle(t)

	require.Equal(t, repository.InteractionDirectionInbound, g.interaction(t, burst.ID).Direction, "the cleared row is no reply")
	cleared := g.row(t, rows[0].Guid)
	require.Empty(t, cleared.PeerHandle)
	require.Equal(t, rows[0].InteractionID, cleared.InteractionID, "the cleared row keeps its link")
}
