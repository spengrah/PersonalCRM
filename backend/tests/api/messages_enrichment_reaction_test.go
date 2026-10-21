package api

import (
	"fmt"
	"net/http"
	"sort"
	"testing"
	"time"

	"personal-crm/backend/internal/repository"

	"github.com/stretchr/testify/require"
)

func (e *enrichmentEnv) seedReaction(t *testing.T, n int, reaction *repository.MessageReaction) *repository.MessagesMessage {
	t.Helper()
	msg, err := e.messages.UpsertMessage(t.Context(), repository.UpsertMessagesMessageParams{
		Guid:        e.guid(n),
		ChatGuid:    e.prefix + "-chat",
		PeerHandle:  "+15550000000",
		Text:        strPtr(`Liked "lunch?"`),
		MessageType: "text",
		SentAt:      time.Date(2026, 3, 1, 12, 0, n, 0, time.UTC),
		MacHostID:   &e.hostID,
		Reaction:    reaction,
	})
	require.NoError(t, err)
	return msg
}

func (e *enrichmentEnv) withoutFactsPath(query string) string {
	path := "/api/v1/host/" + e.hostID.String() + "/messages/enrichment/without-reaction-facts"
	if query != "" {
		path += "?" + query
	}
	return path
}

func (e *enrichmentEnv) listWithoutFacts(t *testing.T, query string) missingBodiesPage {
	t.Helper()
	w := macHTTP(t, e.macHostTestEnv, http.MethodGet, e.withoutFactsPath(query), e.headers, nil)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	requireDataKeys(t, w, map[string][]string{
		"":      {"items", "next_after"},
		"items": {"guid"},
	})
	var page missingBodiesPage
	readData(t, w, &page)
	return page
}

// walkWithoutFacts lists every guid without reaction facts strictly after the
// given guid by walking next_after.
func (e *enrichmentEnv) walkWithoutFacts(t *testing.T, after string, limit int) []string {
	t.Helper()
	var all []string
	for {
		q := fmt.Sprintf("limit=%d", limit)
		if after != "" {
			q += "&after=" + after
		}
		page := e.listWithoutFacts(t, q)
		all = append(all, page.guids()...)
		if page.NextAfter == nil {
			return all
		}
		require.Equal(t, all[len(all)-1], *page.NextAfter, "next_after is the last listed guid")
		after = *page.NextAfter
	}
}

type reactionResponse struct {
	Results []struct {
		Guid    string `json:"guid"`
		Outcome string `json:"outcome"`
	} `json:"results"`
	Totals struct {
		Marked    int `json:"marked"`
		Unchanged int `json:"unchanged"`
		Unknown   int `json:"unknown"`
	} `json:"totals"`
}

func (r reactionResponse) outcomes() []string {
	out := make([]string, len(r.Results))
	for i, res := range r.Results {
		out[i] = res.Outcome
	}
	return out
}

func setReactionItem(guid string, reaction map[string]any) map[string]any {
	return map[string]any{"op": "set_reaction", "guid": guid, "reaction": reaction}
}

func likeOf(target string) map[string]any {
	return map[string]any{"kind": "like", "target_key": target, "removal": false}
}

func (e *enrichmentEnv) setReactions(t *testing.T, items ...map[string]any) reactionResponse {
	t.Helper()
	w := e.postFill(t, items...)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	requireDataKeys(t, w, map[string][]string{
		"":        {"results", "totals"},
		"results": {"guid", "outcome"},
		"totals":  {"filled", "unchanged", "unknown", "added", "outside_window", "cleared", "marked"},
	})
	var res reactionResponse
	readData(t, w, &res)
	return res
}

// spec: MAC-064.lists-rows-without-facts
func TestMessagesEnrichment_WithoutReactionFacts_ListsPagesInGuidOrderAndOmitsMarkedAndDeletedRows(t *testing.T) {
	e := setupEnrichmentEnv(t)
	for n := 1; n <= 5; n++ {
		e.seed(t, n, strPtr("hello"))
	}
	e.seedReaction(t, 6, &repository.MessageReaction{Kind: "love", TargetKey: "target"})
	gone := e.seed(t, 7, strPtr("hello"))
	require.NoError(t, e.messages.TestSoftDeleteMessage(t.Context(), gone.ID))

	all := e.walkWithoutFacts(t, e.prefix, 2)

	require.True(t, sort.StringsAreSorted(all), "guids ascend across pages")
	require.Equal(t, []string{e.guid(1), e.guid(2), e.guid(3), e.guid(4), e.guid(5)}, ownedGuids(e, all),
		"a message that carries a kind and a deleted message are not listed")
	first := e.listWithoutFacts(t, "limit=2&after="+e.prefix)
	require.Len(t, first.Items, 2)
	require.NotNil(t, first.NextAfter, "a cursor while more remain")
}

// spec: MAC-064.marks-reaction
func TestMessagesEnrichment_SetReaction_StoresFactsOnAMessageWithoutThem(t *testing.T) {
	e := setupEnrichmentEnv(t)
	e.seed(t, 1, strPtr(`Reacted 👍 to "lunch?"`))
	thumbs := "👍"

	res := e.setReactions(t, setReactionItem(e.guid(1), map[string]any{"kind": "emoji", "emoji": thumbs, "target_key": "target", "removal": true}))

	require.Equal(t, []string{"marked"}, res.outcomes())
	require.Equal(t, 1, res.Totals.Marked)
	require.Equal(t, &repository.MessageReaction{Kind: "emoji", Emoji: &thumbs, TargetKey: "target", Removal: true}, e.stored(t, 1).Reaction)
}

// spec: MAC-064.second-run-unchanged
func TestMessagesEnrichment_SetReaction_SameItemAgainIsUnchanged(t *testing.T) {
	e := setupEnrichmentEnv(t)
	e.seed(t, 1, strPtr(`Liked "lunch?"`))
	item := setReactionItem(e.guid(1), likeOf("target"))

	require.Equal(t, []string{"marked"}, e.setReactions(t, item).outcomes())
	second := e.setReactions(t, item)

	require.Equal(t, []string{"unchanged"}, second.outcomes())
	require.Equal(t, 0, second.Totals.Marked)
	require.Equal(t, 1, second.Totals.Unchanged)
	require.Equal(t, &repository.MessageReaction{Kind: "like", TargetKey: "target"}, e.stored(t, 1).Reaction)
}

// spec: MAC-064.keeps-existing-facts
func TestMessagesEnrichment_SetReaction_DifferentFactsOnAMarkedMessageKeepTheStoredOnes(t *testing.T) {
	e := setupEnrichmentEnv(t)
	stored := &repository.MessageReaction{Kind: "love", TargetKey: "target"}
	e.seedReaction(t, 1, stored)

	res := e.setReactions(t, setReactionItem(e.guid(1), map[string]any{"kind": "laugh", "target_key": "other", "removal": true}))

	require.Equal(t, []string{"unchanged"}, res.outcomes())
	require.Equal(t, stored, e.stored(t, 1).Reaction)
}

// spec: MAC-064.unknown-guid-not-created
func TestMessagesEnrichment_SetReaction_UnknownGuidCreatesNothing(t *testing.T) {
	e := setupEnrichmentEnv(t)
	gone := e.seed(t, 2, strPtr(`Liked "lunch?"`))
	require.NoError(t, e.messages.TestSoftDeleteMessage(t.Context(), gone.ID))
	deletedBefore := e.stored(t, 2)

	res := e.setReactions(t, setReactionItem(e.guid(1), likeOf("target")), setReactionItem(e.guid(2), likeOf("target")))

	require.Equal(t, []string{"unknown", "unknown"}, res.outcomes())
	require.Equal(t, 2, res.Totals.Unknown)
	_, err := e.messages.TestGetMessageIncludingDeleted(t.Context(), e.guid(1))
	require.Error(t, err, "no row is created for an unknown guid")
	require.Equal(t, deletedBefore, e.stored(t, 2), "a deleted message is not marked")
}

// spec: MAC-064.row-identity-preserved
func TestMessagesEnrichment_SetReaction_MarkedRowKeepsItsIdentityAndState(t *testing.T) {
	e := setupEnrichmentEnv(t)
	e.seedLinked(t)
	before := []*repository.MessagesMessage{e.stored(t, 1), e.stored(t, 2), e.stored(t, 3)}
	require.NotNil(t, before[0].InteractionID)
	require.NotNil(t, before[1].ClaimedAt)

	res := e.setReactions(t, setReactionItem(e.guid(1), likeOf("t1")), setReactionItem(e.guid(2), likeOf("t2")), setReactionItem(e.guid(3), likeOf("t3")))

	require.Equal(t, []string{"marked", "marked", "marked"}, res.outcomes())
	for i := range before {
		after := e.stored(t, i+1)
		require.Equal(t, &repository.MessageReaction{Kind: "like", TargetKey: fmt.Sprintf("t%d", i+1)}, after.Reaction)
		expected := *before[i]
		expected.Reaction = after.Reaction
		require.Equal(t, expected, *after, "only the reaction facts of row %d change", i+1)
	}
}

// spec: MAC-064.no-cadence-or-event-effect
func TestMessagesEnrichment_SetReaction_HasNoCadenceFollowUpOrEventEffect(t *testing.T) {
	e := setupEnrichmentEnv(t)
	contact := e.seedLinked(t)
	contactRepo := repository.NewContactRepository(e.database.Queries)
	taskRepo := repository.NewContactTaskRepository(e.database.Queries)
	eventRepo := repository.NewEventRepository(e.database.Queries)
	ctx := t.Context()

	contactBefore, err := contactRepo.GetContact(ctx, contact.ID)
	require.NoError(t, err)
	tasksBefore, err := taskRepo.ListContactTasksByContact(ctx, contact.ID)
	require.NoError(t, err)
	eventsBefore, err := eventRepo.CountBySource(ctx, repository.InteractionSourceMessages)
	require.NoError(t, err)

	e.setReactions(t, setReactionItem(e.guid(1), likeOf("t1")), setReactionItem(e.guid(3), likeOf("t3")))

	contactAfter, err := contactRepo.GetContact(ctx, contact.ID)
	require.NoError(t, err)
	require.Equal(t, contactBefore, contactAfter, "cadence fields, awaiting-reply window and skip state are unchanged")
	tasksAfter, err := taskRepo.ListContactTasksByContact(ctx, contact.ID)
	require.NoError(t, err)
	require.Equal(t, tasksBefore, tasksAfter)
	eventsAfter, err := eventRepo.CountBySource(ctx, repository.InteractionSourceMessages)
	require.NoError(t, err)
	require.Equal(t, eventsBefore, eventsAfter, "no event is published")
}

// spec: MAC-064.invalid-request-writes-nothing
func TestMessagesEnrichment_SetReaction_InvalidRequestWritesNothing(t *testing.T) {
	e := setupEnrichmentEnv(t)
	e.seed(t, 1, strPtr(`Liked "lunch?"`))
	valid := setReactionItem(e.guid(1), likeOf("target"))

	cases := map[string]map[string]any{
		"no reaction":                  {"op": "set_reaction", "guid": e.guid(2)},
		"null reaction":                setReactionItem(e.guid(2), nil),
		"a kind outside the set":       setReactionItem(e.guid(2), map[string]any{"kind": "wow", "target_key": "t", "removal": false}),
		"an empty target key":          setReactionItem(e.guid(2), map[string]any{"kind": "like", "target_key": "", "removal": false}),
		"the emoji kind without emoji": setReactionItem(e.guid(2), map[string]any{"kind": "emoji", "target_key": "t", "removal": false}),
		"an emoji on another kind":     setReactionItem(e.guid(2), map[string]any{"kind": "like", "emoji": "👍", "target_key": "t", "removal": false}),
		"removal missing":              setReactionItem(e.guid(2), map[string]any{"kind": "like", "target_key": "t"}),
		"guid missing":                 {"op": "set_reaction", "reaction": likeOf("t")},
	}
	for name, bad := range cases {
		t.Run(name, func(t *testing.T) {
			w := e.postFill(t, valid, bad)
			require.Equal(t, http.StatusBadRequest, w.Code, "body: %s", w.Body.String())
			require.Nil(t, e.stored(t, 1).Reaction)
		})
	}

	t.Run("over the item cap", func(t *testing.T) {
		items := make([]map[string]any, 501)
		for i := range items {
			items[i] = valid
		}
		w := e.postFill(t, items...)
		require.Equal(t, http.StatusBadRequest, w.Code, "body: %s", w.Body.String())
		require.Nil(t, e.stored(t, 1).Reaction)
	})
}

// spec: MAC-064.requires-host-credentials
func TestMessagesEnrichment_ReactionExchange_RequiresHostCredentials(t *testing.T) {
	e := setupEnrichmentEnv(t)
	e.seed(t, 1, strPtr(`Liked "lunch?"`))

	w := macHTTP(t, e.macHostTestEnv, http.MethodGet, e.withoutFactsPath(""), nil, nil)
	require.Equal(t, http.StatusUnauthorized, w.Code, "body: %s", w.Body.String())
	w = macHTTP(t, e.macHostTestEnv, http.MethodPost, e.fillPath(), nil,
		map[string]any{"items": []map[string]any{setReactionItem(e.guid(1), likeOf("target"))}})
	require.Equal(t, http.StatusUnauthorized, w.Code, "body: %s", w.Body.String())
	require.Nil(t, e.stored(t, 1).Reaction)
}
