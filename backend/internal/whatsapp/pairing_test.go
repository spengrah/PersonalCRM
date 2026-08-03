package whatsapp

import (
	"context"
	"errors"
	"testing"
	"time"

	"personal-crm/backend/internal/accelerated"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types/events"
)

// --- validation and conflict -----------------------------------------------

func TestStartPairing_RejectsUnknownMethod(t *testing.T) {
	m, _, _, _ := newTestManager(t, newFakeClient(), false)
	assert.ErrorIs(t, m.StartPairing(context.Background(), PairRequest{Method: "carrier-pigeon"}), ErrUnknownPairMethod)
}

func TestStartPairing_RejectsNonE164Phone(t *testing.T) {
	m, _, _, _ := newTestManager(t, newFakeClient(), false)
	for _, phone := range []string{"", "5551234567", "+123", "+1555123456789012", "+1-555-123-4567"} {
		assert.ErrorIs(t, m.StartPairing(context.Background(), PairRequest{Method: PairMethodPhone, Phone: phone}),
			ErrInvalidPhone, "phone %q must be rejected", phone)
	}
}

func TestStartPairing_AcceptsE164Phone(t *testing.T) {
	cli := newFakeClient()
	m, _, _, _ := newTestManager(t, cli, false)

	require.NoError(t, m.StartPairing(context.Background(), PairRequest{Method: PairMethodPhone, Phone: "+15551234567"}))

	status := m.Status()
	require.NotNil(t, status.Pairing)
	require.NotNil(t, status.Pairing.PairCode)
	assert.Equal(t, "ABCD1234", *status.Pairing.PairCode)
	assert.Equal(t, PairMethodPhone, status.Pairing.Method)
	assert.Equal(t, StatePairing, status.State)
}

func TestStartPairing_RejectsSecondConcurrentPairing(t *testing.T) {
	cli := newFakeClient()
	m, _, _, _ := newTestManager(t, cli, false)

	require.NoError(t, m.StartPairing(context.Background(), PairRequest{Method: PairMethodPhone, Phone: "+15551234567"}))
	assert.ErrorIs(t, m.StartPairing(context.Background(), PairRequest{Method: PairMethodPhone, Phone: "+15551234567"}),
		ErrPairingInProgress, "at most one pairing runs at a time")
}

func TestStartPairing_RejectsWhenConnected(t *testing.T) {
	cli := newFakeClient()
	m, _, _, _ := newTestManager(t, cli, true)
	require.NoError(t, m.Start(context.Background()))
	cli.mu.Lock()
	cli.loggedIn = true
	cli.mu.Unlock()

	assert.ErrorIs(t, m.StartPairing(context.Background(), PairRequest{Method: PairMethodQR}), ErrAlreadyConnected)
}

// TestStartPairing_UsesBrowserShapedDisplayName pins the server-validated
// "Browser (OS)" shape. A branded string is rejected with a 400 by WhatsApp.
func TestStartPairing_UsesBrowserShapedDisplayName(t *testing.T) {
	cli := newFakeClient()
	m, _, _, _ := newTestManager(t, cli, false)

	require.NoError(t, m.StartPairing(context.Background(), PairRequest{Method: PairMethodPhone, Phone: "+15551234567"}))
	assert.Contains(t, cli.callLog(), "pair_phone:"+pairClientDisplayName)
	assert.Equal(t, "Chrome (Linux)", pairClientDisplayName)
}

// --- QR flow ---------------------------------------------------------------

func TestStartPairing_QRReturnsFirstCodeWithinTimeout(t *testing.T) {
	cli := newFakeClient()
	m, _, _, _ := newTestManager(t, cli, false)
	cli.qrChan <- whatsmeow.QRChannelItem{Event: whatsmeow.QRChannelEventCode, Code: "QR-CODE-1", Timeout: time.Minute}

	require.NoError(t, m.StartPairing(context.Background(), PairRequest{Method: PairMethodQR}))

	status := m.Status()
	require.NotNil(t, status.Pairing)
	require.NotNil(t, status.Pairing.QRCode)
	assert.Equal(t, "QR-CODE-1", *status.Pairing.QRCode)
	assert.Nil(t, status.Pairing.PairCode)
}

// TestStartPairing_QRChannelOpenedBeforeConnect pins the library's required
// ordering: GetQRChannel after Connect silently never yields a code.
func TestStartPairing_QRChannelOpenedBeforeConnect(t *testing.T) {
	cli := newFakeClient()
	m, _, _, _ := newTestManager(t, cli, false)
	cli.qrChan <- whatsmeow.QRChannelItem{Event: whatsmeow.QRChannelEventCode, Code: "QR-CODE-1"}

	require.NoError(t, m.StartPairing(context.Background(), PairRequest{Method: PairMethodQR}))

	calls := cli.callLog()
	qrAt := indexOf(calls, "qr_channel")
	connectAt := indexOf(calls, "connect")
	require.GreaterOrEqual(t, qrAt, 0)
	require.GreaterOrEqual(t, connectAt, 0)
	assert.Less(t, qrAt, connectAt, "GetQRChannel must be called before Connect")
}

// TestStartPairing_QRTimesOutWithoutCode covers the bounded wait: the API
// contract promises a code in the response, so the wait has to end somewhere.
func TestStartPairing_QRTimesOutWithoutCode(t *testing.T) {
	cli := newFakeClient()
	m, _, _, _ := newTestManager(t, cli, false)
	// Nothing is pushed onto the QR channel; cancel the request context so the
	// bounded wait resolves without burning qrFirstCodeTimeout in the suite.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := m.StartPairing(ctx, PairRequest{Method: PairMethodQR})
	assert.ErrorIs(t, err, ErrQRCodeTimeout)
	assert.Nil(t, m.Status().Pairing, "a timed-out pairing leaves no state behind")
	assert.Contains(t, cli.callLog(), "delete_device",
		"the partially written device is removed, mirroring how Telegram deletes its half-written session row")
}

func TestStartPairing_ConnectFailureAbandonsPairing(t *testing.T) {
	cli := newFakeClient()
	cli.connectErr = errors.New("dial failed")
	m, _, _, _ := newTestManager(t, cli, false)

	assert.Error(t, m.StartPairing(context.Background(), PairRequest{Method: PairMethodQR}))
	assert.Nil(t, m.Status().Pairing)
	assert.Equal(t, StateNotPaired, m.Status().State)
}

// TestPairingExpiry_UsesAcceleratedClock guards the time.Now() prohibition.
func TestPairingExpiry_UsesAcceleratedClock(t *testing.T) {
	cli := newFakeClient()
	m, _, _, _ := newTestManager(t, cli, false)

	before := accelerated.GetCurrentTime()
	require.NoError(t, m.StartPairing(context.Background(), PairRequest{Method: PairMethodPhone, Phone: "+15551234567"}))
	after := accelerated.GetCurrentTime()

	status := m.Status()
	require.NotNil(t, status.Pairing)
	assert.False(t, status.Pairing.ExpiresAt.Before(before.Add(authTTL)))
	assert.False(t, status.Pairing.ExpiresAt.After(after.Add(authTTL)))
	assert.Equal(t, 5*time.Minute, authTTL)
}

// --- cancel ----------------------------------------------------------------

func TestCancelPairing_IsIdempotent(t *testing.T) {
	cli := newFakeClient()
	m, _, _, _ := newTestManager(t, cli, false)

	m.CancelPairing() // no pairing in flight
	require.NoError(t, m.StartPairing(context.Background(), PairRequest{Method: PairMethodPhone, Phone: "+15551234567"}))

	m.CancelPairing()
	assert.Nil(t, m.Status().Pairing)
	assert.Equal(t, StateNotPaired, m.Status().State)
	assert.Contains(t, cli.callLog(), "delete_device")

	m.CancelPairing() // second cancel changes nothing
	assert.Nil(t, m.Status().Pairing)
}

// --- disconnect ------------------------------------------------------------

func TestDisconnect_HappyPathDeletesDevice(t *testing.T) {
	cli := newFakeClient()
	m, syncStore, _, _ := newTestManager(t, cli, true)
	require.NoError(t, m.Start(context.Background()))
	require.True(t, m.handleEvent(&events.Connected{}))

	result, err := m.Disconnect(context.Background(), false)
	require.NoError(t, err)
	assert.True(t, result.RemoteUnlinked)
	assert.False(t, result.Forced)

	calls := cli.callLog()
	assert.Contains(t, calls, "logout")
	assert.Contains(t, calls, "delete_device")
	assert.Equal(t, StateNotPaired, m.Status().State)
	assert.Contains(t, syncStore.callLog(), "status:disabled")
}

// TestDisconnect_KeepsDeviceWhenLogoutFails is the safety property: a failed
// remote unlink must never discard the local credentials, or the device is
// orphaned on the phone with no local record to retry from.
func TestDisconnect_KeepsDeviceWhenLogoutFails(t *testing.T) {
	cli := newFakeClient()
	cli.logoutErr = errors.New("server rejected logout")
	m, _, _, _ := newTestManager(t, cli, true)
	require.NoError(t, m.Start(context.Background()))
	require.True(t, m.handleEvent(&events.Connected{}))

	result, err := m.Disconnect(context.Background(), false)
	assert.Nil(t, result)
	assert.ErrorIs(t, err, ErrRemoteUnlinkFailed)
	assert.NotContains(t, cli.callLog(), "delete_device", "the device must be KEPT so the user can retry")
	assert.Equal(t, StateDisconnectFailed, m.Status().State)
}

// TestDisconnect_LoggedOutTerminalClearsWithoutRemoteCall covers the one
// server-confirmed "already unlinked" signal.
func TestDisconnect_LoggedOutTerminalClearsWithoutRemoteCall(t *testing.T) {
	cli := newFakeClient()
	m, _, _, _ := newTestManager(t, cli, true)
	require.NoError(t, m.Start(context.Background()))
	require.True(t, m.handleEvent(&events.Connected{}))
	require.True(t, m.handleEvent(&events.LoggedOut{}))

	result, err := m.Disconnect(context.Background(), false)
	require.NoError(t, err)
	assert.True(t, result.AlreadyUnlinked)
	assert.NotContains(t, cli.callLog(), "logout", "a confirmed-unlinked device needs no remote call")
	assert.Contains(t, cli.callLog(), "delete_device")
	assert.Equal(t, StateNotPaired, m.Status().State)
}

// TestDisconnect_NonLoggedOutTerminalKeepsDeviceWhenConnectFails is the
// fail-forward regression guard: a ban, an outdated client, a replaced stream
// or a plain network error prove nothing about whether the device is still
// linked, so none of them may clear local credentials.
func TestDisconnect_NonLoggedOutTerminalKeepsDeviceWhenConnectFails(t *testing.T) {
	terminals := []struct {
		name  string
		event any
	}{
		{"temporary ban", &events.TemporaryBan{Expire: time.Hour}},
		{"client outdated", &events.ClientOutdated{}},
		{"stream replaced", &events.StreamReplaced{}},
		{"never connected", nil},
	}

	for _, tt := range terminals {
		t.Run(tt.name, func(t *testing.T) {
			cli := newFakeClient()
			m, _, _, _ := newTestManager(t, cli, true)
			require.NoError(t, m.Start(context.Background()))
			if tt.event != nil {
				require.True(t, m.handleEvent(&events.Connected{}))
				require.True(t, m.handleEvent(tt.event))
			}

			// The unlink-only connect fails.
			cli.connectErr = errors.New("network unreachable")
			cli.mu.Lock()
			cli.connected = false
			cli.mu.Unlock()

			result, err := m.Disconnect(context.Background(), false)
			assert.Nil(t, result)
			assert.ErrorIs(t, err, ErrRemoteUnlinkFailed)
			assert.NotContains(t, cli.callLog(), "delete_device",
				"a failed connect is not evidence that the device was unlinked")
			assert.Equal(t, StateDisconnectFailed, m.Status().State)
		})
	}
}

// TestDisconnect_ForceClearsLocalStateWithWarning is the only path that clears
// without server confirmation, and it says so.
func TestDisconnect_ForceClearsLocalStateWithWarning(t *testing.T) {
	cli := newFakeClient()
	cli.logoutErr = errors.New("still failing")
	m, _, _, _ := newTestManager(t, cli, true)
	require.NoError(t, m.Start(context.Background()))

	result, err := m.Disconnect(context.Background(), true)
	require.NoError(t, err)
	assert.True(t, result.Forced)
	assert.NotEmpty(t, result.Warning, "the user must be told to unlink from their phone")
	assert.Contains(t, result.Warning, "Linked Devices")
	assert.NotContains(t, cli.callLog(), "logout", "force skips the remote unlink entirely")
	assert.Contains(t, cli.callLog(), "delete_device")
	assert.Equal(t, StateNotPaired, m.Status().State)
}

func TestDisconnect_WhenNotPairedReportsNotPaired(t *testing.T) {
	cli := newFakeClient()
	m, _, _, _ := newTestManager(t, cli, false)
	require.NoError(t, m.Start(context.Background()))

	result, err := m.Disconnect(context.Background(), false)
	assert.Nil(t, result)
	assert.ErrorIs(t, err, ErrNotPaired)
}

// TestDisconnect_ReconnectsSolelyToUnlinkWhenNotConnected covers the deliberate
// asymmetry: Start declines to RESUME INGESTING on a terminal device, which is
// a different question from a user explicitly asking to unlink one.
func TestDisconnect_ReconnectsSolelyToUnlinkWhenNotConnected(t *testing.T) {
	cli := newFakeClient()
	m, _, _, _ := newTestManager(t, cli, true)
	require.NoError(t, m.Start(context.Background()))
	require.True(t, m.handleEvent(&events.Connected{}))
	require.True(t, m.handleEvent(&events.ClientOutdated{}))

	result, err := m.Disconnect(context.Background(), false)
	require.NoError(t, err)
	assert.True(t, result.RemoteUnlinked)
	assert.Contains(t, cli.callLog(), "logout")
	assert.Contains(t, cli.callLog(), "delete_device")
}

// --- history fetcher availability ------------------------------------------

// TestHistoryFetcher_NilWhenNotConnected: the drainer defers the chunk rather
// than claiming one it cannot process.
func TestHistoryFetcher_NilWhenNotConnected(t *testing.T) {
	m, _, _, _ := newTestManager(t, newFakeClient(), true)
	assert.Nil(t, m.HistoryFetcher(), "no session yet")

	require.NoError(t, m.Start(context.Background()))
	assert.Nil(t, m.HistoryFetcher(),
		"a fake session carries no *whatsmeow.Client, so there is nothing to fetch through")
}
