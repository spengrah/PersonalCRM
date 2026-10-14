package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// enrichmentEnv is a paired host plus a guid namespace unique to one test, so
// rows seeded here are told apart from rows other tests leave in the shared
// database.
type enrichmentEnv struct {
	*macHostTestEnv
	hostID  uuid.UUID
	headers map[string]string
	prefix  string
}

func setupEnrichmentEnv(t *testing.T) *enrichmentEnv {
	t.Helper()
	env := setupMacHostEnv(t)

	plain, _, err := env.macService.CreatePairingToken(t.Context())
	require.NoError(t, err)
	res, err := env.macService.PairWithToken(t.Context(), plain, "enrichment-host", "0.1.0", 1)
	require.NoError(t, err)

	e := &enrichmentEnv{
		macHostTestEnv: env,
		hostID:         res.HostID,
		headers: map[string]string{
			"X-Mac-Host-ID": res.HostID.String(),
			"Authorization": "Bearer " + res.APIKey,
		},
		prefix: "enr-" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12],
	}
	// Registered after the env's own cleanup, so it runs first and the host
	// row is still present when the staged rows are removed.
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 10*time.Second)
		defer cancel()
		_ = env.messages.HardDeleteByMacHost(ctx, res.HostID)
	})
	return e
}

func (e *enrichmentEnv) guid(n int) string { return fmt.Sprintf("%s-%04d", e.prefix, n) }

func (e *enrichmentEnv) seed(t *testing.T, n int, text *string) *repository.MessagesMessage {
	t.Helper()
	return e.seedFor(t, n, text, nil)
}

func (e *enrichmentEnv) seedFor(t *testing.T, n int, text *string, contactID *uuid.UUID) *repository.MessagesMessage {
	t.Helper()
	msg, err := e.messages.UpsertMessage(t.Context(), repository.UpsertMessagesMessageParams{
		Guid:             e.guid(n),
		ChatGuid:         e.prefix + "-chat",
		PeerHandle:       "+15550000000",
		Text:             text,
		MessageType:      "text",
		SentAt:           time.Date(2026, 3, 1, 12, 0, n, 0, time.UTC),
		MatchedContactID: contactID,
		MacHostID:        &e.hostID,
	})
	require.NoError(t, err)
	return msg
}

func strPtr(s string) *string { return &s }

type missingBodiesPage struct {
	Items []struct {
		Guid string `json:"guid"`
	} `json:"items"`
	NextAfter *string `json:"next_after"`
}

// requireDataKeys asserts the envelope's data object has exactly the wanted
// keys. wanted[""] names the keys of data itself; any other entry names the
// keys of every object in that array or object field of data. The typed
// response structs drop unknown properties, so only the raw JSON shows an
// added field.
func requireDataKeys(t *testing.T, w *httptest.ResponseRecorder, wanted map[string][]string) {
	t.Helper()
	var envelope struct {
		Data map[string]json.RawMessage `json:"data"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &envelope), "body: %s", w.Body.String())
	keysOf := func(raw json.RawMessage) []string {
		var obj map[string]json.RawMessage
		require.NoError(t, json.Unmarshal(raw, &obj), "object expected, got %s", raw)
		keys := make([]string, 0, len(obj))
		for k := range obj {
			keys = append(keys, k)
		}
		return keys
	}
	dataKeys := make([]string, 0, len(envelope.Data))
	for k := range envelope.Data {
		dataKeys = append(dataKeys, k)
	}
	require.ElementsMatch(t, wanted[""], dataKeys, "data keys")
	for field, keys := range wanted {
		if field == "" {
			continue
		}
		raw := envelope.Data[field]
		var arr []json.RawMessage
		if json.Unmarshal(raw, &arr) != nil {
			arr = []json.RawMessage{raw}
		}
		for _, el := range arr {
			require.ElementsMatch(t, keys, keysOf(el), "keys of %s element", field)
		}
	}
}

func (p missingBodiesPage) guids() []string {
	out := make([]string, len(p.Items))
	for i, it := range p.Items {
		out[i] = it.Guid
	}
	return out
}

func (e *enrichmentEnv) missingPath(query string) string {
	path := "/api/v1/host/" + e.hostID.String() + "/messages/enrichment/missing-bodies"
	if query != "" {
		path += "?" + query
	}
	return path
}

func (e *enrichmentEnv) listMissing(t *testing.T, query string) missingBodiesPage {
	t.Helper()
	w := macHTTP(t, e.macHostTestEnv, http.MethodGet, e.missingPath(query), e.headers, nil)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	requireDataKeys(t, w, map[string][]string{
		"":      {"items", "next_after"},
		"items": {"guid"},
	})
	var page missingBodiesPage
	readData(t, w, &page)
	return page
}

// walkMissing lists every body-less guid strictly after the given guid by
// walking next_after, so rows other tests left behind only lengthen the walk.
func (e *enrichmentEnv) walkMissing(t *testing.T, after string, limit int) []string {
	t.Helper()
	var all []string
	for {
		q := fmt.Sprintf("limit=%d", limit)
		if after != "" {
			q += "&after=" + after
		}
		page := e.listMissing(t, q)
		all = append(all, page.guids()...)
		if page.NextAfter == nil {
			return all
		}
		require.Equal(t, all[len(all)-1], *page.NextAfter, "next_after is the last listed guid")
		after = *page.NextAfter
	}
}

func ownedGuids(e *enrichmentEnv, guids []string) []string {
	var out []string
	for _, g := range guids {
		if strings.HasPrefix(g, e.prefix) {
			out = append(out, g)
		}
	}
	return out
}

// spec: MAC-057.lists-body-less-rows
func TestMessagesEnrichment_MissingBodies_ListsNullTextRow(t *testing.T) {
	e := setupEnrichmentEnv(t)
	e.seed(t, 1, nil)

	got := ownedGuids(e, e.walkMissing(t, e.prefix, 500))
	require.Equal(t, []string{e.guid(1)}, got)
}

// spec: MAC-057.lists-body-less-rows
func TestMessagesEnrichment_MissingBodies_ListsEmptyTextRow(t *testing.T) {
	e := setupEnrichmentEnv(t)
	e.seed(t, 1, strPtr(""))

	got := ownedGuids(e, e.walkMissing(t, e.prefix, 500))
	require.Equal(t, []string{e.guid(1)}, got)
}

// spec: MAC-057.lists-body-less-rows
func TestMessagesEnrichment_MissingBodies_OmitsRowWithBody(t *testing.T) {
	e := setupEnrichmentEnv(t)
	e.seed(t, 1, strPtr("hello"))
	e.seed(t, 2, nil)

	got := ownedGuids(e, e.walkMissing(t, e.prefix, 500))
	require.Equal(t, []string{e.guid(2)}, got)
}

// spec: MAC-057.lists-body-less-rows
func TestMessagesEnrichment_MissingBodies_OmitsSoftDeletedRow(t *testing.T) {
	e := setupEnrichmentEnv(t)
	gone := e.seed(t, 1, nil)
	e.seed(t, 2, nil)
	require.NoError(t, e.messages.TestSoftDeleteMessage(t.Context(), gone.ID))

	got := ownedGuids(e, e.walkMissing(t, e.prefix, 500))
	require.Equal(t, []string{e.guid(2)}, got)
}

// spec: MAC-057.lists-body-less-rows
func TestMessagesEnrichment_MissingBodies_ListsRowsPushedByAnyHost(t *testing.T) {
	e := setupEnrichmentEnv(t)
	other, err := e.database.Queries.SeedRevokedMacHost(t.Context(), db.SeedRevokedMacHostParams{
		Hostname:        "enrichment-other-host",
		DaemonVersion:   "0.1.0",
		ProtocolVersion: 1,
		ApiKeyHash:      "unused",
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = e.messages.HardDeleteByMacHost(context.WithoutCancel(t.Context()), other.ID)
	})
	_, err = e.messages.UpsertMessage(t.Context(), repository.UpsertMessagesMessageParams{
		Guid:        e.guid(1),
		ChatGuid:    e.prefix + "-chat",
		PeerHandle:  "+15550000000",
		MessageType: "text",
		SentAt:      time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC),
		MacHostID:   &other.ID,
	})
	require.NoError(t, err)

	got := ownedGuids(e, e.walkMissing(t, e.prefix, 500))
	require.Equal(t, []string{e.guid(1)}, got)
}

// spec: MAC-057.lists-body-less-rows
func TestMessagesEnrichment_MissingBodies_WalkListsEveryRowOnce(t *testing.T) {
	e := setupEnrichmentEnv(t)
	for n := 1; n <= 5; n++ {
		e.seed(t, n, nil)
	}

	all := e.walkMissing(t, e.prefix, 2)
	require.True(t, sort.StringsAreSorted(all), "guids ascend across pages")
	require.Equal(t, []string{e.guid(1), e.guid(2), e.guid(3), e.guid(4), e.guid(5)}, ownedGuids(e, all))
	seen := map[string]bool{}
	for _, g := range all {
		require.False(t, seen[g], "guid %s listed twice", g)
		seen[g] = true
	}
}

// spec: MAC-057.lists-body-less-rows
func TestMessagesEnrichment_MissingBodies_AfterIsExclusiveAndOmittedStartsLowest(t *testing.T) {
	e := setupEnrichmentEnv(t)
	for n := 1; n <= 3; n++ {
		e.seed(t, n, nil)
	}

	require.Equal(t, []string{e.guid(1), e.guid(2), e.guid(3)}, ownedGuids(e, e.walkMissing(t, "", 500)),
		"with after omitted the listing starts at the lowest guid")

	page := e.listMissing(t, "limit=500&after="+e.guid(1))
	for _, g := range page.guids() {
		require.Greater(t, g, e.guid(1), "after is exclusive")
	}
	require.Equal(t, []string{e.guid(2), e.guid(3)}, ownedGuids(e, e.walkMissing(t, e.guid(1), 500)))
}

// spec: MAC-057.lists-body-less-rows
func TestMessagesEnrichment_MissingBodies_DefaultLimitIs500(t *testing.T) {
	e := setupEnrichmentEnv(t)
	for n := 1; n <= 501; n++ {
		e.seed(t, n, nil)
	}

	page := e.listMissing(t, "")
	require.Len(t, page.Items, 500)
	require.NotNil(t, page.NextAfter, "more body-less rows remain")
	require.Equal(t, page.Items[499].Guid, *page.NextAfter)
}

// spec: MAC-057.lists-body-less-rows
func TestMessagesEnrichment_MissingBodies_LimitBounds(t *testing.T) {
	e := setupEnrichmentEnv(t)
	for n := 1; n <= 3; n++ {
		e.seed(t, n, nil)
	}

	require.Len(t, e.listMissing(t, "limit=1").Items, 1)
	require.LessOrEqual(t, len(e.listMissing(t, "limit=500").Items), 500)

	for _, bad := range []string{"limit=0", "limit=501"} {
		w := macHTTP(t, e.macHostTestEnv, http.MethodGet, e.missingPath(bad), e.headers, nil)
		require.Equal(t, http.StatusBadRequest, w.Code, "%s; body: %s", bad, w.Body.String())
	}
}

func TestMessagesEnrichment_MissingBodies_RequiresHostCredentials(t *testing.T) {
	e := setupEnrichmentEnv(t)

	w := macHTTP(t, e.macHostTestEnv, http.MethodGet, e.missingPath(""), nil, nil)
	require.Equal(t, http.StatusUnauthorized, w.Code, "body: %s", w.Body.String())
}

// --- POST /api/v1/host/:id/messages/enrichment ---

type fillResponse struct {
	Results []struct {
		Guid    string `json:"guid"`
		Outcome string `json:"outcome"`
	} `json:"results"`
	Totals struct {
		Filled    int `json:"filled"`
		Unchanged int `json:"unchanged"`
		Unknown   int `json:"unknown"`
	} `json:"totals"`
}

func (r fillResponse) outcomes() []string {
	out := make([]string, len(r.Results))
	for i, res := range r.Results {
		out[i] = res.Outcome
	}
	return out
}

func fillItem(guid, body string) map[string]any {
	return map[string]any{"op": "fill_body", "guid": guid, "body": body}
}

func (e *enrichmentEnv) fillPath() string {
	return "/api/v1/host/" + e.hostID.String() + "/messages/enrichment"
}

func (e *enrichmentEnv) postFill(t *testing.T, items ...map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	return macHTTP(t, e.macHostTestEnv, http.MethodPost, e.fillPath(), e.headers, map[string]any{"items": items})
}

func (e *enrichmentEnv) fill(t *testing.T, items ...map[string]any) fillResponse {
	t.Helper()
	w := e.postFill(t, items...)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	requireDataKeys(t, w, map[string][]string{
		"":        {"results", "totals"},
		"results": {"guid", "outcome"},
		"totals":  {"filled", "unchanged", "unknown", "added", "outside_window", "cleared"},
	})
	var res fillResponse
	readData(t, w, &res)
	return res
}

func (e *enrichmentEnv) postRaw(t *testing.T, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, e.fillPath(), bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range e.headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	e.router.ServeHTTP(w, req)
	return w
}

// stored reads a row whether or not it is soft-deleted.
func (e *enrichmentEnv) stored(t *testing.T, n int) *repository.MessagesMessage {
	t.Helper()
	msg, err := e.messages.TestGetMessageIncludingDeleted(t.Context(), e.guid(n))
	require.NoError(t, err)
	return msg
}

func (e *enrichmentEnv) requireBodyless(t *testing.T, n int) {
	t.Helper()
	text := e.stored(t, n).Text
	require.True(t, text == nil || *text == "", "row %d should still have no body, has %v", n, text)
}

// spec: MAC-057.fills-missing-body
func TestMessagesEnrichment_Fill_NullTextRowGainsBody(t *testing.T) {
	e := setupEnrichmentEnv(t)
	e.seed(t, 1, nil)

	res := e.fill(t, fillItem(e.guid(1), "recovered body"))

	require.Equal(t, []string{"filled"}, res.outcomes())
	require.Equal(t, strPtr("recovered body"), e.stored(t, 1).Text)
}

// spec: MAC-057.fills-missing-body
func TestMessagesEnrichment_Fill_EmptyTextRowGainsBody(t *testing.T) {
	e := setupEnrichmentEnv(t)
	e.seed(t, 1, strPtr(""))

	res := e.fill(t, fillItem(e.guid(1), "recovered body"))

	require.Equal(t, []string{"filled"}, res.outcomes())
	require.Equal(t, strPtr("recovered body"), e.stored(t, 1).Text)
}

// spec: MAC-057.keeps-existing-body
func TestMessagesEnrichment_Fill_RowWithBodyKeepsIt(t *testing.T) {
	e := setupEnrichmentEnv(t)
	e.seed(t, 1, strPtr("original"))

	res := e.fill(t, fillItem(e.guid(1), "a different body"))

	require.Equal(t, []string{"unchanged"}, res.outcomes())
	require.Equal(t, strPtr("original"), e.stored(t, 1).Text)
}

// spec: MAC-057.unknown-guid-not-created
func TestMessagesEnrichment_Fill_UnknownGuidCreatesNothing(t *testing.T) {
	e := setupEnrichmentEnv(t)

	res := e.fill(t, fillItem(e.guid(1), "orphan body"))

	require.Equal(t, []string{"unknown"}, res.outcomes())
	_, err := e.messages.TestGetMessageIncludingDeleted(t.Context(), e.guid(1))
	require.Error(t, err, "no row is created for an unknown guid")
}

// spec: MAC-057.unknown-guid-not-created
func TestMessagesEnrichment_Fill_SoftDeletedRowIsUnknownAndUnchanged(t *testing.T) {
	e := setupEnrichmentEnv(t)
	gone := e.seed(t, 1, nil)
	require.NoError(t, e.messages.TestSoftDeleteMessage(t.Context(), gone.ID))
	before := e.stored(t, 1)

	res := e.fill(t, fillItem(e.guid(1), "body for a deleted row"))

	require.Equal(t, []string{"unknown"}, res.outcomes())
	require.Equal(t, before, e.stored(t, 1))
}

// seedLinked stages rows in the three states the enrichment must not disturb:
// processed and linked to an interaction, claimed by an aggregation session,
// and merely matched to a contact. It returns the contact.
func (e *enrichmentEnv) seedLinked(t *testing.T) *repository.Contact {
	t.Helper()
	ctx := t.Context()
	contactRepo := repository.NewContactRepository(e.database.Queries)
	contactRepo.SetPool(e.database.Pool)
	interactionRepo := repository.NewInteractionRepository(e.database.Queries)

	contact, err := contactRepo.CreateContact(ctx, repository.CreateContactRequest{
		FullName: "Enrichment Test " + e.prefix,
		Cadence:  strPtr("weekly"),
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		cleanCtx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 10*time.Second)
		defer cancel()
		_ = e.messages.HardDeleteByMacHost(cleanCtx, e.hostID)
		_ = contactRepo.HardDeleteContact(cleanCtx, contact.ID)
	})

	processed := e.seedFor(t, 1, nil, &contact.ID)
	claimed := e.seedFor(t, 2, strPtr(""), &contact.ID)
	e.seedFor(t, 3, nil, &contact.ID)

	interaction, err := interactionRepo.CreateInteraction(ctx, repository.CreateInteractionRequest{
		ContactID:  contact.ID,
		Source:     repository.InteractionSourceMessages,
		OccurredAt: time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC),
		Direction:  repository.InteractionDirectionInbound,
	})
	require.NoError(t, err)
	require.NoError(t, e.messages.MarkMessagesProcessed(ctx, []uuid.UUID{processed.ID}, interaction.ID))
	claimedIDs, err := e.messages.ClaimMessages(ctx, []uuid.UUID{claimed.ID}, "enrichment-test-session")
	require.NoError(t, err)
	require.Len(t, claimedIDs, 1)
	return contact
}

// spec: MAC-057.row-identity-preserved
func TestMessagesEnrichment_Fill_FilledRowKeepsItsIdentityAndState(t *testing.T) {
	e := setupEnrichmentEnv(t)
	e.seedLinked(t)
	before := []*repository.MessagesMessage{e.stored(t, 1), e.stored(t, 2), e.stored(t, 3)}
	require.NotNil(t, before[0].InteractionID)
	require.NotNil(t, before[0].ProcessedAt)
	require.NotNil(t, before[1].ClaimedAt)

	res := e.fill(t, fillItem(e.guid(1), "one"), fillItem(e.guid(2), "two"), fillItem(e.guid(3), "three"))

	require.Equal(t, []string{"filled", "filled", "filled"}, res.outcomes())
	for i, want := range []string{"one", "two", "three"} {
		after := e.stored(t, i+1)
		require.Equal(t, strPtr(want), after.Text)
		expected := *before[i]
		expected.Text = after.Text
		require.Equal(t, expected, *after, "only the body of row %d changes", i+1)
	}
}

// spec: MAC-057.no-cadence-or-event-effect
func TestMessagesEnrichment_Fill_HasNoCadenceFollowUpOrEventEffect(t *testing.T) {
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

	e.fill(t, fillItem(e.guid(1), "one"), fillItem(e.guid(3), "three"))

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

// spec: MAC-057.fills-missing-body, MAC-057.keeps-existing-body, MAC-057.unknown-guid-not-created
func TestMessagesEnrichment_Fill_MixedRequestAnswersEachItemInOrder(t *testing.T) {
	e := setupEnrichmentEnv(t)
	e.seed(t, 1, nil)
	e.seed(t, 2, strPtr("kept"))

	res := e.fill(t,
		fillItem(e.guid(3), "orphan"),
		fillItem(e.guid(1), "filled body"),
		fillItem(e.guid(2), "ignored"),
	)

	require.Equal(t, []string{"unknown", "filled", "unchanged"}, res.outcomes())
	require.Equal(t, []string{e.guid(3), e.guid(1), e.guid(2)}, []string{res.Results[0].Guid, res.Results[1].Guid, res.Results[2].Guid})
	require.Equal(t, 1, res.Totals.Filled)
	require.Equal(t, 1, res.Totals.Unchanged)
	require.Equal(t, 1, res.Totals.Unknown)
}

// spec: MAC-057.second-run-unchanged
func TestMessagesEnrichment_Fill_IdenticalSecondRequestIsAllUnchanged(t *testing.T) {
	e := setupEnrichmentEnv(t)
	e.seed(t, 1, nil)
	e.seed(t, 2, strPtr(""))
	items := []map[string]any{fillItem(e.guid(1), "one"), fillItem(e.guid(2), "two")}

	first := e.fill(t, items...)
	require.Equal(t, 2, first.Totals.Filled)
	second := e.fill(t, items...)

	require.Equal(t, []string{"unchanged", "unchanged"}, second.outcomes())
	require.Equal(t, 0, second.Totals.Filled)
	require.Equal(t, 2, second.Totals.Unchanged)
	require.Equal(t, 0, second.Totals.Unknown)
	require.Equal(t, strPtr("one"), e.stored(t, 1).Text)
	require.Equal(t, strPtr("two"), e.stored(t, 2).Text)
}

// spec: MAC-057.invalid-request-writes-nothing
func TestMessagesEnrichment_Fill_UnknownOpWritesNothing(t *testing.T) {
	e := setupEnrichmentEnv(t)
	e.seed(t, 1, nil)

	w := e.postFill(t, fillItem(e.guid(1), "valid"), map[string]any{"op": "delete_row", "guid": e.guid(1), "body": "x"})

	require.Equal(t, http.StatusBadRequest, w.Code, "body: %s", w.Body.String())
	e.requireBodyless(t, 1)
}

// spec: MAC-057.invalid-request-writes-nothing
func TestMessagesEnrichment_Fill_ItemWithoutGuidWritesNothing(t *testing.T) {
	e := setupEnrichmentEnv(t)
	e.seed(t, 1, nil)

	w := e.postFill(t, fillItem(e.guid(1), "valid"), map[string]any{"op": "fill_body", "body": "x"})

	require.Equal(t, http.StatusBadRequest, w.Code, "body: %s", w.Body.String())
	e.requireBodyless(t, 1)
}

// spec: MAC-057.invalid-request-writes-nothing
func TestMessagesEnrichment_Fill_ItemWithoutBodyWritesNothing(t *testing.T) {
	e := setupEnrichmentEnv(t)
	e.seed(t, 1, nil)

	w := e.postFill(t, fillItem(e.guid(1), "valid"), map[string]any{"op": "fill_body", "guid": e.guid(2)})

	require.Equal(t, http.StatusBadRequest, w.Code, "body: %s", w.Body.String())
	e.requireBodyless(t, 1)
}

// spec: MAC-057.invalid-request-writes-nothing
func TestMessagesEnrichment_Fill_ItemWithEmptyBodyWritesNothing(t *testing.T) {
	e := setupEnrichmentEnv(t)
	e.seed(t, 1, nil)

	w := e.postFill(t, fillItem(e.guid(1), "valid"), fillItem(e.guid(2), ""))

	require.Equal(t, http.StatusBadRequest, w.Code, "body: %s", w.Body.String())
	e.requireBodyless(t, 1)
}

// spec: MAC-057.fills-missing-body
func TestMessagesEnrichment_Fill_AcceptsFiveHundredItems(t *testing.T) {
	e := setupEnrichmentEnv(t)
	items := make([]map[string]any, 500)
	for i := range items {
		items[i] = fillItem(e.guid(i+1), "body")
	}

	res := e.fill(t, items...)

	require.Len(t, res.Results, 500)
}

// spec: MAC-057.invalid-request-writes-nothing
func TestMessagesEnrichment_Fill_FiveHundredAndOneItemsWriteNothing(t *testing.T) {
	e := setupEnrichmentEnv(t)
	e.seed(t, 1, nil)
	items := make([]map[string]any, 501)
	for i := range items {
		items[i] = fillItem(e.guid(i+1), "body")
	}

	w := e.postFill(t, items...)

	require.Equal(t, http.StatusBadRequest, w.Code, "body: %s", w.Body.String())
	e.requireBodyless(t, 1)
}

// sizedFillBody builds a valid one-item request whose JSON is exactly size bytes.
func sizedFillBody(t *testing.T, guid string, size int) []byte {
	t.Helper()
	build := func(pad int) []byte {
		b, err := json.Marshal(map[string]any{"items": []map[string]any{fillItem(guid, strings.Repeat("x", pad))}})
		require.NoError(t, err)
		return b
	}
	pad := size - len(build(0))
	require.Positive(t, pad)
	body := build(pad)
	require.Len(t, body, size)
	return body
}

// spec: MAC-057.fills-missing-body
func TestMessagesEnrichment_Fill_AcceptsBodyOfExactlyEightMiB(t *testing.T) {
	e := setupEnrichmentEnv(t)
	e.seed(t, 1, nil)

	w := e.postRaw(t, sizedFillBody(t, e.guid(1), 8<<20))

	require.Equal(t, http.StatusOK, w.Code)
	var res fillResponse
	readData(t, w, &res)
	require.Equal(t, []string{"filled"}, res.outcomes())
}

// spec: MAC-057.invalid-request-writes-nothing
func TestMessagesEnrichment_Fill_RejectsBodyOverEightMiBWithoutWriting(t *testing.T) {
	e := setupEnrichmentEnv(t)
	e.seed(t, 1, nil)

	w := e.postRaw(t, sizedFillBody(t, e.guid(1), 8<<20+1))

	require.Equal(t, http.StatusRequestEntityTooLarge, w.Code)
	e.requireBodyless(t, 1)
}

func TestMessagesEnrichment_Fill_RequiresHostCredentials(t *testing.T) {
	e := setupEnrichmentEnv(t)
	e.seed(t, 1, nil)

	w := macHTTP(t, e.macHostTestEnv, http.MethodPost, e.fillPath(), nil,
		map[string]any{"items": []map[string]any{fillItem(e.guid(1), "body")}})

	require.Equal(t, http.StatusUnauthorized, w.Code, "body: %s", w.Body.String())
	e.requireBodyless(t, 1)
}
