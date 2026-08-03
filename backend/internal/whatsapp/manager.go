package whatsapp

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
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
	//
	// It is WALL-CLOCK time, deliberately: a bounded wait for a websocket is
	// real time, while the pairing TTL the tests advance is accelerated time.
	qrFirstCodeTimeout = 10 * time.Second

	// pairClientDisplayName must match the server-validated "Browser (OS)"
	// shape. The library documents that the specific browser does not matter,
	// only the shape, so a branded string would simply be rejected.
	pairClientDisplayName = "Chrome (Linux)"

	// deviceOSName is what the linked device shows on the phone's Linked
	// Devices screen.
	deviceOSName = "Personal CRM"

	// terminalPersistAttempts bounds the retry on the terminal-reason write.
	// The write is what stops the next boot reconnecting, so one transient blip
	// should not lose it — but a durable outage must not block the teardown.
	terminalPersistAttempts = 2

	// metadataBannedUntil persists a ban expiry alongside the terminal reason,
	// so a restart can tell an active ban from an expired one.
	metadataBannedUntil = "banned_until"

	// metadataLinkedJID persists which device the last successful pairing
	// adopted. The whatsmeow store is supposed to hold exactly one device, but a
	// replaced device whose delete failed leaves two, and the library's
	// first-device lookup reads an unordered scan — so the restart path resolves
	// by this JID instead of taking whichever row comes back first.
	metadataLinkedJID = "linked_jid"

	// deviceDeleteAttempts bounds the retry on removing a replaced device. Same
	// discipline as the terminal write, and for the same reason: one transient
	// blip must not leave the store holding two sessions.
	deviceDeleteAttempts = 2
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
	// MarkSyncStateTerminal writes the terminal reason and the error status as
	// ONE operation. Two writes would leave a window in which the row is
	// durably terminal but still idle, and the watchdog's immediate-breach rule
	// never fires on such a row — a permanently lost breach.
	MarkSyncStateTerminal(ctx context.Context, id uuid.UUID, reason string, metadata map[string]any) (*repository.SyncState, error)
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
//
// A session is HANDED OFF, never shared: it is constructed inside
// buildSessionEffect and reaches the loop only as a continuation result, from
// which moment the loop is its sole mutator (retired, paired). client, wa and
// deleteDevice are written once at construction and only read afterwards, which
// is what lets HistoryFetcher call sess.client.IsConnected() off the loop.
type session struct {
	client sessionClient
	// wa is the real client when production built this session, and nil when a
	// test fake did. HistoryFetcher() is available only when it is non-nil,
	// because the three history calls have no seam of their own.
	wa *whatsmeow.Client
	// deleteDevice removes the device row. Used when a pairing is cancelled
	// (the partially written device) and when a replaced device is retired.
	deleteDevice func(ctx context.Context) error
	// paired reports whether the device carried an ID when the session was
	// built.
	paired bool
	// jid is the device's own JID, when it has one. It is what the stale-pair
	// cleanup guards compare against, so a targeted delete can never remove the
	// live device.
	jid *types.JID
	// extraRows records that the resolver found the selected device alongside
	// others. A selector hit over a multi-row store is a degraded store, not a
	// clean resume.
	extraRows bool
	// healSelector records that the selector was absent or stale and should be
	// (re-)persisted as this device's JID. A single surviving device is always
	// resumed; the tie-breaker only matters when there is a tie.
	healSelector bool

	// retired records that this session has been superseded (a newer pairing
	// was adopted, the local device was cleared) or terminally handled. It is
	// written and read only by turns, and it is what makes attribution
	// permanent: a retired session is stale forever, whatever st.sess points at
	// by the time one of its queued events is delivered.
	retired bool
}

// Manager owns the whatsmeow client lifecycle, the pairing state machine, and
// the durable capture of history-sync notifications.
//
// It holds only immutable collaborators plus the actor plumbing. Every mutable
// field lives in actorState, which only the loop goroutine may touch.
type Manager struct {
	container *sqlstore.Container
	log       waLog.Logger
	cfg       *config.WhatsAppConfig
	syncRepo  syncStateWriter
	waRepo    backfillReader

	inbox      chan actorMsg
	stopping   chan struct{}
	loopExited chan struct{}
	done       chan struct{}

	effects       sync.WaitGroup
	effectCtx     context.Context
	cancelEffects context.CancelFunc

	snap     atomic.Pointer[snapshot]
	backfill atomic.Pointer[cachedBackfill]
	stopOnce sync.Once

	// timeouts is fixed at construction. See managerTimeouts.
	timeouts managerTimeouts
}

// NewManager builds the manager, applies the process-wide history-request
// device props, publishes the initial snapshot and starts the actor loop. It
// does not connect: Start() decides that, and only once the readiness gate is
// satisfied.
func NewManager(
	container *sqlstore.Container,
	log waLog.Logger,
	cfg *config.WhatsAppConfig,
	syncRepo syncStateWriter,
	waRepo backfillReader,
) *Manager {
	applyDeviceProps()

	effectCtx, cancelEffects := context.WithCancel(context.Background())
	m := &Manager{
		container:     container,
		log:           log,
		cfg:           cfg,
		syncRepo:      syncRepo,
		waRepo:        waRepo,
		inbox:         make(chan actorMsg, inboxCapacity),
		stopping:      make(chan struct{}),
		loopExited:    make(chan struct{}),
		done:          make(chan struct{}),
		effectCtx:     effectCtx,
		cancelEffects: cancelEffects,
		timeouts:      defaultTimeouts(),
	}

	st := &actorState{
		ingestor: refusingIngestor{},
		status:   Status{Configured: true, State: StateNotReady, Reason: ReasonIngestNotWired},
		devices:  m.containerDeviceOps(),
	}
	st.newSession = m.newContainerSession
	m.publish(st)
	m.startLoop(st)
	return m
}

// containerDeviceOps is the production device-store seam. It goes through the
// CONTAINER, so the staged purge needs no client and no device row of its own —
// which is what makes a forced disconnect work on a store too ambiguous to
// resolve.
func (m *Manager) containerDeviceOps() deviceOps {
	return deviceOps{
		list:      func(ctx context.Context) ([]types.JID, error) { return listDevices(ctx, m.container) },
		deleteAll: func(ctx context.Context, jids []types.JID) error { return deleteDevices(ctx, m.container, jids) },
		deleteJID: func(ctx context.Context, jid types.JID) error { return deleteDeviceByJID(ctx, m.container, jid) },
	}
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

// --- readiness seams (operations, so their ordering against Start is enforced
// by the queue rather than by a lock) ---------------------------------------

// SetIngestor installs the real message ingestor. Until it is called the
// default REFUSES, which withholds the ack rather than dropping the message.
func (m *Manager) SetIngestor(i MessageIngestor) {
	m.runOp(func(st *actorState, reply chan opResult) {
		st.ingestor = i
		reply <- opResult{}
	})
}

// SetHistoryRecorder installs the history-notification recorder. There is no
// default: a no-op here is silent, unrecoverable history loss.
func (m *Manager) SetHistoryRecorder(r HistoryNotificationRecorder) {
	m.runOp(func(st *actorState, reply chan opResult) {
		st.historyRecorder = r
		reply <- opResult{}
	})
}

// SetHistoryDrainReady records that the history drain worker is registered.
//
// It does NOT connect by itself. Start is the sole activation point: spreading
// activation across three setters would make "which PR turns WhatsApp on" depend
// on setter ordering, and would give Start's carefully-ordered gate a second,
// undisciplined entrance.
func (m *Manager) SetHistoryDrainReady() {
	m.runOp(func(st *actorState, reply chan opResult) {
		st.historyDrainReady = true
		reply <- opResult{}
	})
}

// setSessionFactory replaces the client-construction seam. It is an operation,
// so it is ordered against everything else the caller does.
func (m *Manager) setSessionFactory(fn sessionFactory) {
	m.runOp(func(st *actorState, reply chan opResult) {
		st.newSession = fn
		reply <- opResult{}
	})
}

// setDeviceOps replaces the device-store seam.
func (m *Manager) setDeviceOps(ops deviceOps) {
	m.runOp(func(st *actorState, reply chan opResult) {
		st.devices = ops
		reply <- opResult{}
	})
}

// --- client construction ---------------------------------------------------

// newClient is the single private constructor used by BOTH the boot path and
// the pairing paths. Everything that makes the session safe is set here, before
// anything connects, so there is no code path that produces a connected client
// without them.
func (m *Manager) newClient(device *store.Device, sess *session) *whatsmeow.Client {
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

	// Retry a failed FIRST connection. EnableAutoReconnect (default true) only
	// covers a socket that dropped after connecting; the initial dial is retried
	// only when this is set, and it defaults to false.
	cli.InitialAutoReconnect = true

	// WithSuccessStatus, not AddEventHandler: the plain variant wraps a void
	// handler and hard-codes a true return, so our false could never reach the
	// dispatcher and the withheld-ack contract would silently not exist.
	//
	// The handler closes over the session that owns this client, so events can
	// be attributed to their emitter.
	cli.AddEventHandlerWithSuccessStatus(func(evt any) bool {
		return m.handleEventFor(sess, evt)
	})

	return cli
}

// newContainerSession is the production seam implementation.
func (m *Manager) newContainerSession(ctx context.Context, req sessionRequest) (*session, error) {
	if m.container == nil {
		return nil, errors.New("whatsapp: no device container")
	}

	var device *store.Device
	var res deviceResolution
	if req.fresh {
		device = m.container.NewDevice()
	} else {
		var err error
		device, res, err = resolveLinkedDevice(ctx, m.container, req.linked)
		if err != nil {
			return nil, err
		}
	}

	sess := &session{
		deleteDevice: device.Delete,
		paired:       res.paired,
		jid:          res.jid,
		extraRows:    res.extraRows,
		healSelector: res.healSelector,
	}
	cli := m.newClient(device, sess)
	sess.client = cli
	sess.wa = cli
	return sess, nil
}

// --- Start -----------------------------------------------------------------

// Start brings the WhatsApp stack up. It never returns a fatal error for a
// WhatsApp-side problem: a WhatsApp failure must not abort the process boot.
//
// It is three turns — decide, resolve, connect — and returns only after the
// last, so its "always nil" contract is unchanged. Boot is single-threaded, so
// waiting costs nothing.
func (m *Manager) Start(ctx context.Context) error {
	res := m.runOp(m.opStart(ctx))
	if res.err != nil && errors.Is(res.err, ErrOperationSuperseded) {
		// A boot that lost the race to a user-initiated pairing has nothing to
		// report and must not turn a superseded start into a process-level
		// error. Start is the one caller that maps this away.
		logger.Info().Msg("whatsapp: start was superseded by a concurrent pairing")
	}
	return nil
}

func (m *Manager) opStart(ctx context.Context) func(*actorState, chan opResult) {
	return func(st *actorState, reply chan opResult) {
		if st.startInFlight {
			logger.Warn().Msg("whatsapp: a start is already in flight")
			reply <- opResult{}
			return
		}

		// 1. Readiness gate. Without a real ingestor, a recorder, and a
		//    registered drainer there is nowhere durable for an arriving message
		//    or history chunk to go, so connecting would acknowledge and discard
		//    data.
		if ready, missing := st.ready(); !ready {
			st.status.State = StateNotReady
			st.status.Reason = ReasonIngestNotWired
			st.status.Missing = missing
			logger.Info().Str("missing", missing).Msg("whatsapp: not ready, not connecting")
			reply <- opResult{}
			return
		}

		// The gate is satisfied, so any dependency the status was waiting on has
		// arrived; leaving the name behind would report a stale one.
		st.status.Missing = ""

		// 2. Ensure the sync-state row exists and read back what it persisted.
		//    This read is a precondition for connecting, not a best effort.
		terminalReason, bannedUntil, err := m.ensureSyncState(ctx, st)
		if err != nil {
			m.failStart(st, err)
			reply <- opResult{}
			return
		}

		st.startInFlight = true
		m.launch(st,
			[]effect{buildSessionEffect{build: st.newSession, req: sessionRequest{linked: st.linkedJID}}},
			launchOneShot,
			fence{sess: st.sess, pairing: st.pairing},
			opFlags{start: true},
			func(st *actorState, res effectResult) bool {
				return m.contStartResolved(st, res, terminalReason, bannedUntil, reply)
			},
			func(err error) { reply <- opResult{err: err} },
		)
	}
}

func (m *Manager) contStartResolved(st *actorState, res effectResult, terminalReason string, bannedUntil *time.Time, reply chan opResult) bool {
	if err := res.firstErr(); err != nil {
		if errors.Is(err, ErrDeviceStoreAmbiguous) {
			// Two or more stored devices and none of them is the one the
			// selector names. There is no branch that picks by luck: the
			// resolver refuses, and a forced disconnect is the remedy.
			st.status.State = StateError
			st.status.Reason = ReasonDeviceStoreAmbiguous
			m.updateSyncStatus(st, repository.SyncStatusError, err.Error())
			logger.Error().Err(err).Msg("whatsapp: the device store is ambiguous; refusing to resume any device")
			reply <- opResult{}
			return false
		}
		m.failStart(st, err)
		reply <- opResult{}
		return false
	}

	sess := res.step(0).sess
	if sess == nil || !sess.paired {
		st.status.State = StateNotPaired
		st.status.Reason = ""
		logger.Info().Msg("whatsapp: no linked device, idle until paired")
		reply <- opResult{}
		return false
	}

	if sess.extraRows {
		// A selector hit that leaves other rows in the store is a degraded
		// store, not a clean resume — the documented remedy is a forced
		// disconnect, which purges the enumerated set.
		st.status.ReplacedDeviceRetained = true
	}
	if sess.healSelector && sess.jid != nil {
		// A single surviving device is always resumed, even when the record of
		// which account was linked is missing or stale, and that record is
		// repaired at the same time.
		jid := *sess.jid
		st.linkedJID = &jid
		if attempted, ok := m.writeSelectorMetadata(st); attempted {
			st.status.LinkSelectorPersisted = &ok
		}
	}

	// A paired device whose last decision was terminal is NOT reconnected. The
	// decision has to survive a restart, which is why it is read from the
	// database rather than from memory.
	if _, isTerminal := terminalReasons[terminalReason]; isTerminal {
		if terminalReason != ReasonTemporaryBan || bannedUntil == nil || bannedUntil.After(accelerated.GetCurrentTime()) {
			st.status.State = StateDisconnected
			st.status.Reason = terminalReason
			st.status.BannedUntil = bannedUntil
			logger.Warn().Str("reason", terminalReason).Msg("whatsapp: durable terminal state, not reconnecting")
			reply <- opResult{}
			return false
		}
	}

	st.sess = sess
	st.status.State = StateConnecting
	st.status.Reason = ""

	m.launch(st,
		[]effect{connectEffect{sess: sess}},
		launchOneShot,
		fence{sess: sess, pairing: st.pairing},
		opFlags{start: true},
		func(st *actorState, res effectResult) bool { return m.contStartConnected(st, res, reply) },
		func(err error) { reply <- opResult{err: err} },
	)
	return true
}

func (m *Manager) contStartConnected(st *actorState, res effectResult, reply chan opResult) bool {
	if err := res.firstErr(); err != nil {
		if st.sess != nil {
			st.sess.retired = true
			st.sess = nil
		}
		m.failStart(st, err)
		reply <- opResult{}
		return false
	}
	// Nothing to publish: the Connected control event does that.
	logger.Info().Msg("whatsapp: connecting with stored device")
	reply <- opResult{}
	return false
}

// failStart records a boot failure without aborting the process.
func (m *Manager) failStart(st *actorState, err error) {
	st.status.State = StateError
	st.status.Reason = err.Error()
	m.updateSyncStatus(st, repository.SyncStatusError, err.Error())
	logger.Warn().Err(err).Msg("whatsapp: failed to start")
}

// --- sync-state writes (bounded, and inside the turn) ----------------------

// turnCtx bounds a repository call made from inside a turn. The call stays in
// the turn — that is the atomic-terminal-write requirement — so a hung database
// must bound the turn instead of wedging the loop.
func turnCtx(parent context.Context) (context.Context, context.CancelFunc) {
	if parent == nil {
		parent = context.Background()
	}
	return context.WithTimeout(parent, actorDBTimeout)
}

// ensureSyncState resolves (creating if absent) the external_sync_state row and
// returns the persisted terminal reason and ban expiry, so Start can honour a
// decision taken before the last restart.
func (m *Manager) ensureSyncState(parent context.Context, st *actorState) (terminalReason string, bannedUntil *time.Time, err error) {
	if m.syncRepo == nil {
		return "", nil, nil
	}

	ctx, cancel := turnCtx(parent)
	defer cancel()

	var state *repository.SyncState
	state, err = m.syncRepo.GetSyncStateBySource(ctx, syncSource, nil)
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
			return "", nil, fmt.Errorf("create whatsapp sync state: %w", err)
		}
	default:
		// Fail closed. The persisted terminal reason lives on this row, so a row
		// we cannot read is a row whose "do not reconnect" decision we cannot
		// see — connecting anyway would reconnect exactly the dead or banned
		// device the decision exists to hold back.
		return "", nil, fmt.Errorf("load whatsapp sync state: %w", err)
	}

	id := state.ID
	st.syncStateID = &id

	if raw, ok := state.Metadata[metadataLinkedJID].(string); ok && raw != "" {
		if parsed, parseErr := types.ParseJID(raw); parseErr == nil {
			st.linkedJID = &parsed
		}
	}
	if reason, ok := state.Metadata[repository.SyncStateMetadataTerminalReason].(string); ok {
		terminalReason = reason
	}
	if raw, ok := state.Metadata[metadataBannedUntil].(string); ok {
		if t, parseErr := time.Parse(time.RFC3339, raw); parseErr == nil {
			bannedUntil = &t
		}
	}
	return terminalReason, bannedUntil, nil
}

// updateSyncStatus writes the connection status onto external_sync_state so the
// staleness watchdog and the settings staleness banner see WhatsApp for free.
func (m *Manager) updateSyncStatus(st *actorState, status repository.SyncStatus, errMsg string) {
	if st.syncStateID == nil || m.syncRepo == nil {
		return
	}
	ctx, cancel := turnCtx(context.Background())
	defer cancel()

	var msg *string
	if errMsg != "" {
		msg = &errMsg
	}
	if _, err := m.syncRepo.UpdateSyncStateStatus(ctx, *st.syncStateID, status, msg); err != nil {
		logger.Warn().Err(err).Msg("whatsapp: failed to update sync state status")
	}
}

// markTerminal durably records a permanent disconnect. It runs INSIDE the
// terminal turn, so the metadata write, the status='error' write, the in-memory
// transition and TerminalReasonPersisted are one indivisible step.
//
// The reason and the error status are ONE write, not two. Both are load-bearing
// and neither is useful alone: the restart gate reads the metadata and nothing
// else, while the staleness watchdog opens its immediate breach only for a row
// that is BOTH in error and carries a reason. Splitting them was a permanently
// lost breach.
func (m *Manager) markTerminal(st *actorState, reason string, bannedUntil *time.Time) error {
	if st.syncStateID == nil || m.syncRepo == nil {
		return errors.New("whatsapp: no sync state row to record the terminal reason on")
	}
	metadata := st.metadataBase()
	metadata[repository.SyncStateMetadataTerminalReason] = reason
	if bannedUntil != nil {
		metadata[metadataBannedUntil] = bannedUntil.UTC().Format(time.RFC3339)
	}

	ctx, cancel := turnCtx(context.Background())
	defer cancel()

	var err error
	for attempt := 0; attempt < terminalPersistAttempts; attempt++ {
		if _, err = m.syncRepo.MarkSyncStateTerminal(ctx, *st.syncStateID, reason, metadata); err == nil {
			return nil
		}
	}
	return fmt.Errorf("record terminal disconnect %q: %w", reason, err)
}

// clearTerminalReason removes a stale terminal decision once a connection
// actually succeeds. It writes the metadata base, which carries the linked JID
// forward: every write replaces the whole document.
func (m *Manager) clearTerminalReason(st *actorState) {
	if st.syncStateID == nil || m.syncRepo == nil {
		return
	}
	ctx, cancel := turnCtx(context.Background())
	defer cancel()
	if _, err := m.syncRepo.UpdateSyncStateMetadata(ctx, *st.syncStateID, st.metadataBase()); err != nil {
		logger.Warn().Err(err).Msg("whatsapp: failed to clear terminal disconnect reason")
	}
}

// writeSelectorMetadata persists which device is now linked (and, with it,
// clears the replaced device's terminal reason — it is the same single write).
//
// attempted reports whether there was a row to write to at all; ok reports
// whether the write landed. A selector that could not be persisted is a
// SURFACED failure, not a log line: the record of WHICH device is linked is
// what makes the restart path deterministic.
func (m *Manager) writeSelectorMetadata(st *actorState) (attempted, ok bool) {
	if st.syncStateID == nil || m.syncRepo == nil {
		return false, false
	}
	ctx, cancel := turnCtx(context.Background())
	defer cancel()

	var err error
	for attempt := 0; attempt < terminalPersistAttempts; attempt++ {
		if _, err = m.syncRepo.UpdateSyncStateMetadata(ctx, *st.syncStateID, st.metadataBase()); err == nil {
			return true, true
		}
	}
	logger.Error().Err(err).Msg("whatsapp: could not persist which device is linked; a restart may not resolve it deterministically")
	return true, false
}

// --- attribution -----------------------------------------------------------

// eventScope attributes a lifecycle event to the client that emitted it.
//
// whatsmeow dispatches most lifecycle events asynchronously — LoggedOut and
// TemporaryBan from `go cli.dispatchEvent(...)` in handleConnectFailure,
// StreamReplaced and Disconnected likewise — so a queued event from a client
// this manager has already cancelled, superseded or replaced can land at any
// later moment, including after its device row has been deleted.
type eventScope int

const (
	// scopeInstalled: the event came from the session the manager currently
	// owns (or from an unattributed caller, which only tests produce).
	scopeInstalled eventScope = iota
	// scopePairing: the event came from the in-flight pairing's client, which
	// is not yet — and may never be — the installed session.
	scopePairing
	// scopeStale: the event came from a client the manager has abandoned. It
	// speaks for nothing and is dropped.
	scopeStale
)

// scopeOf attributes an event to its emitter. It runs inside a turn, so
// attribution, the decision and the state transition that follow are one
// indivisible step by construction; there is no check-then-act gap to close.
func (st *actorState) scopeOf(from *session) (eventScope, *pairingState) {
	switch {
	case from != nil && from.retired:
		// Superseded or already terminally handled. Retirement is permanent, so
		// this stays stale no matter what st.sess points at now.
		return scopeStale, st.pairing
	case from == nil:
		// Unattributed. Production always binds a session (newClient closes over
		// one), so this is the test-only entry point.
		if st.sess == nil && st.pairing != nil {
			return scopePairing, st.pairing
		}
		return scopeInstalled, st.pairing
	case st.sess != nil && from == st.sess:
		return scopeInstalled, st.pairing
	case st.pairing != nil && st.pairing.owns(from):
		return scopePairing, st.pairing
	default:
		return scopeStale, st.pairing
	}
}

// --- the event handler -----------------------------------------------------

// handleEvent is the unattributed entry point. Production never uses it —
// newClient binds a session — but it keeps the tests' call shape.
func (m *Manager) handleEvent(evt any) bool {
	return m.handleEventFor(nil, evt)
}

// handleEventFor is the single event handler, registered through
// AddEventHandlerWithSuccessStatus so its false return reaches the dispatcher
// and — under SynchronousAck — withholds the stanza ack.
//
// The switch has exactly two groups and no state-mutating default: an event
// enters the actor IF AND ONLY IF it can change who the manager owns.
//
//   - CONTROL PLANE: enqueued, and the handler returns true at once. The verdict
//     for all of them is unconditionally true (there is no stanza to ack), so
//     waiting for the turn would buy a constant at the price of putting a turn
//     that can legitimately run for seconds on the critical path of a
//     synchronously-dispatched node handler.
//   - DATA PLANE: never enters the queue. It mutates no manager state and reads
//     only the published snapshot, so D10's record-synchronously-then-return
//     contract is computed on the dispatching goroutine exactly as before.
func (m *Manager) handleEventFor(sess *session, evt any) bool {
	ctx := context.Background()

	switch e := evt.(type) {
	// --- control plane -----------------------------------------------------
	//
	// Losslessness is decided by WHO DISPATCHES the event, which is a property
	// of the library rather than a guess. Seven of the eight arrive on a
	// goroutine the library created for no other purpose, so blocking them costs
	// a parked goroutine and nothing else.
	case *events.LoggedOut, *events.StreamReplaced, *events.ClientOutdated, *events.TemporaryBan:
		// A dropped terminal event may be the last thing that client ever emits,
		// and its loss is the durable record that stops the next boot
		// reconnecting a dead or banned session. Permanently.
		m.enqueueControl(sess, evt, true)
		return true

	case *events.PairSuccess, *events.PairError:
		// Both are dispatched from the same dedicated goroutine, which fires
		// exactly one of them. Dropping either loses the adoption of a device the
		// user just linked, or the only notification that a pairing-written row
		// needs deleting.
		m.enqueueControl(sess, evt, true)
		return true

	case *events.Disconnected:
		m.enqueueControl(sess, evt, true)
		return true

	case *events.Connected:
		// The ONLY event dispatched synchronously from a node handler, and the
		// only one whose loss is genuinely recoverable: it carries no durable
		// decision, and whatsmeow re-emits lifecycle events on every reconnect.
		// Blocking a node handler past 300 s would cost message ordering for the
		// whole session.
		m.enqueueControl(sess, evt, false)
		return true

	// --- data plane --------------------------------------------------------
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

// enqueueControl submits a lifecycle event. ctrlEventMsg carries no reply
// channel, so a future control event cannot decide to withhold an ack by
// accident: it would have to move to the data plane or acquire its own
// synchronous path.
func (m *Manager) enqueueControl(from *session, evt any, lossless bool) {
	msg := &ctrlEventMsg{from: from, evt: evt}
	if lossless {
		select {
		case m.inbox <- msg:
		case <-m.stopping:
		}
		return
	}

	timer := time.NewTimer(m.timeouts.ctrlEnqueue)
	defer timer.Stop()
	select {
	case m.inbox <- msg:
	case <-m.stopping:
	case <-timer.C:
		logger.Error().
			Str("event", fmt.Sprintf("%T", evt)).
			Int("queue_depth", len(m.inbox)).
			Msg("whatsapp: dropped a control event after the enqueue deadline")
	}
}

// handleControlEvent runs one lifecycle event as a turn.
func (m *Manager) handleControlEvent(st *actorState, from *session, evt any) {
	switch e := evt.(type) {
	case *events.Connected:
		m.onConnected(st, from)
	case *events.Disconnected:
		m.onDisconnected(st, from)
	case *events.PairSuccess:
		m.onPairSuccess(st, from, e)
	case *events.PairError:
		m.onPairError(st, from, e)
	case *events.LoggedOut:
		m.onTerminal(st, from, ReasonLoggedOut, nil)
	case *events.StreamReplaced:
		m.onTerminal(st, from, ReasonStreamReplaced, nil)
	case *events.ClientOutdated:
		m.onTerminal(st, from, ReasonClientOutdated, nil)
	case *events.TemporaryBan:
		until := accelerated.GetCurrentTime().Add(e.Expire)
		m.onTerminal(st, from, ReasonTemporaryBan, &until)
	default:
		logger.Debug().Str("event", fmt.Sprintf("%T", evt)).Msg("whatsapp: unhandled control event")
	}
}

// tearDownOrphan disconnects a client the manager no longer owns. It is an
// effect, never an inline call: Client.Disconnect takes the library's socket
// lock and a turn may make no socket call.
func (m *Manager) tearDownOrphan(st *actorState, sess *session) {
	if sess == nil {
		return
	}
	m.launch(st, []effect{disconnectEffect{sess: sess}}, launchDetached, fence{}, opFlags{}, nil, nil)
}

func (m *Manager) onConnected(st *actorState, from *session) {
	switch scope, _ := st.scopeOf(from); scope {
	case scopeStale:
		// A client the manager no longer owns — an abandoned pairing whose
		// socket came up anyway, or a session already retired by a terminal
		// event — must not publish a connection state, and must not be left
		// holding one either.
		logger.Debug().Msg("whatsapp: ignoring Connected from an abandoned client")
		m.tearDownOrphan(st, from)
		return
	case scopePairing:
		// A pairing client connects to the pairing websocket before the device
		// is linked. Reporting "connected" here would tell the settings page the
		// account is live while the user is still holding an unscanned QR code.
		logger.Debug().Msg("whatsapp: socket connected for pairing")
		return
	}

	var jid, pushName *string
	if sess := st.sess; sess != nil && sess.wa != nil && sess.wa.Store != nil {
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
	st.status.State = StateConnected
	st.status.Reason = ""
	st.status.BannedUntil = nil
	st.status.TerminalReasonPersisted = nil
	st.status.ConnectedAt = &now
	if jid != nil {
		st.status.JID = jid
		if parsed, err := types.ParseJID(*jid); err == nil && parsed.Server == types.DefaultUserServer {
			user := parsed.User
			st.status.PhoneNumber = &user
		}
	}
	if pushName != nil {
		st.status.PushName = pushName
	}

	m.clearTerminalReason(st)
	m.updateSyncStatus(st, repository.SyncStatusIdle, "")
	logger.Info().Msg("whatsapp: connected")
}

// onDisconnected downgrades the published state to reconnecting. whatsmeow
// retries transient failures on its own, so there is no bespoke reconnect loop
// here — only the report.
func (m *Manager) onDisconnected(st *actorState, from *session) {
	switch scope, _ := st.scopeOf(from); scope {
	case scopeStale:
		logger.Debug().Msg("whatsapp: ignoring Disconnected from an abandoned client")
		return
	case scopePairing:
		logger.Debug().Msg("whatsapp: pairing socket disconnected")
		return
	}

	// Only a live connection is downgraded. A drop reported while the state is
	// already connecting belongs to the library's own initial-connect retry.
	if st.status.State == StateConnected {
		st.status.State = StateReconnecting
	}
}

// onPairSuccess adopts a completed pairing.
//
// It does NOT publish "connected". The library documents that PairSuccess is
// generally followed by a websocket reconnection and that callers should wait
// for Connected, so the pairing is reported as connecting and only onConnected —
// itself identity-checked — makes it live.
func (m *Manager) onPairSuccess(st *actorState, from *session, e *events.PairSuccess) {
	jid := e.ID.String()

	var superseded *session
	scope, pairing := st.scopeOf(from)
	switch scope {
	case scopePairing:
		pairing.cancelled = true
		if pairing.cancel != nil {
			pairing.cancel()
		}
		// The client that completed the pairing becomes the live session: it
		// already carries the event handler and the manual-history flags, and
		// whatsmeow reconnects it as the linked device.
		if adopted := pairing.sess; adopted != nil {
			adopted.paired = true
			adopted.jid = &e.ID
			if st.sess != nil && st.sess != adopted {
				superseded = st.sess
				superseded.retired = true
			}
			st.sess = adopted
		}
		st.pairing = nil

	case scopeInstalled:
		// A re-pair reported on the already-installed session: same device, so
		// there is nothing to retire.

	default:
		logger.Warn().Msg("whatsapp: ignoring PairSuccess from an abandoned pairing client")
		m.tearDownOrphan(st, from)
		m.deleteStalePairDevice(st, e.ID)
		return
	}

	st.status.State = StateConnecting
	st.status.Reason = ""
	st.status.JID = &jid
	// ConnectedAt is stamped by onConnected: nothing is connected yet.
	st.status.ConnectedAt = nil
	// Both of these described the device this pairing replaces.
	st.status.TerminalReasonPersisted = nil
	st.status.BannedUntil = nil
	if e.ID.Server == types.DefaultUserServer {
		user := e.ID.User
		st.status.PhoneNumber = &user
	}
	if e.BusinessName != "" {
		name := e.BusinessName
		st.status.PushName = &name
	}

	// Record WHICH device is now linked, and drop the terminal decision that
	// belonged to the one it replaces. Both are one metadata write, in this
	// turn, so a terminal event racing this adoption cannot land its reason
	// afterwards.
	adoptedJID := e.ID
	st.linkedJID = &adoptedJID
	if attempted, ok := m.writeSelectorMetadata(st); attempted {
		// The pairing is NOT torn down on a failed write: the device is
		// genuinely linked remotely, and unlinking it because we failed to write
		// a row would destroy what the user just did.
		st.status.LinkSelectorPersisted = &ok
	}

	if superseded != nil {
		// A failed selector write makes this delete MORE important, not less:
		// two rows with a stale-or-absent selector is the one state the resolver
		// refuses, while one row with a stale-or-absent selector heals.
		m.launch(st,
			[]effect{disconnectEffect{sess: superseded}, deleteDeviceEffect{sess: superseded}},
			launchOneShot,
			fence{sess: st.sess, pairing: st.pairing},
			opFlags{},
			func(st *actorState, res effectResult) bool {
				if err := res.firstErr(); err != nil {
					st.status.ReplacedDeviceRetained = true
					logger.Error().Err(err).
						Msg("whatsapp: could not delete the replaced device; the device store now holds a stale session")
				}
				return false
			},
			func(error) {},
		)
	}
	logger.Info().Msg("whatsapp: device paired, awaiting connection")
}

// onPairError handles the event the library dispatches when a pair-success
// arrived but finishing the pairing locally failed.
//
// It is a CONTROL event, not a default-branch log line: it is the only signal
// that a pairing-written device row survived a FAILED pairing, and the targeted
// cleanup it gates would otherwise never be scheduled.
func (m *Manager) onPairError(st *actorState, from *session, e *events.PairError) {
	scope, pairing := st.scopeOf(from)
	switch scope {
	case scopePairing:
		logger.Warn().Err(e.Error).Msg("whatsapp: pairing failed")
		m.abandonPairingTurn(st, pairing, errors.Join(ErrPairingCancelled, e.Error))
		m.deleteStalePairDevice(st, e.ID)
	case scopeStale:
		logger.Warn().Err(e.Error).Msg("whatsapp: PairError from an abandoned pairing client")
		m.tearDownOrphan(st, from)
		m.deleteStalePairDevice(st, e.ID)
	default:
		// A stray pair report on the installed session. Deleting its device
		// would unlink a working account, so this is handled as stale MINUS the
		// delete.
		logger.Warn().Err(e.Error).Msg("whatsapp: PairError reported on the installed session")
	}
}

// deleteStalePairDevice removes, by JID, a device row the library saved for an
// attempt the manager has already abandoned.
//
// The library guarantees the pairing: every failure path after Store.Save
// succeeds deletes the store and dispatches PairError, so IF a pairing-written
// row survives, a PairSuccess or PairError naming its JID was dispatched. That
// makes this complete rather than best-effort.
//
// Two guards, because a targeted delete is a loaded weapon: never the linked
// JID, and never the installed session's device. A stale event whose JID is the
// live device is a re-pair report on the installed session, not an orphan.
func (m *Manager) deleteStalePairDevice(st *actorState, jid types.JID) {
	if jid.IsEmpty() {
		return
	}
	if st.linkedJID != nil && st.linkedJID.ToNonAD() == jid.ToNonAD() {
		logger.Debug().Str("jid", jid.String()).Msg("whatsapp: not deleting the linked device on a stale pair event")
		return
	}
	if st.sess != nil && st.sess.jid != nil && st.sess.jid.ToNonAD() == jid.ToNonAD() {
		logger.Debug().Str("jid", jid.String()).Msg("whatsapp: not deleting the installed session's device on a stale pair event")
		return
	}

	m.launch(st,
		[]effect{deleteDeviceByJIDEffect{del: st.devices.deleteJID, jid: jid}},
		launchOneShot,
		fence{sess: st.sess, pairing: st.pairing},
		opFlags{},
		func(st *actorState, res effectResult) bool {
			if err := res.firstErr(); err != nil {
				st.status.ReplacedDeviceRetained = true
				logger.Error().Err(err).Str("jid", jid.String()).
					Msg("whatsapp: could not delete an abandoned pairing's device; the store holds a stale session")
			}
			return false
		},
		func(error) {},
	)
}

// onTerminal records a permanent disconnect: the reason is persisted BEFORE the
// client is torn down, nothing is retried, and Start() honours the decision
// after a restart.
func (m *Manager) onTerminal(st *actorState, from *session, reason string, bannedUntil *time.Time) {
	scope, pairing := st.scopeOf(from)
	switch scope {
	case scopeStale:
		logger.Warn().Str("reason", reason).
			Msg("whatsapp: ignoring terminal event from an abandoned client")
		m.tearDownOrphan(st, from)
		return
	case scopePairing:
		// The attempt is over, and its client is torn down with it. The
		// installed session's durable "do not reconnect" decision is not this
		// client's to write: it is a different device.
		logger.Warn().Str("reason", reason).Msg("whatsapp: pairing attempt ended")
		m.abandonPairingTurn(st, pairing, ErrPairingCancelled)
		return
	}

	// Retire before anything else, and remove it from its slot in the same
	// turn: a retired session is never left in st.sess. No event this session
	// has already queued may mutate the manager again — including the Connected
	// a reconnect attempt may still deliver, which would otherwise clear the
	// very terminal record written below.
	sess := st.sess
	if sess != nil {
		sess.retired = true
		st.sess = nil
	}

	persistErr := m.markTerminal(st, reason, bannedUntil)

	st.status.State = StateDisconnected
	st.status.Reason = reason
	st.status.BannedUntil = bannedUntil
	st.status.ConnectedAt = nil
	persisted := persistErr == nil
	st.status.TerminalReasonPersisted = &persisted
	if persistErr != nil {
		// Best effort, and safe: a status-only write cannot produce the row that
		// was the problem (terminal metadata on a non-error row), because it
		// writes no metadata.
		m.updateSyncStatus(st, repository.SyncStatusError, reason+" (reason not durably recorded)")
	}

	// The session is dead either way — the server ended it — so it is always
	// torn down. Auto-reconnect must be cancelled even when we could not record
	// why, or the client keeps retrying a session the server has ended.
	m.tearDownOrphan(st, sess)

	if persistErr != nil {
		logger.Error().Err(persistErr).Str("reason", reason).
			Msg("whatsapp: could not durably record the permanent disconnect; a restart may reconnect this device")
		return
	}
	logger.Warn().Str("reason", reason).Msg("whatsapp: permanent disconnect, not reconnecting")
}

// --- data plane ------------------------------------------------------------

// handleMessage forwards an ordinary message to the ingestor.
//
// It reads the published snapshot ONCE at entry and uses that seam for the whole
// event, so a SetIngestor landing mid-event cannot make one event use two
// ingestors. It is deliberately session-agnostic: attribution exists to stop a
// dead client publishing state, and a real message is a real message.
func (m *Manager) handleMessage(ctx context.Context, e *events.Message) bool {
	var ingestor MessageIngestor
	if s := m.snap.Load(); s != nil {
		ingestor = s.ingestor
	}

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
	var recorder HistoryNotificationRecorder
	if s := m.snap.Load(); s != nil {
		recorder = s.historyRecorder
	}

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
		// store pre-clamp message content.
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
		// recovers rather than merely logging.
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
