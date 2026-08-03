package whatsapp

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/logger"
	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
	"go.mau.fi/whatsmeow"
	waProto "go.mau.fi/whatsmeow/proto/waCompanionReg"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"
)

const (
	// authTTL bounds an in-flight pairing, mirroring Telegram's auth TTL.
	authTTL = 5 * time.Minute

	// qrFirstCodeTimeout bounds how long StartPairing waits for the first QR
	// code before giving up. The API contract promises a code in the response,
	// so the wait has to be bounded somewhere.
	qrFirstCodeTimeout = 10 * time.Second

	// pairClientDisplayName must match the server-validated "Browser (OS)"
	// shape. The library documents that the specific browser does not matter,
	// only the shape, so a branded string would simply be rejected.
	pairClientDisplayName = "Chrome (Linux)"

	// deviceOSName is what the linked device shows on the phone's Linked
	// Devices screen.
	deviceOSName = "Personal CRM"

	// metadataTerminalReason and metadataBannedUntil persist the
	// permanent-disconnect decision so a restart does not undo it.
	metadataTerminalReason = "terminal_reason"
	metadataBannedUntil    = "banned_until"
)

// syncSource is the external_sync_state source string. It is also the
// comms_message source and the aggregation ref prefix.
const syncSource = repository.InteractionSourceWhatsApp

// terminalReasons is the set of reasons that must survive a restart. Start()
// consults it before deciding to connect: the terminal decision lives in the
// database, not only in memory.
var terminalReasons = map[string]struct{}{
	ReasonLoggedOut:      {},
	ReasonStreamReplaced: {},
	ReasonClientOutdated: {},
	ReasonTemporaryBan:   {},
}

// deviceProps are process-wide (store.DeviceProps is a package-level global in
// whatsmeow), so they are applied exactly once, from the feature-gated manager
// constructor, and never again.
var deviceProps sync.Once

// syncStateWriter is the slice of repository.SyncRepository the manager uses.
// Narrowed to a seam so the event-handling unit tests can observe what was
// persisted and in what order.
type syncStateWriter interface {
	GetSyncStateBySource(ctx context.Context, source string, accountID *string) (*repository.SyncState, error)
	CreateSyncState(ctx context.Context, req repository.CreateSyncStateRequest) (*repository.SyncState, error)
	UpdateSyncStateStatus(ctx context.Context, id uuid.UUID, status repository.SyncStatus, errorMessage *string) (*repository.SyncState, error)
	UpdateSyncStateMetadata(ctx context.Context, id uuid.UUID, metadata map[string]any) (*repository.SyncState, error)
}

// backfillReader is the slice of repository.WhatsAppRepository the status
// endpoint reads.
type backfillReader interface {
	CountByStateAndDisposition(ctx context.Context) (map[string]int, error)
	ObservedFloor(ctx context.Context) (*time.Time, error)
}

// sessionClient is the slice of *whatsmeow.Client the lifecycle and pairing
// paths use. Production is always the real client; tests substitute a fake so
// no unit test dials WhatsApp.
type sessionClient interface {
	Connect() error
	Disconnect()
	IsConnected() bool
	IsLoggedIn() bool
	Logout(ctx context.Context) error
	GetQRChannel(ctx context.Context) (<-chan whatsmeow.QRChannelItem, error)
	PairPhone(ctx context.Context, phone string, showPushNotification bool, clientType whatsmeow.PairClientType, clientDisplayName string) (string, error)
}

var _ sessionClient = (*whatsmeow.Client)(nil)

// session bundles a client with the device it was built over.
type session struct {
	client sessionClient
	// wa is the real client when production built this session, and nil when a
	// test fake did. HistoryFetcher() is available only when it is non-nil,
	// because the three history calls have no seam of their own.
	wa *whatsmeow.Client
	// deleteDevice removes the device row. Used when a pairing is cancelled
	// (the partially written device) and on a confirmed unlink.
	deleteDevice func(ctx context.Context) error
	// paired reports whether the device carried an ID when the session was
	// built.
	paired bool
}

// Manager owns the whatsmeow client lifecycle, the pairing state machine, and
// the durable capture of history-sync notifications.
type Manager struct {
	container *sqlstore.Container
	log       waLog.Logger
	cfg       *config.WhatsAppConfig
	syncRepo  syncStateWriter
	waRepo    backfillReader

	// newSession is the client-construction seam. fresh=true asks for a brand
	// new unpaired device (pairing); fresh=false loads the stored one.
	newSession func(ctx context.Context, fresh bool) (*session, error)

	mu          sync.RWMutex
	sess        *session
	status      Status
	pairing     *pairingState
	syncStateID *uuid.UUID

	ingestor          MessageIngestor
	historyRecorder   HistoryNotificationRecorder
	historyDrainReady bool
}

// NewManager builds the manager and applies the process-wide history-request
// device props. It does not connect: Start() decides that, and only once the
// readiness gate is satisfied.
func NewManager(
	container *sqlstore.Container,
	log waLog.Logger,
	cfg *config.WhatsAppConfig,
	syncRepo syncStateWriter,
	waRepo backfillReader,
) *Manager {
	applyDeviceProps()

	m := &Manager{
		container: container,
		log:       log,
		cfg:       cfg,
		syncRepo:  syncRepo,
		waRepo:    waRepo,
		ingestor:  refusingIngestor{},
		status:    Status{Configured: true, State: StateNotReady, Reason: ReasonIngestNotWired},
	}
	m.newSession = m.newContainerSession
	return m
}

// applyDeviceProps sets the history-request registration payload. These values
// are baked into the pairing registration and CANNOT be widened afterwards
// without unlinking and re-pairing, which is why the window is a constant
// rather than an environment knob.
//
// InlineInitialPayloadInE2EeMsg is the one that defaults to the WRONG value:
// the library asks for the bootstrap chunk to be delivered with history
// embedded in the protocol message, and such a chunk is dropped un-projected
// because persisting it would store pre-clamp content.
func applyDeviceProps() {
	deviceProps.Do(func() {
		store.DeviceProps.Os = proto.String(deviceOSName)
		store.DeviceProps.PlatformType = waProto.DeviceProps_DESKTOP.Enum()
		store.DeviceProps.RequireFullSync = proto.Bool(true)
		if store.DeviceProps.HistorySyncConfig == nil {
			store.DeviceProps.HistorySyncConfig = &waProto.DeviceProps_HistorySyncConfig{}
		}
		store.DeviceProps.HistorySyncConfig.FullSyncDaysLimit = proto.Uint32(HistorySyncDaysLimit)
		store.DeviceProps.HistorySyncConfig.InlineInitialPayloadInE2EeMsg = proto.Bool(false)
	})
}

// SetIngestor installs the real message ingestor. Until it is called the
// default REFUSES, which withholds the ack rather than dropping the message.
func (m *Manager) SetIngestor(i MessageIngestor) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ingestor = i
}

// SetHistoryRecorder installs the history-notification recorder. There is no
// default: a no-op here is silent, unrecoverable history loss.
func (m *Manager) SetHistoryRecorder(r HistoryNotificationRecorder) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.historyRecorder = r
}

// SetHistoryDrainReady records that the history drain worker is registered.
// This is the last of the three readiness facts, and therefore the call that
// first permits a connection.
func (m *Manager) SetHistoryDrainReady() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.historyDrainReady = true
}

// Ready reports whether the client may connect, and names the missing piece
// when it may not.
func (m *Manager) Ready() (bool, string) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.readyLocked()
}

func (m *Manager) readyLocked() (bool, string) {
	if _, isDefault := m.ingestor.(refusingIngestor); isDefault || m.ingestor == nil {
		return false, "message ingestor is not wired"
	}
	if m.historyRecorder == nil {
		return false, "history notification recorder is not wired"
	}
	if !m.historyDrainReady {
		return false, "history drain worker is not registered"
	}
	return true, ""
}

// newClient is the single private constructor used by BOTH the boot path and
// the pairing paths. Everything that makes the session safe is set here, before
// anything connects, so there is no code path that produces a connected client
// without them.
func (m *Manager) newClient(device *store.Device) *whatsmeow.Client {
	cli := whatsmeow.NewClient(device, m.log)

	// Acks fire only after our handlers return, so a crash between receipt and
	// persistence becomes a redelivery rather than a loss.
	cli.SynchronousAck = true

	// Manual history mode. Without both of these the library downloads the
	// history blob, dispatches it ignoring our result, deletes it server-side
	// on the next statement, and sends the protocol receipt behind our back —
	// none of which is recoverable if our side fails.
	cli.ManualHistorySyncDownload = true
	cli.DisableManualHistorySyncReceipt = true

	// WithSuccessStatus, not AddEventHandler: the plain variant wraps a void
	// handler and hard-codes a true return, so our false could never reach the
	// dispatcher and the withheld-ack contract would silently not exist.
	cli.AddEventHandlerWithSuccessStatus(m.handleEvent)

	return cli
}

// newContainerSession is the production seam implementation: a client over
// either a brand-new unpaired device or the stored one.
func (m *Manager) newContainerSession(ctx context.Context, fresh bool) (*session, error) {
	if m.container == nil {
		return nil, errors.New("whatsapp: no device container")
	}

	var device *store.Device
	var paired bool
	if fresh {
		device = m.container.NewDevice()
	} else {
		var err error
		device, paired, err = LoadOrCreateDevice(ctx, m.container)
		if err != nil {
			return nil, err
		}
	}

	cli := m.newClient(device)
	return &session{
		client:       cli,
		wa:           cli,
		deleteDevice: device.Delete,
		paired:       paired,
	}, nil
}

// Start brings the WhatsApp stack up. It never returns a fatal error for a
// WhatsApp-side problem: a WhatsApp failure must not abort the process boot.
func (m *Manager) Start(ctx context.Context) error {
	// 1. Readiness gate. Without a real ingestor, a recorder, and a registered
	//    drainer there is nowhere durable for an arriving message or history
	//    chunk to go, so connecting would acknowledge and discard data.
	if ready, missing := m.Ready(); !ready {
		m.setStatus(func(s *Status) {
			s.State = StateNotReady
			s.Reason = ReasonIngestNotWired
		})
		logger.Info().Str("missing", missing).Msg("whatsapp: not ready, not connecting")
		return nil
	}

	// 2. Ensure the sync-state row exists and read back what it persisted.
	terminalReason, bannedUntil := m.ensureSyncState(ctx)

	// 3. Load the device. No device means nothing to resume.
	sess, err := m.newSession(ctx, false)
	if err != nil {
		m.failStart(ctx, err)
		return nil
	}
	if !sess.paired {
		m.setStatus(func(s *Status) {
			s.State = StateNotPaired
			s.Reason = ""
		})
		logger.Info().Msg("whatsapp: no linked device, idle until paired")
		return nil
	}

	// 4. A paired device whose last decision was terminal is NOT reconnected.
	//    The decision has to survive a restart, which is why it is read from
	//    the database rather than from memory.
	if _, isTerminal := terminalReasons[terminalReason]; isTerminal {
		if terminalReason != ReasonTemporaryBan || bannedUntil == nil || bannedUntil.After(accelerated.GetCurrentTime()) {
			m.setStatus(func(s *Status) {
				s.State = StateDisconnected
				s.Reason = terminalReason
				s.BannedUntil = bannedUntil
			})
			logger.Warn().Str("reason", terminalReason).Msg("whatsapp: durable terminal state, not reconnecting")
			return nil
		}
	}

	// 5. Connect.
	m.mu.Lock()
	m.sess = sess
	m.status.State = StateConnecting
	m.status.Reason = ""
	m.mu.Unlock()

	if err := sess.client.Connect(); err != nil {
		m.failStart(ctx, err)
		return nil
	}
	logger.Info().Msg("whatsapp: connecting with stored device")
	return nil
}

// failStart records a boot failure without aborting the process.
func (m *Manager) failStart(ctx context.Context, err error) {
	m.setStatus(func(s *Status) {
		s.State = StateError
		s.Reason = err.Error()
	})
	m.updateSyncStatus(ctx, repository.SyncStatusError, err.Error())
	logger.Warn().Err(err).Msg("whatsapp: failed to start")
}

// Stop tears the client down. It never logs out: a process shutdown must not
// unlink the device.
func (m *Manager) Stop() {
	m.mu.Lock()
	sess := m.sess
	pairing := m.pairing
	m.pairing = nil
	m.mu.Unlock()

	if pairing != nil {
		pairing.cancel()
		if pairing.sess != nil {
			pairing.sess.client.Disconnect()
		}
	}
	if sess != nil {
		sess.client.Disconnect()
	}
}

// SelfJID reports the linked account's JID when one is known.
func (m *Manager) SelfJID() (types.JID, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.status.JID == nil {
		return types.EmptyJID, false
	}
	jid, err := types.ParseJID(*m.status.JID)
	if err != nil {
		return types.EmptyJID, false
	}
	return jid, true
}

// Status returns the current connection, pairing and backfill snapshot. The
// backfill counts are read outside the lock, since they hit the database.
func (m *Manager) Status() Status {
	m.mu.RLock()
	out := m.status
	if m.pairing != nil {
		p := m.pairing.snapshot()
		out.Pairing = &p
	}
	m.mu.RUnlock()

	out.Backfill = m.backfillStatus(context.Background())
	return out
}

func (m *Manager) backfillStatus(ctx context.Context) BackfillStatus {
	var out BackfillStatus
	if m.waRepo == nil {
		return out
	}
	counts, err := m.waRepo.CountByStateAndDisposition(ctx)
	if err != nil {
		logger.Warn().Err(err).Msg("whatsapp: failed to read backfill counts")
	} else {
		for key, n := range counts {
			state, disposition, ok := splitCountKey(key)
			if !ok {
				continue
			}
			switch state {
			case repository.HistoryNotificationStatePending:
				out.Pending += n
			case repository.HistoryNotificationStateProcessing:
				out.Processing += n
			case repository.HistoryNotificationStateFailed:
				out.Failed += n
			}
			if disposition == repository.HistoryDispositionDroppedInline {
				out.DroppedInlineChunks += n
			}
		}
	}

	floor, err := m.waRepo.ObservedFloor(ctx)
	if err != nil {
		logger.Warn().Err(err).Msg("whatsapp: failed to read observed backfill floor")
	} else {
		out.ObservedFloorAt = floor
	}
	return out
}

// splitCountKey splits the repository's "<state>/<disposition>" count key.
func splitCountKey(key string) (state, disposition string, ok bool) {
	for i := 0; i < len(key); i++ {
		if key[i] == '/' {
			return key[:i], key[i+1:], true
		}
	}
	return "", "", false
}

// setStatus mutates the status under the write lock.
func (m *Manager) setStatus(mutate func(*Status)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	mutate(&m.status)
}

// ensureSyncState resolves (creating if absent) the external_sync_state row and
// returns the persisted terminal reason and ban expiry, so Start can honour a
// decision taken before the last restart.
func (m *Manager) ensureSyncState(ctx context.Context) (terminalReason string, bannedUntil *time.Time) {
	if m.syncRepo == nil {
		return "", nil
	}

	state, err := m.syncRepo.GetSyncStateBySource(ctx, syncSource, nil)
	switch {
	case err == nil:
	case errors.Is(err, db.ErrNotFound):
		// Enabled: false — the row is a status carrier for the settings page
		// and the staleness watchdog, never a scheduler input. WhatsApp is
		// push-shaped: nothing polls it.
		state, err = m.syncRepo.CreateSyncState(ctx, repository.CreateSyncStateRequest{
			Source:   syncSource,
			Enabled:  false,
			Status:   repository.SyncStatusIdle,
			Strategy: repository.SyncStrategyPush,
		})
		if err != nil {
			logger.Warn().Err(err).Msg("whatsapp: failed to create sync state")
			return "", nil
		}
	default:
		logger.Warn().Err(err).Msg("whatsapp: failed to load sync state")
		return "", nil
	}

	m.mu.Lock()
	id := state.ID
	m.syncStateID = &id
	m.mu.Unlock()

	if reason, ok := state.Metadata[metadataTerminalReason].(string); ok {
		terminalReason = reason
	}
	if raw, ok := state.Metadata[metadataBannedUntil].(string); ok {
		if t, parseErr := time.Parse(time.RFC3339, raw); parseErr == nil {
			bannedUntil = &t
		}
	}
	return terminalReason, bannedUntil
}

func (m *Manager) currentSyncStateID() *uuid.UUID {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.syncStateID
}

// updateSyncStatus writes the connection status onto external_sync_state so the
// staleness watchdog and the settings staleness banner see WhatsApp for free.
func (m *Manager) updateSyncStatus(ctx context.Context, status repository.SyncStatus, errMsg string) {
	id := m.currentSyncStateID()
	if id == nil || m.syncRepo == nil {
		return
	}
	var msg *string
	if errMsg != "" {
		msg = &errMsg
	}
	if _, err := m.syncRepo.UpdateSyncStateStatus(ctx, *id, status, msg); err != nil {
		logger.Warn().Err(err).Msg("whatsapp: failed to update sync state status")
	}
}

// persistTerminalReason durably records a permanent disconnect. It must happen
// BEFORE the client is torn down, so a crash mid-teardown still leaves the
// decision recorded and the next boot does not reconnect into the same wall.
func (m *Manager) persistTerminalReason(ctx context.Context, reason string, bannedUntil *time.Time) {
	id := m.currentSyncStateID()
	if id == nil || m.syncRepo == nil {
		return
	}
	metadata := map[string]any{metadataTerminalReason: reason}
	if bannedUntil != nil {
		metadata[metadataBannedUntil] = bannedUntil.UTC().Format(time.RFC3339)
	}
	if _, err := m.syncRepo.UpdateSyncStateMetadata(ctx, *id, metadata); err != nil {
		logger.Error().Err(err).Str("reason", reason).
			Msg("whatsapp: failed to persist terminal disconnect reason; a restart may retry a dead session")
	}
}

// clearTerminalReason removes a stale terminal decision once a connection
// actually succeeds.
func (m *Manager) clearTerminalReason(ctx context.Context) {
	id := m.currentSyncStateID()
	if id == nil || m.syncRepo == nil {
		return
	}
	if _, err := m.syncRepo.UpdateSyncStateMetadata(ctx, *id, map[string]any{}); err != nil {
		logger.Warn().Err(err).Msg("whatsapp: failed to clear terminal disconnect reason")
	}
}

// handleEvent is the single event handler, registered through
// AddEventHandlerWithSuccessStatus so its false return reaches the dispatcher
// and — under SynchronousAck — withholds the stanza ack, making WhatsApp
// redeliver rather than us losing the message.
func (m *Manager) handleEvent(evt any) bool {
	ctx := context.Background()

	switch e := evt.(type) {
	case *events.Connected:
		m.onConnected(ctx)
		return true

	case *events.Disconnected:
		// whatsmeow retries transient failures on its own; there is no bespoke
		// reconnect loop here.
		m.setStatus(func(s *Status) {
			if s.State == StateConnected {
				s.State = StateReconnecting
			}
		})
		return true

	case *events.PairSuccess:
		m.onPairSuccess(ctx, e)
		return true

	case *events.LoggedOut:
		m.onTerminal(ctx, ReasonLoggedOut, nil)
		return true

	case *events.StreamReplaced:
		m.onTerminal(ctx, ReasonStreamReplaced, nil)
		return true

	case *events.ClientOutdated:
		m.onTerminal(ctx, ReasonClientOutdated, nil)
		return true

	case *events.TemporaryBan:
		until := accelerated.GetCurrentTime().Add(e.Expire)
		m.onTerminal(ctx, ReasonTemporaryBan, &until)
		return true

	case *events.Message:
		if notif := e.RawMessage.GetProtocolMessage().GetHistorySyncNotification(); notif != nil {
			return m.handleHistoryNotification(ctx, e, notif)
		}
		return m.handleMessage(ctx, e)

	case *events.HistorySync:
		// Unreachable while manual mode holds: the automatic loop that
		// dispatches this event is never started. Arriving here means the
		// manual flags were lost, which would mean the library is downloading
		// and deleting our one-shot history behind our back.
		logger.Error().Msg("whatsapp: unexpected HistorySync event — manual history flags were lost")
		return false

	default:
		logger.Debug().Str("event", fmt.Sprintf("%T", evt)).Msg("whatsapp: unhandled event")
		return true
	}
}

func (m *Manager) onConnected(ctx context.Context) {
	var jid, pushName *string
	m.mu.RLock()
	sess := m.sess
	pairingInFlight := m.pairing != nil
	m.mu.RUnlock()

	if pairingInFlight {
		// A pairing client connects to the pairing websocket before the device
		// is linked. Reporting "connected" here would tell the settings page
		// the account is live while the user is still holding an unscanned QR
		// code; PairSuccess is what actually completes the link.
		logger.Debug().Msg("whatsapp: socket connected for pairing")
		return
	}

	if sess != nil && sess.wa != nil && sess.wa.Store != nil {
		if sess.wa.Store.ID != nil {
			s := sess.wa.Store.ID.String()
			jid = &s
		}
		if sess.wa.Store.PushName != "" {
			p := sess.wa.Store.PushName
			pushName = &p
		}
	}

	now := accelerated.GetCurrentTime()
	m.setStatus(func(s *Status) {
		s.State = StateConnected
		s.Reason = ""
		s.BannedUntil = nil
		s.ConnectedAt = &now
		if jid != nil {
			s.JID = jid
			if parsed, err := types.ParseJID(*jid); err == nil && parsed.Server == types.DefaultUserServer {
				user := parsed.User
				s.PhoneNumber = &user
			}
		}
		if pushName != nil {
			s.PushName = pushName
		}
	})

	m.clearTerminalReason(ctx)
	m.updateSyncStatus(ctx, repository.SyncStatusIdle, "")
	logger.Info().Msg("whatsapp: connected")
}

func (m *Manager) onPairSuccess(ctx context.Context, e *events.PairSuccess) {
	jid := e.ID.String()
	now := accelerated.GetCurrentTime()

	m.mu.Lock()
	if m.pairing != nil {
		m.pairing.cancel()
		// The client that completed the pairing becomes the live session: it
		// already carries the event handler and the manual-history flags, and
		// whatsmeow reconnects it as the linked device.
		if m.pairing.sess != nil {
			m.pairing.sess.paired = true
			m.sess = m.pairing.sess
		}
		m.pairing = nil
	}
	m.status.State = StateConnected
	m.status.Reason = ""
	m.status.JID = &jid
	m.status.ConnectedAt = &now
	if e.ID.Server == types.DefaultUserServer {
		user := e.ID.User
		m.status.PhoneNumber = &user
	}
	if e.BusinessName != "" {
		name := e.BusinessName
		m.status.PushName = &name
	}
	m.mu.Unlock()

	m.clearTerminalReason(ctx)
	m.updateSyncStatus(ctx, repository.SyncStatusIdle, "")
	logger.Info().Msg("whatsapp: device paired")
}

// onTerminal records a permanent disconnect: the reason is persisted BEFORE the
// client is torn down, nothing is retried, and Start() will honour the decision
// after a restart.
func (m *Manager) onTerminal(ctx context.Context, reason string, bannedUntil *time.Time) {
	m.persistTerminalReason(ctx, reason, bannedUntil)

	m.mu.Lock()
	sess := m.sess
	if m.pairing != nil {
		m.pairing.cancel()
		m.pairing = nil
	}
	m.status.State = StateDisconnected
	m.status.Reason = reason
	m.status.BannedUntil = bannedUntil
	m.status.ConnectedAt = nil
	m.mu.Unlock()

	if sess != nil {
		sess.client.Disconnect()
	}
	m.updateSyncStatus(ctx, repository.SyncStatusError, reason)
	logger.Warn().Str("reason", reason).Msg("whatsapp: permanent disconnect, not reconnecting")
}

// handleMessage forwards an ordinary message to the ingestor. PR3 has no
// parser, so the seam is reached with the identity fields the manager can
// supply; the ingestor that fills it in arrives with the ingest PR.
func (m *Manager) handleMessage(ctx context.Context, e *events.Message) bool {
	m.mu.RLock()
	ingestor := m.ingestor
	m.mu.RUnlock()

	if ingestor == nil {
		logger.Error().Msg("whatsapp: no message ingestor; withholding ack")
		return false
	}

	msg := IngestedMessage{
		MessageID:  e.Info.ID,
		ChatJID:    e.Info.Chat.ToNonAD().String(),
		IsOutgoing: e.Info.IsFromMe,
		SentAt:     e.Info.Timestamp,
	}
	if err := ingestor.IngestMessage(ctx, msg); err != nil {
		logger.Error().Err(err).Str("message_id", e.Info.ID).
			Msg("whatsapp: message ingest failed; withholding ack so WhatsApp redelivers")
		return false
	}
	return true
}

// handleHistoryNotification does exactly one thing: strip any inline payload,
// decide the disposition, and record the notification synchronously. It must
// not download, project, clamp, ack, or delete — under manual mode none of
// those has happened yet, so a failure here is recoverable through redelivery
// while the media is still on the server.
func (m *Manager) handleHistoryNotification(ctx context.Context, e *events.Message, notif *waE2E.HistorySyncNotification) bool {
	m.mu.RLock()
	recorder := m.historyRecorder
	m.mu.RUnlock()

	if recorder == nil {
		logger.Error().Str("protocol_msg_id", e.Info.ID).
			Msg("whatsapp: no history recorder; withholding ack so WhatsApp redelivers")
		return false
	}

	// Strip before marshalling. Marshalling the un-stripped notification would
	// embed pre-clamp history inside the stored bytes, which is exactly what
	// the clamp rule forbids.
	stripped, ok := proto.Clone(notif).(*waE2E.HistorySyncNotification)
	if !ok {
		logger.Error().Str("protocol_msg_id", e.Info.ID).
			Msg("whatsapp: failed to clone history notification; withholding ack")
		return false
	}
	inlined := stripped.InitialHistBootstrapInlinePayload != nil
	stripped.InitialHistBootstrapInlinePayload = nil

	disposition := repository.HistoryDispositionProject
	if inlined {
		// The server inlined the bootstrap chunk against our explicit
		// non-inline request. It is dropped un-projected rather than
		// transiently persisted, because persisting it — even briefly — would
		// store pre-clamp message content. The bytes go out of scope with this
		// handler and are passed to nothing.
		disposition = repository.HistoryDispositionDroppedInline
	}

	payload, err := proto.Marshal(stripped)
	if err != nil {
		logger.Error().Err(err).Str("protocol_msg_id", e.Info.ID).
			Msg("whatsapp: failed to marshal history notification; withholding ack")
		return false
	}

	var oldest *time.Time
	if ts := stripped.GetOldestMsgInChunkTimestampSec(); ts > 0 {
		t := time.Unix(ts, 0).UTC()
		oldest = &t
	}

	if err := recorder.RecordHistoryNotification(
		ctx,
		e.Info.ID,
		payload,
		stripped.GetSyncType().String(),
		int32(stripped.GetChunkOrder()),
		oldest,
		disposition,
	); err != nil {
		// Nothing was downloaded, acked, or deleted, and manual mode means the
		// media is still on the server — so withholding the ack genuinely
		// recovers rather than merely logging. The recorder is idempotent on
		// protocol_msg_id, so the redelivery is a no-op once this succeeds.
		logger.Error().Err(err).
			Str("protocol_msg_id", e.Info.ID).
			Str("direct_path", stripped.GetDirectPath()).
			Str("enc_handle", stripped.GetEncHandle()).
			Msg("whatsapp: failed to record history notification; withholding ack so WhatsApp redelivers")
		return false
	}

	if inlined {
		logger.Warn().
			Str("sync_type", stripped.GetSyncType().String()).
			Uint32("chunk_order", stripped.GetChunkOrder()).
			Int64("oldest_msg_ts", stripped.GetOldestMsgInChunkTimestampSec()).
			Msg("whatsapp: bootstrap chunk arrived inline against a non-inline request; dropped un-projected")
	}
	return true
}
