//go:build integration_testdb

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/api/handlers"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"
	"personal-crm/backend/internal/synthetic/declare"
	"personal-crm/backend/internal/synthetic/factory"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The declared-seeding endpoints asserted at the HTTP tier — these are the new
// PUBLIC contracts, and a test that drove declare.Run directly would prove
// nothing about status codes, response shapes, or the request validation the
// handler owns.
//
// Isolation: an isolated per-test clone, no t.Parallel(). Seeding starts a live
// River client, and namespace scoping does not isolate river_job CONSUMPTION.

func declaredAPINS(t *testing.T) string {
	t.Helper()
	return "a" + uuid.NewString()[:8]
}

// newDeclaredSeedRouter builds a router carrying the production test-route
// surface, including the BESPOKE seed service — the dual-shape cleanup test
// needs the legacy prefix branch to be genuinely wired, not stubbed.
func newDeclaredSeedRouter(t *testing.T) (*gin.Engine, *db.Database, context.Context) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	ctx := context.Background()
	database, _ := newIsolatedRiverTestDB(t, ctx)

	contactRepo := repository.NewContactRepository(database.Queries)
	contactRepo.SetPool(database.Pool)
	methodRepo := repository.NewContactMethodRepository(database.Queries)
	interactionRepo := repository.NewInteractionRepository(database.Queries)
	cadenceUpdater := buildCadenceUpdaterForAPITest(t, database)
	assertSvc, cache := buildKnowledgeDepsForAPITest(t, database, nil)
	contactService := service.NewContactService(
		database, contactRepo, methodRepo, interactionRepo,
		repository.NewContactTaskRepository(database.Queries), nil, nil,
		cadenceUpdater, assertSvc, cache, nil,
	)

	seedSvc := service.NewTestSeedService(
		database,
		repository.NewExternalContactRepository(database.Queries),
		contactService,
		repository.NewCalendarEventRepository(database.Queries),
		repository.NewMacHostRepository(database.Queries),
		repository.NewMeetingNoteRepository(database.Queries),
	)
	handler := handlers.NewTestHandler(seedSvc, service.NewTestLockService(accelerated.GetCurrentTime), database)

	router := gin.New()
	v1 := router.Group("/api/v1")
	handlers.RegisterTestRoutes(v1, handler)
	return router, database, ctx
}

func postDeclared(t *testing.T, router *gin.Engine, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	payload, err := json.Marshal(body)
	require.NoError(t, err)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(w, req)
	return w
}

func decodeDeclaredSeed(t *testing.T, w *httptest.ResponseRecorder) handlers.SeedDeclaredResponse {
	t.Helper()
	var envelope struct {
		Success bool                          `json:"success"`
		Data    handlers.SeedDeclaredResponse `json:"data"`
		Error   map[string]any                `json:"error"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &envelope), "body: %s", w.Body.String())
	require.True(t, envelope.Success, "body: %s", w.Body.String())
	return envelope.Data
}

func decodeNamespaceCleanup(t *testing.T, w *httptest.ResponseRecorder) handlers.CleanupNamespacesResponse {
	t.Helper()
	var envelope struct {
		Success bool                               `json:"success"`
		Data    handlers.CleanupNamespacesResponse `json:"data"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &envelope), "body: %s", w.Body.String())
	require.True(t, envelope.Success, "body: %s", w.Body.String())
	return envelope.Data
}

func cleanupNamespaces(t *testing.T, router *gin.Engine, namespaces []string, seed uint64) handlers.CleanupNamespacesResponse {
	t.Helper()
	body := map[string]any{"namespaces": namespaces}
	if seed != 0 {
		body["seed"] = seed
	}
	w := postDeclared(t, router, "/api/v1/test/cleanup", body)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	return decodeNamespaceCleanup(t, w)
}

// A 201 must carry a manifest whose every id and name is REAL: a client keys
// its selectors off these values, so a manifest that did not match the stored
// rows would produce tests that look green while asserting nothing.
func TestSeedDeclaredEndpoint_CreatesManifest(t *testing.T) {
	router, database, ctx := newDeclaredSeedRouter(t)
	namespace := declaredAPINS(t)

	w := postDeclared(t, router, "/api/v1/test/seed/declared", map[string]any{
		"behavior_id": "CAD-026", "namespace": namespace,
	})
	require.Equal(t, http.StatusCreated, w.Code, "body: %s", w.Body.String())

	manifest := decodeDeclaredSeed(t, w)
	assert.Equal(t, namespace, manifest.Namespace)
	assert.False(t, manifest.Anchor.IsZero())
	require.Len(t, manifest.Entities, 3)

	contactRepo := repository.NewContactRepository(database.Queries)
	for handle, entity := range manifest.Entities {
		assert.Equal(t, "contact", entity.Kind, "handle %q", handle)
		id, err := uuid.Parse(entity.ID)
		require.NoError(t, err, "handle %q id is not a uuid", handle)
		stored, err := contactRepo.GetContact(ctx, id)
		require.NoError(t, err, "handle %q names a contact that does not exist", handle)
		assert.Equal(t, entity.Name, stored.FullName, "handle %q name does not match the stored row", handle)
	}

	cleanupNamespaces(t, router, []string{namespace}, 0)
}

// The response echoes the EFFECTIVE namespace, which may carry a -sN suffix the
// caller never asked for. A client asserting against its own request string
// would silently target the wrong world.
func TestSeedDeclaredEndpoint_EffectiveNamespace(t *testing.T) {
	router, database, ctx := newDeclaredSeedRouter(t)
	support := repository.NewSyntheticSupportRepository(database.Queries)

	namespace := declaredAPINS(t)
	gen := factory.NewGenerator(factory.DefaultSeed, namespace)
	require.NoError(t, support.InsertTelegramChatConfigInBand(ctx, gen.PeerBandStart()))

	w := postDeclared(t, router, "/api/v1/test/seed/declared", map[string]any{
		"behavior_id": "DSH-005", "namespace": namespace,
	})
	require.Equal(t, http.StatusCreated, w.Code, "body: %s", w.Body.String())

	manifest := decodeDeclaredSeed(t, w)
	assert.NotEqual(t, namespace, manifest.Namespace, "the occupied band must have forced a re-salt")
	assert.Contains(t, manifest.Namespace, namespace+"-s")

	// The REQUESTED token still reaches the re-salted world.
	res := cleanupNamespaces(t, router, []string{namespace}, 0)
	assert.Contains(t, res.Expansions[namespace], manifest.Namespace)
	assert.Equal(t, declare.StatusCleaned, res.Results[manifest.Namespace].Status)

	_, _ = support.DeleteTelegramChatConfigsByChatIds(ctx, []int64{gen.PeerBandStart()})
}

// The seed and cleanup grammars have to MEET at the boundary. A requested
// namespace right at the accepted limit can still be re-salted — the effective
// token grows by the suffix and is never revalidated — and that longer token is
// what the client hands straight back to cleanup. If the requested limit does
// not reserve room for the suffix, this round trip seeds a world whose own
// cleanup request is rejected outright, leaving rows in the shared E2E database.
//
// The longest accepted requested namespace is DERIVED from the validator rather
// than read off a constant, so the test also holds the handler's own max= bound
// to it: a bound tighter than the package's rejects the seed, a looser one lets
// the over-long effective token through to the cleanup call below.
func TestSeedDeclaredEndpoint_MaxLengthNamespaceResalts(t *testing.T) {
	router, database, ctx := newDeclaredSeedRouter(t)
	support := repository.NewSyntheticSupportRepository(database.Queries)

	namespace := longestAcceptedNamespace(t, declaredAPINS(t))
	gen := factory.NewGenerator(factory.DefaultSeed, namespace)
	require.NoError(t, support.InsertTelegramChatConfigInBand(ctx, gen.PeerBandStart()))
	t.Cleanup(func() {
		_, _ = support.DeleteTelegramChatConfigsByChatIds(context.Background(), []int64{gen.PeerBandStart()})
	})

	w := postDeclared(t, router, "/api/v1/test/seed/declared", map[string]any{
		"behavior_id": "DSH-005", "namespace": namespace,
	})
	require.Equal(t, http.StatusCreated, w.Code, "body: %s", w.Body.String())

	manifest := decodeDeclaredSeed(t, w)
	require.NotEqual(t, namespace, manifest.Namespace, "the occupied band must have forced a re-salt")

	// Cleanup by the EFFECTIVE token, which is what a client that received the
	// manifest sends. cleanupNamespaces requires a 200, so an effective token
	// the cleanup grammar rejects fails here.
	res := cleanupNamespaces(t, router, []string{manifest.Namespace}, 0)
	assert.Equal(t, declare.StatusCleaned, res.Results[manifest.Namespace].Status)
}

// longestAcceptedNamespace pads a unique base to the longest string
// ValidateRequestedNamespace still accepts.
func longestAcceptedNamespace(t *testing.T, base string) string {
	t.Helper()
	longest := ""
	for candidate := base; len(candidate) <= 128; candidate += "a" {
		if declare.ValidateRequestedNamespace(candidate) == nil {
			longest = candidate
		}
	}
	require.NotEmpty(t, longest, "no accepted requested namespace found from base %q", base)
	return longest
}

func TestSeedDeclaredEndpoint_Conflict409(t *testing.T) {
	router, _, _ := newDeclaredSeedRouter(t)
	namespace := declaredAPINS(t)

	first := postDeclared(t, router, "/api/v1/test/seed/declared", map[string]any{
		"behavior_id": "DSH-005", "namespace": namespace,
	})
	require.Equal(t, http.StatusCreated, first.Code, "body: %s", first.Body.String())

	t.Run("occupied namespace", func(t *testing.T) {
		w := postDeclared(t, router, "/api/v1/test/seed/declared", map[string]any{
			"behavior_id": "DSH-005", "namespace": namespace,
		})
		assert.Equal(t, http.StatusConflict, w.Code, "body: %s", w.Body.String())
	})

	t.Run("nested under a live namespace", func(t *testing.T) {
		w := postDeclared(t, router, "/api/v1/test/seed/declared", map[string]any{
			"behavior_id": "DSH-005", "namespace": namespace + "-child",
		})
		assert.Equal(t, http.StatusConflict, w.Code, "body: %s", w.Body.String())
	})

	// The reserved salt grammar is a REQUEST error, not a conflict: it can never
	// succeed on retry.
	t.Run("reserved salt suffix", func(t *testing.T) {
		w := postDeclared(t, router, "/api/v1/test/seed/declared", map[string]any{
			"behavior_id": "DSH-005", "namespace": declaredAPINS(t) + "-s1",
		})
		assert.Equal(t, http.StatusBadRequest, w.Code, "body: %s", w.Body.String())
	})

	cleanupNamespaces(t, router, []string{namespace}, 0)
}

// A 500 must carry enough for a client to recover: which namespace, and whether
// anything was left behind. The failpoint exists precisely so a VALID request
// can be made to fail at the HTTP tier.
// inRequestedFamily reports whether `effective` is a namespace the REQUESTED
// token can still address: the token itself, or the token plus the `-sN` re-salt
// suffix saltVariants mints, and nothing else.
//
// Anchored on purpose. A prefix test would also accept "<token>x" and
// "<token>-child", which are DIFFERENT namespaces — a client handed one of those
// as its recovery token cannot address the world it just seeded, which is the
// contract this test exists to verify.
//
// The suffix INDEX is deliberately left unbounded. Cleanup does NOT discover
// family members: expandNamespace probes exactly "-s1".."-s<maxSaltAttempt>" for
// a host marker, so an out-of-range variant would be unreachable from the
// requested token. What makes the bound safe to omit here is that minting and
// probing read the SAME constant — saltVariants and expandNamespace both loop to
// maxSaltAttempt, declared once — so every variant the toolkit can mint is one
// expansion probes, by construction. An out-of-range "-sN" can therefore only be
// a corrupted namespace, and grammar cannot tell a corrupt "-s8" from a
// legitimate one without restating a production constant this test cannot see,
// which is the parallel-list defect rather than coverage.
func inRequestedFamily(effective, requested string) bool {
	if effective == requested {
		return true
	}
	index, ok := strings.CutPrefix(effective, requested+"-s")
	if !ok || index == "" {
		return false
	}
	for _, digit := range index {
		if digit < '0' || digit > '9' {
			return false
		}
	}
	return true
}

func TestSeedDeclaredEndpoint_FailureCarriesRecoveryMetadata(t *testing.T) {
	router, database, ctx := newDeclaredSeedRouter(t)
	namespace := declaredAPINS(t)

	restore := declare.SetFailpointForTest(declare.FailpointAfterFirstEntity)
	w := postDeclared(t, router, "/api/v1/test/seed/declared", map[string]any{
		"behavior_id": "CAD-026", "namespace": namespace,
	})
	restore()

	require.Equal(t, http.StatusInternalServerError, w.Code, "body: %s", w.Body.String())
	var envelope struct {
		Success bool                         `json:"success"`
		Data    handlers.SeedDeclaredFailure `json:"data"`
		Error   map[string]any               `json:"error"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &envelope))
	assert.False(t, envelope.Success)
	assert.NotEmpty(t, envelope.Error)
	// The EFFECTIVE namespace, which may legitimately carry a -sN re-salt suffix
	// the caller never asked for. Asserting equality would reject a correct
	// response; what the contract promises is that the reported namespace belongs
	// to the requested token's family, which is what makes it cleanable.
	effective := envelope.Data.Namespace
	assert.True(t, inRequestedFamily(effective, namespace),
		"the reported namespace %q must be the requested token %q or one of its -sN salt variants", effective, namespace)
	// The other half of the advertised contract. A failpoint fires after the
	// FIRST entity, so the run's own failure teardown ran and returned cleanly —
	// the endpoint must SAY so. Without this the handler could hardcode false, or
	// omit the field entirely, and the later cleanup would still pass.
	assert.True(t, envelope.Data.Cleaned, "the failure body must report that the partial world was cleaned")

	// And `cleaned` must be TRUE OF THE DATABASE, not merely of a function that
	// returned nil. The flag is derived from the teardown's error return, so a
	// teardown that silently deleted nothing would still advertise cleaned=true
	// and leave the partial world standing. Read the namespace's own rows back.
	support := repository.NewSyntheticSupportRepository(database.Queries)
	prefix := factory.NewGenerator(factory.DefaultSeed, effective).Prefix()
	remaining, err := support.SelectContactIDsByFullNamePrefix(ctx, prefix)
	require.NoError(t, err)
	assert.Empty(t, remaining, "cleaned=true must mean the partial world is GONE, not that teardown returned nil")

	// Contacts are only ONE residue class. The harness teardown independently
	// owns rows no contact-keyed read can see — the namespaced mac_host marker,
	// the meeting_notes hung off it, the namespace ownership records and the
	// harness's PRIVATE river_queue row — and a teardown that dropped the
	// contacts but leaked any of those still returns nil, so `cleaned` still
	// reports true.
	//
	// The cross-request cleanup ladder deletes every one of those classes and
	// reports its per-class counts, so running it as the fallback and requiring
	// EVERY count to be zero says "the harness left nothing for me to find" over
	// the whole class list at once — including classes added later, which a
	// hand-written list here would silently stop covering. It is deliberately
	// paired with the contact read above rather than replacing it: this check is
	// comprehensive but trusts the ladder's own accounting, while that one is
	// narrow but reads the table directly.
	// Checked over EVERY result the cleanup returns rather than one looked up by
	// name. Keying the lookup on a single name is what makes this check fragile:
	// the request names one token, and the response is keyed by whichever
	// namespace the sweep actually resolved — the EFFECTIVE name when residue is
	// still discoverable, the REQUESTED token when the family is already empty
	// (which is this test's own happy path after a re-salt). A lookup that missed
	// would yield a zero-value result whose Deleted map is empty, and the loop
	// would then iterate NOTHING and pass. Requiring at least one result, and a
	// non-empty class map inside each, is what stops this from silently becoming
	// no check at all.
	res := cleanupNamespaces(t, router, []string{namespace}, 0)
	require.NotEmpty(t, res.Results, "cleanup reported no results at all for %q", namespace)
	for ns, result := range res.Results {
		assert.True(t, inRequestedFamily(ns, namespace),
			"cleanup reported namespace %q, which is outside the requested token %q family", ns, namespace)
		assert.Equal(t, declare.StatusCleaned, result.Status, "namespace %s", ns)
		require.NotEmpty(t, result.Deleted,
			"namespace %s must report per-class counts, or the residue check below checks nothing", ns)
		for class, n := range result.Deleted {
			assert.Zero(t, n,
				"the failure teardown reported cleaned=true but left %d %s row(s) in %s for the fallback sweep", n, class, ns)
		}
	}
}

func TestCleanupEndpoint_DualShape(t *testing.T) {
	router, _, _ := newDeclaredSeedRouter(t)

	t.Run("legacy prefix shape is unchanged", func(t *testing.T) {
		seeded := postDeclared(t, router, "/api/v1/test/seed/contacts", map[string]any{
			"prefix":   "legacyshape",
			"contacts": []map[string]any{{"full_name": "legacyshape-Someone"}},
		})
		require.Equal(t, http.StatusCreated, seeded.Code, "body: %s", seeded.Body.String())

		w := postDeclared(t, router, "/api/v1/test/cleanup", map[string]any{"prefix": "legacyshape"})
		require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
		var envelope struct {
			Success bool                     `json:"success"`
			Data    handlers.CleanupResponse `json:"data"`
		}
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &envelope))
		assert.True(t, envelope.Success)
		assert.GreaterOrEqual(t, envelope.Data.DeletedContacts, int64(1),
			"the prefix branch must still report its per-class counts")
	})

	t.Run("declared shape reports per-namespace outcomes", func(t *testing.T) {
		namespace := declaredAPINS(t)
		seeded := postDeclared(t, router, "/api/v1/test/seed/declared", map[string]any{
			"behavior_id": "DSH-005", "namespace": namespace,
		})
		require.Equal(t, http.StatusCreated, seeded.Code, "body: %s", seeded.Body.String())

		res := cleanupNamespaces(t, router, []string{namespace}, 0)
		outcome := res.Results[namespace]
		require.Equal(t, declare.StatusCleaned, outcome.Status)
		assert.GreaterOrEqual(t, outcome.Deleted["contacts"], int64(2))
		assert.Equal(t, []string{namespace}, res.Expansions[namespace])
	})

	// host_id belongs to the prefix shape. The declared branch has no host to
	// scope meeting-note deletion to, so a request carrying both used to validate
	// cleanly, take the declared branch, and answer 200 — telling the caller its
	// cleanup succeeded while the host's meeting notes were never touched. A
	// combination whose work cannot be done must be refused, not half-honoured.
	t.Run("rejects host_id alongside namespaces", func(t *testing.T) {
		host := postDeclared(t, router, "/api/v1/test/seed/mac-hosts", map[string]any{
			"hostname": "declared-shape-host-" + declaredAPINS(t),
		})
		require.Equal(t, http.StatusOK, host.Code, "body: %s", host.Body.String())
		var hostEnvelope struct {
			Data handlers.SeedMacHostResponse `json:"data"`
		}
		require.NoError(t, json.Unmarshal(host.Body.Bytes(), &hostEnvelope))
		require.NotEmpty(t, hostEnvelope.Data.HostID)

		w := postDeclared(t, router, "/api/v1/test/cleanup", map[string]any{
			"namespaces": []string{declaredAPINS(t)},
			"host_id":    hostEnvelope.Data.HostID,
		})
		require.Equal(t, http.StatusBadRequest, w.Code, "body: %s", w.Body.String())
		assert.Contains(t, w.Body.String(), "host_id")
	})

	t.Run("rejects an ambiguous or empty request", func(t *testing.T) {
		cases := []map[string]any{
			{}, // neither
			{"prefix": "x", "namespaces": []string{"y"}}, // both
			{"namespaces": []string{"dup", "dup"}},       // duplicates
			{"namespaces": make([]string, 33)},           // oversize
			{"namespaces": []string{"BAD_CHARSET"}},      // charset
		}
		for i, body := range cases {
			w := postDeclared(t, router, "/api/v1/test/cleanup", body)
			assert.Equal(t, http.StatusBadRequest, w.Code, "case %d body: %s", i, w.Body.String())
		}
	})
}

// The seed parameter must round-trip: cleanup rebuilds the generator from
// (seed, namespace), so cleaning with a different seed would derive the wrong
// numeric-band tokens and leave rows behind.
func TestCleanupEndpoint_CustomSeed(t *testing.T) {
	router, database, ctx := newDeclaredSeedRouter(t)
	const customSeed = uint64(987654321)
	namespace := declaredAPINS(t)

	w := postDeclared(t, router, "/api/v1/test/seed/declared", map[string]any{
		"behavior_id": "DSH-005", "namespace": namespace, "seed": customSeed,
	})
	require.Equal(t, http.StatusCreated, w.Code, "body: %s", w.Body.String())
	manifest := decodeDeclaredSeed(t, w)

	support := repository.NewSyntheticSupportRepository(database.Queries)
	prefix := factory.SyntheticSourcePrefix + manifest.Namespace + "-"
	before, err := support.SelectContactIDsByFullNamePrefix(ctx, prefix)
	require.NoError(t, err)
	require.Len(t, before, 2)

	res := cleanupNamespaces(t, router, []string{namespace}, customSeed)
	require.Equal(t, declare.StatusCleaned, res.Results[manifest.Namespace].Status)

	after, err := support.SelectContactIDsByFullNamePrefix(ctx, prefix)
	require.NoError(t, err)
	assert.Empty(t, after)
}
