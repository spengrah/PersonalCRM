//go:build integration_testdb

package tests

import (
	"context"
	"os"
	"testing"
	"time"

	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/testdb"
	wapkg "personal-crm/backend/internal/whatsapp"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mau.fi/whatsmeow/proto/waAdv"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"
)

// waStoreEnv is one test's private database. whatsmeow's device store is a
// singleton per database (GetFirstDevice takes whatever row exists), so every
// test here gets an ephemeral clone rather than sharing the package DB. The
// clone also means whatsmeow's own DDL burst runs against a throwaway schema.
type waStoreEnv struct {
	ctx      context.Context
	database *db.Database
	log      waLog.Logger
}

func setupWhatsAppStoreTest(t *testing.T) *waStoreEnv {
	t.Helper()
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set")
	}
	ctx := context.Background()
	cloneURL, drop := testdb.NewEphemeralClone(t)
	t.Cleanup(drop)

	cfg := config.TestConfig()
	cfg.Database.URL = cloneURL
	database, err := db.NewDatabase(ctx, cfg.Database)
	require.NoError(t, err)
	t.Cleanup(database.Close)

	return &waStoreEnv{ctx: ctx, database: database, log: wapkg.NewWALogger("whatsapp-test")}
}

// saveLinkedDevice populates a fresh device with the identity fields a paired
// device carries and persists it. PutDevice dereferences device.Account
// unconditionally, so a device that has never completed pairing cannot be
// saved without one — this is the minimum shape the store accepts.
func saveLinkedDevice(t *testing.T, env *waStoreEnv, container *sqlstore.Container, jid types.JID, pushName string) *store.Device {
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
	require.NoError(t, device.Save(env.ctx))
	return device
}

// TestWhatsAppDeviceContainer_UpgradeIsIdempotent proves the device store can be
// built twice against the same database without error — the boot path runs
// Upgrade on every start, so a non-idempotent upgrade would break every restart
// after the first.
func TestWhatsAppDeviceContainer_UpgradeIsIdempotent(t *testing.T) {
	t.Parallel()
	env := setupWhatsAppStoreTest(t)

	first, err := wapkg.NewDeviceContainer(env.ctx, env.database.Pool, env.log)
	require.NoError(t, err)
	require.NotNil(t, first)

	second, err := wapkg.NewDeviceContainer(env.ctx, env.database.Pool, env.log)
	require.NoError(t, err, "Upgrade must be idempotent — it runs on every boot")
	require.NotNil(t, second)

	// A successful read against a fresh database is only possible if the
	// whatsmeow_* tables actually exist, which is what proves the upgrade ran.
	// The lookup is BY JID: there is deliberately no "give me whichever device
	// you find" loader, because that is how first-row roulette gets called.
	device, paired, err := wapkg.LoadDeviceByJID(env.ctx, second, types.NewJID("15550000000", types.DefaultUserServer))
	require.NoError(t, err)
	assert.False(t, paired, "a fresh database has no linked device")
	assert.Nil(t, device)
}

func TestWhatsAppDeviceContainer_RejectsNilPool(t *testing.T) {
	t.Parallel()
	_, err := wapkg.NewDeviceContainer(context.Background(), nil, wapkg.NewWALogger("t"))
	assert.Error(t, err)
}

// TestWhatsAppDeviceStore_PairedDeviceSurvivesNewManager is the restart
// guarantee: the linked session lives in the database, so a fresh process finds
// it and does not ask the user to re-pair.
func TestWhatsAppDeviceStore_PairedDeviceSurvivesNewManager(t *testing.T) {
	t.Parallel()
	env := setupWhatsAppStoreTest(t)

	first, err := wapkg.NewDeviceContainer(env.ctx, env.database.Pool, env.log)
	require.NoError(t, err)

	jid := types.NewJID("15551234567", types.DefaultUserServer)
	jid.Device = 3
	saveLinkedDevice(t, env, first, jid, "Restart Survivor")

	// A SECOND container over the same database — a fresh process's view.
	second, err := wapkg.NewDeviceContainer(env.ctx, env.database.Pool, env.log)
	require.NoError(t, err)

	loaded, paired, err := wapkg.LoadDeviceByJID(env.ctx, second, jid)
	require.NoError(t, err)
	require.True(t, paired, "the linked session must survive a restart without re-pairing")
	require.NotNil(t, loaded.ID)
	assert.Equal(t, jid.String(), loaded.ID.String())
	assert.Equal(t, "Restart Survivor", loaded.PushName)
}

// TestWhatsAppManager_StartWithNoDeviceStaysIdle exercises the real device
// store: with readiness satisfied and no stored device, Start returns without
// connecting and reports "not paired".
func TestWhatsAppManager_StartWithNoDeviceStaysIdle(t *testing.T) {
	t.Parallel()
	env := setupWhatsAppStoreTest(t)

	container, err := wapkg.NewDeviceContainer(env.ctx, env.database.Pool, env.log)
	require.NoError(t, err)

	syncRepo := repository.NewSyncRepository(env.database.Queries)
	waRepo := repository.NewWhatsAppRepository(env.database.Queries)

	m := wapkg.NewManager(container, env.log, &config.TestConfig().WhatsApp, syncRepo, waRepo)
	m.SetIngestor(&countingIngestor{})
	m.SetHistoryRecorder(wapkg.NewHistoryRecorder(waRepo))
	m.SetHistoryDrainReady()

	require.NoError(t, m.Start(env.ctx))
	t.Cleanup(m.Stop)

	assert.Equal(t, wapkg.StateNotPaired, m.Status().State)
	assert.Nil(t, m.HistoryFetcher(), "nothing is connected, so the drainer defers rather than claiming")
}

// TestWhatsAppManager_StartInPR3WiringReportsNotReady covers the state a real
// deployment of this change is actually in: the recorder is wired, the ingestor
// and drainer are not, so the client refuses to connect and says why.
func TestWhatsAppManager_StartInPR3WiringReportsNotReady(t *testing.T) {
	t.Parallel()
	env := setupWhatsAppStoreTest(t)

	container, err := wapkg.NewDeviceContainer(env.ctx, env.database.Pool, env.log)
	require.NoError(t, err)

	syncRepo := repository.NewSyncRepository(env.database.Queries)
	waRepo := repository.NewWhatsAppRepository(env.database.Queries)

	m := wapkg.NewManager(container, env.log, &config.TestConfig().WhatsApp, syncRepo, waRepo)
	m.SetHistoryRecorder(wapkg.NewHistoryRecorder(waRepo))

	require.NoError(t, m.Start(env.ctx))
	t.Cleanup(m.Stop)

	status := m.Status()
	assert.Equal(t, wapkg.StateNotReady, status.State)
	assert.Equal(t, wapkg.ReasonIngestNotWired, status.Reason)

	assert.ErrorIs(t, m.StartPairing(env.ctx, wapkg.PairRequest{Method: wapkg.PairMethodQR}), wapkg.ErrIngestNotWired,
		"pairing must refuse on the same precondition, not bypass it")
}

// TestWhatsAppManager_SyncStateRowCreated: the staleness watchdog and the
// settings staleness banner both key off external_sync_state, so the row has to
// exist for WhatsApp to be visible at all.
func TestWhatsAppManager_SyncStateRowCreated(t *testing.T) {
	t.Parallel()
	env := setupWhatsAppStoreTest(t)

	container, err := wapkg.NewDeviceContainer(env.ctx, env.database.Pool, env.log)
	require.NoError(t, err)

	syncRepo := repository.NewSyncRepository(env.database.Queries)
	waRepo := repository.NewWhatsAppRepository(env.database.Queries)

	m := wapkg.NewManager(container, env.log, &config.TestConfig().WhatsApp, syncRepo, waRepo)
	m.SetIngestor(&countingIngestor{})
	m.SetHistoryRecorder(wapkg.NewHistoryRecorder(waRepo))
	m.SetHistoryDrainReady()
	require.NoError(t, m.Start(env.ctx))
	t.Cleanup(m.Stop)

	state, err := syncRepo.GetSyncStateBySource(env.ctx, repository.InteractionSourceWhatsApp, nil)
	require.NoError(t, err)
	assert.Equal(t, repository.InteractionSourceWhatsApp, state.Source)
	assert.False(t, state.Enabled,
		"the row is a status carrier for the settings page, never a scheduler input")
}

// TestWhatsAppHistoryRecorder_PersistsNotificationToInbox exercises the real
// recorder over the real repository, including the idempotency that makes a
// withheld ack safe: WhatsApp redelivers, and the second write is a no-op.
func TestWhatsAppHistoryRecorder_PersistsNotificationToInbox(t *testing.T) {
	t.Parallel()
	env := setupWhatsAppStoreTest(t)

	waRepo := repository.NewWhatsAppRepository(env.database.Queries)
	recorder := wapkg.NewHistoryRecorder(waRepo)

	payload := []byte{1, 2, 3, 4}
	oldest := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

	require.NoError(t, recorder.RecordHistoryNotification(env.ctx, "proto-1", payload, "INITIAL_BOOTSTRAP", 1, &oldest, repository.HistoryDispositionProject))
	require.NoError(t, recorder.RecordHistoryNotification(env.ctx, "proto-1", payload, "INITIAL_BOOTSTRAP", 1, &oldest, repository.HistoryDispositionProject),
		"a redelivered protocol message must collapse onto the existing row")

	rows, err := waRepo.ListNotifications(env.ctx, []string{repository.HistoryNotificationStatePending})
	require.NoError(t, err)
	require.Len(t, rows, 1, "the redelivery must not create a second row")
	assert.Equal(t, payload, rows[0].Notification)
	assert.Equal(t, repository.HistoryDispositionProject, rows[0].Disposition)
	assert.Equal(t, repository.HistoryPhaseRecorded, rows[0].Phase)
}

// TestWhatsAppHistoryRecorder_DroppedInlineEntersAtProjectedPhase: a dropped
// chunk still runs the phase machine so it receives its protocol receipt, but it
// enters past download and projection, so neither can happen for it.
func TestWhatsAppHistoryRecorder_DroppedInlineEntersAtProjectedPhase(t *testing.T) {
	t.Parallel()
	env := setupWhatsAppStoreTest(t)

	waRepo := repository.NewWhatsAppRepository(env.database.Queries)
	recorder := wapkg.NewHistoryRecorder(waRepo)

	require.NoError(t, recorder.RecordHistoryNotification(env.ctx, "proto-dropped", []byte{9}, "INITIAL_BOOTSTRAP", 0, nil, repository.HistoryDispositionDroppedInline))

	rows, err := waRepo.ListNotifications(env.ctx, []string{repository.HistoryNotificationStatePending})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, repository.HistoryDispositionDroppedInline, rows[0].Disposition)
	assert.Equal(t, repository.HistoryPhaseProjected, rows[0].Phase,
		"nothing to download and nothing to project, but the receipt still has to go out")
}

// --- LID verification -------------------------------------------------------

// TestWhatsAppLIDStore_VerificationMatchesOnNormalizedPair exercises the exact
// read-back the history fetcher performs, against a real sqlstore LID store.
//
// The check has to be EQUALITY on the normalized pair, not mere presence: the
// library logs and swallows its own mapping-store failures while still
// reporting the download as a success, and a stale row that maps a LID to a
// different phone number would satisfy a presence check while mis-attributing
// every message from that peer.
func TestWhatsAppLIDStore_VerificationMatchesOnNormalizedPair(t *testing.T) {
	t.Parallel()
	env := setupWhatsAppStoreTest(t)

	container, err := wapkg.NewDeviceContainer(env.ctx, env.database.Pool, env.log)
	require.NoError(t, err)

	device := saveLinkedDevice(t, env, container, types.NewJID("15550000000", types.DefaultUserServer), "LID Test")
	require.NotNil(t, device.LIDs, "the LID store is attached when the device is initialized on save")

	lidA := types.NewJID("111111111111111", types.HiddenUserServer)
	pnA := types.NewJID("15551111111", types.DefaultUserServer)
	lidB := types.NewJID("222222222222222", types.HiddenUserServer)
	pnB := types.NewJID("15552222222", types.DefaultUserServer)
	wrongPN := types.NewJID("15559999999", types.DefaultUserServer)

	require.NoError(t, device.LIDs.PutLIDMapping(env.ctx, lidA, pnA))

	chunk := func(mappings ...*waHistorySync.PhoneNumberToLIDMapping) *waHistorySync.HistorySync {
		return &waHistorySync.HistorySync{
			SyncType:                 waHistorySync.HistorySync_INITIAL_BOOTSTRAP.Enum(),
			PhoneNumberToLidMappings: mappings,
		}
	}
	mapping := func(pn, lid types.JID) *waHistorySync.PhoneNumberToLIDMapping {
		return &waHistorySync.PhoneNumberToLIDMapping{
			PnJID:  proto.String(pn.String()),
			LidJID: proto.String(lid.String()),
		}
	}

	t.Run("all mappings present", func(t *testing.T) {
		assert.NoError(t, wapkg.VerifyHistoryLIDMappings(env.ctx, device.LIDs, chunk(mapping(pnA, lidA))))
	})

	t.Run("mapping absent is incomplete", func(t *testing.T) {
		err := wapkg.VerifyHistoryLIDMappings(env.ctx, device.LIDs, chunk(mapping(pnA, lidA), mapping(pnB, lidB)))
		assert.ErrorIs(t, err, wapkg.ErrLIDMappingsIncomplete)
	})

	t.Run("stale mapping to the wrong phone is incomplete", func(t *testing.T) {
		require.NoError(t, device.LIDs.PutLIDMapping(env.ctx, lidB, wrongPN))
		err := wapkg.VerifyHistoryLIDMappings(env.ctx, device.LIDs, chunk(mapping(pnB, lidB)))
		assert.ErrorIs(t, err, wapkg.ErrLIDMappingsIncomplete,
			"a presence check would pass here and silently mis-attribute the peer")
	})

	t.Run("no mappings is trivially complete", func(t *testing.T) {
		assert.NoError(t, wapkg.VerifyHistoryLIDMappings(env.ctx, device.LIDs, chunk()))
	})
}

// TestWhatsAppLIDStore_VerificationExpectsOnlyWhatTheLibraryKeeps covers the two
// ways a mapping a chunk carried legitimately does not end up in the store.
//
// Each case drives the REAL PutManyLIDMappings — the same entry point whatsmeow
// calls while downloading a history chunk — and asserts the resulting store
// state BEFORE asserting the verification verdict. That ordering is the point:
// the first assertion pins the library behaviour the verification models, so if
// a future whatsmeow changes it, this fails on the premise instead of quietly
// leaving the verification modelling a store that no longer behaves that way.
//
// Verified against a chunk whose raw list was demanded in full, a one-shot
// bootstrap stalls until its attempt budget burns and is then failed terminally,
// with nothing a retry can change.
func TestWhatsAppLIDStore_VerificationExpectsOnlyWhatTheLibraryKeeps(t *testing.T) {
	t.Parallel()
	env := setupWhatsAppStoreTest(t)

	container, err := wapkg.NewDeviceContainer(env.ctx, env.database.Pool, env.log)
	require.NoError(t, err)

	device := saveLinkedDevice(t, env, container, types.NewJID("15550000001", types.DefaultUserServer), "LID Reduction Test")
	require.NotNil(t, device.LIDs)

	chunk := func(mappings ...*waHistorySync.PhoneNumberToLIDMapping) *waHistorySync.HistorySync {
		return &waHistorySync.HistorySync{
			SyncType:                 waHistorySync.HistorySync_INITIAL_BOOTSTRAP.Enum(),
			PhoneNumberToLidMappings: mappings,
		}
	}
	mapping := func(pn, lid types.JID) *waHistorySync.PhoneNumberToLIDMapping {
		return &waHistorySync.PhoneNumberToLIDMapping{
			PnJID:  proto.String(pn.String()),
			LidJID: proto.String(lid.String()),
		}
	}

	t.Run("later LID for one phone number evicts the earlier", func(t *testing.T) {
		sharedPN := types.NewJID("15553333333", types.DefaultUserServer)
		staleLID := types.NewJID("333333333333333", types.HiddenUserServer)
		currentLID := types.NewJID("444444444444444", types.HiddenUserServer)

		carried := chunk(mapping(sharedPN, staleLID), mapping(sharedPN, currentLID))
		require.NoError(t, device.LIDs.PutManyLIDMappings(env.ctx, []store.LIDMapping{
			{LID: staleLID, PN: sharedPN},
			{LID: currentLID, PN: sharedPN},
		}), "the library's own storage call for a chunk's mappings")

		// The premise: one LID per phone number is a UNIQUE constraint, so the
		// store CANNOT hold both and the earlier write is gone.
		stale, err := device.LIDs.GetPNForLID(env.ctx, staleLID)
		require.NoError(t, err)
		require.True(t, stale.IsEmpty(), "the earlier LID must have been evicted for this case to be the one under test")
		current, err := device.LIDs.GetPNForLID(env.ctx, currentLID)
		require.NoError(t, err)
		require.Equal(t, sharedPN.ToNonAD(), current.ToNonAD())

		assert.NoError(t, wapkg.VerifyHistoryLIDMappings(env.ctx, device.LIDs, carried),
			"the evicted pair is not storable, so demanding it back stalls the chunk forever")
	})

	t.Run("entry the library ignores is not expected back", func(t *testing.T) {
		pn := types.NewJID("15554444444", types.DefaultUserServer)
		// Not on the hidden-user server, so PutManyLIDMappings drops it.
		notALID := types.NewJID("555555555555555", types.DefaultUserServer)

		carried := chunk(mapping(pn, notALID))
		require.NoError(t, device.LIDs.PutManyLIDMappings(env.ctx, []store.LIDMapping{{LID: notALID, PN: pn}}),
			"the library reports success while silently ignoring the entry")

		// Probed from the phone-number side because the store REFUSES a
		// GetPNForLID for a non-LID JID outright — which is the other way this
		// entry used to stall a chunk: the read-back errored rather than
		// returning empty, and no retry could change that either.
		stored, err := device.LIDs.GetLIDForPN(env.ctx, pn)
		require.NoError(t, err)
		require.True(t, stored.IsEmpty(), "the library must have ignored this entry for this case to be the one under test")

		assert.NoError(t, wapkg.VerifyHistoryLIDMappings(env.ctx, device.LIDs, carried),
			"an entry the library declines to store can never read back")
	})

	t.Run("a surviving pair is still verified exactly", func(t *testing.T) {
		pn := types.NewJID("15555555555", types.DefaultUserServer)
		lid := types.NewJID("666666666666666", types.HiddenUserServer)
		wrongPN := types.NewJID("15556666666", types.DefaultUserServer)

		require.NoError(t, device.LIDs.PutLIDMapping(env.ctx, lid, wrongPN))

		err := wapkg.VerifyHistoryLIDMappings(env.ctx, device.LIDs, chunk(mapping(pn, lid)))
		assert.ErrorIs(t, err, wapkg.ErrLIDMappingsIncomplete,
			"reducing the expectation must not weaken the mis-attribution guard for pairs that DO survive")
	})
}

// countingIngestor is a real-enough MessageIngestor to satisfy the readiness
// gate. It records nothing this test asserts on; its only job is to not be the
// refusing default.
type countingIngestor struct{ n int }

func (c *countingIngestor) IngestMessage(context.Context, wapkg.IngestedMessage) error {
	c.n++
	return nil
}
