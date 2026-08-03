package whatsapp

// The manager is an actor.
//
// All mutable manager state is owned by exactly one goroutine — the loop below —
// and is reachable only from inside a turn of that loop. There is no mutex in
// this package. Decide-and-act atomicity is therefore not a discipline that can
// be applied incompletely: a turn is indivisible by construction, because
// nothing else runs on the state while it executes.
//
// Long-running I/O never happens in a turn. A turn that needs it emits EFFECTS
// (blocking steps that run off the loop) plus a CONTINUATION, and the
// continuation re-enters through exactly one code path — the loop's contMsg arm
// — which applies the FENCE before the continuation body can run. That is the
// whole of the ownership-generation mechanism the earlier design expressed as a
// counter every new call site had to remember to check.

import (
	"context"
	"fmt"
	"slices"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/logger"
	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"
)

const (
	// actorDBTimeout bounds every repository call made from inside a turn. The
	// calls stay in the turn — that is the atomic-terminal-write requirement —
	// so a hung database must bound the turn rather than wedge the loop.
	actorDBTimeout = 2 * time.Second

	// maxTurnBudget is the arithmetic worst case for one turn, plus a second of
	// slack: a terminal turn makes terminalPersistAttempts writes plus at most
	// one status-only fallback, each bounded by actorDBTimeout. It is measured
	// and logged, not assumed.
	maxTurnBudget = 3*actorDBTimeout + time.Second

	// inboxCapacity bounds the mailbox. The producers are the eight
	// control-event kinds, a handful of API operations, one continuation per
	// in-flight operation, and one QR item per ~20 s. The data plane — the only
	// high-rate source — never enters the queue at all, so this is generous
	// rather than tuned.
	inboxCapacity = 64

	// ctrlEnqueueDeadline bounds the ONE control event whose loss is
	// recoverable. See enqueueControl.
	ctrlEnqueueDeadline = 30 * time.Second

	// effectDeadline bounds a one-shot effect batch, so a Logout or Connect
	// against a black-holed socket cannot outlive it.
	effectDeadline = 30 * time.Second

	// drainDrainTimeout bounds the wait for a QR drain goroutine to unwind
	// before its client is disconnected. The library removes its own QR handler
	// from a goroutine, so the client must still exist when that runs.
	drainDrainTimeout = 5 * time.Second

	// stopLoopDeadline is how long Stop waits for the loop to exit before
	// declaring it wedged and taking the bounded shutdown tier.
	stopLoopDeadline = 10 * time.Second

	// backfillReadTimeout bounds the two database reads Status() makes on the
	// caller's goroutine. The status endpoint is what an operator hits DURING an
	// outage, so it must never be the thing that hangs.
	backfillReadTimeout = 2 * time.Second
)

// managerTimeouts carries the four bounds whose EXPIRY is a behaviour with its
// own contract. They are construction-time values rather than constants solely
// so a test can drive an expiry without sleeping for the production bound;
// production always uses defaultTimeouts and never changes them afterwards.
type managerTimeouts struct {
	effect      time.Duration
	stopLoop    time.Duration
	drainDrain  time.Duration
	ctrlEnqueue time.Duration
}

func defaultTimeouts() managerTimeouts {
	return managerTimeouts{
		effect:      effectDeadline,
		stopLoop:    stopLoopDeadline,
		drainDrain:  drainDrainTimeout,
		ctrlEnqueue: ctrlEnqueueDeadline,
	}
}

// --- state -----------------------------------------------------------------

// sessionRequest is the input to the client-construction seam.
type sessionRequest struct {
	// fresh asks for a brand-new unpaired device (pairing). Otherwise the
	// stored device is resolved.
	fresh bool
	// linked is the JID the last successful pairing adopted, used to resolve
	// the stored device deterministically.
	linked *types.JID
}

// sessionFactory builds a session. It is the seam tests replace; production is
// Manager.newContainerSession.
type sessionFactory func(ctx context.Context, req sessionRequest) (*session, error)

// deviceOps is the device-store seam the staged purge and the JID-targeted
// cleanup use. It goes through the CONTAINER, so it needs no client and no
// device row of its own.
type deviceOps struct {
	list      func(ctx context.Context) ([]types.JID, error)
	deleteAll func(ctx context.Context, jids []types.JID) error
	deleteJID func(ctx context.Context, jid types.JID) error
}

// actorState is every mutable field the manager has. It is created by the loop,
// passed only as a parameter into turn bodies, and never stored on Manager, in
// a package variable, or in a closure that a goroutine could capture. It has no
// lock because it has exactly one reader-writer: the loop goroutine.
type actorState struct {
	sess        *session
	pairing     *pairingState
	status      Status
	syncStateID *uuid.UUID
	linkedJID   *types.JID

	ingestor          MessageIngestor
	historyRecorder   HistoryNotificationRecorder
	historyDrainReady bool

	newSession sessionFactory
	devices    deviceOps

	// unlinkInFlight and startInFlight serialize the two operations that can
	// span several turns. Both are read and written only by turns, which is what
	// makes the pairing/unlink mutual exclusion (A2.5 part 1) airtight rather
	// than merely likely.
	unlinkInFlight bool
	startInFlight  bool

	// pendingStaleDeletes holds device JIDs whose targeted cleanup was deferred
	// because an attempt was in flight when the proving event arrived. They are
	// re-guarded and launched by the turn that frees the pairing slot, so a
	// deferral is a delay rather than a lost cleanup.
	pendingStaleDeletes []types.JID

	// launchClosed is set ONLY by shutdownState, on the loop, as its last act
	// before the loop exits. It is what lets Stop wait on m.effects without
	// racing an Add it cannot exclude.
	launchClosed bool
}

// ready reports whether the client may connect, and names the missing piece
// when it may not.
func (st *actorState) ready() (bool, string) {
	if _, isDefault := st.ingestor.(refusingIngestor); isDefault || st.ingestor == nil {
		return false, "message ingestor is not wired"
	}
	if st.historyRecorder == nil {
		return false, "history notification recorder is not wired"
	}
	if !st.historyDrainReady {
		return false, "history drain worker is not registered"
	}
	return true, ""
}

// metadataBase seeds a metadata write with the keys that must survive it. Every
// write replaces the whole document, so the linked JID has to be carried
// forward explicitly or a terminal write would erase the only thing that makes
// the restart path deterministic.
func (st *actorState) metadataBase() map[string]any {
	base := map[string]any{}
	if st.linkedJID != nil {
		base[metadataLinkedJID] = st.linkedJID.String()
	}
	return base
}

// --- the fence -------------------------------------------------------------

// fence is the slot state a deciding turn saw. Its continuation may run only if
// both slots still hold exactly what the decision was about.
//
// It is POINTER IDENTITY on the slots rather than a counter, so it also covers
// the case a counter covered by accident: a turn that decided "there is no
// installed session" is fenced on sess == nil still being true. There is no ABA,
// because a session that leaves the slot is retired first and retirement is
// checked.
type fence struct {
	sess    *session
	pairing *pairingState
}

func (st *actorState) fenceOK(f fence) bool {
	if st.sess != f.sess || st.pairing != f.pairing {
		return false
	}
	if f.sess != nil && f.sess.retired {
		return false
	}
	if f.pairing != nil && f.pairing.cancelled {
		return false
	}
	return true
}

// opFlags is DATA, not a callback: the small set of things that end an
// operation. The loop clears them itself on any path that ends the operation,
// including the abort path, so a body that returns early cannot forget to.
type opFlags struct {
	unlink bool
	start  bool
	// pairing, when set, is the attempt this operation owns. If the operation
	// ends while the attempt is still in the slot, the loop abandons it — which
	// is what stops a continuation aborted on the fence from stranding a pairing
	// with a half-built client and no drain.
	pairing *pairingState
}

// releaseOp ends an operation. It runs on the loop, in the same turn as the
// continuation that ended it.
func (m *Manager) releaseOp(st *actorState, rel opFlags) {
	if rel.unlink {
		st.unlinkInFlight = false
	}
	if rel.start {
		st.startInFlight = false
	}
	if rel.pairing != nil && st.pairing == rel.pairing {
		m.abandonPairingTurn(st, rel.pairing, ErrPairingCancelled)
	}
}

// discardOrphans drops every session an aborted batch built OR connected.
//
// Effects that leave a client live report their session, so this covers the
// build-to-unlink path too: whatsmeow does not disconnect on a failed logout, so
// a continuation that aborts on the fence would otherwise strand a connected
// client nobody owns. Doing it HERE, in the loop, is what makes it impossible
// for an operation to forget.
//
// A session the manager currently OWNS is never discarded — a fence can fail
// for reasons that leave the batch's session installed or in the pairing slot,
// and disconnecting it would tear down a live account. A PAIRED session is
// disconnected but never deleted: its device row is the link itself.
func (m *Manager) discardOrphans(st *actorState, res effectResult) {
	owned := func(s *session) bool {
		if st.sess == s {
			return true
		}
		return st.pairing != nil && st.pairing.sess == s
	}

	var seen []*session
	for _, step := range res.steps {
		s := step.sess
		if s == nil || owned(s) {
			continue
		}
		if slices.Contains(seen, s) {
			continue
		}
		seen = append(seen, s)

		fx := []effect{disconnectEffect{sess: s}}
		if !s.paired {
			fx = append(fx, deleteDeviceEffect{sess: s})
		}
		m.launch(st, fx, launchDetached, fence{}, opFlags{}, nil, nil)
	}
}

// --- messages --------------------------------------------------------------

type actorMsg interface{ actorMessage() }

// ctrlEventMsg is a lifecycle event, carrying the session that emitted it so
// attribution is a field rather than an inference.
type ctrlEventMsg struct {
	from *session
	evt  any
}

// settleMsg is a FIFO barrier. Because the inbox is FIFO, a settle returning
// means every previously submitted message has been fully processed AND
// published.
type settleMsg struct{ reply chan struct{} }

// inspectMsg returns a read-only COPY of the loop's state. There is no callback
// variant: nothing caller-supplied ever runs on the loop.
type inspectMsg struct{ reply chan stateView }

// opMsg is an API operation. The loop never waits for the caller; the caller
// waits on its own reply channel.
type opMsg struct {
	run   func(st *actorState, reply chan opResult)
	reply chan opResult
}

// contMsg re-enters the loop after blocking I/O. It is the ONLY way a
// continuation body can execute, which is why the fence is applied here and
// nowhere else.
type contMsg struct {
	fence    fence
	releases opFlags
	body     contBody
	abort    func(error)
	result   effectResult
}

type qrMsg struct {
	p    *pairingState
	item whatsmeow.QRChannelItem
}

type qrClosedMsg struct{ p *pairingState }

func (*ctrlEventMsg) actorMessage() {}
func (*settleMsg) actorMessage()    {}
func (*inspectMsg) actorMessage()   {}
func (*opMsg) actorMessage()        {}
func (*contMsg) actorMessage()      {}
func (*qrMsg) actorMessage()        {}
func (*qrClosedMsg) actorMessage()  {}

// contBody is a continuation. It reports whether the operation continues; when
// it does not, the loop releases the operation's flags itself.
type contBody func(st *actorState, res effectResult) (operationContinues bool)

// opResult is what an operation replies to its caller.
type opResult struct {
	err error
	val any
}

// --- snapshot --------------------------------------------------------------

// snapshot is immutable once published. Every field is either a value, an
// interface whose implementation is immutable after installation, or a pointer
// the loop has already deep-copied.
type snapshot struct {
	status  Status
	sess    *session
	ready   bool
	missing string

	// The data plane reads these and never enters the queue. They live here,
	// not in actorState alone, precisely because they must be readable off the
	// loop. actorState holds the authoritative copy; publish copies the
	// interface values across, and an interface value is a two-word copy, so a
	// reader can never observe a half-installed seam.
	ingestor        MessageIngestor
	historyRecorder HistoryNotificationRecorder

	// teardown is what lets Stop tear the stack down WITHOUT the loop's
	// cooperation. It is republished every turn like everything else.
	teardown teardownHandle
}

// teardownHandle is everything Stop needs, published rather than handed over,
// so a wedged loop cannot deprive Stop of it.
type teardownHandle struct {
	sess        *session
	pairingSess *session
	// carried holds sessions inherited from an earlier handle during a union.
	// Keeping them separate is what makes the union COMPLETE: a session the
	// final turn replaced still holds a socket, and one it installed is only in
	// the final handle.
	carried      []*session
	pairingCtx   context.CancelFunc
	drainDone    <-chan struct{}
	drainStarted bool
}

// sessions lists every client this handle must disconnect, deduplicated.
func (h teardownHandle) sessions() []*session {
	var out []*session
	add := func(s *session) {
		if s == nil {
			return
		}
		for _, existing := range out {
			if existing == s {
				return
			}
		}
		out = append(out, s)
	}
	add(h.sess)
	add(h.pairingSess)
	for _, s := range h.carried {
		add(s)
	}
	return out
}

// unionHandles merges the early handle into the final one. Client.Disconnect is
// idempotent, so the union costs nothing and misses nothing.
func unionHandles(early, final teardownHandle) teardownHandle {
	out := final
	out.carried = append(append([]*session(nil), final.carried...), early.sess, early.pairingSess)
	if out.pairingCtx == nil {
		out.pairingCtx = early.pairingCtx
	}
	if out.drainDone == nil {
		out.drainDone = early.drainDone
		out.drainStarted = early.drainStarted
	}
	return out
}

// cachedBackfill is the last good backfill read. Status() serves it, marked
// stale, when a fresh read times out. It carries no timestamp: nothing reads
// one, and a field nobody reads is a field that cannot be right.
type cachedBackfill struct {
	Val BackfillStatus
}

// teardownHandleOf builds the handle for the current state.
func (st *actorState) teardownHandleOf() teardownHandle {
	h := teardownHandle{sess: st.sess}
	if p := st.pairing; p != nil {
		h.pairingSess = p.sess
		h.pairingCtx = p.cancel
		h.drainDone = p.drainDone
		h.drainStarted = p.drainStarted
	}
	return h
}

// publish makes the turn's state visible. It runs at the end of EVERY turn,
// unconditionally: a dirty flag would be cheaper and is deliberately not used,
// because "remember to mark the state dirty" is the same shape of forgettable
// convention this design exists to delete.
func (m *Manager) publish(st *actorState) {
	ready, missing := st.ready()
	s := &snapshot{
		status:          st.status.clone(),
		sess:            st.sess,
		ready:           ready,
		missing:         missing,
		ingestor:        st.ingestor,
		historyRecorder: st.historyRecorder,
		teardown:        st.teardownHandleOf(),
	}
	if p := st.pairing; p != nil {
		pv := p.snapshot()
		s.status.Pairing = &pv
	}
	m.snap.Store(s)
}

// --- the loop --------------------------------------------------------------

// startLoop is the only place the actor goroutine is created.
func (m *Manager) startLoop(st *actorState) {
	go m.loop(st)
}

func (m *Manager) loop(st *actorState) {
	defer close(m.loopExited)
	for {
		// Shutdown linearization: a closed `stopping` wins over a ready inbox,
		// which Go's select would otherwise decide by coin toss. After
		// close(m.stopping) the loop dispatches at most the turn already
		// executing and then exits; the mailbox is abandoned, not drained.
		select {
		case <-m.stopping:
			m.shutdownState(st)
			return
		default:
		}

		select {
		case <-m.stopping:
			m.shutdownState(st)
			return
		case msg := <-m.inbox:
			started := accelerated.GetCurrentTime()
			m.dispatch(st, msg)
			m.publish(st)
			if elapsed := accelerated.GetCurrentTime().Sub(started); elapsed > maxTurnBudget {
				logger.Error().
					Str("message", fmt.Sprintf("%T", msg)).
					Dur("elapsed", elapsed).
					Msg("whatsapp: actor turn exceeded its budget")
			}
		}
	}
}

func (m *Manager) dispatch(st *actorState, msg actorMsg) {
	switch v := msg.(type) {
	case *ctrlEventMsg:
		m.handleControlEvent(st, v.from, v.evt)

	case *settleMsg:
		close(v.reply)

	case *inspectMsg:
		v.reply <- st.view()

	case *opMsg:
		v.run(st, v.reply)

	case *contMsg:
		if !st.fenceOK(v.fence) {
			// The operation decided about state that has since moved on. Work
			// already committed against its OWN target stands; only the
			// publication is withheld — and `abort` takes no *actorState, so an
			// aborting operation is structurally incapable of publishing.
			m.discardOrphans(st, v.result)
			m.releaseOp(st, v.releases)
			if v.abort != nil {
				v.abort(ErrOperationSuperseded)
			}
			return
		}
		if !v.body(st, v.result) {
			m.releaseOp(st, v.releases)
		}

	case *qrMsg:
		m.onQRItem(st, v.p, v.item)

	case *qrClosedMsg:
		m.onQRClosed(st, v.p)
	}
}

// shutdownState is the loop's last act. It retires the installed session and
// cancels any pairing for the benefit of a turn that was mid-flight, closes the
// effect-launch gate, and publishes a final snapshot — which is the handle Stop
// reads on its clean-exit tier.
func (m *Manager) shutdownState(st *actorState) {
	if st.sess != nil {
		st.sess.retired = true
	}
	if p := st.pairing; p != nil {
		p.cancelled = true
		if p.cancel != nil {
			p.cancel()
		}
	}
	st.launchClosed = true
	m.publish(st)
}

// --- submission ------------------------------------------------------------

// submit enqueues a message. Every wait in this package selects on `stopping`,
// never on `done`: `done` closes only after effects.Wait(), so waiting on it
// here would deadlock a shutdown whose mailbox was abandoned with an effect in
// flight.
func (m *Manager) submit(msg actorMsg) bool {
	select {
	case m.inbox <- msg:
		return true
	case <-m.stopping:
		return false
	}
}

// runOp submits an operation and waits for its reply. It bails on `stopping` on
// BOTH halves: a send can land in the buffer moments before the loop exits, and
// without the second half the caller would wait for a reply nobody will write.
//
// The trailing barrier is load-bearing, not tidiness. A reply is sent from
// INSIDE its turn, while publish runs at the END of it, so a caller that
// returned on the reply alone could read a snapshot its own operation had
// already superseded. Waiting the barrier out here — once, for every operation —
// makes "the call returned" and "its outcome is visible" the same instant,
// without asking any turn body to remember to publish before replying.
func (m *Manager) runOp(run func(*actorState, chan opResult)) opResult {
	reply := make(chan opResult, 1)
	if !m.submit(&opMsg{run: run, reply: reply}) {
		return opResult{err: ErrManagerStopped}
	}
	select {
	case res := <-reply:
		m.settle()
		return res
	case <-m.stopping:
		return opResult{err: ErrManagerStopped}
	}
}

// settle is a FIFO barrier used by tests: when it returns, every message
// submitted before it has been fully processed and published.
func (m *Manager) settle() {
	reply := make(chan struct{})
	if !m.submit(&settleMsg{reply: reply}) {
		return
	}
	select {
	case <-reply:
	case <-m.stopping:
	}
}

// stateView is a read-only copy of the loop's state, taken inside a turn. The
// pointers are identity handles for assertions; the booleans that would
// otherwise require dereferencing loop-owned fields are copied out.
type stateView struct {
	Sess             *session
	SessRetired      bool
	Pairing          *pairingState
	PairingSess      *session
	PairingCancelled bool
	PairingDrainOn   bool
	LinkedJID        *types.JID
	Ready            bool
	Missing          string
	UnlinkInFlight   bool
	StartInFlight    bool
	SyncStateID      *uuid.UUID
}

func (st *actorState) view() stateView {
	v := stateView{
		Sess:           st.sess,
		Pairing:        st.pairing,
		LinkedJID:      st.linkedJID,
		UnlinkInFlight: st.unlinkInFlight,
		StartInFlight:  st.startInFlight,
		SyncStateID:    st.syncStateID,
	}
	v.Ready, v.Missing = st.ready()
	if st.sess != nil {
		v.SessRetired = st.sess.retired
	}
	if st.pairing != nil {
		v.PairingSess = st.pairing.sess
		v.PairingCancelled = st.pairing.cancelled
		v.PairingDrainOn = st.pairing.drainStarted
	}
	return v
}

// inspect returns a value, not the state, and takes no callback. It bails on
// `stopping` and returns the zero value after Stop, so an inspection cannot
// hang a test binary.
func (m *Manager) inspect() stateView {
	reply := make(chan stateView, 1)
	if !m.submit(&inspectMsg{reply: reply}) {
		return stateView{}
	}
	select {
	case v := <-reply:
		return v
	case <-m.stopping:
		return stateView{}
	}
}

// --- effects ---------------------------------------------------------------

// effect is a blocking step run OFF the loop. Effects are a closed set of value
// types with explicit fields — never closures over the state — so an effect
// structurally cannot reach *actorState.
type effect interface {
	run(ctx context.Context) stepResult
}

// stepResult is one effect's outcome. Errors are DATA that a fenced turn
// interprets, never control flow: a failed Logout and a failed device purge
// produce completely different publications.
type stepResult struct {
	err  error
	sess *session
	qr   <-chan whatsmeow.QRChannelItem
	code string
	jids []types.JID
}

type effectResult struct {
	steps []stepResult
	// err is the batch-level outcome: cancellation, deadline expiry, or a
	// recovered panic. It is a RESULT, never an early return — see runEffects.
	err error
}

func (r effectResult) step(i int) stepResult {
	if i >= 0 && i < len(r.steps) {
		return r.steps[i]
	}
	return stepResult{}
}

// firstErr reports the batch error, or the first failing step's error.
func (r effectResult) firstErr() error {
	if r.err != nil {
		return r.err
	}
	for _, s := range r.steps {
		if s.err != nil {
			return s.err
		}
	}
	return nil
}

type launchKind int

const (
	// launchOneShot: bounded batch, exactly one continuation.
	launchOneShot launchKind = iota
	// launchDetached: bounded batch, no continuation (teardown work whose
	// outcome nothing waits on).
	launchDetached
	// launchLong: the QR drain, which reads its channel for the life of the
	// attempt and is bounded by the pairing context rather than by a deadline.
	launchLong
)

// launch is the ONLY place effects.Add and the runEffects spawn appear. It is
// called only from a turn, i.e. only on the loop goroutine. Nothing calls
// runEffects directly; a turn that tried would bypass both the shutdown gate
// and the goroutine census.
func (m *Manager) launch(st *actorState, fx []effect, kind launchKind, f fence, rel opFlags, body contBody, abort func(error)) bool {
	if st.launchClosed {
		if abort != nil {
			abort(ErrManagerStopped)
		}
		return false
	}
	m.effects.Add(1)
	go m.runEffects(fx, kind, f, rel, body, abort)
	return true
}

// runEffects executes a batch off the loop and re-enters through a
// continuation.
//
// It NEVER returns without submitting a continuation, except when m.stopping is
// closed. Cancellation and deadline expiry are outcomes to REPORT, not early
// returns: an effectDeadline expiry outside shutdown is an ordinary operation
// failure, and returning without a continuation would strand the operation's
// in-flight flag and leave its caller waiting on a reply nobody would write.
func (m *Manager) runEffects(batch []effect, kind launchKind, f fence, rel opFlags, body contBody, abort func(error)) {
	defer m.effects.Done()

	res := m.execEffects(batch, kind)
	if body == nil {
		return
	}
	select {
	case m.inbox <- &contMsg{fence: f, releases: rel, body: body, abort: abort, result: res}:
	case <-m.stopping:
		// The only path that submits nothing: here the reply IS ErrManagerStopped
		// and the state is dying anyway.
		if abort != nil {
			abort(ErrManagerStopped)
		}
	}
}

// execEffects runs the batch, gating each step on cancellation BEFORE it starts.
// Several effects wrap client calls that take no context at all
// (Client.Disconnect has no ctx parameter), so handing a cancelled context to
// the batch cannot preempt them — the gate is what makes "after Stop cancels the
// effect context, no effect that has not already begun will begin" true.
func (m *Manager) execEffects(batch []effect, kind launchKind) (res effectResult) {
	ctx := m.effectCtx
	if kind != launchLong {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(m.effectCtx, m.timeouts.effect)
		defer cancel()
	}

	defer func() {
		if r := recover(); r != nil {
			// A panicking effect must fail its operation, not take the process
			// down: whatsmeow's own dispatcher recovers, and we must not be less
			// safe than the library.
			res.err = fmt.Errorf("whatsapp: effect panicked: %v", r)
			logger.Error().Str("panic", fmt.Sprint(r)).Msg("whatsapp: effect panicked")
		}
	}()

	for _, fx := range batch {
		if err := ctx.Err(); err != nil {
			res.err = err
			break
		}
		step := fx.run(ctx)
		res.steps = append(res.steps, step)
		if step.err != nil {
			break
		}
	}
	return res
}

// --- effect types ----------------------------------------------------------

type buildSessionEffect struct {
	build sessionFactory
	req   sessionRequest
}

func (e buildSessionEffect) run(ctx context.Context) stepResult {
	if e.build == nil {
		return stepResult{err: fmt.Errorf("whatsapp: no session factory")}
	}
	sess, err := e.build(ctx, e.req)
	return stepResult{sess: sess, err: err}
}

type connectEffect struct{ sess *session }

// run dials under the BATCH context, so a black-holed dial is bounded by
// effectDeadline and cancelled by Stop. It reports the session it connected, so
// a continuation that aborts on the fence hands a live socket to teardown rather
// than orphaning it (see discardOrphans).
func (e connectEffect) run(ctx context.Context) stepResult {
	if e.sess == nil {
		return stepResult{err: fmt.Errorf("whatsapp: no session to connect")}
	}
	return stepResult{sess: e.sess, err: e.sess.client.ConnectContext(ctx)}
}

type disconnectEffect struct{ sess *session }

func (e disconnectEffect) run(context.Context) stepResult {
	if e.sess != nil {
		e.sess.client.Disconnect()
	}
	return stepResult{}
}

type logoutEffect struct{ sess *session }

// run reports its session for the same reason connectEffect does: whatsmeow
// deliberately does NOT disconnect on a failed logout, so a fence-failed
// continuation would otherwise leave a connected client nobody owns.
func (e logoutEffect) run(ctx context.Context) stepResult {
	if e.sess == nil {
		return stepResult{err: fmt.Errorf("whatsapp: no session to log out")}
	}
	return stepResult{sess: e.sess, err: e.sess.client.Logout(ctx)}
}

type openQRChannelEffect struct {
	sess *session
	// pairCtx is the attempt's own context, created by the turn that claimed the
	// slot. GetQRChannel registers a handler on the client and only this
	// context's cancellation removes it, so it must be the attempt's context and
	// not the batch's.
	pairCtx context.Context
}

func (e openQRChannelEffect) run(context.Context) stepResult {
	if e.sess == nil {
		return stepResult{err: fmt.Errorf("whatsapp: no session for the QR channel")}
	}
	ch, err := e.sess.client.GetQRChannel(e.pairCtx)
	return stepResult{qr: ch, err: err}
}

type pairPhoneEffect struct {
	sess  *session
	phone string
	ctx   context.Context
}

func (e pairPhoneEffect) run(ctx context.Context) stepResult {
	if e.sess == nil {
		return stepResult{err: fmt.Errorf("whatsapp: no session for phone pairing")}
	}
	callCtx := e.ctx
	if callCtx == nil {
		callCtx = ctx
	}
	code, err := e.sess.client.PairPhone(callCtx, e.phone, false, whatsmeow.PairClientChrome, pairClientDisplayName)
	return stepResult{code: code, err: err}
}

type deleteDeviceEffect struct{ sess *session }

func (e deleteDeviceEffect) run(ctx context.Context) stepResult {
	if e.sess == nil || e.sess.deleteDevice == nil {
		return stepResult{}
	}
	// Retried on the same discipline as the terminal write: one transient blip
	// must not leave the store holding two sessions.
	var err error
	for attempt := 0; attempt < deviceDeleteAttempts; attempt++ {
		if err = e.sess.deleteDevice(ctx); err == nil {
			return stepResult{}
		}
	}
	return stepResult{err: fmt.Errorf("delete device: %w", err)}
}

// listDevicesEffect is the NON-destructive enumeration: stage a of the staged
// purge. Aborting on the fence after it destroys nothing.
type listDevicesEffect struct {
	list func(ctx context.Context) ([]types.JID, error)
}

func (e listDevicesEffect) run(ctx context.Context) stepResult {
	if e.list == nil {
		return stepResult{}
	}
	jids, err := e.list(ctx)
	return stepResult{jids: jids, err: err}
}

// deleteDevicesEffect deletes EXACTLY the frozen list — never "whatever is in
// the store now", which is delete-what-you-find wearing a different hat.
type deleteDevicesEffect struct {
	del  func(ctx context.Context, jids []types.JID) error
	jids []types.JID
}

func (e deleteDevicesEffect) run(ctx context.Context) stepResult {
	if e.del == nil || len(e.jids) == 0 {
		return stepResult{}
	}
	return stepResult{err: e.del(ctx, e.jids)}
}

// deleteDeviceByJIDEffect removes exactly one device, on the event that proves
// it was written. It is the backstop for an abandoned pairing whose library-side
// save landed after our teardown had already no-opped.
type deleteDeviceByJIDEffect struct {
	del func(ctx context.Context, jid types.JID) error
	jid types.JID
}

func (e deleteDeviceByJIDEffect) run(ctx context.Context) stepResult {
	if e.del == nil {
		return stepResult{}
	}
	var err error
	for attempt := 0; attempt < deviceDeleteAttempts; attempt++ {
		if err = e.del(ctx, e.jid); err == nil {
			return stepResult{}
		}
	}
	return stepResult{err: fmt.Errorf("delete device %s: %w", e.jid, err)}
}

// teardownPairingEffect ends an attempt's client. It waits for the QR drain to
// unwind FIRST — bounded, and skipped entirely when no drain was ever launched —
// so the library's handler-removal goroutine runs against a client that still
// exists.
type teardownPairingEffect struct {
	sess         *session
	drainDone    <-chan struct{}
	drainStarted bool
	drainWait    time.Duration
}

func (e teardownPairingEffect) run(ctx context.Context) stepResult {
	if e.drainStarted && e.drainDone != nil {
		timer := time.NewTimer(e.drainWait)
		select {
		case <-e.drainDone:
		case <-timer.C:
			logger.Warn().Msg("whatsapp: QR drain did not unwind before the teardown deadline")
		}
		timer.Stop()
	}
	if e.sess == nil {
		return stepResult{}
	}
	e.sess.client.Disconnect()
	if e.sess.deleteDevice != nil {
		if err := e.sess.deleteDevice(ctx); err != nil {
			logger.Warn().Err(err).Msg("whatsapp: failed to delete abandoned pairing device")
		}
	}
	return stepResult{}
}

// drainQRChannelEffect is the one long-lived effect. It reads the QR channel for
// the life of the attempt and submits a qrMsg per item, then a qrClosedMsg when
// the library closes the channel. It has no continuation; its messages carry the
// attempt they belong to and are fenced by the ordinary pairing-slot check
// inside their turns.
//
// It is spawned through launch like every other effect, which is what keeps
// m.effects a complete census of live effect goroutines.
type drainQRChannelEffect struct {
	m  *Manager
	p  *pairingState
	ch <-chan whatsmeow.QRChannelItem
}

func (e drainQRChannelEffect) run(context.Context) stepResult {
	defer close(e.p.drainDone)
	for {
		select {
		case <-e.p.ctx.Done():
			return stepResult{}
		case item, open := <-e.ch:
			if !open {
				e.m.submit(&qrClosedMsg{p: e.p})
				return stepResult{}
			}
			if !e.m.submit(&qrMsg{p: e.p, item: item}) {
				return stepResult{}
			}
		}
	}
}

// --- shutdown --------------------------------------------------------------

// Stop tears the stack down. It never logs out: a process shutdown must not
// unlink the device.
//
// The guarantee is deliberately TWO-TIER. On a clean loop exit it is strong:
// every effect goroutine has finished when Stop returns. On a wedged loop it is
// bounded, not zero — every effect is cancelled, no effect that had not already
// begun will begin, and at most one call per in-flight batch is still unwinding
// under a cancellation already issued. Waiting anyway would be a WaitGroup
// misuse, because the launch gate cannot close until the loop reaches
// shutdownState.
func (m *Manager) Stop() {
	m.stopOnce.Do(func() {
		// 1. First, before anything else. Every wait in the package selects on
		//    it, so every submitter, settle and inspect is released at once,
		//    regardless of what the loop is doing.
		close(m.stopping)

		// 2. The EARLY handle, loaded at once so cancellation is prompt. It may
		//    be stale by one turn; every use below is idempotent.
		h0 := m.snap.Load().teardown

		// 3. Cancel: the QR drain and the library's own QR handler first, then
		//    every effect.
		if h0.pairingCtx != nil {
			h0.pairingCtx()
		}
		m.cancelEffects()

		// 4. The tier discriminator.
		clean := false
		timer := time.NewTimer(m.timeouts.stopLoop)
		select {
		case <-m.loopExited:
			clean = true
		case <-timer.C:
			logger.Error().Msg("whatsapp: the actor loop did not exit within its deadline; tearing down from the last published handle")
		}
		timer.Stop()

		// 5. Choose the handle by tier. On a clean exit shutdownState's final
		//    publish happens-before close(loopExited) on the loop goroutine, and
		//    the atomic store/load pair carries that ordering, so h1 is the
		//    loop's FINAL handle.
		h := h0
		if clean {
			h = unionHandles(h0, m.snap.Load().teardown)
			if h.pairingCtx != nil {
				h.pairingCtx()
			}
		}

		// 6. Wait for the drain only if one was ever launched.
		if h.drainStarted && h.drainDone != nil {
			drainTimer := time.NewTimer(m.timeouts.drainDrain)
			select {
			case <-h.drainDone:
			case <-drainTimer.C:
				logger.Warn().Msg("whatsapp: QR drain did not unwind during shutdown")
			}
			drainTimer.Stop()
		}

		// 7. Disconnect, never log out.
		for _, s := range h.sessions() {
			s.client.Disconnect()
		}

		// 8. Tier 1 only.
		if clean {
			m.effects.Wait()
		} else {
			logger.Error().Msg("whatsapp: skipping the effect wait because the loop is wedged; effects were cancelled")
		}

		// 9. "Shutdown complete". Nothing waits on this to make progress.
		close(m.done)
	})
}

// --- readers ---------------------------------------------------------------

// Ready reports whether the client may connect, and names the missing piece when
// it may not. It reads the published snapshot: wait-free, and never a mix of two
// turns' state.
func (m *Manager) Ready() (bool, string) {
	s := m.snap.Load()
	if s == nil {
		return false, "manager is not started"
	}
	return s.ready, s.missing
}

// Status returns the current connection, pairing and backfill snapshot.
//
// It never routes through the queue. GET /whatsapp/auth/status is the settings
// page's poll and the one endpoint that must answer when something is wrong;
// putting it behind a turn would hang it precisely when an operator is trying to
// find out why.
func (m *Manager) Status() Status {
	s := m.snap.Load()
	if s == nil {
		return Status{Configured: true, State: StateNotReady, Reason: ReasonIngestNotWired}
	}
	// Cloned on the way out as well as on the way in: publish-only cloning would
	// let two concurrent readers share pointer targets, read-only cloning would
	// leave the loop holding aliases into what it published.
	out := s.status.clone()
	out.Backfill = m.backfillStatus(context.Background())
	return out
}

// SelfJID reports the linked account's JID when one is known.
func (m *Manager) SelfJID() (types.JID, bool) {
	s := m.snap.Load()
	if s == nil || s.status.JID == nil {
		return types.EmptyJID, false
	}
	jid, err := types.ParseJID(*s.status.JID)
	if err != nil {
		return types.EmptyJID, false
	}
	return jid, true
}

// backfillStatus reads the drain counters on the CALLER's goroutine — they are
// not manager state. Both reads are bounded and the last good value is cached,
// so a wedged history table degrades the answer rather than hanging it.
func (m *Manager) backfillStatus(ctx context.Context) BackfillStatus {
	if m.waRepo == nil {
		return BackfillStatus{}
	}

	readCtx, cancel := context.WithTimeout(ctx, backfillReadTimeout)
	defer cancel()

	counts, countErr := m.waRepo.CountByStateAndDisposition(readCtx)
	floor, floorErr := m.waRepo.ObservedFloor(readCtx)
	if countErr != nil || floorErr != nil {
		if countErr != nil {
			logger.Warn().Err(countErr).Msg("whatsapp: failed to read backfill counts")
		}
		if floorErr != nil {
			logger.Warn().Err(floorErr).Msg("whatsapp: failed to read observed backfill floor")
		}
		out := BackfillStatus{}
		if cached := m.backfill.Load(); cached != nil {
			out = cached.Val
		}
		// Never a fabricated zero presented as fresh.
		out.Stale = true
		return out
	}

	var out BackfillStatus
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
	out.ObservedFloorAt = floor
	m.backfill.Store(&cachedBackfill{Val: out})
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
