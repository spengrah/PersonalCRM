package whatsapp

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/logger"
	"personal-crm/backend/internal/repository"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"
)

// phoneRegex is the same E.164 shape the Telegram handler validates.
var phoneRegex = regexp.MustCompile(`^\+[0-9]{7,15}$`)

// forcedClearWarning is what a forced unlink tells the user. Forcing contacts
// WhatsApp not at all, so it may well still be linked.
const forcedClearWarning = "Local credentials were cleared without contacting WhatsApp. Unlink this device from your phone's Linked Devices screen to complete the disconnect."

// pairingState is the single in-flight pairing. There is no session token:
// WhatsApp pairing has no user-supplied step to correlate, so at most one
// pairing runs at a time and its state is read back through GET /auth/status.
//
// Every field is loop-owned — written and read only inside a turn — except
// ctx/cancel/drainDone, which are written once by the turn that claims the slot
// (before any effect exists) and only read afterwards. That ordering is what
// makes every teardown path able to cancel the attempt whatever stage it has
// reached.
type pairingState struct {
	method    string
	phone     string
	expiresAt time.Time

	qrCode   *string
	pairCode *string
	sess     *session
	// cancelled records that this attempt was torn down. It is what stops a scan
	// that completed after the user pressed cancel from being adopted, and it is
	// checked by the fence, so a continuation deciding about a cancelled attempt
	// aborts.
	cancelled bool
	// first records that the first QR item has been seen, which is the library's
	// documented signal that the connection is fully established.
	first bool

	// prevState/prevReason are what the status said before this attempt claimed
	// the slot. An attempt that ends without pairing has to put that back: a
	// re-pair started over a durable "logged out" decision must not overwrite it
	// with "not paired", which reads as a clean slate and hides the reason the
	// user was re-pairing.
	prevState  string
	prevReason string

	ctx       context.Context
	cancel    context.CancelFunc
	drainDone chan struct{}
	// drainStarted records whether a drain goroutine exists. Nothing waits on
	// drainDone unless it is true — a cancel during the session build must not
	// pay a five-second teardown for a goroutine that does not exist.
	drainStarted bool

	// ready carries the first-code outcome to the caller. Buffered (capacity 1)
	// and sent non-blockingly, so a caller that gave up cannot wedge a turn.
	ready chan pairOutcome
}

// displaced is the status an ended attempt puts back. A not_ready recorded
// before the manager started is not a fact about the link — the attempt reached
// the slot by PASSING the readiness gate — so it is the one value not restored.
func (p *pairingState) displaced() (state, reason string) {
	if p.prevState == "" || p.prevState == StateNotReady {
		return StateNotPaired, ""
	}
	return p.prevState, p.prevReason
}

// pairOutcome is the first-code result StartPairing's caller waits for.
type pairOutcome struct{ err error }

// startHandle is what opStartPairing replies immediately. The attempt is
// detached from the request by design — the user has minutes to scan — so the
// caller waits off the loop and the attempt outlives it.
type startHandle struct {
	p     *pairingState
	ready <-chan pairOutcome
}

// owns reports whether the given session is this attempt's live client. A nil
// session means the caller did not identify one (tests only); a cancelled
// attempt owns nothing.
func (p *pairingState) owns(sess *session) bool {
	if p.cancelled {
		return false
	}
	return sess == nil || p.sess == sess
}

// expired reports whether the pairing's TTL has elapsed. An expired pairing must
// not block a new one: its QR codes have run out and its pair code is no longer
// accepted, so refusing a fresh attempt would wedge the settings page until
// someone thought to press cancel.
func (p *pairingState) expired(now time.Time) bool {
	return !now.Before(p.expiresAt)
}

func (p *pairingState) snapshot() Pairing {
	out := Pairing{Method: p.method, ExpiresAt: p.expiresAt}
	if p.qrCode != nil {
		code := *p.qrCode
		out.QRCode = &code
	}
	if p.pairCode != nil {
		code := *p.pairCode
		out.PairCode = &code
	}
	return out
}

// sendPairOutcome is non-blocking: the channel has capacity 1 and only the first
// outcome matters.
func sendPairOutcome(p *pairingState, err error) {
	if p == nil || p.ready == nil {
		return
	}
	select {
	case p.ready <- pairOutcome{err: err}:
	default:
	}
}

// --- StartPairing ----------------------------------------------------------

// StartPairing begins a pairing attempt by QR code or phone code.
//
// One immediate turn claims the slot and replies with a handle; the caller then
// waits OFF the loop for the first code, with the timer where it has always
// been — in the caller — so the actor needs no timer wheel.
func (m *Manager) StartPairing(ctx context.Context, req PairRequest) error {
	res := m.runOp(m.opStartPairing(ctx, req))
	if res.err != nil {
		return res.err
	}
	h, _ := res.val.(*startHandle)
	if h == nil {
		return ErrManagerStopped
	}

	// Wall-clock, deliberately: a bounded wait for a websocket is real time. The
	// TTL comparison on pairingState.expiresAt stays on the accelerated clock.
	timer := time.NewTimer(qrFirstCodeTimeout)
	defer timer.Stop()

	select {
	case out := <-h.ready:
		// Same barrier as runOp's: the outcome is sent from inside its turn, so
		// without this a caller could read a status its own attempt had already
		// changed.
		m.settle()
		return out.err
	case <-timer.C:
		// By IDENTITY, never "whatever is in the slot": another attempt may have
		// taken it over in the meantime.
		m.abandonAttempt(h.p, ErrQRCodeTimeout)
		return ErrQRCodeTimeout
	case <-ctx.Done():
		m.abandonAttempt(h.p, ErrQRCodeTimeout)
		return ErrQRCodeTimeout
	case <-m.stopping:
		return ErrManagerStopped
	}
}

// abandonAttempt ends a specific attempt through a turn.
func (m *Manager) abandonAttempt(p *pairingState, cause error) {
	m.runOp(func(st *actorState, reply chan opResult) {
		m.abandonPairingTurn(st, p, cause)
		reply <- opResult{}
	})
}

func (m *Manager) opStartPairing(ctx context.Context, req PairRequest) func(*actorState, chan opResult) {
	return func(st *actorState, reply chan opResult) {
		// The readiness gate is applied here as well as in Start: pairing that
		// bypassed it would produce a connected, manual-history client with no
		// durable sink, which is the exact failure the gate exists to prevent.
		if ready, missing := st.ready(); !ready {
			st.status.State = StateNotReady
			st.status.Reason = ReasonIngestNotWired
			st.status.Missing = missing
			logger.Info().Str("missing", missing).Msg("whatsapp: pairing refused, integration not ready")
			reply <- opResult{err: ErrIngestNotWired}
			return
		}

		st.status.Missing = ""

		switch req.Method {
		case PairMethodQR:
		case PairMethodPhone:
			if !phoneRegex.MatchString(req.Phone) {
				reply <- opResult{err: ErrInvalidPhone}
				return
			}
		default:
			reply <- opResult{err: ErrUnknownPairMethod}
			return
		}

		// Link and unlink are mutually exclusive. Both flags are read and written
		// only by turns, and both operations can only BEGIN in a turn, so the two
		// can never overlap — which removes the interleaving rather than fencing
		// it, the only honest response to a hazard whose evidence (the library's
		// own device-row write) lives outside our state.
		if st.unlinkInFlight {
			reply <- opResult{err: ErrUnlinkInProgress}
			return
		}

		if st.sess != nil && st.sess.client.IsConnected() && st.sess.client.IsLoggedIn() {
			reply <- opResult{err: ErrAlreadyConnected}
			return
		}

		now := accelerated.GetCurrentTime()
		var stale *pairingState
		if st.pairing != nil {
			if !st.pairing.expired(now) {
				reply <- opResult{err: ErrPairingInProgress}
				return
			}
			stale = st.pairing
			st.pairing = nil
		}

		p := &pairingState{
			method:    req.Method,
			phone:     req.Phone,
			expiresAt: now.Add(authTTL),
			ready:     make(chan pairOutcome, 1),
			drainDone: make(chan struct{}),
		}
		// Created HERE, in the claiming turn, before any effect exists: from this
		// instant every teardown path can cancel the attempt whatever stage it
		// has reached. The pairing outlives the request that started it, so its
		// context is detached from the caller's.
		p.ctx, p.cancel = context.WithCancel(context.WithoutCancel(ctx))

		// Captured BEFORE the attempt overwrites it, and only when this attempt
		// is the one displacing a real status — a second attempt taking over
		// from a first must inherit what the first displaced, not "pairing".
		if st.status.State == StatePairing && stale != nil {
			p.prevState, p.prevReason = stale.prevState, stale.prevReason
		} else {
			p.prevState, p.prevReason = st.status.State, st.status.Reason
		}

		st.pairing = p
		st.status.State = StatePairing
		st.status.Reason = ""

		if stale != nil {
			m.retireAttempt(st, stale, ErrPairingCancelled)
		}

		reply <- opResult{val: &startHandle{p: p, ready: p.ready}}

		// A FRESH unpaired device: pairing over the stored one would corrupt an
		// existing link.
		m.launch(st,
			[]effect{buildSessionEffect{build: st.newSession, req: sessionRequest{fresh: true}}},
			launchOneShot,
			fence{sess: st.sess, pairing: p},
			opFlags{pairing: p},
			func(st *actorState, res effectResult) bool { return m.contPairingSessionBuilt(st, res, p) },
			func(err error) { sendPairOutcome(p, err) },
		)
	}
}

func (m *Manager) contPairingSessionBuilt(st *actorState, res effectResult, p *pairingState) bool {
	if err := res.firstErr(); err != nil {
		m.abandonPairingTurn(st, p, err)
		return false
	}
	sess := res.step(0).sess
	if sess == nil {
		m.abandonPairingTurn(st, p, ErrPairingCancelled)
		return false
	}
	p.sess = sess

	// GetQRChannel BEFORE Connect: the library requires that order. A cancel
	// arriving anywhere inside this batch cancels p.ctx immediately, so the
	// library's QR handler unwinds without waiting for Connect to return.
	m.launchDial(st, sess)
	m.launch(st,
		[]effect{
			openQRChannelEffect{sess: sess, pairCtx: p.ctx},
			connectEffect{sess: sess},
		},
		launchOneShot,
		fence{sess: st.sess, pairing: p},
		opFlags{pairing: p},
		func(st *actorState, res effectResult) bool { return m.contPairingConnected(st, res, p) },
		func(err error) { sendPairOutcome(p, err) },
	)
	return true
}

func (m *Manager) contPairingConnected(st *actorState, res effectResult, p *pairingState) bool {
	if err := res.firstErr(); err != nil {
		m.abandonPairingTurn(st, p, err)
		return false
	}
	ch := res.step(0).qr
	if ch == nil {
		m.abandonPairingTurn(st, p, ErrPairingCancelled)
		return false
	}
	if m.launch(st, []effect{drainQRChannelEffect{m: m, p: p, ch: ch}}, launchLong, fence{}, opFlags{}, nil, nil) {
		p.drainStarted = true
	}
	return true
}

// onQRItem handles one item from the QR drain. It is fenced by the ordinary
// pairing-slot check: an item from an attempt that no longer holds the slot
// speaks for nothing.
func (m *Manager) onQRItem(st *actorState, p *pairingState, item whatsmeow.QRChannelItem) {
	if st.pairing != p || p.cancelled {
		return
	}

	switch item.Event {
	case whatsmeow.QRChannelEventCode:
		// A phone-code attempt also receives QR codes; they are not the user's
		// affordance there, so they are not stored.
		if p.method == PairMethodQR {
			code := item.Code
			p.qrCode = &code
		}
		if p.first {
			return
		}
		p.first = true
		if p.method == PairMethodQR {
			// The code is now in the published snapshot, so the caller's Status()
			// carries it.
			sendPairOutcome(p, nil)
			return
		}
		// PairPhone is called only after the first QR item proves the connection
		// is established — the library's own documented precondition.
		m.launch(st,
			[]effect{pairPhoneEffect{sess: p.sess, phone: p.phone, ctx: p.ctx}},
			launchOneShot,
			fence{sess: st.sess, pairing: p},
			opFlags{pairing: p},
			func(st *actorState, res effectResult) bool { return m.contPairCode(st, res, p) },
			func(err error) { sendPairOutcome(p, err) },
		)

	case whatsmeow.QRChannelSuccess.Event:
		// Pairing completed; the PairSuccess event on the already-attached
		// handler is what clears the pairing state, so there is no window in
		// which a pairing completes unobserved.

	case whatsmeow.QRChannelScannedWithoutMultidevice.Event:
		// NOT a failure, whatever its "err-" prefix suggests. The library emits
		// this and keeps going: the QR emitter is a separate goroutine that is
		// still counting down the remaining codes, so the very next item is
		// another scannable code. Ending the attempt here would throw away a
		// pairing the user can finish by flipping one setting on their phone.
		st.status.Reason = ReasonScannedWithoutMultidevice
		logger.Warn().Msg("whatsapp: the code was scanned with multi-device mode off; enable it and scan the next code")

	case whatsmeow.QRChannelEventPasskeyRequest, whatsmeow.QRChannelEventPasskeyResponse:
		// The account wants to finish pairing through a passkey handoff. The
		// library only emits these when the user has to confirm — it answers the
		// automatic case itself — and there is no surface here for that
		// exchange, so the attempt ends with a reason instead of running out of
		// codes while the user waits for a prompt that never comes.
		logger.Warn().Str("event", item.Event).Msg("whatsapp: pairing requires a passkey confirmation, which is not supported")
		m.abandonPairingTurn(st, p, ErrPasskeyPairingUnsupported)
		// After the attempt ends, so the restore does not wipe it: why the
		// pairing the user was watching stopped is the one thing they need.
		st.status.Reason = ReasonPasskeyPairingUnsupported

	default:
		// Everything that is left ends the attempt: timeout, an outdated client,
		// an unexpected connection state, and the error item the library emits
		// for a pairing failure.
		logger.Warn().Str("event", item.Event).Err(item.Error).
			Msg("whatsapp: the pairing attempt ended")
		m.abandonPairingTurn(st, p, ErrPairingCancelled)
	}
}

// onQRClosed ends an attempt whose codes ran out. This is what stops an
// exhausted attempt from occupying the single pairing slot.
func (m *Manager) onQRClosed(st *actorState, p *pairingState) {
	if st.pairing != p {
		return
	}
	m.abandonPairingTurn(st, p, ErrQRCodeTimeout)
}

func (m *Manager) contPairCode(st *actorState, res effectResult, p *pairingState) bool {
	if err := res.firstErr(); err != nil {
		m.abandonPairingTurn(st, p, err)
		return false
	}
	code := res.step(0).code
	p.pairCode = &code
	sendPairOutcome(p, nil)
	return true
}

// abandonPairingTurn ends an attempt: it releases the slot if the attempt still
// holds it, and retires the attempt itself.
func (m *Manager) abandonPairingTurn(st *actorState, p *pairingState, cause error) {
	if p == nil {
		return
	}
	if st.pairing == p {
		st.pairing = nil
		if st.status.State == StatePairing {
			// Back to whatever the attempt displaced, not to a hardcoded blank
			// slate.
			st.status.State, st.status.Reason = p.displaced()
		}
	}
	m.retireAttempt(st, p, cause)
}

// retireAttempt tells the attempt's caller why it ended, then ends it. The
// ending itself is endAttempt's — one act, wherever an attempt dies from.
func (m *Manager) retireAttempt(st *actorState, p *pairingState, cause error) {
	if p == nil || p.cancelled {
		return
	}
	sendPairOutcome(p, cause)
	st.endAttempt(p, false)
}

// CancelPairing aborts an in-flight pairing. It is idempotent.
func (m *Manager) CancelPairing() {
	m.runOp(func(st *actorState, reply chan opResult) {
		if st.pairing != nil {
			m.abandonPairingTurn(st, st.pairing, ErrPairingCancelled)
		}
		reply <- opResult{}
	})
}

// --- Disconnect ------------------------------------------------------------

// Disconnect unlinks the device.
//
// Local credentials are cleared only on positive evidence that the remote
// device is gone. A failed connect, a ban, an outdated client or a replaced
// stream proves nothing about whether the device is still linked, so treating
// any failure as "already unlinked" would orphan a live device on the user's
// phone with no local record of it.
//
// It is a fenced state machine: every publication AND every destructive step
// sits behind a fence check applied by the loop, so no failure path can publish
// over a session it never decided about.
// The context bounds the WAIT, not the unlink. An unlink that has reached the
// server cannot be recalled, so a caller that gives up gets its goroutine back
// and the operation still runs to completion and publishes — the same shape as
// StartPairing, whose attempt also outlives the request that began it.
func (m *Manager) Disconnect(ctx context.Context, force bool) (*DisconnectResult, error) {
	res := m.runOpCtx(ctx, m.opDisconnect(force))
	if res.err != nil {
		return nil, res.err
	}
	out, _ := res.val.(*DisconnectResult)
	return out, nil
}

func (m *Manager) opDisconnect(force bool) func(*actorState, chan opResult) {
	return func(st *actorState, reply chan opResult) {
		// Serializing unlinks is not new caution: two overlapping unlinks were
		// always incoherent — two Logouts, two publications, one device.
		if st.unlinkInFlight {
			reply <- opResult{err: ErrUnlinkInProgress}
			return
		}
		// Unlink and pairing are mutually exclusive (A2.5 part 1). This refusal
		// is what makes it impossible for a purge to delete a device row the
		// library saved for a pairing whose PairSuccess is still in the mailbox.
		if st.pairing != nil {
			reply <- opResult{err: ErrPairingInProgress}
			return
		}

		state, reason := st.status.State, st.status.Reason

		// Retiring a session always means removing it from its slot in the same
		// turn: a retired session is never left in st.sess. The fence this
		// operation captures is therefore always sess:nil, which is why the
		// retired-check cannot make its own continuations abort.
		//
		// whatsmeow starts auto-reconnect on a remote disconnect, so a session
		// that merely looks down is very likely mid-retry; retiring and
		// disconnecting it before building a second client is what stops two
		// clients racing on the same device.
		st.unlinkInFlight = true
		rel := opFlags{unlink: true}
		// Ownership ends now; the RELEASE is handed to the operation, because a
		// remote logout still needs the socket. The loop releases it when the
		// operation ends, on every path including abort — which is what stops a
		// failed logout leaving a connected client nobody owns.
		sess := st.sess
		st.retireFor(&rel, sess)
		abort := func(err error) { reply <- opResult{err: err} }

		switch {
		case force:
			// The only path that clears local state without server confirmation,
			// and it says so in its result. It never has to resolve a device,
			// which is what makes it the working remedy for an ambiguous store.
			m.launchPurge(st, sess, rel, reply,
				&DisconnectResult{Forced: true, Warning: forcedClearWarning},
				ReasonForcedCleanupFailed)

		case serverConfirmedUnlinked(state, reason):
			// Positive evidence that the remote device is already gone: WhatsApp
			// said so, or a previous unlink completed remotely and only the local
			// clear failed. Neither is a guess.
			m.launchPurge(st, sess, rel, reply,
				&DisconnectResult{AlreadyUnlinked: true},
				ReasonLocalCleanupFailed)

		case sess != nil && sess.client.IsConnected():
			m.launch(st, []effect{logoutEffect{sess: sess}}, launchOneShot, fence{}, rel,
				func(st *actorState, res effectResult) bool {
					return m.contLogout(st, res, sess, rel, reply)
				}, abort)

		default:
			// No live client and no logged_out proof. Build one solely to log
			// out: Start declines to RESUME INGESTING on a terminal device, which
			// is a different question from a user explicitly asking to unlink one.
			// The old client is released FIRST, in the same batch, so it is
			// stopped before a second client for the same device is even built
			// — whatsmeow auto-reconnects a remote disconnect, and two clients
			// on one device race the unlink against a reconnect. The
			// operation's own release at the end is then an idempotent repeat.
			m.launch(st, []effect{
				releaseSessionEffect{release: sessionRelease{sess: sess}, drainWait: m.timeouts.drainDrain},
				buildSessionEffect{build: st.newSession, req: sessionRequest{linked: st.linkedJID}},
			}, launchOneShot, fence{}, rel,
				func(st *actorState, res effectResult) bool {
					return m.contUnlinkSessionBuilt(st, res, rel, reply)
				}, abort)
		}
	}
}

func (m *Manager) contLogout(st *actorState, res effectResult, sess *session, rel opFlags, reply chan opResult) bool {
	err := res.firstErr()
	if err == nil {
		m.launchPurge(st, sess, rel, reply, &DisconnectResult{RemoteUnlinked: true}, ReasonLocalCleanupFailed)
		return true
	}
	if !logoutFailedAfterRemoteUnlink(err) {
		m.recordDisconnectFailure(st, err, reply)
		return false
	}
	// The device IS unlinked server-side; only the library's own local delete
	// failed. Telling the user to retry the unlink would be wrong twice over.
	logger.Warn().Err(err).Msg("whatsapp: remote unlink succeeded but the library's local delete failed; clearing locally on retry")
	m.launchPurge(st, sess, rel, reply, &DisconnectResult{RemoteUnlinked: true}, ReasonLocalCleanupFailed)
	return true
}

func (m *Manager) contUnlinkSessionBuilt(st *actorState, res effectResult, rel opFlags, reply chan opResult) bool {
	if err := res.firstErr(); err != nil {
		m.recordDisconnectFailure(st, err, reply)
		return false
	}
	built := res.step(1).sess
	if built == nil || !built.paired {
		// Retirement is the only way a session dies, on this path too: the
		// client was built, so something has to close it.
		st.retire(built, false)
		reply <- opResult{err: ErrNotPaired}
		return false
	}
	// The built client is live from here, so its release joins the operation's:
	// however the operation ends, including abort on the fence, the loop closes
	// it. A Stop that cancels the operation outright closes it too, by ending
	// the effect context this connection descends from. Copied rather than
	// appended in place, because the flags this operation was launched with are
	// still held by the continuation that produced this turn.
	rel.holds = append([]*session(nil), rel.holds...)
	st.retireFor(&rel, built)

	m.launchDial(st, built)
	m.launch(st, []effect{
		connectEffect{sess: built},
		logoutEffect{sess: built},
	}, launchOneShot, fence{}, rel,
		func(st *actorState, res effectResult) bool {
			return m.contUnlinkLoggedOut(st, res, built, rel, reply)
		},
		func(err error) { reply <- opResult{err: err} })
	return true
}

func (m *Manager) contUnlinkLoggedOut(st *actorState, res effectResult, built *session, rel opFlags, reply chan opResult) bool {
	err := res.firstErr()
	if err == nil {
		m.launchPurge(st, built, rel, reply, &DisconnectResult{RemoteUnlinked: true}, ReasonLocalCleanupFailed)
		return true
	}
	if !logoutFailedAfterRemoteUnlink(err) {
		// A failed connect is not evidence of anything, so the device is KEPT.
		// The client itself is released by the operation ending; whatsmeow
		// leaves a failed logout CONNECTED, so nothing here may skip that.
		m.recordDisconnectFailure(st, err, reply)
		return false
	}
	logger.Warn().Err(err).Msg("whatsapp: remote unlink succeeded but the library's local delete failed; clearing locally on retry")
	m.launchPurge(st, built, rel, reply, &DisconnectResult{RemoteUnlinked: true}, ReasonLocalCleanupFailed)
	return true
}

// launchPurge is stage a of the staged purge: a NON-destructive enumeration.
// Aborting on the fence after it destroys nothing.
func (m *Manager) launchPurge(st *actorState, sess *session, rel opFlags, reply chan opResult, result *DisconnectResult, failReason string) {
	m.launch(st, []effect{
		releaseSessionEffect{release: sessionRelease{sess: sess}, drainWait: m.timeouts.drainDrain},
		listDevicesEffect{list: st.devices.list},
	}, launchOneShot, fence{}, rel,
		func(st *actorState, res effectResult) bool {
			return m.contPurgeFenced(st, res, rel, reply, result, failReason)
		},
		func(err error) { reply <- opResult{err: err} })
}

// contPurgeFenced is stage b: the fence is checked BEFORE anything is deleted,
// and the enumeration set is frozen onto the operation.
func (m *Manager) contPurgeFenced(st *actorState, res effectResult, rel opFlags, reply chan opResult, result *DisconnectResult, failReason string) bool {
	if err := res.firstErr(); err != nil {
		// A store the purge cannot READ is a cleanup that did not happen. The
		// credentials are still there, so publishing not_paired would tell the
		// user the integration is clean while the next boot would resume the very
		// device they asked to remove.
		m.failLocalCleanup(st, failReason, fmt.Errorf("read device store: %w", err), reply)
		return false
	}
	frozen := res.step(1).jids

	m.launch(st, []effect{
		deleteDevicesEffect{del: st.devices.deleteAll, jids: frozen},
		listDevicesEffect{list: st.devices.list},
	}, launchOneShot, fence{}, rel,
		func(st *actorState, res effectResult) bool {
			return m.contDisconnect(st, res, frozen, reply, result, failReason)
		},
		func(err error) { reply <- opResult{err: err} })
	return true
}

// contDisconnect is stage d: the whole publication, as one turn. The metadata
// clear and the status='disabled' write cannot be separated even in principle,
// because the turn does not end between them.
func (m *Manager) contDisconnect(st *actorState, res effectResult, frozen []types.JID, reply chan opResult, result *DisconnectResult, failReason string) bool {
	if err := res.firstErr(); err != nil {
		m.failLocalCleanup(st, failReason, err, reply)
		return false
	}

	remaining := res.step(1).jids

	// Verification is "every frozen JID is gone", not "zero rows remain" — the
	// second formulation is delete-what-you-find wearing a different hat.
	for _, jid := range frozen {
		if containsJID(remaining, jid) {
			m.failLocalCleanup(st, failReason, fmt.Errorf("device %s is still stored", jid), reply)
			return false
		}
	}
	// A row that appeared AFTER the enumeration is a supersession, never a clean
	// not_paired — and it is deliberately not deleted: the purge never deletes a
	// row it did not observe before its last fence.
	for _, jid := range remaining {
		if !containsJID(frozen, jid) {
			logger.Warn().Str("jid", jid.String()).
				Msg("whatsapp: a device appeared while the unlink was in flight; not publishing its outcome")
			reply <- opResult{err: ErrOperationSuperseded}
			return false
		}
	}

	m.applyLocalClear(st)
	reply <- opResult{val: result}
	return false
}

// applyLocalClear is the whole successful-unlink transition, in one turn.
func (m *Manager) applyLocalClear(st *actorState) {
	st.retire(st.sess, false)
	st.linkedJID = nil
	st.status.State = StateNotPaired
	st.status.Reason = ""
	st.status.JID = nil
	st.status.PhoneNumber = nil
	st.status.PushName = nil
	st.status.ConnectedAt = nil
	st.status.BannedUntil = nil
	// These are only meaningful alongside a terminal state or a live link;
	// leaving them set would report a stale verdict about an account that is no
	// longer there.
	st.status.TerminalReasonPersisted = nil
	st.status.LinkSelectorPersisted = nil
	st.status.ReplacedDeviceRetained = false

	m.clearTerminalReason(st)
	m.updateSyncStatus(st, repository.SyncStatusDisabled, "")
}

// failLocalCleanup publishes a cleanup that did not happen. The credentials are
// still stored, so the status must not say not_paired, and the caller gets a
// distinct error: the remedy is completing the local clear, not retrying an
// unlink.
//
// failReason encodes what evidence the caller had: only a caller that actually
// confirmed the remote unlink may leave behind a state that a later disconnect
// reads as "already unlinked".
func (m *Manager) failLocalCleanup(st *actorState, failReason string, cause error, reply chan opResult) {
	st.status.State = StateDisconnectFailed
	st.status.Reason = failReason
	m.updateSyncStatus(st, repository.SyncStatusError, cause.Error())
	logger.Error().Err(cause).Msg("whatsapp: local credentials could not be cleared; the device is still stored")
	reply <- opResult{err: errors.Join(ErrLocalCleanupFailed, cause)}
}

// recordDisconnectFailure keeps the device and reports the failure so the user
// can retry or force. It is a continuation body, so it cannot run without having
// passed the fence.
func (m *Manager) recordDisconnectFailure(st *actorState, cause error, reply chan opResult) {
	st.status.State = StateDisconnectFailed
	st.status.Reason = cause.Error()
	m.updateSyncStatus(st, repository.SyncStatusError, cause.Error())
	logger.Warn().Err(cause).Msg("whatsapp: remote unlink failed; local credentials kept")
	reply <- opResult{err: errors.Join(ErrRemoteUnlinkFailed, cause)}
}

func containsJID(list []types.JID, jid types.JID) bool {
	for _, candidate := range list {
		if candidate.ToNonAD() == jid.ToNonAD() {
			return true
		}
	}
	return false
}

// logoutStoreDeleteMarker is how the library reports that the REMOTE unlink
// already succeeded and only its own local store delete failed.
//
// Logout sends the unlink IQ, then disconnects, then deletes the local store,
// and wraps the two failures differently: a failed IQ becomes "error sending
// logout request: …" and a failed delete becomes "error deleting data from
// store: …". The second is the only shape that means the device is gone
// server-side, and the library exposes no sentinel for it.
const logoutStoreDeleteMarker = "error deleting data from store"

// logoutFailedAfterRemoteUnlink reports whether a Logout error arrived AFTER the
// remote unlink had already succeeded. Anything unrecognised is treated as a
// failed remote unlink, which is the conservative answer: it keeps the device.
func logoutFailedAfterRemoteUnlink(err error) bool {
	// PREFIX, not substring. The marker is the wrapper whatsmeow puts on the
	// LOCAL store delete it performs after the remote logout succeeded, so it is
	// always the front of the message. A substring match would also accept a
	// server-supplied error that happened to contain the phrase, and this
	// predicate decides whether to destroy local credentials.
	return err != nil && strings.HasPrefix(err.Error(), logoutStoreDeleteMarker)
}

// serverConfirmedUnlinked reports whether the manager already holds positive
// evidence that the remote device is gone, so a further remote call would be
// pointless (and, against an unlinked device, would fail).
//
// A FORCED clear that failed does not qualify: it made no remote call, so it
// learned nothing about the remote device, and a later unlink must still try.
func serverConfirmedUnlinked(state, reason string) bool {
	return (state == StateDisconnected && reason == ReasonLoggedOut) ||
		(state == StateDisconnectFailed && reason == ReasonLocalCleanupFailed)
}
