package whatsapp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"personal-crm/backend/internal/accelerated"

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
	// them reads as passing while the invariant is gone — and a census that
	// cannot see a goroutine is not a census. time.AfterFunc is the one that
	// bit: its callback runs on a goroutine no `go` statement mentions and no
	// WaitGroup counts. The dial watchdog is a launched effect for exactly that
	// reason.
	for _, banned := range []string{"errgroup", ".Go(", "sync.WaitGroup.Go", "time.AfterFunc"} {
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

	started := accelerated.GetCurrentTime()
	require.True(t, m.handleEventFor(nil, &events.LoggedOut{}))

	done := make(chan struct{})
	go func() { m.settle(); close(done) }()

	select {
	case <-done:
	case <-time.After(maxTurnBudget + 2*time.Second):
		t.Fatalf("the worst turn exceeded its budget of %s", maxTurnBudget)
	}
	assert.Less(t, accelerated.GetCurrentTime().Sub(started), maxTurnBudget+2*time.Second)
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

	started := accelerated.GetCurrentTime()
	wedged := m.Status().Backfill
	assert.Less(t, accelerated.GetCurrentTime().Sub(started), backfillReadTimeout+2*time.Second,
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

	started := accelerated.GetCurrentTime()
	assert.True(t, m.handleEventFor(sess, &events.Connected{}),
		"the handler always reports success: there is no stanza to withhold")
	assert.Less(t, accelerated.GetCurrentTime().Sub(started), 2*time.Second,
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
		// deferred marks the case where the abandonment leaves ANOTHER attempt
		// in the slot, so the targeted delete has to wait for that attempt to
		// end before its guards can mean anything.
		deferred bool
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
			deferred: true,
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

			if tt.deferred {
				m.settle()
				require.Equal(t, []types.JID{lateJID}, devices.remaining(),
					"while another attempt is in flight the delete waits: its JID is not yet knowable")
				// Ending that attempt is what lets the deferred cleanup run.
				m.CancelPairing()
			}

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

		consistently(t, "a stale event naming the LIVE device is a re-pair report, not an orphan",
			300*time.Millisecond, func() bool {
				return len(devices.deletedJIDs()) == 0 && len(devices.remaining()) == 1
			})
	})

	t.Run("jid equals the installed session's device", func(t *testing.T) {
		cli := newFakeClient()
		m, _, _, _, devices := newTestManagerWithDevices(t, cli, true)
		require.NoError(t, m.Start(context.Background()))
		require.True(t, dispatchEvent(t, m, nil, &events.Connected{}))

		orphan := &session{client: newFakeClient(), retired: true}
		require.True(t, dispatchEvent(t, m, orphan, &events.PairError{ID: testDeviceJID, Error: errors.New("boom")}))
		m.settle()

		consistently(t, "deleting the installed session's device would unlink a working account",
			300*time.Millisecond, func() bool {
				return len(devices.deletedJIDs()) == 0 && len(devices.remaining()) == 1
			})
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
			m.launch(st, []effect{releaseSessionEffect{release: sessionRelease{sess: st.sess}}}, launchDetached, fence{}, opFlags{}, nil, nil)
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
	ok := m.launch(st, []effect{releaseSessionEffect{}}, launchDetached, fence{}, opFlags{},
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

// --- the dial is bounded by our context, not by the library's ----------------

// TestConnect_HonoursTheEffectDeadline is the P0 guard.
//
// whatsmeow implements Connect() as ConnectContext(cli.BackgroundEventCtx), so a
// dial made through it is bounded by the LIBRARY's lifetime and by nothing of
// ours. Against a black-holed socket that means Start never returns and the
// effect deadline is decorative. The fake blocks until its context is cancelled
// and by no other means, so a regression to the context-free call hangs here
// rather than passing quietly.
func TestConnect_HonoursTheEffectDeadline(t *testing.T) {
	cli := newFakeClient()
	cli.blackHoleDial = true

	m := newDeadlineManager(t, 100*time.Millisecond)
	useClient(m, cli, true)

	done := make(chan struct{})
	go func() { defer close(done); _ = m.Start(context.Background()) }()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Start never returned: the dial is not bounded by the effect deadline")
	}

	assert.Contains(t, cli.callLog(), "connect_cancelled",
		"the dial must observe OUR cancellation, not the library's background context")
	assert.Equal(t, StateError, m.Status().State)
}

// TestConnect_ConnectionOutlivesTheBatchThatDialedIt is the other half of the
// same contract, and the half a blocked-dial fake cannot see.
//
// whatsmeow retains the context handed to ConnectContext as the socket's parent
// and gives it to auto-reconnect. Dialling under the batch context would
// therefore succeed and then close the connection the instant execEffects
// returned — a live-looking manager with a dead socket and no reconnect. The
// fake retains the context and runs a read pump under it, so that regression is
// observable rather than argued.
func TestConnect_ConnectionOutlivesTheBatchThatDialedIt(t *testing.T) {
	cli := newFakeClient()
	m, _, _, _ := newTestManager(t, cli, true)

	require.NoError(t, m.Start(context.Background()))
	require.Equal(t, StateConnecting, m.Status().State)

	// Start returned, so the batch that dialled has completed and its context
	// has been cancelled. The connection must not care.
	consistently(t, "the connection outlives the batch that dialled it", 300*time.Millisecond, func() bool {
		return cli.connCtxErr() == nil && cli.pumpRunning()
	})
}

// TestStop_CancelsTheConnectionContext: the connection context descends from
// the manager's effect context, so shutdown reaches every live socket — which
// is what stops auto-reconnect from outliving the process's intent.
func TestStop_CancelsTheConnectionContext(t *testing.T) {
	cli := newFakeClient()
	m, _, _, _ := newTestManager(t, cli, true)
	require.NoError(t, m.Start(context.Background()))
	require.True(t, cli.pumpRunning())

	m.Stop()

	eventually(t, "the connection context dies with the manager", func() bool {
		return cli.connCtxErr() != nil && !cli.pumpRunning()
	})
}

// TestDisconnect_TearingDownASessionEndsItsConnectionContext: a session the
// manager has finished with must not leave auto-reconnect running under a
// context nobody cancels.
func TestDisconnect_TearingDownASessionEndsItsConnectionContext(t *testing.T) {
	cli := newFakeClient()
	m, _, _, _ := newTestManager(t, cli, true)
	require.NoError(t, m.Start(context.Background()))
	require.True(t, cli.pumpRunning())

	require.True(t, dispatchEvent(t, m, nil, &events.Connected{}))
	_, err := m.Disconnect(context.Background(), false)
	require.NoError(t, err)

	eventually(t, "tearing the session down ends its connection context", func() bool {
		return cli.connCtxErr() != nil && !cli.pumpRunning()
	})
}

// TestStop_UnblocksABlackHoledDial is the shutdown half of the same finding: a
// dial nothing can cancel would keep Stop waiting on the clean tier, and would
// then block Disconnect() behind the client's own socket lock.
func TestStop_UnblocksABlackHoledDial(t *testing.T) {
	cli := newFakeClient()
	cli.blackHoleDial = true
	entered := make(chan struct{})
	cli.connectEntered = entered

	m := newDeadlineManager(t, time.Minute) // far longer than the test may take
	useClient(m, cli, true)

	started := make(chan struct{})
	go func() { defer close(started); _ = m.Start(context.Background()) }()
	<-entered

	stopped := make(chan struct{})
	go func() { m.Stop(); close(stopped) }()

	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop did not return: the in-flight dial ignored the effect context")
	}
	<-started
	assert.Contains(t, cli.callLog(), "connect_cancelled")
}

// TestStalePairEvent_NeverDeletesWhileAPairingIsInFlight is the third guard.
//
// A live attempt's device JID is unknowable to us until its own PairSuccess
// arrives — the library saves the row from its own goroutine and only then
// announces it. So a delayed event from a CANCELLED attempt naming the same
// account must not be allowed to delete the row a NEW attempt just created:
// neither the linked-account guard nor the installed-session guard sees it,
// because the new attempt has adopted nothing yet.
func TestStalePairEvent_NeverDeletesWhileAPairingIsInFlight(t *testing.T) {
	for _, name := range []string{"PairSuccess", "PairError"} {
		t.Run(name, func(t *testing.T) {
			jid := types.NewJID("15551234567", types.DefaultUserServer)

			cli := newFakeClient()
			m, _, _, _, devices := newTestManagerWithDevices(t, cli, false)
			require.NoError(t, m.Start(context.Background()))

			// An attempt is started and abandoned; its pair event is delayed.
			require.NoError(t, m.StartPairing(context.Background(), PairRequest{Method: PairMethodQR}))
			abandoned := pairingSession(t, m)
			require.NotNil(t, abandoned)
			m.CancelPairing()

			// A NEW attempt is started for the same account, and the library
			// saves its row before announcing it.
			require.NoError(t, m.StartPairing(context.Background(), PairRequest{Method: PairMethodQR}))
			require.NotNil(t, m.Status().Pairing)
			devices.add(jid)

			var evt any = &events.PairSuccess{ID: jid}
			if name == "PairError" {
				evt = &events.PairError{ID: jid, Error: errors.New("boom")}
			}
			require.True(t, dispatchEvent(t, m, abandoned, evt))
			m.settle()

			consistently(t, "the row belongs to the attempt in flight; a delayed event from a dead one may not remove it",
				300*time.Millisecond, func() bool {
					return len(devices.deletedJIDs()) == 0 && len(devices.remaining()) == 1
				})
			assert.NotNil(t, m.Status().Pairing, "and the live attempt is untouched")

			// The deferral is a delay, not a lost cleanup: ending the live
			// attempt frees the slot and the guarded delete finally runs.
			m.CancelPairing()
			eventually(t, "the deferred cleanup runs once the slot is free", func() bool {
				return len(devices.remaining()) == 0
			})
		})
	}
}

// TestDisconnect_AbortedUnlinkDoesNotOrphanTheClientItBuilt closes the
// build-to-unlink half of the orphan rule.
//
// whatsmeow deliberately does NOT disconnect on a failed logout, so an unlink
// that built and connected a client and then aborted on the fence would leave a
// live socket owned by nobody: the manager never installed it, and the aborting
// operation is structurally forbidden from touching state. The effects that
// leave a client live therefore report their session, and the loop hands it to
// teardown.
func TestDisconnect_AbortedUnlinkDoesNotOrphanTheClientItBuilt(t *testing.T) {
	installed := newFakeClient()
	m, _, _, _ := newTestManager(t, installed, true)
	require.NoError(t, m.Start(context.Background()))
	// The installed socket is down, so the unlink builds a client of its own —
	// which is the path that can strand one.
	installed.setConnected(false)

	built := newFakeClient()
	built.logoutErr = logoutRemoteFailure()
	entered := make(chan struct{})
	release := make(chan struct{})
	built.logoutEntered = entered
	built.logoutBlock = release
	m.setSessionFactory(func(context.Context, sessionRequest) (*session, error) {
		jid := testDeviceJID
		return fakeSessionFor(built, true, &jid), nil
	})

	unlinkErr := make(chan error, 1)
	go func() {
		_, err := m.Disconnect(context.Background(), false)
		unlinkErr <- err
	}()
	<-entered
	require.True(t, built.IsConnected(), "the unlink client really is connected at this point")

	// A Start installs a session while the unlink is parked in its remote call.
	replacement := newFakeClient()
	m.setSessionFactory(func(context.Context, sessionRequest) (*session, error) {
		jid := testDeviceJID
		return fakeSessionFor(replacement, true, &jid), nil
	})
	require.NoError(t, m.Start(context.Background()))

	close(release)
	assert.ErrorIs(t, <-unlinkErr, ErrOperationSuperseded)

	eventually(t, "the client the aborted unlink built is disconnected, not orphaned", func() bool {
		return !built.IsConnected()
	})
	assert.NotSame(t, built, installedSession(t, m).client, "it was never installed")
}

// TestStaleDelete_RunsInsideItsTurn is the structural proof that replaced the
// re-guard.
//
// A targeted delete launched as an effect can only be guarded BEFORE it runs, so
// a pairing claiming the slot in between could have its freshly saved row
// destroyed — and no fence placed after a delete can un-delete a row. Running
// the delete in the turn makes the guards and the destruction one indivisible
// step. Parking the database call therefore parks the whole loop, which is
// exactly what an asynchronous implementation would not do.
func TestStaleDelete_RunsInsideItsTurn(t *testing.T) {
	jid := types.NewJID("15559990000", types.DefaultUserServer)

	cli := newFakeClient()
	m, _, _, _, devices := newTestManagerWithDevices(t, cli, false)
	require.NoError(t, m.Start(context.Background()))

	// An abandoned attempt whose row is saved late, with another attempt in the
	// slot, so its cleanup is deferred.
	require.NoError(t, m.StartPairing(context.Background(), PairRequest{Method: PairMethodQR}))
	abandoned := pairingSession(t, m)
	m.CancelPairing()
	require.NoError(t, m.StartPairing(context.Background(), PairRequest{Method: PairMethodQR}))
	devices.add(jid)
	require.True(t, dispatchEvent(t, m, abandoned, &events.PairSuccess{ID: jid}))

	entered := make(chan struct{})
	release := make(chan struct{})
	devices.setErr(func(d *fakeDevices) { d.delEntered, d.delBlock = entered, release })

	// Freeing the slot flushes the deferral.
	go m.CancelPairing()
	<-entered

	settled := make(chan struct{})
	go func() { m.settle(); close(settled) }()
	select {
	case <-settled:
		t.Fatal("the loop accepted another message while the delete was running: the delete is not in-turn, so a new attempt could save the very row it is about to destroy")
	case <-time.After(150 * time.Millisecond):
	}

	close(release)
	select {
	case <-settled:
	case <-time.After(3 * time.Second):
		t.Fatal("the loop never resumed")
	}
	assert.Empty(t, devices.remaining(), "and the deferred cleanup did run")
}

// TestStop_FlushesDeferredCleanupRatherThanAbandoningIt is the shutdown half.
//
// A deferral lives only in actor memory, so a shutdown that cancelled the
// pairing without flushing would drop it. On an installation that was never
// paired that is the resurrection case with nothing to mask it: the abandoned
// attempt's row is then the ONLY row and carries no selector, so the resolver's
// single-row heal would adopt it on the next boot as if the user had chosen it.
func TestStop_FlushesDeferredCleanupRatherThanAbandoningIt(t *testing.T) {
	jid := types.NewJID("15559990000", types.DefaultUserServer)

	cli := newFakeClient()
	m, syncStore, _, _, devices := newTestManagerWithDevices(t, cli, false)
	require.NoError(t, m.Start(context.Background()))
	require.Equal(t, StateNotPaired, m.Status().State, "a store that has never been paired")

	require.NoError(t, m.StartPairing(context.Background(), PairRequest{Method: PairMethodQR}))
	abandoned := pairingSession(t, m)
	m.CancelPairing()

	// A second attempt occupies the slot, so the cleanup for the first is
	// deferred rather than run.
	require.NoError(t, m.StartPairing(context.Background(), PairRequest{Method: PairMethodQR}))
	devices.add(jid)
	require.True(t, dispatchEvent(t, m, abandoned, &events.PairSuccess{ID: jid}))
	require.Equal(t, []types.JID{jid}, devices.remaining(), "deferred, as designed")

	m.Stop()

	assert.Empty(t, devices.remaining(),
		"shutdown frees the slot, so it is the last turn that can run the deferred cleanup — and it must")

	// The next boot over the same store finds nothing to resurrect.
	next := newManagerForTest(t, syncStore, &fakeBackfillReader{})
	next.SetIngestor(&fakeIngestor{})
	next.SetHistoryRecorder(&fakeRecorder{})
	next.SetHistoryDrainReady()
	next.setDeviceOps(devices.ops())
	nextClient := newFakeClient()
	next.setSessionFactory(func(_ context.Context, req sessionRequest) (*session, error) {
		jids := devices.remaining()
		if len(jids) == 1 && req.linked == nil {
			// What the real resolver does with a lone row and no selector.
			return fakeSessionFor(nextClient, true, &jids[0]), nil
		}
		return fakeSessionFor(nextClient, false, nil), nil
	})
	require.NoError(t, next.Start(context.Background()))

	assert.Equal(t, StateNotPaired, next.Status().State,
		"an abandoned attempt's device must not become the linked account by default")
	assert.NotContains(t, nextClient.callLog(), "connect")
}

// TestStop_BoundsTheFinalFlushAgainstAStalledDatabase: the last turn's flush
// runs under its OWN budget rather than the effect context Stop has already
// cancelled — and a budget it is, not a background context. A shutdown that
// waits forever on a wedged database is a process that does not shut down.
func TestStop_BoundsTheFinalFlushAgainstAStalledDatabase(t *testing.T) {
	cli := newFakeClient()
	m, _, _, _, devices := newTestManagerWithDevices(t, cli, false)
	require.NoError(t, m.Start(context.Background()))

	require.NoError(t, m.StartPairing(context.Background(), PairRequest{Method: PairMethodQR}))
	abandoned := pairingSession(t, m)
	m.CancelPairing()
	require.NoError(t, m.StartPairing(context.Background(), PairRequest{Method: PairMethodQR}))

	jid := types.NewJID("15559991111", types.DefaultUserServer)
	devices.add(jid)
	require.True(t, dispatchEvent(t, m, abandoned, &events.PairSuccess{ID: jid}))
	require.Equal(t, []types.JID{jid}, devices.remaining(), "deferred behind the live attempt")

	devices.setErr(func(d *fakeDevices) { d.delStall = true })

	stopped := make(chan struct{})
	go func() { m.Stop(); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(actorDBTimeout + 2*time.Second):
		t.Fatalf("Stop did not return within one database budget of a stalled final flush")
	}
}

// TestTurnBudgetHoldsForAStalledFlush is the second worst-turn case.
//
// A deferred-cleanup list is unbounded, so a per-item budget would make the turn
// unbounded with it: four stalled deletes at the database timeout each already
// exceed the whole turn budget. The pass therefore shares ONE budget, and items
// it did not reach stay pending for the next flush trigger rather than being
// lost.
func TestTurnBudgetHoldsForAStalledFlush(t *testing.T) {
	cli := newFakeClient()
	m, _, _, _, devices := newTestManagerWithDevices(t, cli, false)
	require.NoError(t, m.Start(context.Background()))

	// An abandoned attempt, then a live one, so every stale event defers.
	require.NoError(t, m.StartPairing(context.Background(), PairRequest{Method: PairMethodQR}))
	abandoned := pairingSession(t, m)
	m.CancelPairing()
	require.NoError(t, m.StartPairing(context.Background(), PairRequest{Method: PairMethodQR}))

	const pending = 8
	for i := 0; i < pending; i++ {
		jid := types.NewJID(fmt.Sprintf("1555000%04d", i), types.DefaultUserServer)
		devices.add(jid)
		require.True(t, dispatchEvent(t, m, abandoned, &events.PairSuccess{ID: jid}))
	}
	require.Len(t, devices.remaining(), pending, "all deferred, none deleted")

	devices.setErr(func(d *fakeDevices) { d.delStall = true })

	// Freeing the slot runs the flush inside one turn.
	started := accelerated.GetCurrentTime()
	done := make(chan struct{})
	go func() { m.CancelPairing(); close(done) }()

	select {
	case <-done:
	case <-time.After(maxTurnBudget + 2*time.Second):
		t.Fatalf("a stalled flush of %d items exceeded the turn budget of %s: the pass is bounded per item, not as a batch", pending, maxTurnBudget)
	}
	assert.Less(t, accelerated.GetCurrentTime().Sub(started), maxTurnBudget+2*time.Second)

	// Nothing was lost: what the budget did not reach is still pending.
	assert.Len(t, devices.remaining(), pending, "a stalled delete deletes nothing")
	assert.NotEmpty(t, m.inspect().PendingStaleDeletes,
		"items the budget did not reach carry to the next flush rather than being dropped")
}

// TestDisconnect_FailedLogoutStillReleasesTheClient is the class-level guarantee
// applied to the path that had forgotten it.
//
// whatsmeow leaves a failed logout CONNECTED. The failure branch used to publish
// disconnect_failed and stop, so a retry or a Start could build a second client
// for the same device while the first was still live. Release is no longer that
// branch's job: the session's release is handed to the operation, and the loop
// performs it however the operation ends.
func TestDisconnect_FailedLogoutStillReleasesTheClient(t *testing.T) {
	cli := newFakeClient()
	cli.logoutErr = logoutRemoteFailure()
	m, _, _, _ := newTestManager(t, cli, true)
	require.NoError(t, m.Start(context.Background()))
	require.True(t, dispatchEvent(t, m, nil, &events.Connected{}))
	require.True(t, cli.IsConnected())

	_, err := m.Disconnect(context.Background(), false)
	require.ErrorIs(t, err, ErrRemoteUnlinkFailed)
	assert.Equal(t, StateDisconnectFailed, m.Status().State)

	eventually(t, "a failed logout still releases the client the library left connected", func() bool {
		return !cli.IsConnected() && cli.connCtxErr() != nil && !cli.pumpRunning()
	})
}

// TestCancelPairing_ReleasesTheAttemptsConnectionContext: an abandoned attempt's
// context is a child of the manager's, so leaving it alive until Stop would
// accumulate one per attempt and keep the library's auto-reconnect eligible on a
// client nobody owns.
func TestCancelPairing_ReleasesTheAttemptsConnectionContext(t *testing.T) {
	cli := newFakeClient()
	m, _, _, _ := newTestManager(t, cli, false)

	require.NoError(t, m.StartPairing(context.Background(), PairRequest{Method: PairMethodQR}))
	require.True(t, cli.pumpRunning(), "the attempt's socket is up")

	m.CancelPairing()

	eventually(t, "abandoning an attempt ends its connection context", func() bool {
		return cli.connCtxErr() != nil && !cli.pumpRunning()
	})
}

// TestRetire_IsTheOnlyWayASessionDies is the class-level scan.
//
// Every death path goes through retirement, and retirement is the only thing
// that schedules a release — so the package must contain exactly one call to
// the client's Disconnect, inside the single release helper. A new death path
// that closed a client by hand would be a second one.
func TestRetire_IsTheOnlyWayASessionDies(t *testing.T) {
	sources := packageSources(t)
	assert.Equal(t, 1, countOccurrences(sources, ".client.Disconnect()"),
		"exactly one place closes a client: releaseSession")
	// Two cancels, and the second is a NAMED exception rather than a second
	// death path: the dial watchdog aborts a dial that never returned. Counting
	// it stops the exception quietly becoming a way to kill a session by hand.
	assert.Equal(t, 2, countOccurrences(sources, "cancelConn()"),
		"releaseSession, plus the dial watchdog — and nothing else")
	assert.Equal(t, 1, strings.Count(sources["actor.go"], "func (e dialWatchdogEffect) run"),
		"the watchdog is the exception, and it lives in the actor where the census can see it")

	// A dial and its watchdog are emitted together; a connect without one would
	// be an unbounded dial, and a watchdog without one would never settle.
	assert.Equal(t, countOccurrences(sources, "connectEffect{sess:"),
		countOccurrences(sources, "m.launchDial("),
		"every connect is launched with its watchdog")

	// An attempt dies in exactly one place, so cancelling its context, marking
	// it, and releasing its client cannot drift apart.
	assert.Equal(t, 1, countOccurrences(sources, "func (st *actorState) endAttempt("))
	assert.Equal(t, 1, countOccurrences(sources, "p.cancel()"),
		"the attempt context is cancelled only inside endAttempt")
}

// TestStart_SessionReplacedMidDialIsStillReleased: release belongs to the act
// that ENDS a session's ownership, not to the operation that was using it.
//
// A Start installs its session and then dials. A pairing adopted while that dial
// is parked replaces the slot, so the connect's continuation aborts on the fence
// and never runs a failure branch of its own. The replaced session is released
// anyway, because the adoption retired it — otherwise a live socket and its
// auto-reconnect context would survive the account that owned them.
func TestStart_SessionReplacedMidDialIsStillReleased(t *testing.T) {
	startClient := newFakeClient()
	entered := make(chan struct{})
	release := make(chan struct{})
	startClient.connectEntered = entered
	startClient.connectBlock = release

	m, _, _, _ := newTestManager(t, startClient, true)

	startDone := make(chan struct{})
	go func() { defer close(startDone); _ = m.Start(context.Background()) }()
	<-entered
	startSess := installedSession(t, m)
	require.NotNil(t, startSess)

	// A pairing completes while the dial is parked, replacing the slot.
	pairClient := newFakeClient()
	useClient(m, pairClient, false)
	require.NoError(t, m.StartPairing(context.Background(), PairRequest{Method: PairMethodQR}))
	require.True(t, dispatchEvent(t, m, pairingSession(t, m), &events.PairSuccess{
		ID: types.NewJID("15556667777", types.DefaultUserServer),
	}))
	require.NotSame(t, startSess, installedSession(t, m))

	close(release)
	<-startDone

	eventually(t, "the replaced session is released even though its own operation aborted on the fence", func() bool {
		return startClient.connCtxErr() != nil && !startClient.pumpRunning()
	})
}

// TestFinalHandleCarriesWholeReleases closes the one gap the queue-then-drain
// design opens.
//
// The loop's last turn retires whatever it still owns, but the launch gate is
// already shut, so those releases cannot become effects. They leave through the
// published handle instead — and they must leave WHOLE. A bare session is not a
// teardown: it has lost the device row an abandoned attempt must not leave
// behind, and the drain that has to unwind before its client is closed.
func TestFinalHandleCarriesWholeReleases(t *testing.T) {
	cli := newFakeClient()
	sess := fakeSessionFor(cli, false, nil)

	st := &actorState{}
	p := &pairingState{sess: sess, drainDone: make(chan struct{}), drainStarted: true}
	st.pairing = p

	// Exactly what shutdownState does: end the attempt, then free the slot.
	st.endAttempt(p, false)
	st.pairing = nil

	h := st.teardownHandleOf()
	require.Len(t, h.releases, 1, "a release the last turn queued must reach the handle Stop reads")
	assert.Same(t, sess, h.releases[0].sess)
	assert.True(t, h.releases[0].deleteDevice,
		"an abandoned attempt's device row travels with its release, or shutdown leaves it behind")
	assert.True(t, h.releases[0].drainStarted,
		"and so does the drain that must unwind before the client is closed")
	assert.Same(t, sess, h.releases[0].sess)
}

// TestFinalHandleCarriesAReleaseAnOperationStillHolds is the half a cancelled
// operation exposes: Stop cancels the batch, so the continuation never runs and
// the operation never reaches releaseOp. The release is in the STATE rather than
// in the operation's flags precisely so that path cannot lose it.
func TestFinalHandleCarriesAReleaseAnOperationStillHolds(t *testing.T) {
	cli := newFakeClient()
	sess := fakeSessionFor(cli, true, nil)

	st := &actorState{sess: sess}
	rel := opFlags{unlink: true}
	st.retireFor(&rel, sess)

	require.Nil(t, st.sess, "ownership ends immediately")
	h := st.teardownHandleOf()
	require.Len(t, h.releases, 1,
		"a held release is still the loop's obligation, so the handle names it")
	assert.Same(t, sess, h.releases[0].sess)
	assert.True(t, h.releases[0].held)
}

// TestStop_ReleasesASessionAnUnlinkWasStillHolding is the end of the path the
// effect-goroutine test only started.
//
// An unlink parks in Logout, Stop cancels the batch, and the continuation
// therefore never runs — so nothing ever calls releaseOp. The client must still
// be closed and its connection context ended: whatsmeow leaves a failed or
// cancelled logout CONNECTED, and the session is in no slot for a handle to find
// it by. The RELEASE is what the handle carries.
func TestStop_ReleasesASessionAnUnlinkWasStillHolding(t *testing.T) {
	cli := newFakeClient()
	m, _, _, _ := newTestManager(t, cli, true)
	require.NoError(t, m.Start(context.Background()))
	require.True(t, dispatchEvent(t, m, nil, &events.Connected{}))
	require.True(t, cli.IsConnected())

	entered := make(chan struct{})
	never := make(chan struct{})
	t.Cleanup(func() { close(never) })
	cli.mu.Lock()
	cli.logoutEntered = entered
	cli.logoutBlock = never
	cli.mu.Unlock()

	unlinkDone := make(chan struct{})
	go func() { defer close(unlinkDone); _, _ = m.Disconnect(context.Background(), false) }()
	<-entered

	m.Stop()
	<-unlinkDone

	assert.False(t, cli.IsConnected(),
		"a session an operation was holding when Stop cancelled it is still released")
	assert.Error(t, cli.connCtxErr(),
		"and its connection context ends with it, or auto-reconnect outlives the manager")
	eventually(t, "the socket's read pump ends with the context", func() bool { return !cli.pumpRunning() })
}

// TestStop_DeletesTheDeviceOfAnAttemptItAbandoned: shutdown ends the in-flight
// attempt, and an attempt's half-written device row is part of ending it. A
// teardown that reached Stop as a bare session would disconnect the client and
// leave the row — which the next boot can resume as if the user had chosen it.
func TestStop_DeletesTheDeviceOfAnAttemptItAbandoned(t *testing.T) {
	cli := newFakeClient()
	m, _, _, _, _ := newTestManagerWithDevices(t, cli, false)
	require.NoError(t, m.Start(context.Background()))

	require.NoError(t, m.StartPairing(context.Background(), PairRequest{Method: PairMethodQR}))
	require.True(t, cli.pumpRunning(), "the attempt's socket is up")

	m.Stop()

	assert.Contains(t, cli.callLog(), "delete_device",
		"the abandoned attempt's own device row is removed by the release, not left for the next boot")
	assert.Less(t, indexOf(cli.callLog(), "disconnect"), indexOf(cli.callLog(), "delete_device"),
		"and the client is closed before its row goes")
	assert.Error(t, cli.connCtxErr())
}

// TestFlush_RotatesPastAStalledJIDAndRetriesItself is the liveness half of the
// bounded flush: bounding a pass says when it STOPS, not that the work happens.
//
// One stalled JID at the head of the queue used to consume every pass's whole
// budget and be put straight back at the head, so nothing behind it was ever
// reached — and nothing retried the pass at all until an unrelated pairing
// transition happened to occur. Here the stalled JID goes to the BACK, the loop
// schedules its own next pass, and the reachable JID is deleted with no external
// event of any kind. The retry is bounded: once the queue has been rotated all
// the way round with nothing settling, the loop stops asking.
func TestFlush_RotatesPastAStalledJIDAndRetriesItself(t *testing.T) {
	stalled := types.NewJID("15550001111", types.DefaultUserServer)
	reachable := types.NewJID("15550002222", types.DefaultUserServer)

	cli := newFakeClient()
	m, _, _, _, devices := newTestManagerWithDevices(t, cli, false)
	require.NoError(t, m.Start(context.Background()))

	// An abandoned attempt, then a live one, so both stale events defer.
	require.NoError(t, m.StartPairing(context.Background(), PairRequest{Method: PairMethodQR}))
	abandoned := pairingSession(t, m)
	m.CancelPairing()
	require.NoError(t, m.StartPairing(context.Background(), PairRequest{Method: PairMethodQR}))

	for _, jid := range []types.JID{stalled, reachable} {
		devices.add(jid)
		require.True(t, dispatchEvent(t, m, abandoned, &events.PairSuccess{ID: jid}))
	}
	require.Equal(t, []types.JID{stalled, reachable}, devices.remaining(), "both deferred")

	devices.setErr(func(d *fakeDevices) { d.delStallJID = &stalled })

	// ONE trigger, and then nothing: freeing the slot starts the first pass, and
	// every pass after it is the loop's own doing.
	m.CancelPairing()

	eventually(t, "a JID behind a stalled one is reached, without any further external event", func() bool {
		return len(devices.remaining()) == 1 && devices.remaining()[0] == stalled
	})
	assert.Equal(t, []types.JID{stalled}, m.inspect().PendingStaleDeletes,
		"the stalled JID is kept, not dropped")

	// And the retry STOPS. A database that never answers must not turn the actor
	// into a spinner, so the bound is absolute rather than "it looked quiet for
	// a moment": one stalled pass costs a whole database budget, so a window of
	// several budgets would show several more attempts if nothing stopped them.
	//
	// The correct run makes three: the first pass stalls, the second deletes the
	// reachable JID, the third stalls on the JID that came round again — and the
	// pass after that is never scheduled, because every pending JID has now had
	// one to itself.
	const maxAttempts = 4
	consistently(t, "the self-scheduled retries stop once every pending JID has had a pass",
		3*actorDBTimeout, func() bool { return devices.deleteAttempts() <= maxAttempts })
}
