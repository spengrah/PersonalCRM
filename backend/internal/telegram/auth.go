package telegram

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/crypto"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"

	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/auth"
	"github.com/gotd/td/tg"
	"github.com/rs/zerolog/log"
)

var (
	ErrAuthInProgress   = errors.New("auth already in progress")
	ErrAlreadyConnected = errors.New("already connected")
	ErrAuthTokenInvalid = errors.New("invalid auth token")
	ErrAuthTokenExpired = errors.New("auth token expired")
	ErrAuthInternal     = errors.New("internal auth error")
)

// AuthCode carries a verification code from the HTTP handler to the Run callback.
type AuthCode struct {
	Code string
}

// AuthResult carries the outcome of an auth step.
type AuthResult struct {
	Status   string // "awaiting_code", "awaiting_password", "connected", "error"
	CodeType string // "app", "sms", "call" — from gotd's SentCode.Type
	Error    error
	UserID   int64
	Username string
}

// AuthStatus represents the current auth manager status (for the GET /status endpoint).
type AuthStatus struct {
	InProgress bool
	Connected  bool
}

// AuthSession holds the transient in-memory state for a single auth flow.
type AuthSession struct {
	Token     string
	Phone     string
	CodeHash  string
	CreatedAt time.Time

	// Async communication channels. Never close data channels directly — use Done.
	CodeChan     chan AuthCode
	PasswordChan chan string
	ResultChan   chan AuthResult
	Done         chan struct{} // closed on cleanup — signals all select cases
	Cancel       context.CancelFunc
	Timer        *time.Timer
	Step         string // "awaiting_code", "awaiting_password" — tracks expected input
}

// AuthSessionManager manages the in-memory auth flow with a 5-minute TTL.
type AuthSessionManager struct {
	mu          sync.Mutex
	session     *AuthSession
	ttl         time.Duration
	sessionRepo *repository.TelegramSessionRepository
	encryptor   *crypto.TokenEncryptor
	apiID       int
	apiHash     string
	onComplete  func(ctx context.Context) error // called after successful auth
}

// NewAuthSessionManager creates a new auth session manager.
func NewAuthSessionManager(
	sessionRepo *repository.TelegramSessionRepository,
	encryptor *crypto.TokenEncryptor,
	apiID int,
	apiHash string,
	ttl time.Duration,
	onComplete func(ctx context.Context) error,
) *AuthSessionManager {
	return &AuthSessionManager{
		sessionRepo: sessionRepo,
		encryptor:   encryptor,
		apiID:       apiID,
		apiHash:     apiHash,
		ttl:         ttl,
		onComplete:  onComplete,
	}
}

// StartAuth begins a new auth flow. Returns the auth token and the initial result
// (containing CodeType from gotd's SentCode response).
func (m *AuthSessionManager) StartAuth(ctx context.Context, phone string) (string, *AuthResult, error) {
	// Check if already connected — prevent re-auth that could overwrite the active session
	existingSess, err := m.sessionRepo.GetSession(ctx)
	if err == nil && existingSess.AuthState == "connected" {
		return "", nil, ErrAlreadyConnected
	} else if err != nil && !errors.Is(err, db.ErrNotFound) {
		return "", nil, fmt.Errorf("%w: check existing session: %w", ErrAuthInternal, err)
	}

	m.mu.Lock()
	if m.session != nil {
		m.mu.Unlock()
		return "", nil, ErrAuthInProgress
	}

	token, err := generateToken()
	if err != nil {
		m.mu.Unlock()
		return "", nil, fmt.Errorf("generate token: %w", err)
	}

	clientCtx, cancel := context.WithCancel(context.Background())

	sess := &AuthSession{
		Token:        token,
		Phone:        phone,
		CreatedAt:    accelerated.GetCurrentTime(),
		CodeChan:     make(chan AuthCode, 1),
		PasswordChan: make(chan string, 1),
		ResultChan:   make(chan AuthResult, 1),
		Done:         make(chan struct{}),
		Cancel:       cancel,
	}
	m.session = sess
	m.mu.Unlock()

	// TTL timer — fires after ttl, cleans up if auth didn't complete
	sess.Timer = time.AfterFunc(m.ttl, func() {
		m.cleanup(sess)
	})

	// Create session storage for this auth flow
	sessionStorage := NewDatabaseSessionStorage(m.sessionRepo, m.encryptor)

	client := telegram.NewClient(m.apiID, m.apiHash, telegram.Options{
		SessionStorage: sessionStorage,
	})

	// Start the client in a goroutine. Run blocks until ctx is cancelled.
	go func() {
		runErr := client.Run(clientCtx, func(runCtx context.Context) error {
			authClient := auth.NewClient(tg.NewClient(client), rand.Reader, m.apiID, m.apiHash)
			sendCtx, sendCancel := context.WithTimeout(runCtx, 30*time.Second)
			sentCode, err := authClient.SendCode(sendCtx, phone, auth.SendCodeOptions{})
			sendCancel()
			if err != nil {
				return fmt.Errorf("send code: %w", err)
			}

			// Extract code hash and code type
			var codeHash string
			var codeType string
			switch sc := sentCode.(type) {
			case *tg.AuthSentCode:
				codeHash = sc.PhoneCodeHash
				codeType = mapSentCodeType(sc.Type)
			default:
				return fmt.Errorf("unexpected sent code type: %T", sentCode)
			}

			// Store code hash and send initial result
			m.mu.Lock()
			if m.session != nil && m.session.Token == token {
				m.session.CodeHash = codeHash
			}
			m.mu.Unlock()

			m.mu.Lock()
			sess.Step = "awaiting_code"
			m.mu.Unlock()
			sess.ResultChan <- AuthResult{Status: "awaiting_code", CodeType: codeType}

			// Code entry loop — allows retry on invalid code within TTL
			var authResultTG *tg.AuthAuthorization
			for {
				var code AuthCode
				select {
				case code = <-sess.CodeChan:
				case <-sess.Done:
					return nil
				case <-runCtx.Done():
					return runCtx.Err()
				}

				signInCtx, signInCancel := context.WithTimeout(runCtx, 30*time.Second)
				result, signInErr := authClient.SignIn(signInCtx, phone, code.Code, codeHash)
				signInCancel()
				if signInErr == nil {
					authResultTG = result
					break
				}
				if errors.Is(signInErr, auth.ErrPasswordAuthNeeded) {
					// 2FA required — enter password loop
					m.mu.Lock()
					sess.Step = "awaiting_password"
					m.mu.Unlock()
					sess.ResultChan <- AuthResult{Status: "awaiting_password"}

					for {
						var password string
						select {
						case password = <-sess.PasswordChan:
						case <-sess.Done:
							return nil
						case <-runCtx.Done():
							return runCtx.Err()
						}

						pwCtx, pwCancel := context.WithTimeout(runCtx, 30*time.Second)
						pwResult, pwErr := authClient.Password(pwCtx, password)
						pwCancel()
						if pwErr == nil {
							authResultTG = pwResult
							break
						}
						classified := classifyAuthError(pwErr)
						if errors.Is(classified, ErrAuthInternal) {
							sess.ResultChan <- AuthResult{Status: "error", Error: classified}
							return nil
						}
						// Invalid password — allow retry
						sess.ResultChan <- AuthResult{Status: "error", Error: classified}
					}
					break
				}

				classified := classifyAuthError(signInErr)
				if errors.Is(classified, ErrAuthInternal) {
					sess.ResultChan <- AuthResult{Status: "error", Error: classified}
					return nil
				}
				// Invalid code — allow retry
				sess.ResultChan <- AuthResult{Status: "error", Error: classified}
			}

			// Auth successful
			var userID int64
			var username string
			if user, ok := authResultTG.User.(*tg.User); ok {
				userID = user.ID
				username = user.Username
			}

			// Update session with user info and mark as connected
			phonePtr := &phone
			if _, err := m.sessionRepo.UpdateUserInfo(runCtx, repository.UpdateTelegramUserInfoParams{
				TelegramUserID: &userID,
				Username:       &username,
				PhoneNumber:    phonePtr,
			}); err != nil {
				sess.ResultChan <- AuthResult{Status: "error", Error: fmt.Errorf("%w: persist user info: %w", ErrAuthInternal, err)}
				return nil
			}
			if _, err := m.sessionRepo.UpdateAuthState(runCtx, "connected"); err != nil {
				sess.ResultChan <- AuthResult{Status: "error", Error: fmt.Errorf("%w: persist auth state: %w", ErrAuthInternal, err)}
				return nil
			}

			sess.ResultChan <- AuthResult{
				Status:   "connected",
				UserID:   userID,
				Username: username,
			}

			return nil
		})
		if runErr != nil && !errors.Is(runErr, context.Canceled) {
			select {
			case sess.ResultChan <- AuthResult{Status: "error", Error: runErr}:
			case <-sess.Done:
			}
		}
	}()

	// Wait for the first result (should be awaiting_code or error)
	select {
	case result := <-sess.ResultChan:
		if result.Status == "error" {
			m.cleanup(sess)
			return "", nil, result.Error
		}
		return token, &result, nil
	case <-ctx.Done():
		m.cleanup(sess)
		return "", nil, ctx.Err()
	}
}

// VerifyCode submits the verification code for the active auth flow.
func (m *AuthSessionManager) VerifyCode(token string, code string) (*AuthResult, error) {
	m.mu.Lock()
	sess := m.session
	if sess == nil || sess.Token != token {
		m.mu.Unlock()
		return nil, ErrAuthTokenInvalid
	}
	if sess.Step != "awaiting_code" {
		m.mu.Unlock()
		return nil, ErrAuthTokenInvalid
	}
	m.mu.Unlock()

	// Check if session is already done (TTL expired)
	select {
	case <-sess.Done:
		return nil, ErrAuthTokenExpired
	default:
	}

	// Send code to the Run callback
	select {
	case sess.CodeChan <- AuthCode{Code: code}:
	case <-sess.Done:
		return nil, ErrAuthTokenExpired
	}

	// Wait for result
	select {
	case result := <-sess.ResultChan:
		switch result.Status {
		case "connected":
			sess.Timer.Stop()
			if m.onComplete != nil {
				if err := m.onComplete(context.Background()); err != nil {
					log.Warn().Err(err).Msg("telegram: onComplete failed after successful auth")
				}
			}
			m.clearSession(sess)
		case "error":
			if errors.Is(result.Error, ErrAuthInternal) {
				m.cleanup(sess)
			}
			// Don't cleanup on auth rejection (invalid code) — allow retry within TTL
			return nil, result.Error
		}
		return &result, nil
	case <-sess.Done:
		return nil, ErrAuthTokenExpired
	}
}

// VerifyPassword submits the 2FA password for the active auth flow.
func (m *AuthSessionManager) VerifyPassword(token string, password string) (*AuthResult, error) {
	m.mu.Lock()
	sess := m.session
	if sess == nil || sess.Token != token {
		m.mu.Unlock()
		return nil, ErrAuthTokenInvalid
	}
	if sess.Step != "awaiting_password" {
		m.mu.Unlock()
		return nil, ErrAuthTokenInvalid
	}
	m.mu.Unlock()

	select {
	case <-sess.Done:
		return nil, ErrAuthTokenExpired
	default:
	}

	select {
	case sess.PasswordChan <- password:
	case <-sess.Done:
		return nil, ErrAuthTokenExpired
	}

	select {
	case result := <-sess.ResultChan:
		switch result.Status {
		case "connected":
			sess.Timer.Stop()
			if m.onComplete != nil {
				if err := m.onComplete(context.Background()); err != nil {
					log.Warn().Err(err).Msg("telegram: onComplete failed after successful auth")
				}
			}
			m.clearSession(sess)
		case "error":
			if errors.Is(result.Error, ErrAuthInternal) {
				m.cleanup(sess)
			}
			return nil, result.Error
		}
		return &result, nil
	case <-sess.Done:
		return nil, ErrAuthTokenExpired
	}
}

// GetStatus returns the current auth manager status.
func (m *AuthSessionManager) GetStatus() AuthStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	return AuthStatus{
		InProgress: m.session != nil,
	}
}

// cleanup cancels the auth flow and clears all state.
func (m *AuthSessionManager) cleanup(sess *AuthSession) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.session != sess {
		return // already cleaned up or replaced
	}

	// 1. Close Done channel — signals all selects
	select {
	case <-sess.Done:
	default:
		close(sess.Done)
	}

	// 2. Cancel the client context
	sess.Cancel()

	// 3. Stop the TTL timer
	sess.Timer.Stop()

	// 4. Delete any partially-written session row left by StoreSession during key exchange
	if err := m.sessionRepo.DeleteSession(context.Background()); err != nil {
		log.Warn().Err(err).Msg("telegram: failed to delete partial session during auth cleanup")
	}

	// 5. Clear in-memory session
	m.session = nil
}

// clearSession clears the session without cancelling (for successful auth completion).
func (m *AuthSessionManager) clearSession(sess *AuthSession) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.session != sess {
		return
	}
	m.session = nil
}

// CancelAuth cancels any in-progress auth flow.
func (m *AuthSessionManager) CancelAuth() {
	m.mu.Lock()
	sess := m.session
	m.mu.Unlock()
	if sess != nil {
		m.cleanup(sess)
	}
}

// classifyAuthError wraps non-auth-rejection errors with ErrAuthInternal.
// Known Telegram auth rejections (invalid code, invalid password) are returned as-is
// so the handler can return 401. Infrastructure/transport errors get wrapped as 500.
func classifyAuthError(err error) error {
	if errors.Is(err, auth.ErrPasswordInvalid) {
		return err // Telegram rejected the password — user error
	}
	// gotd returns errors with RPC error codes as part of the error message.
	// Check for common auth rejection patterns.
	msg := err.Error()
	for _, pattern := range []string{
		"PHONE_CODE_INVALID",
		"PHONE_CODE_EXPIRED",
		"PASSWORD_HASH_INVALID",
		"SESSION_PASSWORD_NEEDED",
	} {
		if strings.Contains(msg, pattern) {
			return err // Telegram auth rejection — user error
		}
	}
	// Everything else is infrastructure — wrap as internal
	return fmt.Errorf("%w: %w", ErrAuthInternal, err)
}

func generateToken() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func mapSentCodeType(t tg.AuthSentCodeTypeClass) string {
	switch t.(type) {
	case *tg.AuthSentCodeTypeApp:
		return "app"
	case *tg.AuthSentCodeTypeSMS:
		return "sms"
	case *tg.AuthSentCodeTypeCall:
		return "call"
	default:
		return "unknown"
	}
}
