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
	"google.golang.org/api/gmail/v1"
	"google.golang.org/api/people/v1"
)

// Scopes defines the OAuth scopes requested for Google APIs
var Scopes = []string{
	"openid",
	"email",
	"profile",
	gmail.GmailReadonlyScope,
	calendar.CalendarReadonlyScope,
	people.ContactsReadonlyScope,
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

// OAuthService handles Google OAuth2 authentication
type OAuthService struct {
	config    *oauth2.Config
	repo      *repository.OAuthRepository
	encryptor *crypto.TokenEncryptor
}

// UserInfo contains user information from Google
type UserInfo struct {
	Email string `json:"email"`
	Name  string `json:"name"`
}

// NewOAuthService creates a new Google OAuth service
func NewOAuthService(cfg *config.Config, repo *repository.OAuthRepository) (*OAuthService, error) {
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
		config:    oauthConfig,
		repo:      repo,
		encryptor: encryptor,
	}, nil
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
	token, err := s.config.Exchange(ctx, code)
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

	// Create a token source that handles refresh
	tokenSource := s.config.TokenSource(ctx, token)

	// Get a potentially refreshed token
	newToken, err := tokenSource.Token()
	if err != nil {
		return nil, fmt.Errorf("refresh token: %w", err)
	}

	// If token was refreshed, save the new one
	if newToken.AccessToken != token.AccessToken {
		if err := s.updateToken(ctx, cred.ID, newToken); err != nil {
			// Log but don't fail - we still have a valid token
			logger.Warn().Err(err).Msg("failed to save refreshed token")
		}
	}

	return oauth2.NewClient(ctx, tokenSource), nil
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
	client := s.config.Client(ctx, token)

	resp, err := client.Get("https://www.googleapis.com/oauth2/v2/userinfo")
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

	// Encrypt refresh token if present (reuse nonce from access token)
	var refreshCiphertext []byte
	if token.RefreshToken != "" {
		refreshCiphertext, err = s.encryptor.EncryptWithNonce(token.RefreshToken, nonce)
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

	// Encrypt refresh token if present (refresh tokens are sometimes rotated, reuse nonce from access token)
	var refreshCiphertext []byte
	if token.RefreshToken != "" {
		refreshCiphertext, err = s.encryptor.EncryptWithNonce(token.RefreshToken, nonce)
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

	// Decrypt refresh token if present
	var refreshToken string
	if len(cred.RefreshTokenEncrypted) > 0 {
		refreshToken, err = s.encryptor.Decrypt(cred.RefreshTokenEncrypted, cred.EncryptionNonce)
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
