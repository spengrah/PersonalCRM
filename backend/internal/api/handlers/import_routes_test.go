package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"
)

// emptySuggestionDeps satisfies every dependency interface of
// SuggestionService with zero-value returns, so ListSuggestions resolves
// to a clean empty 200 without a database. It exists only to give the
// suggestions route a live, non-panicking handler in the ordering test
// below.
type emptySuggestionDeps struct{}

func (emptySuggestionDeps) GetByID(ctx context.Context, id uuid.UUID) (*repository.ExternalContact, error) {
	return nil, nil
}
func (emptySuggestionDeps) ListUnmatched(ctx context.Context, source string, limit, offset int32, includeUnresolvedTelegram bool) ([]repository.ExternalContact, error) {
	return nil, nil
}
func (emptySuggestionDeps) ListUnmatchedBySources(ctx context.Context, sources []string, limit, offset int32, includeUnresolvedTelegram bool) ([]repository.ExternalContact, error) {
	return nil, nil
}
func (emptySuggestionDeps) ListAllUnmatched(ctx context.Context, limit, offset int32, includeUnresolvedTelegram bool) ([]repository.ExternalContact, error) {
	return nil, nil
}
func (emptySuggestionDeps) CountHiddenUnresolvedTelegram(ctx context.Context, source string) (int64, error) {
	return 0, nil
}
func (emptySuggestionDeps) ListPendingMethodSuggestionRows(ctx context.Context, sourceFilter string) ([]repository.PendingMethodSuggestionRow, error) {
	return nil, nil
}
func (emptySuggestionDeps) ResolveReconcileTarget(ctx context.Context, id uuid.UUID) (*repository.ReconcileTarget, error) {
	return nil, nil
}
func (emptySuggestionDeps) GetForUpdateTx(ctx context.Context, tx pgx.Tx, id uuid.UUID) (*repository.ExternalContact, error) {
	return nil, nil
}
func (emptySuggestionDeps) SetMethodSuggestionSetsTx(ctx context.Context, tx pgx.Tx, id uuid.UUID, pending, dismissed []repository.PendingMethodSuggestion) (*repository.ExternalContact, error) {
	return nil, nil
}
func (emptySuggestionDeps) GetContact(ctx context.Context, id uuid.UUID) (*repository.Contact, error) {
	return nil, nil
}
func (emptySuggestionDeps) ListContactMethodsByContact(ctx context.Context, contactID uuid.UUID) ([]repository.ContactMethod, error) {
	return nil, nil
}
func (emptySuggestionDeps) EnrichContactFromExternalWithSelections(ctx context.Context, crmContactID uuid.UUID, external *repository.ExternalContact, selectedMethods []service.MethodSelection, conflictResolutions map[string]string, cadenceArg *string, name *string) (uuid.UUID, error) {
	return uuid.Nil, nil
}
func (emptySuggestionDeps) FindBestMatchesBatch(ctx context.Context, externals []*repository.ExternalContact) ([]*service.ImportSuggestedMatch, error) {
	return nil, nil
}

// TestRegisterImportRoutes_StaticBeforeParamOrdering pins the
// static-before-param registration order INSIDE RegisterImportRoutes:
// the literal /imports/anarlog-title and /imports/suggestions segments
// must resolve to their own handlers, not be swallowed by the /imports/:id
// param route. It drives the real helper (all three deps set) so a future
// reorder of the registrations inside RegisterImportRoutes is caught here.
//
// Distinguishing signals are dependency-free:
//   - anarlog ListAnarlogTitle with a nil service returns 503,
//   - suggestions ListSuggestions with empty deps returns 200,
//   - GetImportCandidate rejects a non-UUID :id with 400 before any repo call,
//
// so a shadow of either static route by /:id would surface as a 400
// ("Invalid external contact ID") instead of the handler's own status.
func TestRegisterImportRoutes_StaticBeforeParamOrdering(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	v1 := router.Group("/api/v1")

	importHandler := NewImportHandler(nil, nil, nil, nil, nil, nil)
	anarlogHandler := NewAnarlogDiscoveryHandler(nil)
	deps := emptySuggestionDeps{}
	suggestionSvc := service.NewSuggestionService(deps, deps, deps, deps, deps, nil)
	suggestionHandler := NewSuggestionHandler(suggestionSvc)

	RegisterImportRoutes(v1, ImportRouteDeps{
		Import:           importHandler,
		AnarlogDiscovery: anarlogHandler,
		Suggestions:      suggestionHandler,
	})

	cases := []struct {
		name       string
		method     string
		path       string
		wantStatus int
	}{
		{"anarlog-title static wins", http.MethodGet, "/api/v1/imports/anarlog-title", http.StatusServiceUnavailable},
		{"suggestions static wins", http.MethodGet, "/api/v1/imports/suggestions", http.StatusOK},
		{"non-static segment falls to :id", http.MethodGet, "/api/v1/imports/not-a-uuid", http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			req := httptest.NewRequest(tc.method, tc.path, nil)
			router.ServeHTTP(w, req)
			require.Equal(t, tc.wantStatus, w.Code, tc.path)
		})
	}

	// The :id route must reach GetImportCandidate specifically — assert on
	// its distinctive validation error so a wrong-handler binding is caught.
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/imports/not-a-uuid", nil)
	router.ServeHTTP(w, req)
	require.Contains(t, w.Body.String(), "Invalid external contact ID")
}
