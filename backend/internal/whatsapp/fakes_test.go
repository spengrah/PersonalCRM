package whatsapp

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"
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
	// would hold rather than only what the manager published.
	status repository.SyncStatus

	statuses []repository.SyncStatus
	errors   []string
	calls    []string

	// These make the persistence layer fail, which is the only way to reach the
	// paths where a durable write did not happen.
	terminalErr error
	getErr      error
	createErr   error
	metadataErr error

	// terminalEntered is closed on the first terminal write and terminalBlock,
	// when non-nil, holds the writer inside it. Together they let a test park a
	// turn mid-flight and drive a second message against it, which is how
	// serialization is proved directly rather than by asserting a lock was held.
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
// It honours its context, exactly as the real repository call does: the turn's
// actorDBTimeout can only bound a call that is actually context-aware.
func (f *fakeSyncStore) MarkSyncStateTerminal(ctx context.Context, id uuid.UUID, reason string, metadata map[string]any) (*repository.SyncState, error) {
	// Before the fake's own lock, so a blocked write does not also block the
	// test's reads of what has been recorded so far.
	f.mu.Lock()
	entered, block := f.terminalEntered, f.terminalBlock
	f.terminalEntered = nil
	f.mu.Unlock()
	if entered != nil {
		close(entered)
	}
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			f.mu.Lock()
			f.record("terminal")
			f.mu.Unlock()
			return nil, ctx.Err()
		}
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
// durably recorded terminal reason on a row that is not in error.
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
	if f.metadataErr != nil {
		return nil, f.metadataErr
	}
	f.metadata = metadata
	return &repository.SyncState{ID: id, Metadata: metadata}, nil
}

func (f *fakeSyncStore) terminalReason() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	reason, _ := f.metadata[repository.SyncStateMetadataTerminalReason].(string)
	return reason
}

func (f *fakeSyncStore) linkedJID() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	jid, _ := f.metadata[metadataLinkedJID].(string)
	return jid
}

func (f *fakeSyncStore) setErr(apply func(*fakeSyncStore)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	apply(f)
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
	mu     sync.Mutex
	counts map[string]int
	floor  *time.Time
	err    error
	// block, when non-nil, holds both reads inside the repository until it is
	// closed or the caller's context expires — the wedged-repository case.
	block chan struct{}
}

func (f *fakeBackfillReader) wait(ctx context.Context) error {
	f.mu.Lock()
	block := f.block
	f.mu.Unlock()
	if block == nil {
		return nil
	}
	select {
	case <-block:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (f *fakeBackfillReader) CountByStateAndDisposition(ctx context.Context) (map[string]int, error) {
	if err := f.wait(ctx); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	return f.counts, nil
}

func (f *fakeBackfillReader) ObservedFloor(ctx context.Context) (*time.Time, error) {
	if err := f.wait(ctx); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	return f.floor, nil
}

// --- fake device store -----------------------------------------------------

// fakeDevices stands in for the whatsmeow container's device table. The staged
// purge goes through the CONTAINER, not through a session, so this is the seam
// it needs.
type fakeDevices struct {
	mu sync.Mutex

	jids    []types.JID
	listErr error
	delErr  error

	// listEntered/listBlock and delEntered/delBlock park the purge inside a
	// stage, which is how the staged fence is driven at each of its boundaries.
	listEntered chan struct{}
	listBlock   chan struct{}
	delEntered  chan struct{}
	delBlock    chan struct{}
	// delStall makes every delete hang until its context expires — the shape a
	// stalled database has, and the only way to see whether a flush pass is
	// bounded as a batch or per item. delStallJID stalls exactly ONE row, which
	// is what distinguishes a queue that rotates past a stuck head from one that
	// re-presents it forever.
	delStall    bool
	delStallJID *types.JID
	attempts    int

	listed  int
	deleted []types.JID
}

func newFakeDevices(jids ...types.JID) *fakeDevices {
	return &fakeDevices{jids: append([]types.JID(nil), jids...)}
}

func (f *fakeDevices) ops() deviceOps {
	return deviceOps{list: f.list, deleteAll: f.deleteAll, deleteJID: f.deleteOne}
}

func (f *fakeDevices) list(context.Context) ([]types.JID, error) {
	f.mu.Lock()
	entered, block := f.listEntered, f.listBlock
	f.listEntered = nil
	f.listed++
	f.mu.Unlock()
	if entered != nil {
		close(entered)
	}
	if block != nil {
		<-block
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	return append([]types.JID(nil), f.jids...), nil
}

func (f *fakeDevices) deleteAll(ctx context.Context, jids []types.JID) error {
	for _, jid := range jids {
		if err := f.deleteOne(ctx, jid); err != nil {
			return err
		}
	}
	return nil
}

func (f *fakeDevices) deleteOne(ctx context.Context, jid types.JID) error {
	f.mu.Lock()
	entered, block, stall := f.delEntered, f.delBlock, f.delStall
	if f.delStallJID != nil && f.delStallJID.ToNonAD() == jid.ToNonAD() {
		stall = true
	}
	f.attempts++
	f.delEntered = nil
	f.mu.Unlock()
	if stall {
		<-ctx.Done()
		return ctx.Err()
	}
	if entered != nil {
		close(entered)
	}
	if block != nil {
		<-block
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if f.delErr != nil {
		return f.delErr
	}
	f.deleted = append(f.deleted, jid)
	kept := f.jids[:0]
	for _, existing := range f.jids {
		if existing.ToNonAD() != jid.ToNonAD() {
			kept = append(kept, existing)
		}
	}
	f.jids = kept
	return nil
}

// add models a device row the library saved behind our back — the case the
// staged purge's supersession check exists for.
func (f *fakeDevices) add(jid types.JID) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.jids = append(f.jids, jid)
}

func (f *fakeDevices) remaining() []types.JID {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]types.JID(nil), f.jids...)
}

func (f *fakeDevices) deletedJIDs() []types.JID {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]types.JID(nil), f.deleted...)
}

func (f *fakeDevices) setErr(apply func(*fakeDevices)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	apply(f)
}

// deleteAttempts counts every call, stalled or not, which is how a bounded
// retry loop is told from an unbounded one.
func (f *fakeDevices) deleteAttempts() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.attempts
}

func (f *fakeDevices) enumerations() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.listed
}

// --- fake session client ---------------------------------------------------

// sharedLog is an ordered call log two or more fake clients can append to, so a
// test can assert ordering ACROSS clients.
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

// fakeClient stands in for *whatsmeow.Client so no unit test dials WhatsApp.
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
	// holds the caller inside it. They let a test park a multi-step API
	// operation mid-flight and drive a competing one against it.
	logoutEntered chan struct{}
	logoutBlock   chan struct{}

	// connectEntered/connectBlock do the same for Connect, which is what the
	// QR-channel-to-Connect cancellation window needs.
	connectEntered chan struct{}
	connectBlock   chan struct{}

	// disconnectHook runs inside Disconnect. It is how a test makes a client
	// call re-enter the manager's dispatcher.
	disconnectHook func()

	// blackHoleDial makes ConnectContext hang until its context is cancelled,
	// with nothing else to release it — the shape a dial to a black-holed
	// socket actually has.
	blackHoleDial bool

	// connCtx is the context the dial was handed, RETAINED exactly as the real
	// client retains it: whatsmeow stores it as the socket's parent and gives it
	// to auto-reconnect, so a fake that dropped it could not tell a connection
	// that outlives its dial from one closed the instant the batch finished.
	connCtx  context.Context
	pumpDone chan struct{}

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

// ConnectContext honours its context, exactly as the real client's does. A fake
// with a plain Connect() could not tell a dial bounded by the effect deadline
// from one that runs on the library's own background context — which is the
// whole of the regression this seam exists to catch.
func (c *fakeClient) ConnectContext(ctx context.Context) error {
	c.record("connect")

	c.mu.Lock()
	entered, block, err := c.connectEntered, c.connectBlock, c.connectErr
	if c.blackHoleDial && block == nil {
		block = make(chan struct{}) // never closed: only ctx can end this dial
	}
	c.connectEntered = nil
	c.mu.Unlock()
	if entered != nil {
		close(entered)
	}
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			c.record("connect_cancelled")
			return ctx.Err()
		}
	}
	if err != nil {
		return err
	}

	c.mu.Lock()
	c.connected = true
	c.connCtx = ctx
	pump := make(chan struct{})
	c.pumpDone = pump
	c.mu.Unlock()

	// The read pump the real socket starts, and which lives under the context
	// the dial was given. It is what makes "the connection survived its batch"
	// an observable fact rather than an assertion about a pointer.
	go func() {
		<-ctx.Done()
		close(pump)
	}()
	return nil
}

// connCtxErr reports the state of the retained connection context: nil while
// the connection is live.
func (c *fakeClient) connCtxErr() error {
	c.mu.Lock()
	ctx := c.connCtx
	c.mu.Unlock()
	if ctx == nil {
		return errors.New("never connected")
	}
	return ctx.Err()
}

// pumpRunning reports whether the socket's read pump is still alive.
func (c *fakeClient) pumpRunning() bool {
	c.mu.Lock()
	pump := c.pumpDone
	c.mu.Unlock()
	if pump == nil {
		return false
	}
	select {
	case <-pump:
		return false
	default:
		return true
	}
}

func (c *fakeClient) Disconnect() {
	c.record("disconnect")
	c.mu.Lock()
	hook := c.disconnectHook
	c.connected = false
	c.mu.Unlock()
	if hook != nil {
		hook()
	}
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

func (c *fakeClient) setConnected(v bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.connected = v
}

// Logout honours its context, exactly as the real client's does: the effect
// runner's deadline can only preempt a call that is actually context-aware.
func (c *fakeClient) Logout(ctx context.Context) error {
	c.record("logout")

	c.mu.Lock()
	entered, block := c.logoutEntered, c.logoutBlock
	c.logoutEntered = nil
	c.mu.Unlock()
	if entered != nil {
		close(entered)
	}
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return ctx.Err()
		}
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
	c.mu.Lock()
	qrErr, silent, ch := c.qrErr, c.qrSilent, c.qrChan
	c.mu.Unlock()
	if qrErr != nil {
		return nil, qrErr
	}
	if !silent {
		select {
		case ch <- whatsmeow.QRChannelItem{Event: whatsmeow.QRChannelEventCode, Code: defaultFakeQRCode, Timeout: time.Minute}:
		default:
		}
	}
	return ch, nil
}

func (c *fakeClient) PairPhone(_ context.Context, _ string, _ bool, _ whatsmeow.PairClientType, displayName string) (string, error) {
	c.record("pair_phone:" + displayName)
	c.mu.Lock()
	defer c.mu.Unlock()
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

func (f *fakeIngestor) setErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

func (f *fakeIngestor) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.messages)
}

func (f *fakeIngestor) first() IngestedMessage {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.messages) == 0 {
		return IngestedMessage{}
	}
	return f.messages[0]
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
	// synchronous.
	entered chan struct{}
	block   chan struct{}
}

func (f *fakeRecorder) RecordHistoryNotification(_ context.Context, protocolMsgID string, notification []byte, syncType string, chunkOrder int32, oldestMsgTS *time.Time, disposition string) error {
	f.mu.Lock()
	entered, block := f.entered, f.block
	f.entered = nil
	f.mu.Unlock()
	if entered != nil {
		close(entered)
	}
	if block != nil {
		<-block
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

func (f *fakeRecorder) setErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

func (f *fakeRecorder) all() []recordedNotification {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recordedNotification(nil), f.recorded...)
}

// --- manager builders ------------------------------------------------------

type testCleanup interface{ Cleanup(func()) }

// fakeSessionFor builds the session a test's factory hands out.
func fakeSessionFor(cli *fakeClient, paired bool, jid *types.JID) *session {
	// Mirrors newClient: a session carries the context that governs its
	// connection's lifetime from the moment it exists.
	connCtx, cancelConn := context.WithCancel(context.Background())
	return &session{
		client:     cli,
		paired:     paired,
		jid:        jid,
		connCtx:    connCtx,
		cancelConn: cancelConn,
		dialDone:   make(chan struct{}),
		deleteDevice: func(context.Context) error {
			cli.record("delete_device")
			cli.mu.Lock()
			defer cli.mu.Unlock()
			return cli.deleteDeviceErr
		},
	}
}

// newTestManager builds a manager whose readiness gate is SATISFIED and whose
// session factory hands out the supplied fake client. Nothing here touches the
// network or the database.
func newTestManager(t testCleanup, cli *fakeClient, paired bool) (*Manager, *fakeSyncStore, *fakeIngestor, *fakeRecorder) {
	m, syncStore, ingestor, recorder, _ := newTestManagerWithDevices(t, cli, paired)
	return m, syncStore, ingestor, recorder
}

func newTestManagerWithDevices(t testCleanup, cli *fakeClient, paired bool) (*Manager, *fakeSyncStore, *fakeIngestor, *fakeRecorder, *fakeDevices) {
	return newTestManagerFull(t, cli, paired, &fakeBackfillReader{})
}

// testDeviceJID is the device a "paired" test manager has stored. The staged
// purge goes through the container, so a paired fixture has to seed the device
// table as well as the session.
var testDeviceJID = types.NewJID("15550000001", types.DefaultUserServer)

func newTestManagerFull(t testCleanup, cli *fakeClient, paired bool, waRepo backfillReader) (*Manager, *fakeSyncStore, *fakeIngestor, *fakeRecorder, *fakeDevices) {
	syncStore := newFakeSyncStore()
	ingestor := &fakeIngestor{}
	recorder := &fakeRecorder{}
	devices := newFakeDevices()
	if paired {
		devices = newFakeDevices(testDeviceJID)
	}

	m := newManagerForTest(t, syncStore, waRepo)
	m.SetIngestor(ingestor)
	m.SetHistoryRecorder(recorder)
	m.SetHistoryDrainReady()
	m.setDeviceOps(devices.ops())
	m.setSessionFactory(func(context.Context, sessionRequest) (*session, error) {
		if cli == nil {
			return nil, errors.New("no client configured")
		}
		if paired {
			jid := testDeviceJID
			return fakeSessionFor(cli, true, &jid), nil
		}
		return fakeSessionFor(cli, false, nil), nil
	})
	return m, syncStore, ingestor, recorder, devices
}

// blockingSessionFactory builds a factory that blocks until the batch context
// expires. It is how an effectDeadline expiry is driven without sleeping for the
// production bound.
func blockingSessionFactory() sessionFactory {
	return func(ctx context.Context, _ sessionRequest) (*session, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
}

// tuneTimeouts adjusts the manager's expiry bounds. It must be called
// immediately after construction, before any operation is submitted: the
// timeouts are read only by effect goroutines, which cannot exist yet.
func tuneTimeouts(m *Manager, tune func(*managerTimeouts)) {
	tune(&m.timeouts)
}

// expirePairing backdates the in-flight attempt past its TTL, from inside a
// turn, so nothing writes loop-owned state off the loop.
func expirePairing(m *Manager) {
	m.runOp(func(st *actorState, reply chan opResult) {
		if st.pairing != nil {
			st.pairing.expiresAt = accelerated.GetCurrentTime().Add(-time.Second)
		}
		reply <- opResult{}
	})
}

// useClient replaces the session factory so the next build hands out cli.
func useClient(m *Manager, cli *fakeClient, paired bool) {
	m.setSessionFactory(func(context.Context, sessionRequest) (*session, error) {
		return fakeSessionFor(cli, paired, nil), nil
	})
}

// newManagerForTest builds a manager and guarantees its loop is shut down, so a
// test binary never leaks an actor goroutine.
func newManagerForTest(t testCleanup, syncRepo syncStateWriter, waRepo backfillReader) *Manager {
	m := NewManager(nil, NewWALogger("whatsapp-test"), &config.WhatsAppConfig{}, syncRepo, waRepo)
	registerManagerCleanup(t, m)
	return m
}

// registerManagerCleanup stops the manager AND joins its goroutines.
//
// Stop alone is not enough for the wedged-loop tier: it deliberately returns
// without waiting, so the loop and its effects can still be running — and still
// LOGGING — when the next test starts. The package's logger tests replace the
// process-wide logger, so a leaked goroutine is a genuine data race under
// -race rather than merely untidy. Joining here is the harness's job, not the
// manager's: a wedged shutdown that blocked the process would be the worse bug.
func registerManagerCleanup(t testCleanup, m *Manager) {
	t.Cleanup(func() {
		m.Stop()
		select {
		case <-m.loopExited:
			// launchClosed is set on the loop before loopExited closes, so no
			// further Add is possible and this Wait is race-free.
			m.effects.Wait()
		case <-time.After(5 * time.Second):
		}
	})
}

// newNotReadyManager builds a manager in the state a real deployment of this PR
// is actually in: recorder wired, ingestor still the refusing default, drainer
// not registered.
func newNotReadyManager(t testCleanup) *Manager {
	m := newManagerForTest(t, newFakeSyncStore(), &fakeBackfillReader{})
	m.SetHistoryRecorder(&fakeRecorder{})
	m.setSessionFactory(func(context.Context, sessionRequest) (*session, error) {
		return nil, errors.New("newSession must not be reached while not ready")
	})
	return m
}

// --- actor test helpers ----------------------------------------------------

// dispatchEvent delivers an event and waits for the actor to finish with it.
//
// A control event now returns true on ENQUEUE rather than after its turn, so the
// assertion that follows needs a barrier. settle is that barrier: because the
// inbox is FIFO, its return means every previously submitted message has been
// fully processed and published — deterministic by construction rather than by
// the handler happening to be synchronous.
func dispatchEvent(t *testing.T, m *Manager, sess *session, evt any) bool {
	t.Helper()
	ok := m.handleEventFor(sess, evt)
	m.settle()
	return ok
}

// pairingSession returns the in-flight attempt's client, copied out from inside
// a turn so nothing dereferences loop-owned state off the loop.
func pairingSession(t *testing.T, m *Manager) *session {
	t.Helper()
	return m.inspect().PairingSess
}

// installedSession returns the installed session pointer for identity
// assertions.
func installedSession(t *testing.T, m *Manager) *session {
	t.Helper()
	return m.inspect().Sess
}

// eventually polls until cond holds. Effects run OFF the loop, so their
// side-effects (a socket closed, a device row deleted) are observed rather than
// assumed to have already happened when the operation returned.
func eventually(t *testing.T, msg string, cond func() bool) {
	t.Helper()
	deadline := accelerated.GetCurrentTime().Add(3 * time.Second)
	for accelerated.GetCurrentTime().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	require.FailNow(t, "condition never held: "+msg)
}

// consistently asserts that cond holds for the WHOLE window.
//
// Effects run off the loop, so a negative assertion taken the instant a turn
// settles proves nothing: the wrong thing may simply not have happened yet.
// Anything asserting that something must NOT happen has to give it the chance.
func consistently(t *testing.T, msg string, window time.Duration, cond func() bool) {
	t.Helper()
	deadline := accelerated.GetCurrentTime().Add(window)
	for accelerated.GetCurrentTime().Before(deadline) {
		if !cond() {
			require.FailNow(t, "condition stopped holding: "+msg)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// parkLoop blocks the actor inside a turn and returns the release function. It
// is how the mailbox-full and wedged-loop contracts are driven.
func parkLoop(t *testing.T, m *Manager) (release func()) {
	t.Helper()
	entered := make(chan struct{})
	gate := make(chan struct{})
	go func() {
		m.runOp(func(_ *actorState, reply chan opResult) {
			close(entered)
			<-gate
			reply <- opResult{}
		})
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		require.FailNow(t, "the loop never entered the parking turn")
	}
	var once sync.Once
	return func() { once.Do(func() { close(gate) }) }
}

// fillMailbox saturates the inbox against a parked loop, so the next submit has
// to block. It is what makes the losslessness contract testable.
func fillMailbox(m *Manager) {
	for i := 0; i < inboxCapacity; i++ {
		select {
		case m.inbox <- &settleMsg{reply: make(chan struct{}, 1)}:
		default:
			return
		}
	}
}
