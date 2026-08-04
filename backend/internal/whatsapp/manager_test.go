package whatsapp

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/repository"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

// --- device props ----------------------------------------------------------

// TestApplyDeviceProps_SetsHistoryWindowOnce pins the four registration values
// that are baked into the pairing payload and cannot be widened afterwards
// without unlinking and re-pairing.
func TestApplyDeviceProps_SetsHistoryWindowOnce(t *testing.T) {
	applyDeviceProps()

	assert.Equal(t, deviceOSName, store.DeviceProps.GetOs())
	assert.Equal(t, "DESKTOP", store.DeviceProps.GetPlatformType().String(),
		"desktop-class clients are offered ~1 year of history; web clients ~3 months")
	assert.True(t, store.DeviceProps.GetRequireFullSync())
	assert.Equal(t, HistorySyncDaysLimit, store.DeviceProps.GetHistorySyncConfig().GetFullSyncDaysLimit())

	// A second manager must not re-apply: DeviceProps is a package-level global
	// in whatsmeow, so mutating it twice is process-wide churn.
	store.DeviceProps.Os = proto.String("sentinel")
	applyDeviceProps()
	assert.Equal(t, "sentinel", store.DeviceProps.GetOs(),
		"applyDeviceProps must run exactly once per process")
	store.DeviceProps.Os = proto.String(deviceOSName)
}

// TestApplyDeviceProps_DisablesInlineInitialPayload is the first line of defence
// for the clamp rule: the library default asks the server to embed history
// inline in the protocol message, and such a chunk has to be dropped.
func TestApplyDeviceProps_DisablesInlineInitialPayload(t *testing.T) {
	applyDeviceProps()
	assert.False(t, store.DeviceProps.GetHistorySyncConfig().GetInlineInitialPayloadInE2EeMsg(),
		"the library defaults this to true; leaving it true makes dropped bootstrap chunks routine")
}

// --- client construction ---------------------------------------------------

// TestNewClient_SetsManualHistoryFlags is the regression guard for the whole
// durability premise. With these flags at their defaults the library downloads
// and deletes our one-shot history behind our back; with AddEventHandler
// instead of the WithSuccessStatus variant, a false return is swallowed.
func TestNewClient_SetsManualHistoryFlags(t *testing.T) {
	m := newManagerForTest(t, newFakeSyncStore(), &fakeBackfillReader{})
	cli := m.newClient(&store.Device{}, &session{})
	require.NotNil(t, cli)

	assert.True(t, cli.SynchronousAck,
		"acks must fire only after our handler returns, so a crash becomes a redelivery")
	assert.True(t, cli.ManualHistorySyncDownload,
		"the library's automatic download+delete loop must never start")
	assert.True(t, cli.DisableManualHistorySyncReceipt,
		"the history receipt must not be sent behind our back")

	// EnableAutoReconnect covers a socket that dropped after connecting and
	// defaults to true; the INITIAL dial is retried only under
	// InitialAutoReconnect, which defaults to FALSE.
	assert.True(t, cli.InitialAutoReconnect,
		"a failed first connection must be retried, not left down until the next restart")
	assert.True(t, cli.EnableAutoReconnect,
		"the initial retry is gated on both flags, so the default must not be turned off")
}

// TestManager_SynchronousAckEnabled is one line and it is the whole durability
// premise for live messages.
func TestManager_SynchronousAckEnabled(t *testing.T) {
	m := newManagerForTest(t, newFakeSyncStore(), &fakeBackfillReader{})
	assert.True(t, m.newClient(&store.Device{}, &session{}).SynchronousAck)
}

// TestNewClient_HandlerFailureReachesDispatcher is the discriminating proof of
// the withheld-ack contract, end to end through the real client's dispatcher.
//
// AddEventHandler wraps a void handler and hard-codes a true return, so if the
// registration used that variant instead, the dispatcher would report success
// here and the ack would be sent for a message we failed to store.
func TestNewClient_HandlerFailureReachesDispatcher(t *testing.T) {
	m, _, ingestor, _ := newTestManager(t, newFakeClient(), true)
	seedSelfJID(m, testOwnPN)
	cli := m.newClient(&store.Device{}, &session{})
	require.NotNil(t, cli)

	//nolint:staticcheck // DangerousInternals is the only way to observe the dispatcher's verdict.
	dispatcher := cli.DangerousInternals()

	assert.False(t, dispatcher.DispatchEvent(newMessageEvent("msg-ok")),
		"a successful ingest lets the ack through")

	ingestor.setErr(errors.New("sink down"))
	assert.True(t, dispatcher.DispatchEvent(newMessageEvent("msg-bad")),
		"a failing ingest must make the dispatcher report handlerFailed, which withholds the ack")
}

// --- readiness gate --------------------------------------------------------

func TestStart_RefusesWhenNotReady(t *testing.T) {
	tests := []struct {
		name  string
		build func(*testing.T) *Manager
	}{
		{
			name: "missing real ingestor",
			build: func(t *testing.T) *Manager {
				m := newNotReadyManager(t)
				m.SetHistoryDrainReady()
				return m
			},
		},
		{
			name: "missing history recorder",
			build: func(t *testing.T) *Manager {
				m := newManagerForTest(t, newFakeSyncStore(), &fakeBackfillReader{})
				m.SetIngestor(&fakeIngestor{})
				m.SetHistoryDrainReady()
				m.setSessionFactory(func(context.Context, sessionRequest) (*session, error) {
					return nil, errors.New("must not be reached")
				})
				return m
			},
		},
		{
			name: "drain not ready",
			build: func(t *testing.T) *Manager {
				m := newManagerForTest(t, newFakeSyncStore(), &fakeBackfillReader{})
				m.SetIngestor(&fakeIngestor{})
				m.SetHistoryRecorder(&fakeRecorder{})
				m.setSessionFactory(func(context.Context, sessionRequest) (*session, error) {
					return nil, errors.New("must not be reached")
				})
				return m
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := tt.build(t)

			ready, missing := m.Ready()
			require.False(t, ready)
			assert.NotEmpty(t, missing, "the missing piece must be named")

			// The session factory errors if reached, so a not_ready status with
			// no error state proves no connect was attempted.
			require.NoError(t, m.Start(context.Background()))
			assert.Equal(t, StateNotReady, m.Status().State)
			assert.Equal(t, ReasonIngestNotWired, m.Status().Reason)
		})
	}
}

// TestStartPairing_RefusesWhenNotReady is the separate half of the gate:
// pairing must not be able to bypass Start's precondition and produce a
// connected client with no durable sink.
func TestStartPairing_RefusesWhenNotReady(t *testing.T) {
	m := newNotReadyManager(t)
	err := m.StartPairing(context.Background(), PairRequest{Method: PairMethodQR})
	assert.ErrorIs(t, err, ErrIngestNotWired)
	assert.Nil(t, m.Status().Pairing, "no pairing state may be created")
}

// TestDefaultIngestor_RefusesAndWithholdsAck is the regression guard for the
// shape that loses data: a default no-op that SUCCEEDS would acknowledge and
// drop a live message irrecoverably.
func TestDefaultIngestor_RefusesAndWithholdsAck(t *testing.T) {
	err := refusingIngestor{}.IngestMessage(context.Background(), IngestedMessage{})
	assert.ErrorIs(t, err, ErrIngestNotWired)

	m := newManagerForTest(t, newFakeSyncStore(), &fakeBackfillReader{})
	m.SetHistoryRecorder(&fakeRecorder{})
	// The own identity is seeded so the REFUSING INGESTOR is what withholds
	// here: without it the handler would withhold one step earlier, for an
	// unknown own identity, and this test would pass without exercising the
	// default at all.
	seedSelfJID(m, testOwnPN)
	assert.False(t, m.handleEvent(newMessageEvent("msg-1")),
		"the default ingestor must withhold the ack, not swallow the message")
}

// --- startup paths ---------------------------------------------------------

func TestStart_WithNoDeviceStaysIdle(t *testing.T) {
	cli := newFakeClient()
	m, _, _, _ := newTestManager(t, cli, false)

	require.NoError(t, m.Start(context.Background()))
	assert.Equal(t, StateNotPaired, m.Status().State)
	assert.NotContains(t, cli.callLog(), "connect", "an unpaired device must never connect")
}

// TestStart_DoesNotReconnectAfterPersistedTerminalReason is the regression
// guard for a manager that retried forever after a logout or a ban: the
// terminal decision has to survive a restart, so it lives in the database.
func TestStart_DoesNotReconnectAfterPersistedTerminalReason(t *testing.T) {
	for _, reason := range []string{ReasonLoggedOut, ReasonStreamReplaced, ReasonClientOutdated} {
		t.Run(reason, func(t *testing.T) {
			cli := newFakeClient()
			m, syncStore, _, _ := newTestManager(t, cli, true)
			syncStore.seedTerminal(reason, nil)

			require.NoError(t, m.Start(context.Background()))

			status := m.Status()
			assert.Equal(t, StateDisconnected, status.State)
			assert.Equal(t, reason, status.Reason)
			assert.NotContains(t, cli.callLog(), "connect",
				"a durably terminal device must not be reconnected after a restart")
		})
	}
}

func TestStart_DoesNotReconnectDuringActiveBan(t *testing.T) {
	cli := newFakeClient()
	m, syncStore, _, _ := newTestManager(t, cli, true)
	until := accelerated.GetCurrentTime().Add(time.Hour)
	syncStore.seedTerminal(ReasonTemporaryBan, &until)

	require.NoError(t, m.Start(context.Background()))

	status := m.Status()
	assert.Equal(t, StateDisconnected, status.State)
	assert.Equal(t, ReasonTemporaryBan, status.Reason)
	require.NotNil(t, status.BannedUntil)
	assert.NotContains(t, cli.callLog(), "connect")
}

// TestStart_ReconnectsWhenPersistedStateIsNotTerminal is the negative control:
// without a terminal reason the same paired device does connect.
func TestStart_ReconnectsWhenPersistedStateIsNotTerminal(t *testing.T) {
	cli := newFakeClient()
	m, _, _, _ := newTestManager(t, cli, true)

	require.NoError(t, m.Start(context.Background()))

	assert.Contains(t, cli.callLog(), "connect")
	assert.Equal(t, StateConnecting, m.Status().State)
}

func TestStart_ExpiredBanReconnects(t *testing.T) {
	cli := newFakeClient()
	m, syncStore, _, _ := newTestManager(t, cli, true)
	until := accelerated.GetCurrentTime().Add(-time.Hour)
	syncStore.seedTerminal(ReasonTemporaryBan, &until)

	require.NoError(t, m.Start(context.Background()))
	assert.Contains(t, cli.callLog(), "connect", "an expired ban is not a terminal state")
}

// TestStart_CreatesSyncStateRow proves the staleness watchdog and the settings
// banner can see WhatsApp at all.
func TestStart_CreatesSyncStateRow(t *testing.T) {
	m, syncStore, _, _ := newTestManager(t, newFakeClient(), false)
	require.NoError(t, m.Start(context.Background()))
	assert.Contains(t, syncStore.callLog(), "create")
}

// TestStart_ConnectFailureDoesNotAbortBoot pins the posture that a WhatsApp
// failure never takes the process down.
func TestStart_ConnectFailureDoesNotAbortBoot(t *testing.T) {
	cli := newFakeClient()
	cli.connectErr = errors.New("dial tcp: no route to host")
	m, syncStore, _, _ := newTestManager(t, cli, true)

	require.NoError(t, m.Start(context.Background()), "a WhatsApp failure must never abort boot")
	assert.Equal(t, StateError, m.Status().State)
	assert.Contains(t, syncStore.callLog(), "status:error")

	// Losing the slot is not the same as being released. A failed dial can
	// leave a half-open socket, and the connection context it dialled under is
	// the library's auto-reconnect parent, so the session dies the same way
	// every other one does — through the release that ends both.
	eventually(t, "a session dropped by a failed dial is released, not merely unslotted", func() bool {
		return indexOf(cli.callLog(), "disconnect") >= 0
	})
}

// TestStart_RefusesAnAmbiguousDeviceStore: two or more stored devices and no
// matching selector has no correct answer, so nothing is resumed.
func TestStart_RefusesAnAmbiguousDeviceStore(t *testing.T) {
	cli := newFakeClient()
	m, _, _, _ := newTestManager(t, cli, true)
	m.setSessionFactory(func(context.Context, sessionRequest) (*session, error) {
		return nil, errors.Join(ErrDeviceStoreAmbiguous, errors.New("2 stored devices"))
	})

	require.NoError(t, m.Start(context.Background()))

	status := m.Status()
	assert.Equal(t, StateError, status.State)
	assert.Equal(t, ReasonDeviceStoreAmbiguous, status.Reason)
	assert.NotContains(t, cli.callLog(), "connect",
		"there is no branch that picks a device by luck")
}

// --- event handling --------------------------------------------------------

func TestHandleEvent_ConnectedSetsConnected(t *testing.T) {
	m, syncStore, _, _ := newTestManager(t, newFakeClient(), true)
	require.NoError(t, m.Start(context.Background()))

	assert.True(t, dispatchEvent(t, m, nil, &events.Connected{}))
	assert.Equal(t, StateConnected, m.Status().State)
	assert.Contains(t, syncStore.callLog(), "status:idle")
}

func TestHandleEvent_DisconnectedSetsReconnecting(t *testing.T) {
	m, _, _, _ := newTestManager(t, newFakeClient(), true)
	require.NoError(t, m.Start(context.Background()))
	require.True(t, dispatchEvent(t, m, nil, &events.Connected{}))

	assert.True(t, dispatchEvent(t, m, nil, &events.Disconnected{}))
	assert.Equal(t, StateReconnecting, m.Status().State,
		"whatsmeow retries transient failures on its own; no user action is required")
}

// TestHandleEvent_PairSuccessAdoptsButAwaitsTheConnection pins the library's own
// rule: PairSuccess "is generally followed by a websocket reconnection, so you
// should wait for the Connected".
func TestHandleEvent_PairSuccessAdoptsButAwaitsTheConnection(t *testing.T) {
	cli := newFakeClient()
	m, syncStore, _, _ := newTestManager(t, cli, false)
	jid := types.NewJID("15551234567", types.DefaultUserServer)

	// Start first so the sync-state row exists: without it the status writes are
	// no-ops and "the sync error is not cleared yet" would pass vacuously.
	require.NoError(t, m.Start(context.Background()))

	require.NoError(t, m.StartPairing(context.Background(), PairRequest{Method: PairMethodQR}))
	pairingSess := pairingSession(t, m)
	require.NotNil(t, pairingSess)

	assert.True(t, dispatchEvent(t, m, pairingSess, &events.PairSuccess{ID: jid}))

	status := m.Status()
	assert.Equal(t, StateConnecting, status.State,
		"PairSuccess is not a live connection; the library reconnects after it")
	assert.Nil(t, status.Pairing, "the pairing slot is released either way")
	assert.Nil(t, status.ConnectedAt, "nothing has connected yet")
	require.NotNil(t, status.JID)
	assert.Equal(t, jid.String(), *status.JID)
	require.NotNil(t, status.PhoneNumber)
	assert.Equal(t, "15551234567", *status.PhoneNumber)
	assert.NotContains(t, syncStore.callLog(), "status:idle",
		"the sync error is cleared by a real connection, not by a completed handshake")
	assert.Equal(t, jid.String(), syncStore.linkedJID(),
		"the adoption records WHICH device is linked, so the restart path is deterministic")

	// The adopted session is the one that then reports Connected.
	require.Same(t, pairingSess, installedSession(t, m), "the pairing client becomes the installed session")
	assert.True(t, dispatchEvent(t, m, pairingSess, &events.Connected{}))

	status = m.Status()
	assert.Equal(t, StateConnected, status.State)
	assert.NotNil(t, status.ConnectedAt)
	assert.Contains(t, syncStore.callLog(), "status:idle")
}

// TestHandleEvent_LoggedOutIsTerminalWithReason covers the whole
// permanent-disconnect contract, including that the reason is persisted BEFORE
// the client is torn down.
func TestHandleEvent_LoggedOutIsTerminalWithReason(t *testing.T) {
	tests := []struct {
		name       string
		event      any
		wantReason string
		wantBanned bool
	}{
		{"logged out", &events.LoggedOut{}, ReasonLoggedOut, false},
		{"stream replaced", &events.StreamReplaced{}, ReasonStreamReplaced, false},
		{"client outdated", &events.ClientOutdated{}, ReasonClientOutdated, false},
		{"temporary ban", &events.TemporaryBan{Expire: 24 * time.Hour}, ReasonTemporaryBan, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cli := newFakeClient()
			m, syncStore, _, _ := newTestManager(t, cli, true)
			require.NoError(t, m.Start(context.Background()))
			require.True(t, dispatchEvent(t, m, nil, &events.Connected{}))

			assert.True(t, dispatchEvent(t, m, nil, tt.event))

			status := m.Status()
			assert.Equal(t, StateDisconnected, status.State)
			assert.Equal(t, tt.wantReason, status.Reason)
			assert.Equal(t, tt.wantReason, syncStore.terminalReason(),
				"the reason must be durable, or a restart would retry a dead session")

			if tt.wantBanned {
				require.NotNil(t, status.BannedUntil)
				assert.WithinDuration(t, accelerated.GetCurrentTime().Add(24*time.Hour), *status.BannedUntil, time.Minute)
			}

			// The decision is written before the teardown disconnect, and the
			// reason and the error status land together.
			calls := syncStore.callLog()
			require.GreaterOrEqual(t, indexOf(calls, "terminal"), 0, "the terminal reason must be persisted")
			assert.Equal(t, repository.SyncStatusError, syncStore.persistedStatus())
			assert.False(t, syncStore.terminalButIdle())
			eventually(t, "the dead client is torn down", func() bool {
				return indexOf(cli.callLog(), "disconnect") >= 0
			})
		})
	}
}

func TestHandleEvent_MessageForwardsToIngestor(t *testing.T) {
	m, _, ingestor, _ := newTestManager(t, newFakeClient(), true)
	seedSelfJID(m, testOwnPN)

	assert.True(t, m.handleEvent(newMessageEvent("msg-42")))
	require.Equal(t, 1, ingestor.count())
	assert.Equal(t, "msg-42", ingestor.first().MessageID)
}

// TestHandleEvent_MessageForwardsParsedFieldsToIngestor is the end-to-end check
// that the handler hands the ingestor a REAL projection rather than the
// four-field stub it used to.
func TestHandleEvent_MessageForwardsParsedFieldsToIngestor(t *testing.T) {
	m, _, ingestor, _ := newTestManager(t, newFakeClient(), true)
	seedSelfJID(m, testOwnPN)

	require.True(t, m.handleEvent(newMessageEvent("msg-42")))
	require.Equal(t, 1, ingestor.count())

	got := ingestor.first()
	assert.Equal(t, ChatTypePrivate, got.ChatType)
	assert.Equal(t, MessageTypeText, got.MessageType)
	require.NotNil(t, got.Body)
	assert.Equal(t, "hello", *got.Body)
	require.NotNil(t, got.PeerJID)
	assert.Equal(t, testPeerPN.String(), *got.PeerJID)
	require.NotNil(t, got.PeerPhoneE164)
	assert.Equal(t, "+15559876543", *got.PeerPhoneE164)
	require.NotNil(t, got.PushName)
	assert.Equal(t, "Peer", *got.PushName)
	require.NotNil(t, got.AccountJID)
	assert.Equal(t, testOwnPN.String(), *got.AccountJID)
}

// TestHandleEvent_UnknownOwnIdentityWithholdsAck: without an own JID the parser
// can neither reject a self-chat nor decide a DM's direction, and WhatsApp does
// not redeliver an acked message — so a drop here would be irreversible.
func TestHandleEvent_UnknownOwnIdentityWithholdsAck(t *testing.T) {
	m, _, ingestor, _ := newTestManager(t, newFakeClient(), true)
	// Deliberately no seedSelfJID.

	assert.False(t, m.handleEvent(newMessageEvent("msg-1")))
	assert.Zero(t, ingestor.count(), "nothing may be staged against an unknown account")
}

// TestHandleEvent_IneligibleChatAcksWithoutIngesting: ineligible is not failure.
func TestHandleEvent_IneligibleChatAcksWithoutIngesting(t *testing.T) {
	for _, chat := range []types.JID{
		types.StatusBroadcastJID,
		types.NewJID("123", types.NewsletterServer),
		testOwnPN, // the self-chat
	} {
		t.Run(chat.String(), func(t *testing.T) {
			m, _, ingestor, _ := newTestManager(t, newFakeClient(), true)
			seedSelfJID(m, testOwnPN)

			evt := newMessageEvent("msg-1")
			evt.Info.Chat = chat

			assert.True(t, m.handleEvent(evt), "withholding here would redeliver forever")
			assert.Zero(t, ingestor.count())
		})
	}
}

// TestHandleEvent_PanicWithholdsAck is the guard for the library's own
// dispatcher behaviour: it recovers a panicking handler and returns its named
// result at the zero value, which reads as "handled successfully" — so without
// this recover a panic would ACK and lose the message.
func TestHandleEvent_PanicWithholdsAck(t *testing.T) {
	m, _, _, _ := newTestManager(t, newFakeClient(), true)
	seedSelfJID(m, testOwnPN)
	m.SetIngestor(panickingIngestor{})

	assert.False(t, m.handleEvent(newMessageEvent("msg-boom")),
		"a panic on the ingest path must withhold the ack, not silently acknowledge")
}

// TestSetIngestor_BindsGroupInfoSource pins the ordering the group gate depends
// on: the seam is bound by SetIngestor, which runs before the single Start, so
// there is no window in which a connected client has an ingestor with no
// source.
func TestSetIngestor_BindsGroupInfoSource(t *testing.T) {
	m := newManagerForTest(t, newFakeSyncStore(), &fakeBackfillReader{})
	binder := &recordingBinder{}

	m.SetIngestor(binder)
	require.NotNil(t, binder.src, "an ingestor cannot be installed without its group-info source")
	assert.Nil(t, binder.src(), "and the bound source reports nil while nothing is connected")
}

func TestStatus_ReportsUnresolvedLIDCount(t *testing.T) {
	m, _, _, _ := newTestManager(t, newFakeClient(), true)
	seedSelfJID(m, testOwnPN)

	assert.Zero(t, m.Status().Ingest.UnresolvedLIDPeers, "the count is meaningful as zero")

	for _, peer := range []string{"88800000002@lid", "88800000003@lid", "88800000002@lid"} {
		evt := newMessageEvent("msg-" + peer)
		evt.Info.Chat = mustParseTestJID(t, peer)
		require.True(t, m.handleEvent(evt))
	}

	assert.Equal(t, 2, m.Status().Ingest.UnresolvedLIDPeers,
		"DISTINCT peers: a per-message counter would report volume under a field named for peers")
}

// TestPeerResolution_UnresolvedLIDIncrementsCounter is the manager-side half of
// the counter: the parser reports the peer, the manager records it once.
func TestPeerResolution_UnresolvedLIDIncrementsCounter(t *testing.T) {
	m, _, _, _ := newTestManager(t, newFakeClient(), true)
	seedSelfJID(m, testOwnPN)

	m.noteUnresolvedLID("88800000002@lid")
	m.noteUnresolvedLID("88800000002@lid")
	assert.Equal(t, 1, m.ingestStatus().UnresolvedLIDPeers)

	m.noteUnresolvedLID("88800000003@lid")
	assert.Equal(t, 2, m.ingestStatus().UnresolvedLIDPeers)
}

func mustParseTestJID(t *testing.T, s string) types.JID {
	t.Helper()
	jid, err := types.ParseJID(s)
	require.NoError(t, err)
	return jid
}

// panickingIngestor is the deliberate defect the panic guard exists for.
type panickingIngestor struct{}

func (panickingIngestor) IngestMessage(context.Context, IngestedMessage) error {
	panic("ingest exploded")
}

// recordingBinder is a MessageIngestor that also implements GroupInfoBinder, so
// the bind can be observed.
type recordingBinder struct {
	src func() GroupInfoFetcher
}

func (b *recordingBinder) IngestMessage(context.Context, IngestedMessage) error { return nil }
func (b *recordingBinder) BindGroupInfoSource(src func() GroupInfoFetcher)      { b.src = src }

// TestHandleEvent_MessageIngestErrorWithholdsAck is what makes an unprocessable
// message a redelivery rather than a silent drop.
func TestHandleEvent_MessageIngestErrorWithholdsAck(t *testing.T) {
	m, _, ingestor, _ := newTestManager(t, newFakeClient(), true)
	seedSelfJID(m, testOwnPN)
	ingestor.setErr(errors.New("database down"))

	assert.False(t, m.handleEvent(newMessageEvent("msg-1")))
}

// TestHandleEvent_HistorySyncEventIsUnexpected makes a lost-flags regression
// loud rather than silent: manual mode should make this event unreachable.
func TestHandleEvent_HistorySyncEventIsUnexpected(t *testing.T) {
	m, _, _, _ := newTestManager(t, newFakeClient(), true)
	assert.False(t, m.handleEvent(&events.HistorySync{}),
		"an arriving HistorySync means the manual flags were lost and the library is deleting our history")
}

func TestHandleEvent_UnknownEventIsIgnored(t *testing.T) {
	m, _, _, _ := newTestManager(t, newFakeClient(), true)
	type unrelated struct{}
	assert.True(t, m.handleEvent(&unrelated{}))
}

// --- history notification capture ------------------------------------------

// TestHandleEvent_HistoryNotificationRecordedBeforeReturning proves the capture
// is synchronous and does exactly one thing.
//
// Synchronicity is proved by holding the recorder inside its call and observing
// that handleEvent has NOT returned: a detached capture would let the handler
// return — and the ack fire — while the chunk was still unpersisted.
func TestHandleEvent_HistoryNotificationRecordedBeforeReturning(t *testing.T) {
	m, _, ingestor, recorder := newTestManager(t, newFakeClient(), true)
	recorder.entered = make(chan struct{})
	recorder.block = make(chan struct{})
	entered := recorder.entered

	returned := make(chan bool, 1)
	evt := newHistoryNotificationEvent("proto-1", nil)
	go func() { returned <- m.handleEvent(evt) }()

	<-entered
	select {
	case <-returned:
		t.Fatal("handleEvent returned while the recorder was still in flight — the capture is not synchronous")
	case <-time.After(50 * time.Millisecond):
	}
	close(recorder.block)
	assert.True(t, <-returned)

	recorded := recorder.all()
	require.Len(t, recorded, 1)
	assert.Equal(t, "proto-1", recorded[0].ProtocolMsgID)
	assert.Equal(t, repository.HistoryDispositionProject, recorded[0].Disposition)

	assert.Equal(t, 0, ingestor.count(), "a history notification is never projected here")

	// The stored bytes round-trip to the notification.
	var parsed waE2E.HistorySyncNotification
	require.NoError(t, proto.Unmarshal(recorded[0].Notification, &parsed))
	assert.Equal(t, "direct/path/1", parsed.GetDirectPath())
	assert.Equal(t, "enc-handle-1", parsed.GetEncHandle())
}

// TestHandleEvent_HistoryNotificationRecordFailureWithholdsAck: nothing was
// downloaded, acked or deleted, and manual mode leaves the media on the server,
// so withholding the ack genuinely recovers.
func TestHandleEvent_HistoryNotificationRecordFailureWithholdsAck(t *testing.T) {
	m, _, _, recorder := newTestManager(t, newFakeClient(), true)
	recorder.setErr(errors.New("insert failed"))

	assert.False(t, m.handleEvent(newHistoryNotificationEvent("proto-1", nil)))
	assert.Empty(t, recorder.all())
}

func TestHandleEvent_HistoryNotificationWithoutRecorderWithholdsAck(t *testing.T) {
	m := newManagerForTest(t, newFakeSyncStore(), &fakeBackfillReader{})
	assert.False(t, m.handleEvent(newHistoryNotificationEvent("proto-1", nil)),
		"a missing recorder must never ack a one-shot history chunk away")
}

// TestHandleEvent_InlineBootstrapChunkIsDroppedNotStored pins the ratified
// exception: a bootstrap chunk the server inlines against our explicit
// non-inline request is dropped un-projected rather than persisted.
func TestHandleEvent_InlineBootstrapChunkIsDroppedNotStored(t *testing.T) {
	m, _, ingestor, recorder := newTestManager(t, newFakeClient(), true)

	secret := []byte("PRE-CLAMP-HISTORY-PAYLOAD")
	assert.True(t, m.handleEvent(newHistoryNotificationEvent("proto-inline", secret)))

	recorded := recorder.all()
	require.Len(t, recorded, 1)
	assert.Equal(t, repository.HistoryDispositionDroppedInline, recorded[0].Disposition)
	assert.NotContains(t, string(recorded[0].Notification), string(secret),
		"the inline payload must never reach the database, even transiently")
	assert.Equal(t, 0, ingestor.count())
}

// TestHandleEvent_StoredNotificationNeverContainsPayloadBytes asserts the
// stripping on EVERY path, media-backed and inlined alike.
func TestHandleEvent_StoredNotificationNeverContainsPayloadBytes(t *testing.T) {
	tests := []struct {
		name    string
		payload []byte
		want    string
	}{
		{"media backed", nil, repository.HistoryDispositionProject},
		{"inlined", []byte("PRE-CLAMP-HISTORY-PAYLOAD"), repository.HistoryDispositionDroppedInline},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, _, _, recorder := newTestManager(t, newFakeClient(), true)
			require.True(t, m.handleEvent(newHistoryNotificationEvent("proto-1", tt.payload)))

			recorded := recorder.all()
			require.Len(t, recorded, 1)
			assert.Equal(t, tt.want, recorded[0].Disposition)

			var parsed waE2E.HistorySyncNotification
			require.NoError(t, proto.Unmarshal(recorded[0].Notification, &parsed))
			assert.Nil(t, parsed.InitialHistBootstrapInlinePayload,
				"the persisted notification is a media pointer, never message content")
		})
	}
}

// TestHandleEvent_HistoryNotificationDoesNotMutateTheEvent proves the strip
// operates on a clone.
func TestHandleEvent_HistoryNotificationDoesNotMutateTheEvent(t *testing.T) {
	m, _, _, _ := newTestManager(t, newFakeClient(), true)

	payload := []byte("PRE-CLAMP-HISTORY-PAYLOAD")
	evt := newHistoryNotificationEvent("proto-inline", payload)
	require.True(t, m.handleEvent(evt))

	original := evt.RawMessage.GetProtocolMessage().GetHistorySyncNotification()
	assert.Equal(t, payload, original.GetInitialHistBootstrapInlinePayload())
}

// --- status ----------------------------------------------------------------

func TestStatus_ReportsBackfillCounts(t *testing.T) {
	floor := time.Date(2026, 3, 4, 5, 0, 0, 0, time.UTC)
	reader := &fakeBackfillReader{
		counts: map[string]int{
			"pending/project":        2,
			"processing/project":     1,
			"failed/project":         3,
			"done/dropped_inline":    4,
			"pending/dropped_inline": 1,
			"done/project":           9,
			"malformed-key-no-slash": 7,
		},
		floor: &floor,
	}
	m, _, _, _, _ := newTestManagerFull(t, newFakeClient(), true, reader)

	backfill := m.Status().Backfill
	assert.Equal(t, 3, backfill.Pending)
	assert.Equal(t, 1, backfill.Processing)
	assert.Equal(t, 3, backfill.Failed)
	assert.Equal(t, 5, backfill.DroppedInlineChunks,
		"the dropped count sums every state, since a dropped chunk still runs the phase machine")
	require.NotNil(t, backfill.ObservedFloorAt)
	assert.Equal(t, floor, *backfill.ObservedFloorAt)
	assert.False(t, backfill.Stale, "a successful read is not stale")
}

func TestStatus_ReportsNotReadyReason(t *testing.T) {
	m := newNotReadyManager(t)
	require.NoError(t, m.Start(context.Background()))

	status := m.Status()
	assert.True(t, status.Configured)
	assert.Equal(t, StateNotReady, status.State)
	assert.Equal(t, ReasonIngestNotWired, status.Reason)
}

func TestSelfJID(t *testing.T) {
	cli := newFakeClient()
	m, _, _, _ := newTestManager(t, cli, false)
	_, ok := m.SelfJID()
	assert.False(t, ok)

	jid := types.NewJID("15551234567", types.DefaultUserServer)
	require.NoError(t, m.StartPairing(context.Background(), PairRequest{Method: PairMethodQR}))
	require.True(t, dispatchEvent(t, m, pairingSession(t, m), &events.PairSuccess{ID: jid}))

	got, ok := m.SelfJID()
	require.True(t, ok)
	assert.Equal(t, jid.String(), got.String())
}

// --- helpers ---------------------------------------------------------------

func indexOf(haystack []string, needle string) int {
	for i, s := range haystack {
		if s == needle {
			return i
		}
	}
	return -1
}

func countCalls(haystack []string, needle string) int {
	n := 0
	for _, s := range haystack {
		if s == needle {
			n++
		}
	}
	return n
}

// newMessageEvent builds an inbound direct message. Message and RawMessage
// carry the same content: the parser reads the UNWRAPPED Message, while the
// history-notification branch upstream reads RawMessage.
func newMessageEvent(id string) *events.Message {
	body := "hello"
	return &events.Message{
		Info: types.MessageInfo{
			ID: id,
			MessageSource: types.MessageSource{
				Chat: types.NewJID("15559876543", types.DefaultUserServer),
			},
			Timestamp: time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC),
			PushName:  "Peer",
		},
		Message:    &waE2E.Message{Conversation: proto.String(body)},
		RawMessage: &waE2E.Message{Conversation: proto.String(body)},
	}
}

func newHistoryNotificationEvent(protocolMsgID string, inlinePayload []byte) *events.Message {
	notif := &waE2E.HistorySyncNotification{
		DirectPath:                        proto.String("direct/path/1"),
		EncHandle:                         proto.String("enc-handle-1"),
		FileEncSHA256:                     []byte{1, 2, 3},
		MediaKey:                          []byte{4, 5, 6},
		SyncType:                          waE2E.HistorySyncType_INITIAL_BOOTSTRAP.Enum(),
		ChunkOrder:                        proto.Uint32(1),
		OldestMsgInChunkTimestampSec:      proto.Int64(time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC).Unix()),
		InitialHistBootstrapInlinePayload: inlinePayload,
	}
	return &events.Message{
		Info: types.MessageInfo{
			ID: protocolMsgID,
			MessageSource: types.MessageSource{
				Chat: types.NewJID("15559876543", types.DefaultUserServer),
			},
			Timestamp: time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC),
		},
		RawMessage: &waE2E.Message{
			ProtocolMessage: &waE2E.ProtocolMessage{HistorySyncNotification: notif},
		},
	}
}

// TestHandleEvent_TerminalReasonWriteFailureIsSurfaced covers the path a fake
// that cannot fail could never reach.
func TestHandleEvent_TerminalReasonWriteFailureIsSurfaced(t *testing.T) {
	cli := newFakeClient()
	m, syncStore, _, _ := newTestManager(t, cli, true)
	require.NoError(t, m.Start(context.Background()))
	require.True(t, dispatchEvent(t, m, nil, &events.Connected{}))

	syncStore.resetCalls()
	syncStore.setErr(func(f *fakeSyncStore) { f.terminalErr = errors.New("database is down") })

	assert.True(t, dispatchEvent(t, m, nil, &events.LoggedOut{}))

	status := m.Status()
	assert.Equal(t, StateDisconnected, status.State)
	assert.Equal(t, ReasonLoggedOut, status.Reason)
	require.NotNil(t, status.TerminalReasonPersisted,
		"the field must be PRESENT — omitting it hides exactly the case a client must act on")
	assert.False(t, *status.TerminalReasonPersisted,
		"the status must admit the decision will not survive a restart")

	assert.Empty(t, syncStore.terminalReason(), "nothing was durably recorded")
	eventually(t, "the dead session is torn down anyway", func() bool {
		return indexOf(cli.callLog(), "disconnect") >= 0
	})

	// The write is retried rather than abandoned on the first blip.
	assert.Equal(t, terminalPersistAttempts, countCalls(syncStore.callLog(), "terminal"))
}

// TestHandleEvent_TerminalDecisionIsOneDurableWrite closes the lost-breach gap.
func TestHandleEvent_TerminalDecisionIsOneDurableWrite(t *testing.T) {
	t.Run("both land together", func(t *testing.T) {
		m, syncStore, _, _ := newTestManager(t, newFakeClient(), true)
		require.NoError(t, m.Start(context.Background()))
		require.True(t, dispatchEvent(t, m, nil, &events.Connected{}))
		syncStore.resetCalls()

		require.True(t, dispatchEvent(t, m, nil, &events.LoggedOut{}))

		assert.Equal(t, 1, countCalls(syncStore.callLog(), "terminal"),
			"one operation carries both halves of the decision")
		assert.Equal(t, 0, countCalls(syncStore.callLog(), "metadata"),
			"a metadata-only write is the shape that can strand a terminal reason on an idle row")
		assert.Equal(t, ReasonLoggedOut, syncStore.terminalReason())
		assert.Equal(t, repository.SyncStatusError, syncStore.persistedStatus())
		assert.False(t, syncStore.terminalButIdle(),
			"the watchdog only breaches on error+reason, so this row must never exist")
	})

	t.Run("failure injection leaves nothing partial", func(t *testing.T) {
		m, syncStore, _, _ := newTestManager(t, newFakeClient(), true)
		require.NoError(t, m.Start(context.Background()))
		require.True(t, dispatchEvent(t, m, nil, &events.Connected{}))

		syncStore.setErr(func(f *fakeSyncStore) { f.terminalErr = errors.New("database is down") })

		require.True(t, dispatchEvent(t, m, nil, &events.LoggedOut{}))

		assert.False(t, syncStore.terminalButIdle(),
			"a failed decision writes neither half, so it can never end terminal-but-idle")
		assert.Empty(t, syncStore.terminalReason())
		persisted := m.Status().TerminalReasonPersisted
		require.NotNil(t, persisted)
		assert.False(t, *persisted, "the status admits the decision is not durable")
	})
}

// --- session-scoped lifecycle events ---------------------------------------

// TestHandleEvent_TerminalFromAbandonedClientIsIgnored is the stale-terminal
// regression guard.
func TestHandleEvent_TerminalFromAbandonedClientIsIgnored(t *testing.T) {
	terminals := []struct {
		name  string
		event any
	}{
		{"logged out", &events.LoggedOut{}},
		{"stream replaced", &events.StreamReplaced{}},
		{"client outdated", &events.ClientOutdated{}},
		{"temporary ban", &events.TemporaryBan{Expire: time.Hour}},
	}

	for _, tt := range terminals {
		t.Run(tt.name, func(t *testing.T) {
			cli := newFakeClient()
			m, syncStore, _, _ := newTestManager(t, cli, true)
			require.NoError(t, m.Start(context.Background()))
			require.True(t, dispatchEvent(t, m, nil, &events.Connected{}))
			require.Equal(t, StateConnected, m.Status().State)

			// A client from an attempt the manager abandoned long ago.
			orphanClient := newFakeClient()
			orphan := &session{client: orphanClient, retired: true}
			syncStore.resetCalls()

			assert.True(t, dispatchEvent(t, m, orphan, tt.event))

			assert.Equal(t, StateConnected, m.Status().State,
				"a dead client may not publish the live session as disconnected")
			assert.Empty(t, syncStore.terminalReason(),
				"a dead client's verdict must never become the durable decision")
			assert.Equal(t, 0, countCalls(syncStore.callLog(), "terminal"))
			assert.NotContains(t, cli.callLog(), "disconnect",
				"the installed session must not be torn down by someone else's event")
			eventually(t, "the orphan is torn down rather than left holding a socket", func() bool {
				return indexOf(orphanClient.callLog(), "disconnect") >= 0
			})
		})
	}
}

// TestHandleEvent_TerminalFromPairingClientDoesNotSpeakForTheSession: a pairing
// attempt that dies is a different device from the installed session.
func TestHandleEvent_TerminalFromPairingClientDoesNotSpeakForTheSession(t *testing.T) {
	installed := newFakeClient()
	m, syncStore, _, _ := newTestManager(t, installed, true)
	require.NoError(t, m.Start(context.Background()))
	require.True(t, dispatchEvent(t, m, nil, &events.Connected{}))

	pairingClient := newFakeClient()
	useClient(m, pairingClient, false)
	installed.setConnected(false)
	require.NoError(t, m.StartPairing(context.Background(), PairRequest{Method: PairMethodQR}))
	pairingSess := pairingSession(t, m)
	require.NotNil(t, pairingSess)
	syncStore.resetCalls()

	assert.True(t, dispatchEvent(t, m, pairingSess, &events.LoggedOut{}))

	assert.Nil(t, m.Status().Pairing, "the dead attempt releases its slot")
	eventually(t, "the attempt's half-written device is removed", func() bool {
		return indexOf(pairingClient.callLog(), "delete_device") >= 0
	})
	assert.Empty(t, syncStore.terminalReason(),
		"the installed session's restart gate is not the pairing client's to write")
	assert.Equal(t, 0, countCalls(syncStore.callLog(), "terminal"))
	assert.NotContains(t, installed.callLog(), "disconnect")
}

// TestHandleEvent_TerminalOnTheSessionLeavesAnInFlightPairingAlone is the
// converse: the installed session ending is not a reason to destroy a pairing
// the user is in the middle of.
func TestHandleEvent_TerminalOnTheSessionLeavesAnInFlightPairingAlone(t *testing.T) {
	installed := newFakeClient()
	m, _, _, _ := newTestManager(t, installed, true)
	require.NoError(t, m.Start(context.Background()))
	require.True(t, dispatchEvent(t, m, nil, &events.Connected{}))
	require.True(t, dispatchEvent(t, m, nil, &events.Disconnected{}))
	installed.setConnected(false)

	installedSess := installedSession(t, m)
	require.NotNil(t, installedSess)

	pairingClient := newFakeClient()
	useClient(m, pairingClient, false)
	require.NoError(t, m.StartPairing(context.Background(), PairRequest{Method: PairMethodQR}))
	require.NotNil(t, m.Status().Pairing)

	assert.True(t, dispatchEvent(t, m, installedSess, &events.LoggedOut{}))

	assert.NotNil(t, m.Status().Pairing, "the user's in-flight pairing survives")
	assert.NotContains(t, pairingClient.callLog(), "delete_device")
	eventually(t, "the terminated session is torn down", func() bool {
		return indexOf(installed.callLog(), "disconnect") >= 0
	})
}

// TestHandleEvent_DisconnectedFromAbandonedClientIsIgnored: a socket dropping on
// a client nobody owns says nothing about the installed session.
func TestHandleEvent_DisconnectedFromAbandonedClientIsIgnored(t *testing.T) {
	m, _, _, _ := newTestManager(t, newFakeClient(), true)
	require.NoError(t, m.Start(context.Background()))
	require.True(t, dispatchEvent(t, m, nil, &events.Connected{}))

	orphan := &session{client: newFakeClient(), retired: true}
	assert.True(t, dispatchEvent(t, m, orphan, &events.Disconnected{}))
	assert.Equal(t, StateConnected, m.Status().State,
		"the live session is still connected; someone else's socket dropped")
}

// TestHandleEvent_DisconnectedFromPairingClientIsIgnored: the pairing websocket
// dropping is the pairing's business, not the installed session's state.
func TestHandleEvent_DisconnectedFromPairingClientIsIgnored(t *testing.T) {
	installed := newFakeClient()
	m, _, _, _ := newTestManager(t, installed, true)
	require.NoError(t, m.Start(context.Background()))
	require.True(t, dispatchEvent(t, m, nil, &events.Connected{}))
	installedSess := installedSession(t, m)
	installed.setConnected(false)

	pairingClient := newFakeClient()
	useClient(m, pairingClient, false)
	require.NoError(t, m.StartPairing(context.Background(), PairRequest{Method: PairMethodQR}))
	pairingSess := pairingSession(t, m)
	require.NotNil(t, pairingSess)

	// StartPairing publishes "pairing" over the connection state while the
	// installed session is still live underneath it. One status field cannot show
	// both, so the connection state is restored through the installed session's
	// own Connected — otherwise the assertion would pass on any implementation,
	// since onDisconnected only ever downgrades a CONNECTED state.
	require.True(t, dispatchEvent(t, m, installedSess, &events.Connected{}))
	require.Equal(t, StateConnected, m.Status().State)

	assert.True(t, dispatchEvent(t, m, pairingSess, &events.Disconnected{}))
	assert.Equal(t, StateConnected, m.Status().State,
		"the pairing socket dropping is not the installed session's state to lose")
}

// TestNewClient_HandlerIsBoundToItsOwnSession is the discriminating proof of the
// registration itself, through a REAL client.
//
// The other identity tests call handleEventFor directly, so replacing newClient's
// closure with the unattributed m.handleEvent would leave every one of them
// green while production silently lost all attribution.
func TestNewClient_HandlerIsBoundToItsOwnSession(t *testing.T) {
	m, syncStore, _, _ := newTestManager(t, newFakeClient(), true)
	require.NoError(t, m.Start(context.Background()))
	require.Equal(t, StateConnecting, m.Status().State)
	syncStore.resetCalls()

	// A real client for a session the manager does not own.
	orphan := &session{}
	cli := m.newClient(&store.Device{}, orphan)
	require.NotNil(t, cli)
	orphan.client = cli
	orphan.wa = cli

	//nolint:staticcheck // DangerousInternals is the only way to reach the real dispatcher.
	cli.DangerousInternals().DispatchEvent(&events.Connected{})
	m.settle()

	assert.Equal(t, StateConnecting, m.Status().State,
		"an orphan client's Connected must not publish the manager as connected")
	assert.NotContains(t, syncStore.callLog(), "status:idle")
}

// TestHandleEvent_TerminalReasonPersistedOnHappyPath is the positive control.
func TestHandleEvent_TerminalReasonPersistedOnHappyPath(t *testing.T) {
	m, _, _, _ := newTestManager(t, newFakeClient(), true)
	require.NoError(t, m.Start(context.Background()))
	require.True(t, dispatchEvent(t, m, nil, &events.Connected{}))

	require.True(t, dispatchEvent(t, m, nil, &events.LoggedOut{}))
	persisted := m.Status().TerminalReasonPersisted
	require.NotNil(t, persisted)
	assert.True(t, *persisted)
}

// TestStart_FailsClosedWhenSyncStateUnreadable: the persisted terminal reason is
// the only thing holding a dead or banned device back.
func TestStart_FailsClosedWhenSyncStateUnreadable(t *testing.T) {
	cli := newFakeClient()
	m, syncStore, _, _ := newTestManager(t, cli, true)
	syncStore.setErr(func(f *fakeSyncStore) { f.getErr = errors.New("database is down") })

	require.NoError(t, m.Start(context.Background()), "a WhatsApp failure never aborts boot")

	assert.Equal(t, StateError, m.Status().State)
	assert.NotContains(t, cli.callLog(), "connect",
		"an unreadable terminal decision must not be read as an absent one")
}

// TestStatus_NamesTheMissingDependency: reporting not_ready without saying what
// is missing tells the operator nothing they can act on.
func TestStatus_NamesTheMissingDependency(t *testing.T) {
	tests := []struct {
		name  string
		build func(*testing.T) *Manager
		want  string
	}{
		{
			name: "missing ingestor",
			build: func(t *testing.T) *Manager {
				m := newNotReadyManager(t)
				m.SetHistoryDrainReady()
				return m
			},
			want: "message ingestor",
		},
		{
			name: "missing recorder",
			build: func(t *testing.T) *Manager {
				m := newManagerForTest(t, newFakeSyncStore(), &fakeBackfillReader{})
				m.SetIngestor(&fakeIngestor{})
				m.SetHistoryDrainReady()
				return m
			},
			want: "history notification recorder",
		},
		{
			name: "missing drainer",
			build: func(t *testing.T) *Manager {
				m := newManagerForTest(t, newFakeSyncStore(), &fakeBackfillReader{})
				m.SetIngestor(&fakeIngestor{})
				m.SetHistoryRecorder(&fakeRecorder{})
				return m
			},
			want: "history drain worker",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := tt.build(t)
			m.setSessionFactory(func(context.Context, sessionRequest) (*session, error) {
				return nil, errors.New("must not be reached")
			})

			require.NoError(t, m.Start(context.Background()))
			status := m.Status()
			assert.Equal(t, StateNotReady, status.State)
			assert.Equal(t, ReasonIngestNotWired, status.Reason, "the machine-readable code stays stable")
			assert.Contains(t, status.Missing, tt.want, "the status must name the dependency")

			// A refused pairing records the same detail, on a manager that never
			// ran Start, so the settings page sees it either way.
			fresh := tt.build(t)
			fresh.setSessionFactory(func(context.Context, sessionRequest) (*session, error) {
				return nil, errors.New("must not be reached")
			})
			assert.ErrorIs(t, fresh.StartPairing(context.Background(), PairRequest{Method: PairMethodQR}), ErrIngestNotWired)
			assert.Contains(t, fresh.Status().Missing, tt.want)
		})
	}
}

// TestStart_FailsClosedWhenSyncStateCannotBeCreated is the sibling of the read
// failure: the row is where the terminal decision lives.
func TestStart_FailsClosedWhenSyncStateCannotBeCreated(t *testing.T) {
	cli := newFakeClient()
	m, syncStore, _, _ := newTestManager(t, cli, true)
	syncStore.setErr(func(f *fakeSyncStore) {
		f.exists = false // force the create path
		f.createErr = errors.New("database is down")
	})

	require.NoError(t, m.Start(context.Background()), "a WhatsApp failure never aborts boot")

	assert.Equal(t, StateError, m.Status().State)
	assert.NotContains(t, cli.callLog(), "connect",
		"without a status row there is nowhere to record a terminal decision, so connecting is unsafe")
	assert.Contains(t, syncStore.callLog(), "create", "the create path was actually taken")
}

// TestOnConnected_FromRetiredSessionIsIgnored: the session has been terminally
// handled, so its queued Connected must not clear the terminal record it just
// wrote.
func TestOnConnected_FromRetiredSessionIsIgnored(t *testing.T) {
	cli := newFakeClient()
	m, syncStore, _, _ := newTestManager(t, cli, true)
	require.NoError(t, m.Start(context.Background()))
	require.True(t, dispatchEvent(t, m, nil, &events.Connected{}))

	sess := installedSession(t, m)
	require.NotNil(t, sess)

	require.True(t, dispatchEvent(t, m, sess, &events.LoggedOut{}))
	require.Equal(t, ReasonLoggedOut, syncStore.terminalReason())

	// whatsmeow's reconnect can still deliver a Connected for the socket that
	// was coming up as the logout landed.
	assert.True(t, dispatchEvent(t, m, sess, &events.Connected{}))

	assert.Equal(t, StateDisconnected, m.Status().State,
		"a retired session cannot report itself connected again")
	assert.Equal(t, ReasonLoggedOut, syncStore.terminalReason(),
		"and cannot clear the terminal record that stops the next boot reconnecting")
}

// TestOnPairSuccess_RetiresAndDeletesTheReplacedSession is the re-pair path.
func TestOnPairSuccess_RetiresAndDeletesTheReplacedSession(t *testing.T) {
	oldClient := newFakeClient()
	m, _, _, _ := newTestManager(t, oldClient, true)
	require.NoError(t, m.Start(context.Background()))
	require.True(t, dispatchEvent(t, m, nil, &events.Connected{}))
	require.True(t, dispatchEvent(t, m, nil, &events.Disconnected{}))
	oldClient.setConnected(false)

	oldSess := installedSession(t, m)
	require.NotNil(t, oldSess)

	newClient := newFakeClient()
	useClient(m, newClient, false)
	require.NoError(t, m.StartPairing(context.Background(), PairRequest{Method: PairMethodQR}))
	pairingSess := pairingSession(t, m)
	require.NotNil(t, pairingSess)

	require.True(t, dispatchEvent(t, m, pairingSess, &events.PairSuccess{
		ID: types.NewJID("15559998888", types.DefaultUserServer),
	}))

	assert.Same(t, pairingSess, installedSession(t, m))
	eventually(t, "the replaced device is removed, or the next boot can resume it instead", func() bool {
		return indexOf(oldClient.callLog(), "delete_device") >= 0
	})
	assert.Contains(t, oldClient.callLog(), "disconnect")
	assert.NotContains(t, newClient.callLog(), "delete_device",
		"the device that was just linked is the one that stays")
	eventually(t, "the replaced session's connection context ends with it: it is auto-reconnect's parent, and a device row is being deleted out from under it", func() bool {
		return oldClient.connCtxErr() != nil
	})
	assert.Less(t, indexOf(oldClient.callLog(), "disconnect"), indexOf(oldClient.callLog(), "delete_device"),
		"the client is stopped before its row is removed, or a live client can write the row back")

	// The replaced session's queued events are inert from here.
	assert.True(t, dispatchEvent(t, m, oldSess, &events.LoggedOut{}))
	assert.Equal(t, StateConnecting, m.Status().State)
}

// TestOnPairSuccess_RetainedReplacedDeviceIsRetriedAndSurfaced covers the case
// where the replacement cannot be completed.
func TestOnPairSuccess_RetainedReplacedDeviceIsRetriedAndSurfaced(t *testing.T) {
	oldClient := newFakeClient()
	oldClient.deleteDeviceErr = errors.New("database is down")
	m, _, _, _ := newTestManager(t, oldClient, true)
	require.NoError(t, m.Start(context.Background()))
	require.True(t, dispatchEvent(t, m, nil, &events.Connected{}))
	oldClient.setConnected(false)

	newClient := newFakeClient()
	useClient(m, newClient, false)
	require.NoError(t, m.StartPairing(context.Background(), PairRequest{Method: PairMethodQR}))
	pairingSess := pairingSession(t, m)
	require.NotNil(t, pairingSess)

	require.True(t, dispatchEvent(t, m, pairingSess, &events.PairSuccess{
		ID: types.NewJID("15554443333", types.DefaultUserServer),
	}))

	eventually(t, "one transient blip must not be enough to leave the store holding two sessions", func() bool {
		return countCalls(oldClient.callLog(), "delete_device") == deviceDeleteAttempts
	})
	eventually(t, "a stale stored session is reported, not logged and forgotten", func() bool {
		return m.Status().ReplacedDeviceRetained
	})
}

// TestOnPairSuccess_ClearsTheReplacedDevicesTerminalFields: both fields describe
// a decision taken about the device this pairing replaces.
func TestOnPairSuccess_ClearsTheReplacedDevicesTerminalFields(t *testing.T) {
	oldClient := newFakeClient()
	m, _, _, _ := newTestManager(t, oldClient, true)
	require.NoError(t, m.Start(context.Background()))
	require.True(t, dispatchEvent(t, m, nil, &events.Connected{}))
	require.True(t, dispatchEvent(t, m, nil, &events.TemporaryBan{Expire: 24 * time.Hour}))
	oldClient.setConnected(false)

	before := m.Status()
	require.NotNil(t, before.BannedUntil, "the fixture must actually set both fields")
	require.NotNil(t, before.TerminalReasonPersisted)

	newClient := newFakeClient()
	useClient(m, newClient, false)
	require.NoError(t, m.StartPairing(context.Background(), PairRequest{Method: PairMethodQR}))
	pairingSess := pairingSession(t, m)
	require.NotNil(t, pairingSess)
	require.True(t, dispatchEvent(t, m, pairingSess, &events.PairSuccess{
		ID: types.NewJID("15551112222", types.DefaultUserServer),
	}))

	status := m.Status()
	assert.Equal(t, StateConnecting, status.State)
	assert.Nil(t, status.BannedUntil,
		"the ban belonged to the replaced account, not to the one just linked")
	assert.Nil(t, status.TerminalReasonPersisted,
		"no terminal decision has been taken about the new device")
}

// TestOnPairSuccess_SelectorPersistFailureIsSurfaced: the record of WHICH device
// is linked is what makes the restart path deterministic, so losing it is
// reported rather than logged away — and the pairing is NOT torn down, because
// the device is genuinely linked remotely.
func TestOnPairSuccess_SelectorPersistFailureIsSurfaced(t *testing.T) {
	cli := newFakeClient()
	m, syncStore, _, _ := newTestManager(t, cli, false)
	require.NoError(t, m.Start(context.Background()))

	require.NoError(t, m.StartPairing(context.Background(), PairRequest{Method: PairMethodQR}))
	pairingSess := pairingSession(t, m)
	require.NotNil(t, pairingSess)

	syncStore.setErr(func(f *fakeSyncStore) { f.metadataErr = errors.New("database is down") })

	require.True(t, dispatchEvent(t, m, pairingSess, &events.PairSuccess{
		ID: types.NewJID("15550001111", types.DefaultUserServer),
	}))

	status := m.Status()
	require.NotNil(t, status.LinkSelectorPersisted)
	assert.False(t, *status.LinkSelectorPersisted)
	assert.Equal(t, StateConnecting, status.State, "the link itself is real and stands")
	assert.Same(t, pairingSess, installedSession(t, m), "the pairing is not torn down")
}

// TestOnPairSuccess_SelectorWriteFailureStillDeletesTheReplacedDevice pins the
// counter-intuitive consequence: losing the selector makes the delete MORE
// important, not less. Two rows with a stale-or-absent selector is the one state
// the resolver refuses; one row heals.
func TestOnPairSuccess_SelectorWriteFailureStillDeletesTheReplacedDevice(t *testing.T) {
	oldClient := newFakeClient()
	m, syncStore, _, _ := newTestManager(t, oldClient, true)
	require.NoError(t, m.Start(context.Background()))
	require.True(t, dispatchEvent(t, m, nil, &events.Connected{}))
	oldClient.setConnected(false)

	newClient := newFakeClient()
	useClient(m, newClient, false)
	require.NoError(t, m.StartPairing(context.Background(), PairRequest{Method: PairMethodQR}))
	pairingSess := pairingSession(t, m)
	require.NotNil(t, pairingSess)

	syncStore.setErr(func(f *fakeSyncStore) { f.metadataErr = errors.New("database is down") })

	require.True(t, dispatchEvent(t, m, pairingSess, &events.PairSuccess{
		ID: types.NewJID("15557778888", types.DefaultUserServer),
	}))

	persisted := m.Status().LinkSelectorPersisted
	require.NotNil(t, persisted)
	assert.False(t, *persisted)
	eventually(t, "the replaced device is still deleted, which moves the store to the self-healing state", func() bool {
		return indexOf(oldClient.callLog(), "delete_device") >= 0
	})
}

// TestNoteUnresolvedLID_SaturatesAtTheCap pins the bound on the copy-on-write
// set: it is O(n) per newly seen peer on the library's serialized handler
// goroutine, so it cannot be allowed to grow without limit.
func TestNoteUnresolvedLID_SaturatesAtTheCap(t *testing.T) {
	m := newManagerForTest(t, newFakeSyncStore(), &fakeBackfillReader{})

	for i := range maxUnresolvedLIDPeers + 25 {
		m.noteUnresolvedLID(fmt.Sprintf("%d@lid", i))
	}

	assert.Equal(t, maxUnresolvedLIDPeers, m.ingestStatus().UnresolvedLIDPeers,
		"the count saturates rather than growing without bound")
}
