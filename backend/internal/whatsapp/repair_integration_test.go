//go:build integration_testdb

// Proofs about the whatsmeow device STORE, over a real sqlstore.Container.
//
// They live in this package because the claims are about the library's own
// device table: GetFirstDevice returns the first row of an unordered
// GetAllDevices and documents that it is only for callers that do not want
// multiple sessions. A fake device seam could assert that a delete was
// requested, but not that the store the next boot reads actually holds one row —
// which is the part that decides whether the restart resumes the device the user
// just linked or the one they replaced.
package whatsapp

import (
	"context"
	"errors"
	"testing"

	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/testdb"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	waAdv "go.mau.fi/whatsmeow/proto/waAdv"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

// deviceStoreFixture is a real container over an ephemeral database clone.
type deviceStoreFixture struct {
	ctx       context.Context
	cfg       *config.Config
	container *sqlstore.Container
	syncRepo  *repository.SyncRepository
	waRepo    *repository.WhatsAppRepository
}

func newDeviceStoreFixture(t *testing.T) *deviceStoreFixture {
	t.Helper()
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	ctx := context.Background()
	cloneURL, drop := testdb.NewEphemeralClone(t)
	t.Cleanup(drop)

	cfg := config.TestConfig()
	cfg.Database.URL = cloneURL
	database, err := db.NewDatabase(ctx, cfg.Database)
	require.NoError(t, err)
	t.Cleanup(database.Close)

	container, err := NewDeviceContainer(ctx, database.Pool, NewWALogger("whatsapp-test"))
	require.NoError(t, err)

	return &deviceStoreFixture{
		ctx:       ctx,
		cfg:       cfg,
		container: container,
		syncRepo:  repository.NewSyncRepository(database.Queries),
		waRepo:    repository.NewWhatsAppRepository(database.Queries),
	}
}

// manager builds a READY manager over the fixture's real container.
func (f *deviceStoreFixture) manager(t *testing.T) *Manager {
	t.Helper()
	m := NewManager(f.container, NewWALogger("whatsapp-test"), &f.cfg.WhatsApp, f.syncRepo, f.waRepo)
	registerManagerCleanup(t, m)
	m.SetIngestor(stalenessTestIngestor{})
	m.SetHistoryRecorder(NewHistoryRecorder(f.waRepo))
	m.SetHistoryDrainReady()
	return m
}

func (f *deviceStoreFixture) storedJIDs(t *testing.T) []types.JID {
	t.Helper()
	jids, err := listDevices(f.ctx, f.container)
	require.NoError(t, err)
	return jids
}

func TestWhatsAppRePair_LeavesExactlyOneStoredDevice(t *testing.T) {
	t.Parallel()
	f := newDeviceStoreFixture(t)

	oldJID := types.NewJID("15551110000", types.DefaultUserServer)
	oldJID.Device = 1
	oldDevice := saveTestLinkedDevice(t, f.ctx, f.container, oldJID, "Replaced Device")

	m := f.manager(t)

	oldClient := newFakeClient()
	newClient := newFakeClient()
	newJID := types.NewJID("15552220000", types.DefaultUserServer)
	newJID.Device = 2

	m.setSessionFactory(func(ctx context.Context, req sessionRequest) (*session, error) {
		if !req.fresh {
			jid := oldJID
			return attachConnCtx(&session{client: oldClient, paired: true, jid: &jid, deleteDevice: oldDevice.Delete}), nil
		}
		// What the library does on a successful pairing: the fresh device is
		// written to the store. That is what makes the leftover row possible.
		freshDevice := saveTestLinkedDevice(t, f.ctx, f.container, newJID, "New Device")
		return attachConnCtx(&session{client: newClient, deleteDevice: freshDevice.Delete}), nil
	})

	require.NoError(t, m.Start(f.ctx))

	// Re-pairing is permitted while the old session is down.
	oldClient.setConnected(false)
	require.NoError(t, m.StartPairing(f.ctx, PairRequest{Method: PairMethodQR}))
	pairingSess := pairingSession(t, m)
	require.NotNil(t, pairingSess)

	require.Len(t, f.storedJIDs(t), 2,
		"mid-pairing the store legitimately holds both — that is the window")

	require.True(t, dispatchEvent(t, m, pairingSess, &events.PairSuccess{ID: newJID}))

	eventually(t, "a re-pair leaves exactly one device", func() bool {
		return len(f.storedJIDs(t)) == 1
	})
	remaining := f.storedJIDs(t)
	assert.Equal(t, newJID.String(), remaining[0].String(), "the surviving device is the one just linked")

	// And the restart path agrees.
	_, res, err := resolveLinkedDevice(f.ctx, f.container, &newJID)
	require.NoError(t, err)
	require.True(t, res.paired)
	require.NotNil(t, res.jid)
	assert.Equal(t, newJID.String(), res.jid.String())
	assert.False(t, res.extraRows)
}

// TestWhatsAppRePair_RetainedDeviceStillResumesTheLinkedOne is the failure path
// of the same invariant: when the replaced device cannot be deleted the store
// really does hold two rows, and GetFirstDevice would then be a coin toss.
func TestWhatsAppRePair_RetainedDeviceStillResumesTheLinkedOne(t *testing.T) {
	t.Parallel()
	f := newDeviceStoreFixture(t)

	oldJID := types.NewJID("15551110000", types.DefaultUserServer)
	oldJID.Device = 1
	saveTestLinkedDevice(t, f.ctx, f.container, oldJID, "Retained Device")

	m := f.manager(t)

	oldClient := newFakeClient()
	newClient := newFakeClient()
	newJID := types.NewJID("15552220000", types.DefaultUserServer)
	newJID.Device = 2

	m.setSessionFactory(func(ctx context.Context, req sessionRequest) (*session, error) {
		if !req.fresh {
			// The delete of the replaced device fails, every attempt.
			jid := oldJID
			return attachConnCtx(&session{client: oldClient, paired: true, jid: &jid, deleteDevice: func(context.Context) error {
				return errors.New("device store is unreachable")
			}}), nil
		}
		freshDevice := saveTestLinkedDevice(t, f.ctx, f.container, newJID, "New Device")
		return attachConnCtx(&session{client: newClient, deleteDevice: freshDevice.Delete}), nil
	})

	require.NoError(t, m.Start(f.ctx))
	oldClient.setConnected(false)
	require.NoError(t, m.StartPairing(f.ctx, PairRequest{Method: PairMethodQR}))
	pairingSess := pairingSession(t, m)
	require.NotNil(t, pairingSess)
	require.True(t, dispatchEvent(t, m, pairingSess, &events.PairSuccess{ID: newJID}))

	require.Len(t, f.storedJIDs(t), 2, "the delete failed, so the stale row is genuinely still there")
	eventually(t, "a store holding two sessions is a degraded state the user can see", func() bool {
		return m.Status().ReplacedDeviceRetained
	})

	// The restart path, through a SECOND manager over the same database — the
	// only thing that decides which account resumes.
	restarted := f.manager(t)
	resumedClient := newFakeClient()
	resolved := make(chan types.JID, 4)
	restarted.setSessionFactory(func(ctx context.Context, req sessionRequest) (*session, error) {
		_, res, err := resolveLinkedDevice(ctx, f.container, req.linked)
		if err != nil {
			return nil, err
		}
		if res.jid != nil {
			resolved <- *res.jid
		}
		return attachConnCtx(&session{client: resumedClient, paired: res.paired, jid: res.jid, extraRows: res.extraRows}), nil
	})
	require.NoError(t, restarted.Start(f.ctx))

	require.NotEmpty(t, resolved)
	assert.Equal(t, newJID.String(), (<-resolved).String(),
		"the device that resumes is the one that was linked, not whichever row the unordered scan returned first")
	assert.True(t, restarted.Status().ReplacedDeviceRetained,
		"a selector hit over a multi-row store is a degraded store, not a clean resume")
}

// TestResolveLinkedDevice_RefusesToGuess tables the whole resolution rule.
//
// The invariant is one sentence: it refuses if and only if the store holds two
// or more devices and none of them is the one the selector names. A selector is
// a tie-breaker; with no tie there is nothing for it to break.
func TestResolveLinkedDevice_RefusesToGuess(t *testing.T) {
	t.Parallel()

	jidA := types.NewJID("15551110000", types.DefaultUserServer)
	jidA.Device = 1
	jidB := types.NewJID("15552220000", types.DefaultUserServer)
	jidB.Device = 2
	missing := types.NewJID("15559990000", types.DefaultUserServer)
	missing.Device = 9

	tests := []struct {
		name        string
		stored      []types.JID
		selector    *types.JID
		wantErr     error
		wantPaired  bool
		wantJID     *types.JID
		wantExtra   bool
		wantHealing bool
		explanation string
	}{
		{
			name: "selector hits, only row", stored: []types.JID{jidA}, selector: &jidA,
			wantPaired: true, wantJID: &jidA,
			explanation: "the clean resume",
		},
		{
			name: "selector hits, other rows exist", stored: []types.JID{jidA, jidB}, selector: &jidA,
			wantPaired: true, wantJID: &jidA, wantExtra: true,
			explanation: "resumed, but the store is degraded and says so",
		},
		{
			name: "selector misses, no rows", stored: nil, selector: &missing,
			explanation: "nothing to resume: a fresh unpaired device",
		},
		{
			name: "stale selector, exactly one row", stored: []types.JID{jidB}, selector: &missing,
			wantPaired: true, wantJID: &jidB, wantHealing: true,
			explanation: "the healing row: a stale selector over a single device is not ambiguous",
		},
		{
			name: "selector misses, two rows", stored: []types.JID{jidA, jidB}, selector: &missing,
			wantErr:     ErrDeviceStoreAmbiguous,
			explanation: "two devices and no tie-breaker has no correct answer",
		},
		{
			name: "no selector, no rows", stored: nil,
			explanation: "a fresh unpaired device",
		},
		{
			name: "no selector, exactly one row", stored: []types.JID{jidA},
			wantPaired: true, wantJID: &jidA, wantHealing: true,
			explanation: "heals a store written before the selector existed",
		},
		{
			name: "no selector, two rows", stored: []types.JID{jidA, jidB},
			wantErr:     ErrDeviceStoreAmbiguous,
			explanation: "refuses rather than returning an arbitrary row",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newDeviceStoreFixture(t)
			for _, jid := range tt.stored {
				saveTestLinkedDevice(t, f.ctx, f.container, jid, "device")
			}

			device, res, err := resolveLinkedDevice(f.ctx, f.container, tt.selector)
			if tt.wantErr != nil {
				require.ErrorIs(t, err, tt.wantErr, tt.explanation)
				assert.Nil(t, device)
				return
			}
			require.NoError(t, err, tt.explanation)
			require.NotNil(t, device)
			assert.Equal(t, tt.wantPaired, res.paired, tt.explanation)
			assert.Equal(t, tt.wantExtra, res.extraRows, tt.explanation)
			assert.Equal(t, tt.wantHealing, res.healSelector, tt.explanation)
			if tt.wantJID == nil {
				assert.Nil(t, res.jid)
				return
			}
			require.NotNil(t, res.jid)
			assert.Equal(t, tt.wantJID.String(), res.jid.String(), tt.explanation)
		})
	}
}

// TestResolveLinkedDevice_StaleSelectorWithOneRowHeals is the row an earlier
// draft got wrong, seen through the manager's own boot path: a failed selector
// write after a SUCCESSFUL replacement delete leaves exactly this state, and
// refusing it would strand a perfectly good single device.
func TestResolveLinkedDevice_StaleSelectorWithOneRowHeals(t *testing.T) {
	t.Parallel()
	f := newDeviceStoreFixture(t)

	live := types.NewJID("15552220000", types.DefaultUserServer)
	live.Device = 2
	saveTestLinkedDevice(t, f.ctx, f.container, live, "Only Device")

	m := f.manager(t)
	require.NoError(t, m.Start(f.ctx))

	state, err := f.syncRepo.GetSyncStateBySource(f.ctx, repository.InteractionSourceWhatsApp, nil)
	require.NoError(t, err)
	assert.Equal(t, live.String(), state.Metadata[metadataLinkedJID],
		"the record of which account is linked is repaired at the same time")
	assert.NotEqual(t, StateError, m.Status().State, "a single device is always resumed")
	persisted := m.Status().LinkSelectorPersisted
	require.NotNil(t, persisted)
	assert.True(t, *persisted)
}

// TestClearLocalDevice_PurgesTheEnumeratedSetOverARealStore is the resurrection
// scenario end to end: a retained device A alongside the linked device B, and a
// forced clear that removes exactly the set it enumerated.
func TestClearLocalDevice_PurgesTheEnumeratedSetOverARealStore(t *testing.T) {
	t.Parallel()
	f := newDeviceStoreFixture(t)

	jidA := types.NewJID("15551110000", types.DefaultUserServer)
	jidA.Device = 1
	jidB := types.NewJID("15552220000", types.DefaultUserServer)
	jidB.Device = 2
	saveTestLinkedDevice(t, f.ctx, f.container, jidA, "Retained")
	saveTestLinkedDevice(t, f.ctx, f.container, jidB, "Linked")

	m := f.manager(t)
	cli := newFakeClient()
	m.setSessionFactory(func(ctx context.Context, req sessionRequest) (*session, error) {
		jid := jidB
		return attachConnCtx(&session{client: cli, paired: true, jid: &jid}), nil
	})

	result, err := m.Disconnect(f.ctx, true)
	require.NoError(t, err)
	assert.True(t, result.Forced)
	assert.Empty(t, f.storedJIDs(t), "the clear removes exactly {A, B}")

	// A subsequent boot reports not_paired rather than resuming A.
	restarted := f.manager(t)
	require.NoError(t, restarted.Start(f.ctx))
	assert.Equal(t, StateNotPaired, restarted.Status().State)
}

// TestDisconnect_RefusedAfterTheLibrarySavedTheNewDeviceButBeforePairSuccess is
// the P0, driven at the library's real save-then-announce point.
//
// handlePair writes the new device's row and only AFTER that returns does its
// goroutine dispatch PairSuccess. Since control events are enqueue-only, the
// adoption turn can still be in the mailbox while an unlink decides. The 409 is
// the proof that the dangerous ordering is unreachable: the unlink never
// enumerates, so there is nothing for a stale decision to delete.
func TestDisconnect_RefusedAfterTheLibrarySavedTheNewDeviceButBeforePairSuccess(t *testing.T) {
	for _, force := range []bool{false, true} {
		name := "unlink"
		if force {
			name = "forced unlink"
		}
		t.Run(name, func(t *testing.T) {
			f := newDeviceStoreFixture(t)

			jidA := types.NewJID("15551110000", types.DefaultUserServer)
			jidA.Device = 1
			jidB := types.NewJID("15552220000", types.DefaultUserServer)
			jidB.Device = 2
			deviceA := saveTestLinkedDevice(t, f.ctx, f.container, jidA, "Installed")

			m := f.manager(t)
			oldClient := newFakeClient()
			newClient := newFakeClient()
			m.setSessionFactory(func(ctx context.Context, req sessionRequest) (*session, error) {
				if !req.fresh {
					jid := jidA
					return attachConnCtx(&session{client: oldClient, paired: true, jid: &jid, healSelector: true, deleteDevice: deviceA.Delete}), nil
				}
				// The library's Store.Save, exactly where handlePair does it.
				fresh := saveTestLinkedDevice(t, f.ctx, f.container, jidB, "Just Linked")
				return attachConnCtx(&session{client: newClient, deleteDevice: fresh.Delete}), nil
			})

			require.NoError(t, m.Start(f.ctx))
			oldClient.setConnected(false)
			require.NoError(t, m.StartPairing(f.ctx, PairRequest{Method: PairMethodQR}))
			pairingSess := pairingSession(t, m)
			require.NotNil(t, pairingSess)
			require.Len(t, f.storedJIDs(t), 2, "B's row is saved, exactly as the library saves it")

			// The unlink's turn is queued BEFORE the adoption's, so it decides
			// while PairSuccess is still in the mailbox.
			release := parkLoop(t, m)
			unlinkErr := make(chan error, 1)
			go func() {
				_, err := m.Disconnect(f.ctx, force)
				unlinkErr <- err
			}()
			eventually(t, "the unlink is queued", func() bool { return len(m.inbox) >= 1 })
			require.True(t, m.handleEventFor(pairingSess, &events.PairSuccess{ID: jidB}))
			release()

			assert.ErrorIs(t, <-unlinkErr, ErrPairingInProgress)
			m.settle()

			eventually(t, "B is adopted once the queued PairSuccess drains, and A is the row that goes", func() bool {
				jids := f.storedJIDs(t)
				return len(jids) == 1 && jids[0].String() == jidB.String()
			})

			state, err := f.syncRepo.GetSyncStateBySource(f.ctx, repository.InteractionSourceWhatsApp, nil)
			require.NoError(t, err)
			assert.Equal(t, jidB.String(), state.Metadata[metadataLinkedJID],
				"and the selector names the device the user just linked")
		})
	}
}

// TestStop_LeavesAnOrphanThatTheNextBootReports is the one abandonment case the
// JID-targeted cleanup cannot cover: the process is ending, the JID is not yet
// known, and handlePair may complete after our last instruction. The guarantee
// moves to the next boot, where the orphan is surfaced rather than silent.
func TestStop_LeavesAnOrphanThatTheNextBootReports(t *testing.T) {
	t.Parallel()
	f := newDeviceStoreFixture(t)

	jidA := types.NewJID("15551110000", types.DefaultUserServer)
	jidA.Device = 1
	jidB := types.NewJID("15552220000", types.DefaultUserServer)
	jidB.Device = 2
	saveTestLinkedDevice(t, f.ctx, f.container, jidA, "Linked")

	m := f.manager(t)
	cli := newFakeClient()
	m.setSessionFactory(func(ctx context.Context, req sessionRequest) (*session, error) {
		if req.fresh {
			return attachConnCtx(&session{client: newFakeClient()}), nil
		}
		_, res, err := resolveLinkedDevice(ctx, f.container, req.linked)
		if err != nil {
			return nil, err
		}
		return attachConnCtx(&session{client: cli, paired: res.paired, jid: res.jid, extraRows: res.extraRows, healSelector: res.healSelector}), nil
	})
	require.NoError(t, m.Start(f.ctx))
	require.Equal(t, StateConnecting, m.Status().State)
	cli.setConnected(false)
	require.NoError(t, m.StartPairing(f.ctx, PairRequest{Method: PairMethodQR}))

	m.Stop()

	// The library's own goroutine completes Store.Save after our last
	// instruction. Nothing in-process can observe it.
	saveTestLinkedDevice(t, f.ctx, f.container, jidB, "Orphan")
	require.Len(t, f.storedJIDs(t), 2)

	restarted := f.manager(t)
	resumed := newFakeClient()
	resolved := make(chan types.JID, 4)
	restarted.setSessionFactory(func(ctx context.Context, req sessionRequest) (*session, error) {
		_, res, err := resolveLinkedDevice(ctx, f.container, req.linked)
		if err != nil {
			return nil, err
		}
		if res.jid != nil {
			resolved <- *res.jid
		}
		return attachConnCtx(&session{client: resumed, paired: res.paired, jid: res.jid, extraRows: res.extraRows}), nil
	})
	require.NoError(t, restarted.Start(f.ctx))

	require.NotEmpty(t, resolved)
	assert.Equal(t, jidA.String(), (<-resolved).String(),
		"the selected device is resumed: the abandoned attempt is not what the user wants resumed")
	assert.True(t, restarted.Status().ReplacedDeviceRetained,
		"the orphan is surfaced, with a forced disconnect as the documented remedy")
}

// attachConnCtx mirrors newClient: a session carries the context that governs
// its connection's lifetime from the moment it exists. Hand-built fixtures have
// to do it too, or connectEffect refuses them — which is the point of that
// refusal being loud.
func attachConnCtx(s *session) *session {
	s.connCtx, s.cancelConn = context.WithCancel(context.Background())
	s.dialDone = make(chan struct{})
	return s
}

func saveTestLinkedDevice(t *testing.T, ctx context.Context, container *sqlstore.Container, jid types.JID, pushName string) *store.Device {
	t.Helper()
	device := container.NewDevice()
	device.ID = &jid
	device.PushName = pushName
	device.Platform = "test"
	device.Account = &waAdv.ADVSignedDeviceIdentity{
		Details:             []byte{0x0a, 0x01},
		AccountSignatureKey: make([]byte, 32),
		AccountSignature:    make([]byte, 64),
		DeviceSignature:     make([]byte, 64),
	}
	require.NoError(t, device.Save(ctx))
	return device
}
