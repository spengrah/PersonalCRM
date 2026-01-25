package todoist

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/crypto"
	"personal-crm/backend/internal/logger"
	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
)

// Scopes defines the OAuth scopes requested for Todoist API
// data:read_write is required for Sync API reads and commands
// data:delete is included for future deletion flows without re-auth
var Scopes = []string{
	"data:read_write",
	"data:delete",
}

// ProviderName is the identifier for Todoist OAuth credentials
const ProviderName = "todoist"

// Todoist OAuth endpoints (variables to allow testing overrides)
var (
	AuthorizationEndpoint = "https://app.todoist.com/oauth/authorize"
	TokenEndpoint         = "https://api.todoist.com/oauth/access_token"
	RevokeEndpoint        = "https://api.todoist.com/api/v1/revoke"
	UserInfoEndpoint      = "https://api.todoist.com/api/v1/user"
)

// OAuthServiceInterface defines the interface for Todoist OAuth operations
// This interface allows for mocking in tests
type OAuthServiceInterface interface {
	GetAuthURL(state string) string
	ExchangeCode(ctx context.Context, code string) (*repository.OAuthCredentialStatus, error)
	ListAccounts(ctx context.Context) ([]repository.OAuthCredentialStatus, error)
	GetAccountStatus(ctx context.Context, id uuid.UUID) (*repository.OAuthCredentialStatus, error)
	RevokeAccount(ctx context.Context, id uuid.UUID) error
}

// Ensure OAuthService implements OAuthServiceInterface
var _ OAuthServiceInterface = (*OAuthService)(nil)

// OAuthService handles Todoist OAuth2 authentication
type OAuthService struct {
	clientID     string
	clientSecret string
	redirectURL  string
	repo         *repository.OAuthRepository
	syncRepo     *repository.SyncRepository
	encryptor    *crypto.TokenEncryptor
	httpClient   *http.Client
}

// UserInfo contains user information from Todoist
type UserInfo struct {
	ID    string `json:"id"`
	Email string `json:"email"`
}

// TokenResponse represents the OAuth token response from Todoist
type TokenResponse struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
}

// NewOAuthService creates a new Todoist OAuth service
func NewOAuthService(cfg *config.Config, repo *repository.OAuthRepository, syncRepo *repository.SyncRepository) (*OAuthService, error) {
	if cfg.Todoist.ClientID == "" || cfg.Todoist.ClientSecret == "" {
		return nil, fmt.Errorf("todoist OAuth credentials not configured")
	}

	encryptor, err := crypto.NewTokenEncryptor(cfg.External.TokenEncryptionKey)
	if err != nil {
		return nil, fmt.Errorf("create token encryptor: %w", err)
	}

	return &OAuthService{
		clientID:     cfg.Todoist.ClientID,
		clientSecret: cfg.Todoist.ClientSecret,
		redirectURL:  cfg.Todoist.RedirectURL,
		repo:         repo,
		syncRepo:     syncRepo,
		encryptor:    encryptor,
		httpClient:   &http.Client{},
	}, nil
}

// GetAuthURL returns the URL to redirect user for Todoist authorization
func (s *OAuthService) GetAuthURL(state string) string {
	params := url.Values{}
	params.Set("client_id", s.clientID)
	params.Set("scope", strings.Join(Scopes, ","))
	params.Set("state", state)

	return AuthorizationEndpoint + "?" + params.Encode()
}

// ExchangeCode exchanges an authorization code for tokens and stores them
func (s *OAuthService) ExchangeCode(ctx context.Context, code string) (*repository.OAuthCredentialStatus, error) {
	// Exchange code for token
	token, err := s.exchangeCodeForToken(ctx, code)
	if err != nil {
		return nil, fmt.Errorf("exchange code: %w", err)
	}

	// Get user info to determine account ID and email
	userInfo, err := s.getUserInfo(ctx, token.AccessToken)
	if err != nil {
		return nil, fmt.Errorf("get user info: %w", err)
	}

	// Store the token
	cred, err := s.storeToken(ctx, token, userInfo)
	if err != nil {
		return nil, fmt.Errorf("store token: %w", err)
	}

	// Return status (non-sensitive info)
	return &repository.OAuthCredentialStatus{
		ID:          cred.ID,
		Provider:    cred.Provider,
		AccountID:   cred.AccountID,
		AccountName: cred.AccountName,
		ExpiresAt:   cred.ExpiresAt,
		Scopes:      cred.Scopes,
		CreatedAt:   cred.CreatedAt,
		UpdatedAt:   cred.UpdatedAt,
	}, nil
}

// exchangeCodeForToken exchanges the authorization code for an access token
func (s *OAuthService) exchangeCodeForToken(ctx context.Context, code string) (*TokenResponse, error) {
	data := url.Values{}
	data.Set("client_id", s.clientID)
	data.Set("client_secret", s.clientSecret)
	data.Set("code", code)
	data.Set("redirect_uri", s.redirectURL)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, TokenEndpoint, strings.NewReader(data.Encode()))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("token request: %w", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			logger.Warn().Err(err).Msg("failed to close token response body")
		}
	}()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("token exchange failed with status %d: %s", resp.StatusCode, string(body))
	}

	var tokenResp TokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&tokenResp); err != nil {
		return nil, fmt.Errorf("decode token response: %w", err)
	}

	return &tokenResp, nil
}

// getUserInfo fetches the user's ID and email from Todoist
func (s *OAuthService) getUserInfo(ctx context.Context, accessToken string) (*UserInfo, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, UserInfoEndpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("user info request: %w", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			logger.Warn().Err(err).Msg("failed to close user info response body")
		}
	}()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("user info request failed with status %d: %s", resp.StatusCode, string(body))
	}

	var userInfo UserInfo
	if err := json.NewDecoder(resp.Body).Decode(&userInfo); err != nil {
		return nil, fmt.Errorf("decode user info: %w", err)
	}

	return &userInfo, nil
}

// storeToken encrypts and stores the OAuth token
func (s *OAuthService) storeToken(ctx context.Context, token *TokenResponse, userInfo *UserInfo) (*repository.OAuthCredential, error) {
	// Encrypt access token
	accessCiphertext, nonce, err := s.encryptor.Encrypt(token.AccessToken)
	if err != nil {
		return nil, fmt.Errorf("encrypt access token: %w", err)
	}

	// Use email as account_name for display in UI (as per issue spec)
	var accountName *string
	if userInfo.Email != "" {
		accountName = &userInfo.Email
	}

	// Todoist tokens don't have refresh tokens or expiry
	return s.repo.Upsert(ctx, repository.UpsertOAuthCredentialRequest{
		Provider:              ProviderName,
		AccountID:             userInfo.ID, // Use Todoist user ID as account_id
		AccountName:           accountName, // Use email for display
		AccessTokenEncrypted:  accessCiphertext,
		RefreshTokenEncrypted: nil, // Todoist doesn't provide refresh tokens
		EncryptionNonce:       nonce,
		TokenType:             token.TokenType,
		ExpiresAt:             nil, // Todoist tokens are long-lived until revoked
		Scopes:                Scopes,
	})
}

// ListAccounts returns all connected Todoist accounts
func (s *OAuthService) ListAccounts(ctx context.Context) ([]repository.OAuthCredentialStatus, error) {
	return s.repo.ListStatusesByProvider(ctx, ProviderName)
}

// GetAccountStatus returns the status of a specific account
func (s *OAuthService) GetAccountStatus(ctx context.Context, id uuid.UUID) (*repository.OAuthCredentialStatus, error) {
	return s.repo.GetStatus(ctx, id)
}

// RevokeAccount disconnects a specific Todoist account
func (s *OAuthService) RevokeAccount(ctx context.Context, id uuid.UUID) error {
	// Get the credential to get the access token for revocation
	cred, err := s.repo.GetByID(ctx, id)
	if err != nil {
		return fmt.Errorf("get credential: %w", err)
	}

	// Delete associated sync states before credential deletion.
	// This is best-effort cleanup: credential deletion proceeds even if sync state cleanup fails.
	// We use the account_id (Todoist user ID) to identify which sync states to delete.
	if s.syncRepo != nil {
		if err := s.syncRepo.DeleteSyncStatesByAccountID(ctx, cred.AccountID); err != nil {
			logger.Warn().Err(err).Str("account_id", cred.AccountID).Msg("failed to delete sync states for account")
		}
	}

	// Decrypt access token
	accessToken, err := s.encryptor.Decrypt(cred.AccessTokenEncrypted, cred.EncryptionNonce)
	if err != nil {
		// Log but continue - we still want to delete local credential
		logger.Warn().Err(err).Msg("failed to decrypt token for revocation")
	} else {
		// Revoke token with Todoist using RFC7009
		if revokeErr := s.revokeToken(ctx, accessToken); revokeErr != nil {
			logger.Warn().Err(revokeErr).Msg("failed to revoke token with Todoist")
		}
	}

	// Delete from database
	return s.repo.Delete(ctx, id)
}

// revokeToken revokes the access token with Todoist using RFC7009
func (s *OAuthService) revokeToken(ctx context.Context, accessToken string) error {
	data := url.Values{}
	data.Set("token", accessToken)
	data.Set("token_type_hint", "access_token")

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, RevokeEndpoint, strings.NewReader(data.Encode()))
	if err != nil {
		return fmt.Errorf("create revoke request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	// Use HTTP Basic auth with client credentials
	basicAuth := base64.StdEncoding.EncodeToString([]byte(s.clientID + ":" + s.clientSecret))
	req.Header.Set("Authorization", "Basic "+basicAuth)

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("revoke request: %w", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			logger.Warn().Err(err).Msg("failed to close revoke response body")
		}
	}()

	// RFC7009 allows 200 OK for successful revocation
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("revoke failed with status %d: %s", resp.StatusCode, string(body))
	}

	return nil
}

// GetAccessToken retrieves and decrypts the access token for an account
// This is useful for services that need to make API calls on behalf of the user
func (s *OAuthService) GetAccessToken(ctx context.Context, accountID string) (string, error) {
	cred, err := s.repo.GetByProviderAndAccount(ctx, ProviderName, accountID)
	if err != nil {
		return "", fmt.Errorf("get credential: %w", err)
	}

	accessToken, err := s.encryptor.Decrypt(cred.AccessTokenEncrypted, cred.EncryptionNonce)
	if err != nil {
		return "", fmt.Errorf("decrypt access token: %w", err)
	}

	return accessToken, nil
}

// HasAnyAccount checks if any Todoist account is connected
func (s *OAuthService) HasAnyAccount(ctx context.Context) bool {
	count, err := s.repo.Count(ctx, ProviderName)
	return err == nil && count > 0
}

// SetHTTPClient allows setting a custom HTTP client (useful for testing)
func (s *OAuthService) SetHTTPClient(client *http.Client) {
	s.httpClient = client
}

// exchangeCodeForTokenWithURL is a test helper that allows specifying a custom endpoint URL
func (s *OAuthService) exchangeCodeForTokenWithURL(ctx context.Context, code string, endpointURL string) (*TokenResponse, error) {
	data := url.Values{}
	data.Set("client_id", s.clientID)
	data.Set("client_secret", s.clientSecret)
	data.Set("code", code)
	data.Set("redirect_uri", s.redirectURL)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpointURL, strings.NewReader(data.Encode()))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("token request: %w", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			logger.Warn().Err(err).Msg("failed to close token response body")
		}
	}()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("token exchange failed with status %d: %s", resp.StatusCode, string(body))
	}

	var tokenResp TokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&tokenResp); err != nil {
		return nil, fmt.Errorf("decode token response: %w", err)
	}

	return &tokenResp, nil
}

// getUserInfoWithURL is a test helper that allows specifying a custom endpoint URL
func (s *OAuthService) getUserInfoWithURL(ctx context.Context, accessToken string, endpointURL string) (*UserInfo, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpointURL, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("user info request: %w", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			logger.Warn().Err(err).Msg("failed to close user info response body")
		}
	}()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("user info request failed with status %d: %s", resp.StatusCode, string(body))
	}

	var userInfo UserInfo
	if err := json.NewDecoder(resp.Body).Decode(&userInfo); err != nil {
		return nil, fmt.Errorf("decode user info: %w", err)
	}

	return &userInfo, nil
}

// revokeTokenWithURL is a test helper that allows specifying a custom endpoint URL
func (s *OAuthService) revokeTokenWithURL(ctx context.Context, accessToken string, endpointURL string) error {
	data := url.Values{}
	data.Set("token", accessToken)
	data.Set("token_type_hint", "access_token")

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpointURL, strings.NewReader(data.Encode()))
	if err != nil {
		return fmt.Errorf("create revoke request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	// Use HTTP Basic auth with client credentials
	basicAuth := base64.StdEncoding.EncodeToString([]byte(s.clientID + ":" + s.clientSecret))
	req.Header.Set("Authorization", "Basic "+basicAuth)

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("revoke request: %w", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			logger.Warn().Err(err).Msg("failed to close revoke response body")
		}
	}()

	// RFC7009 allows 200 OK for successful revocation
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("revoke failed with status %d: %s", resp.StatusCode, string(body))
	}

	return nil
}
