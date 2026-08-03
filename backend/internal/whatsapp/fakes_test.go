package whatsapp

import (
	"context"
	"errors"
	"sync"
	"time"

	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
	"go.mau.fi/whatsmeow"
)

// --- fake sync-state store -------------------------------------------------

// fakeSyncStore records what the manager persisted and in what order, which is
// what makes "the terminal reason is written BEFORE the client is torn down"
// assertable rather than assumed.
type fakeSyncStore struct {
	mu sync.Mutex

	id       uuid.UUID
	exists   bool
	metadata map[string]any
	// status is the row's persisted status, so a test can ask what the DATABASE
	// would hold rather than only what the manager published. That is what makes
	// "the row can never end durably terminal-but-idle" assertable.
	status repository.SyncStatus

	statuses []repository.SyncStatus
	errors   []string
	calls    []string

	// These make the persistence layer fail, which is the only way to reach the
	// paths where a durable write did not happen.
	terminalErr error
	getErr      error
	createErr   error

	// terminalEntered is closed on the first terminal write and terminalBlock,
	// when non-nil, holds the writer inside it. Together they let a test stop a
	// lifecycle event mid-flight and drive a second one against it, which is the
	// only way to exercise the interleaving the ownership rule exists for.
	terminalEntered chan struct{}
	terminalBlock   chan struct{}
}

func newFakeSyncStore() *fakeSyncStore {
	return &fakeSyncStore{id: uuid.New(), metadata: map[string]any{}, status: repository.SyncStatusIdle}
}

func (f *fakeSyncStore) record(call string) {
	f.calls = append(f.calls, call)
}

func (f *fakeSyncStore) GetSyncStateBySource(_ context.Context, source string, _ *string) (*repository.SyncState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("get")
	if f.getErr != nil {
		return nil, f.getErr
	}
	if !f.exists {
		return nil, db.ErrNotFound
	}
	meta := make(map[string]any, len(f.metadata))
	for k, v := range f.metadata {
		meta[k] = v
	}
	return &repository.SyncState{ID: f.id, Source: source, Metadata: meta}, nil
}

func (f *fakeSyncStore) CreateSyncState(_ context.Context, req repository.CreateSyncStateRequest) (*repository.SyncState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("create")
	if f.createErr != nil {
		return nil, f.createErr
	}
	f.exists = true
	return &repository.SyncState{ID: f.id, Source: req.Source, Enabled: req.Enabled}, nil
}

func (f *fakeSyncStore) UpdateSyncStateStatus(_ context.Context, id uuid.UUID, status repository.SyncStatus, errorMessage *string) (*repository.SyncState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("status:" + string(status))
	f.status = status
	f.statuses = append(f.statuses, status)
	if errorMessage != nil {
		f.errors = append(f.errors, *errorMessage)
	}
	return &repository.SyncState{ID: id}, nil
}

// MarkSyncStateTerminal is the atomic terminal write. The fake models it as one
// operation on one row deliberately: a fake that applied the status and the
// metadata separately could not tell an atomic implementation from a split one.
func (f *fakeSyncStore) MarkSyncStateTerminal(_ context.Context, id uuid.UUID, reason string, metadata map[string]any) (*repository.SyncState, error) {
	// Before the fake's own lock, so a blocked write does not also block the
	// test's reads of what has been recorded so far.
	if f.terminalEntered != nil {
		close(f.terminalEntered)
		f.terminalEntered = nil
	}
	if f.terminalBlock != nil {
		<-f.terminalBlock
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("terminal")
	if f.terminalErr != nil {
		return nil, f.terminalErr
	}
	f.status = repository.SyncStatusError
	f.statuses = append(f.statuses, repository.SyncStatusError)
	f.errors = append(f.errors, reason)
	f.metadata = metadata
	return &repository.SyncState{ID: id, Status: f.status, Metadata: metadata}, nil
}

// terminalButIdle reports the row shape the staleness watchdog can never see: a
// durably recorded terminal reason on a row that is not in error. It is the
// exact state a split write could leave behind.
func (f *fakeSyncStore) terminalButIdle() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	reason, ok := f.metadata[repository.SyncStateMetadataTerminalReason].(string)
	return ok && reason != "" && f.status != repository.SyncStatusError
}

// persistedStatus reports the row's status as the database would hold it.
func (f *fakeSyncStore) persistedStatus() repository.SyncStatus {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.status
}

func (f *fakeSyncStore) UpdateSyncStateMetadata(_ context.Context, id uuid.UUID, metadata map[string]any) (*repository.SyncState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("metadata")
	f.metadata = metadata
	return &repository.SyncState{ID: id, Metadata: metadata}, nil
}

func (f *fakeSyncStore) terminalReason() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	reason, _ := f.metadata[repository.SyncStateMetadataTerminalReason].(string)
	return reason
}

// resetCalls clears the recorded call log so a test can count only the calls a
// specific event produced.
func (f *fakeSyncStore) resetCalls() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = nil
}

func (f *fakeSyncStore) callLog() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

// seedTerminal makes the store look like a previous run recorded a terminal
// disconnect.
func (f *fakeSyncStore) seedTerminal(reason string, bannedUntil *time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.exists = true
	f.status = repository.SyncStatusError
	f.metadata = map[string]any{repository.SyncStateMetadataTerminalReason: reason}
	if bannedUntil != nil {
		f.metadata[metadataBannedUntil] = bannedUntil.UTC().Format(time.RFC3339)
	}
}

// --- fake backfill reader --------------------------------------------------

type fakeBackfillReader struct {
	counts map[string]int
	floor  *time.Time
	err    error
}

func (f *fakeBackfillReader) CountByStateAndDisposition(context.Context) (map[string]int, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.counts, nil
}

func (f *fakeBackfillReader) ObservedFloor(context.Context) (*time.Time, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.floor, nil
}

// --- fake session client ---------------------------------------------------

// fakeClient stands in for *whatsmeow.Client so no unit test dials WhatsApp.
// It records the order of the lifecycle calls, which is what the
// handler-before-connect and keep-the-device-on-failure contracts turn on.
// sharedLog is an ordered call log two or more fake clients can append to, so a
// test can assert ordering ACROSS clients. Per-client logs can only show that
// each call happened, not which happened first.
type sharedLog struct {
	mu    sync.Mutex
	calls []string
}

func (l *sharedLog) record(entry string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls = append(l.calls, entry)
}

func (l *sharedLog) entries() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.calls...)
}

type fakeClient struct {
	mu sync.Mutex

	// name labels this client's entries in shared, when set.
	name   string
	shared *sharedLog

	calls []string

	connectErr      error
	logoutErr       error
	qrErr           error
	pairErr         error
	deleteDeviceErr error

	// logoutEntered is closed on the first Logout and logoutBlock, when non-nil,
	// holds the caller inside it. They let a test stop a multi-step API
	// operation mid-flight and drive a competing one against it.
	logoutEntered chan struct{}
	logoutBlock   chan struct{}

	connected bool
	loggedIn  bool

	qrChan chan whatsmeow.QRChannelItem
	// qrSilent suppresses the default emitted code, so a test can drive the
	// bounded-wait timeout.
	qrSilent bool
	pairCode string
}

// defaultFakeQRCode is what GetQRChannel emits unless the test suppresses it.
const defaultFakeQRCode = "QR-CODE-1"

func newFakeClient() *fakeClient {
	return &fakeClient{qrChan: make(chan whatsmeow.QRChannelItem, 4), pairCode: "ABCD1234"}
}

func (c *fakeClient) record(call string) {
	c.mu.Lock()
	c.calls = append(c.calls, call)
	shared, name := c.shared, c.name
	c.mu.Unlock()
	if shared != nil {
		shared.record(name + ":" + call)
	}
}

func (c *fakeClient) callLog() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.calls...)
}

func (c *fakeClient) Connect() error {
	c.record("connect")
	if c.connectErr != nil {
		return c.connectErr
	}
	c.mu.Lock()
	c.connected = true
	c.mu.Unlock()
	return nil
}

func (c *fakeClient) Disconnect() {
	c.record("disconnect")
	c.mu.Lock()
	c.connected = false
	c.mu.Unlock()
}

func (c *fakeClient) IsConnected() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.connected
}

func (c *fakeClient) IsLoggedIn() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.loggedIn
}

func (c *fakeClient) Logout(context.Context) error {
	c.record("logout")

	c.mu.Lock()
	entered, block := c.logoutEntered, c.logoutBlock
	c.logoutEntered = nil
	c.mu.Unlock()
	if entered != nil {
		close(entered)
	}
	if block != nil {
		<-block
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	return c.logoutErr
}

// GetQRChannel emits one code by default. The real library always generates QR
// codes — even for phone-code pairing, where the first item is its documented
// signal that the connection is established — so a fake that emitted none would
// make every pairing time out. Set qrSilent to exercise that timeout.
func (c *fakeClient) GetQRChannel(context.Context) (<-chan whatsmeow.QRChannelItem, error) {
	c.record("qr_channel")
	if c.qrErr != nil {
		return nil, c.qrErr
	}
	if !c.qrSilent {
		select {
		case c.qrChan <- whatsmeow.QRChannelItem{Event: whatsmeow.QRChannelEventCode, Code: defaultFakeQRCode, Timeout: time.Minute}:
		default:
		}
	}
	return c.qrChan, nil
}

func (c *fakeClient) PairPhone(_ context.Context, _ string, _ bool, _ whatsmeow.PairClientType, displayName string) (string, error) {
	c.record("pair_phone:" + displayName)
	if c.pairErr != nil {
		return "", c.pairErr
	}
	return c.pairCode, nil
}

var _ sessionClient = (*fakeClient)(nil)

// --- fake ingestor / recorder ----------------------------------------------

type fakeIngestor struct {
	mu       sync.Mutex
	messages []IngestedMessage
	err      error
}

func (f *fakeIngestor) IngestMessage(_ context.Context, msg IngestedMessage) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.messages = append(f.messages, msg)
	return f.err
}

func (f *fakeIngestor) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.messages)
}

type recordedNotification struct {
	ProtocolMsgID string
	Notification  []byte
	SyncType      string
	ChunkOrder    int32
	OldestMsgTS   *time.Time
	Disposition   string
}

type fakeRecorder struct {
	mu       sync.Mutex
	recorded []recordedNotification
	err      error

	// entered is closed on the first call and block, when non-nil, holds the
	// recorder inside the call. Together they let a test prove the record is
	// synchronous: if the handler returned while the recorder was still
	// blocked, the capture would be running detached and a crash between the
	// two would lose the chunk.
	entered chan struct{}
	block   chan struct{}
}

func (f *fakeRecorder) RecordHistoryNotification(_ context.Context, protocolMsgID string, notification []byte, syncType string, chunkOrder int32, oldestMsgTS *time.Time, disposition string) error {
	if f.entered != nil {
		close(f.entered)
		f.entered = nil
	}
	if f.block != nil {
		<-f.block
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.recorded = append(f.recorded, recordedNotification{
		ProtocolMsgID: protocolMsgID,
		Notification:  append([]byte(nil), notification...),
		SyncType:      syncType,
		ChunkOrder:    chunkOrder,
		OldestMsgTS:   oldestMsgTS,
		Disposition:   disposition,
	})
	return nil
}

func (f *fakeRecorder) all() []recordedNotification {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recordedNotification(nil), f.recorded...)
}

// --- manager builders ------------------------------------------------------

// newTestManager builds a manager whose readiness gate is SATISFIED and whose
// session factory hands out the supplied fake client. Nothing here touches the
// network or the database.
func newTestManager(t interface{ Cleanup(func()) }, cli *fakeClient, paired bool) (*Manager, *fakeSyncStore, *fakeIngestor, *fakeRecorder) {
	syncStore := newFakeSyncStore()
	ingestor := &fakeIngestor{}
	recorder := &fakeRecorder{}

	m := NewManager(nil, NewWALogger("whatsapp-test"), &config.WhatsAppConfig{}, syncStore, &fakeBackfillReader{})
	m.SetIngestor(ingestor)
	m.SetHistoryRecorder(recorder)
	m.SetHistoryDrainReady()
	m.newSession = func(context.Context, bool) (*session, error) {
		if cli == nil {
			return nil, errors.New("no client configured")
		}
		return &session{client: cli, paired: paired, deleteDevice: func(context.Context) error {
			cli.record("delete_device")
			cli.mu.Lock()
			defer cli.mu.Unlock()
			return cli.deleteDeviceErr
		}}, nil
	}
	return m, syncStore, ingestor, recorder
}

// newNotReadyManager builds a manager in the state a real deployment of this PR
// is actually in: recorder wired, ingestor still the refusing default, drainer
// not registered.
func newNotReadyManager() *Manager {
	m := NewManager(nil, NewWALogger("whatsapp-test"), &config.WhatsAppConfig{}, newFakeSyncStore(), &fakeBackfillReader{})
	m.SetHistoryRecorder(&fakeRecorder{})
	m.newSession = func(context.Context, bool) (*session, error) {
		return nil, errors.New("newSession must not be reached while not ready")
	}
	return m
}
