package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/todoist"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fixedOAuthTestBaseUnix is a deterministic TIME_BASE (a fixed Unix second) used
// to drive accelerated.GetCurrentTime() in the state-expiry tests below without
// ever calling time.Now() directly. Its absolute value is irrelevant — only the
// relative shift applied against it matters.
const fixedOAuthTestBaseUnix int64 = 1735689600 // 2025-01-01T00:00:00Z

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

// MockTodoistOAuthService is a mock implementation of
// todoist.OAuthServiceInterface, mirroring MockOAuthService's configurable
// func-field shape so Todoist tests can twin the Google callback/auth-URL
// tests one-for-one.
type MockTodoistOAuthService struct {
	GetAuthURLFunc       func(state string) string
	ExchangeCodeFunc     func(ctx context.Context, code string) (*repository.OAuthCredentialStatus, error)
	ListAccountsFunc     func(ctx context.Context) ([]repository.OAuthCredentialStatus, error)
	GetAccountStatusFunc func(ctx context.Context, id uuid.UUID) (*repository.OAuthCredentialStatus, error)
	RevokeAccountFunc    func(ctx context.Context, id uuid.UUID) error
}

var _ todoist.OAuthServiceInterface = (*MockTodoistOAuthService)(nil)

func (m *MockTodoistOAuthService) GetAuthURL(state string) string {
	if m.GetAuthURLFunc != nil {
		return m.GetAuthURLFunc(state)
	}
	return "https://todoist.com/oauth/authorize?state=" + state
}

func (m *MockTodoistOAuthService) ExchangeCode(ctx context.Context, code string) (*repository.OAuthCredentialStatus, error) {
	if m.ExchangeCodeFunc != nil {
		return m.ExchangeCodeFunc(ctx, code)
	}
	return nil, errors.New("not implemented")
}

func (m *MockTodoistOAuthService) ListAccounts(ctx context.Context) ([]repository.OAuthCredentialStatus, error) {
	if m.ListAccountsFunc != nil {
		return m.ListAccountsFunc(ctx)
	}
	return nil, nil
}

func (m *MockTodoistOAuthService) GetAccountStatus(ctx context.Context, id uuid.UUID) (*repository.OAuthCredentialStatus, error) {
	if m.GetAccountStatusFunc != nil {
		return m.GetAccountStatusFunc(ctx, id)
	}
	return nil, db.ErrNotFound
}

func (m *MockTodoistOAuthService) RevokeAccount(ctx context.Context, id uuid.UUID) error {
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

// spec: SET-002[0]
func TestGetGoogleAuthURL_StoresStateServerSide(t *testing.T) {
	mock := &MockOAuthService{}
	handler := NewOAuthHandler(mock, "http://localhost:3000")

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/auth/google", nil)

	handler.GetGoogleAuthURL(c)

	require.Equal(t, http.StatusOK, w.Code)

	var response struct {
		Data struct {
			URL   string `json:"url"`
			State string `json:"state"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
	require.NotEmpty(t, response.Data.State)

	// The exact state handed back to the caller must have been stored
	// server-side: validateState accepts it exactly once, proving the copy in
	// the response and the copy in the store are the same value, not merely
	// two independently-generated randoms.
	assert.True(t, handler.validateState(response.Data.State), "the state returned to the caller must have been stored server-side")
	assert.False(t, handler.validateState(response.Data.State), "a stored state must be single-use")
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

	t.Run("invokes email and gchat reconcilers on valid exchange", func(t *testing.T) {
		mock := &MockOAuthService{
			ExchangeCodeFunc: func(ctx context.Context, code string) (*repository.OAuthCredentialStatus, error) {
				return &repository.OAuthCredentialStatus{
					ID:        uuid.New(),
					Provider:  "google",
					AccountID: "test@example.com",
				}, nil
			},
		}
		handler := NewOAuthHandler(mock, "http://localhost:3000")

		emailCalled, gchatCalled := false, false
		handler.SetEmailStateReconciler(func(context.Context) error {
			emailCalled = true
			return nil
		})
		handler.SetGChatStateReconciler(func(context.Context) error {
			gchatCalled = true
			return nil
		})

		state := "valid-state-789"
		handler.storeState(state)

		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest("GET", "/auth/google/callback?code=authcode&state="+state, nil)

		handler.GoogleCallback(c)

		assert.Equal(t, http.StatusFound, w.Code)
		assert.Contains(t, w.Header().Get("Location"), "/settings?auth=success")
		assert.True(t, emailCalled, "email reconciler must be invoked after a successful connect")
		assert.True(t, gchatCalled, "gchat reconciler must be invoked after a successful connect")
	})

	t.Run("gchat reconciler error is non-fatal (still redirects success)", func(t *testing.T) {
		mock := &MockOAuthService{
			ExchangeCodeFunc: func(ctx context.Context, code string) (*repository.OAuthCredentialStatus, error) {
				return &repository.OAuthCredentialStatus{
					ID:        uuid.New(),
					Provider:  "google",
					AccountID: "test@example.com",
				}, nil
			},
		}
		handler := NewOAuthHandler(mock, "http://localhost:3000")
		handler.SetGChatStateReconciler(func(context.Context) error {
			return errors.New("reconcile boom")
		})

		state := "valid-state-abc"
		handler.storeState(state)

		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest("GET", "/auth/google/callback?code=authcode&state="+state, nil)

		handler.GoogleCallback(c)

		// A reconciler error must NOT fail the connect — the account is connected
		// regardless; boot / the next connect retries.
		assert.Equal(t, http.StatusFound, w.Code)
		assert.Contains(t, w.Header().Get("Location"), "/settings?auth=success")
	})
}

// spec: SET-003[3]
func TestGoogleCallback_ExpiredStateAbortsBeforeExchange(t *testing.T) {
	exchangeCalled := false
	mock := &MockOAuthService{
		ExchangeCodeFunc: func(ctx context.Context, code string) (*repository.OAuthCredentialStatus, error) {
			exchangeCalled = true
			return &repository.OAuthCredentialStatus{
				ID:        uuid.New(),
				Provider:  "google",
				AccountID: "test@example.com",
			}, nil
		},
	}
	handler := NewOAuthHandler(mock, "http://localhost:3000")

	t.Setenv("TIME_ACCELERATION", "60")
	t.Setenv("TIME_BASE", strconv.FormatInt(fixedOAuthTestBaseUnix, 10))

	state := "expiring-state-" + uuid.New().String()[:8]
	handler.storeState(state)

	// Shift the accelerated clock's base far enough into the past that the
	// resulting accelerated "now" lands well beyond the state's 10-minute TTL
	// (a 1000s base shift * 60x acceleration = 60,000 accelerated seconds,
	// which dwarfs both the 600s TTL and any real-wall-clock jitter between
	// the two GetCurrentTime() calls involved).
	t.Setenv("TIME_BASE", strconv.FormatInt(fixedOAuthTestBaseUnix-1000, 10))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/auth/google/callback?code=authcode&state="+state, nil)

	handler.GoogleCallback(c)

	assert.Equal(t, http.StatusFound, w.Code)
	location := w.Header().Get("Location")
	assert.Contains(t, location, "/settings?auth=error")
	assert.Contains(t, location, "message=invalid_state")
	assert.False(t, exchangeCalled, "an expired state must abort the flow before the code exchange")
}

func TestGoogleCallback_FailureRedirectsIncludeProvider(t *testing.T) {
	t.Run("provider error carries provider name", func(t *testing.T) {
		// spec: SET-004[2]
		mock := &MockOAuthService{}
		handler := NewOAuthHandler(mock, "http://localhost:3000")

		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest("GET", "/auth/google/callback?error=access_denied", nil)

		handler.GoogleCallback(c)

		assert.Equal(t, http.StatusFound, w.Code)
		location := w.Header().Get("Location")
		assert.Contains(t, location, "auth=error")
		assert.Contains(t, location, "provider=google")
		assert.Contains(t, location, "message=access_denied")
	})

	t.Run("invalid state carries provider name", func(t *testing.T) {
		// spec: SET-004[2]
		mock := &MockOAuthService{}
		handler := NewOAuthHandler(mock, "http://localhost:3000")

		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest("GET", "/auth/google/callback?code=authcode&state=never-stored-"+uuid.New().String()[:8], nil)

		handler.GoogleCallback(c)

		assert.Equal(t, http.StatusFound, w.Code)
		location := w.Header().Get("Location")
		assert.Contains(t, location, "auth=error")
		assert.Contains(t, location, "provider=google")
		assert.Contains(t, location, "message=invalid_state")
	})

	t.Run("exchange failure carries provider name", func(t *testing.T) {
		// spec: SET-004[2]
		mock := &MockOAuthService{
			ExchangeCodeFunc: func(ctx context.Context, code string) (*repository.OAuthCredentialStatus, error) {
				return nil, errors.New("exchange failed")
			},
		}
		handler := NewOAuthHandler(mock, "http://localhost:3000")

		state := "provider-check-state-" + uuid.New().String()[:8]
		handler.storeState(state)

		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest("GET", "/auth/google/callback?code=authcode&state="+state, nil)

		handler.GoogleCallback(c)

		assert.Equal(t, http.StatusFound, w.Code)
		location := w.Header().Get("Location")
		assert.Contains(t, location, "auth=error")
		assert.Contains(t, location, "provider=google")
		assert.Contains(t, location, "message=exchange_failed")
	})
}

// spec: SET-004[3]
func TestGoogleCallback_ErrorRedirectIsProperlyURLEncoded(t *testing.T) {
	mock := &MockOAuthService{}
	handler := NewOAuthHandler(mock, "http://localhost:3000")

	// A provider error message containing characters that require
	// percent-encoding (a space, and an "&" that would otherwise be read as a
	// query-param delimiter).
	rawMessage := "invalid state & retry"
	reqURL := "/auth/google/callback?" + (url.Values{"error": {rawMessage}}).Encode()

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", reqURL, nil)

	handler.GoogleCallback(c)

	assert.Equal(t, http.StatusFound, w.Code)
	location := w.Header().Get("Location")

	parsed, err := url.Parse(location)
	require.NoError(t, err)
	assert.Equal(t, "error", parsed.Query().Get("auth"))
	assert.Equal(t, "google", parsed.Query().Get("provider"))
	assert.Equal(t, rawMessage, parsed.Query().Get("message"), "the message must round-trip exactly through proper query encoding, not string concatenation")
	assert.NotContains(t, location, " ", "a literal space in the Location header means a param was concatenated rather than URL-encoded")
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

		// spec: SET-003[2]
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

// spec: SET-002[1]
func TestOAuthState_ExpiresAfterTenMinutes(t *testing.T) {
	mock := &MockOAuthService{}
	handler := NewOAuthHandler(mock, "http://localhost:3000")

	t.Setenv("TIME_ACCELERATION", "60")
	t.Setenv("TIME_BASE", strconv.FormatInt(fixedOAuthTestBaseUnix, 10))

	state := "ttl-state-" + uuid.New().String()[:8]
	handler.storeState(state)

	// Shift the accelerated clock's base far enough into the past that the
	// resulting accelerated "now" lands well beyond the state's 10-minute TTL
	// (a 1000s base shift * 60x acceleration = 60,000 accelerated seconds,
	// which dwarfs both the 600s TTL and any real-wall-clock jitter between
	// the two GetCurrentTime() calls involved).
	t.Setenv("TIME_BASE", strconv.FormatInt(fixedOAuthTestBaseUnix-1000, 10))

	assert.False(t, handler.validateState(state), "a state older than 10 minutes must be rejected")
}

// spec: SET-002[2]
// Twin of TestGetGoogleAuthURL: proves the Todoist auth-URL response also
// carries both the provider authorization URL and the state.
func TestGetTodoistAuthURL(t *testing.T) {
	mock := &MockTodoistOAuthService{
		GetAuthURLFunc: func(state string) string {
			return "https://todoist.com/oauth/authorize?state=" + state + "&client_id=test"
		},
	}

	handler := NewOAuthHandler(&MockOAuthService{}, "http://localhost:3000")
	handler.SetTodoistOAuth(mock)

	t.Run("returns auth URL with state", func(t *testing.T) {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest("GET", "/auth/todoist", nil)

		handler.GetTodoistAuthURL(c)

		assert.Equal(t, http.StatusOK, w.Code)

		var response struct {
			Data struct {
				URL   string `json:"url"`
				State string `json:"state"`
			} `json:"data"`
		}
		err := json.Unmarshal(w.Body.Bytes(), &response)
		require.NoError(t, err)

		assert.Contains(t, response.Data.URL, "todoist.com")
		assert.Contains(t, response.Data.URL, response.Data.State)
		assert.NotEmpty(t, response.Data.State)
	})
}

// spec: SET-003[0]
// Twin of the Google error-short-circuit ordering: a provider error param
// must abort the flow before any state validation or code exchange. A
// valid state is stored ahead of the request so that, if the short-circuit
// did NOT run first, the handler would fall through and consume it during
// state validation (and/or invoke the exchange) -- neither of which may
// happen here.
func TestTodoistCallback_ErrorShortCircuitsBeforeStateOrExchange(t *testing.T) {
	exchangeCalled := false
	mock := &MockTodoistOAuthService{
		ExchangeCodeFunc: func(ctx context.Context, code string) (*repository.OAuthCredentialStatus, error) {
			exchangeCalled = true
			return &repository.OAuthCredentialStatus{
				ID:        uuid.New(),
				Provider:  "todoist",
				AccountID: "test-todoist-user",
			}, nil
		},
	}
	handler := NewOAuthHandler(&MockOAuthService{}, "http://localhost:3000")
	handler.SetTodoistOAuth(mock)

	state := "todoist-error-short-circuit-" + uuid.New().String()[:8]
	handler.storeState(state)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/auth/todoist/callback?error=access_denied&code=authcode&state="+state, nil)

	handler.TodoistCallback(c)

	assert.Equal(t, http.StatusFound, w.Code)
	location := w.Header().Get("Location")
	assert.Contains(t, location, "/settings?auth=error")
	assert.Contains(t, location, "provider=todoist")
	assert.Contains(t, location, "message=access_denied")
	assert.False(t, exchangeCalled, "a provider error must short-circuit before the code exchange")
	assert.True(t, handler.validateState(state), "a provider error must short-circuit before state validation, leaving the state unconsumed")
}

// spec: SET-003[1]
// Twin of the Google validate-then-exchange ordering: an invalid state
// aborts before the exchange runs, and a valid state is validated and
// consumed (single-use) before the exchange is attempted -- regardless of
// whether that exchange goes on to succeed or fail.
func TestTodoistCallback_ValidatesStateBeforeExchange(t *testing.T) {
	t.Run("invalid state aborts before exchange", func(t *testing.T) {
		exchangeCalled := false
		mock := &MockTodoistOAuthService{
			ExchangeCodeFunc: func(ctx context.Context, code string) (*repository.OAuthCredentialStatus, error) {
				exchangeCalled = true
				return &repository.OAuthCredentialStatus{
					ID:        uuid.New(),
					Provider:  "todoist",
					AccountID: "test-todoist-user",
				}, nil
			},
		}
		handler := NewOAuthHandler(&MockOAuthService{}, "http://localhost:3000")
		handler.SetTodoistOAuth(mock)

		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest("GET", "/auth/todoist/callback?code=authcode&state=never-stored-"+uuid.New().String()[:8], nil)

		handler.TodoistCallback(c)

		assert.Equal(t, http.StatusFound, w.Code)
		location := w.Header().Get("Location")
		assert.Contains(t, location, "/settings?auth=error")
		assert.Contains(t, location, "message=invalid_state")
		assert.False(t, exchangeCalled, "an invalid state must abort the flow before the code exchange")
	})

	t.Run("valid state is consumed before the exchange runs, regardless of outcome", func(t *testing.T) {
		mock := &MockTodoistOAuthService{
			ExchangeCodeFunc: func(ctx context.Context, code string) (*repository.OAuthCredentialStatus, error) {
				return nil, errors.New("exchange failed")
			},
		}
		handler := NewOAuthHandler(&MockOAuthService{}, "http://localhost:3000")
		handler.SetTodoistOAuth(mock)

		state := "todoist-consume-state-" + uuid.New().String()[:8]
		handler.storeState(state)

		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest("GET", "/auth/todoist/callback?code=authcode&state="+state, nil)

		handler.TodoistCallback(c)

		assert.Equal(t, http.StatusFound, w.Code)
		assert.Contains(t, w.Header().Get("Location"), "message=exchange_failed")
		// The state must already be consumed even though the exchange itself
		// failed -- proof that validation-and-consumption happens BEFORE the
		// exchange is attempted, not as a side effect of a successful one.
		assert.False(t, handler.validateState(state), "the CSRF state must be consumed before the exchange runs, regardless of the exchange outcome")
	})
}

// assertTrivialRedirectBody proves a callback response never carries a
// rendered page or a JSON body. net/http's Redirect helper (which
// gin.Context.Redirect delegates to) writes a small boilerplate
// `<a href="...">Found</a>.` anchor body for GET requests, purely mirroring
// the Location header -- that boilerplate is the only content a redirect
// may legitimately carry, so this asserts the body (if any) contains
// neither JSON markers nor an actual rendered page, and stays trivially
// small.
func assertTrivialRedirectBody(t *testing.T, w *httptest.ResponseRecorder) {
	t.Helper()
	body := w.Body.String()
	assert.NotContains(t, body, "{", "a callback redirect must never carry a JSON body")
	assert.NotContains(t, body, "<html", "a callback redirect must never carry a rendered page")
	assert.NotContains(t, body, "<body", "a callback redirect must never carry a rendered page")
	assert.Less(t, len(body), 300, "a callback redirect's body must be trivially small (net/http's boilerplate anchor link, at most) -- never a rendered page")
}

// spec: SET-004[0]
// Neither provider's callback redirect may carry a rendered page or JSON
// body -- the response must be a bare redirect (allowing only net/http's
// own trivial boilerplate anchor body, never one this handler wrote).
func TestOAuthCallbackRedirect_BodyIsEmpty(t *testing.T) {
	t.Run("google callback redirect carries no body", func(t *testing.T) {
		handler := NewOAuthHandler(&MockOAuthService{}, "http://localhost:3000")

		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest("GET", "/auth/google/callback?error=access_denied", nil)

		handler.GoogleCallback(c)

		assert.Equal(t, http.StatusFound, w.Code)
		assertTrivialRedirectBody(t, w)
	})

	t.Run("todoist callback redirect carries no body", func(t *testing.T) {
		handler := NewOAuthHandler(&MockOAuthService{}, "http://localhost:3000")
		handler.SetTodoistOAuth(&MockTodoistOAuthService{})

		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest("GET", "/auth/todoist/callback?error=access_denied", nil)

		handler.TodoistCallback(c)

		assert.Equal(t, http.StatusFound, w.Code)
		assertTrivialRedirectBody(t, w)
	})
}

// spec: SET-004[0]
// Twin of the Google redirect-shape assertions: every TodoistCallback
// outcome must redirect to the settings surface carrying the matching
// outcome and provider name.
func TestTodoistCallback_RedirectShape(t *testing.T) {
	t.Run("provider error", func(t *testing.T) {
		handler := NewOAuthHandler(&MockOAuthService{}, "http://localhost:3000")
		handler.SetTodoistOAuth(&MockTodoistOAuthService{})

		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest("GET", "/auth/todoist/callback?error=access_denied", nil)

		handler.TodoistCallback(c)

		assert.Equal(t, http.StatusFound, w.Code)
		location := w.Header().Get("Location")
		assert.Contains(t, location, "/settings?auth=error")
		assert.Contains(t, location, "provider=todoist")
		assert.Contains(t, location, "message=access_denied")
	})

	t.Run("invalid state", func(t *testing.T) {
		handler := NewOAuthHandler(&MockOAuthService{}, "http://localhost:3000")
		handler.SetTodoistOAuth(&MockTodoistOAuthService{})

		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest("GET", "/auth/todoist/callback?code=authcode&state=invalid", nil)

		handler.TodoistCallback(c)

		assert.Equal(t, http.StatusFound, w.Code)
		location := w.Header().Get("Location")
		assert.Contains(t, location, "/settings?auth=error")
		assert.Contains(t, location, "provider=todoist")
		assert.Contains(t, location, "message=invalid_state")
	})

	t.Run("exchange failure", func(t *testing.T) {
		mock := &MockTodoistOAuthService{
			ExchangeCodeFunc: func(ctx context.Context, code string) (*repository.OAuthCredentialStatus, error) {
				return nil, errors.New("exchange failed")
			},
		}
		handler := NewOAuthHandler(&MockOAuthService{}, "http://localhost:3000")
		handler.SetTodoistOAuth(mock)

		state := "todoist-shape-exchange-fail-" + uuid.New().String()[:8]
		handler.storeState(state)

		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest("GET", "/auth/todoist/callback?code=authcode&state="+state, nil)

		handler.TodoistCallback(c)

		assert.Equal(t, http.StatusFound, w.Code)
		location := w.Header().Get("Location")
		assert.Contains(t, location, "/settings?auth=error")
		assert.Contains(t, location, "provider=todoist")
		assert.Contains(t, location, "message=exchange_failed")
	})
}

// spec: SET-004[1]
// Twin of TestGoogleCallback's "redirects to success on valid exchange":
// a successful Todoist exchange redirects with auth=success and
// provider=todoist.
func TestTodoistCallback_SuccessRedirectIncludesProvider(t *testing.T) {
	accountID := uuid.New()
	mock := &MockTodoistOAuthService{
		ExchangeCodeFunc: func(ctx context.Context, code string) (*repository.OAuthCredentialStatus, error) {
			return &repository.OAuthCredentialStatus{
				ID:        accountID,
				Provider:  "todoist",
				AccountID: "test-todoist-user",
			}, nil
		},
	}
	handler := NewOAuthHandler(&MockOAuthService{}, "http://localhost:3000")
	handler.SetTodoistOAuth(mock)

	state := "todoist-success-state-" + uuid.New().String()[:8]
	handler.storeState(state)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/auth/todoist/callback?code=authcode&state="+state, nil)

	handler.TodoistCallback(c)

	assert.Equal(t, http.StatusFound, w.Code)
	location := w.Header().Get("Location")
	assert.Contains(t, location, "/settings?auth=success")
	assert.Contains(t, location, "provider=todoist")
}

// spec: SET-008[0]
// Twin of TestListGoogleAccounts: proves ListTodoistAccounts' 200 response
// carries an array of connected-account objects. The shape assertion below
// decodes into map[string]interface{} (the raw wire shape) rather than the
// TodoistAccountResponse DTO, so a json-tag rename on the DTO cannot mask a
// dropped/renamed field, and the fixture uses pairwise-distinct
// created/updated/expires timestamps and a run-unique account id/name so a
// field swap between them is detectable.
func TestListTodoistAccounts(t *testing.T) {
	t.Run("returns empty list when no accounts", func(t *testing.T) {
		mock := &MockTodoistOAuthService{
			ListAccountsFunc: func(ctx context.Context) ([]repository.OAuthCredentialStatus, error) {
				return []repository.OAuthCredentialStatus{}, nil
			},
		}
		handler := NewOAuthHandler(&MockOAuthService{}, "http://localhost:3000")
		handler.SetTodoistOAuth(mock)

		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest("GET", "/auth/todoist/accounts", nil)

		handler.ListTodoistAccounts(c)

		assert.Equal(t, http.StatusOK, w.Code)

		var response struct {
			Data []map[string]interface{} `json:"data"`
		}
		err := json.Unmarshal(w.Body.Bytes(), &response)
		require.NoError(t, err)
		assert.Empty(t, response.Data)
	})

	t.Run("returns list of accounts with literal wire-key shape", func(t *testing.T) {
		accountID := uuid.New()
		externalAccountID := "todoist-user-" + uuid.New().String()[:8]
		accountName := "Todoist User " + uuid.New().String()[:8]
		created := accelerated.GetCurrentTime()
		updated := created.Add(30 * time.Minute)
		expires := created.Add(1 * time.Hour)

		mock := &MockTodoistOAuthService{
			ListAccountsFunc: func(ctx context.Context) ([]repository.OAuthCredentialStatus, error) {
				return []repository.OAuthCredentialStatus{
					{
						ID:          accountID,
						Provider:    "todoist",
						AccountID:   externalAccountID,
						AccountName: &accountName,
						ExpiresAt:   &expires,
						Scopes:      []string{"data:read_write", "data:delete"},
						CreatedAt:   created,
						UpdatedAt:   updated,
					},
				}, nil
			},
		}
		handler := NewOAuthHandler(&MockOAuthService{}, "http://localhost:3000")
		handler.SetTodoistOAuth(mock)

		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest("GET", "/auth/todoist/accounts", nil)

		handler.ListTodoistAccounts(c)

		assert.Equal(t, http.StatusOK, w.Code)

		var response struct {
			Data []map[string]interface{} `json:"data"`
		}
		err := json.Unmarshal(w.Body.Bytes(), &response)
		require.NoError(t, err)
		require.Len(t, response.Data, 1)

		account := response.Data[0]
		assert.Equal(t, accountID.String(), account["id"], "wire key 'id'")
		assert.Equal(t, externalAccountID, account["account_id"], "wire key 'account_id'")
		assert.Equal(t, accountName, account["account_name"], "wire key 'account_name'")
		assert.Equal(t, expires.Format(time.RFC3339), account["expires_at"], "wire key 'expires_at'")
		assert.Equal(t, created.Format(time.RFC3339), account["created_at"], "wire key 'created_at'")
		assert.Equal(t, updated.Format(time.RFC3339), account["updated_at"], "wire key 'updated_at'")
		scopes, ok := account["scopes"].([]interface{})
		require.True(t, ok, "wire key 'scopes' must be an array")
		assert.ElementsMatch(t, []interface{}{"data:read_write", "data:delete"}, scopes)
	})

	t.Run("returns error on service failure", func(t *testing.T) {
		mock := &MockTodoistOAuthService{
			ListAccountsFunc: func(ctx context.Context) ([]repository.OAuthCredentialStatus, error) {
				return nil, errors.New("database error")
			},
		}
		handler := NewOAuthHandler(&MockOAuthService{}, "http://localhost:3000")
		handler.SetTodoistOAuth(mock)

		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest("GET", "/auth/todoist/accounts", nil)

		handler.ListTodoistAccounts(c)

		assert.Equal(t, http.StatusInternalServerError, w.Code)
	})
}

// Twin of TestGetGoogleAccountStatus: proves GetTodoistAccountStatus shares
// the same malformed-id / unknown-id / success contract as its Google
// counterpart.
func TestGetTodoistAccountStatus(t *testing.T) {
	t.Run("returns error on invalid UUID", func(t *testing.T) {
		// spec: SET-008[1]
		handler := NewOAuthHandler(&MockOAuthService{}, "http://localhost:3000")
		handler.SetTodoistOAuth(&MockTodoistOAuthService{})

		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest("GET", "/auth/todoist/accounts/not-a-uuid/status", nil)
		c.Params = []gin.Param{{Key: "id", Value: "not-a-uuid"}}

		handler.GetTodoistAccountStatus(c)

		assert.Equal(t, http.StatusBadRequest, w.Code)
	})

	t.Run("returns 404 for non-existent account", func(t *testing.T) {
		// spec: SET-008[2]
		mock := &MockTodoistOAuthService{
			GetAccountStatusFunc: func(ctx context.Context, id uuid.UUID) (*repository.OAuthCredentialStatus, error) {
				return nil, db.ErrNotFound
			},
		}
		handler := NewOAuthHandler(&MockOAuthService{}, "http://localhost:3000")
		handler.SetTodoistOAuth(mock)

		accountID := uuid.New()
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest("GET", "/auth/todoist/accounts/"+accountID.String()+"/status", nil)
		c.Params = []gin.Param{{Key: "id", Value: accountID.String()}}

		handler.GetTodoistAccountStatus(c)

		assert.Equal(t, http.StatusNotFound, w.Code)
	})

	t.Run("returns account status", func(t *testing.T) {
		// spec: SET-008[3]
		accountID := uuid.New()
		externalAccountID := "todoist-user-" + uuid.New().String()[:8]
		accountName := "Todoist Status User " + uuid.New().String()[:8]
		created := accelerated.GetCurrentTime()
		updated := created.Add(15 * time.Minute)

		mock := &MockTodoistOAuthService{
			GetAccountStatusFunc: func(ctx context.Context, id uuid.UUID) (*repository.OAuthCredentialStatus, error) {
				return &repository.OAuthCredentialStatus{
					ID:          accountID,
					Provider:    "todoist",
					AccountID:   externalAccountID,
					AccountName: &accountName,
					Scopes:      []string{"data:read_write"},
					CreatedAt:   created,
					UpdatedAt:   updated,
				}, nil
			},
		}
		handler := NewOAuthHandler(&MockOAuthService{}, "http://localhost:3000")
		handler.SetTodoistOAuth(mock)

		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest("GET", "/auth/todoist/accounts/"+accountID.String()+"/status", nil)
		c.Params = []gin.Param{{Key: "id", Value: accountID.String()}}

		handler.GetTodoistAccountStatus(c)

		assert.Equal(t, http.StatusOK, w.Code)

		var response struct {
			Data map[string]interface{} `json:"data"`
		}
		err := json.Unmarshal(w.Body.Bytes(), &response)
		require.NoError(t, err)
		assert.Equal(t, accountID.String(), response.Data["id"], "wire key 'id'")
		assert.Equal(t, externalAccountID, response.Data["account_id"], "wire key 'account_id'")
		assert.Equal(t, accountName, response.Data["account_name"], "wire key 'account_name'")
		assert.Equal(t, created.Format(time.RFC3339), response.Data["created_at"], "wire key 'created_at'")
		assert.Equal(t, updated.Format(time.RFC3339), response.Data["updated_at"], "wire key 'updated_at'")
	})
}

// Twin of TestRevokeGoogleAccount: proves RevokeTodoistAccount shares the
// same malformed-id / unknown-id / success-confirmation contract as its
// Google counterpart.
func TestRevokeTodoistAccount(t *testing.T) {
	t.Run("returns error on invalid UUID", func(t *testing.T) {
		// spec: SET-008[1]
		handler := NewOAuthHandler(&MockOAuthService{}, "http://localhost:3000")
		handler.SetTodoistOAuth(&MockTodoistOAuthService{})

		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest("POST", "/auth/todoist/accounts/not-a-uuid/revoke", nil)
		c.Params = []gin.Param{{Key: "id", Value: "not-a-uuid"}}

		handler.RevokeTodoistAccount(c)

		assert.Equal(t, http.StatusBadRequest, w.Code)
	})

	t.Run("returns 404 for non-existent account", func(t *testing.T) {
		// spec: SET-008[2]
		mock := &MockTodoistOAuthService{
			RevokeAccountFunc: func(ctx context.Context, id uuid.UUID) error {
				return db.ErrNotFound
			},
		}
		handler := NewOAuthHandler(&MockOAuthService{}, "http://localhost:3000")
		handler.SetTodoistOAuth(mock)

		accountID := uuid.New()
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest("POST", "/auth/todoist/accounts/"+accountID.String()+"/revoke", nil)
		c.Params = []gin.Param{{Key: "id", Value: accountID.String()}}

		handler.RevokeTodoistAccount(c)

		assert.Equal(t, http.StatusNotFound, w.Code)
	})

	t.Run("successfully revokes account", func(t *testing.T) {
		// spec: SET-008[3]
		revokedID := uuid.Nil
		mock := &MockTodoistOAuthService{
			RevokeAccountFunc: func(ctx context.Context, id uuid.UUID) error {
				revokedID = id
				return nil
			},
		}
		handler := NewOAuthHandler(&MockOAuthService{}, "http://localhost:3000")
		handler.SetTodoistOAuth(mock)

		accountID := uuid.New()
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest("POST", "/auth/todoist/accounts/"+accountID.String()+"/revoke", nil)
		c.Params = []gin.Param{{Key: "id", Value: accountID.String()}}

		handler.RevokeTodoistAccount(c)

		assert.Equal(t, http.StatusOK, w.Code)
		assert.Equal(t, accountID, revokedID)

		var response struct {
			Data map[string]interface{} `json:"data"`
		}
		err := json.Unmarshal(w.Body.Bytes(), &response)
		require.NoError(t, err)
		message, ok := response.Data["message"].(string)
		require.True(t, ok, "wire key 'message' must be a string")
		assert.Contains(t, message, "disconnected")
	})

	t.Run("returns error on service failure", func(t *testing.T) {
		mock := &MockTodoistOAuthService{
			RevokeAccountFunc: func(ctx context.Context, id uuid.UUID) error {
				return errors.New("revocation failed")
			},
		}
		handler := NewOAuthHandler(&MockOAuthService{}, "http://localhost:3000")
		handler.SetTodoistOAuth(mock)

		accountID := uuid.New()
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest("POST", "/auth/todoist/accounts/"+accountID.String()+"/revoke", nil)
		c.Params = []gin.Param{{Key: "id", Value: accountID.String()}}

		handler.RevokeTodoistAccount(c)

		assert.Equal(t, http.StatusInternalServerError, w.Code)
	})
}
