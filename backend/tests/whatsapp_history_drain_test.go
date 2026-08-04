//go:build integration_testdb

package tests

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"
	"personal-crm/backend/internal/testdb"
	"personal-crm/backend/internal/whatsapp"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/proto/waWeb"
	"google.golang.org/protobuf/proto"
)

// --- harness ----------------------------------------------------------------

// drainEnv is one test's view of the whole drain path over a REAL database: the
// real notification inbox with its claim/lease/fencing SQL, the real ingestor
// with its gate and matcher, and the real staging writes into comms_message.
//
// Every test gets its own ephemeral clone rather than sharing the package DB,
// for the same reason the repository tests do: ClaimNextNotification is
// deliberately database-global (there is exactly one drainer), so it cannot be
// namespace-scoped.
type drainEnv struct {
	ctx      context.Context
	wa       *repository.WhatsAppRepository
	comms    *repository.CommsMessageRepository
	database *db.Database
	group    *stubGroupInfo
	fetcher  *fakeHistoryFetcher
	ingestor *whatsapp.Ingestor
	drainer  *whatsapp.HistoryDrainer
	ns       string
}

func setupHistoryDrainTest(t *testing.T) *drainEnv {
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

	env := &drainEnv{
		ctx:      ctx,
		wa:       repository.NewWhatsAppRepository(database.Queries),
		comms:    repository.NewCommsMessageRepository(database.Queries),
		database: database,
		group:    &stubGroupInfo{info: &whatsapp.ChatGroupInfo{Title: "Book Club", MemberCount: 4}},
		ns:       syntheticNS(t),
	}
	env.group.account = env.accountJID()
	env.comms.SetPool(database.Pool)

	gate := whatsapp.NewChatGate(env.wa, 10)
	gate.BindGroupInfoSource(func() whatsapp.GroupInfoFetcher { return env.group })
	matcher := whatsapp.NewPeerMatcher(
		service.NewIdentityService(repository.NewIdentityRepository(database.Queries)),
		env.comms,
		repository.NewExternalContactRepository(database.Queries),
		nil,
		2,
	)
	env.ingestor = whatsapp.NewIngestor(env.comms, gate, matcher)
	env.fetcher = newFakeHistoryFetcher(env.accountJID())
	env.drainer = whatsapp.NewHistoryDrainer(env.wa, env.ingestor, gate, func() whatsapp.HistoryFetcher {
		return env.fetcher
	})
	return env
}

func (e *drainEnv) accountJID() string    { return e.ns + "own@s.whatsapp.net" }
func (e *drainEnv) dmJID() string         { return e.ns + "peer@s.whatsapp.net" }
func (e *drainEnv) lidJID() string        { return e.ns + "888@lid" }
func (e *drainEnv) groupJID() string      { return e.ns + "group@g.us" }
func (e *drainEnv) extID(s string) string { return "wa-" + e.ns + "-" + s }

// record stores one media-backed chunk, exactly as the live event handler does.
func (e *drainEnv) record(t *testing.T, protocolMsgID string, order int32) uuid.UUID {
	t.Helper()
	id, err := e.wa.RecordNotification(e.ctx, protocolMsgID, []byte("pointer-"+protocolMsgID),
		"FULL", order, nil, repository.HistoryDispositionProject)
	require.NoError(t, err)
	return id
}

func (e *drainEnv) recordDroppedInline(t *testing.T, protocolMsgID string) uuid.UUID {
	t.Helper()
	id, err := e.wa.RecordNotification(e.ctx, protocolMsgID, []byte("pointer-"+protocolMsgID),
		"INITIAL_BOOTSTRAP", 0, nil, repository.HistoryDispositionDroppedInline)
	require.NoError(t, err)
	return id
}

func (e *drainEnv) row(t *testing.T, id uuid.UUID) repository.HistoryNotification {
	t.Helper()
	rows, err := e.wa.ListNotifications(e.ctx, []string{
		repository.HistoryNotificationStatePending,
		repository.HistoryNotificationStateProcessing,
		repository.HistoryNotificationStateDone,
		repository.HistoryNotificationStateFailed,
	})
	require.NoError(t, err)
	for _, r := range rows {
		if r.ID == id {
			return r
		}
	}
	t.Fatalf("notification %s not found", id)
	return repository.HistoryNotification{}
}

// storedRow reads the staged row for one external id.
func (e *drainEnv) storedRow(t *testing.T, externalID string) *repository.CommsMessage {
	t.Helper()
	row, err := e.comms.GetLatestByExternalID(e.ctx, repository.InteractionSourceWhatsApp, externalID)
	require.NoError(t, err)
	return row
}

func (e *drainEnv) noStoredRow(t *testing.T, externalID string) {
	t.Helper()
	_, err := e.comms.GetLatestByExternalID(e.ctx, repository.InteractionSourceWhatsApp, externalID)
	assert.ErrorIs(t, err, db.ErrNotFound, "nothing may be staged for %s", externalID)
}

// unmatchedCount counts a peer's LIVE unmatched staging rows. Every drained
// message in these tests is unmatched (the clone has no contacts), so this is
// the row COUNT — which is what "one row per message" needs.
func (e *drainEnv) unmatchedCount(t *testing.T, peerJID string) int64 {
	t.Helper()
	counts, err := e.comms.ListUnmatchedPeerCounts(e.ctx, repository.InteractionSourceWhatsApp, &peerJID, 1)
	require.NoError(t, err)
	if len(counts) == 0 {
		return 0
	}
	return counts[0].TotalCount
}

// --- the fake history fetcher ----------------------------------------------

// fakeHistoryFetcher is the seam the drainer reaches the live client through.
// No test may reach a real one.
//
// ProjectHistoryMessage deliberately does the same shape of work the real seam
// does — build an IngestedMessage off the envelope — because what is under test
// here is the DRAINER's logic and the REAL ingest/staging path below it.
type fakeHistoryFetcher struct {
	calls []string

	chunk       *waHistorySync.HistorySync
	downloadErr error
	ackErr      error
	deleteErr   error
	account     string
}

func newFakeHistoryFetcher(account string) *fakeHistoryFetcher {
	return &fakeHistoryFetcher{account: account}
}

func (f *fakeHistoryFetcher) DownloadHistorySync(context.Context, []byte) (*waHistorySync.HistorySync, error) {
	f.calls = append(f.calls, "download")
	if f.downloadErr != nil {
		return nil, f.downloadErr
	}
	return f.chunk, nil
}

func (f *fakeHistoryFetcher) AckHistorySync(context.Context, string) error {
	f.calls = append(f.calls, "ack")
	return f.ackErr
}

func (f *fakeHistoryFetcher) DeleteHistoryMedia(context.Context, []byte) error {
	f.calls = append(f.calls, "delete")
	return f.deleteErr
}

func (f *fakeHistoryFetcher) AccountJID() string { return f.account }

func (f *fakeHistoryFetcher) ProjectHistoryMessage(_ context.Context, chatJID string, web *waWeb.WebMessageInfo) (whatsapp.IngestedMessage, bool, error) {
	chatType := whatsapp.ChatTypePrivate
	if strings.HasSuffix(chatJID, "@g.us") {
		chatType = whatsapp.ChatTypeGroup
	}
	account := f.account
	body := web.GetMessage().GetConversation()
	msg := whatsapp.IngestedMessage{
		MessageID:   web.GetKey().GetID(),
		ChatJID:     chatJID,
		ChatType:    chatType,
		SentAt:      time.Unix(int64(web.GetMessageTimestamp()), 0).UTC().Truncate(time.Microsecond),
		Body:        &body,
		MessageType: whatsapp.MessageTypeText,
		AccountJID:  &account,
	}
	// A DM's counterpart is the chat itself, in both directions — the same
	// attribution the live parser makes.
	if chatType == whatsapp.ChatTypePrivate {
		peer := chatJID
		msg.PeerJID = &peer
	} else if participant := web.GetParticipant(); participant != "" {
		peer := participant
		msg.PeerJID = &peer
	}
	return msg, true, nil
}

var _ whatsapp.HistoryFetcher = (*fakeHistoryFetcher)(nil)

// --- chunk fixtures ---------------------------------------------------------

var (
	drainAfterFloor  = whatsapp.BackfillFloorTime().Add(72 * time.Hour)
	drainBeforeFloor = whatsapp.BackfillFloorTime().Add(-72 * time.Hour)
)

func drainWebMessage(id string, sentAt time.Time, body string) *waWeb.WebMessageInfo {
	return &waWeb.WebMessageInfo{
		Key:              &waCommon.MessageKey{ID: proto.String(id)},
		MessageTimestamp: proto.Uint64(uint64(sentAt.Unix())),
		Message:          &waE2E.Message{Conversation: proto.String(body)},
	}
}

func drainConversation(id string, msgs ...*waWeb.WebMessageInfo) *waHistorySync.Conversation {
	conv := &waHistorySync.Conversation{ID: proto.String(id)}
	for _, m := range msgs {
		conv.Messages = append(conv.Messages, &waHistorySync.HistorySyncMsg{Message: m})
	}
	return conv
}

func drainChunk(convs ...*waHistorySync.Conversation) *waHistorySync.HistorySync {
	syncType := waHistorySync.HistorySync_FULL
	return &waHistorySync.HistorySync{SyncType: &syncType, Conversations: convs}
}

// --- tests ------------------------------------------------------------------

// TestHistoryDrainer_StagesAChunkEndToEnd is the happy path against real SQL:
// the chunk walks the whole phase machine and its messages land in
// comms_message through the same choke point the live path uses.
func TestHistoryDrainer_StagesAChunkEndToEnd(t *testing.T) {
	t.Parallel()
	env := setupHistoryDrainTest(t)

	env.fetcher.chunk = drainChunk(drainConversation(env.dmJID(),
		drainWebMessage(env.extID("m1"), drainAfterFloor, "hello"),
		drainWebMessage(env.extID("m2"), drainAfterFloor.Add(time.Minute), "again"),
	))
	id := env.record(t, "proto-"+env.ns, 0)

	require.NoError(t, env.drainer.Drain(env.ctx))

	row := env.row(t, id)
	assert.Equal(t, repository.HistoryNotificationStateDone, row.State)
	assert.Equal(t, repository.HistoryPhaseDeleted, row.Phase)
	assert.NotNil(t, env.storedRow(t, env.extID("m1")))
	assert.NotNil(t, env.storedRow(t, env.extID("m2")))
	assert.EqualValues(t, 2, env.unmatchedCount(t, env.dmJID()))
	assert.Equal(t, []string{"download", "ack", "delete"}, env.fetcher.calls)
}

// TestHistoryDrainer_PreClampContentAbsentFromCommsMessage bounds the claim the
// spec makes precisely: no pre-horizon body reaches comms_message. A chunk of
// only pre-clamp conversations completes and stages nothing at all.
func TestHistoryDrainer_PreClampContentAbsentFromCommsMessage(t *testing.T) {
	t.Parallel()
	env := setupHistoryDrainTest(t)

	env.fetcher.chunk = drainChunk(
		drainConversation(env.dmJID(),
			drainWebMessage(env.extID("old1"), drainBeforeFloor, "a secret from before the horizon"),
			drainWebMessage(env.extID("new1"), drainAfterFloor, "in scope"),
		),
		drainConversation(env.ns+"other@s.whatsapp.net",
			drainWebMessage(env.extID("old2"), drainBeforeFloor, "also from before"),
		),
	)
	id := env.record(t, "proto-"+env.ns, 0)

	require.NoError(t, env.drainer.Drain(env.ctx))

	assert.Equal(t, repository.HistoryNotificationStateDone, env.row(t, id).State)
	env.noStoredRow(t, env.extID("old1"))
	env.noStoredRow(t, env.extID("old2"))
	kept := env.storedRow(t, env.extID("new1"))
	require.NotNil(t, kept.Body)
	assert.Equal(t, "in scope", *kept.Body)
	assert.EqualValues(t, 1, env.unmatchedCount(t, env.dmJID()),
		"the only row for this peer is the post-horizon one")
	assert.Zero(t, env.unmatchedCount(t, env.ns+"other@s.whatsapp.net"),
		"a conversation entirely below the horizon stages nothing at all")
}

// TestHistoryDrainer_ReprocessingIsIdempotent: the staging upsert is
// content-immutable on conflict, which is what makes a re-drain — and the
// resume path's conversation re-walk — free of duplicates without a line of
// drainer code.
func TestHistoryDrainer_ReprocessingIsIdempotent(t *testing.T) {
	t.Parallel()
	env := setupHistoryDrainTest(t)

	env.fetcher.chunk = drainChunk(drainConversation(env.dmJID(),
		drainWebMessage(env.extID("m1"), drainAfterFloor, "hello"),
	))
	env.record(t, "proto-a-"+env.ns, 0)
	require.NoError(t, env.drainer.Drain(env.ctx))

	// The same chunk delivered again, as WhatsApp's redelivery would.
	env.record(t, "proto-b-"+env.ns, 1)
	require.NoError(t, env.drainer.Drain(env.ctx))

	assert.EqualValues(t, 1, env.unmatchedCount(t, env.dmJID()),
		"one row per message, however often it is drained")
}

// TestHistoryDrainer_MatchesLivePathEndState: a message delivered live and the
// same message delivered by history converge on ONE row with the same
// attribution — the property that makes projecting through the shared ingest
// choke point worth the seam.
func TestHistoryDrainer_MatchesLivePathEndState(t *testing.T) {
	t.Parallel()
	env := setupHistoryDrainTest(t)

	sentAt := drainAfterFloor.Truncate(time.Microsecond)
	external := env.extID("converge")
	account := env.accountJID()
	peer := env.dmJID()

	// Live first.
	require.NoError(t, env.ingestor.IngestMessage(env.ctx, whatsapp.IngestedMessage{
		MessageID:   external,
		ChatJID:     peer,
		ChatType:    whatsapp.ChatTypePrivate,
		SentAt:      sentAt,
		Body:        strPtr2("converging"),
		MessageType: whatsapp.MessageTypeText,
		PeerJID:     &peer,
		AccountJID:  &account,
	}))
	live := env.storedRow(t, external)
	require.EqualValues(t, 1, env.unmatchedCount(t, peer))

	// Then the same message arrives in the backfill.
	env.fetcher.chunk = drainChunk(drainConversation(peer, drainWebMessage(external, sentAt, "converging")))
	env.record(t, "proto-"+env.ns, 0)
	require.NoError(t, env.drainer.Drain(env.ctx))

	after := env.storedRow(t, external)
	require.EqualValues(t, 1, env.unmatchedCount(t, peer), "the two paths converge on one row")
	assert.Equal(t, live.ID, after.ID)
	assert.Equal(t, live.ThreadID, after.ThreadID)
	assert.Equal(t, live.AccountID, after.AccountID)
	assert.Equal(t, live.PeerHandle, after.PeerHandle)
	assert.Equal(t, live.Direction, after.Direction)
	assert.Equal(t, live.Body, after.Body)
}

// TestHistoryDrainer_LIDDMIsKeyedOnThePhoneNumberThread: history keyed on a LID
// and a live message on the phone-number server land under ONE thread_id, so
// one conversation does not render as two interaction streams.
func TestHistoryDrainer_LIDDMIsKeyedOnThePhoneNumberThread(t *testing.T) {
	t.Parallel()
	env := setupHistoryDrainTest(t)

	account := env.accountJID()
	pn := env.dmJID()

	// The live half, on the phone-number server.
	require.NoError(t, env.ingestor.IngestMessage(env.ctx, whatsapp.IngestedMessage{
		MessageID:   env.extID("live"),
		ChatJID:     pn,
		ChatType:    whatsapp.ChatTypePrivate,
		SentAt:      drainAfterFloor.Add(time.Hour).Truncate(time.Microsecond),
		Body:        strPtr2("live message"),
		MessageType: whatsapp.MessageTypeText,
		PeerJID:     &pn,
		AccountJID:  &account,
	}))

	// The history half, keyed on the LID but naming its phone-number twin.
	conv := drainConversation(env.lidJID(), drainWebMessage(env.extID("hist"), drainAfterFloor, "history message"))
	conv.PnJID = proto.String(pn)
	env.fetcher.chunk = drainChunk(conv)
	env.record(t, "proto-"+env.ns, 0)
	require.NoError(t, env.drainer.Drain(env.ctx))

	liveRow := env.storedRow(t, env.extID("live"))
	histRow := env.storedRow(t, env.extID("hist"))
	require.NotNil(t, liveRow)
	require.NotNil(t, histRow)
	require.NotNil(t, liveRow.ThreadID)
	require.NotNil(t, histRow.ThreadID)
	assert.Equal(t, pn, *liveRow.ThreadID)
	assert.Equal(t, pn, *histRow.ThreadID, "one chat, one thread — not one per transport")
	assert.EqualValues(t, 2, env.unmatchedCount(t, pn))
}

// TestHistoryDrainer_ResumesAfterLostLease: a worker abandoned mid-chunk leaves
// no terminal write, and the reclaimed run projects only the conversations
// AFTER the checkpointed one while producing the identical final row set.
func TestHistoryDrainer_ResumesAfterLostLease(t *testing.T) {
	t.Parallel()
	env := setupHistoryDrainTest(t)

	convA := drainConversation(env.ns+"a@s.whatsapp.net", drainWebMessage(env.extID("a1"), drainAfterFloor, "one"))
	convB := drainConversation(env.ns+"b@s.whatsapp.net", drainWebMessage(env.extID("b1"), drainAfterFloor, "two"))
	env.fetcher.chunk = drainChunk(convA, convB)
	id := env.record(t, "proto-"+env.ns, 0)

	// Claim it by hand, walk it to 'downloaded' and checkpoint conversation 0,
	// then let the lease expire — the "worker died mid-chunk" shape.
	first, err := env.wa.ClaimNextNotification(env.ctx)
	require.NoError(t, err)
	ok, err := env.wa.AdvancePhase(env.ctx, id, *first.ClaimToken,
		repository.HistoryPhaseRecorded, repository.HistoryPhaseDownloaded)
	require.NoError(t, err)
	require.True(t, ok)
	peerA := env.ns + "a@s.whatsapp.net"
	require.NoError(t, env.ingestor.IngestMessage(env.ctx, whatsapp.IngestedMessage{
		MessageID: env.extID("a1"), ChatJID: peerA,
		ChatType: whatsapp.ChatTypePrivate, SentAt: drainAfterFloor.Truncate(time.Microsecond),
		Body: strPtr2("one"), MessageType: whatsapp.MessageTypeText, PeerJID: &peerA,
	}))
	ok, err = env.wa.SaveCheckpoint(env.ctx, id, *first.ClaimToken,
		[]byte(fmt.Sprintf(`{"conversation_index":0,"chat_jid":%q}`, env.ns+"a@s.whatsapp.net")))
	require.NoError(t, err)
	require.True(t, ok)
	require.NoError(t, env.wa.BackdateClaimForTest(env.ctx, id))

	require.NoError(t, env.drainer.Drain(env.ctx))

	assert.Equal(t, repository.HistoryNotificationStateDone, env.row(t, id).State)
	assert.NotNil(t, env.storedRow(t, env.extID("a1")))
	assert.NotNil(t, env.storedRow(t, env.extID("b1")))
	assert.EqualValues(t, 1, env.unmatchedCount(t, env.ns+"a@s.whatsapp.net"),
		"the conversation completed before the lease was lost is not stored twice")
	assert.EqualValues(t, 1, env.unmatchedCount(t, env.ns+"b@s.whatsapp.net"))
}

// TestHistoryDrainer_StaleWorkerCannotClobberSuccessor: after a reclaim, every
// fenced write from the stale token is a no-op — which is what lets the drainer
// abandon a chunk without a shutdown hook.
func TestHistoryDrainer_StaleWorkerCannotClobberSuccessor(t *testing.T) {
	t.Parallel()
	env := setupHistoryDrainTest(t)

	id := env.record(t, "proto-"+env.ns, 0)
	stale, err := env.wa.ClaimNextNotification(env.ctx)
	require.NoError(t, err)
	require.NoError(t, env.wa.BackdateClaimForTest(env.ctx, id))
	successor, err := env.wa.ClaimNextNotification(env.ctx)
	require.NoError(t, err)
	require.NotEqual(t, *stale.ClaimToken, *successor.ClaimToken)

	saved, err := env.wa.SaveCheckpoint(env.ctx, id, *stale.ClaimToken, []byte(`{"conversation_index":9}`))
	require.NoError(t, err)
	assert.False(t, saved)

	advanced, err := env.wa.AdvancePhase(env.ctx, id, *stale.ClaimToken,
		repository.HistoryPhaseRecorded, repository.HistoryPhaseDownloaded)
	require.NoError(t, err)
	assert.False(t, advanced)

	done, err := env.wa.MarkNotificationDone(env.ctx, id, *stale.ClaimToken)
	require.NoError(t, err)
	assert.False(t, done)

	failed, err := env.wa.MarkNotificationFailed(env.ctx, id, *stale.ClaimToken, "stale")
	require.NoError(t, err)
	assert.False(t, failed)

	row := env.row(t, id)
	assert.Equal(t, repository.HistoryPhaseRecorded, row.Phase, "nothing the stale worker did applied")
	assert.Equal(t, repository.HistoryNotificationStateProcessing, row.State)
	assert.Nil(t, row.LastError)
}

// TestHistoryDrainer_RejectsIllegalPhaseJump: without the edge map the token
// fence alone would accept a caller naming its own jump — projected straight to
// deleted, skipping the protocol receipt, after which WhatsApp redelivers the
// chunk forever.
func TestHistoryDrainer_RejectsIllegalPhaseJump(t *testing.T) {
	t.Parallel()
	env := setupHistoryDrainTest(t)

	id := env.record(t, "proto-"+env.ns, 0)
	claimed, err := env.wa.ClaimNextNotification(env.ctx)
	require.NoError(t, err)

	ok, err := env.wa.AdvancePhase(env.ctx, id, *claimed.ClaimToken,
		repository.HistoryPhaseRecorded, repository.HistoryPhaseProjected)
	require.Error(t, err)
	assert.ErrorIs(t, err, repository.ErrIllegalPhaseEdge)
	assert.False(t, ok)
	assert.Equal(t, repository.HistoryPhaseRecorded, env.row(t, id).Phase)
}

// TestHistoryDrainer_FailedChunkIsRequeueable: a terminal failure is
// operator-recoverable, and the requeued chunk completes on the next drain.
func TestHistoryDrainer_FailedChunkIsRequeueable(t *testing.T) {
	t.Parallel()
	env := setupHistoryDrainTest(t)

	env.fetcher.chunk = drainChunk(drainConversation(env.dmJID(),
		drainWebMessage(env.extID("m1"), drainAfterFloor, "hello"),
	))
	env.fetcher.downloadErr = fmt.Errorf("download: %w", whatsmeow.ErrMediaDownloadFailedWith404)
	id := env.record(t, "proto-"+env.ns, 0)

	require.NoError(t, env.drainer.Drain(env.ctx), "a decided failure is not the job's error")
	failed := env.row(t, id)
	require.Equal(t, repository.HistoryNotificationStateFailed, failed.State)
	require.NotNil(t, failed.LastError)

	// The operator fixes the cause and requeues.
	env.fetcher.downloadErr = nil
	require.NoError(t, env.wa.RequeueFailedNotification(env.ctx, id))

	require.NoError(t, env.drainer.Drain(env.ctx))
	assert.Equal(t, repository.HistoryNotificationStateDone, env.row(t, id).State)
	assert.NotNil(t, env.storedRow(t, env.extID("m1")))
}

// TestHistoryDrainer_TransientFailuresAreCappedAndBecomeRequeueable pins all
// three halves of the attempt cap: it fires, the requeue grants a fresh
// attempt, and a second failure returns the row to failed.
func TestHistoryDrainer_TransientFailuresAreCappedAndBecomeRequeueable(t *testing.T) {
	t.Parallel()
	env := setupHistoryDrainTest(t)

	env.fetcher.downloadErr = errors.New("connection reset")
	id := env.record(t, "proto-"+env.ns, 0)

	// Each claim increments attempts; the drainer converts the transient
	// failure into a terminal one once the budget is spent.
	var lastErr error
	for i := 0; i < 12; i++ {
		lastErr = env.drainer.Drain(env.ctx)
		if env.row(t, id).State == repository.HistoryNotificationStateFailed {
			break
		}
		require.Error(t, lastErr, "under the cap the failure is returned so River records it")
		require.NoError(t, env.wa.BackdateClaimForTest(env.ctx, id))
	}

	failed := env.row(t, id)
	require.Equal(t, repository.HistoryNotificationStateFailed, failed.State,
		"a permanently-transient chunk must not re-download its payload forever")
	require.NotNil(t, failed.LastError)
	assert.Contains(t, *failed.LastError, "gave up after")

	// The requeue does not reset attempts — that is lifetime triage evidence —
	// so it grants exactly one fresh attempt.
	require.NoError(t, env.wa.RequeueFailedNotification(env.ctx, id))
	assert.Equal(t, failed.Attempts, env.row(t, id).Attempts, "attempts is not reset by a requeue")

	require.NoError(t, env.drainer.Drain(env.ctx))
	assert.Equal(t, repository.HistoryNotificationStateFailed, env.row(t, id).State,
		"a still-broken chunk returns to failed rather than becoming unrecoverable")
}

// TestHistoryDrainer_DroppedInlineChunkIsNeverProjected: the chunk arrived with
// its history inlined against our explicit non-inline request, so the payload
// was discarded at capture. It still owes a protocol receipt.
func TestHistoryDrainer_DroppedInlineChunkIsNeverProjected(t *testing.T) {
	t.Parallel()
	env := setupHistoryDrainTest(t)

	env.fetcher.chunk = drainChunk(drainConversation(env.dmJID(),
		drainWebMessage(env.extID("never"), drainAfterFloor, "must not be stored"),
	))
	id := env.recordDroppedInline(t, "proto-"+env.ns)
	require.Equal(t, repository.HistoryPhaseProjected, env.row(t, id).Phase,
		"a dropped chunk enters the phase machine past the download and the projection")

	require.NoError(t, env.drainer.Drain(env.ctx))

	assert.Equal(t, repository.HistoryNotificationStateDone, env.row(t, id).State)
	assert.Equal(t, []string{"ack"}, env.fetcher.calls, "no download and no delete for a payload we never held")
	env.noStoredRow(t, env.extID("never"))
}

// TestHistoryDrainer_DroppedInlineRequeueRetriesOnlyTheReceipt: a requeued
// dropped row re-enters at 'projected', so a second run can only re-send the
// receipt — it can never project the payload it never had.
func TestHistoryDrainer_DroppedInlineRequeueRetriesOnlyTheReceipt(t *testing.T) {
	t.Parallel()
	env := setupHistoryDrainTest(t)

	env.fetcher.chunk = drainChunk(drainConversation(env.dmJID(),
		drainWebMessage(env.extID("never"), drainAfterFloor, "must not be stored"),
	))
	env.fetcher.ackErr = errors.New("socket closed")
	id := env.recordDroppedInline(t, "proto-"+env.ns)

	require.Error(t, env.drainer.Drain(env.ctx))
	require.NoError(t, env.wa.BackdateClaimForTest(env.ctx, id))

	env.fetcher.ackErr = nil
	require.NoError(t, env.drainer.Drain(env.ctx))

	assert.Equal(t, repository.HistoryNotificationStateDone, env.row(t, id).State)
	assert.Equal(t, []string{"ack", "ack"}, env.fetcher.calls)
	env.noStoredRow(t, env.extID("never"))
}

// TestHistoryDrainer_GroupChunkIsGatedBeforeAnythingIsStored: an undecidable
// group stores nothing anywhere in the chunk, including the DM alongside it.
func TestHistoryDrainer_GroupChunkIsGatedBeforeAnythingIsStored(t *testing.T) {
	t.Parallel()
	env := setupHistoryDrainTest(t)

	env.fetcher.chunk = drainChunk(
		drainConversation(env.dmJID(), drainWebMessage(env.extID("dm"), drainAfterFloor, "hey")),
		drainConversation(env.groupJID(), drainWebMessage(env.extID("grp"), drainAfterFloor, "hi all")),
	)
	id := env.record(t, "proto-"+env.ns, 0)
	env.group.info = nil
	env.group.err = errors.New("socket closed")

	require.Error(t, env.drainer.Drain(env.ctx))

	env.noStoredRow(t, env.extID("dm"))
	env.noStoredRow(t, env.extID("grp"))
	assert.Equal(t, repository.HistoryNotificationStateProcessing, env.row(t, id).State)

	// Once the group resolves, the whole chunk stages.
	env.group.err = nil
	env.group.info = &whatsapp.ChatGroupInfo{Title: "Book Club", MemberCount: 4}
	require.NoError(t, env.wa.BackdateClaimForTest(env.ctx, id))
	require.NoError(t, env.drainer.Drain(env.ctx))

	assert.NotNil(t, env.storedRow(t, env.extID("dm")))
	assert.NotNil(t, env.storedRow(t, env.extID("grp")))
}

// TestHistoryDrainer_OversizedGroupHistoryIsNotStored: the fail-closed size
// gate applies to backfill exactly as it does to live messages.
func TestHistoryDrainer_OversizedGroupHistoryIsNotStored(t *testing.T) {
	t.Parallel()
	env := setupHistoryDrainTest(t)

	env.fetcher.chunk = drainChunk(
		drainConversation(env.groupJID(), drainWebMessage(env.extID("grp"), drainAfterFloor, "hi all")),
		drainConversation(env.dmJID(), drainWebMessage(env.extID("dm"), drainAfterFloor, "hey")),
	)
	env.group.info = &whatsapp.ChatGroupInfo{Title: "Huge", MemberCount: 400}
	id := env.record(t, "proto-"+env.ns, 0)

	require.NoError(t, env.drainer.Drain(env.ctx))

	assert.Equal(t, repository.HistoryNotificationStateDone, env.row(t, id).State)
	assert.NotNil(t, env.storedRow(t, env.extID("dm")))
	env.noStoredRow(t, env.extID("grp"))
}

// TestStatus_ReportsBackfillProgressAndObservedFloor: the counts move while a
// backfill is in flight and clear when it completes, and the observed floor is
// the oldest message actually staged — the empirical answer to how deep the
// one-shot history reached.
func TestStatus_ReportsBackfillProgressAndObservedFloor(t *testing.T) {
	t.Parallel()
	env := setupHistoryDrainTest(t)

	manager := whatsapp.NewManager(nil, whatsapp.NewWALogger("whatsapp-test"),
		&config.TestConfig().WhatsApp, repository.NewSyncRepository(env.database.Queries), env.wa)
	t.Cleanup(manager.Stop)

	id := env.record(t, "proto-"+env.ns, 0)
	pending := manager.Status().Backfill
	assert.Equal(t, 1, pending.Pending)
	assert.Equal(t, 0, pending.Processing)
	assert.Nil(t, pending.ObservedFloorAt, "nothing is staged yet")

	oldest := drainAfterFloor.Truncate(time.Microsecond)
	env.fetcher.chunk = drainChunk(drainConversation(env.dmJID(),
		drainWebMessage(env.extID("m2"), oldest.Add(48*time.Hour), "later"),
		drainWebMessage(env.extID("m1"), oldest, "earliest"),
	))
	require.NoError(t, env.drainer.Drain(env.ctx))
	require.Equal(t, repository.HistoryNotificationStateDone, env.row(t, id).State)

	done := manager.Status().Backfill
	assert.Equal(t, 0, done.Pending)
	assert.Equal(t, 0, done.Processing)
	assert.Equal(t, 0, done.Failed)
	require.NotNil(t, done.ObservedFloorAt, "the floor is reported once something is staged")
	assert.WithinDuration(t, oldest, done.ObservedFloorAt.UTC(), time.Second)
}

// TestHistoryDrainer_FailedChunkIsCountedAndListedForTheOperator: a chunk that
// cannot be projected is surfaced rather than silently lost.
func TestHistoryDrainer_FailedChunkIsCountedAndListedForTheOperator(t *testing.T) {
	t.Parallel()
	env := setupHistoryDrainTest(t)

	manager := whatsapp.NewManager(nil, whatsapp.NewWALogger("whatsapp-test"),
		&config.TestConfig().WhatsApp, repository.NewSyncRepository(env.database.Queries), env.wa)
	t.Cleanup(manager.Stop)

	env.fetcher.downloadErr = fmt.Errorf("download: %w", whatsmeow.ErrNoURLPresent)
	id := env.record(t, "proto-"+env.ns, 0)
	require.NoError(t, env.drainer.Drain(env.ctx))

	assert.Equal(t, 1, manager.Status().Backfill.Failed)
	listed, err := env.wa.ListNotifications(env.ctx, []string{repository.HistoryNotificationStateFailed})
	require.NoError(t, err)
	require.Len(t, listed, 1)
	assert.Equal(t, id, listed[0].ID)
	require.NotNil(t, listed[0].LastError)
}
