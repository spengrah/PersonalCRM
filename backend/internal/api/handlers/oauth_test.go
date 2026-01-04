package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// MockOAuthService is a mock implementation of google.OAuthServiceInterface
type MockOAuthService struct {
	GetAuthURLFunc       func(state string) string
	ExchangeCodeFunc     func(ctx context.Context, code string) (*repository.OAuthCredentialStatus, error)
	ListAccountsFunc     func(ctx context.Context) ([]repository.OAuthCredentialStatus, error)
	GetAccountStatusFunc func(ctx context.Context, id uuid.UUID) (*repository.OAuthCredentialStatus, error)
	RevokeAccountFunc    func(ctx context.Context, id uuid.UUID) error
}

func (m *MockOAuthService) GetAuthURL(state string) string {
	if m.GetAuthURLFunc != nil {
		return m.GetAuthURLFunc(state)
	}
	return "https://accounts.google.com/o/oauth2/v2/auth?state=" + state
}

func (m *MockOAuthService) ExchangeCode(ctx context.Context, code string) (*repository.OAuthCredentialStatus, error) {
	if m.ExchangeCodeFunc != nil {
		return m.ExchangeCodeFunc(ctx, code)
	}
	return nil, errors.New("not implemented")
}

func (m *MockOAuthService) ListAccounts(ctx context.Context) ([]repository.OAuthCredentialStatus, error) {
	if m.ListAccountsFunc != nil {
		return m.ListAccountsFunc(ctx)
	}
	return nil, nil
}

func (m *MockOAuthService) GetAccountStatus(ctx context.Context, id uuid.UUID) (*repository.OAuthCredentialStatus, error) {
	if m.GetAccountStatusFunc != nil {
		return m.GetAccountStatusFunc(ctx, id)
	}
	return nil, db.ErrNotFound
}

func (m *MockOAuthService) RevokeAccount(ctx context.Context, id uuid.UUID) error {
	if m.RevokeAccountFunc != nil {
		return m.RevokeAccountFunc(ctx, id)
	}
	return nil
}

func init() {
	gin.SetMode(gin.TestMode)
}

func TestGetGoogleAuthURL(t *testing.T) {
	mock := &MockOAuthService{
		GetAuthURLFunc: func(state string) string {
			return "https://accounts.google.com/o/oauth2/v2/auth?state=" + state + "&client_id=test"
		},
	}

	handler := NewOAuthHandler(mock, "http://localhost:3000")

	t.Run("returns auth URL with state", func(t *testing.T) {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest("GET", "/auth/google", nil)

		handler.GetGoogleAuthURL(c)

		assert.Equal(t, http.StatusOK, w.Code)

		var response struct {
			Data struct {
				URL   string `json:"url"`
				State string `json:"state"`
			} `json:"data"`
		}
		err := json.Unmarshal(w.Body.Bytes(), &response)
		require.NoError(t, err)

		assert.Contains(t, response.Data.URL, "accounts.google.com")
		assert.Contains(t, response.Data.URL, response.Data.State)
		assert.NotEmpty(t, response.Data.State)
	})
}

func TestGoogleCallback(t *testing.T) {
	t.Run("redirects on Google error", func(t *testing.T) {
		mock := &MockOAuthService{}
		handler := NewOAuthHandler(mock, "http://localhost:3000")

		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest("GET", "/auth/google/callback?error=access_denied", nil)

		handler.GoogleCallback(c)

		assert.Equal(t, http.StatusFound, w.Code)
		location := w.Header().Get("Location")
		assert.Contains(t, location, "/settings?auth=error")
		assert.Contains(t, location, "message=access_denied")
	})

	t.Run("redirects on invalid state", func(t *testing.T) {
		mock := &MockOAuthService{}
		handler := NewOAuthHandler(mock, "http://localhost:3000")

		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest("GET", "/auth/google/callback?code=authcode&state=invalid", nil)

		handler.GoogleCallback(c)

		assert.Equal(t, http.StatusFound, w.Code)
		location := w.Header().Get("Location")
		assert.Contains(t, location, "/settings?auth=error")
		assert.Contains(t, location, "message=invalid_state")
	})

	t.Run("redirects on exchange failure", func(t *testing.T) {
		mock := &MockOAuthService{
			ExchangeCodeFunc: func(ctx context.Context, code string) (*repository.OAuthCredentialStatus, error) {
				return nil, errors.New("exchange failed")
			},
		}
		handler := NewOAuthHandler(mock, "http://localhost:3000")

		// First store a valid state
		state := "valid-state-123"
		handler.storeState(state)

		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest("GET", "/auth/google/callback?code=authcode&state="+state, nil)

		handler.GoogleCallback(c)

		assert.Equal(t, http.StatusFound, w.Code)
		location := w.Header().Get("Location")
		assert.Contains(t, location, "/settings?auth=error")
		assert.Contains(t, location, "message=exchange_failed")
	})

	t.Run("redirects to success on valid exchange", func(t *testing.T) {
		accountID := uuid.New()
		mock := &MockOAuthService{
			ExchangeCodeFunc: func(ctx context.Context, code string) (*repository.OAuthCredentialStatus, error) {
				return &repository.OAuthCredentialStatus{
					ID:        accountID,
					Provider:  "google",
					AccountID: "test@example.com",
				}, nil
			},
		}
		handler := NewOAuthHandler(mock, "http://localhost:3000")

		// Store valid state
		state := "valid-state-456"
		handler.storeState(state)

		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest("GET", "/auth/google/callback?code=authcode&state="+state, nil)

		handler.GoogleCallback(c)

		assert.Equal(t, http.StatusFound, w.Code)
		location := w.Header().Get("Location")
		assert.Contains(t, location, "/settings?auth=success")
		assert.Contains(t, location, "provider=google")
	})
}

func TestListGoogleAccounts(t *testing.T) {
	t.Run("returns empty list when no accounts", func(t *testing.T) {
		mock := &MockOAuthService{
			ListAccountsFunc: func(ctx context.Context) ([]repository.OAuthCredentialStatus, error) {
				return []repository.OAuthCredentialStatus{}, nil
			},
		}
		handler := NewOAuthHandler(mock, "http://localhost:3000")

		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest("GET", "/auth/google/accounts", nil)

		handler.ListGoogleAccounts(c)

		assert.Equal(t, http.StatusOK, w.Code)

		var response struct {
			Data []GoogleAccountResponse `json:"data"`
		}
		err := json.Unmarshal(w.Body.Bytes(), &response)
		require.NoError(t, err)
		assert.Empty(t, response.Data)
	})

	t.Run("returns list of accounts", func(t *testing.T) {
		accountID := uuid.New()
		accountName := "Test User"
		now := accelerated.GetCurrentTime()
		expires := now.Add(1 * time.Hour)

		mock := &MockOAuthService{
			ListAccountsFunc: func(ctx context.Context) ([]repository.OAuthCredentialStatus, error) {
				return []repository.OAuthCredentialStatus{
					{
						ID:          accountID,
						Provider:    "google",
						AccountID:   "test@example.com",
						AccountName: &accountName,
						ExpiresAt:   &expires,
						Scopes:      []string{"gmail.readonly", "calendar.readonly"},
						CreatedAt:   now,
						UpdatedAt:   now,
					},
				}, nil
			},
		}
		handler := NewOAuthHandler(mock, "http://localhost:3000")

		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest("GET", "/auth/google/accounts", nil)

		handler.ListGoogleAccounts(c)

		assert.Equal(t, http.StatusOK, w.Code)

		var response struct {
			Data []GoogleAccountResponse `json:"data"`
		}
		err := json.Unmarshal(w.Body.Bytes(), &response)
		require.NoError(t, err)
		require.Len(t, response.Data, 1)
		assert.Equal(t, accountID.String(), response.Data[0].ID)
		assert.Equal(t, "test@example.com", response.Data[0].AccountID)
		assert.Equal(t, &accountName, response.Data[0].AccountName)
		assert.Len(t, response.Data[0].Scopes, 2)
	})

	t.Run("returns error on service failure", func(t *testing.T) {
		mock := &MockOAuthService{
			ListAccountsFunc: func(ctx context.Context) ([]repository.OAuthCredentialStatus, error) {
				return nil, errors.New("database error")
			},
		}
		handler := NewOAuthHandler(mock, "http://localhost:3000")

		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest("GET", "/auth/google/accounts", nil)

		handler.ListGoogleAccounts(c)

		assert.Equal(t, http.StatusInternalServerError, w.Code)
	})
}

func TestGetGoogleAccountStatus(t *testing.T) {
	t.Run("returns error on invalid UUID", func(t *testing.T) {
		mock := &MockOAuthService{}
		handler := NewOAuthHandler(mock, "http://localhost:3000")

		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest("GET", "/auth/google/accounts/not-a-uuid/status", nil)
		c.Params = []gin.Param{{Key: "id", Value: "not-a-uuid"}}

		handler.GetGoogleAccountStatus(c)

		assert.Equal(t, http.StatusBadRequest, w.Code)
	})

	t.Run("returns 404 for non-existent account", func(t *testing.T) {
		mock := &MockOAuthService{
			GetAccountStatusFunc: func(ctx context.Context, id uuid.UUID) (*repository.OAuthCredentialStatus, error) {
				return nil, db.ErrNotFound
			},
		}
		handler := NewOAuthHandler(mock, "http://localhost:3000")

		accountID := uuid.New()
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest("GET", "/auth/google/accounts/"+accountID.String()+"/status", nil)
		c.Params = []gin.Param{{Key: "id", Value: accountID.String()}}

		handler.GetGoogleAccountStatus(c)

		assert.Equal(t, http.StatusNotFound, w.Code)
	})

	t.Run("returns account status", func(t *testing.T) {
		accountID := uuid.New()
		accountName := "Test User"
		now := accelerated.GetCurrentTime()

		mock := &MockOAuthService{
			GetAccountStatusFunc: func(ctx context.Context, id uuid.UUID) (*repository.OAuthCredentialStatus, error) {
				return &repository.OAuthCredentialStatus{
					ID:          accountID,
					Provider:    "google",
					AccountID:   "test@example.com",
					AccountName: &accountName,
					Scopes:      []string{"gmail.readonly"},
					CreatedAt:   now,
					UpdatedAt:   now,
				}, nil
			},
		}
		handler := NewOAuthHandler(mock, "http://localhost:3000")

		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest("GET", "/auth/google/accounts/"+accountID.String()+"/status", nil)
		c.Params = []gin.Param{{Key: "id", Value: accountID.String()}}

		handler.GetGoogleAccountStatus(c)

		assert.Equal(t, http.StatusOK, w.Code)

		var response struct {
			Data GoogleAccountResponse `json:"data"`
		}
		err := json.Unmarshal(w.Body.Bytes(), &response)
		require.NoError(t, err)
		assert.Equal(t, accountID.String(), response.Data.ID)
		assert.Equal(t, "test@example.com", response.Data.AccountID)
	})
}

func TestRevokeGoogleAccount(t *testing.T) {
	t.Run("returns error on invalid UUID", func(t *testing.T) {
		mock := &MockOAuthService{}
		handler := NewOAuthHandler(mock, "http://localhost:3000")

		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest("POST", "/auth/google/accounts/not-a-uuid/revoke", nil)
		c.Params = []gin.Param{{Key: "id", Value: "not-a-uuid"}}

		handler.RevokeGoogleAccount(c)

		assert.Equal(t, http.StatusBadRequest, w.Code)
	})

	t.Run("returns 404 for non-existent account", func(t *testing.T) {
		mock := &MockOAuthService{
			RevokeAccountFunc: func(ctx context.Context, id uuid.UUID) error {
				return db.ErrNotFound
			},
		}
		handler := NewOAuthHandler(mock, "http://localhost:3000")

		accountID := uuid.New()
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest("POST", "/auth/google/accounts/"+accountID.String()+"/revoke", nil)
		c.Params = []gin.Param{{Key: "id", Value: accountID.String()}}

		handler.RevokeGoogleAccount(c)

		assert.Equal(t, http.StatusNotFound, w.Code)
	})

	t.Run("successfully revokes account", func(t *testing.T) {
		revokedID := uuid.Nil
		mock := &MockOAuthService{
			RevokeAccountFunc: func(ctx context.Context, id uuid.UUID) error {
				revokedID = id
				return nil
			},
		}
		handler := NewOAuthHandler(mock, "http://localhost:3000")

		accountID := uuid.New()
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest("POST", "/auth/google/accounts/"+accountID.String()+"/revoke", nil)
		c.Params = []gin.Param{{Key: "id", Value: accountID.String()}}

		handler.RevokeGoogleAccount(c)

		assert.Equal(t, http.StatusOK, w.Code)
		assert.Equal(t, accountID, revokedID)

		var response struct {
			Data struct {
				Message string `json:"message"`
			} `json:"data"`
		}
		err := json.Unmarshal(w.Body.Bytes(), &response)
		require.NoError(t, err)
		assert.Contains(t, response.Data.Message, "disconnected")
	})

	t.Run("returns error on service failure", func(t *testing.T) {
		mock := &MockOAuthService{
			RevokeAccountFunc: func(ctx context.Context, id uuid.UUID) error {
				return errors.New("revocation failed")
			},
		}
		handler := NewOAuthHandler(mock, "http://localhost:3000")

		accountID := uuid.New()
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest("POST", "/auth/google/accounts/"+accountID.String()+"/revoke", nil)
		c.Params = []gin.Param{{Key: "id", Value: accountID.String()}}

		handler.RevokeGoogleAccount(c)

		assert.Equal(t, http.StatusInternalServerError, w.Code)
	})
}

func TestStateValidation(t *testing.T) {
	t.Run("state can only be used once", func(t *testing.T) {
		mock := &MockOAuthService{}
		handler := NewOAuthHandler(mock, "http://localhost:3000")

		state := "one-time-state"
		handler.storeState(state)

		// First validation should succeed
		assert.True(t, handler.validateState(state))

		// Second validation should fail (state consumed)
		assert.False(t, handler.validateState(state))
	})

	t.Run("unknown state is rejected", func(t *testing.T) {
		mock := &MockOAuthService{}
		handler := NewOAuthHandler(mock, "http://localhost:3000")

		assert.False(t, handler.validateState("unknown-state"))
	})
}
