//go:build integration_testdb

package tests

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"personal-crm/backend/internal/api/handlers"
	"personal-crm/backend/internal/synthetic/factory"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newRematchRouter builds a Gin router carrying only the production rematch
// route surface, wired to the env's live RematchService + ContactService. It
// reuses the same RematchService instance the publisher registers pending jobs
// on (setupRematchEnv guarantees the registry ← bus ← contactSvc wiring), so a
// job minted by Rescan is immediately visible through GetJob.
func newRematchRouter(env *rematchTestEnv) *gin.Engine {
	// Gin mode is set once in TestMain; setting it here would race the
	// unsynchronized ginMode global across the two t.Parallel() tests.
	router := gin.New()
	v1 := router.Group("/api/v1")
	handlers.RegisterRematchRoutes(v1, handlers.NewRematchHandler(env.rematchSvc, env.contactSvc))
	return router
}

// rescanResponse mirrors handlers.RescanResponse for JSON unwrapping.
type rescanResponse struct {
	RematchJobID *string `json:"rematch_job_id"`
}

// rematchJobResponse mirrors handlers.RematchJobResponse (only the fields the
// tests assert on).
type rematchJobResponse struct {
	ID        string `json:"id"`
	ContactID string `json:"contact_id"`
	Status    string `json:"status"`
	// Pointer so a dropped "matched" key (API stops reporting the count)
	// is detectable as nil. The value itself is legitimately 0 at immediate
	// poll time — the async dispatcher has not run yet (D3) — so we assert
	// presence, not a specific count.
	Matched *int `json:"matched"`
	Methods []struct {
		Type  string `json:"type"`
		Value string `json:"value"`
	} `json:"methods"`
}

// doRequest serves an HTTP request against the router and returns the recorder.
func doRequest(router *gin.Engine, method, path string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, nil)
	router.ServeHTTP(w, req)
	return w
}

// TestRematchAPI_PollableRescan proves the synchronous, immediately-pollable
// contract of the rescan + job endpoints: a rescan over eligible methods mints
// a job visible the moment it returns, and a rescan with no eligible methods
// returns a null job id.
func TestRematchAPI_PollableRescan(t *testing.T) {
	t.Parallel()
	env := setupRematchEnv(t)
	router := newRematchRouter(env)

	t.Run("Rescan_WithEligibleMethod_ReturnsPollableJob", func(t *testing.T) {
		// spec: IMP-021
		// Seed a contact WITH an email method — email is the only eligible
		// (registered) rematch method type in the base env.
		contact, cleanup := seedMigrationContact(env.ctx, t, env.database, env.gen, factory.WithEmail())
		t.Cleanup(cleanup)

		methods, err := env.contactMethodRepo.ListContactMethodsByContact(env.ctx, contact.ID)
		require.NoError(t, err)
		require.Len(t, methods, 1)
		require.Equal(t, "email", methods[0].Type)
		// RescanRematch dispatches over the NORMALIZED method value.
		wantEmail := methods[0].ValueNormalized

		// POST rescan → 200 with a non-null rematch_job_id.
		w := doRequest(router, http.MethodPost, "/api/v1/rematch/contacts/"+contact.ID.String()+"/rescan")
		require.Equal(t, http.StatusOK, w.Code)

		var rescanEnvelope struct {
			Success bool           `json:"success"`
			Data    rescanResponse `json:"data"`
		}
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &rescanEnvelope))
		require.True(t, rescanEnvelope.Success)
		require.NotNil(t, rescanEnvelope.Data.RematchJobID, "eligible email method must mint a job id")
		jobID := *rescanEnvelope.Data.RematchJobID

		// GET the job immediately — RegisterPending seeds it synchronously
		// inside RescanRematch, so it is pollable before the dispatcher worker
		// runs. We do NOT wait for completion (that is the E2E flow's job).
		w = doRequest(router, http.MethodGet, "/api/v1/rematch/jobs/"+jobID)
		require.Equal(t, http.StatusOK, w.Code)

		var jobEnvelope struct {
			Success bool               `json:"success"`
			Data    rematchJobResponse `json:"data"`
		}
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &jobEnvelope))
		require.True(t, jobEnvelope.Success)
		assert.Equal(t, jobID, jobEnvelope.Data.ID)
		assert.Equal(t, contact.ID.String(), jobEnvelope.Data.ContactID)
		assert.NotEmpty(t, jobEnvelope.Data.Status, "job status is reported immediately")
		require.NotNil(t, jobEnvelope.Data.Matched, "job response reports a matched count (IMP-021)")

		// The methods being rematched are reported and include the email.
		var foundEmail bool
		for _, m := range jobEnvelope.Data.Methods {
			if m.Type == "email" && m.Value == wantEmail {
				foundEmail = true
			}
		}
		assert.True(t, foundEmail, "job methods should include the eligible email %q, got %+v", wantEmail, jobEnvelope.Data.Methods)
	})

	t.Run("Rescan_NoEligibleMethods_ReturnsNullJobID", func(t *testing.T) {
		// spec: IMP-021
		// Seed a contact with NO method — nothing is eligible, so rescan
		// returns a null job id rather than enqueueing a no-op job.
		contact, cleanup := seedMigrationContact(env.ctx, t, env.database, env.gen, factory.WithNoMethods())
		t.Cleanup(cleanup)

		w := doRequest(router, http.MethodPost, "/api/v1/rematch/contacts/"+contact.ID.String()+"/rescan")
		require.Equal(t, http.StatusOK, w.Code)

		var rescanEnvelope struct {
			Success bool           `json:"success"`
			Data    rescanResponse `json:"data"`
		}
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &rescanEnvelope))
		require.True(t, rescanEnvelope.Success)
		assert.Nil(t, rescanEnvelope.Data.RematchJobID, "no eligible methods must return a null job id")
	})
}

// TestRematchAPI_JobEndpointErrors covers the generic not-found / validation
// contracts of the rematch endpoints. These are framework-level HTTP contracts
// that no behavior owns, so they carry NO citation.
func TestRematchAPI_JobEndpointErrors(t *testing.T) {
	t.Parallel()
	env := setupRematchEnv(t)
	router := newRematchRouter(env)

	t.Run("Rescan_UnknownContact_Returns404", func(t *testing.T) {
		w := doRequest(router, http.MethodPost, "/api/v1/rematch/contacts/"+uuid.NewString()+"/rescan")
		assert.Equal(t, http.StatusNotFound, w.Code)
	})

	t.Run("GetJob_UnknownJob_Returns404", func(t *testing.T) {
		w := doRequest(router, http.MethodGet, "/api/v1/rematch/jobs/"+uuid.NewString())
		assert.Equal(t, http.StatusNotFound, w.Code)
	})

	t.Run("GetJob_MalformedJobID_Returns400", func(t *testing.T) {
		w := doRequest(router, http.MethodGet, "/api/v1/rematch/jobs/not-a-uuid")
		assert.Equal(t, http.StatusBadRequest, w.Code)
	})
}
