package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"personal-crm/backend/internal/api"
	"personal-crm/backend/internal/api/handlers"
	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The operations endpoint's HTTP surface. CON-062 is surface: api, so these
// tests are its citing coverage — the service-level suite in
// backend/tests/contact_method_operations_integration_test.go pins the fold and
// apply mechanisms, but only these show the endpoint accepts, serializes, and
// returns them.

// methodOpsShared is one database pool and one router shared by every test in
// this file. Each test opening its own pool would multiply the per-package
// connection ceiling by the test count; the router is stateless, and every test
// works on its own contact, so sharing is safe under t.Parallel().
var (
	methodOpsSharedOnce sync.Once
	methodOpsSharedDB   *db.Database
	methodOpsSharedRtr  *gin.Engine
)

func methodOpsShared(t *testing.T) (*db.Database, *gin.Engine) {
	t.Helper()
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}

	methodOpsSharedOnce.Do(func() {
		cfg := config.TestConfig()
		cfg.Database.URL = os.Getenv("DATABASE_URL")
		database, err := db.NewDatabase(context.Background(), cfg.Database)
		if err != nil {
			panic("failed to connect to test database: " + err.Error())
		}
		methodOpsSharedDB = database
		// nil bus and nil rematch registry: these tests pin HTTP behavior, not
		// the event contract, and the service already treats both as optional.
		methodOpsSharedRtr = newMethodOpsRouter(service.NewContactMethodService(database, nil, nil))
	})
	if methodOpsSharedDB == nil {
		t.Skip("shared method-ops database unavailable")
	}
	return methodOpsSharedDB, methodOpsSharedRtr
}

// newMethodOpsRouter registers the contact-method route surface through the
// production route registrar, so route shape is exercised rather than restated.
func newMethodOpsRouter(applier handlers.ContactMethodApplier) *gin.Engine {
	router := gin.New()
	router.Use(api.RequestIDMiddleware())
	v1 := router.Group("/api/v1")
	contacts := v1.Group("/contacts")
	contacts.POST("/:id/methods", handlers.NewContactMethodHandler(applier).ApplyOperations)
	return router
}

type methodOpsFx struct {
	t         *testing.T
	ctx       context.Context
	router    *gin.Engine
	repo      *repository.ContactMethodRepository
	contactID uuid.UUID
}

// newMethodOpsFx creates one contact in this test's own namespace. Fixtures are
// never shared between tests, so parallel copies cannot observe each other.
func newMethodOpsFx(t *testing.T) *methodOpsFx {
	t.Helper()
	database, router := methodOpsShared(t)
	ctx := context.Background()

	contactRepo := repository.NewContactRepository(database.Queries)
	contact, err := contactRepo.CreateContact(ctx, repository.CreateContactRequest{
		FullName: "MethodOpsAPI " + uuid.NewString(),
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = contactRepo.HardDeleteContact(ctx, contact.ID) })

	return &methodOpsFx{
		t:         t,
		ctx:       ctx,
		router:    router,
		repo:      repository.NewContactMethodRepository(database.Queries),
		contactID: contact.ID,
	}
}

// seed inserts a method server-side, bypassing the endpoint. A method the
// client never saw is exactly what the regression is about.
func (f *methodOpsFx) seed(methodType, value string, primary bool) repository.ContactMethod {
	f.t.Helper()
	m, err := f.repo.CreateContactMethod(f.ctx, repository.CreateContactMethodRequest{
		ContactID: f.contactID,
		Type:      methodType,
		Value:     value,
		IsPrimary: primary,
	})
	require.NoError(f.t, err)
	return *m
}

func (f *methodOpsFx) stored() []repository.ContactMethod {
	f.t.Helper()
	got, err := f.repo.ListContactMethodsByContact(f.ctx, f.contactID)
	require.NoError(f.t, err)
	return got
}

func (f *methodOpsFx) storedByID(id uuid.UUID) *repository.ContactMethod {
	f.t.Helper()
	for _, m := range f.stored() {
		if m.ID == id {
			found := m
			return &found
		}
	}
	return nil
}

// opsResponse mirrors the wire response, unwrapped from the APIResponse
// envelope so assertions read against the documented shape.
type opsResponse struct {
	Methods      []handlers.ContactMethodResponse        `json:"methods"`
	RematchJobID string                                  `json:"rematch_job_id"`
	Results      []handlers.ContactMethodOperationResult `json:"results"`
}

func (f *methodOpsFx) post(ops ...map[string]any) (*httptest.ResponseRecorder, opsResponse) {
	f.t.Helper()
	return f.postTo(f.contactID.String(), ops...)
}

func (f *methodOpsFx) postTo(contactID string, ops ...map[string]any) (*httptest.ResponseRecorder, opsResponse) {
	f.t.Helper()
	if ops == nil {
		ops = []map[string]any{}
	}
	body, err := json.Marshal(map[string]any{"operations": ops})
	require.NoError(f.t, err)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/contacts/"+contactID+"/methods", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	f.router.ServeHTTP(w, req)

	var envelope struct {
		Data opsResponse `json:"data"`
	}
	if w.Code == http.StatusOK {
		require.NoError(f.t, json.Unmarshal(w.Body.Bytes(), &envelope))
	}
	return w, envelope.Data
}

func addOp(methodType, value string) map[string]any {
	return map[string]any{"op": "add", "type": methodType, "value": value}
}

func addPrimaryOp(methodType, value string) map[string]any {
	return map[string]any{"op": "add", "type": methodType, "value": value, "is_primary": true}
}

func updateOp(id uuid.UUID, methodType, value string) map[string]any {
	return map[string]any{"op": "update", "method_id": id.String(), "type": methodType, "value": value}
}

func idOp(op string, id uuid.UUID) map[string]any {
	return map[string]any{"op": op, "method_id": id.String()}
}

// uniqueEmail keeps every fixture's values distinct across parallel tests, so a
// duplicate-value rejection can only come from the payload under test.
func uniqueEmail() string {
	return "m" + uuid.NewString()[:8] + "@example.test"
}

// --- CON-062[0]: absence expresses nothing ---------------------------------

// TestMethodOps_UnnamedMethodsSurvive is the regression. A method the payload
// does not name must come through byte-identical in EVERY persisted column.
//
// The full-row assertion is the point, not thoroughness for its own sake: the
// apply stage works by physical delete-and-reinsert, so an implementation that
// needlessly reinserted every row would preserve id, type, value and is_primary
// while silently moving created_at and updated_at — passing a weaker test named
// for catching exactly that.
func TestMethodOps_UnnamedMethodsSurvive(t *testing.T) {
	t.Parallel()
	f := newMethodOpsFx(t)
	untouched := f.seed("email", uniqueEmail(), true)

	w, body := f.post(addOp("phone", "+15550101234"))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Len(t, body.Methods, 2)

	after := f.storedByID(untouched.ID)
	require.NotNil(t, after, "a method no operation named was destroyed")
	assert.Equal(t, untouched.ID, after.ID)
	assert.Equal(t, untouched.Type, after.Type)
	assert.Equal(t, untouched.Value, after.Value)
	assert.Equal(t, untouched.ValueNormalized, after.ValueNormalized)
	assert.Equal(t, untouched.IsPrimary, after.IsPrimary)
	assert.Equal(t, untouched.CreatedAt, after.CreatedAt)
	assert.Equal(t, untouched.UpdatedAt, after.UpdatedAt,
		"updated_at moved: the row was rewritten despite no operation naming it")
}

// TestMethodOps_PromoteDemotesPreviousPrimary pins the one carve-out to the
// rule above: at most one primary is a database constraint, so promoting one
// necessarily demotes another. That is a real side effect on an unnamed row and
// is stated rather than hidden.
func TestMethodOps_PromoteDemotesPreviousPrimary(t *testing.T) {
	t.Parallel()
	f := newMethodOpsFx(t)
	oldPrimary := f.seed("email", uniqueEmail(), true)
	newPrimary := f.seed("phone", "+15550102345", false)

	w, _ := f.post(idOp("set_primary", newPrimary.ID))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	assert.False(t, f.storedByID(oldPrimary.ID).IsPrimary, "previous primary was not demoted")
	assert.True(t, f.storedByID(newPrimary.ID).IsPrimary, "named row was not promoted")
}

// TestMethodOps_ClearNonPrimaryDoesNotDemoteCurrentPrimary catches an
// implementation that clears the primary flag contact-wide instead of for the
// named row. A global clear would let a stale form demote a row it never saw.
func TestMethodOps_ClearNonPrimaryDoesNotDemoteCurrentPrimary(t *testing.T) {
	t.Parallel()
	f := newMethodOpsFx(t)
	primary := f.seed("email", uniqueEmail(), true)
	other := f.seed("phone", "+15550103456", false)

	w, _ := f.post(idOp("clear_primary", other.ID))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	assert.True(t, f.storedByID(primary.ID).IsPrimary,
		"clearing a non-primary row demoted the actual primary")
}

func TestMethodOps_ClearNamedPrimaryLeavesNone(t *testing.T) {
	t.Parallel()
	f := newMethodOpsFx(t)
	primary := f.seed("email", uniqueEmail(), true)

	w, body := f.post(idOp("clear_primary", primary.ID))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	assert.False(t, f.storedByID(primary.ID).IsPrimary)
	for _, m := range body.Methods {
		assert.False(t, m.IsPrimary, "response still reports a primary")
	}
}

// --- CON-062[1]: all or nothing --------------------------------------------

func TestMethodOps_InvalidOperationAppliesNothing(t *testing.T) {
	t.Parallel()
	f := newMethodOpsFx(t)
	existing := f.seed("email", uniqueEmail(), true)
	before := f.stored()

	// A valid add alongside an operation naming a nonexistent id.
	w, _ := f.post(
		addOp("phone", "+15550104567"),
		idOp("set_primary", uuid.New()),
	)
	require.Equal(t, http.StatusBadRequest, w.Code)

	after := f.stored()
	require.Len(t, after, len(before), "a rejected payload applied part of itself")
	assert.Equal(t, existing.Value, f.storedByID(existing.ID).Value)
}

// TestMethodOps_AddWithIsPrimaryPromotesAfterInsert asserts WHICH row ends up
// primary, not merely that one does. Counting primaries alone is vacuous here:
// the seeded row is already primary, so an implementation that dropped
// promotion entirely would leave exactly one primary and pass.
func TestMethodOps_AddWithIsPrimaryPromotesAfterInsert(t *testing.T) {
	t.Parallel()
	f := newMethodOpsFx(t)
	seeded := f.seed("email", uniqueEmail(), true)

	// Inserts are always non-primary and promotion is always last, so this can
	// never violate the one-primary index mid-apply.
	w, body := f.post(addPrimaryOp("phone", "+15550105678"))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	primaries := []string{}
	for _, m := range body.Methods {
		if m.IsPrimary {
			primaries = append(primaries, m.ID)
		}
	}
	require.Len(t, primaries, 1, "expected exactly one primary")
	assert.NotEqual(t, seeded.ID.String(), primaries[0],
		"the added row was never promoted; the seeded primary just stayed put")

	added := f.storedByID(uuid.MustParse(primaries[0]))
	require.NotNil(t, added)
	assert.Equal(t, "phone", added.Type, "the promoted row is not the added row")
	assert.False(t, f.storedByID(seeded.ID).IsPrimary, "the seeded primary was not demoted")
}

// --- CON-062[2]: order independence ----------------------------------------

// TestMethodOps_OutcomeIndependentOfPayloadOrder uses a payload whose classes
// genuinely interact: the add reuses the key the update frees, so a fold that
// does not order update before add produces a duplicate; and the primary moves
// to a row created in the same payload, exercising demote-then-promote.
//
// Each permutation runs against its OWN fresh fixture. Run sequentially against
// one contact, only the first permutation would start from the specified
// pre-state and the rest would be replays that pass even if the permutation
// would have failed from the original state.
func TestMethodOps_OutcomeIndependentOfPayloadOrder(t *testing.T) {
	t.Parallel()

	// Semantic comparison: server-generated ids and timestamps differ per
	// fixture, so compare (type, value, is_primary) plus whether the row kept
	// its identity relative to THAT fixture's own pre-state.
	type semanticRow struct {
		Type      string
		Value     string
		IsPrimary bool
		KeptID    bool
	}

	runPermutation := func(t *testing.T, order []int) ([]semanticRow, string) {
		f := newMethodOpsFx(t)
		reused := uniqueEmail()
		a := f.seed("email", reused, true)
		b := f.seed("phone", "+15550106789", false)

		all := []map[string]any{
			idOp("remove", b.ID),
			updateOp(a.ID, "email", uniqueEmail()),
			addPrimaryOp("email", reused),
		}
		payload := make([]map[string]any, 0, len(all))
		for _, i := range order {
			payload = append(payload, all[i])
		}

		w, _ := f.post(payload...)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())

		out := make([]semanticRow, 0, 2)
		for _, m := range f.stored() {
			out = append(out, semanticRow{
				Type:      m.Type,
				Value:     m.Value,
				IsPrimary: m.IsPrimary,
				KeptID:    m.ID == a.ID,
			})
		}
		return out, reused
	}

	// The update's new value is freshly generated per permutation, so compare
	// the shape that does not depend on it: the reused key must be present and
	// primary, and the updated row must have kept A's identity.
	//
	// Asserting WHICH row is primary is load-bearing. Counting primaries alone
	// is satisfied by the pre-state primary staying put, so an implementation
	// that never moved the designation at all would pass every permutation.
	assertShape := func(t *testing.T, rows []semanticRow, reusedValue string) {
		require.Len(t, rows, 2)
		kept := 0
		primaries := []semanticRow{}
		for _, r := range rows {
			if r.IsPrimary {
				primaries = append(primaries, r)
			}
			if r.KeptID {
				kept++
			}
		}
		assert.Equal(t, 1, kept, "the updated row did not preserve its identity")
		require.Len(t, primaries, 1, "expected exactly one primary")
		assert.Equal(t, reusedValue, primaries[0].Value,
			"the primary is not the row the add created; the designation never moved")
		assert.False(t, primaries[0].KeptID,
			"the primary is still the pre-state row A rather than the added row")
	}

	permutations := [][]int{
		{0, 1, 2}, {0, 2, 1}, {1, 0, 2},
		{1, 2, 0}, {2, 0, 1}, {2, 1, 0},
	}
	for _, order := range permutations {
		t.Run(fmt.Sprint(order), func(t *testing.T) {
			t.Parallel()
			rows, reused := runPermutation(t, order)
			assertShape(t, rows, reused)
		})
	}
}

// TestMethodOps_TwoAddsOrderIndependent covers intra-class ordering.
func TestMethodOps_TwoAddsOrderIndependent(t *testing.T) {
	t.Parallel()
	first, second := uniqueEmail(), uniqueEmail()

	collect := func(t *testing.T, ops ...map[string]any) []string {
		f := newMethodOpsFx(t)
		w, _ := f.post(ops...)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		values := []string{}
		for _, m := range f.stored() {
			values = append(values, m.Type+"|"+m.Value)
		}
		assert.Len(t, values, 2)
		return values
	}

	var forward, reverse []string
	t.Run("forward", func(t *testing.T) {
		forward = collect(t, addOp("email", first), addOp("email", second))
	})
	t.Run("reverse", func(t *testing.T) {
		reverse = collect(t, addOp("email", second), addOp("email", first))
	})
	assert.ElementsMatch(t, forward, reverse)
}

// --- CON-062[3][4]: idempotency --------------------------------------------

func TestMethodOps_DuplicateAddIsNoOp(t *testing.T) {
	t.Parallel()
	f := newMethodOpsFx(t)
	existing := f.seed("email", uniqueEmail(), false)

	w, body := f.post(addOp("email", existing.Value))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	assert.Len(t, f.stored(), 1, "an add of an existing value duplicated the row")
	require.Len(t, body.Results, 1)
	assert.Equal(t, "matched_existing", body.Results[0].Outcome)
	assert.Equal(t, existing.ID.String(), body.Results[0].MethodID)
}

func TestMethodOps_IdenticalAddsCoalesce(t *testing.T) {
	t.Parallel()
	f := newMethodOpsFx(t)
	value := uniqueEmail()

	w, body := f.post(addOp("email", value), addOp("email", value))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	assert.Len(t, f.stored(), 1, "identical adds created two rows")
	require.Len(t, body.Results, 2, "a coalesced add lost its result")
	assert.Equal(t, body.Results[0].MethodID, body.Results[1].MethodID)
}

func TestMethodOps_RemoveAlreadyAbsentSucceeds(t *testing.T) {
	t.Parallel()
	f := newMethodOpsFx(t)

	// An id that does not exist anywhere. A retried removal must be idempotent,
	// so this resolves through the ownership lookup's nonexistent branch.
	w, body := f.post(idOp("remove", uuid.New()))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Len(t, body.Results, 1)
	assert.Equal(t, "no_op", body.Results[0].Outcome)
}

func TestMethodOps_DuplicateRemovesCoalesce(t *testing.T) {
	t.Parallel()
	f := newMethodOpsFx(t)
	target := f.seed("email", uniqueEmail(), false)

	w, body := f.post(idOp("remove", target.ID), idOp("remove", target.ID))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	assert.Nil(t, f.storedByID(target.ID))
	require.Len(t, body.Results, 2, "a coalesced remove lost its result")
	assert.Equal(t, target.ID.String(), body.Results[0].MethodID)
	assert.Equal(t, target.ID.String(), body.Results[1].MethodID)
}

func TestMethodOps_NamedRemovalSucceeds(t *testing.T) {
	t.Parallel()
	f := newMethodOpsFx(t)
	target := f.seed("email", uniqueEmail(), false)
	survivor := f.seed("phone", "+15550107890", false)

	w, _ := f.post(idOp("remove", target.ID))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	assert.Nil(t, f.storedByID(target.ID), "the named row was not removed")
	assert.NotNil(t, f.storedByID(survivor.ID), "an unnamed row was removed")
}

// --- CON-062[5][6]: final-state validation ---------------------------------

func TestMethodOps_DuplicateFinalStateRejected(t *testing.T) {
	t.Parallel()
	f := newMethodOpsFx(t)
	a := f.seed("email", uniqueEmail(), false)
	b := f.seed("email", uniqueEmail(), false)

	// Updating B onto A's value would leave two rows with one key.
	w, _ := f.post(updateOp(b.ID, "email", a.Value))
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Equal(t, b.Value, f.storedByID(b.ID).Value, "a rejected payload mutated a row")
}

func TestMethodOps_TwoPrimaryDesignationsRejected(t *testing.T) {
	t.Parallel()
	f := newMethodOpsFx(t)
	a := f.seed("email", uniqueEmail(), false)
	b := f.seed("phone", "+15550108901", false)

	w, _ := f.post(idOp("set_primary", a.ID), idOp("set_primary", b.ID))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// --- CON-062[7]: a method id is not a capability ---------------------------

// TestMethodOps_ForeignMethodIDRejected table-drives EVERY id-bearing verb.
// The ownership check is currently shared, so one verb would cover the code as
// written — but the guarantee is per-verb, and a future verb-specific path that
// bypassed the precheck would otherwise regress silently. It is asserted where
// it protects a destructive operation, not merely where it is convenient.
func TestMethodOps_ForeignMethodIDRejected(t *testing.T) {
	t.Parallel()

	for _, verb := range []string{"remove", "update", "set_primary", "clear_primary"} {
		t.Run(verb, func(t *testing.T) {
			t.Parallel()
			victim := newMethodOpsFx(t)
			foreign := victim.seed("email", uniqueEmail(), true)
			before := *victim.storedByID(foreign.ID)

			op := idOp(verb, foreign.ID)
			if verb == "update" {
				op = updateOp(foreign.ID, "email", uniqueEmail())
			}

			attacker := newMethodOpsFx(t)
			w, _ := attacker.post(op)

			assert.Equal(t, http.StatusNotFound, w.Code,
				"an operation naming another contact's method was not rejected")

			after := victim.storedByID(foreign.ID)
			require.NotNil(t, after, "another contact's method was destroyed through a foreign id")
			assert.Equal(t, before.Value, after.Value, "another contact's method was altered")
			assert.Equal(t, before.IsPrimary, after.IsPrimary, "another contact's primary flag was changed")
			assert.Equal(t, before.UpdatedAt, after.UpdatedAt, "another contact's method row was rewritten")
			assert.Empty(t, attacker.stored(), "the operation leaked a row onto the caller's contact")
		})
	}
}

// TestMethodOps_RemoveNonexistentSucceedsButForeignRejected walks the ownership
// rule's three cases in one place. The distinction is what lets a retried
// removal succeed while a foreign id can never be acted on.
func TestMethodOps_RemoveNonexistentSucceedsButForeignRejected(t *testing.T) {
	t.Parallel()
	victim := newMethodOpsFx(t)
	foreign := victim.seed("email", uniqueEmail(), false)

	f := newMethodOpsFx(t)
	own := f.seed("email", uniqueEmail(), false)

	t.Run("own id applies", func(t *testing.T) {
		w, _ := f.post(idOp("remove", own.ID))
		assert.Equal(t, http.StatusOK, w.Code)
		assert.Nil(t, f.storedByID(own.ID))
	})
	t.Run("nonexistent id is a successful no-op", func(t *testing.T) {
		w, _ := f.post(idOp("remove", uuid.New()))
		assert.Equal(t, http.StatusOK, w.Code)
	})
	t.Run("foreign id is rejected", func(t *testing.T) {
		w, _ := f.post(idOp("remove", foreign.ID))
		assert.Equal(t, http.StatusNotFound, w.Code)
		assert.NotNil(t, victim.storedByID(foreign.ID))
	})
}

// --- CON-062[9]: conflicts rejected, not resolved by order ------------------

// TestMethodOps_ConflictingOperationsRejected covers every documented conflict,
// each asserting a 400 AND zero mutation. A payload rejected after partial
// application would be the same silent-corruption class this endpoint exists to
// eliminate.
func TestMethodOps_ConflictingOperationsRejected(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		ops  func(a, b repository.ContactMethod) []map[string]any
	}{
		{"two updates on one id", func(a, _ repository.ContactMethod) []map[string]any {
			return []map[string]any{
				updateOp(a.ID, "email", uniqueEmail()),
				updateOp(a.ID, "email", uniqueEmail()),
			}
		}},
		{"remove and update on one id", func(a, _ repository.ContactMethod) []map[string]any {
			return []map[string]any{idOp("remove", a.ID), updateOp(a.ID, "email", uniqueEmail())}
		}},
		{"remove and set_primary on one id", func(a, _ repository.ContactMethod) []map[string]any {
			return []map[string]any{idOp("remove", a.ID), idOp("set_primary", a.ID)}
		}},
		{"remove and clear_primary on one id", func(a, _ repository.ContactMethod) []map[string]any {
			return []map[string]any{idOp("remove", a.ID), idOp("clear_primary", a.ID)}
		}},
		{"set_primary and clear_primary together", func(a, b repository.ContactMethod) []map[string]any {
			return []map[string]any{idOp("set_primary", a.ID), idOp("clear_primary", b.ID)}
		}},
		{"two primary designations", func(a, b repository.ContactMethod) []map[string]any {
			return []map[string]any{idOp("set_primary", a.ID), idOp("set_primary", b.ID)}
		}},
		{"two adds differing only in stored casing", func(_, _ repository.ContactMethod) []map[string]any {
			value := uniqueEmail()
			return []map[string]any{addOp("email", value), addOp("email", strings.ToUpper(value))}
		}},
		// A primary designation on a NEW row necessarily travels on the add,
		// because a row that does not exist yet has no id for set_primary to
		// name. Two such adds are therefore two designations, and counting
		// only the explicit verbs let them through with the last one in
		// payload order winning — an order-dependent outcome.
		{"two adds each designating primary", func(_, _ repository.ContactMethod) []map[string]any {
			return []map[string]any{
				addPrimaryOp("email", uniqueEmail()),
				addPrimaryOp("phone", "+15550114567"),
			}
		}},
		{"add designating primary alongside set_primary", func(_, b repository.ContactMethod) []map[string]any {
			return []map[string]any{addPrimaryOp("email", uniqueEmail()), idOp("set_primary", b.ID)}
		}},
		{"add designating primary alongside clear_primary", func(a, _ repository.ContactMethod) []map[string]any {
			return []map[string]any{addPrimaryOp("email", uniqueEmail()), idOp("clear_primary", a.ID)}
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newMethodOpsFx(t)
			a := f.seed("email", uniqueEmail(), true)
			b := f.seed("phone", "+15550109012", false)
			before := f.stored()

			w, _ := f.post(tc.ops(a, b)...)
			assert.Equal(t, http.StatusBadRequest, w.Code, "conflict was resolved rather than rejected")

			after := f.stored()
			assert.Len(t, after, len(before), "a rejected conflict mutated stored state")
			assert.Equal(t, a.Value, f.storedByID(a.ID).Value)
			assert.Equal(t, b.IsPrimary, f.storedByID(b.ID).IsPrimary)
		})
	}
}

// TestMethodOps_UpdateWithPrimaryDesignationSucceeds pins the inverse: an
// update and a primary designation naming the same id is a legitimate
// combination, not a conflict. Nothing else covers the permitted case, so a
// validator that rejected all same-id pairs would otherwise look correct.
func TestMethodOps_UpdateWithPrimaryDesignationSucceeds(t *testing.T) {
	t.Parallel()
	f := newMethodOpsFx(t)
	a := f.seed("email", uniqueEmail(), false)
	newValue := uniqueEmail()

	w, _ := f.post(updateOp(a.ID, "email", newValue), idOp("set_primary", a.ID))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	after := f.storedByID(a.ID)
	require.NotNil(t, after)
	assert.Equal(t, newValue, after.Value, "the update did not land")
	assert.True(t, after.IsPrimary, "the primary designation did not land")
}

// --- CON-062[10][11]: stable-key updates over HTTP --------------------------

// TestMethodOps_StableKeyUpdatePersistsThroughAPI is CON-062[10]'s citing test.
// A case-only edit changes the stored value while (type, value_normalized) is
// unchanged, so the row is never deleted and never reinserted. Without the
// in-place update phase the endpoint returns 200 having discarded the edit —
// the failure must land on the stored-value assertion, not on an error.
func TestMethodOps_StableKeyUpdatePersistsThroughAPI(t *testing.T) {
	t.Parallel()
	f := newMethodOpsFx(t)
	lower := uniqueEmail()
	a := f.seed("email", lower, false)
	upper := strings.ToUpper(lower)

	w, body := f.post(updateOp(a.ID, "email", upper))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	after := f.storedByID(a.ID)
	require.NotNil(t, after)
	assert.Equal(t, upper, after.Value, "a value edit that did not move the key was silently discarded")
	assert.Equal(t, a.ID, after.ID, "row identity was not preserved")
	assert.Equal(t, a.CreatedAt, after.CreatedAt, "created_at was not preserved")

	require.Len(t, body.Methods, 1)
	assert.Equal(t, upper, body.Methods[0].Value, "the response did not reflect the new value")
}

// TestMethodOps_KeyChangingPrimaryUpdateKeepsPrimaryThroughAPI is CON-062[11]'s
// citing test. Updating the primary's value changes its key, forcing the
// delete-and-reinsert path, which returns the row non-primary. A promotion rule
// phrased as a designation delta fires neither demote nor promote here and
// leaves the contact with NO primary.
func TestMethodOps_KeyChangingPrimaryUpdateKeepsPrimaryThroughAPI(t *testing.T) {
	t.Parallel()
	f := newMethodOpsFx(t)
	a := f.seed("email", uniqueEmail(), true)
	f.seed("phone", "+15550110123", false)
	newValue := uniqueEmail()

	w, body := f.post(updateOp(a.ID, "email", newValue))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	after := f.storedByID(a.ID)
	require.NotNil(t, after)
	assert.Equal(t, newValue, after.Value)
	assert.True(t, after.IsPrimary, "the updated row lost its primary flag")

	primaries := 0
	for _, m := range body.Methods {
		if m.IsPrimary {
			primaries++
		}
	}
	assert.Equal(t, 1, primaries, "expected exactly one primary in the response")
}

// --- CON-062[12][13]: the results contract ----------------------------------

// TestMethodOps_ResultsCoverEveryOperation pins one result per SUBMITTED
// operation, including no-ops. This is the contract a later acknowledged-state
// advance depends on: a client that cannot learn which row its own addition
// resolved to will later fail to edit or remove that method.
func TestMethodOps_ResultsCoverEveryOperation(t *testing.T) {
	t.Parallel()
	f := newMethodOpsFx(t)
	existing := f.seed("email", uniqueEmail(), false)
	absent := uuid.New()

	w, body := f.post(
		addOp("phone", "+15550111234"), // created
		addOp("email", existing.Value), // matched_existing, a no-op
		idOp("remove", absent),         // no_op, no row ever existed
	)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Len(t, body.Results, 3, "results were emitted only for operations that changed a row")

	for i, r := range body.Results {
		assert.Equal(t, i, r.Index, "results are not at their submitted indices")
		assert.NotEmpty(t, r.MethodID, "every result must identify the method it addressed")
	}
	assert.Equal(t, "created", body.Results[0].Outcome)
	assert.NotNil(t, body.Results[0].Method)

	assert.Equal(t, "matched_existing", body.Results[1].Outcome)
	assert.Equal(t, existing.ID.String(), body.Results[1].MethodID)
	require.NotNil(t, body.Results[1].Method, "a matched add must carry the resolved row")

	assert.Equal(t, "no_op", body.Results[2].Outcome)
	assert.Equal(t, absent.String(), body.Results[2].MethodID)
	assert.Nil(t, body.Results[2].Method, "a removal must not carry a snapshot")
}

// TestMethodOps_ResultsIndexAgainstSubmittedNotFoldedOperations uses payloads
// whose folds are NOT one-to-one. Earlier cases all folded one-to-one, so an
// implementation emitting one result per folded operation passed them.
func TestMethodOps_ResultsIndexAgainstSubmittedNotFoldedOperations(t *testing.T) {
	t.Parallel()

	t.Run("two identical adds", func(t *testing.T) {
		t.Parallel()
		f := newMethodOpsFx(t)
		value := uniqueEmail()

		w, body := f.post(addOp("email", value), addOp("email", value))
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		require.Len(t, body.Results, 2, "coalesced adds did not each get a result")
		assert.Equal(t, 0, body.Results[0].Index)
		assert.Equal(t, 1, body.Results[1].Index)
		assert.Equal(t, body.Results[0].MethodID, body.Results[1].MethodID,
			"coalesced siblings resolved to different rows")
	})

	t.Run("two identical removes", func(t *testing.T) {
		t.Parallel()
		f := newMethodOpsFx(t)
		target := f.seed("email", uniqueEmail(), false)

		w, body := f.post(idOp("remove", target.ID), idOp("remove", target.ID))
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		require.Len(t, body.Results, 2, "coalesced removes did not each get a result")
		assert.Equal(t, 0, body.Results[0].Index)
		assert.Equal(t, 1, body.Results[1].Index)
	})
}

// TestMethodOps_ResultsCarryResolvedRowSnapshot pins that snapshots reflect
// POST-apply state rather than echoing the submitted operation's fields.
func TestMethodOps_ResultsCarryResolvedRowSnapshot(t *testing.T) {
	t.Parallel()
	f := newMethodOpsFx(t)
	a := f.seed("email", uniqueEmail(), false)
	b := f.seed("phone", "+15550112345", false)
	newValue := uniqueEmail()

	w, body := f.post(updateOp(a.ID, "email", newValue), idOp("set_primary", b.ID))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Len(t, body.Results, 2)

	require.NotNil(t, body.Results[0].Method)
	assert.Equal(t, newValue, body.Results[0].Method.Value, "the update's snapshot is stale")

	require.NotNil(t, body.Results[1].Method)
	assert.True(t, body.Results[1].Method.IsPrimary,
		"the set_primary snapshot does not reflect post-apply state")
}

// TestMethodOps_RemoveResultsCarryAddressedIDWithoutSnapshot pins the
// discriminated shape. A uniform "snapshot on every result" contract is
// unsatisfiable for either case here: a removed row has no post-apply state,
// and an absent-id removal has no row at all.
func TestMethodOps_RemoveResultsCarryAddressedIDWithoutSnapshot(t *testing.T) {
	t.Parallel()
	f := newMethodOpsFx(t)
	real := f.seed("email", uniqueEmail(), false)
	absent := uuid.New()

	w, body := f.post(idOp("remove", real.ID), idOp("remove", absent))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Len(t, body.Results, 2)

	assert.Equal(t, real.ID.String(), body.Results[0].MethodID)
	assert.Nil(t, body.Results[0].Method, "a real removal carried a snapshot")
	assert.Equal(t, "removed", body.Results[0].Outcome)

	assert.Equal(t, absent.String(), body.Results[1].MethodID,
		"an absent removal must report the SUBMITTED id")
	assert.Nil(t, body.Results[1].Method, "an absent removal carried a snapshot")
}

// TestMethodOps_ResultsResolveAddToExistingRow is the case client-side
// inference cannot get right. With a stored phone whose trigger-normalized key
// equals a differently-spelled submitted value, the add resolves to the
// existing row — and the result must carry THAT row's id and stored value, not
// the submitted spelling.
func TestMethodOps_ResultsResolveAddToExistingRow(t *testing.T) {
	t.Parallel()
	f := newMethodOpsFx(t)
	// The trigger maps a bare 10-digit number to +1XXXXXXXXXX, so both
	// spellings share one normalized key.
	stored := f.seed("phone", "5550113456", true)

	w, body := f.post(addOp("phone", "+15550113456"))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	assert.Len(t, f.stored(), 1, "a differently-spelled add duplicated the row")
	require.Len(t, body.Results, 1)
	assert.Equal(t, "matched_existing", body.Results[0].Outcome)
	assert.Equal(t, stored.ID.String(), body.Results[0].MethodID,
		"the client was not told which row its add resolved to")
	require.NotNil(t, body.Results[0].Method)
	assert.Equal(t, stored.Value, body.Results[0].Method.Value,
		"the snapshot echoed the submitted value instead of the resolved row's stored value")
	assert.True(t, body.Results[0].Method.IsPrimary,
		"the snapshot did not report the resolved row's primary flag")
}

// --- Validation -------------------------------------------------------------

// TestMethodOps_BlankValueRejected pins the deliberate split from the create
// path. Create DROPS a blank entry; an explicit add or update must REJECT it.
// Dropping here would turn an unsatisfiable intent into a silent success.
func TestMethodOps_BlankValueRejected(t *testing.T) {
	t.Parallel()
	f := newMethodOpsFx(t)
	a := f.seed("email", uniqueEmail(), false)

	t.Run("add", func(t *testing.T) {
		w, _ := f.post(addOp("email", ""))
		assert.Equal(t, http.StatusBadRequest, w.Code)
	})
	t.Run("update", func(t *testing.T) {
		w, _ := f.post(updateOp(a.ID, "email", ""))
		assert.Equal(t, http.StatusBadRequest, w.Code)
		assert.Equal(t, a.Value, f.storedByID(a.ID).Value, "a blank update silently no-opped")
	})
}

func TestMethodOps_IrrelevantFieldsRejected(t *testing.T) {
	t.Parallel()
	f := newMethodOpsFx(t)
	a := f.seed("email", uniqueEmail(), false)

	// A remove carrying a value is malformed: the client learns immediately
	// rather than believing an ignored field took effect.
	w, _ := f.post(map[string]any{
		"op": "remove", "method_id": a.ID.String(), "value": "anything@example.test",
	})
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.NotNil(t, f.storedByID(a.ID))
}

// TestMethodOps_UpdateRejectsIsPrimary must include the FALSE case. The rule is
// that the field is forbidden on update, which is a presence test — a validator
// checking "is it true" passes the false case while doing nothing, so only the
// false case proves presence detection was implemented.
func TestMethodOps_UpdateRejectsIsPrimary(t *testing.T) {
	t.Parallel()
	f := newMethodOpsFx(t)
	a := f.seed("email", uniqueEmail(), false)

	for _, isPrimary := range []bool{true, false} {
		t.Run(fmt.Sprint(isPrimary), func(t *testing.T) {
			w, _ := f.post(map[string]any{
				"op": "update", "method_id": a.ID.String(),
				"type": "email", "value": uniqueEmail(), "is_primary": isPrimary,
			})
			assert.Equal(t, http.StatusBadRequest, w.Code,
				"is_primary is forbidden on update regardless of its value")
		})
	}
}

// TestMethodOps_OperationValueFormatValidated pins the format rules the
// CON-015 amendment assigns to the operations path.
func TestMethodOps_OperationValueFormatValidated(t *testing.T) {
	t.Parallel()

	longPhone := ""
	for i := 0; i < 60; i++ {
		longPhone += "1"
	}

	cases := []struct {
		name string
		op   map[string]any
	}{
		{"malformed email", addOp("email", "not-an-email")},
		{"over-length phone", addOp("phone", longPhone)},
		{"unknown type", map[string]any{"op": "add", "type": "carrier-pigeon", "value": "x"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newMethodOpsFx(t)
			w, _ := f.post(tc.op)
			assert.Equal(t, http.StatusBadRequest, w.Code)
			assert.Empty(t, f.stored(), "an invalid operation was applied")
		})
	}
}

// --- Boundaries -------------------------------------------------------------

func TestMethodOps_UnknownContactReturns404(t *testing.T) {
	t.Parallel()
	f := newMethodOpsFx(t)

	w, _ := f.postTo(uuid.NewString(), addOp("email", uniqueEmail()))
	assert.Equal(t, http.StatusNotFound, w.Code)
}

// TestMethodOps_EmptyOperationsIsNoOp pins that "nothing to do" is a success.
// A `required` tag on the operations slice would make this a 400.
func TestMethodOps_EmptyOperationsIsNoOp(t *testing.T) {
	t.Parallel()
	f := newMethodOpsFx(t)
	existing := f.seed("email", uniqueEmail(), true)

	w, body := f.post()
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Empty(t, body.Results)
	require.Len(t, body.Methods, 1)
	assert.Equal(t, existing.ID.String(), body.Methods[0].ID)
}

// --- C6: the trigger mirror -------------------------------------------------

// TestMethodOps_HandleResolvesThroughTriggerMirror pins THE MIRROR, not the
// database backstop.
//
// The trigger strips ALL leading '@' for handle types, while identity
// normalization strips exactly one. With discord:foo stored, a mirror that
// inherited the one-@ behavior folds "@@foo" to "@foo", concludes the value is
// new, INSERTs it, and only then does the trigger normalize it to "foo" — a
// unique violation surfacing as a 500 on a request that should have succeeded.
//
// A correct mirror folds "@@foo" to "foo", matches the stored row, and resolves
// the add to it. So the correct answer here is a 200 that does NOT duplicate
// the row, per CON-062[3] — not a rejection. There is no input for which a
// correct mirror produces a conflict against a single pre-existing row: a
// rejection needs two DIFFERENT stored values sharing one normalized key in one
// payload, which is the casing case in _ConflictingOperationsRejected.
//
// Because the mirror short-circuits before any statement runs, this test cannot
// reach the 23505 path, and pins the mirror alone.
func TestMethodOps_HandleResolvesThroughTriggerMirror(t *testing.T) {
	t.Parallel()
	f := newMethodOpsFx(t)
	handle := "h" + uuid.NewString()[:8]
	stored := f.seed("discord", handle, false)

	w, body := f.post(addOp("discord", "@@"+handle))
	require.Equal(t, http.StatusOK, w.Code,
		"a handle whose normalized key already exists reached the database instead of being folded")
	assert.Len(t, f.stored(), 1, "the add duplicated a row the trigger normalizes identically")

	require.Len(t, body.Results, 1)
	assert.Equal(t, "matched_existing", body.Results[0].Outcome)
	assert.Equal(t, stored.ID.String(), body.Results[0].MethodID,
		"the add did not resolve to the row the trigger considers identical")
}

// --- The handler's domain-error translation ---------------------------------

// stubApplier returns a fixed error, so the handler's error mapping can be
// exercised for a domain error a correct fold makes unreachable from a real
// request. This is the seam the handler's narrow interface exists for.
type stubApplier struct{ err error }

func (s stubApplier) ApplyOperations(context.Context, uuid.UUID, []service.ContactMethodOperation) (*service.ApplyContactMethodsResult, error) {
	return nil, s.err
}

// TestMethodOps_ConflictErrorTranslatesTo400 proves HTTP MAPPING only — not
// repository classification (covered at the service level) and not mid-apply
// rollback (an acknowledged gap). A correct mirror rejects trigger collisions
// before SQL, so nothing else can reach this branch.
func TestMethodOps_ConflictErrorTranslatesTo400(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		err  error
		want int
	}{
		{"value conflict", fmt.Errorf("wrapped: %w", repository.ErrMethodValueConflict), http.StatusBadRequest},
		{"invalid operations", fmt.Errorf("wrapped: %w", service.ErrInvalidOperations), http.StatusBadRequest},
		{"foreign method id", fmt.Errorf("wrapped: %w", service.ErrMethodNotOwned), http.StatusNotFound},
		{"unknown contact", fmt.Errorf("wrapped: %w", db.ErrNotFound), http.StatusNotFound},
		{"unexpected failure", errors.New("boom"), http.StatusInternalServerError},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			router := newMethodOpsRouter(stubApplier{err: tc.err})

			body, err := json.Marshal(map[string]any{
				"operations": []map[string]any{addOp("email", "someone@example.test")},
			})
			require.NoError(t, err)
			req := httptest.NewRequest(http.MethodPost,
				"/api/v1/contacts/"+uuid.NewString()+"/methods", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			router.ServeHTTP(w, req)

			assert.Equal(t, tc.want, w.Code)
			if tc.want == http.StatusInternalServerError {
				assert.NotContains(t, w.Body.String(), "boom",
					"the raw error leaked into the response body")
			}
		})
	}
}

// --- The route surface ------------------------------------------------------

// TestMethodRoutes_NoDesiredSetPut asserts PUT /contacts/:id/methods is NOT
// registered. A PUT taking the full desired list is wholesale replace wearing a
// sub-resource costume: absence would again imply deletion, reintroducing the
// exact defect this endpoint exists to eliminate.
//
// CON-062[0] tests POST semantics and would stay green alongside a new PUT, so
// a guarantee that depends on a route never being added is asserted
// mechanically rather than left in prose.
func TestMethodRoutes_NoDesiredSetPut(t *testing.T) {
	t.Parallel()

	router := newMethodOpsRouterFull()
	req := httptest.NewRequest(http.MethodPut,
		"/api/v1/contacts/"+uuid.NewString()+"/methods", bytes.NewReader([]byte(`{"methods":[]}`)))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Contains(t, []int{http.StatusNotFound, http.StatusMethodNotAllowed}, w.Code,
		"PUT /contacts/:id/methods is registered; a desired-set endpoint reintroduces the lost-update defect")
}

// newMethodOpsRouterFull registers the whole contact route surface through the
// production registrar, so the negative assertion above is made against the
// routes the binary actually serves rather than a hand-picked subset.
func newMethodOpsRouterFull() *gin.Engine {
	router := gin.New()
	router.HandleMethodNotAllowed = true
	v1 := router.Group("/api/v1")
	handlers.RegisterContactRoutes(v1, handlers.ContactRouteDeps{
		Contact:       &handlers.ContactHandler{},
		Interaction:   &handlers.InteractionHandler{},
		Note:          &handlers.NoteHandler{},
		ContactMethod: handlers.NewContactMethodHandler(stubApplier{err: errors.New("unused")}),
	})
	return router
}
