package telegram

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/crypto"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/updates"
	"github.com/gotd/td/tg"
	"github.com/rs/zerolog/log"
)

const authTTL = 5 * time.Minute

// TelegramStatus represents the current connection status.
type TelegramStatus struct {
	Connected          bool       `json:"connected"`
	Username           *string    `json:"username,omitempty"`
	PhoneNumber        *string    `json:"phone_number,omitempty"`
	LastSyncAt         *time.Time `json:"last_sync_at,omitempty"`
	Error              *string    `json:"error,omitempty"`
	ConnectedAt        *time.Time `json:"connected_at,omitempty"`
	BackfillInProgress bool       `json:"backfill_in_progress,omitempty"`
	BackfillTotal      int        `json:"backfill_total,omitempty"`
	BackfillCompleted  int        `json:"backfill_completed,omitempty"`
}

// ChatWithTracking wraps a chat config with the computed effective_tracked flag.
type ChatWithTracking struct {
	repository.TelegramChatConfig
	EffectiveTracked bool
}

// TelegramManager orchestrates the long-lived MTProto connection lifecycle.
type TelegramManager struct {
	sessionRepo     *repository.TelegramSessionRepository
	updateStateRepo *repository.TelegramUpdateStateRepository
	chatConfigRepo  *repository.TelegramChatConfigRepository
	messageRepo     *repository.TelegramMessageRepository
	syncRepo        *repository.SyncRepository
	encryptor       *crypto.TokenEncryptor
	apiID           int
	apiHash         string
	cfg             *config.TelegramConfig

	authManager *AuthSessionManager

	mu           sync.Mutex
	client       *telegram.Client
	clientCtx    context.Context
	cancel       context.CancelFunc
	running      bool
	status       TelegramStatus
	startupErr   error
	disconnected bool       // set on explicit Disconnect — prevents stale DB fallback
	syncStateID  *uuid.UUID // cached sync state row UUID

	backfillMu         sync.Mutex
	backfillInProgress bool
	backfillTotal      int
	backfillCompleted  int
}

// NewTelegramManager creates the manager and its embedded AuthSessionManager.
func NewTelegramManager(
	sessionRepo *repository.TelegramSessionRepository,
	updateStateRepo *repository.TelegramUpdateStateRepository,
	chatConfigRepo *repository.TelegramChatConfigRepository,
	messageRepo *repository.TelegramMessageRepository,
	syncRepo *repository.SyncRepository,
	encryptor *crypto.TokenEncryptor,
	apiID int,
	apiHash string,
	cfg *config.TelegramConfig,
) *TelegramManager {
	m := &TelegramManager{
		sessionRepo:     sessionRepo,
		updateStateRepo: updateStateRepo,
		chatConfigRepo:  chatConfigRepo,
		messageRepo:     messageRepo,
		syncRepo:        syncRepo,
		encryptor:       encryptor,
		apiID:           apiID,
		apiHash:         apiHash,
		cfg:             cfg,
	}

	m.authManager = NewAuthSessionManager(
		sessionRepo,
		encryptor,
		apiID,
		apiHash,
		authTTL,
		m.OnAuthComplete,
	)

	return m
}

// Start loads the session from DB and starts the MTProto connection if authenticated.
func (m *TelegramManager) Start(ctx context.Context) error {
	// Load existing sync state UUID (if any) for later updates
	syncState, err := m.syncRepo.GetSyncStateBySource(ctx, "telegram", nil)
	if err == nil {
		m.syncStateID = &syncState.ID
	} else if !errors.Is(err, db.ErrNotFound) {
		return fmt.Errorf("load telegram sync state: %w", err)
	}

	// Check if we have a connected session
	sess, err := m.sessionRepo.GetSession(ctx)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			log.Info().Msg("telegram: no session found, skipping connection")
			return nil
		}
		return fmt.Errorf("load telegram session: %w", err)
	}
	if sess.AuthState != "connected" {
		log.Info().Msg("telegram: session not connected, skipping connection")
		return nil
	}

	return m.startConnection(ctx)
}

func (m *TelegramManager) startConnection(ctx context.Context) error {
	m.mu.Lock()
	if m.running {
		m.mu.Unlock()
		return nil
	}
	m.mu.Unlock()

	sessionStorage := NewDatabaseSessionStorage(m.sessionRepo, m.encryptor)
	stateStorage := NewPostgresStateStorage(m.updateStateRepo)

	clientCtx, cancel := context.WithCancel(context.Background())

	m.mu.Lock()
	m.clientCtx = clientCtx
	m.cancel = cancel
	m.running = true
	m.startupErr = nil
	m.disconnected = false // clear on reconnect
	m.mu.Unlock()

	go m.runWithReconnect(clientCtx, cancel, sessionStorage, stateStorage)

	return nil
}

const (
	reconnectBaseDelay = 5 * time.Second
	reconnectMaxDelay  = 5 * time.Minute
)

// runWithReconnect runs the MTProto client with automatic reconnection on failure.
func (m *TelegramManager) runWithReconnect(clientCtx context.Context, cancel context.CancelFunc, sessionStorage *DatabaseSessionStorage, stateStorage *PostgresStateStorage) {
	// Resolve selfUserID from stored session
	var selfUserID int64
	if sess, err := m.sessionRepo.GetSession(clientCtx); err == nil && sess.TelegramUserID != nil {
		selfUserID = *sess.TelegramUserID
	}

	// Create message handler
	handler := NewMessageHandler(
		m.messageRepo,
		m.chatConfigRepo,
		m.syncRepo,
		m.syncStateID,
		selfUserID,
		m.cfg.GroupMaxMembers,
	)

	// Create dispatcher with real handlers
	dispatcher := tg.NewUpdateDispatcher()
	dispatcher.OnNewMessage(handler.HandleNewMessage)
	dispatcher.OnEditMessage(handler.HandleEditMessage)
	dispatcher.OnDeleteMessages(handler.HandleDeleteMessages)
	dispatcher.OnChatParticipant(handler.HandleChatParticipant)

	delay := reconnectBaseDelay
	for {
		client := telegram.NewClient(m.apiID, m.apiHash, telegram.Options{
			SessionStorage: sessionStorage,
			UpdateHandler: updates.New(updates.Config{
				Handler:      dispatcher,
				Storage:      stateStorage,
				AccessHasher: NewPostgresChannelHasher(m.updateStateRepo),
			}),
		})

		m.mu.Lock()
		m.client = client
		m.mu.Unlock()

		runErr := client.Run(clientCtx, func(runCtx context.Context) error {
			// Give handler access to the API client (valid for this Run callback's lifetime)
			handler.SetAPI(tg.NewClient(client))

			m.mu.Lock()
			now := accelerated.GetCurrentTime()
			m.status = TelegramStatus{Connected: true, ConnectedAt: &now}
			m.mu.Unlock()

			if sess, err := m.sessionRepo.GetSession(runCtx); err == nil {
				m.mu.Lock()
				m.status.Username = sess.Username
				m.status.PhoneNumber = sess.PhoneNumber
				m.mu.Unlock()
			}

			m.updateSyncStatus(runCtx, repository.SyncStatusIdle, nil)
			log.Info().Msg("telegram: connection established")
			delay = reconnectBaseDelay // reset on successful connect

			// Trigger backfill if needed
			m.maybeStartBackfill(runCtx, client, handler)

			<-runCtx.Done()
			return runCtx.Err()
		})

		// If context was cancelled (clean shutdown/disconnect), exit the loop
		if errors.Is(runErr, context.Canceled) {
			m.mu.Lock()
			m.running = false
			m.status = TelegramStatus{Connected: false}
			m.mu.Unlock()
			return
		}

		// Connection error — log and attempt reconnect
		errMsg := ""
		if runErr != nil {
			errMsg = runErr.Error()
		}
		m.mu.Lock()
		m.status = TelegramStatus{Connected: false, Error: &errMsg}
		m.mu.Unlock()
		m.updateSyncStatus(context.Background(), repository.SyncStatusError, &errMsg)
		log.Warn().Err(runErr).Dur("retry_in", delay).Msg("telegram: connection lost, will reconnect")

		// Wait before reconnecting, respecting context cancellation
		select {
		case <-time.After(delay):
			delay = min(delay*2, reconnectMaxDelay)
		case <-clientCtx.Done():
			m.mu.Lock()
			m.running = false
			m.status = TelegramStatus{Connected: false}
			m.mu.Unlock()
			return
		}
	}
}

// Stop cancels the context and waits for the client to exit.
func (m *TelegramManager) Stop() {
	m.mu.Lock()
	cancel := m.cancel
	m.mu.Unlock()

	if cancel != nil {
		cancel()
	}
}

// Status returns the current connection status.
func (m *TelegramManager) Status() TelegramStatus {
	m.mu.Lock()
	defer m.mu.Unlock()

	// If running, return in-memory status enriched with sync state data
	if m.running {
		status := m.status
		m.enrichStatusFromSyncState(&status)
		m.backfillMu.Lock()
		status.BackfillInProgress = m.backfillInProgress
		status.BackfillTotal = m.backfillTotal
		status.BackfillCompleted = m.backfillCompleted
		m.backfillMu.Unlock()
		return status
	}

	// If startup failed, return the error
	if m.startupErr != nil {
		errMsg := m.startupErr.Error()
		return TelegramStatus{Connected: false, Error: &errMsg}
	}

	// If explicitly disconnected, don't fall back to stale DB state
	if m.disconnected {
		return TelegramStatus{Connected: false}
	}

	// Fall back to DB state for test/fresh-boot scenarios
	sess, err := m.sessionRepo.GetSession(context.Background())
	if err != nil {
		return TelegramStatus{Connected: false}
	}
	status := TelegramStatus{
		Connected:   sess.AuthState == "connected",
		Username:    sess.Username,
		PhoneNumber: sess.PhoneNumber,
	}
	m.enrichStatusFromSyncState(&status)
	return status
}

// enrichStatusFromSyncState reads last_sync_at from the external_sync_state row.
func (m *TelegramManager) enrichStatusFromSyncState(status *TelegramStatus) {
	if m.syncStateID == nil {
		return
	}
	syncState, err := m.syncRepo.GetSyncState(context.Background(), *m.syncStateID)
	if err != nil {
		return
	}
	status.LastSyncAt = syncState.LastSyncAt
}

// IsConnected returns whether the connection is active.
func (m *TelegramManager) IsConnected() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.running && m.status.Connected
}

// AuthManager returns the auth session manager for the HTTP handler.
func (m *TelegramManager) AuthManager() *AuthSessionManager {
	return m.authManager
}

// OnAuthComplete is called after successful auth to start the connection.
func (m *TelegramManager) OnAuthComplete(ctx context.Context) error {
	// Ensure sync state row exists with Enabled: false
	if m.syncStateID == nil {
		state, err := m.syncRepo.CreateSyncState(ctx, repository.CreateSyncStateRequest{
			Source:   "telegram",
			Enabled:  false,
			Status:   repository.SyncStatusIdle,
			Strategy: repository.SyncStrategyFetchAll,
		})
		if err != nil {
			log.Warn().Err(err).Msg("telegram: failed to create sync state")
		} else {
			m.syncStateID = &state.ID
		}
	}

	return m.startConnection(ctx)
}

// Disconnect stops the connection, clears the session, and removes sync state.
func (m *TelegramManager) Disconnect(ctx context.Context) error {
	// 1. Cancel any in-progress auth
	m.authManager.CancelAuth()

	// 2. Stop the live MTProto client (but don't commit in-memory state yet)
	m.mu.Lock()
	cancel := m.cancel
	syncStateID := m.syncStateID
	m.mu.Unlock()

	if cancel != nil {
		cancel()
	}

	// 3. Delete telegram session from DB (critical — must succeed)
	if err := m.sessionRepo.DeleteSession(ctx); err != nil {
		// Session deletion failed — connection is already stopped but DB still has
		// the row. Don't set disconnected=true so Status() still reflects the DB state.
		// User can retry the disconnect.
		m.mu.Lock()
		m.cancel = nil
		m.running = false
		m.client = nil
		errMsg := "disconnect failed: session still in database"
		m.status = TelegramStatus{Connected: false, Error: &errMsg}
		m.mu.Unlock()
		return fmt.Errorf("delete telegram session: %w", err)
	}

	// 4. Commit in-memory teardown (session successfully deleted)
	m.mu.Lock()
	m.cancel = nil
	m.running = false
	m.client = nil
	m.syncStateID = nil
	m.status = TelegramStatus{Connected: false}
	m.startupErr = nil
	m.disconnected = true
	m.mu.Unlock()

	// 5. Delete sync state row (best-effort — non-critical)
	if syncStateID != nil {
		if err := m.syncRepo.DeleteSyncState(ctx, *syncStateID); err != nil {
			log.Warn().Err(err).Msg("telegram: failed to delete sync state")
		}
	}

	log.Info().Msg("telegram: disconnected")
	return nil
}

// ListChats returns all group chat configs with effective_tracked computed.
func (m *TelegramManager) ListChats(ctx context.Context) ([]ChatWithTracking, error) {
	cfgs, err := m.chatConfigRepo.ListConfigs(ctx)
	if err != nil {
		return nil, err
	}
	result := make([]ChatWithTracking, 0, len(cfgs))
	for _, cfg := range cfgs {
		if cfg.ChatType == "private" {
			continue // only group chats
		}
		result = append(result, ChatWithTracking{
			TelegramChatConfig: cfg,
			EffectiveTracked:   EffectiveTracked(cfg.Status, cfg.MemberCount, m.cfg.GroupMaxMembers),
		})
	}
	return result, nil
}

// GetChatStatus returns the current status string for a chat, or empty if not found.
func (m *TelegramManager) GetChatStatus(ctx context.Context, chatID int64) (string, error) {
	cfg, err := m.chatConfigRepo.GetConfig(ctx, chatID)
	if err != nil {
		return "", err
	}
	return cfg.Status, nil
}

// UpdateChatStatus updates a chat's status and returns the updated config.
func (m *TelegramManager) UpdateChatStatus(ctx context.Context, chatID int64, status string) (*ChatWithTracking, error) {
	cfg, err := m.chatConfigRepo.UpdateStatus(ctx, chatID, status)
	if err != nil {
		return nil, err
	}
	result := &ChatWithTracking{
		TelegramChatConfig: *cfg,
		EffectiveTracked:   EffectiveTracked(cfg.Status, cfg.MemberCount, m.cfg.GroupMaxMembers),
	}
	return result, nil
}

// TriggerChatBackfill resets backfill state and starts async backfill for a chat.
func (m *TelegramManager) TriggerChatBackfill(ctx context.Context, telegramChatID int64) error {
	if err := m.chatConfigRepo.ResetBackfill(ctx, telegramChatID); err != nil {
		return fmt.Errorf("reset backfill: %w", err)
	}

	m.mu.Lock()
	isRunning := m.running
	client := m.client
	clientCtx := m.clientCtx
	m.mu.Unlock()

	if isRunning && client != nil && clientCtx != nil {
		go func() {
			backfiller := NewBackfiller(
				tg.NewClient(client),
				m.messageRepo,
				m.chatConfigRepo,
				m.syncRepo,
				m.syncStateID,
				m.selfUserID(clientCtx),
				m.cfg.GroupMaxMembers,
				m.cfg.BackfillSince,
				func(total, completed int) {
					m.backfillMu.Lock()
					m.backfillInProgress = completed < total
					m.backfillTotal = total
					m.backfillCompleted = completed
					m.backfillMu.Unlock()
				},
			)
			if err := backfiller.BackfillChat(clientCtx, telegramChatID); err != nil {
				log.Warn().Err(err).Int64("chat_id", telegramChatID).Msg("telegram: retroactive backfill failed")
			}
			m.backfillMu.Lock()
			m.backfillInProgress = false
			m.backfillMu.Unlock()
		}()
	}
	return nil
}

func (m *TelegramManager) selfUserID(ctx context.Context) int64 {
	sess, err := m.sessionRepo.GetSession(ctx)
	if err != nil || sess.TelegramUserID == nil {
		return 0
	}
	return *sess.TelegramUserID
}

// maybeStartBackfill checks if backfill is needed and starts it in a goroutine.
func (m *TelegramManager) maybeStartBackfill(ctx context.Context, client *telegram.Client, handler *MessageHandler) {
	chats, err := m.chatConfigRepo.ListForBackfill(ctx)
	if err != nil {
		log.Warn().Err(err).Msg("telegram: failed to check backfill state")
		return
	}

	// If no chats exist at all, this is first connect — backfill will discover them
	allConfigs, err := m.chatConfigRepo.ListConfigs(ctx)
	if err != nil {
		log.Warn().Err(err).Msg("telegram: failed to list configs for backfill check")
		return
	}
	needsBackfill := len(chats) > 0 || len(allConfigs) == 0

	if !needsBackfill {
		return
	}

	go func() {
		backfiller := NewBackfiller(
			tg.NewClient(client),
			m.messageRepo,
			m.chatConfigRepo,
			m.syncRepo,
			m.syncStateID,
			m.selfUserID(ctx),
			m.cfg.GroupMaxMembers,
			m.cfg.BackfillSince,
			func(total, completed int) {
				m.backfillMu.Lock()
				m.backfillInProgress = completed < total
				m.backfillTotal = total
				m.backfillCompleted = completed
				m.backfillMu.Unlock()
			},
		)
		if err := backfiller.Run(ctx); err != nil {
			log.Warn().Err(err).Msg("telegram: backfill failed")
		}
		m.backfillMu.Lock()
		m.backfillInProgress = false
		m.backfillMu.Unlock()
	}()
}

// updateSyncStatus updates the external_sync_state row for Telegram.
func (m *TelegramManager) updateSyncStatus(ctx context.Context, status repository.SyncStatus, errMsg *string) {
	if m.syncStateID == nil {
		return
	}
	_, err := m.syncRepo.UpdateSyncStateStatus(ctx, *m.syncStateID, status, errMsg)
	if err != nil {
		log.Warn().Err(err).Msg("telegram: failed to update sync state status")
	}
}
