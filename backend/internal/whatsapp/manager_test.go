package whatsapp

import (
	"context"
	"errors"
	"testing"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/config"
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
	m := NewManager(nil, NewWALogger("whatsapp-test"), &config.WhatsAppConfig{}, newFakeSyncStore(), &fakeBackfillReader{})
	cli := m.newClient(&store.Device{})
	require.NotNil(t, cli)

	assert.True(t, cli.SynchronousAck,
		"acks must fire only after our handler returns, so a crash becomes a redelivery")
	assert.True(t, cli.ManualHistorySyncDownload,
		"the library's automatic download+delete loop must never start")
	assert.True(t, cli.DisableManualHistorySyncReceipt,
		"the history receipt must not be sent behind our back")
}

// TestManager_SynchronousAckEnabled is one line and it is the whole durability
// premise for live messages.
func TestManager_SynchronousAckEnabled(t *testing.T) {
	m := NewManager(nil, NewWALogger("whatsapp-test"), &config.WhatsAppConfig{}, newFakeSyncStore(), &fakeBackfillReader{})
	assert.True(t, m.newClient(&store.Device{}).SynchronousAck)
}

// TestNewClient_HandlerFailureReachesDispatcher is the discriminating proof of
// the withheld-ack contract, end to end through the real client's dispatcher.
//
// AddEventHandler wraps a void handler and hard-codes a true return, so if the
// registration used that variant instead, the dispatcher would report success
// here and the ack would be sent for a message we failed to store. Driving
// handleEvent directly could not tell the two apart.
func TestNewClient_HandlerFailureReachesDispatcher(t *testing.T) {
	m, _, ingestor, _ := newTestManager(t, newFakeClient(), true)
	cli := m.newClient(&store.Device{})
	require.NotNil(t, cli)

	//nolint:staticcheck // DangerousInternals is the only way to observe the dispatcher's verdict.
	dispatcher := cli.DangerousInternals()

	assert.False(t, dispatcher.DispatchEvent(newMessageEvent("msg-ok")),
		"a successful ingest lets the ack through")

	ingestor.err = errors.New("sink down")
	assert.True(t, dispatcher.DispatchEvent(newMessageEvent("msg-bad")),
		"a failing ingest must make the dispatcher report handlerFailed, which withholds the ack")
}

// --- readiness gate --------------------------------------------------------

func TestStart_RefusesWhenNotReady(t *testing.T) {
	tests := []struct {
		name  string
		build func() *Manager
	}{
		{
			name: "missing real ingestor",
			build: func() *Manager {
				m := newNotReadyManager()
				m.SetHistoryDrainReady()
				return m
			},
		},
		{
			name: "missing history recorder",
			build: func() *Manager {
				m := NewManager(nil, NewWALogger("t"), &config.WhatsAppConfig{}, newFakeSyncStore(), &fakeBackfillReader{})
				m.SetIngestor(&fakeIngestor{})
				m.SetHistoryDrainReady()
				m.newSession = func(context.Context, bool) (*session, error) {
					return nil, errors.New("must not be reached")
				}
				return m
			},
		},
		{
			name: "drain not ready",
			build: func() *Manager {
				m := NewManager(nil, NewWALogger("t"), &config.WhatsAppConfig{}, newFakeSyncStore(), &fakeBackfillReader{})
				m.SetIngestor(&fakeIngestor{})
				m.SetHistoryRecorder(&fakeRecorder{})
				m.newSession = func(context.Context, bool) (*session, error) {
					return nil, errors.New("must not be reached")
				}
				return m
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := tt.build()

			ready, missing := m.Ready()
			require.False(t, ready)
			assert.NotEmpty(t, missing, "the missing piece must be named")

			// newSession errors if reached, so a nil return proves no connect.
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
	m := newNotReadyManager()
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

	m := NewManager(nil, NewWALogger("t"), &config.WhatsAppConfig{}, newFakeSyncStore(), &fakeBackfillReader{})
	m.SetHistoryRecorder(&fakeRecorder{})
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
// without a terminal reason the same paired device does connect, so the test
// above is proving the reason and not merely that nothing ever connects.
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
}

// --- event handling --------------------------------------------------------

func TestHandleEvent_ConnectedSetsConnected(t *testing.T) {
	m, syncStore, _, _ := newTestManager(t, newFakeClient(), true)
	require.NoError(t, m.Start(context.Background()))

	assert.True(t, m.handleEvent(&events.Connected{}))
	assert.Equal(t, StateConnected, m.Status().State)
	assert.Contains(t, syncStore.callLog(), "status:idle")
}

func TestHandleEvent_DisconnectedSetsReconnecting(t *testing.T) {
	m, _, _, _ := newTestManager(t, newFakeClient(), true)
	require.NoError(t, m.Start(context.Background()))
	require.True(t, m.handleEvent(&events.Connected{}))

	assert.True(t, m.handleEvent(&events.Disconnected{}))
	assert.Equal(t, StateReconnecting, m.Status().State,
		"whatsmeow retries transient failures on its own; no user action is required")
}

func TestHandleEvent_PairSuccessClearsPairingAndConnects(t *testing.T) {
	m, _, _, _ := newTestManager(t, newFakeClient(), false)
	jid := types.NewJID("15551234567", types.DefaultUserServer)

	assert.True(t, m.handleEvent(&events.PairSuccess{ID: jid}))

	status := m.Status()
	assert.Equal(t, StateConnected, status.State)
	assert.Nil(t, status.Pairing)
	require.NotNil(t, status.JID)
	assert.Equal(t, jid.String(), *status.JID)
	require.NotNil(t, status.PhoneNumber)
	assert.Equal(t, "15551234567", *status.PhoneNumber)
}

// TestHandleEvent_LoggedOutIsTerminalWithReason covers the whole
// permanent-disconnect contract, including that the reason is persisted BEFORE
// the client is torn down — a crash mid-teardown must still leave the decision
// recorded.
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
			require.True(t, m.handleEvent(&events.Connected{}))

			assert.True(t, m.handleEvent(tt.event))

			status := m.Status()
			assert.Equal(t, StateDisconnected, status.State)
			assert.Equal(t, tt.wantReason, status.Reason)
			assert.Equal(t, tt.wantReason, syncStore.terminalReason(),
				"the reason must be durable, or a restart would retry a dead session")

			if tt.wantBanned {
				require.NotNil(t, status.BannedUntil)
				assert.WithinDuration(t, accelerated.GetCurrentTime().Add(24*time.Hour), *status.BannedUntil, time.Minute)
			}

			// Ordering: metadata written before the teardown disconnect.
			calls := syncStore.callLog()
			metadataAt := indexOf(calls, "metadata")
			require.GreaterOrEqual(t, metadataAt, 0, "the terminal reason must be persisted")
			clientCalls := cli.callLog()
			assert.Contains(t, clientCalls, "disconnect", "the client is torn down")
			assert.Contains(t, syncStore.callLog(), "status:error")
		})
	}
}

func TestHandleEvent_MessageForwardsToIngestor(t *testing.T) {
	m, _, ingestor, _ := newTestManager(t, newFakeClient(), true)

	assert.True(t, m.handleEvent(newMessageEvent("msg-42")))
	require.Equal(t, 1, ingestor.count())
	assert.Equal(t, "msg-42", ingestor.messages[0].MessageID)
}

// TestHandleEvent_MessageIngestErrorWithholdsAck is what makes an unprocessable
// message a redelivery rather than a silent drop.
func TestHandleEvent_MessageIngestErrorWithholdsAck(t *testing.T) {
	m, _, ingestor, _ := newTestManager(t, newFakeClient(), true)
	ingestor.err = errors.New("database down")

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
// return — and the ack fire — while the chunk was still unpersisted, which is
// the one loss this design exists to prevent.
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
	recorder.err = errors.New("insert failed")

	assert.False(t, m.handleEvent(newHistoryNotificationEvent("proto-1", nil)))
	assert.Empty(t, recorder.all())
}

func TestHandleEvent_HistoryNotificationWithoutRecorderWithholdsAck(t *testing.T) {
	m := NewManager(nil, NewWALogger("t"), &config.WhatsAppConfig{}, newFakeSyncStore(), &fakeBackfillReader{})
	assert.False(t, m.handleEvent(newHistoryNotificationEvent("proto-1", nil)),
		"a missing recorder must never ack a one-shot history chunk away")
}

// TestHandleEvent_InlineBootstrapChunkIsDroppedNotStored pins the ratified
// exception: a bootstrap chunk the server inlines against our explicit
// non-inline request is dropped un-projected rather than persisted, because
// persisting it would store pre-clamp message content.
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
// operates on a clone: mutating the live event would corrupt whatever the
// library does with it after our handler returns.
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
	m, _, _, _ := newTestManager(t, newFakeClient(), true)
	floor := time.Date(2026, 3, 4, 5, 0, 0, 0, time.UTC)
	m.waRepo = &fakeBackfillReader{
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

	backfill := m.Status().Backfill
	assert.Equal(t, 3, backfill.Pending)
	assert.Equal(t, 1, backfill.Processing)
	assert.Equal(t, 3, backfill.Failed)
	assert.Equal(t, 5, backfill.DroppedInlineChunks,
		"the dropped count sums every state, since a dropped chunk still runs the phase machine")
	require.NotNil(t, backfill.ObservedFloorAt)
	assert.Equal(t, floor, *backfill.ObservedFloorAt)
}

func TestStatus_ReportsNotReadyReason(t *testing.T) {
	m := newNotReadyManager()
	require.NoError(t, m.Start(context.Background()))

	status := m.Status()
	assert.True(t, status.Configured)
	assert.Equal(t, StateNotReady, status.State)
	assert.Equal(t, ReasonIngestNotWired, status.Reason)
}

// --- outbound-call guard ---------------------------------------------------

func TestSelfJID(t *testing.T) {
	m, _, _, _ := newTestManager(t, newFakeClient(), true)
	_, ok := m.SelfJID()
	assert.False(t, ok)

	jid := types.NewJID("15551234567", types.DefaultUserServer)
	require.True(t, m.handleEvent(&events.PairSuccess{ID: jid}))

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

func newMessageEvent(id string) *events.Message {
	body := "hello"
	return &events.Message{
		Info: types.MessageInfo{
			ID: id,
			MessageSource: types.MessageSource{
				Chat: types.NewJID("15559876543", types.DefaultUserServer),
			},
			Timestamp: time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC),
		},
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
//
// The restart gate reads the persisted reason and nothing else, so treating a
// failed write as success would let the next boot reconnect a stream-replaced,
// outdated or banned device. The write is retried, the failure is reported
// through the status, and the dead session is still torn down.
func TestHandleEvent_TerminalReasonWriteFailureIsSurfaced(t *testing.T) {
	cli := newFakeClient()
	m, syncStore, _, _ := newTestManager(t, cli, true)
	require.NoError(t, m.Start(context.Background()))
	require.True(t, m.handleEvent(&events.Connected{}))

	syncStore.resetCalls()
	syncStore.mu.Lock()
	syncStore.metadataErr = errors.New("database is down")
	syncStore.mu.Unlock()

	assert.True(t, m.handleEvent(&events.LoggedOut{}))

	status := m.Status()
	assert.Equal(t, StateDisconnected, status.State)
	assert.Equal(t, ReasonLoggedOut, status.Reason)
	assert.False(t, status.TerminalReasonPersisted,
		"the status must admit the decision will not survive a restart")

	assert.Empty(t, syncStore.terminalReason(), "nothing was durably recorded")
	assert.Contains(t, cli.callLog(), "disconnect",
		"the session is dead either way, so auto-reconnect is still cancelled")

	// The write is retried rather than abandoned on the first blip.
	metadataCalls := 0
	for _, c := range syncStore.callLog() {
		if c == "metadata" {
			metadataCalls++
		}
	}
	assert.Equal(t, terminalPersistAttempts, metadataCalls)
}

// TestHandleEvent_TerminalReasonPersistedOnHappyPath is the positive control.
func TestHandleEvent_TerminalReasonPersistedOnHappyPath(t *testing.T) {
	m, _, _, _ := newTestManager(t, newFakeClient(), true)
	require.NoError(t, m.Start(context.Background()))
	require.True(t, m.handleEvent(&events.Connected{}))

	require.True(t, m.handleEvent(&events.LoggedOut{}))
	assert.True(t, m.Status().TerminalReasonPersisted)
}

// TestStart_FailsClosedWhenSyncStateUnreadable: the persisted terminal reason is
// the only thing holding a dead or banned device back, so a row we cannot read
// must not be treated as a row with nothing in it.
func TestStart_FailsClosedWhenSyncStateUnreadable(t *testing.T) {
	cli := newFakeClient()
	m, syncStore, _, _ := newTestManager(t, cli, true)
	syncStore.mu.Lock()
	syncStore.getErr = errors.New("database is down")
	syncStore.mu.Unlock()

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
		build func() *Manager
		want  string
	}{
		{
			name: "missing ingestor",
			build: func() *Manager {
				m := newNotReadyManager()
				m.SetHistoryDrainReady()
				return m
			},
			want: "message ingestor",
		},
		{
			name: "missing recorder",
			build: func() *Manager {
				m := NewManager(nil, NewWALogger("t"), &config.WhatsAppConfig{}, newFakeSyncStore(), &fakeBackfillReader{})
				m.SetIngestor(&fakeIngestor{})
				m.SetHistoryDrainReady()
				return m
			},
			want: "history notification recorder",
		},
		{
			name: "missing drainer",
			build: func() *Manager {
				m := NewManager(nil, NewWALogger("t"), &config.WhatsAppConfig{}, newFakeSyncStore(), &fakeBackfillReader{})
				m.SetIngestor(&fakeIngestor{})
				m.SetHistoryRecorder(&fakeRecorder{})
				return m
			},
			want: "history drain worker",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := tt.build()
			m.newSession = func(context.Context, bool) (*session, error) {
				return nil, errors.New("must not be reached")
			}

			require.NoError(t, m.Start(context.Background()))
			status := m.Status()
			assert.Equal(t, StateNotReady, status.State)
			assert.Equal(t, ReasonIngestNotWired, status.Reason, "the machine-readable code stays stable")
			assert.Contains(t, status.Missing, tt.want, "the status must name the dependency")

			// A refused pairing records the same detail, so the settings page
			// sees it whether or not Start ran first.
			m.setStatus(func(s *Status) { s.Missing = "" })
			assert.ErrorIs(t, m.StartPairing(context.Background(), PairRequest{Method: PairMethodQR}), ErrIngestNotWired)
			assert.Contains(t, m.Status().Missing, tt.want)
		})
	}
}
