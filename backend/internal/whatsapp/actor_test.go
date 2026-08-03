package whatsapp

import (
	"context"
	"errors"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

// --- source-scan assertions -------------------------------------------------
//
// Together these say "there is one goroutine, one publish point, and no shared
// state". Each names a concrete failure that no type can prevent. They are
// tripwires, not the guarantee: the guarantee is the -race lane, which a
// blocking callback invoked from library code would not escape.

// packageSources returns the package's non-test .go files with comments and
// string literals stripped, so a scan cannot be fooled by prose or by a message
// that happens to contain the token it looks for.
func packageSources(t *testing.T) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(".")
	require.NoError(t, err)

	out := map[string]string{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		raw, err := os.ReadFile(name)
		require.NoError(t, err)
		out[name] = stripCommentsAndStrings(string(raw))
	}
	require.NotEmpty(t, out, "the scan found no sources, which would make every assertion below vacuous")
	require.Contains(t, out, "actor.go")
	return out
}

func stripCommentsAndStrings(src string) string {
	var b strings.Builder
	for i := 0; i < len(src); {
		switch {
		case strings.HasPrefix(src[i:], "//"):
			for i < len(src) && src[i] != '\n' {
				i++
			}
		case strings.HasPrefix(src[i:], "/*"):
			end := strings.Index(src[i+2:], "*/")
			if end < 0 {
				return b.String()
			}
			i += end + 4
		case src[i] == '"' || src[i] == '`':
			quote := src[i]
			i++
			for i < len(src) && src[i] != quote {
				if quote == '"' && src[i] == '\\' {
					i++
				}
				i++
			}
			i++
		default:
			b.WriteByte(src[i])
			i++
		}
	}
	return b.String()
}

func countOccurrences(sources map[string]string, needle string) int {
	n := 0
	for _, src := range sources {
		n += strings.Count(src, needle)
	}
	return n
}

// TestPackageHasNoMutex is the machine-checked form of the whole design: there
// is no shared mutable state, so there is nothing to lock.
//
// The permitted primitives are asserted by COUNT rather than allowlisted by
// type, because a count is what catches the third one someone adds.
func TestPackageHasNoMutex(t *testing.T) {
	sources := packageSources(t)

	for _, banned := range []string{"sync.Mutex", "sync.RWMutex", "sync.Map"} {
		for name, src := range sources {
			assert.NotContains(t, src, banned,
				"%s reintroduces shared mutable state — the defect class this design removes", name)
		}
	}

	assert.Equal(t, 2, countOccurrences(sources, "sync.Once"),
		"exactly two: the process-wide device props and Manager.stopOnce")
	assert.Equal(t, 1, countOccurrences(sources, "sync.WaitGroup"),
		"exactly one: Manager.effects, the effect-goroutine census")
	assert.Equal(t, 2, countOccurrences(sources, "atomic.Pointer"),
		"exactly two: the published snapshot and the backfill cache")
}

var goStatementRe = regexp.MustCompile(`(^|[\s;{}(])go\s+[A-Za-z_(]`)

// TestOnlyTheActorStartsGoroutines: a helper that "just" spawns a goroutine
// touching manager state is how the pairing goroutines came to need their own
// lock in the first place.
func TestOnlyTheActorStartsGoroutines(t *testing.T) {
	sources := packageSources(t)

	for name, src := range sources {
		found := goStatementRe.FindAllString(src, -1)
		if name == "actor.go" {
			assert.Len(t, found, 2,
				"actor.go starts exactly two goroutines: the loop, and the single runEffects spawn inside launch")
			continue
		}
		assert.Empty(t, found, "%s starts a goroutine outside the actor", name)
	}

	// The ways to start a goroutine without writing `go `. A scan that misses
	// them reads as passing while the invariant is gone.
	for _, banned := range []string{"errgroup", ".Go(", "sync.WaitGroup.Go"} {
		assert.Equal(t, 0, countOccurrences(sources, banned),
			"%s is a way to start a goroutine that the `go ` scan would not see", banned)
	}
}

// TestSnapshotIsPublishedFromOnePlace guards against a second publication path,
// which is the shape of every failure-path finding this design closed.
func TestSnapshotIsPublishedFromOnePlace(t *testing.T) {
	sources := packageSources(t)

	assert.Equal(t, 1, countOccurrences(sources, "m.snap.Store("),
		"the snapshot is published only by publish(), only from the loop")
	assert.Equal(t, 1, strings.Count(sources["actor.go"], "m.snap.Store("),
		"and that one place is actor.go")

	// A named exception: the backfill cache holds no manager state and is
	// written by Status() on the caller's goroutine. Counting it too stops the
	// exception quietly becoming a second state-publication path.
	assert.Equal(t, 1, countOccurrences(sources, "m.backfill.Store("))
}

// --- behavioural guards -----------------------------------------------------

// TestActorNeverBlocksOnItsOwnDispatch is the runtime half of the
// only-the-actor-starts-goroutines assertion.
//
// A turn emits a disconnectEffect on a client whose Disconnect re-enters the
// manager and WAITS for the loop. If the loop ever ran an effect inline, that
// wait would be the loop waiting for itself and this test would time out.
func TestActorNeverBlocksOnItsOwnDispatch(t *testing.T) {
	cli := newFakeClient()
	m, _, _, _ := newTestManager(t, cli, true)
	require.NoError(t, m.Start(context.Background()))
	require.True(t, dispatchEvent(t, m, nil, &events.Connected{}))

	reentered := make(chan struct{}, 1)
	cli.mu.Lock()
	cli.disconnectHook = func() {
		m.settle() // would deadlock against the loop if effects ran inline
		select {
		case reentered <- struct{}{}:
		default:
		}
	}
	cli.mu.Unlock()

	// A terminal event emits a disconnectEffect for the dead session.
	require.True(t, m.handleEventFor(nil, &events.LoggedOut{}))

	select {
	case <-reentered:
	case <-time.After(5 * time.Second):
		t.Fatal("the loop ran an effect inline and deadlocked against its own dispatch")
	}
}

// TestActorSerializesTerminalAndAdoption replaces the test that proved a mutex
// was held. Under the actor the race is unrepresentable, so what is proved is
// SERIALIZATION directly: the adoption is not applied while the terminal turn is
// parked, and the terminal never speaks for the session that replaced it.
func TestActorSerializesTerminalAndAdoption(t *testing.T) {
	installed := newFakeClient()
	m, syncStore, _, _ := newTestManager(t, installed, true)
	require.NoError(t, m.Start(context.Background()))
	require.True(t, dispatchEvent(t, m, nil, &events.Connected{}))

	sessA := installedSession(t, m)
	require.NotNil(t, sessA)
	installed.setConnected(false)

	pairingClient := newFakeClient()
	useClient(m, pairingClient, false)
	require.NoError(t, m.StartPairing(context.Background(), PairRequest{Method: PairMethodQR}))
	sessB := pairingSession(t, m)
	require.NotNil(t, sessB)

	entered := make(chan struct{})
	release := make(chan struct{})
	syncStore.setErr(func(f *fakeSyncStore) {
		f.terminalEntered = entered
		f.terminalBlock = release
	})

	require.True(t, m.handleEventFor(sessA, &events.LoggedOut{}))
	<-entered

	// The adoption is submitted while the terminal turn is parked. It queues.
	require.True(t, m.handleEventFor(sessB, &events.PairSuccess{
		ID: types.NewJID("15551234567", types.DefaultUserServer),
	}))

	assert.NotNil(t, m.Status().Pairing,
		"the adoption has NOT been applied: turns run strictly one at a time")

	close(release)
	m.settle()

	assert.Same(t, sessB, installedSession(t, m), "the newly paired session is installed")
	assert.Equal(t, StateConnecting, m.Status().State,
		"a dead session must not publish the session that replaced it as disconnected")
	assert.NotContains(t, pairingClient.callLog(), "disconnect",
		"the newly paired client must not be torn down by the old session's event")

	// Retirement is permanent: a Connected still queued on A cannot revive it.
	assert.True(t, dispatchEvent(t, m, sessA, &events.Connected{}))
	assert.Equal(t, StateConnecting, m.Status().State)
}

// TestTurnBudgetHoldsForTheWorstTurn asserts the arithmetic rather than assuming
// it: a terminal turn against a database that never answers is the worst case,
// and it must still end inside the budget the queue's latency rests on.
func TestTurnBudgetHoldsForTheWorstTurn(t *testing.T) {
	m, syncStore, _, _ := newTestManager(t, newFakeClient(), true)
	require.NoError(t, m.Start(context.Background()))
	require.True(t, dispatchEvent(t, m, nil, &events.Connected{}))

	wedged := make(chan struct{})
	t.Cleanup(func() { close(wedged) })
	syncStore.setErr(func(f *fakeSyncStore) { f.terminalBlock = wedged })

	started := time.Now() //nolint:forbidigo // Wall-clock: the budget is real elapsed time.
	require.True(t, m.handleEventFor(nil, &events.LoggedOut{}))

	done := make(chan struct{})
	go func() { m.settle(); close(done) }()

	select {
	case <-done:
	case <-time.After(maxTurnBudget + 2*time.Second):
		t.Fatalf("the worst turn exceeded its budget of %s", maxTurnBudget)
	}
	assert.Less(t, time.Since(started), maxTurnBudget+2*time.Second)
}

// TestStatusSnapshotIsDeepCopied: a shallow copy would publish pointers into
// loop-owned memory, so a caller mutating a returned Status would corrupt the
// published snapshot and race the loop.
func TestStatusSnapshotIsDeepCopied(t *testing.T) {
	cli := newFakeClient()
	m, _, _, _ := newTestManager(t, cli, false)
	require.NoError(t, m.Start(context.Background()))
	require.NoError(t, m.StartPairing(context.Background(), PairRequest{Method: PairMethodPhone, Phone: "+15551234567"}))
	require.True(t, dispatchEvent(t, m, pairingSession(t, m), &events.PairSuccess{
		ID:           types.NewJID("15551234567", types.DefaultUserServer),
		BusinessName: "Acme",
	}))
	require.True(t, dispatchEvent(t, m, installedSession(t, m), &events.Connected{}))
	require.NoError(t, m.StartPairing(context.Background(), PairRequest{Method: PairMethodPhone, Phone: "+15559998888"}))

	before := m.Status()
	require.NotNil(t, before.JID)
	require.NotNil(t, before.PhoneNumber)
	require.NotNil(t, before.PushName)
	require.NotNil(t, before.ConnectedAt)
	require.NotNil(t, before.Pairing)
	require.NotNil(t, before.Pairing.PairCode)

	*before.JID = "tampered"
	*before.PhoneNumber = "tampered"
	*before.PushName = "tampered"
	*before.ConnectedAt = time.Unix(0, 0)
	*before.Pairing.PairCode = "tampered"
	before.Pairing.Method = "tampered"

	after := m.Status()
	assert.NotEqual(t, "tampered", *after.JID)
	assert.NotEqual(t, "tampered", *after.PhoneNumber)
	assert.NotEqual(t, "tampered", *after.PushName)
	assert.NotEqual(t, time.Unix(0, 0), *after.ConnectedAt)
	assert.NotEqual(t, "tampered", *after.Pairing.PairCode)
	assert.NotEqual(t, "tampered", after.Pairing.Method)
}

// TestStatus_ReturnsCachedBackfillWhenTheRepositoryIsWedged: the status endpoint
// is what an operator hits DURING an outage, so a wedged history table must
// degrade the answer rather than hang it.
func TestStatus_ReturnsCachedBackfillWhenTheRepositoryIsWedged(t *testing.T) {
	reader := &fakeBackfillReader{counts: map[string]int{"pending/project": 7}}
	m, _, _, _, _ := newTestManagerFull(t, newFakeClient(), true, reader)

	fresh := m.Status().Backfill
	require.Equal(t, 7, fresh.Pending)
	require.False(t, fresh.Stale)

	block := make(chan struct{})
	t.Cleanup(func() { close(block) })
	reader.mu.Lock()
	reader.block = block
	reader.mu.Unlock()

	started := time.Now() //nolint:forbidigo // Wall-clock: the read bound is real elapsed time.
	wedged := m.Status().Backfill
	assert.Less(t, time.Since(started), backfillReadTimeout+2*time.Second,
		"the read is bounded, so the endpoint answers rather than hanging")
	assert.True(t, wedged.Stale, "a value that could not be refreshed says so")
	assert.Equal(t, 7, wedged.Pending, "the last good counts are served, never a fabricated zero")
}

// --- the data plane never enters the queue ----------------------------------

// TestDataPlaneRaceOnIngestorSwap hammers the seam installation while messages
// are dispatched from several goroutines. The worst an interleaving can produce
// is "this event used the previous seam", which is correct — the previous seam
// was a real, durable sink at the moment the event arrived.
func TestDataPlaneRaceOnIngestorSwap(t *testing.T) {
	m, _, _, _ := newTestManager(t, newFakeClient(), true)

	var wg sync.WaitGroup
	stop := make(chan struct{})

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			m.SetIngestor(&fakeIngestor{})
		}
	}()

	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				m.handleEvent(newMessageEvent("msg"))
			}
		}()
	}

	time.Sleep(10 * time.Millisecond)
	close(stop)
	wg.Wait()
}

// TestDataPlaneRaceOnRecorderSwap is the same for the capture path.
func TestDataPlaneRaceOnRecorderSwap(t *testing.T) {
	m, _, _, _ := newTestManager(t, newFakeClient(), true)

	var wg sync.WaitGroup
	stop := make(chan struct{})

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			m.SetHistoryRecorder(&fakeRecorder{})
		}
	}()

	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				m.handleEvent(newHistoryNotificationEvent("proto", nil))
			}
		}()
	}
	time.Sleep(10 * time.Millisecond)
	close(stop)
	wg.Wait()
}

// TestDataPlaneRecorderFailureUnderConcurrentSwap: a recorder failure must still
// withhold the ack while a swap is in flight.
func TestDataPlaneRecorderFailureUnderConcurrentSwap(t *testing.T) {
	m, _, _, _ := newTestManager(t, newFakeClient(), true)

	failing := &fakeRecorder{}
	failing.setErr(errors.New("insert failed"))
	m.SetHistoryRecorder(failing)

	var wg sync.WaitGroup
	stop := make(chan struct{})

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			m.SetHistoryRecorder(failing)
		}
	}()

	withheld := 0
	for i := 0; i < 200; i++ {
		if !m.handleEvent(newHistoryNotificationEvent("proto", nil)) {
			withheld++
		}
	}
	close(stop)
	wg.Wait()

	assert.Equal(t, 200, withheld, "every failed capture withholds its ack")
}

// --- control-plane enqueue policy -------------------------------------------

// TestControlEvent_ReturnsImmediatelyWhileATurnIsBlocked: the verdict for every
// control event is a constant, so waiting for the turn would put a
// seconds-long database write on the critical path of a synchronously
// dispatched node handler.
func TestControlEvent_ReturnsImmediatelyWhileATurnIsBlocked(t *testing.T) {
	m, _, _, _ := newTestManager(t, newFakeClient(), true)
	require.NoError(t, m.Start(context.Background()))

	release := parkLoop(t, m)
	t.Cleanup(release)

	done := make(chan bool, 1)
	go func() { done <- m.handleEventFor(nil, &events.Connected{}) }()

	select {
	case ok := <-done:
		assert.True(t, ok)
	case <-time.After(2 * time.Second):
		t.Fatal("the handler waited for its turn; the ack path must never do that")
	}
}

// TestFullMailbox_TerminalEventsAreEventuallyPersisted: a dropped terminal event
// may be the last thing that client ever emits, and its loss is the durable
// record that stops the next boot reconnecting a dead or banned session.
// Permanently.
func TestFullMailbox_TerminalEventsAreEventuallyPersisted(t *testing.T) {
	terminals := []struct {
		name  string
		event any
		want  string
	}{
		{"logged out", &events.LoggedOut{}, ReasonLoggedOut},
		{"stream replaced", &events.StreamReplaced{}, ReasonStreamReplaced},
		{"client outdated", &events.ClientOutdated{}, ReasonClientOutdated},
		{"temporary ban", &events.TemporaryBan{Expire: time.Hour}, ReasonTemporaryBan},
	}

	for _, tt := range terminals {
		t.Run(tt.name, func(t *testing.T) {
			syncStore := newFakeSyncStore()
			m := newManagerForTest(t, syncStore, &fakeBackfillReader{})
			// Short enough that a bounded-drop policy would FIRE while the loop
			// is still parked. With the lossless policy the enqueue simply waits
			// it out — which is the whole difference this test has to see.
			tuneTimeouts(m, func(tm *managerTimeouts) { tm.ctrlEnqueue = 50 * time.Millisecond })
			cli := newFakeClient()
			m.SetIngestor(&fakeIngestor{})
			m.SetHistoryRecorder(&fakeRecorder{})
			m.SetHistoryDrainReady()
			useClient(m, cli, true)

			require.NoError(t, m.Start(context.Background()))
			require.True(t, dispatchEvent(t, m, nil, &events.Connected{}))
			sess := installedSession(t, m)

			release := parkLoop(t, m)
			fillMailbox(m)

			delivered := make(chan bool, 1)
			go func() { delivered <- m.handleEventFor(sess, tt.event) }()

			// The handler is blocked on the full mailbox, which is exactly the
			// point: it must not give up. The wait is deliberately several times
			// the enqueue deadline — a policy that dropped this event would have
			// returned by now.
			select {
			case <-delivered:
				t.Fatal("the event was dropped rather than waiting for room: a terminal event may be the last thing that client ever emits")
			case <-time.After(10 * tm50):
			}

			release()
			assert.True(t, <-delivered)
			eventually(t, "the terminal reason is durably recorded", func() bool {
				return syncStore.terminalReason() == tt.want
			})
			eventually(t, "the session is retired", func() bool {
				return m.Status().State == StateDisconnected
			})
		})
	}
}

// tm50 is the short enqueue deadline the losslessness tests tune to.
const tm50 = 50 * time.Millisecond

// TestFullMailbox_PairSuccessIsEventuallyAdopted: dropping it would lose the
// adoption of a device the user just linked.
func TestFullMailbox_PairSuccessIsEventuallyAdopted(t *testing.T) {
	cli := newFakeClient()
	m, syncStore, _, _ := newTestManager(t, cli, false)
	require.NoError(t, m.Start(context.Background()))
	require.NoError(t, m.StartPairing(context.Background(), PairRequest{Method: PairMethodQR}))
	sess := pairingSession(t, m)
	require.NotNil(t, sess)

	jid := types.NewJID("15551234567", types.DefaultUserServer)
	release := parkLoop(t, m)
	fillMailbox(m)

	delivered := make(chan bool, 1)
	go func() { delivered <- m.handleEventFor(sess, &events.PairSuccess{ID: jid}) }()

	select {
	case <-delivered:
		t.Fatal("PairSuccess was accepted while the mailbox was full and the loop parked")
	case <-time.After(50 * time.Millisecond):
	}

	release()
	assert.True(t, <-delivered)
	eventually(t, "the device is adopted and the selector written", func() bool {
		return syncStore.linkedJID() == jid.String()
	})
}

// TestFullMailbox_PairErrorIsEventuallyHandled: PairError is the only signal
// that a pairing-written device row survived a FAILED pairing, so its
// losslessness is not optional.
func TestFullMailbox_PairErrorIsEventuallyHandled(t *testing.T) {
	cli := newFakeClient()
	m, _, _, _, devices := newTestManagerWithDevices(t, cli, false)
	require.NoError(t, m.Start(context.Background()))
	require.NoError(t, m.StartPairing(context.Background(), PairRequest{Method: PairMethodQR}))
	sess := pairingSession(t, m)
	require.NotNil(t, sess)

	lateJID := types.NewJID("15559990000", types.DefaultUserServer)
	devices.add(lateJID)

	release := parkLoop(t, m)
	fillMailbox(m)

	delivered := make(chan bool, 1)
	go func() {
		delivered <- m.handleEventFor(sess, &events.PairError{ID: lateJID, Error: errors.New("pairing failed")})
	}()

	select {
	case <-delivered:
		t.Fatal("PairError was accepted while the mailbox was full and the loop parked")
	case <-time.After(50 * time.Millisecond):
	}

	release()
	assert.True(t, <-delivered)
	eventually(t, "the attempt is abandoned", func() bool { return m.Status().Pairing == nil })
	eventually(t, "the row the library saved for the failed attempt is deleted by JID", func() bool {
		return len(devices.remaining()) == 0
	})
}

// TestFullMailbox_ConnectedIsDroppedWithAnError is the ONE sanctioned drop.
//
// Connected is the only control event dispatched synchronously from a node
// handler, and the only one whose loss is recoverable: it carries no durable
// decision and whatsmeow re-emits it on every reconnect. Blocking a node handler
// past the library's ordering horizon would cost message ordering for the whole
// session.
func TestFullMailbox_ConnectedIsDroppedWithAnError(t *testing.T) {
	m := newDeadlineManager(t, effectDeadline)
	tuneTimeouts(m, func(tm *managerTimeouts) { tm.ctrlEnqueue = 50 * time.Millisecond })
	cli := newFakeClient()
	useClient(m, cli, true)
	require.NoError(t, m.Start(context.Background()))
	sess := installedSession(t, m)

	release := parkLoop(t, m)
	fillMailbox(m)

	started := time.Now() //nolint:forbidigo // Wall-clock: the enqueue bound is real elapsed time.
	assert.True(t, m.handleEventFor(sess, &events.Connected{}),
		"the handler always reports success: there is no stanza to withhold")
	assert.Less(t, time.Since(started), 2*time.Second,
		"the drop happens at the deadline rather than blocking the node handler")

	release()
	m.settle()
	assert.NotEqual(t, StateConnected, m.Status().State,
		"the dropped event left the status where it was — bounded and self-correcting")
}

// TestHandleEvent_PairErrorIsNotADefaultBranchEvent is the direct guard on the
// finding: with PairError on the default arm the targeted cleanup it gates is
// never scheduled.
func TestHandleEvent_PairErrorIsNotADefaultBranchEvent(t *testing.T) {
	cli := newFakeClient()
	m, _, _, _ := newTestManager(t, cli, false)
	require.NoError(t, m.StartPairing(context.Background(), PairRequest{Method: PairMethodQR}))
	sess := pairingSession(t, m)
	require.NotNil(t, sess)

	assert.True(t, dispatchEvent(t, m, sess, &events.PairError{
		ID:    types.NewJID("15551234567", types.DefaultUserServer),
		Error: errors.New("pairing failed"),
	}))

	assert.Nil(t, m.Status().Pairing,
		"the event reached the pairing machinery rather than the debug-log default")
	assert.Equal(t, StateNotPaired, m.Status().State)
}

// --- an abandoned pairing's late-saved device -------------------------------

// TestAbandonedPairing_LateSaveIsDeletedOnTheStalePairSuccess covers the
// ordinary case mutual exclusion does not: an attempt abandoned BEFORE its
// library-side Store.Save completed. The teardown delete has already no-opped
// (the library refuses to delete a device with no JID), so the backstop is the
// event that PROVES the row exists.
func TestAbandonedPairing_LateSaveIsDeletedOnTheStalePairSuccess(t *testing.T) {
	lateJID := types.NewJID("15559990000", types.DefaultUserServer)

	tests := []struct {
		name    string
		abandon func(t *testing.T, m *Manager, sess *session)
	}{
		{
			name:    "cancel",
			abandon: func(_ *testing.T, m *Manager, _ *session) { m.CancelPairing() },
		},
		{
			name: "ttl expiry taken over",
			abandon: func(t *testing.T, m *Manager, _ *session) {
				expirePairing(m)
				require.NoError(t, m.StartPairing(context.Background(), PairRequest{Method: PairMethodQR}))
			},
		},
		{
			name: "terminal event on its own client",
			abandon: func(t *testing.T, m *Manager, sess *session) {
				require.True(t, dispatchEvent(t, m, sess, &events.LoggedOut{}))
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cli := newFakeClient()
			m, _, _, _, devices := newTestManagerWithDevices(t, cli, false)
			require.NoError(t, m.Start(context.Background()))
			require.NoError(t, m.StartPairing(context.Background(), PairRequest{Method: PairMethodQR}))
			sess := pairingSession(t, m)
			require.NotNil(t, sess)

			tt.abandon(t, m, sess)

			// The library's own goroutine completes Store.Save after our
			// teardown has already run.
			devices.add(lateJID)

			require.True(t, dispatchEvent(t, m, sess, &events.PairSuccess{ID: lateJID}))

			eventually(t, "the late-saved device is deleted by JID", func() bool {
				return len(devices.remaining()) == 0
			})
		})
	}
}

// TestAbandonedPairing_LateSaveIsDeletedOnAStalePairError is the other half: the
// path taken when the library's own post-save cleanup failed.
func TestAbandonedPairing_LateSaveIsDeletedOnAStalePairError(t *testing.T) {
	lateJID := types.NewJID("15559990000", types.DefaultUserServer)

	cli := newFakeClient()
	m, _, _, _, devices := newTestManagerWithDevices(t, cli, false)
	require.NoError(t, m.Start(context.Background()))
	require.NoError(t, m.StartPairing(context.Background(), PairRequest{Method: PairMethodQR}))
	sess := pairingSession(t, m)
	require.NotNil(t, sess)

	m.CancelPairing()
	devices.add(lateJID)

	require.True(t, dispatchEvent(t, m, sess, &events.PairError{ID: lateJID, Error: errors.New("boom")}))

	eventually(t, "the late-saved device is deleted by JID", func() bool {
		return len(devices.remaining()) == 0
	})
}

// TestStalePairEvent_NeverDeletesTheLinkedDevice: a targeted delete is a loaded
// weapon. Without these guards the fix would be more dangerous than the bug.
func TestStalePairEvent_NeverDeletesTheLinkedDevice(t *testing.T) {
	t.Run("jid equals the recorded linked device", func(t *testing.T) {
		cli := newFakeClient()
		m, _, _, _, devices := newTestManagerWithDevices(t, cli, false)
		require.NoError(t, m.Start(context.Background()))
		require.NoError(t, m.StartPairing(context.Background(), PairRequest{Method: PairMethodQR}))
		linked := types.NewJID("15551110000", types.DefaultUserServer)
		sess := pairingSession(t, m)
		require.True(t, dispatchEvent(t, m, sess, &events.PairSuccess{ID: linked}))
		devices.add(linked)

		orphan := &session{client: newFakeClient(), retired: true}
		require.True(t, dispatchEvent(t, m, orphan, &events.PairSuccess{ID: linked}))
		m.settle()

		assert.Equal(t, []types.JID{linked}, devices.remaining(),
			"a stale event naming the LIVE device is a re-pair report, not an orphan")
	})

	t.Run("jid equals the installed session's device", func(t *testing.T) {
		cli := newFakeClient()
		m, _, _, _, devices := newTestManagerWithDevices(t, cli, true)
		require.NoError(t, m.Start(context.Background()))
		require.True(t, dispatchEvent(t, m, nil, &events.Connected{}))

		orphan := &session{client: newFakeClient(), retired: true}
		require.True(t, dispatchEvent(t, m, orphan, &events.PairError{ID: testDeviceJID, Error: errors.New("boom")}))
		m.settle()

		assert.Equal(t, []types.JID{testDeviceJID}, devices.remaining(),
			"deleting the installed session's device would unlink a working account")
	})
}

// --- shutdown ----------------------------------------------------------------

// TestStop_TearsDownWhileTheLoopIsWedged: the teardown handle is PUBLISHED, not
// handed over, so a wedged loop cannot deprive Stop of it.
func TestStop_TearsDownWhileTheLoopIsWedged(t *testing.T) {
	m := newDeadlineManager(t, effectDeadline)
	tuneTimeouts(m, func(tm *managerTimeouts) { tm.stopLoop = 100 * time.Millisecond })
	installed := newFakeClient()
	useClient(m, installed, true)
	require.NoError(t, m.Start(context.Background()))
	installed.setConnected(false)

	pairingClient := newFakeClient()
	useClient(m, pairingClient, false)
	require.NoError(t, m.StartPairing(context.Background(), PairRequest{Method: PairMethodQR}))
	attempt := m.inspect().Pairing
	require.NotNil(t, attempt)

	release := parkLoop(t, m)
	t.Cleanup(release)

	stopped := make(chan struct{})
	go func() { m.Stop(); close(stopped) }()

	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop did not return while the loop was wedged")
	}

	select {
	case <-attempt.ctx.Done():
	default:
		t.Fatal("the pairing context was not cancelled")
	}
	assert.Contains(t, installed.callLog(), "disconnect")
	assert.Contains(t, pairingClient.callLog(), "disconnect")
}

// TestStop_DispatchesNothingAfterStopping is the linearization rule, tested
// rather than asserted: select picking randomly between a closed `stopping` and
// a ready inbox is fixed at the CONSUMER, once.
func TestStop_DispatchesNothingAfterStopping(t *testing.T) {
	m := newDeadlineManager(t, effectDeadline)
	tuneTimeouts(m, func(tm *managerTimeouts) { tm.stopLoop = 100 * time.Millisecond })
	useClient(m, newFakeClient(), true)

	release := parkLoop(t, m)
	t.Cleanup(release)

	ran := 0
	var mu sync.Mutex
	for i := 0; i < 5; i++ {
		msg := &opMsg{
			run: func(_ *actorState, reply chan opResult) {
				mu.Lock()
				ran++
				mu.Unlock()
				reply <- opResult{}
			},
			reply: make(chan opResult, 1),
		}
		select {
		case m.inbox <- msg:
		default:
			t.Fatal("the mailbox should have room for the queued operations")
		}
	}

	stopped := make(chan struct{})
	go func() { m.Stop(); close(stopped) }()
	<-stopped

	release()
	time.Sleep(100 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	assert.Zero(t, ran, "the mailbox is abandoned, not drained")
}

// TestStop_ReturnsWhileTheDatabaseIsWedged: a shutdown must not depend on the
// loop's cooperation.
func TestStop_ReturnsWhileTheDatabaseIsWedged(t *testing.T) {
	syncStore := newFakeSyncStore()
	m := NewManager(nil, NewWALogger("whatsapp-test"), nil, syncStore, &fakeBackfillReader{})
	tuneTimeouts(m, func(tm *managerTimeouts) { tm.stopLoop = 100 * time.Millisecond })
	registerManagerCleanup(t, m)
	m.SetIngestor(&fakeIngestor{})
	m.SetHistoryRecorder(&fakeRecorder{})
	m.SetHistoryDrainReady()
	cli := newFakeClient()
	useClient(m, cli, true)
	require.NoError(t, m.Start(context.Background()))
	require.True(t, dispatchEvent(t, m, nil, &events.Connected{}))

	wedged := make(chan struct{})
	t.Cleanup(func() { close(wedged) })
	syncStore.setErr(func(f *fakeSyncStore) { f.terminalBlock = wedged })

	require.True(t, m.handleEventFor(nil, &events.LoggedOut{}))
	time.Sleep(20 * time.Millisecond) // let the turn reach the wedged write

	stopped := make(chan struct{})
	go func() { m.Stop(); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop did not return while the database was wedged")
	}

	// And a control event dispatched during the wedge still returns promptly.
	done := make(chan bool, 1)
	go func() { done <- m.handleEventFor(nil, &events.Connected{}) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("a control-event handler blocked after Stop")
	}
}

// TestStop_WaitsForEveryEffectGoroutine is the clean-exit tier: Stop returning
// means no effect goroutine is still running, and the wait is bounded by a
// cancellation rather than by luck.
func TestStop_WaitsForEveryEffectGoroutine(t *testing.T) {
	cli := newFakeClient()
	m, _, _, _ := newTestManager(t, cli, true)
	require.NoError(t, m.Start(context.Background()))
	require.True(t, dispatchEvent(t, m, nil, &events.Connected{}))

	entered := make(chan struct{})
	never := make(chan struct{})
	t.Cleanup(func() { close(never) })
	cli.mu.Lock()
	cli.logoutEntered = entered
	cli.logoutBlock = never // released only by the effect context's cancellation
	cli.mu.Unlock()

	unlinkDone := make(chan struct{})
	go func() { defer close(unlinkDone); _, _ = m.Disconnect(context.Background(), false) }()
	<-entered

	m.Stop()

	// Stop returned on the clean tier, which means effects.Wait() ran — so the
	// logout goroutine has already observed its cancellation and finished.
	select {
	case <-unlinkDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop returned while an effect goroutine was still running")
	}
}

// TestStop_FinalTurnMayStillLaunchEffects: the turn already executing when
// `stopping` closes still runs to completion and may still launch. The gate is
// closed by the LOOP, in shutdownState, which is what lets Stop wait without
// racing an Add it cannot exclude.
func TestStop_FinalTurnMayStillLaunchEffects(t *testing.T) {
	cli := newFakeClient()
	m, _, _, _ := newTestManager(t, cli, true)
	require.NoError(t, m.Start(context.Background()))

	entered := make(chan struct{})
	gate := make(chan struct{})
	effectRan := make(chan struct{})

	go func() {
		m.runOp(func(st *actorState, reply chan opResult) {
			close(entered)
			<-gate // Stop closes `stopping` while this turn is executing
			m.launch(st, []effect{disconnectEffect{sess: st.sess}}, launchDetached, fence{}, opFlags{}, nil, nil)
			close(effectRan)
			reply <- opResult{}
		})
	}()
	<-entered

	stopped := make(chan struct{})
	go func() { m.Stop(); close(stopped) }()

	eventually(t, "Stop closed stopping first", func() bool {
		select {
		case <-m.stopping:
			return true
		default:
			return false
		}
	})
	close(gate)
	<-effectRan

	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop never completed after the final turn launched an effect")
	}
}

// TestLaunch_IsClosedAfterShutdownState is the other half of the gate: a turn
// that tried to launch after shutdownState gets ErrManagerStopped rather than
// spawning a goroutine nothing is waiting for.
func TestLaunch_IsClosedAfterShutdownState(t *testing.T) {
	m, _, _, _ := newTestManager(t, newFakeClient(), true)

	aborted := make(chan error, 1)
	st := &actorState{launchClosed: true}
	ok := m.launch(st, []effect{disconnectEffect{}}, launchDetached, fence{}, opFlags{},
		func(*actorState, effectResult) bool { return false },
		func(err error) { aborted <- err })

	assert.False(t, ok)
	select {
	case err := <-aborted:
		assert.ErrorIs(t, err, ErrManagerStopped)
	default:
		t.Fatal("a refused launch must still answer its caller")
	}
}

// TestStop_TearsDownASessionInstalledByTheFinalTurn: the handle Stop tears down
// from is the one shutdownState published, not a stale one.
func TestStop_TearsDownASessionInstalledByTheFinalTurn(t *testing.T) {
	m, _, _, _ := newTestManager(t, newFakeClient(), true)

	late := newFakeClient()
	lateSess := fakeSessionFor(late, true, nil)

	entered := make(chan struct{})
	gate := make(chan struct{})
	go func() {
		m.runOp(func(st *actorState, reply chan opResult) {
			close(entered)
			<-gate
			st.sess = lateSess
			reply <- opResult{}
		})
	}()
	<-entered

	stopped := make(chan struct{})
	go func() { m.Stop(); close(stopped) }()
	eventually(t, "Stop closed stopping first", func() bool {
		select {
		case <-m.stopping:
			return true
		default:
			return false
		}
	})
	close(gate)

	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop never returned")
	}
	assert.Contains(t, late.callLog(), "disconnect",
		"a session installed by the final turn is in the handle Stop reads")
}

// TestStop_ReleasesAContinuationBlockedOnAFullMailbox is the round-3 shutdown
// deadlock: with continuations selecting on `done` — which closes only after
// effects.Wait() — an abandoned full mailbox parks the very goroutine Wait is
// waiting for.
func TestStop_ReleasesAContinuationBlockedOnAFullMailbox(t *testing.T) {
	cli := newFakeClient()
	m, _, _, _ := newTestManager(t, cli, true)
	require.NoError(t, m.Start(context.Background()))
	require.True(t, dispatchEvent(t, m, nil, &events.Connected{}))

	entered := make(chan struct{})
	release := make(chan struct{})
	cli.mu.Lock()
	cli.logoutEntered = entered
	cli.logoutBlock = release
	cli.mu.Unlock()

	unlinkDone := make(chan struct{})
	go func() { defer close(unlinkDone); _, _ = m.Disconnect(context.Background(), false) }()
	<-entered

	// Park the loop BEFORE filling, so the mailbox is still full when the
	// continuation submits: a running loop would simply drain it and the
	// continuation would never block at all.
	parkRelease := parkLoop(t, m)
	fillMailbox(m)

	stopped := make(chan struct{})
	go func() { m.Stop(); close(stopped) }()
	eventually(t, "Stop closed stopping", func() bool {
		select {
		case <-m.stopping:
			return true
		default:
			return false
		}
	})

	// Releasing the park lets the loop finish its turn and then exit on the
	// top-of-loop check WITHOUT draining — so Stop takes the CLEAN tier and
	// really does call effects.Wait(), which is what the deadlock needs.
	parkRelease()
	select {
	case <-m.loopExited:
	case <-time.After(5 * time.Second):
		t.Fatal("the loop never exited")
	}
	close(release)

	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop deadlocked against a continuation blocked on the abandoned mailbox")
	}
	<-unlinkDone
}

// TestStop_WedgedLoopDoesNotStartAQueuedEffect is the load-bearing half of the
// tier-2 guarantee: every effect is cancelled, and NO effect that had not
// already begun will begin. The queued step here is a client call, so if it ran
// the assertion would see it.
func TestStop_WedgedLoopDoesNotStartAQueuedEffect(t *testing.T) {
	m := newDeadlineManager(t, effectDeadline)
	tuneTimeouts(m, func(tm *managerTimeouts) { tm.stopLoop = 100 * time.Millisecond })

	cli := newFakeClient()
	entered := make(chan struct{})
	release := make(chan struct{})
	cli.connectEntered = entered
	cli.connectBlock = release
	useClient(m, cli, true)

	// No live client, so the unlink builds one: the batch is
	// [connectEffect, logoutEffect] — the first parked, the second not started.
	unlinkDone := make(chan struct{})
	go func() { defer close(unlinkDone); _, _ = m.Disconnect(context.Background(), false) }()
	<-entered

	parkRelease := parkLoop(t, m)
	t.Cleanup(parkRelease)

	stopped := make(chan struct{})
	go func() { m.Stop(); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop did not return on the wedged tier")
	}

	close(release)
	time.Sleep(100 * time.Millisecond)

	assert.NotContains(t, cli.callLog(), "logout",
		"no client call may begin after the effect context was cancelled")
}

// TestStartPairing_QRTimeoutWhileTheLoopIsBusy: the caller-owned first-code
// timer lives in the caller, so it fires whatever the loop is doing, and the
// attempt it abandons is named by IDENTITY.
func TestStartPairing_QRTimeoutWhileTheLoopIsBusy(t *testing.T) {
	cli := newFakeClient()
	cli.qrSilent = true
	m, _, _, _ := newTestManager(t, cli, false)

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- m.StartPairing(ctx, PairRequest{Method: PairMethodQR}) }()

	eventually(t, "the attempt is in the slot", func() bool { return m.Status().Pairing != nil })
	attempt := m.inspect().Pairing
	require.NotNil(t, attempt)

	release := parkLoop(t, m)
	cancel()

	// The caller's wait has resolved; its abandon is queued behind the busy loop.
	select {
	case <-errCh:
		t.Fatal("the abandon must be ordered against the loop, not applied off it")
	case <-time.After(50 * time.Millisecond):
	}

	release()
	assert.ErrorIs(t, <-errCh, ErrQRCodeTimeout)
	assert.Nil(t, m.Status().Pairing, "the attempt it named by identity is gone")
}

// TestSetHistoryDrainReady_DoesNotConnectByItself pins the design we rejected:
// a readiness transition must not be a second entrance to Start's gate.
func TestSetHistoryDrainReady_DoesNotConnectByItself(t *testing.T) {
	cli := newFakeClient()
	m := newManagerForTest(t, newFakeSyncStore(), &fakeBackfillReader{})
	m.setSessionFactory(func(context.Context, sessionRequest) (*session, error) {
		jid := testDeviceJID
		return fakeSessionFor(cli, true, &jid), nil
	})
	m.SetIngestor(&fakeIngestor{})
	m.SetHistoryRecorder(&fakeRecorder{})

	m.SetHistoryDrainReady()
	m.settle()

	ready, _ := m.Ready()
	assert.True(t, ready, "the readiness set is complete")
	assert.Empty(t, cli.callLog(), "and nothing connected: Start is the sole activation point")
	assert.Equal(t, StateNotReady, m.Status().State)
}
