package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"
	"personal-crm/backend/tests/testsupport"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// These tests observe GET /api/v1/contacts/:id/interactions and
// GET /api/v1/timeline-items/:kind/:id/content, with corrections made through
// POST /api/v1/interactions/merge and POST /api/v1/interactions/:id/removals.

type timelineIdentity struct {
	Kind string    `json:"kind"`
	ID   uuid.UUID `json:"id"`
}

type timelineMember struct {
	Source       string          `json:"source"`
	SourceRef    *string         `json:"source_ref"`
	Label        string          `json:"label"`
	ContentKind  string          `json:"content_kind"`
	MessageCount int             `json:"message_count"`
	IsGroup      bool            `json:"is_group"`
	VenueTags    []timelineVenue `json:"venue_tags"`
	Event        map[string]any  `json:"event"`
	Call         map[string]any  `json:"call"`
}

type timelineVenue struct {
	Key  string `json:"key"`
	Kind string `json:"kind"`
}

type timelineItem struct {
	Identity     timelineIdentity `json:"identity"`
	Direction    string           `json:"direction"`
	OccurredAt   time.Time        `json:"occurred_at"`
	CadenceInert bool             `json:"cadence_inert"`
	Members      []timelineMember `json:"members"`
}

type timelinePage struct {
	Items []timelineItem
	Total int
}

// timeline reads the contact's interaction list and requires 200.
func (e *correctionEnv) timeline(t *testing.T, contactID uuid.UUID, query string) timelinePage {
	t.Helper()
	w := e.get(t, "/api/v1/contacts/"+contactID.String()+"/interactions"+query)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var envelope struct {
		Data struct {
			Items []timelineItem `json:"items"`
		} `json:"data"`
		Meta struct {
			Pagination struct {
				Total int `json:"total"`
			} `json:"pagination"`
		} `json:"meta"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &envelope))
	return timelinePage{Items: envelope.Data.Items, Total: envelope.Meta.Pagination.Total}
}

func (e *correctionEnv) get(t *testing.T, path string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	e.router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
	return w
}

func memberSources(item timelineItem) []string {
	sources := make([]string, len(item.Members))
	for i, m := range item.Members {
		sources[i] = m.Source
	}
	return sources
}

// spec: IXN-033.typed-identity
// spec: IXN-033.merged-once-combined
// spec: IXN-033.members-listed
func TestTimelineList_MergedInteractionIsOneItem(t *testing.T) {
	testsupport.RequireDatabase(t)
	t.Parallel()
	e := newCorrectionEnv(t)

	// The contact's inbound call and its later outbound manual entry, on two
	// interactions merged together, give one mutual item at the later time.
	a := e.contact(t, "monthly")
	inbound := e.call(t, a, e.ref("call"), repository.InteractionDirectionInbound, e.at(0))
	outbound := e.record(t, a, repository.InteractionSourceManual, "", repository.InteractionDirectionOutbound, e.at(3))
	e.merge(t, a, inbound, *outbound.InteractionNodeID)

	page := e.timeline(t, a, "")

	require.Equal(t, 1, page.Total)
	require.Len(t, page.Items, 1)
	item := page.Items[0]
	require.Equal(t, timelineIdentity{Kind: "interaction", ID: inbound}, item.Identity)
	require.Equal(t, repository.InteractionDirectionMutual, item.Direction)
	require.True(t, item.OccurredAt.Equal(e.at(3)))
	require.False(t, item.CadenceInert)
	require.Equal(t, []string{repository.InteractionSourcePhoneCalls, repository.InteractionSourceManual}, memberSources(item))
}

// emailRef is the source ref of a contact's email observation of a thread on a
// day: observations of one thread and day share an interaction node.
func emailRef(contactID uuid.UUID, thread, day string) string {
	return contactID.String() + ":" + thread + ":" + day
}

// linkEmail stores an email message in a thread, sent when the observation
// occurred, and links it to the observation.
func (e *correctionEnv) linkEmail(t *testing.T, observation *repository.Interaction, thread, external string) repository.CommsMessage {
	t.Helper()
	return e.linkEmailSentAt(t, observation, thread, external, observation.OccurredAt)
}

// linkEmailSentAt stores an email message in a thread, sent at sentAt, and links
// it to an observation.
func (e *correctionEnv) linkEmailSentAt(t *testing.T, observation *repository.Interaction, thread, external string, sentAt time.Time) repository.CommsMessage {
	t.Helper()
	comms := repository.NewCommsMessageRepository(e.database.Queries)
	subject, body, peer := "Synthetic subject "+thread, "Synthetic body "+external, "synthetic-peer"
	row, err := comms.UpsertMessage(e.ctx, repository.UpsertCommsMessageParams{
		Source: repository.InteractionSourceEmail, ExternalID: external, ThreadID: &thread, Subject: &subject, Body: &body,
		PeerHandle: &peer, Direction: repository.InteractionDirectionInbound, SentAt: sentAt, MatchedContactID: observation.ContactID,
	})
	require.NoError(t, err)
	require.NoError(t, comms.MarkMessagesProcessed(e.ctx, []uuid.UUID{row.ID}, observation.ID))
	return *row
}

// spec: IXN-033.members-listed
func TestTimelineList_AttendeeOnlyOnEarlierMember(t *testing.T) {
	testsupport.RequireDatabase(t)
	t.Parallel()
	e := newCorrectionEnv(t)

	// A is on both members. B is on the first only, and C is on the second only,
	// earlier than A.
	a, b, c := e.contact(t, "monthly"), e.contact(t, "monthly"), e.contact(t, "monthly")
	thread1, thread2, day := e.ref("thread"), e.ref("thread"), "2026-01-02"
	aFirst := e.record(t, a, repository.InteractionSourceEmail, emailRef(a, thread1, day), repository.InteractionDirectionInbound, e.at(0))
	bFirst := e.record(t, b, repository.InteractionSourceEmail, emailRef(b, thread1, day), repository.InteractionDirectionInbound, e.at(0))
	aSecond := e.record(t, a, repository.InteractionSourceEmail, emailRef(a, thread2, day), repository.InteractionDirectionInbound, e.at(5))
	cSecond := e.record(t, c, repository.InteractionSourceEmail, emailRef(c, thread2, day), repository.InteractionDirectionInbound, e.at(2))
	e.merge(t, a, *aFirst.InteractionNodeID, *aSecond.InteractionNodeID)

	forB := e.timeline(t, b, "").Items
	forA := e.timeline(t, a, "").Items

	require.Len(t, forB, 1)
	require.Equal(t, timelineIdentity{Kind: "interaction", ID: *aFirst.InteractionNodeID}, forB[0].Identity)
	require.Equal(t, repository.InteractionDirectionInbound, forB[0].Direction)
	require.True(t, forB[0].OccurredAt.Equal(e.at(0)))
	require.Equal(t, []string{*bFirst.SourceRef, *cSecond.SourceRef}, memberRefs(forB[0]), "B's own observation, and the unobserved member's earliest")
	require.Len(t, forA, 1)
	require.True(t, forA[0].OccurredAt.Equal(e.at(5)))
	require.Equal(t, []string{*aFirst.SourceRef, *aSecond.SourceRef}, memberRefs(forA[0]), "the context contact's own observation of each member")
}

func memberRefs(item timelineItem) []string {
	refs := make([]string, len(item.Members))
	for i, m := range item.Members {
		refs[i] = *m.SourceRef
	}
	return refs
}

// spec: IXN-033.removed-absent
func TestTimelineList_RemovedParticipationIsAbsent(t *testing.T) {
	testsupport.RequireDatabase(t)
	t.Parallel()
	e := newCorrectionEnv(t)

	a, b := e.contact(t, "monthly"), e.contact(t, "monthly")
	ref := e.event(t)
	node := e.meeting(t, a, ref, e.at(0))
	e.meeting(t, b, ref, e.at(0))
	e.remove(t, node, removalBody(service.RemovalReasonDidNotAttend, a))

	require.Empty(t, e.timeline(t, a, "").Items)
	require.Equal(t, 0, e.timeline(t, a, "").Total)
	forB := e.timeline(t, b, "").Items
	require.Len(t, forB, 1)
	require.Equal(t, node, forB[0].Identity.ID)
}

// spec: IXN-033.typed-identity
func TestTimelineList_UncorrectedCalendarObservation(t *testing.T) {
	testsupport.RequireDatabase(t)
	t.Parallel()
	e := newCorrectionEnv(t)

	a := e.contact(t, "monthly")
	ref := e.event(t)
	node := e.meeting(t, a, ref, e.at(0))
	event, err := repository.NewCalendarEventRepository(e.database.Queries).GetByID(e.ctx, uuid.MustParse(ref))
	require.NoError(t, err)

	items := e.timeline(t, a, "").Items

	require.Len(t, items, 1)
	require.Equal(t, timelineIdentity{Kind: "interaction", ID: node}, items[0].Identity)
	require.Len(t, items[0].Members, 1)
	member := items[0].Members[0]
	require.Equal(t, repository.InteractionSourceGCal, member.Source)
	require.Equal(t, ref, *member.SourceRef)
	require.Equal(t, *event.Title, member.Label)
	require.Equal(t, "none", member.ContentKind)
	require.Equal(t, 0, member.MessageCount)
	require.False(t, member.IsGroup)
	require.Empty(t, member.VenueTags)
	require.Equal(t, *event.Title, member.Event["title"])
	require.Nil(t, member.Call)
}

// spec: IXN-033.typed-identity
func TestTimelineList_BurstAndMissedCallKinds(t *testing.T) {
	testsupport.RequireDatabase(t)
	t.Parallel()
	e := newCorrectionEnv(t)

	a, other := e.contact(t, "monthly"), e.contact(t, "monthly")
	burst := e.record(t, a, repository.InteractionSourceGChat, e.ref("gchat"), repository.InteractionDirectionInbound, e.at(1))
	phones := repository.NewPhoneCallRepository(e.database.Queries)
	answered := false
	missed, err := phones.UpsertCall(e.ctx, repository.UpsertPhoneCallParams{
		CallUniqueID: e.ref("missed"), PeerHandle: "synthetic-peer", PeerNormalized: "synthetic-peer", Service: repository.PhoneCallServiceVoice,
		Direction: repository.PhoneCallDirectionInbound, Answered: &answered, StartedAt: e.at(2), MatchedContactID: &a,
	})
	require.NoError(t, err)
	require.NoError(t, phones.MarkProcessed(e.ctx, repository.MarkProcessedParams{ID: missed.ID}))
	_, err = phones.UpsertCall(e.ctx, repository.UpsertPhoneCallParams{
		CallUniqueID: e.ref("missed"), PeerHandle: "synthetic-peer", PeerNormalized: "synthetic-peer", Service: repository.PhoneCallServiceVoice,
		Direction: repository.PhoneCallDirectionInbound, Answered: &answered, StartedAt: e.at(3), MatchedContactID: &other,
	})
	require.NoError(t, err)

	items := e.timeline(t, a, "").Items

	require.Len(t, items, 2)
	require.Equal(t, timelineIdentity{Kind: "missed_call", ID: missed.ID}, items[0].Identity)
	require.True(t, items[0].CadenceInert)
	require.Equal(t, repository.InteractionDirectionInbound, items[0].Direction)
	require.True(t, items[0].OccurredAt.Equal(e.at(2)))
	require.Equal(t, []string{repository.InteractionSourcePhoneCalls}, memberSources(items[0]))
	require.Equal(t, "Missed call", items[0].Members[0].Label)
	require.Equal(t, false, items[0].Members[0].Call["has_voicemail"])
	require.Equal(t, timelineIdentity{Kind: "message_burst", ID: burst.ID}, items[1].Identity)
	require.False(t, items[1].CadenceInert)
	require.Equal(t, []string{repository.InteractionSourceGChat}, memberSources(items[1]))
}

// spec: IXN-033.unmerged-distinct
func TestTimelineList_UnmergedDuplicatesStayTwoItems(t *testing.T) {
	testsupport.RequireDatabase(t)
	t.Parallel()
	e := newCorrectionEnv(t)

	a := e.contact(t, "monthly")
	x := e.meeting(t, a, e.event(t), e.at(0))
	y := e.meeting(t, a, e.event(t), e.at(0))

	page := e.timeline(t, a, "")

	require.Equal(t, 2, page.Total)
	require.ElementsMatch(t, []uuid.UUID{x, y}, []uuid.UUID{page.Items[0].Identity.ID, page.Items[1].Identity.ID})
	require.Greater(t, page.Items[0].Identity.ID.String(), page.Items[1].Identity.ID.String(), "equal times order by identity id, descending")
}

// spec: IXN-033.effective-time-order-and-filters
func TestTimelineList_RangeBoundsItemsByEffectiveTime(t *testing.T) {
	testsupport.RequireDatabase(t)
	t.Parallel()
	e := newCorrectionEnv(t)

	// One merged item whose members are at hour 0 and hour 3: it occurs at hour 3.
	a := e.contact(t, "monthly")
	early := e.call(t, a, e.ref("call"), repository.InteractionDirectionInbound, e.at(0))
	late := e.call(t, a, e.ref("call"), repository.InteractionDirectionInbound, e.at(3))
	e.merge(t, a, early, late)
	between := func(from, to int) string { return "?from=" + rfc3339(e.at(from)) + "&to=" + rfc3339(e.at(to)) }

	require.Equal(t, 1, e.timeline(t, a, between(2, 4)).Total, "a range holding the item's time but not its earlier member's keeps it, counted once")
	require.Len(t, e.timeline(t, a, between(2, 4)).Items, 1)
	require.Equal(t, 0, e.timeline(t, a, between(-1, 1)).Total, "a range holding the earlier member's time but not the item's drops it")
	require.Equal(t, 0, e.timeline(t, a, between(4, 5)).Total, "an item before from is dropped")
	require.Equal(t, 1, e.timeline(t, a, between(3, 4)).Total, "an item at from is kept")
	require.Equal(t, 0, e.timeline(t, a, between(2, 3)).Total, "an item at to is dropped")
}

// spec: IXN-033.effective-time-order-and-filters
func TestTimelineList_VenueFilterReadsEveryMembersContainer(t *testing.T) {
	testsupport.RequireDatabase(t)
	t.Parallel()
	e := newCorrectionEnv(t)

	a := e.contact(t, "monthly")
	thread1, thread2, day := e.ref("thread"), e.ref("thread"), "2026-01-02"
	first := e.record(t, a, repository.InteractionSourceEmail, emailRef(a, thread1, day), repository.InteractionDirectionInbound, e.at(0))
	second := e.record(t, a, repository.InteractionSourceEmail, emailRef(a, thread2, day), repository.InteractionDirectionInbound, e.at(1))
	e.linkEmail(t, first, thread1, e.ref("message"))
	e.linkEmail(t, second, thread2, e.ref("message"))
	e.merge(t, a, *first.InteractionNodeID, *second.InteractionNodeID)
	phones := repository.NewPhoneCallRepository(e.database.Queries)
	answered := false
	missed, err := phones.UpsertCall(e.ctx, repository.UpsertPhoneCallParams{
		CallUniqueID: e.ref("missed"), PeerHandle: "synthetic-peer", PeerNormalized: "synthetic-peer", Service: repository.PhoneCallServiceVoice,
		Direction: repository.PhoneCallDirectionInbound, Answered: &answered, StartedAt: e.at(2), MatchedContactID: &a,
	})
	require.NoError(t, err)
	require.NoError(t, phones.MarkProcessed(e.ctx, repository.MarkProcessedParams{ID: missed.ID}))

	require.Equal(t, 2, e.timeline(t, a, "").Total)
	for _, thread := range []string{thread1, thread2} {
		page := e.timeline(t, a, "?venue=email:"+thread)
		require.Equal(t, 1, page.Total, "a venue holding any member's container keeps the item, and drops the missed call")
		require.Equal(t, "interaction", page.Items[0].Identity.Kind)
	}
	require.Equal(t, 0, e.timeline(t, a, "?venue=email:"+e.ref("thread")).Total)
}

// spec: IXN-033.typed-identity
func TestTimelineList_ItemNamesItsIdentityOnce(t *testing.T) {
	testsupport.RequireDatabase(t)
	t.Parallel()
	e := newCorrectionEnv(t)

	a := e.contact(t, "monthly")
	e.call(t, a, e.ref("call"), repository.InteractionDirectionInbound, e.at(0))

	w := e.get(t, "/api/v1/contacts/"+a.String()+"/interactions")
	require.Equal(t, http.StatusOK, w.Code)
	var envelope struct {
		Data struct {
			Items []map[string]json.RawMessage `json:"items"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &envelope))

	require.Len(t, envelope.Data.Items, 1)
	require.ElementsMatch(t, []string{"identity", "direction", "occurred_at", "cadence_inert", "members"}, keysOf(envelope.Data.Items[0]))
	var members []map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(envelope.Data.Items[0]["members"], &members))
	require.Len(t, members, 1)
	require.ElementsMatch(t, []string{"source", "source_ref", "label", "content_kind", "message_count", "is_group", "venue_tags"}, keysOf(members[0]))
}

func keysOf(m map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

type contentMessage struct {
	ID         uuid.UUID `json:"id"`
	Sender     string    `json:"sender"`
	IsOutgoing bool      `json:"is_outgoing"`
	Body       string    `json:"body"`
	VenueKey   string    `json:"venue_key"`
	Source     string    `json:"source"`
	Channel    *string   `json:"channel"`
	Container  *string   `json:"container"`
	ContentRef string    `json:"content_ref"`
}

type contentNote struct {
	Title      *string `json:"title"`
	Summary    *string `json:"summary"`
	Memo       *string `json:"memo"`
	Source     string  `json:"source"`
	Channel    string  `json:"channel"`
	Container  *string `json:"container"`
	ContentRef string  `json:"content_ref"`
}

type contentMember struct {
	Source       string           `json:"source"`
	Kind         string           `json:"kind"`
	Messages     []contentMessage `json:"messages"`
	MeetingNotes []contentNote    `json:"meeting_notes"`
}

type contentAnswer struct {
	Identity timelineIdentity `json:"identity"`
	Members  []contentMember  `json:"members"`
}

// contentPath is the content read of the typed identity in the context of a
// contact.
func contentPath(kind string, id, contactID uuid.UUID) string {
	return "/api/v1/timeline-items/" + kind + "/" + id.String() + "/content?contact_id=" + contactID.String()
}

// content reads a timeline item's content and requires 200.
func (e *correctionEnv) content(t *testing.T, kind string, id, contactID uuid.UUID) contentAnswer {
	t.Helper()
	w := e.get(t, contentPath(kind, id, contactID))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var envelope struct {
		Data contentAnswer `json:"data"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &envelope))
	return envelope.Data
}

// linkNote stores a meeting note for an Anarlog session, linked to a calendar
// event when eventID is given.
func (e *correctionEnv) linkNote(t *testing.T, sessionID uuid.UUID, eventID *uuid.UUID, title string) {
	t.Helper()
	var linkedKind *string
	if eventID != nil {
		kind := repository.LinkedKindEvent
		linkedKind = &kind
	}
	summary, memo := title+" summary", title+" memo"
	tx, err := e.database.Pool.Begin(e.ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(e.ctx) }()
	_, err = repository.NewMeetingNoteRepository(e.database.Queries).InsertMeetingNoteTx(e.ctx, tx, repository.InsertMeetingNoteParams{
		AnarlogSessionID: sessionID, Title: &title, Summary: &summary, Memo: &memo, LinkedKind: linkedKind, LinkedID: eventID,
		LinkageState: repository.LinkageStateLinked, MeetingAt: e.at(0),
	})
	require.NoError(t, err)
	require.NoError(t, tx.Commit(e.ctx))
}

// meetingVenueKey is the venue key of a calendar event's meeting.
func (e *correctionEnv) meetingVenueKey(t *testing.T, eventRef string) string {
	t.Helper()
	event, err := repository.NewCalendarEventRepository(e.database.Queries).GetByID(e.ctx, uuid.MustParse(eventRef))
	require.NoError(t, err)
	return "gcal:" + repository.GCalVenueContainerID(event.GcalEventID, event.GcalCalendarID, event.GoogleAccountID)
}

// spec: IXN-034.members-grouped
// spec: IXN-034.item-attribution
func TestTimelineContent_CalendarMergedWithCall(t *testing.T) {
	testsupport.RequireDatabase(t)
	t.Parallel()
	e := newCorrectionEnv(t)

	a := e.contact(t, "monthly")
	ref := e.event(t)
	meeting := e.meeting(t, a, ref, e.at(0))
	e.linkNote(t, uuid.New(), ptr(uuid.MustParse(ref)), "First note")
	e.linkNote(t, uuid.New(), ptr(uuid.MustParse(ref)), "Second note")
	call := e.call(t, a, e.ref("call"), repository.InteractionDirectionInbound, e.at(2))
	e.merge(t, a, meeting, call)

	answer := e.content(t, "interaction", meeting, a)

	require.Equal(t, timelineIdentity{Kind: "interaction", ID: meeting}, answer.Identity)
	require.Len(t, answer.Members, 2)
	calendar, phone := answer.Members[0], answer.Members[1]
	require.Equal(t, repository.InteractionSourceGCal, calendar.Source)
	require.Equal(t, "meeting_note", calendar.Kind)
	require.Len(t, calendar.MeetingNotes, 2)
	for _, note := range calendar.MeetingNotes {
		require.Equal(t, repository.InteractionSourceAnarlogSessions, note.Source)
		require.Equal(t, "meeting", note.Channel)
		require.Equal(t, e.meetingVenueKey(t, ref), *note.Container)
		require.NotEmpty(t, note.ContentRef)
	}
	require.Equal(t, repository.InteractionSourcePhoneCalls, phone.Source)
	require.Equal(t, "call", phone.Kind)
	require.Empty(t, phone.Messages)
	require.Empty(t, phone.MeetingNotes)
	require.NotNil(t, phone.Messages, "empty arrays, not null")
}

func ptr[T any](v T) *T { return &v }

// spec: IXN-034.no-alias-container
// spec: IXN-034.members-grouped
func TestTimelineContent_MergedEmailsKeepTheirOwnThreads(t *testing.T) {
	testsupport.RequireDatabase(t)
	t.Parallel()
	e := newCorrectionEnv(t)

	a := e.contact(t, "monthly")
	thread1, thread2, day := e.ref("thread"), e.ref("thread"), "2026-01-02"
	first := e.record(t, a, repository.InteractionSourceEmail, emailRef(a, thread1, day), repository.InteractionDirectionInbound, e.at(0))
	second := e.record(t, a, repository.InteractionSourceEmail, emailRef(a, thread2, day), repository.InteractionDirectionInbound, e.at(1))
	e.linkEmail(t, first, thread1, e.ref("message"))
	e.linkEmail(t, first, thread1, e.ref("message"))
	e.linkEmail(t, second, thread2, e.ref("message"))
	e.merge(t, a, *first.InteractionNodeID, *second.InteractionNodeID)

	answer := e.content(t, "interaction", *first.InteractionNodeID, a)

	require.Len(t, answer.Members, 2)
	for i, thread := range []string{thread1, thread2} {
		member := answer.Members[i]
		require.Equal(t, "messages", member.Kind)
		require.Len(t, member.Messages, map[int]int{0: 2, 1: 1}[i])
		seen := map[uuid.UUID]bool{}
		for _, message := range member.Messages {
			require.Equal(t, repository.InteractionSourceEmail, message.Source)
			require.Equal(t, "email_thread", *message.Channel)
			require.Equal(t, "email:"+thread, *message.Container)
			require.Equal(t, message.VenueKey, *message.Container)
			require.False(t, seen[message.ID], "each message once within its member")
			seen[message.ID] = true
		}
	}
}

// spec: IXN-034.item-attribution
func TestTimelineContent_BurstChannels(t *testing.T) {
	testsupport.RequireDatabase(t)
	t.Parallel()
	e := newCorrectionEnv(t)

	a := e.contact(t, "monthly")
	group := e.record(t, a, repository.InteractionSourceGChat, e.ref("gchat"), repository.InteractionDirectionInbound, e.at(0))
	space := e.ref("space")
	comms := repository.NewCommsMessageRepository(e.database.Queries)
	body, peer := "Synthetic group body", "synthetic-peer"
	groupMessage, err := comms.UpsertMessage(e.ctx, repository.UpsertCommsMessageParams{
		Source: repository.InteractionSourceGChat, ExternalID: e.ref("message"), ThreadID: &space, Body: &body, PeerHandle: &peer,
		Direction: repository.InteractionDirectionInbound, SentAt: e.at(0), MatchedContactID: a,
	})
	require.NoError(t, err)
	require.NoError(t, comms.MarkMessagesProcessed(e.ctx, []uuid.UUID{groupMessage.ID}, group.ID))
	oneToOne := e.record(t, a, repository.InteractionSourceMessages, e.ref("messages"), repository.InteractionDirectionOutbound, e.at(1))
	chat := e.ref("chat")
	messages := repository.NewMessagesMessageRepository(e.database.Queries)
	text := "Synthetic direct body"
	direct, err := messages.UpsertMessage(e.ctx, repository.UpsertMessagesMessageParams{
		Guid: e.ref("guid"), ChatGuid: chat, PeerHandle: "synthetic-peer", Text: &text, MessageType: "text", SentAt: e.at(1), MatchedContactID: &a,
	})
	require.NoError(t, err)
	require.NoError(t, messages.MarkMessagesProcessed(e.ctx, []uuid.UUID{direct.ID}, oneToOne.ID))

	groupAnswer := e.content(t, "message_burst", group.ID, a)
	directAnswer := e.content(t, "message_burst", oneToOne.ID, a)

	require.Equal(t, timelineIdentity{Kind: "message_burst", ID: group.ID}, groupAnswer.Identity)
	require.Len(t, groupAnswer.Members, 1)
	require.Equal(t, repository.InteractionSourceGChat, groupAnswer.Members[0].Source)
	require.Len(t, groupAnswer.Members[0].Messages, 1)
	require.Equal(t, "group_chat", *groupAnswer.Members[0].Messages[0].Channel)
	require.Equal(t, "gchat:"+space, *groupAnswer.Members[0].Messages[0].Container)
	require.Len(t, directAnswer.Members[0].Messages, 1)
	require.Equal(t, "dm", *directAnswer.Members[0].Messages[0].Channel)
	require.Equal(t, "messages:"+chat, *directAnswer.Members[0].Messages[0].Container)
	require.Equal(t, repository.InteractionSourceMessages, directAnswer.Members[0].Messages[0].Source)
}

// spec: IXN-034.item-attribution
func TestTimelineContent_UnlinkedSessionNote(t *testing.T) {
	testsupport.RequireDatabase(t)
	t.Parallel()
	e := newCorrectionEnv(t)

	a := e.contact(t, "monthly")
	session := uuid.New()
	node := e.record(t, a, repository.InteractionSourceAnarlogSessions, "anarlog:"+session.String()+":"+a.String(), repository.InteractionDirectionMutual, e.at(0))
	e.linkNote(t, session, nil, "Session note")

	answer := e.content(t, "interaction", *node.InteractionNodeID, a)

	require.Len(t, answer.Members, 1)
	require.Len(t, answer.Members[0].MeetingNotes, 1)
	note := answer.Members[0].MeetingNotes[0]
	require.Equal(t, "session", note.Channel)
	require.Equal(t, "anarlog_sessions:"+session.String(), *note.Container)
	require.Equal(t, repository.InteractionSourceAnarlogSessions, note.Source)
}

// spec: IXN-034.item-attribution
func TestTimelineContent_MessageWithoutContainerHasNullChannel(t *testing.T) {
	testsupport.RequireDatabase(t)
	t.Parallel()
	e := newCorrectionEnv(t)

	a := e.contact(t, "monthly")
	observation := e.record(t, a, repository.InteractionSourceEmail, emailRef(a, e.ref("thread"), "2026-01-02"), repository.InteractionDirectionInbound, e.at(0))
	_, err := e.database.Queries.TestInsertCommsMessageLinked(e.ctx, db.TestInsertCommsMessageLinkedParams{
		Source: repository.InteractionSourceEmail, ExternalID: e.ref("message"), Direction: repository.InteractionDirectionInbound,
		SentAt: e.at(0), MatchedContactID: &a, InteractionID: &observation.ID,
	})
	require.NoError(t, err)

	answer := e.content(t, "interaction", *observation.InteractionNodeID, a)

	require.Len(t, answer.Members[0].Messages, 1)
	message := answer.Members[0].Messages[0]
	require.Empty(t, message.VenueKey)
	require.Nil(t, message.Channel)
	require.Nil(t, message.Container)
	require.Equal(t, repository.InteractionSourceEmail, message.Source)
}

// spec: IXN-034.merged-away-id
func TestTimelineContent_MergedAwayIdReadsTheRoot(t *testing.T) {
	testsupport.RequireDatabase(t)
	t.Parallel()
	e := newCorrectionEnv(t)

	a := e.contact(t, "monthly")
	thread1, thread2, day := e.ref("thread"), e.ref("thread"), "2026-01-02"
	first := e.record(t, a, repository.InteractionSourceEmail, emailRef(a, thread1, day), repository.InteractionDirectionInbound, e.at(0))
	second := e.record(t, a, repository.InteractionSourceEmail, emailRef(a, thread2, day), repository.InteractionDirectionInbound, e.at(1))
	e.linkEmail(t, first, thread1, e.ref("message"))
	e.linkEmail(t, second, thread2, e.ref("message"))
	e.merge(t, a, *first.InteractionNodeID, *second.InteractionNodeID)

	byRoot := e.content(t, "interaction", *first.InteractionNodeID, a)
	byLoser := e.content(t, "interaction", *second.InteractionNodeID, a)

	require.Equal(t, *first.InteractionNodeID, byLoser.Identity.ID)
	require.Equal(t, byRoot, byLoser)
}

// spec: IXN-034.stable-content-ref
func TestTimelineContent_ContentRefIsStable(t *testing.T) {
	testsupport.RequireDatabase(t)
	t.Parallel()
	e := newCorrectionEnv(t)

	// Two contacts hold their own copies of one email message.
	a, b := e.contact(t, "monthly"), e.contact(t, "monthly")
	thread, day, external := e.ref("thread"), "2026-01-02", e.ref("message")
	aObservation := e.record(t, a, repository.InteractionSourceEmail, emailRef(a, thread, day), repository.InteractionDirectionInbound, e.at(0))
	bObservation := e.record(t, b, repository.InteractionSourceEmail, emailRef(b, thread, day), repository.InteractionDirectionInbound, e.at(0))
	e.linkEmail(t, aObservation, thread, external)
	e.linkEmail(t, bObservation, thread, external)
	other := e.record(t, a, repository.InteractionSourceManual, "", repository.InteractionDirectionOutbound, e.at(1))

	before := e.content(t, "interaction", *aObservation.InteractionNodeID, a)
	e.merge(t, a, *aObservation.InteractionNodeID, *other.InteractionNodeID)
	after := e.content(t, "interaction", *aObservation.InteractionNodeID, a)
	forB := e.content(t, "interaction", *aObservation.InteractionNodeID, b)

	require.Len(t, before.Members[0].Messages, 1)
	require.NotEmpty(t, before.Members[0].Messages[0].ContentRef)
	require.Equal(t, before.Members[0].Messages[0].ContentRef, after.Members[0].Messages[0].ContentRef)
	require.NotEqual(t, before.Members[0].Messages[0].ID, forB.Members[0].Messages[0].ID, "B reads its own copy")
	require.Equal(t, before.Members[0].Messages[0].ContentRef, forB.Members[0].Messages[0].ContentRef)
}

// spec: IXN-034.members-grouped
func TestTimelineContent_UnmergedMemberIsTheBaseRead(t *testing.T) {
	testsupport.RequireDatabase(t)
	t.Parallel()
	e := newCorrectionEnv(t)

	a := e.contact(t, "monthly")
	thread := e.ref("thread")
	observation := e.record(t, a, repository.InteractionSourceEmail, emailRef(a, thread, "2026-01-02"), repository.InteractionDirectionInbound, e.at(0))
	e.linkEmail(t, observation, thread, e.ref("message"))
	e.linkEmail(t, observation, thread, e.ref("message"))
	base, err := service.NewInteractionContentService(e.interactions, repository.NewCommsMessageRepository(e.database.Queries),
		repository.NewTelegramMessageRepository(e.database.Queries), repository.NewMessagesMessageRepository(e.database.Queries),
		repository.NewMeetingNoteRepository(e.database.Queries), repository.NewCalendarEventRepository(e.database.Queries),
		repository.NewPhoneCallRepository(e.database.Queries), e.contactRepo).GetContent(e.ctx, observation.ID)
	require.NoError(t, err)

	answer := e.content(t, "interaction", *observation.InteractionNodeID, a)

	require.Len(t, answer.Members, 1)
	require.Equal(t, base.Kind, answer.Members[0].Kind)
	require.Len(t, answer.Members[0].Messages, len(base.Messages))
	for i, want := range base.Messages {
		got := answer.Members[0].Messages[i]
		require.Equal(t, want.ID, got.ID)
		require.Equal(t, want.Sender, got.Sender)
		require.Equal(t, want.IsOutgoing, got.IsOutgoing)
		require.Equal(t, want.Body, got.Body)
		require.Equal(t, want.VenueKey, got.VenueKey)
	}
}

// spec: IXN-034.other-kinds
func TestTimelineContent_OtherKinds(t *testing.T) {
	testsupport.RequireDatabase(t)
	t.Parallel()
	e := newCorrectionEnv(t)

	a := e.contact(t, "monthly")
	phones := repository.NewPhoneCallRepository(e.database.Queries)
	answered := false
	missed, err := phones.UpsertCall(e.ctx, repository.UpsertPhoneCallParams{
		CallUniqueID: e.ref("missed"), PeerHandle: "synthetic-peer", PeerNormalized: "synthetic-peer", Service: repository.PhoneCallServiceVoice,
		Direction: repository.PhoneCallDirectionInbound, Answered: &answered, StartedAt: e.at(0), MatchedContactID: &a,
	})
	require.NoError(t, err)
	require.NoError(t, phones.MarkProcessed(e.ctx, repository.MarkProcessedParams{ID: missed.ID}))
	noted, bare := e.event(t), e.event(t)
	e.linkNote(t, uuid.New(), ptr(uuid.MustParse(noted)), "Upcoming note")

	call := e.content(t, "missed_call", missed.ID, a)
	withNote := e.content(t, "upcoming_event", uuid.MustParse(noted), a)
	without := e.content(t, "upcoming_event", uuid.MustParse(bare), a)

	require.Equal(t, timelineIdentity{Kind: "missed_call", ID: missed.ID}, call.Identity)
	require.Equal(t, []contentMember{{Source: repository.InteractionSourcePhoneCalls, Kind: "call", Messages: []contentMessage{}, MeetingNotes: []contentNote{}}}, call.Members)
	require.Equal(t, timelineIdentity{Kind: "upcoming_event", ID: uuid.MustParse(noted)}, withNote.Identity)
	require.Len(t, withNote.Members, 1)
	require.Equal(t, repository.InteractionSourceGCal, withNote.Members[0].Source)
	require.Equal(t, "meeting_note", withNote.Members[0].Kind)
	require.Len(t, withNote.Members[0].MeetingNotes, 1)
	require.Equal(t, "meeting", withNote.Members[0].MeetingNotes[0].Channel)
	require.Equal(t, e.meetingVenueKey(t, noted), *withNote.Members[0].MeetingNotes[0].Container)
	require.Equal(t, "none", without.Members[0].Kind)
	require.Empty(t, without.Members[0].MeetingNotes)
}

// spec: IXN-034.invalid-400
func TestTimelineContent_InvalidRequestsAre400(t *testing.T) {
	testsupport.RequireDatabase(t)
	t.Parallel()
	e := newCorrectionEnv(t)

	a := e.contact(t, "monthly")
	id := uuid.New().String()
	for name, path := range map[string]string{
		"unknown kind":       "/api/v1/timeline-items/meeting/" + id + "/content?contact_id=" + a.String(),
		"malformed id":       "/api/v1/timeline-items/interaction/not-a-uuid/content?contact_id=" + a.String(),
		"missing contact_id": "/api/v1/timeline-items/interaction/" + id + "/content",
		"malformed contact":  "/api/v1/timeline-items/interaction/" + id + "/content?contact_id=not-a-uuid",
	} {
		require.Equal(t, http.StatusBadRequest, e.get(t, path).Code, name)
	}
}

// spec: IXN-034.not-found-404
func TestTimelineContent_UnknownItemsAre404(t *testing.T) {
	testsupport.RequireDatabase(t)
	t.Parallel()
	e := newCorrectionEnv(t)

	a := e.contact(t, "monthly")
	node := e.meeting(t, a, e.event(t), e.at(0))
	removedNode := e.record(t, a, repository.InteractionSourceManual, "", repository.InteractionDirectionOutbound, e.at(1))
	require.NoError(t, e.interactions.SoftDeleteInteraction(e.ctx, removedNode.ID))
	burst := e.record(t, a, repository.InteractionSourceGChat, e.ref("gchat"), repository.InteractionDirectionInbound, e.at(2))
	require.NoError(t, e.interactions.SoftDeleteInteraction(e.ctx, burst.ID))
	deleted := e.contact(t, "monthly")
	require.NoError(t, e.contactRepo.SoftDeleteContact(e.ctx, deleted))

	for name, path := range map[string]string{
		"no such interaction":                  contentPath("interaction", uuid.New(), a),
		"a node that is not an interaction":    contentPath("interaction", a, a),
		"an interaction with no live member":   contentPath("interaction", *removedNode.InteractionNodeID, a),
		"a soft-deleted burst":                 contentPath("message_burst", burst.ID, a),
		"a burst id that names an interaction": contentPath("message_burst", node, a),
		"no such missed call":                  contentPath("missed_call", uuid.New(), a),
		"no such upcoming event":               contentPath("upcoming_event", uuid.New(), a),
		"a contact that does not exist":        contentPath("interaction", node, uuid.New()),
		"a soft-deleted participation context": contentPath("interaction", node, deleted),
	} {
		require.Equal(t, http.StatusNotFound, e.get(t, path).Code, name)
	}
}
