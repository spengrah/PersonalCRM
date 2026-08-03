package whatsapp

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/logger"
	"personal-crm/backend/internal/repository"

	"go.mau.fi/whatsmeow"
)

// phoneRegex is the same E.164 shape the Telegram handler validates.
var phoneRegex = regexp.MustCompile(`^\+[0-9]{7,15}$`)

// pairingState is the single in-flight pairing. There is no session token:
// WhatsApp pairing has no user-supplied step to correlate, so at most one
// pairing runs at a time and its state is read back through GET /auth/status.
type pairingState struct {
	method    string
	expiresAt time.Time

	// mu guards every mutable field below. The pairing is published into
	// Manager.pairing before its session and cancel func are attached, so a
	// concurrent Stop() or CancelPairing() can observe it mid-construction.
	mu       sync.Mutex
	qrCode   *string
	pairCode *string
	cancelFn context.CancelFunc
	sess     *session
	// cancelled records that this attempt was torn down. It exists because the
	// attempt is published into Manager.pairing BEFORE its session is built:
	// without it, a cancel landing in that window would clear the slot and the
	// original goroutine would then attach and connect an orphaned client that
	// nothing owns and Stop() cannot reach.
	cancelled bool
}

// attach binds the session and cancel func to the attempt. It returns false when
// the attempt was cancelled while the session was being built, in which case the
// caller must discard the session rather than connect it.
func (p *pairingState) attach(sess *session, cancel context.CancelFunc) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cancelled {
		return false
	}
	p.sess = sess
	p.cancelFn = cancel
	return true
}

// owns reports whether the given session is this attempt's live client. A nil
// session means the caller did not identify one (tests only); a cancelled
// attempt owns nothing, which is what stops a scan that completed after the
// user pressed cancel from being adopted.
func (p *pairingState) owns(sess *session) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cancelled {
		return false
	}
	return sess == nil || p.sess == sess
}

// markCancelled closes the attempt to any further attachment and reports the
// session that still needs tearing down, if one was attached.
func (p *pairingState) markCancelled() *session {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cancelled = true
	return p.sess
}

func (p *pairingState) session() *session {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.sess
}

// expired reports whether the pairing's TTL has elapsed. An expired pairing
// must not block a new one: its QR codes have run out and its pair code is no
// longer accepted, so refusing a fresh attempt would wedge the settings page
// until someone thought to press cancel.
func (p *pairingState) expired(now time.Time) bool {
	return !now.Before(p.expiresAt)
}

func (p *pairingState) setPairCode(code string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.pairCode = &code
}

func (p *pairingState) snapshot() Pairing {
	p.mu.Lock()
	defer p.mu.Unlock()
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

func (p *pairingState) setQRCode(code string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.qrCode = &code
}

func (p *pairingState) cancel() {
	p.mu.Lock()
	cancel := p.cancelFn
	p.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// StartPairing begins a pairing attempt by QR code or phone code.
//
// The readiness gate is applied here as well as in Start: pairing that bypassed
// it would produce a connected, manual-history client with no durable sink,
// which is the exact failure the gate exists to prevent.
func (m *Manager) StartPairing(ctx context.Context, req PairRequest) error {
	if ready, missing := m.Ready(); !ready {
		m.setStatus(func(s *Status) {
			s.State = StateNotReady
			s.Reason = ReasonIngestNotWired
			s.Missing = missing
		})
		logger.Info().Str("missing", missing).Msg("whatsapp: pairing refused, integration not ready")
		return ErrIngestNotWired
	}

	switch req.Method {
	case PairMethodQR:
	case PairMethodPhone:
		if !phoneRegex.MatchString(req.Phone) {
			return ErrInvalidPhone
		}
	default:
		return ErrUnknownPairMethod
	}

	now := accelerated.GetCurrentTime()

	m.mu.Lock()
	// An expired attempt is taken over rather than treated as a conflict.
	var stale *pairingState
	if m.pairing != nil {
		if !m.pairing.expired(now) {
			m.mu.Unlock()
			return ErrPairingInProgress
		}
		stale = m.pairing
		m.pairing = nil
	}
	if m.sess != nil && m.sess.client.IsConnected() && m.sess.client.IsLoggedIn() {
		m.mu.Unlock()
		return ErrAlreadyConnected
	}
	// Claim the slot before doing anything slow, so a concurrent request loses
	// the race rather than building a second client.
	pairing := &pairingState{
		method:    req.Method,
		expiresAt: now.Add(authTTL),
	}
	m.pairing = pairing
	m.bumpGenLocked()
	m.status.State = StatePairing
	m.status.Reason = ""
	m.mu.Unlock()

	// Tear the taken-over attempt down outside the lock, now that the slot
	// belongs to the new one.
	if stale != nil {
		m.teardownPairing(ctx, stale)
	}

	// A fresh unpaired device: pairing over the stored one would corrupt an
	// existing link.
	sess, err := m.newSession(ctx, true)
	if err != nil {
		// By identity, never "whatever is in the slot": between claiming it and
		// failing here, another attempt may have taken it over.
		m.abandonPairing(ctx, pairing)
		return err
	}

	// The pairing outlives the HTTP request that started it — the user has
	// minutes to scan — so its context is detached from the caller's. The
	// bounded wait for the first code still honours the caller's context, so a
	// client that hangs up does not hold the request goroutine.
	pairCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	if !pairing.attach(sess, cancel) {
		// Cancelled while the session was being built. Discard it here — it was
		// never connected and nothing else holds a reference to it.
		cancel()
		m.discardSession(ctx, sess)
		return ErrPairingCancelled
	}

	if err := m.runPairingFlow(ctx, pairCtx, pairing, req.Phone); err != nil {
		m.abandonPairing(ctx, pairing)
		return err
	}
	return nil
}

// discardSession drops a client that was built but never adopted.
func (m *Manager) discardSession(ctx context.Context, sess *session) {
	if sess == nil {
		return
	}
	sess.client.Disconnect()
	if sess.deleteDevice != nil {
		if err := sess.deleteDevice(ctx); err != nil {
			logger.Warn().Err(err).Msg("whatsapp: failed to delete discarded pairing device")
		}
	}
}

// runPairingFlow drives both pairing methods, which share their whole prologue.
//
// The QR channel is opened BEFORE Connect because the library requires that
// order, and BOTH methods then wait for its first item. For QR pairing that item
// is the code the user scans. For phone pairing it is the library's documented
// signal that the connection is fully established — PairPhone's doc says to wait
// for the first QR-channel item (or *events.QR) before calling it, since QR codes
// are generated for code pairing too and simply ignored. Calling PairPhone
// straight after Connect races the handshake.
func (m *Manager) runPairingFlow(reqCtx, pairCtx context.Context, pairing *pairingState, phone string) error {
	sess := pairing.session()
	if sess == nil {
		return ErrPairingCancelled
	}

	qrChan, err := sess.client.GetQRChannel(pairCtx)
	if err != nil {
		return err
	}
	if err := sess.client.Connect(); err != nil {
		return err
	}

	first := make(chan struct{}, 1)
	go m.runQRChannel(pairCtx, pairing, qrChan, first)

	timer := time.NewTimer(qrFirstCodeTimeout)
	defer timer.Stop()

	select {
	case <-first:
	case <-timer.C:
		return ErrQRCodeTimeout
	case <-reqCtx.Done():
		return ErrQRCodeTimeout
	case <-pairCtx.Done():
		return ErrQRCodeTimeout
	}

	if pairing.method == PairMethodQR {
		// runQRChannel already stored the code and keeps it current.
		return nil
	}

	code, err := sess.client.PairPhone(pairCtx, phone, false, whatsmeow.PairClientChrome, pairClientDisplayName)
	if err != nil {
		return err
	}
	pairing.setPairCode(code)
	return nil
}

// runQRChannel drains the QR channel for the life of the pairing. The library
// emits a fresh code every Timeout until the codes run out, so the stored code
// must keep up or the user scans a stale one.
func (m *Manager) runQRChannel(ctx context.Context, pairing *pairingState, qrChan <-chan whatsmeow.QRChannelItem, first chan<- struct{}) {
	sentFirst := false
	for {
		select {
		case <-ctx.Done():
			return
		case item, open := <-qrChan:
			if !open {
				// The library closes the channel once the codes run out.
				// Abandoning here is what stops an exhausted attempt from
				// occupying the single pairing slot.
				m.abandonPairing(context.WithoutCancel(ctx), pairing)
				return
			}
			switch item.Event {
			case whatsmeow.QRChannelEventCode:
				// A phone-code attempt also receives QR codes; they are not the
				// user's affordance there, so they are not stored.
				if pairing.method == PairMethodQR {
					pairing.setQRCode(item.Code)
				}
				if !sentFirst {
					sentFirst = true
					select {
					case first <- struct{}{}:
					default:
					}
				}
			case whatsmeow.QRChannelSuccess.Event:
				// Pairing completed; the PairSuccess event on the already
				// attached handler is what clears the pairing state, so there
				// is no window in which a pairing completes unobserved.
				return
			default:
				logger.Warn().Str("event", item.Event).Msg("whatsapp: QR pairing ended")
				m.abandonPairing(context.WithoutCancel(ctx), pairing)
				return
			}
		}
	}
}

// abandonPairing tears down a failed or terminated pairing and deletes the
// partially written device, mirroring how Telegram removes the session row
// written during key exchange.
func (m *Manager) abandonPairing(ctx context.Context, pairing *pairingState) {
	m.mu.Lock()
	// Only the attempt that still HOLDS the slot may release it or reset the
	// published state. A later attempt has already claimed both, and clearing
	// them on its behalf would abandon a pairing the user is looking at.
	if pairing != nil && m.pairing == pairing {
		m.pairing = nil
		m.bumpGenLocked()
		if m.status.State == StatePairing {
			m.status.State = StateNotPaired
			m.status.Reason = ""
		}
	}
	m.mu.Unlock()

	if pairing == nil {
		return
	}
	// The attempt is torn down whether or not it still held the slot: its client
	// and its half-written device are its own.
	m.teardownPairing(ctx, pairing)
}

// teardownPairing cancels a pairing's goroutine, drops its client, and removes
// the partially written device — mirroring how Telegram removes the session row
// written during key exchange.
func (m *Manager) teardownPairing(ctx context.Context, pairing *pairingState) {
	// Mark first: this is what makes a cancel that lands mid-construction win
	// over the attach that follows it.
	sess := pairing.markCancelled()
	pairing.cancel()
	if sess != nil {
		sess.client.Disconnect()
		if sess.deleteDevice != nil {
			if err := sess.deleteDevice(ctx); err != nil {
				logger.Warn().Err(err).Msg("whatsapp: failed to delete abandoned pairing device")
			}
		}
	}
}

// CancelPairing aborts an in-flight pairing. It is idempotent.
func (m *Manager) CancelPairing() {
	m.mu.RLock()
	pairing := m.pairing
	m.mu.RUnlock()
	if pairing == nil {
		return
	}
	m.abandonPairing(context.Background(), pairing)
}

// Disconnect unlinks the device.
//
// Local credentials are cleared only on positive evidence that the remote
// device is gone. A failed connect, a ban, an outdated client or a replaced
// stream proves nothing about whether the device is still linked, so treating
// any failure as "already unlinked" would orphan a live device on the user's
// phone with no local record of it.
func (m *Manager) Disconnect(ctx context.Context, force bool) (*DisconnectResult, error) {
	// The whole operation decides on this snapshot. gen is captured with it, and
	// re-validated under the lock before every later mutation: an unlink can take
	// seconds (a connect, a remote IQ), and in that time a pairing can be started
	// and adopted. Applying this decision to that session would cancel a pairing
	// the user just completed and orphan its client.
	m.mu.RLock()
	sess := m.sess
	reason := m.status.Reason
	state := m.status.State
	gen := m.gen
	m.mu.RUnlock()

	// Force is the only path that clears local state without server
	// confirmation, and it says so in its result. It can still FAIL: if the
	// device row cannot be deleted, the credentials are still there, and
	// reporting a clean not_paired would be a lie.
	if force {
		if err := m.clearLocalDevice(ctx, sess, ReasonForcedCleanupFailed, gen); err != nil {
			return nil, err
		}
		return &DisconnectResult{
			Forced:  true,
			Warning: "Local credentials were cleared without contacting WhatsApp. Unlink this device from your phone's Linked Devices screen to complete the disconnect.",
		}, nil
	}

	// Positive evidence that the remote device is already gone: WhatsApp said so
	// (LoggedOut), or a previous unlink completed remotely and only the local
	// clear failed. Both skip the remote call; neither is a guess.
	if serverConfirmedUnlinked(state, reason) {
		if err := m.clearLocalDevice(ctx, sess, ReasonLocalCleanupFailed, gen); err != nil {
			return nil, err
		}
		return &DisconnectResult{AlreadyUnlinked: true}, nil
	}

	// A live client can log out directly.
	if sess != nil && sess.client.IsConnected() {
		if err := sess.client.Logout(ctx); err != nil {
			if !logoutFailedAfterRemoteUnlink(err) {
				m.recordDisconnectFailure(ctx, err)
				return nil, errors.Join(ErrRemoteUnlinkFailed, err)
			}
			// The device IS unlinked server-side; only the library's own local
			// delete failed. Telling the user to retry the unlink would be
			// wrong twice over — the unlink worked, and retrying it against an
			// already-unlinked device cannot succeed. Retry the LOCAL clear.
			if clearErr := m.clearLocalDevice(ctx, sess, ReasonLocalCleanupFailed, gen); clearErr != nil {
				return nil, clearErr
			}
			logger.Warn().Err(err).Msg("whatsapp: remote unlink succeeded but the library's local delete failed; cleared locally on retry")
			return &DisconnectResult{RemoteUnlinked: true}, nil
		}
		if err := m.clearLocalDevice(ctx, sess, ReasonLocalCleanupFailed, gen); err != nil {
			return nil, err
		}
		return &DisconnectResult{RemoteUnlinked: true}, nil
	}

	// No live client and no logged_out proof. Build one solely to log out.
	// Start() declines to RESUME INGESTING on a terminal device, which is a
	// different question from a user explicitly asking to unlink one — hence
	// connecting here where Start refused.
	//
	// Retire the installed session FIRST. whatsmeow starts auto-reconnect on a
	// remote disconnect and only Disconnect() marks the drop expected, so a
	// session that merely looks down here is very likely mid-retry. Building a
	// second client without stopping it would put two clients on the same device
	// — the unlink would race a reconnect, and whichever won would leave the
	// other holding a stale socket.
	if sess != nil {
		m.mu.Lock()
		if m.gen != gen {
			m.mu.Unlock()
			return nil, ErrOperationSuperseded
		}
		if m.sess == sess {
			sess.retired = true
			m.sess = nil
			m.bumpGenLocked()
			gen = m.gen
		}
		m.mu.Unlock()
		sess.client.Disconnect()
	}

	sess, err := m.newSession(ctx, false)
	if err != nil {
		m.recordDisconnectFailure(ctx, err)
		return nil, errors.Join(ErrRemoteUnlinkFailed, err)
	}
	if !sess.paired {
		return nil, ErrNotPaired
	}
	if err := sess.client.Connect(); err != nil {
		m.recordDisconnectFailure(ctx, err)
		return nil, errors.Join(ErrRemoteUnlinkFailed, err)
	}
	if err := sess.client.Logout(ctx); err != nil {
		if !logoutFailedAfterRemoteUnlink(err) {
			sess.client.Disconnect()
			m.recordDisconnectFailure(ctx, err)
			return nil, errors.Join(ErrRemoteUnlinkFailed, err)
		}
		if clearErr := m.clearLocalDevice(ctx, sess, ReasonLocalCleanupFailed, gen); clearErr != nil {
			return nil, clearErr
		}
		logger.Warn().Err(err).Msg("whatsapp: remote unlink succeeded but the library's local delete failed; cleared locally on retry")
		return &DisconnectResult{RemoteUnlinked: true}, nil
	}
	if err := m.clearLocalDevice(ctx, sess, ReasonLocalCleanupFailed, gen); err != nil {
		return nil, err
	}
	return &DisconnectResult{RemoteUnlinked: true}, nil
}

// logoutStoreDeleteMarker is how the library reports that the REMOTE unlink
// already succeeded and only its own local store delete failed.
//
// Logout sends the unlink IQ, then disconnects, then deletes the local store,
// and wraps the two failures differently (client.go:715): a failed IQ becomes
// "error sending logout request: …" and a failed delete becomes "error deleting
// data from store: …". The second is the only shape that means the device is
// gone server-side, and the library exposes no sentinel for it.
const logoutStoreDeleteMarker = "error deleting data from store"

// logoutFailedAfterRemoteUnlink reports whether a Logout error arrived AFTER the
// remote unlink had already succeeded. Anything unrecognised is treated as a
// failed remote unlink, which is the conservative answer: it keeps the device.
func logoutFailedAfterRemoteUnlink(err error) bool {
	return err != nil && strings.Contains(err.Error(), logoutStoreDeleteMarker)
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

// recordDisconnectFailure keeps the device and reports the failure so the user
// can retry or force.
func (m *Manager) recordDisconnectFailure(ctx context.Context, err error) {
	m.setStatus(func(s *Status) {
		s.State = StateDisconnectFailed
		s.Reason = err.Error()
	})
	m.updateSyncStatus(ctx, repository.SyncStatusError, err.Error())
	logger.Warn().Err(err).Msg("whatsapp: remote unlink failed; local credentials kept")
}

// clearLocalDevice tears down the session and removes the stored device.
//
// A failed delete is REPORTED, not logged and forgotten: the credentials are
// still on disk, so publishing not_paired would tell the user the integration
// is clean while the next boot would resume the very device they asked to
// remove.
// failReason is the machine-readable reason published if the delete fails. It
// is the caller's to supply because it encodes what evidence the caller had:
// only a caller that actually confirmed the remote unlink may leave behind a
// state that a later disconnect reads as "already unlinked".
func (m *Manager) clearLocalDevice(ctx context.Context, sess *session, failReason string, gen uint64) error {
	// A store we cannot even open is a cleanup that did NOT happen. This is
	// reachable on the ordinary path: after a restart into a persisted logged_out
	// state, Start deliberately installs no session, so both the forced and the
	// server-confirmed clear arrive here with nothing to delete through.
	emptyStore := false
	if sess == nil {
		built, err := m.newSession(ctx, false)
		if err != nil {
			return m.failLocalCleanup(ctx, nil, failReason, fmt.Errorf("open device store: %w", err))
		}
		// A freshly built session over an EMPTY store carries no device row.
		// Deleting it would fail with the library's "device JID must be known",
		// which is not a cleanup failure — there is nothing to clean.
		sess, emptyStore = built, !built.paired
	}

	sess.client.Disconnect()
	if !emptyStore && sess.deleteDevice != nil {
		if err := sess.deleteDevice(ctx); err != nil {
			return m.failLocalCleanup(ctx, sess, failReason, err)
		}
	}

	m.mu.Lock()
	// The device is gone either way, but the STATE transition below speaks for
	// the session this operation decided about. If a pairing was adopted while
	// the delete was in flight, clearing here would retire the new session and
	// drop the pairing slot on behalf of an operation that never saw either.
	if m.gen != gen {
		m.mu.Unlock()
		logger.Warn().Msg("whatsapp: the session changed while the unlink was in flight; not publishing its outcome")
		return ErrOperationSuperseded
	}
	if m.pairing != nil {
		m.pairing.cancel()
		m.pairing = nil
	}
	sess.retired = true
	if m.sess != nil {
		m.sess.retired = true
	}
	m.sess = nil
	m.linkedJID = nil
	m.status.ReplacedDeviceRetained = false
	m.bumpGenLocked()
	m.status.State = StateNotPaired
	m.status.Reason = ""
	m.status.JID = nil
	m.status.PhoneNumber = nil
	m.status.PushName = nil
	m.status.ConnectedAt = nil
	m.status.BannedUntil = nil
	// The field is only meaningful alongside a terminal state; leaving it set
	// would report a stale "the decision was/was not recorded" on a not_paired
	// status that has no decision at all.
	m.status.TerminalReasonPersisted = nil
	m.mu.Unlock()

	m.clearTerminalReason(ctx)
	m.updateSyncStatus(ctx, repository.SyncStatusDisabled, "")
	return nil
}

// failLocalCleanup publishes a cleanup that did not happen. The credentials are
// still stored, so the status must not say not_paired, and the caller gets a
// distinct error: the remedy is completing the local clear, not retrying an
// unlink.
func (m *Manager) failLocalCleanup(ctx context.Context, sess *session, failReason string, cause error) error {
	m.mu.Lock()
	if sess != nil {
		sess.retired = true
		m.bumpGenLocked()
	}
	m.status.State = StateDisconnectFailed
	m.status.Reason = failReason
	m.mu.Unlock()

	m.updateSyncStatus(ctx, repository.SyncStatusError, cause.Error())
	logger.Error().Err(cause).Msg("whatsapp: local credentials could not be cleared; the device is still stored")
	return errors.Join(ErrLocalCleanupFailed, cause)
}
