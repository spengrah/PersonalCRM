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

// TestStartPairing_ExpiredPairingIsTakenOverNotConflicted: an expired attempt
// must not wedge the single pairing slot. Its codes are dead, so refusing a
// fresh attempt would leave the settings page stuck on a conflict until someone
// thought to press cancel.
func TestStartPairing_ExpiredPairingIsTakenOverNotConflicted(t *testing.T) {
	cli := newFakeClient()
	m, _, _, _ := newTestManager(t, cli, false)

	require.NoError(t, m.StartPairing(context.Background(), PairRequest{Method: PairMethodPhone, Phone: "+15551234567"}))

	// Backdate the in-flight attempt past its TTL.
	m.mu.Lock()
	m.pairing.expiresAt = accelerated.GetCurrentTime().Add(-time.Second)
	m.mu.Unlock()

	require.NoError(t, m.StartPairing(context.Background(), PairRequest{Method: PairMethodPhone, Phone: "+15551234567"}),
		"an expired attempt is taken over, not treated as a conflict")

	status := m.Status()
	require.NotNil(t, status.Pairing)
	assert.True(t, status.Pairing.ExpiresAt.After(accelerated.GetCurrentTime()),
		"the surviving attempt is the fresh one, with a live TTL")
	assert.Contains(t, cli.callLog(), "delete_device",
		"the taken-over attempt's half-written device is removed")
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

	require.Eventually(t, func() bool { return m.Status().Pairing == nil }, 2*time.Second, 10*time.Millisecond,
		"a closed QR channel must release the pairing slot")
	assert.Equal(t, StateNotPaired, m.Status().State)
}

// TestRunQRChannel_RefreshesTheStoredCode: the library emits a fresh code every
// Timeout until they run out, so the stored code must keep up or the settings
// page shows a code that no longer scans.
func TestRunQRChannel_RefreshesTheStoredCode(t *testing.T) {
	cli := newFakeClient()
	m, _, _, _ := newTestManager(t, cli, false)

	require.NoError(t, m.StartPairing(context.Background(), PairRequest{Method: PairMethodQR}))
	require.NotNil(t, m.Status().Pairing)
	require.Equal(t, defaultFakeQRCode, *m.Status().Pairing.QRCode)

	cli.qrChan <- whatsmeow.QRChannelItem{Event: whatsmeow.QRChannelEventCode, Code: "QR-CODE-2"}

	require.Eventually(t, func() bool {
		p := m.Status().Pairing
		return p != nil && p.QRCode != nil && *p.QRCode == "QR-CODE-2"
	}, 2*time.Second, 10*time.Millisecond, "the stored code must follow the channel")
}

// TestStartPairing_PhoneWaitsForConnectionBeforeRequestingCode pins the ordering
// the library documents: PairPhone must not be called until the pairing socket
// has produced its first QR item, or it races the handshake. The fake records
// call order, so a regression to "PairPhone straight after Connect" shows up
// here rather than as an intermittent production failure.
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
// from the library too, but they are not the user's affordance, so reporting one
// would show two competing codes on the settings page.
func TestStartPairing_PhoneDoesNotReportAQRCode(t *testing.T) {
	cli := newFakeClient()
	m, _, _, _ := newTestManager(t, cli, false)

	require.NoError(t, m.StartPairing(context.Background(), PairRequest{Method: PairMethodPhone, Phone: "+15551234567"}))

	status := m.Status()
	require.NotNil(t, status.Pairing)
	assert.Nil(t, status.Pairing.QRCode)
	require.NotNil(t, status.Pairing.PairCode)
}

// TestStartPairing_CancelDuringSessionBuildDiscardsTheClient is the deterministic
// proof for the cancel-window race.
//
// The attempt is published into Manager.pairing BEFORE its client exists, so a
// cancel landing in that window used to clear the slot and let the original
// goroutine attach and connect an orphaned client — one Stop() could not reach
// and that could still complete a pairing the manager never recorded. The
// session factory blocks here so the cancel lands squarely inside that window.
func TestStartPairing_CancelDuringSessionBuildDiscardsTheClient(t *testing.T) {
	cli := newFakeClient()
	m, _, _, _ := newTestManager(t, cli, false)

	building := make(chan struct{})
	release := make(chan struct{})
	inner := m.newSession
	m.newSession = func(ctx context.Context, fresh bool) (*session, error) {
		close(building)
		<-release
		return inner(ctx, fresh)
	}

	errCh := make(chan error, 1)
	go func() { errCh <- m.StartPairing(context.Background(), PairRequest{Method: PairMethodQR}) }()

	<-building
	m.CancelPairing()
	close(release)

	assert.ErrorIs(t, <-errCh, ErrPairingCancelled)
	assert.NotContains(t, cli.callLog(), "connect",
		"a cancelled attempt must never connect the client it was still building")
	assert.Contains(t, cli.callLog(), "delete_device",
		"the discarded device is removed rather than orphaned")
	assert.Nil(t, m.Status().Pairing)
	assert.Equal(t, StateNotPaired, m.Status().State)
}

// TestStop_DuringSessionBuildDiscardsTheClient is the same window seen through
// Stop() — a process shutdown must not leave a client connecting behind it.
func TestStop_DuringSessionBuildDiscardsTheClient(t *testing.T) {
	cli := newFakeClient()
	m, _, _, _ := newTestManager(t, cli, false)

	building := make(chan struct{})
	release := make(chan struct{})
	inner := m.newSession
	m.newSession = func(ctx context.Context, fresh bool) (*session, error) {
		close(building)
		<-release
		return inner(ctx, fresh)
	}

	errCh := make(chan error, 1)
	go func() { errCh <- m.StartPairing(context.Background(), PairRequest{Method: PairMethodQR}) }()

	<-building
	m.Stop()
	close(release)

	assert.ErrorIs(t, <-errCh, ErrPairingCancelled)
	assert.NotContains(t, cli.callLog(), "connect",
		"Stop must win over an attach that has not happened yet")
}

// TestDisconnect_StopsTheOldClientBeforeBuildingTheUnlinkClient is the
// deterministic proof for the double-client defect.
//
// whatsmeow starts auto-reconnect on a remote disconnect, and only Disconnect()
// marks the drop expected. So after a Disconnected event the installed client is
// very likely mid-retry; building a second client for the unlink without
// stopping it first puts two clients on one device and races the unlink against
// a reconnect.
//
// Both clients append to ONE ordered log, and the unlink client's CONSTRUCTION
// is recorded on it too. Separate per-client logs could only show that each call
// happened — moving the teardown to after the unlink client was built and
// connected would still satisfy them.
func TestDisconnect_StopsTheOldClientBeforeBuildingTheUnlinkClient(t *testing.T) {
	shared := &sharedLog{}
	old := newFakeClient()
	old.name, old.shared = "old", shared
	unlink := newFakeClient()
	unlink.name, unlink.shared = "unlink", shared

	m, _, _, _ := newTestManager(t, old, true)
	require.NoError(t, m.Start(context.Background()))
	require.True(t, m.handleEvent(&events.Connected{}))

	// The socket drops; whatsmeow is now auto-reconnecting behind the scenes.
	require.True(t, m.handleEvent(&events.Disconnected{}))
	old.mu.Lock()
	old.connected = false
	old.mu.Unlock()
	require.Equal(t, StateReconnecting, m.Status().State)

	m.newSession = func(context.Context, bool) (*session, error) {
		shared.record("unlink:built")
		return &session{client: unlink, paired: true, deleteDevice: func(context.Context) error {
			unlink.record("delete_device")
			return nil
		}}, nil
	}

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
//
// By then the attempt is marked and its device deleted, so publishing
// "connected" would report a live account backed by credentials that no longer
// exist. The earlier fence only covered the window BEFORE the client was
// attached; this covers the window after.
func TestOnPairSuccess_FromCancelledPairingIsIgnored(t *testing.T) {
	cli := newFakeClient()
	m, _, _, _ := newTestManager(t, cli, false)

	require.NoError(t, m.StartPairing(context.Background(), PairRequest{Method: PairMethodQR}))
	pairingSess := m.pairing.session()
	require.NotNil(t, pairingSess)

	m.CancelPairing()
	require.Nil(t, m.Status().Pairing)
	require.Contains(t, cli.callLog(), "delete_device")

	// The scan lands anyway.
	jid := types.NewJID("15551234567", types.DefaultUserServer)
	assert.True(t, m.handleEventFor(pairingSess, &events.PairSuccess{ID: jid}))

	status := m.Status()
	assert.Equal(t, StateNotPaired, status.State,
		"a cancelled pairing must never publish connected")
	assert.Nil(t, status.JID)
	assert.Nil(t, m.HistoryFetcher(), "no session may be adopted")

	disconnects := 0
	for _, c := range cli.callLog() {
		if c == "disconnect" {
			disconnects++
		}
	}
	assert.GreaterOrEqual(t, disconnects, 2,
		"the abandoned client is torn down again rather than left holding a socket")
}

// TestOnPairSuccess_FromSupersededPairingIsIgnored: an earlier attempt whose TTL
// expired was taken over by a newer one. Adopting the stale client would discard
// the pairing the user is actually looking at.
func TestOnPairSuccess_FromSupersededPairingIsIgnored(t *testing.T) {
	cli := newFakeClient()
	m, _, _, _ := newTestManager(t, cli, false)

	require.NoError(t, m.StartPairing(context.Background(), PairRequest{Method: PairMethodQR}))
	stale := m.pairing.session()
	require.NotNil(t, stale)

	m.mu.Lock()
	m.pairing.expiresAt = accelerated.GetCurrentTime().Add(-time.Second)
	m.mu.Unlock()
	require.NoError(t, m.StartPairing(context.Background(), PairRequest{Method: PairMethodQR}))
	current := m.pairing.session()
	require.NotNil(t, current)
	require.NotSame(t, stale, current)

	jid := types.NewJID("15551234567", types.DefaultUserServer)
	assert.True(t, m.handleEventFor(stale, &events.PairSuccess{ID: jid}))

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
	orphan := m.pairing.session()
	m.CancelPairing()

	assert.True(t, m.handleEventFor(orphan, &events.Connected{}))
	assert.Equal(t, StateNotPaired, m.Status().State)
}

// TestClearLocalDevice_ResetsTerminalReasonPersisted: the field is meaningful
// only alongside a terminal state, so a not_paired status must not still carry
// a stale "the decision was recorded" from a device that no longer exists.
func TestClearLocalDevice_ResetsTerminalReasonPersisted(t *testing.T) {
	cli := newFakeClient()
	m, _, _, _ := newTestManager(t, cli, true)
	require.NoError(t, m.Start(context.Background()))
	require.True(t, m.handleEvent(&events.Connected{}))
	require.True(t, m.handleEvent(&events.LoggedOut{}))
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
// deletes the local store, wrapping each failure with its own prefix
// (client.go:715) — so the SECOND shape means the device is already gone
// server-side and only local cleanup is outstanding.
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
	m, _, _, _ := newTestManager(t, cli, true)
	require.NoError(t, m.Start(context.Background()))
	require.True(t, m.handleEvent(&events.Connected{}))

	result, err := m.Disconnect(context.Background(), false)
	assert.Nil(t, result)
	assert.ErrorIs(t, err, ErrRemoteUnlinkFailed)
	assert.NotContains(t, cli.callLog(), "delete_device", "the device is KEPT so the user can retry the unlink")
	assert.Equal(t, StateDisconnectFailed, m.Status().State)
}

// TestDisconnect_LocalDeleteFailureAfterRemoteSuccessIsNotAFailedUnlink is the
// half the old code got backwards.
//
// The library returns this error AFTER the remote unlink has already succeeded.
// Reporting it as "the unlink failed, retry" sends the user at the half that
// worked — and a retried unlink against an already-unlinked device cannot
// succeed. The right response is to finish the LOCAL clear.
func TestDisconnect_LocalDeleteFailureAfterRemoteSuccessIsNotAFailedUnlink(t *testing.T) {
	cli := newFakeClient()
	cli.logoutErr = logoutLocalDeleteFailure()
	m, _, _, _ := newTestManager(t, cli, true)
	require.NoError(t, m.Start(context.Background()))
	require.True(t, m.handleEvent(&events.Connected{}))

	result, err := m.Disconnect(context.Background(), false)
	require.NoError(t, err, "the remote device is gone; this is not a failed unlink")
	require.NotNil(t, result)
	assert.True(t, result.RemoteUnlinked)
	assert.Contains(t, cli.callLog(), "delete_device", "the local clear is completed rather than reported as an unlink failure")
	assert.Equal(t, StateNotPaired, m.Status().State)
}

// TestDisconnect_LocalCleanupFailureIsSurfaced: the device row could not be
// deleted, so the credentials are still there. Publishing not_paired would tell
// the user the integration is clean while the next boot resumes the very device
// they asked to remove.
func TestDisconnect_LocalCleanupFailureIsSurfaced(t *testing.T) {
	cli := newFakeClient()
	cli.logoutErr = logoutLocalDeleteFailure()
	cli.deleteDeviceErr = errors.New("database is down")
	m, _, _, _ := newTestManager(t, cli, true)
	require.NoError(t, m.Start(context.Background()))
	require.True(t, m.handleEvent(&events.Connected{}))

	result, err := m.Disconnect(context.Background(), false)
	assert.Nil(t, result)
	assert.ErrorIs(t, err, ErrLocalCleanupFailed)
	assert.NotErrorIs(t, err, ErrRemoteUnlinkFailed, "the unlink itself succeeded")

	status := m.Status()
	assert.Equal(t, StateDisconnectFailed, status.State)
	assert.Equal(t, ReasonLocalCleanupFailed, status.Reason)
	assert.NotEqual(t, StateNotPaired, status.State, "a device that is still stored is not 'not paired'")

	// A retry makes no further remote call: the device is already unlinked, and
	// an unlink against an unlinked device cannot succeed.
	cli.mu.Lock()
	cli.deleteDeviceErr = nil
	cli.calls = nil
	cli.mu.Unlock()

	result, err = m.Disconnect(context.Background(), false)
	require.NoError(t, err)
	assert.True(t, result.AlreadyUnlinked)
	assert.NotContains(t, cli.callLog(), "logout", "the remote half is settled; only the local clear was outstanding")
	assert.Contains(t, cli.callLog(), "delete_device")
	assert.Equal(t, StateNotPaired, m.Status().State)
}

// TestDisconnect_ForceReportsAFailedLocalClear: force skips the remote call, but
// it cannot skip reality — if the stored device survives, saying it was cleared
// is a lie.
func TestDisconnect_ForceReportsAFailedLocalClear(t *testing.T) {
	cli := newFakeClient()
	cli.deleteDeviceErr = errors.New("database is down")
	m, _, _, _ := newTestManager(t, cli, true)
	require.NoError(t, m.Start(context.Background()))

	result, err := m.Disconnect(context.Background(), true)
	assert.Nil(t, result)
	assert.ErrorIs(t, err, ErrLocalCleanupFailed)
	assert.Equal(t, StateDisconnectFailed, m.Status().State)
	assert.Equal(t, ReasonForcedCleanupFailed, m.Status().Reason,
		"force made no remote call, so it learned nothing about the remote device")

	// And that distinction has to hold: a later ordinary unlink must still try
	// the remote half, rather than reading the failed force as proof the device
	// was already unlinked.
	cli.mu.Lock()
	cli.deleteDeviceErr = nil
	cli.calls = nil
	cli.connected = true
	cli.mu.Unlock()

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
	m, _, _, _ := newTestManager(t, cli, true)
	require.NoError(t, m.Start(context.Background()))
	require.True(t, m.handleEvent(&events.Connected{}))
	require.True(t, m.handleEvent(&events.LoggedOut{}))

	cli.mu.Lock()
	cli.deleteDeviceErr = errors.New("database is down")
	cli.mu.Unlock()

	result, err := m.Disconnect(context.Background(), false)
	assert.Nil(t, result)
	assert.ErrorIs(t, err, ErrLocalCleanupFailed)
	assert.NotContains(t, cli.callLog(), "logout")
	assert.Equal(t, ReasonLocalCleanupFailed, m.Status().Reason)
}

// --- API operations obey the same ownership fence ---------------------------

// TestDisconnect_AbortsWhenAPairingIsAdoptedMidUnlink applies the ownership rule
// to an API operation rather than an event.
//
// An unlink is multi-step and slow — it disconnects the installed session,
// builds a client, connects, and sends a remote request. A pairing started and
// completed in that window installs a NEW session, and the unlink's final step
// used to clear the pairing slot, retire whatever session it found, and set it
// nil — abandoning a pairing the user had just completed and leaving its client
// connected with nothing owning it.
//
// The unlink now carries the generation it decided on and aborts instead.
func TestDisconnect_AbortsWhenAPairingIsAdoptedMidUnlink(t *testing.T) {
	oldClient := newFakeClient()
	entered := make(chan struct{})
	release := make(chan struct{})
	oldClient.logoutEntered = entered
	oldClient.logoutBlock = release

	m, _, _, _ := newTestManager(t, oldClient, true)
	require.NoError(t, m.Start(context.Background()))
	require.True(t, m.handleEvent(&events.Connected{}))

	unlinkErr := make(chan error, 1)
	go func() {
		_, err := m.Disconnect(context.Background(), false)
		unlinkErr <- err
	}()
	<-entered

	// While the unlink is parked inside the remote call, the user re-pairs.
	newClient := newFakeClient()
	m.newSession = func(context.Context, bool) (*session, error) {
		return &session{client: newClient, deleteDevice: func(context.Context) error {
			newClient.record("delete_device")
			return nil
		}}, nil
	}
	require.NoError(t, m.StartPairing(context.Background(), PairRequest{Method: PairMethodQR}))
	pairingSess := m.pairing.session()
	require.NotNil(t, pairingSess)
	require.True(t, m.handleEventFor(pairingSess, &events.PairSuccess{
		ID: types.NewJID("15557778888", types.DefaultUserServer),
	}))

	close(release)
	assert.ErrorIs(t, <-unlinkErr, ErrOperationSuperseded,
		"an unlink whose session was replaced must abort, not apply its decision to the replacement")

	m.mu.RLock()
	installed := m.sess
	m.mu.RUnlock()

	assert.Same(t, pairingSess, installed, "the newly paired session stays installed")
	assert.Equal(t, StateConnecting, m.Status().State,
		"the unlink must not publish the new session as unpaired")
	assert.NotContains(t, newClient.callLog(), "disconnect",
		"the new client must not be left orphaned — disconnected by nobody, owned by nobody")
	assert.NotContains(t, newClient.callLog(), "delete_device",
		"and its device must survive the unlink of the one it replaced")
}

// TestDisconnect_AfterRestartIntoLoggedOutSurfacesAnUnreachableStore is the
// no-session case.
//
// After a restart into a persisted logged_out state, Start deliberately installs
// no session, so both the forced and the server-confirmed clear reach
// clearLocalDevice with nothing to delete through and have to build one. When
// that build fails the credentials are still stored, and answering 200/not_paired
// would report a device as gone while the next boot resumes it.
func TestDisconnect_AfterRestartIntoLoggedOutSurfacesAnUnreachableStore(t *testing.T) {
	for _, force := range []bool{false, true} {
		name := "server confirmed"
		if force {
			name = "forced"
		}
		t.Run(name, func(t *testing.T) {
			cli := newFakeClient()
			m, syncStore, _, _ := newTestManager(t, cli, true)
			syncStore.seedTerminal(ReasonLoggedOut, nil)

			require.NoError(t, m.Start(context.Background()))
			require.Equal(t, StateDisconnected, m.Status().State)
			m.mu.RLock()
			installed := m.sess
			m.mu.RUnlock()
			require.Nil(t, installed, "the terminal gate deliberately installs no session")

			m.newSession = func(context.Context, bool) (*session, error) {
				return nil, errors.New("device store unreachable")
			}

			result, err := m.Disconnect(context.Background(), force)
			assert.Nil(t, result)
			assert.ErrorIs(t, err, ErrLocalCleanupFailed,
				"a store we cannot open is a cleanup that did not happen")
			assert.Equal(t, StateDisconnectFailed, m.Status().State,
				"the credentials are still stored, so the status must not say not_paired")
		})
	}
}

// TestDisconnect_WithNothingStoredClearsCleanly is the negative control for the
// case above: an EMPTY store is not a failed cleanup. The library refuses to
// delete a device with no JID, and treating that refusal as a failure would make
// every clear on an unlinked integration report an error.
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
