package whatsapp

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"personal-crm/backend/internal/accelerated"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"
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

	require.NoError(t, m.StartPairing(context.Background(), PairRequest{Method: PairMethodQR}))

	status := m.Status()
	require.NotNil(t, status.Pairing)
	require.NotNil(t, status.Pairing.QRCode)
	assert.Equal(t, defaultFakeQRCode, *status.Pairing.QRCode)
	assert.Nil(t, status.Pairing.PairCode)
}

// TestStartPairing_QRChannelOpenedBeforeConnect pins the library's required
// ordering: GetQRChannel after Connect silently never yields a code.
func TestStartPairing_QRChannelOpenedBeforeConnect(t *testing.T) {
	cli := newFakeClient()
	m, _, _, _ := newTestManager(t, cli, false)

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
	cli.qrSilent = true
	m, _, _, _ := newTestManager(t, cli, false)
	// No QR item is emitted; cancel the request context so the bounded wait
	// resolves without burning qrFirstCodeTimeout in the suite.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := m.StartPairing(ctx, PairRequest{Method: PairMethodQR})
	assert.ErrorIs(t, err, ErrQRCodeTimeout)
	assert.Nil(t, m.Status().Pairing, "a timed-out pairing leaves no state behind")
	eventually(t, "the partially written device is removed", func() bool {
		return indexOf(cli.callLog(), "delete_device") >= 0
	})
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
	eventually(t, "the abandoned attempt's device is removed", func() bool {
		return indexOf(cli.callLog(), "delete_device") >= 0
	})

	m.CancelPairing() // second cancel changes nothing
	assert.Nil(t, m.Status().Pairing)
}

// --- disconnect ------------------------------------------------------------

func TestDisconnect_HappyPathDeletesDevice(t *testing.T) {
	cli := newFakeClient()
	m, syncStore, _, _, devices := newTestManagerWithDevices(t, cli, true)
	require.NoError(t, m.Start(context.Background()))
	require.True(t, dispatchEvent(t, m, nil, &events.Connected{}))

	result, err := m.Disconnect(context.Background(), false)
	require.NoError(t, err)
	assert.True(t, result.RemoteUnlinked)
	assert.False(t, result.Forced)

	assert.Contains(t, cli.callLog(), "logout")
	assert.Empty(t, devices.remaining(), "the enumerated set is gone")
	assert.Equal(t, []types.JID{testDeviceJID}, devices.deletedJIDs())
	assert.Equal(t, StateNotPaired, m.Status().State)
	assert.Contains(t, syncStore.callLog(), "status:disabled")
}

// TestDisconnect_KeepsDeviceWhenLogoutFails is the safety property: a failed
// remote unlink must never discard the local credentials.
func TestDisconnect_KeepsDeviceWhenLogoutFails(t *testing.T) {
	cli := newFakeClient()
	cli.logoutErr = errors.New("server rejected logout")
	m, _, _, _, devices := newTestManagerWithDevices(t, cli, true)
	require.NoError(t, m.Start(context.Background()))
	require.True(t, dispatchEvent(t, m, nil, &events.Connected{}))

	result, err := m.Disconnect(context.Background(), false)
	assert.Nil(t, result)
	assert.ErrorIs(t, err, ErrRemoteUnlinkFailed)
	assert.Equal(t, []types.JID{testDeviceJID}, devices.remaining(),
		"the device must be KEPT so the user can retry")
	assert.Equal(t, 0, devices.enumerations(), "a failed unlink never reaches the purge")
	assert.Equal(t, StateDisconnectFailed, m.Status().State)
}

// TestDisconnect_LoggedOutTerminalClearsWithoutRemoteCall covers the one
// server-confirmed "already unlinked" signal.
func TestDisconnect_LoggedOutTerminalClearsWithoutRemoteCall(t *testing.T) {
	cli := newFakeClient()
	m, _, _, _, devices := newTestManagerWithDevices(t, cli, true)
	require.NoError(t, m.Start(context.Background()))
	require.True(t, dispatchEvent(t, m, nil, &events.Connected{}))
	require.True(t, dispatchEvent(t, m, nil, &events.LoggedOut{}))

	result, err := m.Disconnect(context.Background(), false)
	require.NoError(t, err)
	assert.True(t, result.AlreadyUnlinked)
	assert.NotContains(t, cli.callLog(), "logout", "a confirmed-unlinked device needs no remote call")
	assert.Empty(t, devices.remaining())
	assert.Equal(t, StateNotPaired, m.Status().State)
}

// TestDisconnect_NonLoggedOutTerminalKeepsDeviceWhenConnectFails is the
// fail-forward regression guard: a ban, an outdated client, a replaced stream
// or a plain network error prove nothing about whether the device is still
// linked.
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
			m, _, _, _, devices := newTestManagerWithDevices(t, cli, true)
			require.NoError(t, m.Start(context.Background()))
			if tt.event != nil {
				require.True(t, dispatchEvent(t, m, nil, &events.Connected{}))
				require.True(t, dispatchEvent(t, m, nil, tt.event))
			}

			// The unlink-only connect fails.
			cli.mu.Lock()
			cli.connectErr = errors.New("network unreachable")
			cli.connected = false
			cli.mu.Unlock()

			result, err := m.Disconnect(context.Background(), false)
			assert.Nil(t, result)
			assert.ErrorIs(t, err, ErrRemoteUnlinkFailed)
			assert.Equal(t, []types.JID{testDeviceJID}, devices.remaining(),
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
	m, _, _, _, devices := newTestManagerWithDevices(t, cli, true)
	require.NoError(t, m.Start(context.Background()))

	result, err := m.Disconnect(context.Background(), true)
	require.NoError(t, err)
	assert.True(t, result.Forced)
	assert.NotEmpty(t, result.Warning, "the user must be told to unlink from their phone")
	assert.Contains(t, result.Warning, "Linked Devices")
	assert.NotContains(t, cli.callLog(), "logout", "force skips the remote unlink entirely")
	assert.Empty(t, devices.remaining())
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
	m, _, _, _, devices := newTestManagerWithDevices(t, cli, true)
	require.NoError(t, m.Start(context.Background()))
	require.True(t, dispatchEvent(t, m, nil, &events.Connected{}))
	require.True(t, dispatchEvent(t, m, nil, &events.ClientOutdated{}))
	cli.setConnected(false)

	result, err := m.Disconnect(context.Background(), false)
	require.NoError(t, err)
	assert.True(t, result.RemoteUnlinked)
	assert.Contains(t, cli.callLog(), "logout")
	assert.Empty(t, devices.remaining())
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

// TestStartPairing_ExpiredPairingIsTakenOverNotConflicted: an expired attempt
// must not wedge the single pairing slot.
func TestStartPairing_ExpiredPairingIsTakenOverNotConflicted(t *testing.T) {
	cli := newFakeClient()
	m, _, _, _ := newTestManager(t, cli, false)

	require.NoError(t, m.StartPairing(context.Background(), PairRequest{Method: PairMethodPhone, Phone: "+15551234567"}))
	expirePairing(m)

	require.NoError(t, m.StartPairing(context.Background(), PairRequest{Method: PairMethodPhone, Phone: "+15551234567"}),
		"an expired attempt is taken over, not treated as a conflict")

	status := m.Status()
	require.NotNil(t, status.Pairing)
	assert.True(t, status.Pairing.ExpiresAt.After(accelerated.GetCurrentTime()),
		"the surviving attempt is the fresh one, with a live TTL")
	eventually(t, "the taken-over attempt's half-written device is removed", func() bool {
		return indexOf(cli.callLog(), "delete_device") >= 0
	})
}

// TestRunQRChannel_ClosedChannelReleasesThePairingSlot: whatsmeow closes the QR
// channel when the codes run out. Leaving the slot claimed there would block
// every later pairing attempt.
func TestRunQRChannel_ClosedChannelReleasesThePairingSlot(t *testing.T) {
	cli := newFakeClient()
	m, _, _, _ := newTestManager(t, cli, false)

	require.NoError(t, m.StartPairing(context.Background(), PairRequest{Method: PairMethodQR}))
	require.NotNil(t, m.Status().Pairing)

	close(cli.qrChan)

	eventually(t, "a closed QR channel must release the pairing slot", func() bool {
		return m.Status().Pairing == nil
	})
	assert.Equal(t, StateNotPaired, m.Status().State)
}

// TestRunQRChannel_RefreshesTheStoredCode: the library emits a fresh code every
// Timeout until they run out, so the stored code must keep up.
func TestRunQRChannel_RefreshesTheStoredCode(t *testing.T) {
	cli := newFakeClient()
	m, _, _, _ := newTestManager(t, cli, false)

	require.NoError(t, m.StartPairing(context.Background(), PairRequest{Method: PairMethodQR}))
	require.NotNil(t, m.Status().Pairing)
	require.Equal(t, defaultFakeQRCode, *m.Status().Pairing.QRCode)

	cli.qrChan <- whatsmeow.QRChannelItem{Event: whatsmeow.QRChannelEventCode, Code: "QR-CODE-2"}

	eventually(t, "the stored code must follow the channel", func() bool {
		p := m.Status().Pairing
		return p != nil && p.QRCode != nil && *p.QRCode == "QR-CODE-2"
	})
}

// TestStartPairing_PhoneWaitsForConnectionBeforeRequestingCode pins the ordering
// the library documents: PairPhone must not be called until the pairing socket
// has produced its first QR item, or it races the handshake.
func TestStartPairing_PhoneWaitsForConnectionBeforeRequestingCode(t *testing.T) {
	cli := newFakeClient()
	m, _, _, _ := newTestManager(t, cli, false)

	require.NoError(t, m.StartPairing(context.Background(), PairRequest{Method: PairMethodPhone, Phone: "+15551234567"}))

	calls := cli.callLog()
	qrAt := indexOf(calls, "qr_channel")
	connectAt := indexOf(calls, "connect")
	pairAt := indexOf(calls, "pair_phone:"+pairClientDisplayName)
	require.GreaterOrEqual(t, qrAt, 0)
	require.GreaterOrEqual(t, connectAt, 0)
	require.GreaterOrEqual(t, pairAt, 0)
	assert.Less(t, qrAt, connectAt, "the QR channel is opened before Connect on both methods")
	assert.Less(t, connectAt, pairAt, "PairPhone comes after Connect")
}

// TestStartPairing_PhoneTimesOutWhenConnectionNeverEstablishes: with no QR item
// the connection is not established, so PairPhone must not be called at all.
func TestStartPairing_PhoneTimesOutWhenConnectionNeverEstablishes(t *testing.T) {
	cli := newFakeClient()
	cli.qrSilent = true
	m, _, _, _ := newTestManager(t, cli, false)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := m.StartPairing(ctx, PairRequest{Method: PairMethodPhone, Phone: "+15551234567"})
	assert.ErrorIs(t, err, ErrQRCodeTimeout)
	assert.NotContains(t, cli.callLog(), "pair_phone:"+pairClientDisplayName,
		"a pairing code must never be requested on a connection that is not established")
}

// TestStartPairing_PhoneDoesNotReportAQRCode: a phone attempt receives QR codes
// from the library too, but they are not the user's affordance.
func TestStartPairing_PhoneDoesNotReportAQRCode(t *testing.T) {
	cli := newFakeClient()
	m, _, _, _ := newTestManager(t, cli, false)

	require.NoError(t, m.StartPairing(context.Background(), PairRequest{Method: PairMethodPhone, Phone: "+15551234567"}))

	status := m.Status()
	require.NotNil(t, status.Pairing)
	assert.Nil(t, status.Pairing.QRCode)
	require.NotNil(t, status.Pairing.PairCode)
}

// TestStartPairing_CancelDuringSessionBuildDiscardsTheClient is the
// deterministic proof for the cancel-window race.
//
// The attempt is in the slot BEFORE its client exists, so a cancel landing in
// that window used to let the original goroutine attach and connect an orphaned
// client — one Stop() could not reach and that could still complete a pairing
// nothing recorded. Under the actor the built session reaches a continuation
// whose fence fails, and the loop itself discards it.
func TestStartPairing_CancelDuringSessionBuildDiscardsTheClient(t *testing.T) {
	cli := newFakeClient()
	m, _, _, _ := newTestManager(t, cli, false)

	building := make(chan struct{})
	release := make(chan struct{})
	m.setSessionFactory(func(context.Context, sessionRequest) (*session, error) {
		close(building)
		<-release
		return fakeSessionFor(cli, false, nil), nil
	})

	errCh := make(chan error, 1)
	go func() { errCh <- m.StartPairing(context.Background(), PairRequest{Method: PairMethodQR}) }()

	<-building
	m.CancelPairing()
	close(release)

	assert.ErrorIs(t, <-errCh, ErrPairingCancelled)
	assert.NotContains(t, cli.callLog(), "connect",
		"a cancelled attempt must never connect the client it was still building")
	eventually(t, "the discarded device is removed rather than orphaned", func() bool {
		return indexOf(cli.callLog(), "delete_device") >= 0
	})
	assert.Nil(t, m.Status().Pairing)
	assert.Equal(t, StateNotPaired, m.Status().State)
}

// TestStartPairing_CancelDuringSessionBuildCancelsTheQRContext: the attempt's
// context exists from the instant the slot is claimed, so a cancel in the
// earliest window still unwinds everything downstream.
func TestStartPairing_CancelDuringSessionBuildCancelsTheQRContext(t *testing.T) {
	cli := newFakeClient()
	m, _, _, _ := newTestManager(t, cli, false)

	building := make(chan struct{})
	release := make(chan struct{})
	m.setSessionFactory(func(context.Context, sessionRequest) (*session, error) {
		close(building)
		<-release
		return fakeSessionFor(cli, false, nil), nil
	})

	go func() { _ = m.StartPairing(context.Background(), PairRequest{Method: PairMethodQR}) }()
	<-building

	attempt := m.inspect().Pairing
	require.NotNil(t, attempt)
	require.NotNil(t, attempt.ctx, "the attempt's context exists before any effect does")

	m.CancelPairing()
	select {
	case <-attempt.ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("the pairing context was not cancelled")
	}
	assert.False(t, m.inspect().PairingDrainOn, "no drain was ever launched, so nothing waits on drainDone")
	close(release)
}

// TestCancelPairing_DuringTheQRChannelToConnectBatch is the window between
// GetQRChannel and Connect.
//
// GetQRChannel registers a handler on the client that only its context's
// cancellation removes, so a cancel here must cancel p.ctx IMMEDIATELY rather
// than after the batch completes — otherwise the library's handler outlives the
// client we are about to discard.
func TestCancelPairing_DuringTheQRChannelToConnectBatch(t *testing.T) {
	cli := newFakeClient()
	entered := make(chan struct{})
	release := make(chan struct{})
	cli.connectEntered = entered
	cli.connectBlock = release

	m, _, _, _ := newTestManager(t, cli, false)

	errCh := make(chan error, 1)
	go func() { errCh <- m.StartPairing(context.Background(), PairRequest{Method: PairMethodQR}) }()

	<-entered
	attempt := m.inspect().Pairing
	require.NotNil(t, attempt)

	m.CancelPairing()
	select {
	case <-attempt.ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("the pairing context must be cancelled immediately, not after the batch completes")
	}

	close(release)
	assert.ErrorIs(t, <-errCh, ErrPairingCancelled)
	assert.Nil(t, m.Status().Pairing)
	eventually(t, "the half-built client is discarded", func() bool {
		return indexOf(cli.callLog(), "delete_device") >= 0
	})
}

// TestCancelPairing_StopsADrainOnAChannelThatNeverCloses: a QR channel that
// emits one code and then never closes must not pin the drain goroutine.
func TestCancelPairing_StopsADrainOnAChannelThatNeverCloses(t *testing.T) {
	cli := newFakeClient()
	m, _, _, _ := newTestManager(t, cli, false)

	require.NoError(t, m.StartPairing(context.Background(), PairRequest{Method: PairMethodQR}))
	view := m.inspect()
	require.NotNil(t, view.Pairing)
	require.True(t, view.PairingDrainOn)
	attempt := view.Pairing

	m.CancelPairing()

	select {
	case <-attempt.drainDone:
	case <-time.After(2 * time.Second):
		t.Fatal("the drain goroutine never exited")
	}
	select {
	case <-attempt.ctx.Done():
	default:
		t.Fatal("the pairing context was not cancelled")
	}
}

// TestStop_StopsADrainOnAChannelThatNeverCloses is the same, through shutdown.
func TestStop_StopsADrainOnAChannelThatNeverCloses(t *testing.T) {
	cli := newFakeClient()
	m, _, _, _ := newTestManager(t, cli, false)

	require.NoError(t, m.StartPairing(context.Background(), PairRequest{Method: PairMethodQR}))
	view := m.inspect()
	require.NotNil(t, view.Pairing)
	require.True(t, view.PairingDrainOn)
	attempt := view.Pairing

	m.Stop()

	select {
	case <-attempt.drainDone:
	case <-time.After(2 * time.Second):
		t.Fatal("the drain goroutine never exited")
	}
	select {
	case <-attempt.ctx.Done():
	default:
		t.Fatal("the pairing context was not cancelled")
	}
}

// TestStop_DuringSessionBuildDiscardsTheClient: a process shutdown must not
// leave a client connecting behind it.
func TestStop_DuringSessionBuildDiscardsTheClient(t *testing.T) {
	cli := newFakeClient()
	m, _, _, _ := newTestManager(t, cli, false)

	building := make(chan struct{})
	release := make(chan struct{})
	m.setSessionFactory(func(context.Context, sessionRequest) (*session, error) {
		close(building)
		<-release
		return fakeSessionFor(cli, false, nil), nil
	})

	errCh := make(chan error, 1)
	go func() { errCh <- m.StartPairing(context.Background(), PairRequest{Method: PairMethodQR}) }()
	<-building

	stopped := make(chan struct{})
	go func() { m.Stop(); close(stopped) }()

	eventually(t, "Stop closes stopping first, before anything else", func() bool {
		select {
		case <-m.stopping:
			return true
		default:
			return false
		}
	})
	close(release)
	<-stopped

	assert.ErrorIs(t, <-errCh, ErrManagerStopped)
	assert.NotContains(t, cli.callLog(), "connect",
		"Stop must win over a build that has not produced a client yet")
}

// TestDisconnect_StopsTheOldClientBeforeBuildingTheUnlinkClient is the
// deterministic proof for the double-client defect.
//
// whatsmeow starts auto-reconnect on a remote disconnect, and only Disconnect()
// marks the drop expected. Building a second client for the unlink without
// stopping the first puts two clients on one device and races the unlink
// against a reconnect. Both clients append to ONE ordered log, and the unlink
// client's CONSTRUCTION is recorded on it too.
func TestDisconnect_StopsTheOldClientBeforeBuildingTheUnlinkClient(t *testing.T) {
	shared := &sharedLog{}
	old := newFakeClient()
	old.name, old.shared = "old", shared
	unlink := newFakeClient()
	unlink.name, unlink.shared = "unlink", shared

	m, _, _, _ := newTestManager(t, old, true)
	require.NoError(t, m.Start(context.Background()))
	require.True(t, dispatchEvent(t, m, nil, &events.Connected{}))

	// The socket drops; whatsmeow is now auto-reconnecting behind the scenes.
	require.True(t, dispatchEvent(t, m, nil, &events.Disconnected{}))
	old.setConnected(false)
	require.Equal(t, StateReconnecting, m.Status().State)

	m.setSessionFactory(func(context.Context, sessionRequest) (*session, error) {
		shared.record("unlink:built")
		jid := testDeviceJID
		return fakeSessionFor(unlink, true, &jid), nil
	})

	result, err := m.Disconnect(context.Background(), false)
	require.NoError(t, err)
	assert.True(t, result.RemoteUnlinked)

	calls := shared.entries()
	oldDisconnect := indexOf(calls, "old:disconnect")
	unlinkBuilt := indexOf(calls, "unlink:built")
	unlinkConnect := indexOf(calls, "unlink:connect")
	unlinkLogout := indexOf(calls, "unlink:logout")

	require.GreaterOrEqual(t, oldDisconnect, 0, "the auto-reconnecting client must be stopped")
	require.GreaterOrEqual(t, unlinkBuilt, 0)
	require.GreaterOrEqual(t, unlinkConnect, 0)
	require.GreaterOrEqual(t, unlinkLogout, 0)

	assert.Less(t, oldDisconnect, unlinkBuilt,
		"the old client must be stopped BEFORE the unlink client is even built")
	assert.Less(t, unlinkBuilt, unlinkConnect)
	assert.Less(t, unlinkConnect, unlinkLogout)
}

// TestOnPairSuccess_FromCancelledPairingIsIgnored is the attached-session race:
// the scan completes after the user pressed cancel.
func TestOnPairSuccess_FromCancelledPairingIsIgnored(t *testing.T) {
	cli := newFakeClient()
	m, _, _, _ := newTestManager(t, cli, false)

	require.NoError(t, m.StartPairing(context.Background(), PairRequest{Method: PairMethodQR}))
	pairingSess := pairingSession(t, m)
	require.NotNil(t, pairingSess)

	m.CancelPairing()
	require.Nil(t, m.Status().Pairing)
	eventually(t, "the cancelled attempt's device is deleted", func() bool {
		return indexOf(cli.callLog(), "delete_device") >= 0
	})

	// The scan lands anyway.
	jid := types.NewJID("15551234567", types.DefaultUserServer)
	assert.True(t, dispatchEvent(t, m, pairingSess, &events.PairSuccess{ID: jid}))

	status := m.Status()
	assert.Equal(t, StateNotPaired, status.State,
		"a cancelled pairing must never publish connected")
	assert.Nil(t, status.JID)
	assert.Nil(t, m.HistoryFetcher(), "no session may be adopted")

	eventually(t, "the abandoned client is torn down again rather than left holding a socket", func() bool {
		return countCalls(cli.callLog(), "disconnect") >= 2
	})
}

// TestOnPairSuccess_FromSupersededPairingIsIgnored: an earlier attempt whose TTL
// expired was taken over by a newer one.
func TestOnPairSuccess_FromSupersededPairingIsIgnored(t *testing.T) {
	cli := newFakeClient()
	m, _, _, _ := newTestManager(t, cli, false)

	require.NoError(t, m.StartPairing(context.Background(), PairRequest{Method: PairMethodQR}))
	stale := pairingSession(t, m)
	require.NotNil(t, stale)

	expirePairing(m)
	require.NoError(t, m.StartPairing(context.Background(), PairRequest{Method: PairMethodQR}))
	current := pairingSession(t, m)
	require.NotNil(t, current)
	require.NotSame(t, stale, current)

	jid := types.NewJID("15551234567", types.DefaultUserServer)
	assert.True(t, dispatchEvent(t, m, stale, &events.PairSuccess{ID: jid}))

	assert.Equal(t, StatePairing, m.Status().State,
		"the superseded attempt must not complete on behalf of the live one")
	assert.NotNil(t, m.Status().Pairing)
}

// TestOnConnected_FromAbandonedClientIsIgnored: the same identity rule on the
// connection event.
func TestOnConnected_FromAbandonedClientIsIgnored(t *testing.T) {
	cli := newFakeClient()
	m, _, _, _ := newTestManager(t, cli, false)

	require.NoError(t, m.StartPairing(context.Background(), PairRequest{Method: PairMethodQR}))
	orphan := pairingSession(t, m)
	m.CancelPairing()

	assert.True(t, dispatchEvent(t, m, orphan, &events.Connected{}))
	assert.Equal(t, StateNotPaired, m.Status().State)
}

// TestClearLocalDevice_ResetsTerminalReasonPersisted: the field is meaningful
// only alongside a terminal state.
func TestClearLocalDevice_ResetsTerminalReasonPersisted(t *testing.T) {
	cli := newFakeClient()
	m, _, _, _ := newTestManager(t, cli, true)
	require.NoError(t, m.Start(context.Background()))
	require.True(t, dispatchEvent(t, m, nil, &events.Connected{}))
	require.True(t, dispatchEvent(t, m, nil, &events.LoggedOut{}))
	require.NotNil(t, m.Status().TerminalReasonPersisted)

	_, err := m.Disconnect(context.Background(), false)
	require.NoError(t, err)

	status := m.Status()
	require.Equal(t, StateNotPaired, status.State)
	assert.Nil(t, status.TerminalReasonPersisted,
		"a status with no terminal decision must not report one either way")
}

// --- unlink outcomes, per half ----------------------------------------------

// logoutRemoteFailure and logoutLocalDeleteFailure are the two error shapes the
// library actually produces. Logout sends the unlink, then disconnects, then
// deletes the local store, wrapping each failure with its own prefix — so the
// SECOND shape means the device is already gone server-side.
func logoutRemoteFailure() error {
	return fmt.Errorf("error sending logout request: %w", errors.New("websocket disconnected before response"))
}

func logoutLocalDeleteFailure() error {
	return fmt.Errorf("error deleting data from store: %w", errors.New("connection refused"))
}

// TestDisconnect_RemoteFailureKeepsTheDevice pins the conservative half against
// the library's real error text, not an invented one.
func TestDisconnect_RemoteFailureKeepsTheDevice(t *testing.T) {
	cli := newFakeClient()
	cli.logoutErr = logoutRemoteFailure()
	m, _, _, _, devices := newTestManagerWithDevices(t, cli, true)
	require.NoError(t, m.Start(context.Background()))
	require.True(t, dispatchEvent(t, m, nil, &events.Connected{}))

	result, err := m.Disconnect(context.Background(), false)
	assert.Nil(t, result)
	assert.ErrorIs(t, err, ErrRemoteUnlinkFailed)
	assert.Equal(t, []types.JID{testDeviceJID}, devices.remaining(),
		"the device is KEPT so the user can retry the unlink")
	assert.Equal(t, StateDisconnectFailed, m.Status().State)
}

// TestDisconnect_LocalDeleteFailureAfterRemoteSuccessIsNotAFailedUnlink is the
// half the old code got backwards: the library returns this error AFTER the
// remote unlink has already succeeded.
func TestDisconnect_LocalDeleteFailureAfterRemoteSuccessIsNotAFailedUnlink(t *testing.T) {
	cli := newFakeClient()
	cli.logoutErr = logoutLocalDeleteFailure()
	m, _, _, _, devices := newTestManagerWithDevices(t, cli, true)
	require.NoError(t, m.Start(context.Background()))
	require.True(t, dispatchEvent(t, m, nil, &events.Connected{}))

	result, err := m.Disconnect(context.Background(), false)
	require.NoError(t, err, "the remote device is gone; this is not a failed unlink")
	require.NotNil(t, result)
	assert.True(t, result.RemoteUnlinked)
	assert.Empty(t, devices.remaining(),
		"the local clear is completed rather than reported as an unlink failure")
	assert.Equal(t, StateNotPaired, m.Status().State)
}

// TestDisconnect_LocalCleanupFailureIsSurfaced: the device row could not be
// deleted, so the credentials are still there.
func TestDisconnect_LocalCleanupFailureIsSurfaced(t *testing.T) {
	cli := newFakeClient()
	cli.logoutErr = logoutLocalDeleteFailure()
	m, _, _, _, devices := newTestManagerWithDevices(t, cli, true)
	devices.setErr(func(d *fakeDevices) { d.delErr = errors.New("database is down") })
	require.NoError(t, m.Start(context.Background()))
	require.True(t, dispatchEvent(t, m, nil, &events.Connected{}))

	result, err := m.Disconnect(context.Background(), false)
	assert.Nil(t, result)
	assert.ErrorIs(t, err, ErrLocalCleanupFailed)
	assert.NotErrorIs(t, err, ErrRemoteUnlinkFailed, "the unlink itself succeeded")

	status := m.Status()
	assert.Equal(t, StateDisconnectFailed, status.State)
	assert.Equal(t, ReasonLocalCleanupFailed, status.Reason)

	// A retry makes no further remote call: the device is already unlinked, and
	// an unlink against an unlinked device cannot succeed.
	devices.setErr(func(d *fakeDevices) { d.delErr = nil })
	cli.mu.Lock()
	cli.calls = nil
	cli.mu.Unlock()

	result, err = m.Disconnect(context.Background(), false)
	require.NoError(t, err)
	assert.True(t, result.AlreadyUnlinked)
	assert.NotContains(t, cli.callLog(), "logout", "the remote half is settled; only the local clear was outstanding")
	assert.Empty(t, devices.remaining())
	assert.Equal(t, StateNotPaired, m.Status().State)
}

// TestDisconnect_ForceReportsAFailedLocalClear: force skips the remote call, but
// it cannot skip reality.
func TestDisconnect_ForceReportsAFailedLocalClear(t *testing.T) {
	cli := newFakeClient()
	m, _, _, _, devices := newTestManagerWithDevices(t, cli, true)
	devices.setErr(func(d *fakeDevices) { d.delErr = errors.New("database is down") })
	require.NoError(t, m.Start(context.Background()))

	result, err := m.Disconnect(context.Background(), true)
	assert.Nil(t, result)
	assert.ErrorIs(t, err, ErrLocalCleanupFailed)
	assert.Equal(t, StateDisconnectFailed, m.Status().State)
	assert.Equal(t, ReasonForcedCleanupFailed, m.Status().Reason,
		"force made no remote call, so it learned nothing about the remote device")

	// A later ordinary unlink must still try the remote half, rather than
	// reading the failed force as proof the device was already unlinked.
	devices.setErr(func(d *fakeDevices) { d.delErr = nil })
	cli.mu.Lock()
	cli.calls = nil
	cli.connected = true
	cli.mu.Unlock()
	// The unlink now has no installed session, so it builds one to log out with.
	m.setSessionFactory(func(context.Context, sessionRequest) (*session, error) {
		jid := testDeviceJID
		return fakeSessionFor(cli, true, &jid), nil
	})

	result, err = m.Disconnect(context.Background(), false)
	require.NoError(t, err)
	assert.True(t, result.RemoteUnlinked)
	assert.Contains(t, cli.callLog(), "logout",
		"a failed forced clear is not evidence of anything; the unlink still has to happen")
}

// TestDisconnect_AlreadyUnlinkedReportsAFailedLocalClear: the same for the
// server-confirmed path, which also clears without a remote call.
func TestDisconnect_AlreadyUnlinkedReportsAFailedLocalClear(t *testing.T) {
	cli := newFakeClient()
	m, _, _, _, devices := newTestManagerWithDevices(t, cli, true)
	require.NoError(t, m.Start(context.Background()))
	require.True(t, dispatchEvent(t, m, nil, &events.Connected{}))
	require.True(t, dispatchEvent(t, m, nil, &events.LoggedOut{}))

	devices.setErr(func(d *fakeDevices) { d.delErr = errors.New("database is down") })

	result, err := m.Disconnect(context.Background(), false)
	assert.Nil(t, result)
	assert.ErrorIs(t, err, ErrLocalCleanupFailed)
	assert.NotContains(t, cli.callLog(), "logout")
	assert.Equal(t, ReasonLocalCleanupFailed, m.Status().Reason)
}

// TestDisconnect_AfterRestartIntoLoggedOutSurfacesAnUnreachableStore is the
// no-session case: after a restart into a persisted logged_out state, Start
// deliberately installs no session, so the clear reaches the purge with nothing
// but the container. A store it cannot ENUMERATE is a cleanup that did not
// happen.
func TestDisconnect_AfterRestartIntoLoggedOutSurfacesAnUnreachableStore(t *testing.T) {
	for _, force := range []bool{false, true} {
		name := "server confirmed"
		if force {
			name = "forced"
		}
		t.Run(name, func(t *testing.T) {
			cli := newFakeClient()
			m, syncStore, _, _, devices := newTestManagerWithDevices(t, cli, true)
			syncStore.seedTerminal(ReasonLoggedOut, nil)

			require.NoError(t, m.Start(context.Background()))
			require.Equal(t, StateDisconnected, m.Status().State)
			require.Nil(t, installedSession(t, m), "the terminal gate deliberately installs no session")

			devices.setErr(func(d *fakeDevices) { d.listErr = errors.New("device store unreachable") })

			result, err := m.Disconnect(context.Background(), force)
			assert.Nil(t, result)
			assert.ErrorIs(t, err, ErrLocalCleanupFailed,
				"a store we cannot read is a cleanup that did not happen")
			assert.Equal(t, StateDisconnectFailed, m.Status().State,
				"the credentials are still stored, so the status must not say not_paired")
			assert.Equal(t, []types.JID{testDeviceJID}, devices.remaining())
		})
	}
}

// TestDisconnect_WithNothingStoredClearsCleanly is the negative control: an
// EMPTY store is not a failed cleanup.
func TestDisconnect_WithNothingStoredClearsCleanly(t *testing.T) {
	cli := newFakeClient()
	m, _, _, _ := newTestManager(t, cli, false)
	require.NoError(t, m.Start(context.Background()))
	require.Equal(t, StateNotPaired, m.Status().State)

	result, err := m.Disconnect(context.Background(), true)
	require.NoError(t, err)
	assert.True(t, result.Forced)
	assert.Equal(t, StateNotPaired, m.Status().State)
}

// --- the three unlink happy paths reach their publication turn ---------------

// TestDisconnect_LiveClientLogoutPublishesNotPaired,
// TestDisconnect_ForceClearPublishesNotPaired and
// TestDisconnect_ServerConfirmedLoggedOutPublishesNotPaired are the direct
// falsification of the retirement/fence contradiction: a fenceOK that rejected a
// retired CAPTURED session would abort all three before they published.
func TestDisconnect_LiveClientLogoutPublishesNotPaired(t *testing.T) {
	cli := newFakeClient()
	m, _, _, _ := newTestManager(t, cli, true)
	require.NoError(t, m.Start(context.Background()))
	require.True(t, dispatchEvent(t, m, nil, &events.Connected{}))

	result, err := m.Disconnect(context.Background(), false)
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.NotErrorIs(t, err, ErrOperationSuperseded)
	assert.Equal(t, StateNotPaired, m.Status().State)
}

func TestDisconnect_ForceClearPublishesNotPaired(t *testing.T) {
	cli := newFakeClient()
	m, _, _, _ := newTestManager(t, cli, true)
	require.NoError(t, m.Start(context.Background()))

	result, err := m.Disconnect(context.Background(), true)
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.True(t, result.Forced)
	assert.Equal(t, StateNotPaired, m.Status().State)
}

func TestDisconnect_ServerConfirmedLoggedOutPublishesNotPaired(t *testing.T) {
	cli := newFakeClient()
	m, _, _, _ := newTestManager(t, cli, true)
	require.NoError(t, m.Start(context.Background()))
	require.True(t, dispatchEvent(t, m, nil, &events.Connected{}))
	require.True(t, dispatchEvent(t, m, nil, &events.LoggedOut{}))

	result, err := m.Disconnect(context.Background(), false)
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.True(t, result.AlreadyUnlinked)
	assert.Equal(t, StateNotPaired, m.Status().State)
}

// --- link and unlink are mutually exclusive ---------------------------------

// TestDisconnect_RefusesWhileAPairingExists removes the interleaving rather than
// fencing it. The fence guards ACTOR state, and a pairing's device row is
// DATABASE state the library writes from its own goroutine — no amount of
// fencing can see that write, so the two operations simply never overlap.
func TestDisconnect_RefusesWhileAPairingExists(t *testing.T) {
	// spec: WHA-015.link-and-unlink-are-mutually-exclusive
	for _, force := range []bool{false, true} {
		name := "unlink"
		if force {
			name = "forced unlink"
		}
		t.Run(name, func(t *testing.T) {
			cli := newFakeClient()
			m, _, _, _, devices := newTestManagerWithDevices(t, cli, true)
			require.NoError(t, m.Start(context.Background()))
			cli.setConnected(false)
			require.NoError(t, m.StartPairing(context.Background(), PairRequest{Method: PairMethodQR}))

			result, err := m.Disconnect(context.Background(), force)
			assert.Nil(t, result)
			assert.ErrorIs(t, err, ErrPairingInProgress)
			assert.Equal(t, 0, devices.enumerations(), "nothing is enumerated")
			assert.Equal(t, []types.JID{testDeviceJID}, devices.remaining(), "and nothing is deleted")
		})
	}
}

// TestStartPairing_RefusesWhileAnUnlinkIsInFlight is the other half.
func TestStartPairing_RefusesWhileAnUnlinkIsInFlight(t *testing.T) {
	// spec: WHA-015.link-and-unlink-are-mutually-exclusive
	cli := newFakeClient()
	entered := make(chan struct{})
	release := make(chan struct{})
	cli.logoutEntered = entered
	cli.logoutBlock = release

	m, _, _, _ := newTestManager(t, cli, true)
	require.NoError(t, m.Start(context.Background()))
	require.True(t, dispatchEvent(t, m, nil, &events.Connected{}))

	done := make(chan struct{})
	go func() { defer close(done); _, _ = m.Disconnect(context.Background(), false) }()
	<-entered

	fresh := newFakeClient()
	useClient(m, fresh, false)
	err := m.StartPairing(context.Background(), PairRequest{Method: PairMethodQR})
	assert.ErrorIs(t, err, ErrUnlinkInProgress)
	assert.Empty(t, fresh.callLog(), "no fresh device is even built")

	close(release)
	<-done
}

// TestDisconnect_RefusesASecondUnlink: two overlapping unlinks were always
// incoherent — two remote logouts, two publications, one device.
func TestDisconnect_RefusesASecondUnlink(t *testing.T) {
	// spec: WHA-015.disconnect-while-unlink-in-flight-conflicts
	cli := newFakeClient()
	entered := make(chan struct{})
	release := make(chan struct{})
	cli.logoutEntered = entered
	cli.logoutBlock = release

	m, _, _, _ := newTestManager(t, cli, true)
	require.NoError(t, m.Start(context.Background()))
	require.True(t, dispatchEvent(t, m, nil, &events.Connected{}))

	done := make(chan struct{})
	go func() { defer close(done); _, _ = m.Disconnect(context.Background(), false) }()
	<-entered

	_, err := m.Disconnect(context.Background(), false)
	assert.ErrorIs(t, err, ErrUnlinkInProgress)

	close(release)
	<-done
}

// TestDisconnect_LateSaveFromAnAbandonedPairingIsSuperseded is the one path
// mutual exclusion does not cover: a row that appears AFTER the enumeration.
// Stage d must report a supersession rather than publishing not_paired, and it
// must not delete the row it never observed.
func TestDisconnect_LateSaveFromAnAbandonedPairingIsSuperseded(t *testing.T) {
	cli := newFakeClient()
	m, _, _, _, devices := newTestManagerWithDevices(t, cli, true)
	require.NoError(t, m.Start(context.Background()))

	lateJID := types.NewJID("15559990000", types.DefaultUserServer)
	delEntered := make(chan struct{})
	delBlock := make(chan struct{})
	devices.setErr(func(d *fakeDevices) {
		d.delEntered = delEntered
		d.delBlock = delBlock
	})

	errCh := make(chan error, 1)
	go func() {
		_, err := m.Disconnect(context.Background(), true)
		errCh <- err
	}()

	<-delEntered
	// The abandoned attempt's library-side save lands after the enumeration.
	devices.add(lateJID)
	close(delBlock)

	assert.ErrorIs(t, <-errCh, ErrOperationSuperseded)
	assert.Equal(t, []types.JID{lateJID}, devices.remaining(),
		"the row that appeared after enumeration is left alone")
	assert.NotEqual(t, StateNotPaired, m.Status().State,
		"a store with credentials in it must never be published as clean")
}

// TestClearLocalDevice_PurgesTheEnumeratedSet is the resurrection scenario: a
// retained device A alongside the linked device B. The clear removes exactly
// {A, B}, so a subsequent boot cannot resume A.
func TestClearLocalDevice_PurgesTheEnumeratedSet(t *testing.T) {
	cli := newFakeClient()
	m, _, _, _, devices := newTestManagerWithDevices(t, cli, true)
	retained := types.NewJID("15551110000", types.DefaultUserServer)
	devices.add(retained)

	require.NoError(t, m.Start(context.Background()))

	result, err := m.Disconnect(context.Background(), true)
	require.NoError(t, err)
	assert.True(t, result.Forced)
	assert.Empty(t, devices.remaining(), "the enumerated set is gone, not just the one in use")
	assert.ElementsMatch(t, []types.JID{testDeviceJID, retained}, devices.deletedJIDs())
	assert.Equal(t, StateNotPaired, m.Status().State)
}

// TestClearLocalDevice_PublicationIsOneTurn asserts the SEQUENCING that the
// pre-actor code broke: the metadata clear and the status='disabled' write both
// happen inside the single publication turn, with nothing interleaved.
func TestClearLocalDevice_PublicationIsOneTurn(t *testing.T) {
	cli := newFakeClient()
	m, syncStore, _, _ := newTestManager(t, cli, true)
	require.NoError(t, m.Start(context.Background()))
	require.True(t, dispatchEvent(t, m, nil, &events.Connected{}))
	syncStore.resetCalls()

	_, err := m.Disconnect(context.Background(), false)
	require.NoError(t, err)

	calls := syncStore.callLog()
	metaAt := indexOf(calls, "metadata")
	disabledAt := indexOf(calls, "status:disabled")
	require.GreaterOrEqual(t, metaAt, 0, "the terminal metadata is cleared")
	require.GreaterOrEqual(t, disabledAt, 0, "the row is marked disabled")
	assert.Equal(t, metaAt+1, disabledAt,
		"the two writes are adjacent because the turn does not end between them")
}

// TestClearLocalDevice_RefusesWhileAPairingExists is the "a clear must never
// erase an adopted linked device" half, proved the way the design achieves it:
// the clear never starts.
func TestClearLocalDevice_RefusesWhileAPairingExists(t *testing.T) {
	cli := newFakeClient()
	m, syncStore, _, _ := newTestManager(t, cli, true)
	require.NoError(t, m.Start(context.Background()))
	cli.setConnected(false)
	require.NoError(t, m.StartPairing(context.Background(), PairRequest{Method: PairMethodQR}))
	require.True(t, dispatchEvent(t, m, pairingSession(t, m), &events.PairSuccess{
		ID: types.NewJID("15552223333", types.DefaultUserServer),
	}))
	// Re-enter a pairing so the exclusion rule applies at the moment of the call.
	require.NoError(t, m.StartPairing(context.Background(), PairRequest{Method: PairMethodQR}))
	linkedBefore := syncStore.linkedJID()
	require.NotEmpty(t, linkedBefore)
	syncStore.resetCalls()

	_, err := m.Disconnect(context.Background(), true)
	assert.ErrorIs(t, err, ErrPairingInProgress)
	assert.Equal(t, linkedBefore, syncStore.linkedJID(), "the linked device record is untouched")
	assert.Empty(t, syncStore.callLog(), "a refused unlink writes nothing at all")
}

// --- the fence keeps a reachable trigger -------------------------------------

// TestDisconnect_AbortsWhenStartInstallsASessionMidUnlink keeps the FENCE under
// test through a path the exclusion rule leaves reachable.
//
// StartPairing is refused while an unlink is in flight, but Start is not — they
// are different lifecycle phases. A Start whose continuation installs a session
// while the unlink is parked inside its remote call moves st.sess from nil to
// non-nil, and the unlink's continuation must abort rather than publish over it.
func TestDisconnect_AbortsWhenStartInstallsASessionMidUnlink(t *testing.T) {
	oldClient := newFakeClient()
	entered := make(chan struct{})
	release := make(chan struct{})
	oldClient.logoutEntered = entered
	oldClient.logoutBlock = release

	m, _, _, _ := newTestManager(t, oldClient, true)
	require.NoError(t, m.Start(context.Background()))
	require.True(t, dispatchEvent(t, m, nil, &events.Connected{}))

	unlinkErr := make(chan error, 1)
	go func() {
		_, err := m.Disconnect(context.Background(), false)
		unlinkErr <- err
	}()
	<-entered

	// While the unlink is parked inside the remote call, a Start installs a new
	// session.
	newClient := newFakeClient()
	m.setSessionFactory(func(context.Context, sessionRequest) (*session, error) {
		jid := testDeviceJID
		return fakeSessionFor(newClient, true, &jid), nil
	})
	require.NoError(t, m.Start(context.Background()))
	installed := installedSession(t, m)
	require.NotNil(t, installed)

	close(release)
	assert.ErrorIs(t, <-unlinkErr, ErrOperationSuperseded,
		"an unlink whose session slot changed must abort, not apply its decision to the replacement")

	assert.Same(t, installed, installedSession(t, m), "the newly installed session stays")
	assert.NotEqual(t, StateNotPaired, m.Status().State,
		"the unlink must not publish the new session as unpaired")
	assert.NotContains(t, newClient.callLog(), "logout")
}

// TestDisconnect_FailurePublicationIsFenced tables the four failure
// publications. Each is a distinct entry point, and each is the same finding: a
// failure path that publishes without passing the fence.
func TestDisconnect_FailurePublicationIsFenced(t *testing.T) {
	tests := []struct {
		name string
		// park returns the channel that signals the unlink has reached its
		// parking point, and the release function.
		setup func(t *testing.T, cli *fakeClient, devices *fakeDevices) (entered chan struct{}, release chan struct{})
	}{
		{
			name: "failed remote logout",
			setup: func(_ *testing.T, cli *fakeClient, _ *fakeDevices) (chan struct{}, chan struct{}) {
				entered, release := make(chan struct{}), make(chan struct{})
				cli.logoutEntered, cli.logoutBlock = entered, release
				cli.logoutErr = logoutRemoteFailure()
				return entered, release
			},
		},
		{
			name: "successful logout, failed enumeration",
			setup: func(_ *testing.T, cli *fakeClient, devices *fakeDevices) (chan struct{}, chan struct{}) {
				entered, release := make(chan struct{}), make(chan struct{})
				devices.setErr(func(d *fakeDevices) {
					d.listEntered, d.listBlock = entered, release
					d.listErr = errors.New("device store unreachable")
				})
				return entered, release
			},
		},
		{
			name: "successful logout, failed device delete",
			setup: func(_ *testing.T, cli *fakeClient, devices *fakeDevices) (chan struct{}, chan struct{}) {
				entered, release := make(chan struct{}), make(chan struct{})
				devices.setErr(func(d *fakeDevices) {
					d.delEntered, d.delBlock = entered, release
					d.delErr = errors.New("database is down")
				})
				return entered, release
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cli := newFakeClient()
			m, _, _, _, devices := newTestManagerWithDevices(t, cli, true)
			require.NoError(t, m.Start(context.Background()))
			require.True(t, dispatchEvent(t, m, nil, &events.Connected{}))

			entered, release := tt.setup(t, cli, devices)

			unlinkErr := make(chan error, 1)
			go func() {
				_, err := m.Disconnect(context.Background(), false)
				unlinkErr <- err
			}()
			<-entered

			// A Start installs a session while the unlink is parked.
			newClient := newFakeClient()
			m.setSessionFactory(func(context.Context, sessionRequest) (*session, error) {
				jid := testDeviceJID
				return fakeSessionFor(newClient, true, &jid), nil
			})
			require.NoError(t, m.Start(context.Background()))
			installed := installedSession(t, m)
			require.NotNil(t, installed)

			close(release)
			assert.ErrorIs(t, <-unlinkErr, ErrOperationSuperseded)

			assert.NotEqual(t, StateDisconnectFailed, m.Status().State,
				"an aborted operation is structurally incapable of publishing")
			assert.Same(t, installed, installedSession(t, m))
		})
	}
}

// --- unlink deadline expiry --------------------------------------------------

// TestDisconnect_EffectDeadlineExpiryReleasesTheUnlink is the direct regression
// on an effect runner that returned early on ctx.Err().
//
// An effectDeadline expiry OUTSIDE shutdown is an ordinary operation failure: it
// must reach a fenced turn, which is the only place the in-flight flag can be
// cleared and the caller replied to. The last assertion — that a second unlink
// is ACCEPTED — is what proves the flag was released rather than merely
// reported.
func TestDisconnect_EffectDeadlineExpiryReleasesTheUnlink(t *testing.T) {
	cli := newFakeClient()
	devices := newFakeDevices(testDeviceJID)

	m := newDeadlineManager(t, 50*time.Millisecond)
	m.setDeviceOps(devices.ops())
	m.setSessionFactory(func(context.Context, sessionRequest) (*session, error) {
		jid := testDeviceJID
		return fakeSessionFor(cli, true, &jid), nil
	})

	require.NoError(t, m.Start(context.Background()))
	require.True(t, dispatchEvent(t, m, nil, &events.Connected{}))

	// A logout that outlives the effect deadline.
	stuck := make(chan struct{})
	t.Cleanup(func() { close(stuck) })
	cli.mu.Lock()
	cli.logoutBlock = stuck
	cli.mu.Unlock()

	_, err := m.Disconnect(context.Background(), false)
	require.Error(t, err, "the caller must be answered, not left waiting")

	eventually(t, "the published state reports the failure rather than a stale not_paired", func() bool {
		return m.Status().State == StateDisconnectFailed
	})
	assert.False(t, m.inspect().UnlinkInFlight, "the in-flight flag is released by the loop")

	// And the release is real, not merely reported.
	cli2 := newFakeClient()
	m.setSessionFactory(func(context.Context, sessionRequest) (*session, error) {
		jid := testDeviceJID
		return fakeSessionFor(cli2, true, &jid), nil
	})
	_, err = m.Disconnect(context.Background(), false)
	assert.NotErrorIs(t, err, ErrUnlinkInProgress,
		"a second unlink must be accepted, which is what proves the flag was released")
}

// TestPairingOperations_EffectDeadlineExpiryReleaseTheirSlot is the same shape
// for the two pairing-adjacent operations: the caller is answered, the slot or
// flag is released, and the next attempt is accepted.
func TestPairingOperations_EffectDeadlineExpiryReleaseTheirSlot(t *testing.T) {
	t.Run("StartPairing", func(t *testing.T) {
		m := newDeadlineManager(t, 50*time.Millisecond)
		m.setSessionFactory(blockingSessionFactory())

		err := m.StartPairing(context.Background(), PairRequest{Method: PairMethodQR})
		require.Error(t, err, "the caller must be answered, not left waiting")
		eventually(t, "the pairing slot is released", func() bool { return m.Status().Pairing == nil })

		// And the release is real: the next attempt is accepted rather than
		// answering ErrPairingInProgress.
		cli := newFakeClient()
		useClient(m, cli, false)
		assert.NoError(t, m.StartPairing(context.Background(), PairRequest{Method: PairMethodQR}))
	})

	t.Run("Start", func(t *testing.T) {
		m := newDeadlineManager(t, 50*time.Millisecond)
		m.setSessionFactory(blockingSessionFactory())

		require.NoError(t, m.Start(context.Background()))
		assert.Equal(t, StateError, m.Status().State, "the failure is published rather than left pending")
		assert.False(t, m.inspect().StartInFlight, "the start flag is released by the loop")

		// And the release is real: a second Start runs rather than being ignored.
		cli := newFakeClient()
		useClient(m, cli, true)
		require.NoError(t, m.Start(context.Background()))
		assert.Equal(t, StateConnecting, m.Status().State)
	})
}

// newDeadlineManager builds a ready manager whose effect deadline is short
// enough to drive an expiry inside a test.
func newDeadlineManager(t *testing.T, deadline time.Duration) *Manager {
	t.Helper()
	m := NewManager(nil, NewWALogger("whatsapp-test"), nil, newFakeSyncStore(), &fakeBackfillReader{})
	tuneTimeouts(m, func(tm *managerTimeouts) { tm.effect = deadline })
	registerManagerCleanup(t, m)
	m.SetIngestor(&fakeIngestor{})
	m.SetHistoryRecorder(&fakeRecorder{})
	m.SetHistoryDrainReady()
	return m
}

// TestQRItems_NonTerminalItemsDoNotEndTheAttempt covers the four QR items the
// library emits WITHOUT closing the channel.
//
// "err-scanned-without-multidevice" reads like a failure and is not one: the
// emitter goroutine is still counting down the remaining codes, so the next item
// is another scannable code and the user finishes by turning multi-device on.
// The passkey items are non-terminal too, but there is no surface here for that
// handoff, so they end the attempt with a reason rather than leaving the user
// waiting for a prompt that never comes.
func TestQRItems_NonTerminalItemsDoNotEndTheAttempt(t *testing.T) {
	t.Run("scanned without multidevice keeps the attempt alive", func(t *testing.T) {
		cli := newFakeClient()
		m, _, _, _ := newTestManager(t, cli, false)
		require.NoError(t, m.Start(context.Background()))
		require.NoError(t, m.StartPairing(context.Background(), PairRequest{Method: PairMethodQR}))

		cli.qrChan <- whatsmeow.QRChannelScannedWithoutMultidevice
		eventually(t, "the user is told what to change, because a silent retry looks like a scan that did nothing",
			func() bool { return m.Status().Reason == ReasonScannedWithoutMultidevice })

		status := m.Status()
		require.NotNil(t, status.Pairing, "the attempt is still live: the codes have not run out")
		assert.Equal(t, StatePairing, status.State)

		// The next code arrives on the same attempt and is published.
		cli.qrChan <- whatsmeow.QRChannelItem{Event: whatsmeow.QRChannelEventCode, Code: "QR-CODE-AFTER-RESCAN"}
		eventually(t, "the attempt keeps serving codes to scan", func() bool {
			p := m.Status().Pairing
			return p != nil && p.QRCode != nil && *p.QRCode == "QR-CODE-AFTER-RESCAN"
		})
	})

	for _, event := range []string{whatsmeow.QRChannelEventPasskeyRequest, whatsmeow.QRChannelEventPasskeyResponse} {
		t.Run("passkey item "+event+" ends the attempt with a reason", func(t *testing.T) {
			cli := newFakeClient()
			m, _, _, _ := newTestManager(t, cli, false)
			require.NoError(t, m.Start(context.Background()))
			require.NoError(t, m.StartPairing(context.Background(), PairRequest{Method: PairMethodQR}))

			cli.qrChan <- whatsmeow.QRChannelItem{Event: event}
			eventually(t, "the attempt ends rather than running out of codes in silence", func() bool {
				return m.Status().Pairing == nil
			})
			assert.Equal(t, ReasonPasskeyPairingUnsupported, m.Status().Reason)
		})
	}
}

// TestCancelledPairing_RestoresWhatItDisplaced: a re-pair started over a durable
// "logged out" decision and then cancelled must put that decision back. Reporting
// "not paired" instead reads as a clean slate and hides the very reason the user
// was re-pairing.
func TestCancelledPairing_RestoresWhatItDisplaced(t *testing.T) {
	cli := newFakeClient()
	m, _, _, _ := newTestManager(t, cli, true)
	require.NoError(t, m.Start(context.Background()))
	require.True(t, dispatchEvent(t, m, nil, &events.Connected{}))
	require.True(t, dispatchEvent(t, m, nil, &events.LoggedOut{}))
	require.Equal(t, StateDisconnected, m.Status().State)
	require.Equal(t, ReasonLoggedOut, m.Status().Reason)

	pairClient := newFakeClient()
	useClient(m, pairClient, false)
	require.NoError(t, m.StartPairing(context.Background(), PairRequest{Method: PairMethodQR}))
	require.Equal(t, StatePairing, m.Status().State)

	m.CancelPairing()

	status := m.Status()
	assert.Equal(t, StateDisconnected, status.State,
		"the terminal decision the attempt displaced is still the truth about this device")
	assert.Equal(t, ReasonLoggedOut, status.Reason)
}

// TestLogoutFailedAfterRemoteUnlink_MatchesOnlyTheLibrarysOwnWrapper: this
// predicate decides whether to destroy local credentials, so what it matches
// has to be ours.
//
// whatsmeow returns "error deleting data from store: %w" from Logout itself,
// AFTER the remote unlink succeeded — so the marker is always the front of the
// message. A substring match would also accept an error whose text came from the
// server, which is text an attacker-influenced payload can reach.
func TestLogoutFailedAfterRemoteUnlink_MatchesOnlyTheLibrarysOwnWrapper(t *testing.T) {
	assert.True(t, logoutFailedAfterRemoteUnlink(logoutLocalDeleteFailure()),
		"the library's own wrapper is the whole point of the predicate")

	assert.False(t, logoutFailedAfterRemoteUnlink(nil))
	assert.False(t, logoutFailedAfterRemoteUnlink(logoutRemoteFailure()),
		"a failed remote unlink proves nothing about the device and must keep the credentials")
	assert.False(t, logoutFailedAfterRemoteUnlink(
		fmt.Errorf("error sending logout request: %w", errors.New("server said: error deleting data from store"))),
		"a server-supplied message that merely CONTAINS the marker must not be read as a successful unlink")
}
