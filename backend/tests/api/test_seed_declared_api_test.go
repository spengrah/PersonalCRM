//go:build integration_testdb

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
func TestSeedDeclaredEndpoint_FailureCarriesRecoveryMetadata(t *testing.T) {
	router, _, _ := newDeclaredSeedRouter(t)
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
	assert.Equal(t, namespace, envelope.Data.Namespace)
	// The other half of the advertised contract. A failpoint fires after the
	// FIRST entity, so the run's own failure teardown ran and returned cleanly —
	// the endpoint must SAY so. Without this the handler could hardcode false, or
	// omit the field entirely, and the later cleanup would still pass.
	assert.True(t, envelope.Data.Cleaned, "the failure body must report that the partial world was cleaned")

	// The namespace is reclaimable regardless of which way `cleaned` reported.
	res := cleanupNamespaces(t, router, []string{namespace}, 0)
	assert.Equal(t, declare.StatusCleaned, res.Results[namespace].Status)
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
