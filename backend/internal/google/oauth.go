package google

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/crypto"
	"personal-crm/backend/internal/logger"
	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/calendar/v3"
	chat "google.golang.org/api/chat/v1"
	"google.golang.org/api/gmail/v1"
	"google.golang.org/api/people/v1"
)

// Scopes defines the OAuth scopes requested for Google APIs.
//
// The three chat.*.readonly scopes back the Google Chat sync provider:
// chat.spaces.readonly lists the user's spaces, chat.messages.readonly lists
// messages, and chat.memberships.readonly is REQUIRED for spaces.members.list
// under User authentication (the membership-resolution path). Adding scopes
// forces a one-time re-consent for already-connected accounts; existing
// Gmail/Calendar tokens keep working with their previously-granted scopes.
var Scopes = []string{
	"openid",
	"email",
	"profile",
	gmail.GmailReadonlyScope,
	calendar.CalendarReadonlyScope,
	people.ContactsReadonlyScope,
	chat.ChatSpacesReadonlyScope,
	chat.ChatMessagesReadonlyScope,
	chat.ChatMembershipsReadonlyScope,
}

// ProviderName is the identifier for Google OAuth credentials
const ProviderName = "google"

// OAuthServiceInterface defines the interface for OAuth operations
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

// defaultUserInfoEndpoint is Google's OpenID userinfo URL.
const defaultUserInfoEndpoint = "https://www.googleapis.com/oauth2/v2/userinfo"

// OAuthService handles Google OAuth2 authentication
type OAuthService struct {
	config    *oauth2.Config
	repo      *repository.OAuthRepository
	syncRepo  *repository.SyncRepository
	encryptor *crypto.TokenEncryptor

	// userInfoEndpoint is the URL queried for the account's email and name. It
	// defaults to Google's endpoint and is overridable only for tests.
	userInfoEndpoint string

	// httpClient, when set, replaces the default HTTP client for token exchange,
	// refresh, and userinfo requests. It exists only to let tests substitute an
	// httptest server; production leaves it nil and uses oauth2's default client.
	httpClient *http.Client
}

// UserInfo contains user information from Google
type UserInfo struct {
	Email string `json:"email"`
	Name  string `json:"name"`
}

// NewOAuthService creates a new Google OAuth service
func NewOAuthService(cfg *config.Config, repo *repository.OAuthRepository, syncRepo *repository.SyncRepository) (*OAuthService, error) {
	if cfg.Google.ClientID == "" || cfg.Google.ClientSecret == "" {
		return nil, fmt.Errorf("google OAuth credentials not configured")
	}

	encryptor, err := crypto.NewTokenEncryptor(cfg.External.TokenEncryptionKey)
	if err != nil {
		return nil, fmt.Errorf("create token encryptor: %w", err)
	}

	oauthConfig := &oauth2.Config{
		ClientID:     cfg.Google.ClientID,
		ClientSecret: cfg.Google.ClientSecret,
		RedirectURL:  cfg.Google.RedirectURL,
		Scopes:       Scopes,
		Endpoint:     google.Endpoint,
	}

	return &OAuthService{
		config:           oauthConfig,
		repo:             repo,
		syncRepo:         syncRepo,
		encryptor:        encryptor,
		userInfoEndpoint: defaultUserInfoEndpoint,
	}, nil
}

// SetHTTPClient sets a custom HTTP client used for token exchange, refresh, and
// userinfo requests. It is intended for tests that substitute an httptest server.
func (s *OAuthService) SetHTTPClient(client *http.Client) {
	s.httpClient = client
}

// SetTokenEndpoint overrides the OAuth token endpoint. It is intended for tests
// that point token exchange and refresh at an httptest server.
func (s *OAuthService) SetTokenEndpoint(tokenURL string) {
	s.config.Endpoint.TokenURL = tokenURL
}

// SetUserInfoEndpoint overrides the userinfo endpoint. It is intended for tests
// that point the account lookup at an httptest server.
func (s *OAuthService) SetUserInfoEndpoint(userInfoURL string) {
	s.userInfoEndpoint = userInfoURL
}

// clientContext returns a context carrying the configured HTTP client so the
// oauth2 library routes token exchange and refresh through it. When no custom
// client is set it returns the original context, preserving production behavior.
func (s *OAuthService) clientContext(ctx context.Context) context.Context {
	if s.httpClient == nil {
		return ctx
	}
	return context.WithValue(ctx, oauth2.HTTPClient, s.httpClient)
}

// GenerateState generates a secure random state for CSRF protection
func GenerateState() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate state: %w", err)
	}
	return base64.URLEncoding.EncodeToString(b), nil
}

// GetAuthURL returns the URL to redirect user for authorization
func (s *OAuthService) GetAuthURL(state string) string {
	return s.config.AuthCodeURL(
		state,
		oauth2.AccessTypeOffline,
		oauth2.SetAuthURLParam("prompt", "consent"),
	)
}

// ExchangeCode exchanges an authorization code for tokens and stores them
func (s *OAuthService) ExchangeCode(ctx context.Context, code string) (*repository.OAuthCredentialStatus, error) {
	// Exchange code for token
	token, err := s.config.Exchange(s.clientContext(ctx), code)
	if err != nil {
		return nil, fmt.Errorf("exchange code: %w", err)
	}

	// Get user info to determine account email
	userInfo, err := s.getUserInfo(ctx, token)
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

// GetClientForAccount returns an authenticated HTTP client for a specific account
// The client automatically handles token refresh
func (s *OAuthService) GetClientForAccount(ctx context.Context, accountID string) (*http.Client, error) {
	token, cred, err := s.getToken(ctx, accountID)
	if err != nil {
		return nil, err
	}

	// Wrap the refreshing token source so that any refresh — whether it happens
	// now or later inside a long-running sync — is written back to the database.
	// Without this only a construction-time refresh would be persisted, so every
	// later refresh would be redundant and a rotated refresh token would be lost.
	source := s.persistingTokenSource(ctx, cred.ID, token)

	return oauth2.NewClient(ctx, source), nil
}

// persistingTokenSource wraps the OAuth refreshing token source so that a token
// changed by a refresh is persisted exactly once. oauth2.ReuseTokenSource caches
// the token and only calls the wrapped source when the cached token is invalid,
// so the persist callback runs only on an actual refresh.
func (s *OAuthService) persistingTokenSource(ctx context.Context, id uuid.UUID, token *oauth2.Token) oauth2.TokenSource {
	base := s.config.TokenSource(s.clientContext(ctx), token)
	persisting := &persistOnRefreshSource{
		ctx:      ctx,
		service:  s,
		id:       id,
		previous: token,
		source:   base,
	}
	return oauth2.ReuseTokenSource(token, persisting)
}

// persistOnRefreshSource persists a refreshed token before returning it. It is
// only consulted by oauth2.ReuseTokenSource when the cached token is no longer
// valid, so Token() returning a value different from the last seen token means a
// real refresh occurred.
type persistOnRefreshSource struct {
	ctx      context.Context
	service  *OAuthService
	id       uuid.UUID
	previous *oauth2.Token
	source   oauth2.TokenSource
}

func (p *persistOnRefreshSource) Token() (*oauth2.Token, error) {
	newToken, err := p.source.Token()
	if err != nil {
		return nil, fmt.Errorf("refresh token: %w", err)
	}

	if newToken.AccessToken != p.previous.AccessToken {
		if err := p.service.updateToken(p.ctx, p.id, newToken); err != nil {
			// Log but don't fail - we still have a valid token to return.
			logger.Warn().Err(err).Msg("failed to save refreshed token")
		}
		p.previous = newToken
	}

	return newToken, nil
}

// ListAccounts returns all connected Google accounts
func (s *OAuthService) ListAccounts(ctx context.Context) ([]repository.OAuthCredentialStatus, error) {
	return s.repo.ListStatusesByProvider(ctx, ProviderName)
}

// GetAccountStatus returns the status of a specific account
func (s *OAuthService) GetAccountStatus(ctx context.Context, id uuid.UUID) (*repository.OAuthCredentialStatus, error) {
	return s.repo.GetStatus(ctx, id)
}

// IsAuthenticated checks if a specific account is connected
func (s *OAuthService) IsAuthenticated(ctx context.Context, accountID string) bool {
	_, err := s.repo.GetByProviderAndAccount(ctx, ProviderName, accountID)
	return err == nil
}

// HasAnyAccount checks if any Google account is connected
func (s *OAuthService) HasAnyAccount(ctx context.Context) bool {
	count, err := s.repo.Count(ctx, ProviderName)
	return err == nil && count > 0
}

// RevokeAccount disconnects a specific Google account
func (s *OAuthService) RevokeAccount(ctx context.Context, id uuid.UUID) error {
	// Get the credential to get the access token for revocation
	cred, err := s.repo.GetByID(ctx, id)
	if err != nil {
		return fmt.Errorf("get credential: %w", err)
	}

	// Delete associated sync states before credential deletion.
	// This is best-effort cleanup: credential deletion proceeds even if sync state cleanup fails.
	// We use the account_id (email) to identify which sync states to delete.
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
		// Revoke token with Google
		revokeURL := "https://oauth2.googleapis.com/revoke?token=" + accessToken
		resp, err := http.Post(revokeURL, "application/x-www-form-urlencoded", nil)
		if err != nil {
			logger.Warn().Err(err).Msg("failed to revoke token with Google")
		} else {
			if err := resp.Body.Close(); err != nil {
				logger.Warn().Err(err).Msg("failed to close revoke response body")
			}
			if resp.StatusCode != http.StatusOK {
				logger.Warn().Int("status", resp.StatusCode).Msg("Google revoke returned non-OK status")
			}
		}
	}

	// Delete from database
	return s.repo.Delete(ctx, id)
}

// getUserInfo fetches the user's email and name from Google
func (s *OAuthService) getUserInfo(ctx context.Context, token *oauth2.Token) (*UserInfo, error) {
	client := s.config.Client(s.clientContext(ctx), token)

	resp, err := client.Get(s.userInfoEndpoint)
	if err != nil {
		return nil, fmt.Errorf("fetch user info: %w", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			logger.Warn().Err(err).Msg("failed to close user info response body")
		}
	}()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("user info request failed with status %d", resp.StatusCode)
	}

	var userInfo UserInfo
	if err := json.NewDecoder(resp.Body).Decode(&userInfo); err != nil {
		return nil, fmt.Errorf("decode user info: %w", err)
	}

	return &userInfo, nil
}

// storeToken encrypts and stores the OAuth token
func (s *OAuthService) storeToken(ctx context.Context, token *oauth2.Token, userInfo *UserInfo) (*repository.OAuthCredential, error) {
	// Encrypt access token
	accessCiphertext, nonce, err := s.encryptor.Encrypt(token.AccessToken)
	if err != nil {
		return nil, fmt.Errorf("encrypt access token: %w", err)
	}

	// Encrypt refresh token if present, using its own nonce. Reusing the access
	// token's nonce with the same key would leak keystream under AES-GCM.
	var refreshCiphertext, refreshNonce []byte
	if token.RefreshToken != "" {
		refreshCiphertext, refreshNonce, err = s.encryptor.Encrypt(token.RefreshToken)
		if err != nil {
			return nil, fmt.Errorf("encrypt refresh token: %w", err)
		}
	}

	var expiresAt *time.Time
	if !token.Expiry.IsZero() {
		expiresAt = &token.Expiry
	}

	var accountName *string
	if userInfo.Name != "" {
		accountName = &userInfo.Name
	}

	return s.repo.Upsert(ctx, repository.UpsertOAuthCredentialRequest{
		Provider:              ProviderName,
		AccountID:             userInfo.Email,
		AccountName:           accountName,
		AccessTokenEncrypted:  accessCiphertext,
		RefreshTokenEncrypted: refreshCiphertext,
		RefreshTokenNonce:     refreshNonce,
		EncryptionNonce:       nonce,
		TokenType:             token.TokenType,
		ExpiresAt:             expiresAt,
		Scopes:                Scopes,
	})
}

// updateToken updates the stored token after a refresh
func (s *OAuthService) updateToken(ctx context.Context, id uuid.UUID, token *oauth2.Token) error {
	// Encrypt access token
	accessCiphertext, nonce, err := s.encryptor.Encrypt(token.AccessToken)
	if err != nil {
		return fmt.Errorf("encrypt access token: %w", err)
	}

	// Encrypt refresh token if present (refresh tokens are sometimes rotated),
	// using its own nonce. Reusing the access token's nonce with the same key
	// would leak keystream under AES-GCM.
	var refreshCiphertext, refreshNonce []byte
	if token.RefreshToken != "" {
		refreshCiphertext, refreshNonce, err = s.encryptor.Encrypt(token.RefreshToken)
		if err != nil {
			return fmt.Errorf("encrypt refresh token: %w", err)
		}
	}

	var expiresAt *time.Time
	if !token.Expiry.IsZero() {
		expiresAt = &token.Expiry
	}

	_, err = s.repo.UpdateTokens(ctx, id, repository.UpdateOAuthTokensRequest{
		AccessTokenEncrypted:  accessCiphertext,
		RefreshTokenEncrypted: refreshCiphertext,
		RefreshTokenNonce:     refreshNonce,
		EncryptionNonce:       nonce,
		ExpiresAt:             expiresAt,
	})

	return err
}

// getToken retrieves and decrypts the OAuth token for an account
func (s *OAuthService) getToken(ctx context.Context, accountID string) (*oauth2.Token, *repository.OAuthCredential, error) {
	cred, err := s.repo.GetByProviderAndAccount(ctx, ProviderName, accountID)
	if err != nil {
		return nil, nil, fmt.Errorf("get credential: %w", err)
	}

	// Decrypt access token
	accessToken, err := s.encryptor.Decrypt(cred.AccessTokenEncrypted, cred.EncryptionNonce)
	if err != nil {
		return nil, nil, fmt.Errorf("decrypt access token: %w", err)
	}

	// Decrypt refresh token if present. Rows written by the current code carry a
	// dedicated refresh_token_nonce; legacy rows (NULL nonce) shared the access
	// token's nonce, so fall back to it for backward compatibility.
	var refreshToken string
	if len(cred.RefreshTokenEncrypted) > 0 {
		refreshNonce := cred.RefreshTokenNonce
		if len(refreshNonce) == 0 {
			refreshNonce = cred.EncryptionNonce
		}
		refreshToken, err = s.encryptor.Decrypt(cred.RefreshTokenEncrypted, refreshNonce)
		if err != nil {
			return nil, nil, fmt.Errorf("decrypt refresh token: %w", err)
		}
	}

	var expiry time.Time
	if cred.ExpiresAt != nil {
		expiry = *cred.ExpiresAt
	}

	token := &oauth2.Token{
		AccessToken:  accessToken,
		RefreshToken: refreshToken,
		TokenType:    cred.TokenType,
		Expiry:       expiry,
	}

	return token, cred, nil
}

// GetTokenForAccount returns the decrypted token for a specific account
// This is useful for services that need to construct their own clients
func (s *OAuthService) GetTokenForAccount(ctx context.Context, accountID string) (*oauth2.Token, error) {
	token, _, err := s.getToken(ctx, accountID)
	return token, err
}
