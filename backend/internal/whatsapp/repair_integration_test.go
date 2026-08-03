//go:build integration_testdb

// Proof that a re-pair leaves exactly ONE device in the whatsmeow store.
//
// It lives in this package, and over a REAL sqlstore.Container, because the
// claim is about the library's own device table: whatsmeow's GetFirstDevice
// returns the first row of an unordered GetAllDevices and documents that it is
// only for callers that do not want multiple sessions. A fake device seam could
// assert that a delete was requested, but not that the store the next boot reads
// actually holds one row — which is the part that decides whether the restart
// resumes the device the user just linked or the one they replaced.
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

func TestWhatsAppRePair_LeavesExactlyOneStoredDevice(t *testing.T) {
	t.Parallel()
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

	log := NewWALogger("whatsapp-test")
	container, err := NewDeviceContainer(ctx, database.Pool, log)
	require.NoError(t, err)

	// The device that is already linked.
	oldJID := types.NewJID("15551110000", types.DefaultUserServer)
	oldJID.Device = 1
	oldDevice := saveTestLinkedDevice(t, ctx, container, oldJID, "Replaced Device")

	m := NewManager(container, log, &cfg.WhatsApp,
		repository.NewSyncRepository(database.Queries),
		repository.NewWhatsAppRepository(database.Queries))
	m.SetIngestor(stalenessTestIngestor{})
	m.SetHistoryRecorder(NewHistoryRecorder(repository.NewWhatsAppRepository(database.Queries)))
	m.SetHistoryDrainReady()

	oldClient := newFakeClient()
	newClient := newFakeClient()
	newJID := types.NewJID("15552220000", types.DefaultUserServer)
	newJID.Device = 2

	m.newSession = func(ctx context.Context, fresh bool) (*session, error) {
		if !fresh {
			return &session{client: oldClient, paired: true, deleteDevice: oldDevice.Delete}, nil
		}
		// What the library does on a successful pairing: the fresh device is
		// written to the store. That is what makes the leftover row possible.
		freshDevice := saveTestLinkedDevice(t, ctx, container, newJID, "New Device")
		return &session{client: newClient, deleteDevice: freshDevice.Delete}, nil
	}

	require.NoError(t, m.Start(ctx))
	t.Cleanup(m.Stop)

	// Re-pairing is permitted while the old session is down.
	oldClient.mu.Lock()
	oldClient.connected = false
	oldClient.mu.Unlock()
	require.NoError(t, m.StartPairing(ctx, PairRequest{Method: PairMethodQR}))
	pairingSess := m.pairing.session()
	require.NotNil(t, pairingSess)

	devices, err := container.GetAllDevices(ctx)
	require.NoError(t, err)
	require.Len(t, devices, 2, "mid-pairing the store legitimately holds both — that is the window")

	require.True(t, m.handleEventFor(pairingSess, &events.PairSuccess{ID: newJID}))

	devices, err = container.GetAllDevices(ctx)
	require.NoError(t, err)
	require.Len(t, devices, 1,
		"a re-pair must leave exactly one device: GetFirstDevice reads an unordered scan, so a leftover row can be the one the next boot resumes")
	require.NotNil(t, devices[0].ID)
	assert.Equal(t, newJID.String(), devices[0].ID.String(), "the surviving device is the one just linked")

	// And the restart path agrees.
	loaded, paired, err := LoadOrCreateDevice(ctx, container)
	require.NoError(t, err)
	require.True(t, paired)
	require.NotNil(t, loaded.ID)
	assert.Equal(t, newJID.String(), loaded.ID.String())
}

// TestWhatsAppRePair_RetainedDeviceStillResumesTheLinkedOne is the failure path
// of the same invariant.
//
// When the replaced device cannot be deleted the store really does hold two
// rows, and GetFirstDevice would then be a coin toss. The linked JID is
// persisted at adoption, so the restart resolves the device that was actually
// linked no matter which row comes back first — and the retained stale row is
// reported rather than hidden.
func TestWhatsAppRePair_RetainedDeviceStillResumesTheLinkedOne(t *testing.T) {
	t.Parallel()
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

	log := NewWALogger("whatsapp-test")
	container, err := NewDeviceContainer(ctx, database.Pool, log)
	require.NoError(t, err)

	oldJID := types.NewJID("15551110000", types.DefaultUserServer)
	oldJID.Device = 1
	saveTestLinkedDevice(t, ctx, container, oldJID, "Retained Device")

	syncRepo := repository.NewSyncRepository(database.Queries)
	waRepo := repository.NewWhatsAppRepository(database.Queries)

	m := NewManager(container, log, &cfg.WhatsApp, syncRepo, waRepo)
	m.SetIngestor(stalenessTestIngestor{})
	m.SetHistoryRecorder(NewHistoryRecorder(waRepo))
	m.SetHistoryDrainReady()

	oldClient := newFakeClient()
	newClient := newFakeClient()
	newJID := types.NewJID("15552220000", types.DefaultUserServer)
	newJID.Device = 2

	m.newSession = func(ctx context.Context, fresh bool) (*session, error) {
		if !fresh {
			// The delete of the replaced device fails, every attempt.
			return &session{client: oldClient, paired: true, deleteDevice: func(context.Context) error {
				return errors.New("device store is unreachable")
			}}, nil
		}
		freshDevice := saveTestLinkedDevice(t, ctx, container, newJID, "New Device")
		return &session{client: newClient, deleteDevice: freshDevice.Delete}, nil
	}

	require.NoError(t, m.Start(ctx))
	t.Cleanup(m.Stop)

	oldClient.mu.Lock()
	oldClient.connected = false
	oldClient.mu.Unlock()
	require.NoError(t, m.StartPairing(ctx, PairRequest{Method: PairMethodQR}))
	pairingSess := m.pairing.session()
	require.NotNil(t, pairingSess)
	require.True(t, m.handleEventFor(pairingSess, &events.PairSuccess{ID: newJID}))

	devices, err := container.GetAllDevices(ctx)
	require.NoError(t, err)
	require.Len(t, devices, 2, "the delete failed, so the stale row is genuinely still there")
	assert.True(t, m.Status().ReplacedDeviceRetained,
		"a store holding two sessions is a degraded state the user can see")

	// The restart path, through a SECOND manager over the same database — the
	// only thing that decides which account resumes.
	restarted := NewManager(container, log, &cfg.WhatsApp, syncRepo, waRepo)
	restarted.SetIngestor(stalenessTestIngestor{})
	restarted.SetHistoryRecorder(NewHistoryRecorder(waRepo))
	restarted.SetHistoryDrainReady()

	terminalReason, _, err := restarted.ensureSyncState(ctx)
	require.NoError(t, err)
	require.Empty(t, terminalReason)

	resumed, err := restarted.newContainerSession(ctx, false)
	require.NoError(t, err)
	require.True(t, resumed.paired)
	require.NotNil(t, resumed.wa.Store.ID)
	assert.Equal(t, newJID.String(), resumed.wa.Store.ID.String(),
		"the device that resumes is the one that was linked, not whichever row the unordered scan returned first")
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
