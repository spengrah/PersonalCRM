//go:build integration_testdb

// Package api's identity_routes_test.go is the first handler-level coverage
// of IdentityHandler over HTTP. Before this file, backend/tests/identity_integration_test.go
// drove the repository and service directly, so a rewrite of a handler call
// site to a wrong-but-signature-compatible repository method (e.g.
// identityRepo.GetByID standing in for identityRepo.UnlinkFromContact) would
// have shipped green. This suite exercises the five registered identity
// routes that PR1-E1 rewrote from service calls to repository calls, through
// the real router, asserting status plus a discriminating field of the
// response shape rather than just 200.
package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"personal-crm/backend/internal/api/handlers"
	"personal-crm/backend/internal/identity"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newIdentityRoutesTest builds a fresh, empty isolated DB clone, wires the
// production identity route surface (RegisterIdentityRoutes) over the real
// IdentityHandler -> repository.IdentityRepository stack, and returns the
// pieces subtests need to seed state and assert against the repository
// directly. Every subtest MUST call this as its first line: ListUnmatchedIdentities
// counts contact_id IS NULL rows DB-wide with no source/account filter, so a
// shared clone would let one subtest's seeded rows leak into a sibling's
// exact-count assertion.
func newIdentityRoutesTest(t *testing.T) (router *gin.Engine, identityRepo *repository.IdentityRepository, contactRepo *repository.ContactRepository, ctx context.Context) {
	t.Helper()
	ctx = context.Background()
	database, _ := newIsolatedRiverTestDB(t, ctx)

	identityRepo = repository.NewIdentityRepository(database.Queries)
	contactRepo = repository.NewContactRepository(database.Queries)
	identityService := service.NewIdentityService(identityRepo)
	handler := handlers.NewIdentityHandler(identityService, identityRepo)

	router = gin.New()
	v1 := router.Group("/api/v1")
	handlers.RegisterIdentityRoutes(v1, handler)

	return router, identityRepo, contactRepo, ctx
}

// doIdentityRequest serves a bodyless HTTP request against the router and
// returns the recorder.
func doIdentityRequest(router *gin.Engine, method, path string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, nil)
	router.ServeHTTP(w, req)
	return w
}

// identityResponse mirrors the JSON fields of repository.ExternalIdentity
// this suite asserts on.
type identityResponse struct {
	ID        string  `json:"id"`
	ContactID *string `json:"contact_id"`
}

// identityEnvelope unwraps api.APIResponse around a single identity.
type identityEnvelope struct {
	Success bool             `json:"success"`
	Data    identityResponse `json:"data"`
}

// identityListEnvelope unwraps api.APIResponse around a list of identities
// plus meta.pagination.
type identityListEnvelope struct {
	Success bool               `json:"success"`
	Data    []identityResponse `json:"data"`
	Meta    struct {
		Pagination struct {
			Total int64 `json:"total"`
		} `json:"pagination"`
	} `json:"meta"`
}

// seedIdentity upserts an external identity for the given contact (nil for
// unmatched) with a per-call-unique identifier so parallel subtests never
// collide on the (identifier, identifier_type, source) unique constraint.
func seedIdentity(t *testing.T, ctx context.Context, identityRepo *repository.IdentityRepository, contactID *uuid.UUID) *repository.ExternalIdentity {
	t.Helper()
	req := repository.UpsertIdentityRequest{
		Identifier:     "identity-" + uuid.NewString(),
		IdentifierType: identity.IdentifierTypeEmail,
		Source:         "gmail",
		ContactID:      contactID,
	}
	if contactID != nil {
		req.MatchType = repository.MatchTypeManual
	}
	ident, err := identityRepo.Upsert(ctx, req)
	require.NoError(t, err)
	return ident
}

// TestIdentityRoutes covers the five identity routes PR1-E1 rewrote from
// IdentityService calls to IdentityRepository calls. POST /identities/:id/link
// is excluded: it reaches IdentityService.LinkIdentity, a near-pass-through
// that this element does not touch.
func TestIdentityRoutes(t *testing.T) {
	t.Parallel()

	t.Run("unmatched list and count", func(t *testing.T) {
		router, identityRepo, contactRepo, ctx := newIdentityRoutesTest(t)

		unmatched := seedIdentity(t, ctx, identityRepo, nil)

		contact, err := contactRepo.CreateContact(ctx, repository.CreateContactRequest{FullName: "Linked Contact"})
		require.NoError(t, err)
		linked := seedIdentity(t, ctx, identityRepo, &contact.ID)

		w := doIdentityRequest(router, http.MethodGet, "/api/v1/identities/unmatched")
		require.Equal(t, http.StatusOK, w.Code)

		var env identityListEnvelope
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &env))

		ids := make([]string, 0, len(env.Data))
		for _, d := range env.Data {
			ids = append(ids, d.ID)
		}
		assert.Contains(t, ids, unmatched.ID.String())
		assert.NotContains(t, ids, linked.ID.String())
		// This is the only coverage CountUnmatched gets, so a rebind of
		// identityRepo.CountUnmatched shows up here or nowhere.
		assert.EqualValues(t, 1, env.Meta.Pagination.Total)
	})

	t.Run("get by id", func(t *testing.T) {
		router, identityRepo, _, ctx := newIdentityRoutesTest(t)

		seeded := seedIdentity(t, ctx, identityRepo, nil)

		w := doIdentityRequest(router, http.MethodGet, "/api/v1/identities/"+seeded.ID.String())
		require.Equal(t, http.StatusOK, w.Code)

		var env identityEnvelope
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &env))
		assert.Equal(t, seeded.ID.String(), env.Data.ID)

		w = doIdentityRequest(router, http.MethodGet, "/api/v1/identities/"+uuid.NewString())
		assert.Equal(t, http.StatusNotFound, w.Code)
	})

	t.Run("delete", func(t *testing.T) {
		router, identityRepo, _, ctx := newIdentityRoutesTest(t)

		seeded := seedIdentity(t, ctx, identityRepo, nil)

		w := doIdentityRequest(router, http.MethodDelete, "/api/v1/identities/"+seeded.ID.String())
		require.Equal(t, http.StatusNoContent, w.Code)
		assert.Empty(t, w.Body.Bytes())

		w = doIdentityRequest(router, http.MethodGet, "/api/v1/identities/"+seeded.ID.String())
		assert.Equal(t, http.StatusNotFound, w.Code)
	})

	t.Run("unlink", func(t *testing.T) {
		router, identityRepo, contactRepo, ctx := newIdentityRoutesTest(t)

		contact, err := contactRepo.CreateContact(ctx, repository.CreateContactRequest{FullName: "Unlink Contact"})
		require.NoError(t, err)
		seeded := seedIdentity(t, ctx, identityRepo, &contact.ID)

		w := doIdentityRequest(router, http.MethodPost, "/api/v1/identities/"+seeded.ID.String()+"/unlink")
		require.Equal(t, http.StatusOK, w.Code)

		var env identityEnvelope
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &env))
		assert.Equal(t, seeded.ID.String(), env.Data.ID)

		// The row survives with its link cleared — this is what distinguishes
		// UnlinkFromContact from every signature-compatible neighbour.
		w = doIdentityRequest(router, http.MethodGet, "/api/v1/identities/"+seeded.ID.String())
		require.Equal(t, http.StatusOK, w.Code)
		var getEnv identityEnvelope
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &getEnv))
		assert.Nil(t, getEnv.Data.ContactID)
	})

	t.Run("identities for contact", func(t *testing.T) {
		router, identityRepo, contactRepo, ctx := newIdentityRoutesTest(t)

		contactA, err := contactRepo.CreateContact(ctx, repository.CreateContactRequest{FullName: "Contact A"})
		require.NoError(t, err)
		contactB, err := contactRepo.CreateContact(ctx, repository.CreateContactRequest{FullName: "Contact B"})
		require.NoError(t, err)

		identA := seedIdentity(t, ctx, identityRepo, &contactA.ID)
		identB := seedIdentity(t, ctx, identityRepo, &contactB.ID)

		w := doIdentityRequest(router, http.MethodGet, "/api/v1/contacts/"+contactA.ID.String()+"/identities")
		require.Equal(t, http.StatusOK, w.Code)

		var env identityListEnvelope
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &env))

		ids := make([]string, 0, len(env.Data))
		for _, d := range env.Data {
			ids = append(ids, d.ID)
		}
		assert.Contains(t, ids, identA.ID.String())
		assert.NotContains(t, ids, identB.ID.String())
	})
}
