package whatsapp

import (
	"context"
	"errors"
	"regexp"
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
		m.abandonPairing(ctx, nil)
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
	if pairing == nil || m.pairing == pairing {
		m.pairing = nil
	}
	if m.status.State == StatePairing {
		m.status.State = StateNotPaired
		m.status.Reason = ""
	}
	m.mu.Unlock()

	if pairing == nil {
		return
	}
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
	m.mu.RLock()
	sess := m.sess
	reason := m.status.Reason
	state := m.status.State
	m.mu.RUnlock()

	// Force is the only path that clears local state without server
	// confirmation, and it says so in its result.
	if force {
		m.clearLocalDevice(ctx, sess)
		return &DisconnectResult{
			Forced:  true,
			Warning: "Local credentials were cleared without contacting WhatsApp. Unlink this device from your phone's Linked Devices screen to complete the disconnect.",
		}, nil
	}

	// The one server-confirmed "already unlinked" signal: whatsmeow raises
	// LoggedOut specifically for a stream error or connect failure that means
	// unpaired. Nothing else qualifies.
	if state == StateDisconnected && reason == ReasonLoggedOut {
		m.clearLocalDevice(ctx, sess)
		return &DisconnectResult{AlreadyUnlinked: true}, nil
	}

	// A live client can log out directly.
	if sess != nil && sess.client.IsConnected() {
		if err := sess.client.Logout(ctx); err != nil {
			m.recordDisconnectFailure(ctx, err)
			return nil, errors.Join(ErrRemoteUnlinkFailed, err)
		}
		m.clearLocalDevice(ctx, sess)
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
		sess.client.Disconnect()
		m.mu.Lock()
		if m.sess == sess {
			m.sess = nil
		}
		m.mu.Unlock()
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
		sess.client.Disconnect()
		m.recordDisconnectFailure(ctx, err)
		return nil, errors.Join(ErrRemoteUnlinkFailed, err)
	}
	m.clearLocalDevice(ctx, sess)
	return &DisconnectResult{RemoteUnlinked: true}, nil
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
func (m *Manager) clearLocalDevice(ctx context.Context, sess *session) {
	if sess == nil {
		if built, err := m.newSession(ctx, false); err == nil {
			sess = built
		}
	}
	if sess != nil {
		sess.client.Disconnect()
		if sess.deleteDevice != nil {
			if err := sess.deleteDevice(ctx); err != nil {
				logger.Warn().Err(err).Msg("whatsapp: failed to delete local device")
			}
		}
	}

	m.mu.Lock()
	if m.pairing != nil {
		m.pairing.cancel()
		m.pairing = nil
	}
	m.sess = nil
	m.status.State = StateNotPaired
	m.status.Reason = ""
	m.status.JID = nil
	m.status.PhoneNumber = nil
	m.status.PushName = nil
	m.status.ConnectedAt = nil
	m.status.BannedUntil = nil
	m.mu.Unlock()

	m.clearTerminalReason(ctx)
	m.updateSyncStatus(ctx, repository.SyncStatusDisabled, "")
}
