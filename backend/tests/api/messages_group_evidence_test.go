package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"personal-crm/backend/internal/api/handlers"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// groupEnv extends the enrichment env with group-chat seeding: members with
// linked bursts, built straight on the repositories so a test can state the
// stored state it starts from.
type groupEnv struct {
	*enrichmentEnv
	seq             int
	contactRepo     *repository.ContactRepository
	interactionRepo *repository.InteractionRepository
}

func setupGroupEnv(t *testing.T) *groupEnv {
	t.Helper()
	e := setupEnrichmentEnv(t)
	contactRepo := repository.NewContactRepository(e.database.Queries)
	contactRepo.SetPool(e.database.Pool)
	interactionRepo := repository.NewInteractionRepository(e.database.Queries)
	queries := e.database.Queries
	// The content read route, so a test can observe what an enrichment write
	// did to a burst's drill-down through the same router.
	content := service.NewInteractionContentService(interactionRepo, repository.NewCommsMessageRepository(queries),
		repository.NewTelegramMessageRepository(queries), e.messages, repository.NewMeetingNoteRepository(queries),
		repository.NewCalendarEventRepository(queries), repository.NewPhoneCallRepository(queries), contactRepo)
	handlers.RegisterContactRoutes(e.router.Group("/api/v1"), handlers.ContactRouteDeps{
		Contact: &handlers.ContactHandler{}, Interaction: handlers.NewInteractionHandler(nil, content),
		Note: &handlers.NoteHandler{}, ContactMethod: &handlers.ContactMethodHandler{},
	})
	return &groupEnv{enrichmentEnv: e, contactRepo: contactRepo, interactionRepo: interactionRepo}
}

type contentRow struct {
	ID         string    `json:"id"`
	Sender     string    `json:"sender"`
	IsOutgoing bool      `json:"is_outgoing"`
	SentAt     time.Time `json:"sent_at"`
	Body       string    `json:"body"`
}

// timelineIdentityOf is the timeline identity an interaction row reads under:
// its interaction node when it has one, else the row itself as a message burst.
func timelineIdentityOf(row *repository.Interaction) (string, uuid.UUID) {
	if row.InteractionNodeID != nil {
		return "interaction", *row.InteractionNodeID
	}
	return "message_burst", row.ID
}

// timelineContentPath is the content route of an interaction row's timeline
// item, read in that row's own contact's participation context.
func timelineContentPath(row *repository.Interaction) string {
	kind, id := timelineIdentityOf(row)
	return "/api/v1/timeline-items/" + kind + "/" + id.String() + "/content?contact_id=" + row.ContactID.String()
}

// content reads the drill-down messages of an interaction in its own
// contact's participation context.
func (g *groupEnv) content(t *testing.T, interaction *repository.Interaction) []contentRow {
	t.Helper()
	path := timelineContentPath(interaction)
	w := macHTTP(t, g.macHostTestEnv, http.MethodGet, path, nil, nil)
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

// senderOf returns the content row with the given body.
func senderOf(t *testing.T, rows []contentRow, body string) contentRow {
	t.Helper()
	for _, r := range rows {
		if r.Body == body {
			return r
		}
	}
	t.Fatalf("no content row with body %q in %+v", body, rows)
	return contentRow{}
}

type enrichmentResponse struct {
	Results []struct {
		Guid    string `json:"guid"`
		Outcome string `json:"outcome"`
	} `json:"results"`
	Totals struct {
		Filled        int `json:"filled"`
		Unchanged     int `json:"unchanged"`
		Unknown       int `json:"unknown"`
		Added         int `json:"added"`
		OutsideWindow int `json:"outside_window"`
		Cleared       int `json:"cleared"`
	} `json:"totals"`
}

func (r enrichmentResponse) outcomes() []string {
	out := make([]string, len(r.Results))
	for i, res := range r.Results {
		out[i] = res.Outcome
	}
	return out
}

// apply posts enrichment items and decodes the 200 answer.
func (e *enrichmentEnv) apply(t *testing.T, items ...map[string]any) enrichmentResponse {
	t.Helper()
	w := e.postFill(t, items...)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	requireDataKeys(t, w, map[string][]string{
		"":        {"results", "totals"},
		"results": {"guid", "outcome"},
		"totals":  {"filled", "unchanged", "unknown", "added", "outside_window", "cleared", "marked"},
	})
	var res enrichmentResponse
	readData(t, w, &res)
	return res
}

func addItem(guid, chat string, sender *string, direction string, sentAt time.Time, body *string) map[string]any {
	item := map[string]any{"op": "add_group_message", "guid": guid, "chat_guid": chat, "direction": direction,
		"sent_at": sentAt.Format(time.RFC3339Nano), "sender": nil, "body": nil}
	if sender != nil {
		item["sender"] = *sender
	}
	if body != nil {
		item["body"] = *body
	}
	return item
}

func clearItem(guid string) map[string]any {
	return map[string]any{"op": "clear_outbound_peer", "guid": guid}
}

func (g *groupEnv) storedGuid(t *testing.T, guid string) (*repository.MessagesMessage, error) {
	t.Helper()
	return g.messages.TestGetMessageIncludingDeleted(t.Context(), guid)
}

// chat names a group chat unique to this test.
func (g *groupEnv) chat(name string) string { return g.prefix + "-" + name }

func (g *groupEnv) nextGuid() string {
	g.seq++
	return g.guid(g.seq)
}

func (g *groupEnv) newMember(t *testing.T) *repository.Contact {
	t.Helper()
	contact, err := g.contactRepo.CreateContact(t.Context(), repository.CreateContactRequest{
		FullName: "Group Member " + g.prefix,
		Cadence:  strPtr("weekly"),
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 10*time.Second)
		defer cancel()
		_ = g.messages.HardDeleteByMacHost(ctx, g.hostID)
		_ = g.contactRepo.HardDeleteContact(ctx, contact.ID)
	})
	return contact
}

// at is a fixed UTC instant offset by whole seconds, so window bounds are
// exact in assertions.
func at(sec int) time.Time {
	return time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC).Add(time.Duration(sec) * time.Second)
}

// seedBurst stores one group-chat message per time for the member and links
// them to a fresh messages interaction of the given direction, as the
// aggregator and recorder leave a processed burst.
func (g *groupEnv) seedBurst(t *testing.T, chat string, member *repository.Contact, direction string, times ...time.Time) (*repository.Interaction, []*repository.MessagesMessage) {
	t.Helper()
	return g.seedLinked(t, chat, true, member, direction, times...)
}

// seedLinked is seedBurst for either kind of chat.
func (g *groupEnv) seedLinked(t *testing.T, chat string, isGroup bool, member *repository.Contact, direction string, times ...time.Time) (*repository.Interaction, []*repository.MessagesMessage) {
	t.Helper()
	ctx := t.Context()
	rows := make([]*repository.MessagesMessage, len(times))
	ids := make([]uuid.UUID, len(times))
	for i, sentAt := range times {
		rows[i] = g.seedRow(t, chat, isGroup, direction == repository.InteractionDirectionOutbound, memberHandle(member), &member.ID, sentAt)
		ids[i] = rows[i].ID
	}
	sort.Slice(times, func(i, j int) bool { return times[i].Before(times[j]) })
	ref := "messages:" + chat + ":" + rows[0].Guid
	interaction, err := g.interactionRepo.CreateInteraction(ctx, repository.CreateInteractionRequest{
		ContactID:  member.ID,
		Source:     repository.InteractionSourceMessages,
		SourceRef:  &ref,
		OccurredAt: times[len(times)-1],
		Direction:  direction,
	})
	require.NoError(t, err)
	require.NoError(t, g.messages.MarkMessagesProcessed(ctx, ids, interaction.ID))
	return interaction, rows
}

func memberHandle(member *repository.Contact) string {
	return "+1555" + strings.ReplaceAll(member.ID.String(), "-", "")[:7]
}

func normalizedFor(peer string) *string {
	if peer == "" {
		return nil
	}
	return &peer
}

// seedRow stores one unprocessed, unlinked message.
func (g *groupEnv) seedRow(t *testing.T, chat string, isGroup, outgoing bool, peer string, matched *uuid.UUID, sentAt time.Time) *repository.MessagesMessage {
	t.Helper()
	row, err := g.messages.UpsertMessage(t.Context(), repository.UpsertMessagesMessageParams{
		Guid:             g.nextGuid(),
		ChatGuid:         chat,
		PeerHandle:       peer,
		PeerNormalized:   normalizedFor(peer),
		Text:             strPtr("seeded"),
		MessageType:      "text",
		SentAt:           sentAt,
		IsOutgoing:       outgoing,
		IsGroupChat:      isGroup,
		MatchedContactID: matched,
		MacHostID:        &g.hostID,
	})
	require.NoError(t, err)
	return row
}

type groupChatWindow struct {
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
}

type groupChatsPage struct {
	Items []struct {
		ChatGuid string            `json:"chat_guid"`
		Windows  []groupChatWindow `json:"windows"`
	} `json:"items"`
	NextAfter *string `json:"next_after"`
}

func (e *enrichmentEnv) groupChatsPath(query string) string {
	path := "/api/v1/host/" + e.hostID.String() + "/messages/enrichment/group-chats"
	if query != "" {
		path += "?" + query
	}
	return path
}

func (e *enrichmentEnv) getGroupChats(t *testing.T, query string) (*httptest.ResponseRecorder, groupChatsPage) {
	t.Helper()
	w := macHTTP(t, e.macHostTestEnv, http.MethodGet, e.groupChatsPath(query), e.headers, nil)
	var page groupChatsPage
	if w.Code == http.StatusOK {
		requireDataKeys(t, w, map[string][]string{
			"":      {"items", "next_after"},
			"items": {"chat_guid", "windows"},
		})
		readData(t, w, &page)
	}
	return w, page
}

// walkGroupChats lists every group chat strictly after the given chat guid by
// walking next_after, and returns only the chats this test's prefix owns.
func (e *enrichmentEnv) walkGroupChats(t *testing.T, after string, limit int) map[string][]groupChatWindow {
	t.Helper()
	owned := map[string][]groupChatWindow{}
	var order []string
	for {
		q := fmt.Sprintf("limit=%d", limit)
		if after != "" {
			q += "&after=" + after
		}
		w, page := e.getGroupChats(t, q)
		require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
		for _, item := range page.Items {
			if strings.HasPrefix(item.ChatGuid, e.prefix) {
				require.NotContains(t, owned, item.ChatGuid, "a chat is listed once across pages")
				owned[item.ChatGuid] = item.Windows
				order = append(order, item.ChatGuid)
			}
		}
		if page.NextAfter == nil {
			require.True(t, sort.StringsAreSorted(order), "chats are ordered by chat_guid ascending")
			return owned
		}
		require.Equal(t, page.Items[len(page.Items)-1].ChatGuid, *page.NextAfter, "next_after is the last listed chat_guid")
		after = *page.NextAfter
	}
}

func requireWindow(t *testing.T, want [2]time.Time, got groupChatWindow) {
	t.Helper()
	require.True(t, want[0].Equal(got.Start), "window start: want %s, got %s", want[0], got.Start)
	require.True(t, want[1].Equal(got.End), "window end: want %s, got %s", want[1], got.End)
}

// spec: ING-042.lists-group-chat-windows
func TestGroupEvidence_GroupChats_ListsABurstWindowFromEarliestToLatestMessage(t *testing.T) {
	g := setupGroupEnv(t)
	member := g.newMember(t)
	chat := g.chat("one-burst")
	g.seedBurst(t, chat, member, repository.InteractionDirectionInbound, at(30), at(10), at(20))

	got := g.walkGroupChats(t, g.prefix, 500)

	require.Len(t, got, 1)
	require.Len(t, got[chat], 1)
	requireWindow(t, [2]time.Time{at(10), at(30)}, got[chat][0])
}

// spec: ING-042.lists-group-chat-windows
func TestGroupEvidence_GroupChats_ListsEveryDistinctWindowOrderedByStartThenEnd(t *testing.T) {
	g := setupGroupEnv(t)
	a, b, c := g.newMember(t), g.newMember(t), g.newMember(t)
	chat := g.chat("two-bursts")
	g.seedBurst(t, chat, a, repository.InteractionDirectionInbound, at(20), at(40))
	g.seedBurst(t, chat, b, repository.InteractionDirectionInbound, at(10), at(50))
	g.seedBurst(t, chat, c, repository.InteractionDirectionOutbound, at(20), at(40))

	got := g.walkGroupChats(t, g.prefix, 500)

	require.Len(t, got[chat], 2, "two bursts with the same window list it once")
	requireWindow(t, [2]time.Time{at(10), at(50)}, got[chat][0])
	requireWindow(t, [2]time.Time{at(20), at(40)}, got[chat][1])
}

// spec: ING-042.lists-group-chat-windows
func TestGroupEvidence_GroupChats_OmitsOneToOneChatsAndChatsWithoutLiveLinkedBursts(t *testing.T) {
	g := setupGroupEnv(t)
	member := g.newMember(t)
	listed := g.chat("listed")
	g.seedBurst(t, listed, member, repository.InteractionDirectionInbound, at(10), at(20))
	g.seedLinked(t, g.chat("one-to-one"), false, member, repository.InteractionDirectionInbound, at(10), at(20))
	g.seedRow(t, g.chat("unlinked"), true, false, memberHandle(member), &member.ID, at(10))
	g.seedRow(t, g.chat("non-contact"), true, false, "+15559990000", nil, at(10))

	got := g.walkGroupChats(t, g.prefix, 500)

	require.Equal(t, []string{listed}, sortedKeys(got))
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// spec: ING-042.lists-group-chat-windows
func TestGroupEvidence_GroupChats_SoftDeletedInteractionAndRowShapeNoWindow(t *testing.T) {
	g := setupGroupEnv(t)
	member, other := g.newMember(t), g.newMember(t)
	widened := g.chat("widened")
	_, rows := g.seedBurst(t, widened, member, repository.InteractionDirectionInbound, at(10), at(20), at(100))
	require.NoError(t, g.messages.TestSoftDeleteMessage(t.Context(), rows[2].ID))
	gone := g.chat("gone")
	deleted, _ := g.seedBurst(t, gone, other, repository.InteractionDirectionInbound, at(10), at(20))
	require.NoError(t, g.interactionRepo.SoftDeleteInteraction(t.Context(), deleted.ID))

	got := g.walkGroupChats(t, g.prefix, 500)

	require.Equal(t, []string{widened}, sortedKeys(got), "a soft-deleted interaction contributes no window")
	require.Len(t, got[widened], 1)
	requireWindow(t, [2]time.Time{at(10), at(20)}, got[widened][0])
}

// spec: ING-042.lists-group-chat-windows
func TestGroupEvidence_GroupChats_PagesByNextAfterListEveryChatOnce(t *testing.T) {
	g := setupGroupEnv(t)
	member := g.newMember(t)
	chats := []string{g.chat("a"), g.chat("b"), g.chat("c")}
	for _, chat := range chats {
		g.seedBurst(t, chat, member, repository.InteractionDirectionInbound, at(10), at(20))
	}

	got := g.walkGroupChats(t, g.prefix, 1)

	require.Equal(t, chats, sortedKeys(got))
}

// spec: ING-042.lists-group-chat-windows
func TestGroupEvidence_GroupChats_AfterIsExclusive(t *testing.T) {
	g := setupGroupEnv(t)
	member := g.newMember(t)
	chats := []string{g.chat("a"), g.chat("b"), g.chat("c")}
	for _, chat := range chats {
		g.seedBurst(t, chat, member, repository.InteractionDirectionInbound, at(10), at(20))
	}

	got := g.walkGroupChats(t, chats[0], 500)

	require.Equal(t, chats[1:], sortedKeys(got))
}

// spec: ING-042.lists-group-chat-windows
func TestGroupEvidence_GroupChats_LimitBounds(t *testing.T) {
	g := setupGroupEnv(t)
	member := g.newMember(t)
	g.seedBurst(t, g.chat("a"), member, repository.InteractionDirectionInbound, at(10))
	g.seedBurst(t, g.chat("b"), member, repository.InteractionDirectionInbound, at(10))

	_, byDefault := g.getGroupChats(t, "")
	require.LessOrEqual(t, len(byDefault.Items), 500)
	_, one := g.getGroupChats(t, "limit=1")
	require.Len(t, one.Items, 1)
	require.NotNil(t, one.NextAfter)
	_, max := g.getGroupChats(t, "limit=500")
	require.LessOrEqual(t, len(max.Items), 500)
	for _, limit := range []string{"0", "501"} {
		w, _ := g.getGroupChats(t, "limit="+limit)
		require.Equal(t, http.StatusBadRequest, w.Code, "limit=%s: %s", limit, w.Body.String())
	}
}

func TestGroupEvidence_GroupChats_RequiresHostCredentials(t *testing.T) {
	g := setupGroupEnv(t)

	w := macHTTP(t, g.macHostTestEnv, http.MethodGet, g.groupChatsPath(""), nil, nil)

	require.Equal(t, http.StatusUnauthorized, w.Code, "body: %s", w.Body.String())
}

// spec: ING-042.adds-missing-in-window-message
func TestGroupEvidence_Add_InboundInsideWindowIsStoredAndLabelledBySenderInEveryContainingBurst(t *testing.T) {
	g := setupGroupEnv(t)
	a, b := g.newMember(t), g.newMember(t)
	chat := g.chat("add")
	burstA, _ := g.seedBurst(t, chat, a, repository.InteractionDirectionInbound, at(10), at(30))
	burstB, _ := g.seedBurst(t, chat, b, repository.InteractionDirectionInbound, at(20), at(40))
	guid := g.nextGuid()

	res := g.apply(t, addItem(guid, chat, strPtr("+15550001111"), "inbound", at(25), strPtr("from a stranger")))

	require.Equal(t, []string{"added"}, res.outcomes())
	require.Equal(t, 1, res.Totals.Added)
	row, err := g.storedGuid(t, guid)
	require.NoError(t, err)
	require.Equal(t, chat, row.ChatGuid)
	require.True(t, at(25).Equal(row.SentAt))
	require.Equal(t, strPtr("from a stranger"), row.Text)
	require.Equal(t, "+15550001111", row.PeerHandle)
	require.False(t, row.IsOutgoing)
	require.True(t, row.IsGroupChat)
	require.Equal(t, &g.hostID, row.MacHostID, "the authenticated host is the row's host")
	require.Nil(t, row.MatchedContactID)
	require.Nil(t, row.InteractionID, "an added message links to no interaction")
	for _, burst := range []*repository.Interaction{burstA, burstB} {
		added := senderOf(t, g.content(t, burst), "from a stranger")
		require.Equal(t, "+15550001111", added.Sender)
		require.False(t, added.IsOutgoing)
	}
}

// spec: ING-042.adds-missing-in-window-message
func TestGroupEvidence_Add_AtExactlyAWindowBoundIsAdded(t *testing.T) {
	g := setupGroupEnv(t)
	member := g.newMember(t)
	chat := g.chat("bounds")
	g.seedBurst(t, chat, member, repository.InteractionDirectionInbound, at(10), at(30))

	res := g.apply(t,
		addItem(g.nextGuid(), chat, strPtr("+15550001111"), "inbound", at(10), nil),
		addItem(g.nextGuid(), chat, strPtr("+15550001111"), "inbound", at(30), nil))

	require.Equal(t, []string{"added", "added"}, res.outcomes())
}

// spec: ING-042.outside-window-not-stored
func TestGroupEvidence_Add_OneSecondOutsideEveryWindowIsNotStored(t *testing.T) {
	g := setupGroupEnv(t)
	member := g.newMember(t)
	chat := g.chat("outside")
	g.seedBurst(t, chat, member, repository.InteractionDirectionInbound, at(10), at(30))
	before, after := g.nextGuid(), g.nextGuid()

	res := g.apply(t,
		addItem(before, chat, strPtr("+15550001111"), "inbound", at(9), nil),
		addItem(after, chat, strPtr("+15550001111"), "inbound", at(31), nil))

	require.Equal(t, []string{"outside_window", "outside_window"}, res.outcomes())
	require.Equal(t, 2, res.Totals.OutsideWindow)
	for _, guid := range []string{before, after} {
		_, err := g.storedGuid(t, guid)
		require.Error(t, err, "nothing is stored outside every window")
	}
}

// spec: ING-042.outside-window-not-stored
func TestGroupEvidence_Add_OneToOneAndUnknownChatsAreOutsideEveryWindow(t *testing.T) {
	g := setupGroupEnv(t)
	member := g.newMember(t)
	oneToOne := g.chat("one-to-one")
	g.seedLinked(t, oneToOne, false, member, repository.InteractionDirectionInbound, at(10), at(30))
	first, second := g.nextGuid(), g.nextGuid()

	res := g.apply(t,
		addItem(first, oneToOne, strPtr("+15550001111"), "inbound", at(20), nil),
		addItem(second, g.chat("nobody-stored-this"), strPtr("+15550001111"), "inbound", at(20), nil))

	require.Equal(t, []string{"outside_window", "outside_window"}, res.outcomes())
	for _, guid := range []string{first, second} {
		_, err := g.storedGuid(t, guid)
		require.Error(t, err)
	}
}

// spec: ING-042.adds-missing-in-window-message
func TestGroupEvidence_Add_OutboundInsideAnInboundBurstIsTheUsersAndLeavesTheBurstAlone(t *testing.T) {
	g := setupGroupEnv(t)
	member := g.newMember(t)
	chat := g.chat("outbound-add")
	burst, _ := g.seedBurst(t, chat, member, repository.InteractionDirectionInbound, at(10), at(30))
	memberBefore, err := g.contactRepo.GetContact(t.Context(), member.ID)
	require.NoError(t, err)
	guid := g.nextGuid()

	res := g.apply(t, addItem(guid, chat, nil, "outbound", at(20), strPtr("my reply")))

	require.Equal(t, []string{"added"}, res.outcomes())
	row, err := g.storedGuid(t, guid)
	require.NoError(t, err)
	require.True(t, row.IsOutgoing)
	require.Empty(t, row.PeerHandle)
	require.Nil(t, row.PeerNormalized)
	require.Nil(t, row.MatchedContactID)
	reply := senderOf(t, g.content(t, burst), "my reply")
	require.Equal(t, "Me", reply.Sender)
	require.True(t, reply.IsOutgoing)
	after, err := g.interactionRepo.GetInteraction(t.Context(), burst.ID)
	require.NoError(t, err)
	require.Equal(t, repository.InteractionDirectionInbound, after.Direction, "the burst stays inbound")
	require.True(t, burst.OccurredAt.Equal(after.OccurredAt))
	memberAfter, err := g.contactRepo.GetContact(t.Context(), member.ID)
	require.NoError(t, err)
	require.Equal(t, memberBefore, memberAfter, "the member's cadence fields are unchanged")
}

// spec: ING-042.existing-message-unchanged
func TestGroupEvidence_Add_AGuidAlreadyStoredIsUnchangedWhateverTheItemSays(t *testing.T) {
	g := setupGroupEnv(t)
	member := g.newMember(t)
	chat := g.chat("existing")
	_, rows := g.seedBurst(t, chat, member, repository.InteractionDirectionInbound, at(10), at(30))
	gone := g.seedRow(t, chat, true, false, memberHandle(member), nil, at(20))
	require.NoError(t, g.messages.TestSoftDeleteMessage(t.Context(), gone.ID))
	liveBefore, err := g.storedGuid(t, rows[0].Guid)
	require.NoError(t, err)
	goneBefore, err := g.storedGuid(t, gone.Guid)
	require.NoError(t, err)

	res := g.apply(t,
		addItem(rows[0].Guid, chat, nil, "outbound", at(20), strPtr("different")),
		addItem(gone.Guid, chat, strPtr("+15550001111"), "inbound", at(20), strPtr("different")),
		addItem(g.nextGuid(), g.chat("elsewhere"), strPtr("+15550001111"), "inbound", at(20), nil))

	require.Equal(t, []string{"unchanged", "unchanged", "outside_window"}, res.outcomes())
	liveAfter, err := g.storedGuid(t, rows[0].Guid)
	require.NoError(t, err)
	require.Equal(t, liveBefore, liveAfter)
	goneAfter, err := g.storedGuid(t, gone.Guid)
	require.NoError(t, err)
	require.Equal(t, goneBefore, goneAfter, "a soft-deleted row is not revived or rewritten")
}

// spec: ING-042.no-cadence-or-event-effect
func TestGroupEvidence_Add_FromAContactCreditsNoParticipationAndPublishesNothing(t *testing.T) {
	g := setupGroupEnv(t)
	member := g.newMember(t)
	chat := g.chat("no-effect")
	g.seedBurst(t, chat, member, repository.InteractionDirectionInbound, at(10), at(30))
	taskRepo := repository.NewContactTaskRepository(g.database.Queries)
	eventRepo := repository.NewEventRepository(g.database.Queries)
	contactBefore, err := g.contactRepo.GetContact(t.Context(), member.ID)
	require.NoError(t, err)
	tasksBefore, err := taskRepo.ListContactTasksByContact(t.Context(), member.ID)
	require.NoError(t, err)
	interactionsBefore, err := g.interactionRepo.CountContactInteractions(t.Context(), member.ID)
	require.NoError(t, err)
	eventsBefore, err := eventRepo.CountBySource(t.Context(), repository.InteractionSourceMessages)
	require.NoError(t, err)

	res := g.apply(t, addItem(g.nextGuid(), chat, strPtr(memberHandle(member)), "inbound", at(20), strPtr("from a member")))

	require.Equal(t, []string{"added"}, res.outcomes())
	contactAfter, err := g.contactRepo.GetContact(t.Context(), member.ID)
	require.NoError(t, err)
	require.Equal(t, contactBefore, contactAfter, "cadence fields, awaiting-reply window and skip state are unchanged")
	tasksAfter, err := taskRepo.ListContactTasksByContact(t.Context(), member.ID)
	require.NoError(t, err)
	require.Equal(t, tasksBefore, tasksAfter)
	interactionsAfter, err := g.interactionRepo.CountContactInteractions(t.Context(), member.ID)
	require.NoError(t, err)
	require.Equal(t, interactionsBefore, interactionsAfter, "an added message creates no interaction")
	eventsAfter, err := eventRepo.CountBySource(t.Context(), repository.InteractionSourceMessages)
	require.NoError(t, err)
	require.Equal(t, eventsBefore, eventsAfter, "no event is published")
}

// spec: ING-042.second-run-unchanged
func TestGroupEvidence_Add_IdenticalSecondRequestIsAllUnchanged(t *testing.T) {
	g := setupGroupEnv(t)
	member := g.newMember(t)
	chat := g.chat("second-run")
	g.seedBurst(t, chat, member, repository.InteractionDirectionInbound, at(10), at(30))
	items := []map[string]any{
		addItem(g.nextGuid(), chat, strPtr("+15550001111"), "inbound", at(20), strPtr("one")),
		addItem(g.nextGuid(), chat, nil, "outbound", at(21), strPtr("two")),
	}

	first := g.apply(t, items...)
	second := g.apply(t, items...)

	require.Equal(t, 2, first.Totals.Added)
	require.Equal(t, []string{"unchanged", "unchanged"}, second.outcomes())
	require.Equal(t, 2, second.Totals.Unchanged)
	require.Zero(t, second.Totals.Added)
}

// seedAttributedOutbound stores an outbound group message the way the old
// daemon sent it, attributed to the member, and links it to the member's
// outbound burst.
func (g *groupEnv) seedAttributedOutbound(t *testing.T, chat string, member *repository.Contact, sentAt time.Time) (*repository.Interaction, *repository.MessagesMessage) {
	t.Helper()
	burst, rows := g.seedBurst(t, chat, member, repository.InteractionDirectionOutbound, sentAt)
	return burst, rows[0]
}

// spec: ING-042.clears-outbound-member-attribution
// spec: ING-042.keeps-identity-membership-and-state
func TestGroupEvidence_Clear_RemovesTheAttributionAndKeepsEverythingElse(t *testing.T) {
	g := setupGroupEnv(t)
	member := g.newMember(t)
	chat := g.chat("clear")
	burst, linked := g.seedAttributedOutbound(t, chat, member, at(20))
	claimed := g.seedRow(t, chat, true, true, memberHandle(member), &member.ID, at(25))
	claimedIDs, err := g.messages.ClaimMessages(t.Context(), []uuid.UUID{claimed.ID}, "clear-test-session")
	require.NoError(t, err)
	require.Len(t, claimedIDs, 1)
	linkedBefore, err := g.storedGuid(t, linked.Guid)
	require.NoError(t, err)
	claimedBefore, err := g.storedGuid(t, claimed.Guid)
	require.NoError(t, err)
	require.NotNil(t, linkedBefore.InteractionID)
	require.NotNil(t, linkedBefore.ProcessedAt)
	require.NotNil(t, claimedBefore.ClaimedAt)
	memberBefore, err := g.contactRepo.GetContact(t.Context(), member.ID)
	require.NoError(t, err)

	res := g.apply(t, clearItem(linked.Guid), clearItem(claimed.Guid))

	require.Equal(t, []string{"cleared", "cleared"}, res.outcomes())
	require.Equal(t, 2, res.Totals.Cleared)
	for _, before := range []*repository.MessagesMessage{linkedBefore, claimedBefore} {
		after, err := g.storedGuid(t, before.Guid)
		require.NoError(t, err)
		require.Empty(t, after.PeerHandle)
		require.Nil(t, after.PeerNormalized)
		require.Nil(t, after.MatchedContactID)
		expected := *before
		expected.PeerHandle, expected.PeerNormalized, expected.MatchedContactID = "", nil, nil
		require.Equal(t, expected, *after, "only the member attribution changes")
	}
	burstAfter, err := g.interactionRepo.GetInteraction(t.Context(), burst.ID)
	require.NoError(t, err)
	require.Equal(t, repository.InteractionDirectionOutbound, burstAfter.Direction)
	require.True(t, burst.OccurredAt.Equal(burstAfter.OccurredAt))
	memberAfter, err := g.contactRepo.GetContact(t.Context(), member.ID)
	require.NoError(t, err)
	require.Equal(t, memberBefore, memberAfter, "the member's cadence fields are unchanged")
	windows := g.walkGroupChats(t, g.prefix, 500)
	require.Len(t, windows[chat], 1)
	requireWindow(t, [2]time.Time{at(20), at(20)}, windows[chat][0])
}

// spec: ING-042.clears-outbound-member-attribution
func TestGroupEvidence_Clear_AnAlreadyClearedRowInboundRowAndOneToOneRowAreUnchanged(t *testing.T) {
	g := setupGroupEnv(t)
	member := g.newMember(t)
	chat := g.chat("clear-noop")
	_, cleared := g.seedAttributedOutbound(t, chat, member, at(20))
	_, inbound := g.seedBurst(t, chat, member, repository.InteractionDirectionInbound, at(10))
	_, oneToOne := g.seedLinked(t, g.chat("dm"), false, member, repository.InteractionDirectionOutbound, at(10))
	require.Equal(t, []string{"cleared"}, g.apply(t, clearItem(cleared.Guid)).outcomes())
	guids := []string{cleared.Guid, inbound[0].Guid, oneToOne[0].Guid}
	before := make([]*repository.MessagesMessage, len(guids))
	for i, guid := range guids[1:] {
		row, err := g.storedGuid(t, guid)
		require.NoError(t, err)
		before[i+1] = row
	}

	res := g.apply(t, clearItem(guids[0]), clearItem(guids[1]), clearItem(guids[2]))

	require.Equal(t, []string{"unchanged", "unchanged", "unchanged"}, res.outcomes())
	for i := 1; i < len(guids); i++ {
		after, err := g.storedGuid(t, guids[i])
		require.NoError(t, err)
		require.Equal(t, before[i], after)
		require.Equal(t, memberHandle(member), after.PeerHandle, "an unchanged row keeps its peer")
	}
}

// spec: ING-042.clears-outbound-member-attribution
func TestGroupEvidence_Clear_AnUnknownGuidAndASoftDeletedRowAreUnknown(t *testing.T) {
	g := setupGroupEnv(t)
	member := g.newMember(t)
	chat := g.chat("clear-unknown")
	gone := g.seedRow(t, chat, true, true, memberHandle(member), &member.ID, at(20))
	require.NoError(t, g.messages.TestSoftDeleteMessage(t.Context(), gone.ID))
	before, err := g.storedGuid(t, gone.Guid)
	require.NoError(t, err)

	res := g.apply(t, clearItem(g.nextGuid()), clearItem(gone.Guid))

	require.Equal(t, []string{"unknown", "unknown"}, res.outcomes())
	require.Equal(t, 2, res.Totals.Unknown)
	after, err := g.storedGuid(t, gone.Guid)
	require.NoError(t, err)
	require.Equal(t, before, after)
}

// spec: ING-042.no-cadence-or-event-effect
// spec: ING-042.second-run-unchanged
func TestGroupEvidence_Enrichment_MixedRequestAnswersInOrderWithAllSixTotalsAndNoEffect(t *testing.T) {
	g := setupGroupEnv(t)
	member := g.newMember(t)
	chat := g.chat("mixed")
	g.seedBurst(t, chat, member, repository.InteractionDirectionInbound, at(10), at(30))
	_, attributed := g.seedAttributedOutbound(t, chat, member, at(40))
	bodyless, err := g.messages.UpsertMessage(t.Context(), repository.UpsertMessagesMessageParams{
		Guid: g.nextGuid(), ChatGuid: chat, PeerHandle: "+15550002222", MessageType: "text", SentAt: at(15),
		IsGroupChat: true, MacHostID: &g.hostID,
	})
	require.NoError(t, err)
	taskRepo := repository.NewContactTaskRepository(g.database.Queries)
	eventRepo := repository.NewEventRepository(g.database.Queries)
	contactBefore, err := g.contactRepo.GetContact(t.Context(), member.ID)
	require.NoError(t, err)
	tasksBefore, err := taskRepo.ListContactTasksByContact(t.Context(), member.ID)
	require.NoError(t, err)
	eventsBefore, err := eventRepo.CountBySource(t.Context(), repository.InteractionSourceMessages)
	require.NoError(t, err)
	added, outside := g.nextGuid(), g.nextGuid()
	items := []map[string]any{
		clearItem(g.nextGuid()),
		addItem(added, chat, strPtr("+15550001111"), "inbound", at(20), strPtr("kept")),
		fillItem(bodyless.Guid, "filled body"),
		addItem(outside, chat, nil, "outbound", at(500), nil),
		clearItem(attributed.Guid),
		fillItem(attributed.Guid, "ignored"),
	}

	first := g.apply(t, items...)

	require.Equal(t, []string{"unknown", "added", "filled", "outside_window", "cleared", "unchanged"}, first.outcomes())
	require.Equal(t, 1, first.Totals.Unknown)
	require.Equal(t, 1, first.Totals.Added)
	require.Equal(t, 1, first.Totals.Filled)
	require.Equal(t, 1, first.Totals.OutsideWindow)
	require.Equal(t, 1, first.Totals.Cleared)
	require.Equal(t, 1, first.Totals.Unchanged)
	contactAfter, err := g.contactRepo.GetContact(t.Context(), member.ID)
	require.NoError(t, err)
	require.Equal(t, contactBefore, contactAfter)
	tasksAfter, err := taskRepo.ListContactTasksByContact(t.Context(), member.ID)
	require.NoError(t, err)
	require.Equal(t, tasksBefore, tasksAfter)
	eventsAfter, err := eventRepo.CountBySource(t.Context(), repository.InteractionSourceMessages)
	require.NoError(t, err)
	require.Equal(t, eventsBefore, eventsAfter)

	second := g.apply(t, items[1], items[2], items[4])

	require.Equal(t, []string{"unchanged", "unchanged", "unchanged"}, second.outcomes())
}

// spec: ING-042.invalid-request-writes-nothing
func TestGroupEvidence_Enrichment_AMalformedItemRejectsTheWholeRequestWithoutWriting(t *testing.T) {
	g := setupGroupEnv(t)
	member := g.newMember(t)
	chat := g.chat("invalid")
	g.seedBurst(t, chat, member, repository.InteractionDirectionInbound, at(10), at(30))
	_, attributed := g.seedAttributedOutbound(t, chat, member, at(20))
	valid := g.nextGuid()
	good := func() map[string]any {
		return addItem(g.nextGuid(), chat, strPtr("+15550001111"), "inbound", at(20), nil)
	}
	with := func(item map[string]any, k string, v any) map[string]any { item[k] = v; return item }
	cases := map[string]map[string]any{
		"empty guid":                 with(good(), "guid", ""),
		"missing guid":               with(good(), "guid", nil),
		"empty chat_guid":            with(good(), "chat_guid", ""),
		"unknown direction":          with(good(), "direction", "sideways"),
		"sent_at not RFC 3339":       with(good(), "sent_at", "yesterday"),
		"sent_at not a string":       with(good(), "sent_at", 5),
		"inbound without sender":     with(good(), "sender", nil),
		"inbound with empty sender":  with(good(), "sender", ""),
		"outbound with sender":       with(with(good(), "direction", "outbound"), "sender", "+15550001111"),
		"outbound with empty sender": with(with(good(), "direction", "outbound"), "sender", ""),
		"empty body":                 with(good(), "body", ""),
		"body not a string":          with(good(), "body", 5),
		"unknown op":                 {"op": "delete_row", "guid": attributed.Guid},
		"clear without guid":         {"op": "clear_outbound_peer"},
		"clear with empty guid":      {"op": "clear_outbound_peer", "guid": ""},
		"add missing every field":    {"op": "add_group_message"},
	}
	for name, bad := range cases {
		t.Run(name, func(t *testing.T) {
			w := g.postFill(t, addItem(valid, chat, strPtr("+15550001111"), "inbound", at(20), nil), clearItem(attributed.Guid), bad)

			require.Equal(t, http.StatusBadRequest, w.Code, "body: %s", w.Body.String())
			_, err := g.storedGuid(t, valid)
			require.Error(t, err, "a valid item beside a malformed one is not written")
			row, err := g.storedGuid(t, attributed.Guid)
			require.NoError(t, err)
			require.Equal(t, memberHandle(member), row.PeerHandle, "a valid clear beside a malformed item is not applied")
		})
	}
}

// spec: ING-042.invalid-request-writes-nothing
func TestGroupEvidence_Enrichment_FiveHundredAndOneItemsWriteNothing(t *testing.T) {
	g := setupGroupEnv(t)
	member := g.newMember(t)
	chat := g.chat("too-many")
	g.seedBurst(t, chat, member, repository.InteractionDirectionInbound, at(10), at(30))
	items := make([]map[string]any, 501)
	for i := range items {
		items[i] = addItem(fmt.Sprintf("%s-bulk-%04d", g.prefix, i), chat, strPtr("+15550001111"), "inbound", at(20), nil)
	}

	w := g.postFill(t, items...)

	require.Equal(t, http.StatusBadRequest, w.Code, "body: %s", w.Body.String())
	_, err := g.storedGuid(t, fmt.Sprintf("%s-bulk-%04d", g.prefix, 0))
	require.Error(t, err)
	res := g.apply(t, items[:500]...)
	require.Equal(t, 500, res.Totals.Added, "five hundred items are accepted")
}

// sizedAddBody builds a valid one-item add request whose JSON is exactly size bytes.
func sizedAddBody(t *testing.T, guid, chat string, size int) []byte {
	t.Helper()
	build := func(pad int) []byte {
		b, err := json.Marshal(map[string]any{"items": []map[string]any{
			addItem(guid, chat, strPtr("+15550001111"), "inbound", at(20), strPtr(strings.Repeat("x", pad)))}})
		require.NoError(t, err)
		return b
	}
	pad := size - len(build(0))
	require.Positive(t, pad)
	body := build(pad)
	require.Len(t, body, size)
	return body
}

// spec: ING-042.invalid-request-writes-nothing
func TestGroupEvidence_Enrichment_EightMiBBodyIsAcceptedAndOneByteMoreIsRejectedWithoutWriting(t *testing.T) {
	g := setupGroupEnv(t)
	member := g.newMember(t)
	chat := g.chat("size")
	g.seedBurst(t, chat, member, repository.InteractionDirectionInbound, at(10), at(30))
	exact, over := g.nextGuid(), g.nextGuid()

	okResponse := g.postRaw(t, sizedAddBody(t, exact, chat, 8<<20))
	tooLarge := g.postRaw(t, sizedAddBody(t, over, chat, 8<<20+1))

	require.Equal(t, http.StatusOK, okResponse.Code)
	var res enrichmentResponse
	readData(t, okResponse, &res)
	require.Equal(t, []string{"added"}, res.outcomes())
	require.Equal(t, http.StatusRequestEntityTooLarge, tooLarge.Code)
	_, err := g.storedGuid(t, over)
	require.Error(t, err)
}

// spec: ING-041.drill-down-labels-senders-honestly
func TestGroupEvidence_Content_AnOutboundRowClearedOfAMembersAttributionIsTheUsersInEveryBurstContainingIt(t *testing.T) {
	g := setupGroupEnv(t)
	sender, other := g.newMember(t), g.newMember(t)
	chat := g.chat("cleared-content")
	senderBurst, rows := g.seedBurst(t, chat, sender, repository.InteractionDirectionOutbound, at(20))
	otherBurst, _ := g.seedBurst(t, chat, other, repository.InteractionDirectionInbound, at(10), at(30))
	require.Equal(t, []string{"cleared"}, g.apply(t, clearItem(rows[0].Guid)).outcomes())

	for _, burst := range []*repository.Interaction{senderBurst, otherBurst} {
		var mine []contentRow
		for _, r := range g.content(t, burst) {
			if r.ID == rows[0].ID.String() {
				mine = append(mine, r)
			}
		}
		require.Len(t, mine, 1, "the cleared row is listed once")
		require.Equal(t, "Me", mine[0].Sender, "never under a member's name")
		require.True(t, mine[0].IsOutgoing)
	}
}
