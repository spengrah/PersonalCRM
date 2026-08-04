package whatsapp

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"

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

// --- chunk fixtures ---------------------------------------------------------

var (
	// afterFloor and beforeFloor straddle BackfillFloor, which is what the
	// clamp is decided on.
	afterFloor  = BackfillFloorTime().Add(48 * time.Hour)
	beforeFloor = BackfillFloorTime().Add(-48 * time.Hour)
)

const (
	testHistoryDMJID    = "15559990001@s.whatsapp.net"
	testHistoryGroupJID = "120363000000000009@g.us"
	testHistoryLIDJID   = "88800000009@lid"
)

// webMessage builds a conversational turn.
func webMessage(id string, sentAt time.Time, body string) *waWeb.WebMessageInfo {
	return &waWeb.WebMessageInfo{
		Key:              &waCommon.MessageKey{ID: proto.String(id)},
		MessageTimestamp: proto.Uint64(uint64(sentAt.Unix())),
		Message:          &waE2E.Message{Conversation: proto.String(body)},
	}
}

// stubEntry builds a NON-message: the envelope WhatsApp reuses for a revoke, an
// undecryptable ciphertext, a missed call or a membership notice. Message is
// nil and the stub type says which.
func stubEntry(id string, sentAt time.Time, stub waWeb.WebMessageInfo_StubType) *waWeb.WebMessageInfo {
	return &waWeb.WebMessageInfo{
		Key:              &waCommon.MessageKey{ID: proto.String(id)},
		MessageTimestamp: proto.Uint64(uint64(sentAt.Unix())),
		MessageStubType:  stub.Enum(),
	}
}

func withStubType(msg *waWeb.WebMessageInfo, stub waWeb.WebMessageInfo_StubType) *waWeb.WebMessageInfo {
	msg.MessageStubType = stub.Enum()
	return msg
}

func conversation(id string, msgs ...*waWeb.WebMessageInfo) *waHistorySync.Conversation {
	conv := &waHistorySync.Conversation{ID: proto.String(id)}
	for _, m := range msgs {
		conv.Messages = append(conv.Messages, &waHistorySync.HistorySyncMsg{Message: m})
	}
	return conv
}

func withParticipants(conv *waHistorySync.Conversation, name string, n int) *waHistorySync.Conversation {
	conv.Name = proto.String(name)
	for i := 0; i < n; i++ {
		conv.Participant = append(conv.Participant, &waHistorySync.GroupParticipant{
			UserJID: proto.String(fmt.Sprintf("1555000%04d@s.whatsapp.net", i)),
		})
	}
	return conv
}

func chunk(convs ...*waHistorySync.Conversation) *waHistorySync.HistorySync {
	syncType := waHistorySync.HistorySync_FULL
	return &waHistorySync.HistorySync{SyncType: &syncType, Conversations: convs}
}

// --- fakes ------------------------------------------------------------------

// fakeHistoryStore is an in-memory notification inbox with the SAME fencing the
// real one has: every write presents the claim token, and a phase advance also
// presents its predecessor. Without both, the drainer's lease-loss and
// resume-at-phase branches could not be driven.
type fakeHistoryStore struct {
	rows []*repository.HistoryNotification

	claims      int
	checkpoints int
	dones       int
	failures    []string
	claimErr    error
	// onSaveCheckpoint runs just before a checkpoint write, so a test can make
	// a successor steal the lease exactly mid-projection.
	onSaveCheckpoint func()
}

func (f *fakeHistoryStore) add(n *repository.HistoryNotification) *repository.HistoryNotification {
	if n.ID == uuid.Nil {
		n.ID = uuid.New()
	}
	if n.State == "" {
		n.State = repository.HistoryNotificationStatePending
	}
	if n.Phase == "" {
		n.Phase = repository.HistoryPhaseRecorded
	}
	if n.Disposition == "" {
		n.Disposition = repository.HistoryDispositionProject
	}
	if n.Checkpoint == nil {
		// The column is JSONB NOT NULL DEFAULT '{}', so a row that has staged
		// nothing still carries two bytes. Reproducing that here is the whole
		// point: a byte test on the checkpoint would pass against nil.
		n.Checkpoint = []byte("{}")
	}
	f.rows = append(f.rows, n)
	return n
}

func (f *fakeHistoryStore) find(id uuid.UUID) *repository.HistoryNotification {
	for _, row := range f.rows {
		if row.ID == id {
			return row
		}
	}
	return nil
}

func (f *fakeHistoryStore) ClaimNextNotification(context.Context) (*repository.HistoryNotification, error) {
	if f.claimErr != nil {
		return nil, f.claimErr
	}
	for _, row := range f.rows {
		if row.State != repository.HistoryNotificationStatePending {
			continue
		}
		f.claims++
		row.State = repository.HistoryNotificationStateProcessing
		row.Attempts++
		token := uuid.New()
		row.ClaimToken = &token
		out := *row
		return &out, nil
	}
	return nil, db.ErrNotFound
}

func (f *fakeHistoryStore) held(id, token uuid.UUID) *repository.HistoryNotification {
	row := f.find(id)
	if row == nil || row.ClaimToken == nil || *row.ClaimToken != token {
		return nil
	}
	return row
}

func (f *fakeHistoryStore) SaveCheckpoint(_ context.Context, id, token uuid.UUID, checkpoint []byte) (bool, error) {
	if f.onSaveCheckpoint != nil {
		f.onSaveCheckpoint()
	}
	row := f.held(id, token)
	if row == nil {
		return false, nil
	}
	f.checkpoints++
	row.Checkpoint = checkpoint
	return true, nil
}

func (f *fakeHistoryStore) AdvancePhase(_ context.Context, id, token uuid.UUID, from, to string) (bool, error) {
	row := f.held(id, token)
	if row == nil || row.Phase != from {
		return false, nil
	}
	row.Phase = to
	return true, nil
}

func (f *fakeHistoryStore) MarkNotificationDone(_ context.Context, id, token uuid.UUID) (bool, error) {
	row := f.held(id, token)
	if row == nil || row.Phase != repository.HistoryPhaseDeleted {
		return false, nil
	}
	f.dones++
	row.State = repository.HistoryNotificationStateDone
	return true, nil
}

func (f *fakeHistoryStore) MarkNotificationFailed(_ context.Context, id, token uuid.UUID, errMsg string) (bool, error) {
	row := f.held(id, token)
	if row == nil {
		return false, nil
	}
	f.failures = append(f.failures, errMsg)
	row.State = repository.HistoryNotificationStateFailed
	row.LastError = &errMsg
	return true, nil
}

// recordingHistoryFetcher records the ORDER of the seam calls, which is how the
// protocol's ordering claims are asserted, and returns canned chunks/errors.
type recordingHistoryFetcher struct {
	calls []string

	chunk       *waHistorySync.HistorySync
	downloadErr error
	ackErr      error
	deleteErr   error

	account string

	// projected is every message id ProjectHistoryMessage was asked about, in
	// order — the discriminator for "the clamp ran BEFORE the parser".
	projected  []string
	projectErr map[string]error
	ineligible map[string]bool
}

func newRecordingFetcher(c *waHistorySync.HistorySync) *recordingHistoryFetcher {
	return &recordingHistoryFetcher{
		chunk:      c,
		account:    testAccountJID,
		projectErr: map[string]error{},
		ineligible: map[string]bool{},
	}
}

func (f *recordingHistoryFetcher) DownloadHistorySync(context.Context, []byte) (*waHistorySync.HistorySync, error) {
	f.calls = append(f.calls, "download")
	if f.downloadErr != nil {
		return nil, f.downloadErr
	}
	return f.chunk, nil
}

func (f *recordingHistoryFetcher) AckHistorySync(context.Context, string) error {
	f.calls = append(f.calls, "ack")
	return f.ackErr
}

func (f *recordingHistoryFetcher) DeleteHistoryMedia(context.Context, []byte) error {
	f.calls = append(f.calls, "delete")
	return f.deleteErr
}

func (f *recordingHistoryFetcher) AccountJID() string { return f.account }

func (f *recordingHistoryFetcher) ProjectHistoryMessage(_ context.Context, chatJID string, web *waWeb.WebMessageInfo) (IngestedMessage, bool, error) {
	id := web.GetKey().GetID()
	f.projected = append(f.projected, id)
	if err := f.projectErr[id]; err != nil {
		return IngestedMessage{}, false, err
	}
	if f.ineligible[id] {
		return IngestedMessage{}, false, nil
	}
	chatType := ChatTypePrivate
	if strings.HasSuffix(chatJID, "@g.us") {
		chatType = ChatTypeGroup
	}
	account := f.account
	body := web.GetMessage().GetConversation()
	return IngestedMessage{
		MessageID:   id,
		ChatJID:     chatJID,
		ChatType:    chatType,
		SentAt:      time.Unix(int64(web.GetMessageTimestamp()), 0).UTC(),
		Body:        &body,
		MessageType: MessageTypeText,
		AccountJID:  &account,
	}, true, nil
}

var _ HistoryFetcher = (*recordingHistoryFetcher)(nil)

// drainEnv is one unit test's drainer over fakes.
type drainEnv struct {
	store    *fakeHistoryStore
	fetcher  *recordingHistoryFetcher
	ingestor *fakeIngestor
	gate     *ChatGate
	group    *fakeGroupInfoFetcher
	drainer  *HistoryDrainer
}

func newDrainEnv(t *testing.T, c *waHistorySync.HistorySync) *drainEnv {
	t.Helper()
	env := &drainEnv{
		store:    &fakeHistoryStore{},
		fetcher:  newRecordingFetcher(c),
		ingestor: &fakeIngestor{},
		group:    &fakeGroupInfoFetcher{info: &ChatGroupInfo{Title: "Book Club", MemberCount: 4}, account: testAccountJID},
	}
	env.gate = gateWith(&fakeChatConfigStore{}, env.group, 10)
	env.drainer = NewHistoryDrainer(env.store, env.ingestor, env.gate, func() HistoryFetcher { return env.fetcher })
	// Pacing is real time; a unit test proving protocol order should not spend
	// it. The field exists precisely so this needs no mutable global.
	env.drainer.pacing = 0
	return env
}

func (e *drainEnv) ingestedIDs() []string {
	out := make([]string, 0, len(e.ingestor.messages))
	for _, m := range e.ingestor.messages {
		out = append(out, m.MessageID)
	}
	return out
}

// --- the clamp --------------------------------------------------------------

// TestHistoryDrainer_ClampDropsPreHorizonMessages: a conversation straddling
// the horizon stages only the newer messages, and the older ones never reach
// the PARSER at all — the strongest form of "discarded, not stored".
func TestHistoryDrainer_ClampDropsPreHorizonMessages(t *testing.T) {
	env := newDrainEnv(t, chunk(conversation(testHistoryDMJID,
		webMessage("old-1", beforeFloor, "ancient"),
		webMessage("new-1", afterFloor, "recent"),
		webMessage("old-2", beforeFloor.Add(-time.Hour), "older still"),
		webMessage("new-2", afterFloor.Add(time.Hour), "also recent"),
	)))
	env.store.add(&repository.HistoryNotification{ProtocolMsgID: "pm-1"})

	require.NoError(t, env.drainer.Drain(context.Background()))

	assert.Equal(t, []string{"new-1", "new-2"}, env.ingestedIDs())
	assert.Equal(t, []string{"new-1", "new-2"}, env.fetcher.projected,
		"a pre-clamp message is never parsed, so its body cannot reach any writer")
}

// --- protocol order ---------------------------------------------------------

// TestHistoryDrainer_DeletesMediaOnlyAfterAck pins D10's ordering. Deleting
// before acknowledging would destroy the payload while WhatsApp still believes
// the chunk is outstanding.
func TestHistoryDrainer_DeletesMediaOnlyAfterAck(t *testing.T) {
	env := newDrainEnv(t, chunk(conversation(testHistoryDMJID, webMessage("m-1", afterFloor, "hi"))))
	row := env.store.add(&repository.HistoryNotification{ProtocolMsgID: "pm-1"})

	require.NoError(t, env.drainer.Drain(context.Background()))

	assert.Equal(t, []string{"download", "ack", "delete"}, env.fetcher.calls)
	assert.Equal(t, repository.HistoryNotificationStateDone, env.store.find(row.ID).State)
}

// TestHistoryDrainer_DoesNotAckOrDeleteOnProjectionFailure: a staging failure
// is transient, so the receipt is withheld and the payload is left on the
// server for the retry.
func TestHistoryDrainer_DoesNotAckOrDeleteOnProjectionFailure(t *testing.T) {
	env := newDrainEnv(t, chunk(conversation(testHistoryDMJID, webMessage("m-1", afterFloor, "hi"))))
	row := env.store.add(&repository.HistoryNotification{ProtocolMsgID: "pm-1"})
	env.ingestor.setErr(errors.New("pool exhausted"))

	err := env.drainer.Drain(context.Background())

	require.Error(t, err)
	assert.Equal(t, []string{"download"}, env.fetcher.calls)
	stored := env.store.find(row.ID)
	assert.Equal(t, repository.HistoryPhaseDownloaded, stored.Phase, "no phase advance past the download")
	assert.Equal(t, repository.HistoryNotificationStateProcessing, stored.State, "no terminal write")
	assert.Empty(t, env.store.failures)
}

// TestHistoryDrainer_CompletesAFreshChunkInOneClaim is the fall-through
// property. Written as a switch with one arm per claim, a fresh chunk would
// need four claims — and since each leaves a live 15-minute lease, roughly an
// hour per chunk.
func TestHistoryDrainer_CompletesAFreshChunkInOneClaim(t *testing.T) {
	env := newDrainEnv(t, chunk(conversation(testHistoryDMJID, webMessage("m-1", afterFloor, "hi"))))
	row := env.store.add(&repository.HistoryNotification{ProtocolMsgID: "pm-1"})

	require.NoError(t, env.drainer.Drain(context.Background()))

	assert.Equal(t, 1, env.store.claims, "one claim carries a fresh chunk all the way to done")
	assert.Equal(t, repository.HistoryNotificationStateDone, env.store.find(row.ID).State)
}

// TestHistoryDrainer_DrainsTheWholeBacklogInOneRun: a bootstrap sync arrives as
// many chunks, and one per tick would take an hour of wall time for no reason.
func TestHistoryDrainer_DrainsTheWholeBacklogInOneRun(t *testing.T) {
	env := newDrainEnv(t, chunk(conversation(testHistoryDMJID, webMessage("m-1", afterFloor, "hi"))))
	for i := 0; i < 3; i++ {
		env.store.add(&repository.HistoryNotification{ProtocolMsgID: fmt.Sprintf("pm-%d", i)})
	}

	require.NoError(t, env.drainer.Drain(context.Background()))

	for _, row := range env.store.rows {
		assert.Equal(t, repository.HistoryNotificationStateDone, row.State)
	}
	assert.Equal(t, 3, env.store.dones)
}

// TestHistoryDrainer_ChunkWithNoConversationsCompletes: several HistorySync
// types (PUSH_NAME, INITIAL_STATUS_V3) carry no conversations at all. Zero
// staged rows is the CORRECT outcome for them, not a failure or a stall.
func TestHistoryDrainer_ChunkWithNoConversationsCompletes(t *testing.T) {
	env := newDrainEnv(t, chunk())
	row := env.store.add(&repository.HistoryNotification{ProtocolMsgID: "pm-1", SyncType: "PUSH_NAME"})

	require.NoError(t, env.drainer.Drain(context.Background()))

	assert.Equal(t, repository.HistoryNotificationStateDone, env.store.find(row.ID).State)
	assert.Equal(t, []string{"download", "ack", "delete"}, env.fetcher.calls)
	assert.Zero(t, env.ingestor.count())
}

// --- the gate pre-pass ------------------------------------------------------

// TestHistoryDrainer_ResolvesEachGroupExactlyOnce: the pre-pass costs one
// metadata lookup per DISTINCT group, not one per message.
//
// It makes no ordering claim — neither fake here records a sequence, so it
// could not tell a pre-pass from a per-conversation lazy gate. The ordering
// property is asserted by TestHistoryDrainer_ResolvesEveryGroupBeforeTheFirstIngest
// (a shared call log) and by _FailsClosedOnUnresolvableGroup (a DM ahead of an
// undecidable group stores nothing).
func TestHistoryDrainer_ResolvesEachGroupExactlyOnce(t *testing.T) {
	env := newDrainEnv(t, chunk(
		withParticipants(conversation(testHistoryGroupJID, webMessage("g-1", afterFloor, "hi")), "Book Club", 4),
		conversation(testHistoryDMJID, webMessage("d-1", afterFloor, "hey")),
	))
	env.store.add(&repository.HistoryNotification{ProtocolMsgID: "pm-1"})

	env.group.info = &ChatGroupInfo{Title: "Book Club", MemberCount: 4}

	require.NoError(t, env.drainer.Drain(context.Background()))

	assert.Equal(t, 1, env.group.calls, "one lookup per distinct group, however many messages it carries")
	assert.Equal(t, []string{"g-1", "d-1"}, env.ingestedIDs())
}

// TestHistoryDrainer_FailsClosedOnUnresolvableGroup: an undecidable group
// aborts the WHOLE chunk with nothing stored anywhere in it — including the DM
// that would otherwise have been perfectly projectable.
func TestHistoryDrainer_FailsClosedOnUnresolvableGroup(t *testing.T) {
	env := newDrainEnv(t, chunk(
		conversation(testHistoryDMJID, webMessage("d-1", afterFloor, "hey")),
		conversation(testHistoryGroupJID, webMessage("g-1", afterFloor, "hi")),
	))
	row := env.store.add(&repository.HistoryNotification{ProtocolMsgID: "pm-1"})
	env.group.info = nil
	env.group.err = errors.New("socket closed")

	err := env.drainer.Drain(context.Background())

	require.Error(t, err)
	assert.ErrorIs(t, err, ErrChatGateUndecided)
	assert.Zero(t, env.ingestor.count(), "nothing in the chunk is stored, not even the decidable DM")
	assert.Equal(t, []string{"download"}, env.fetcher.calls)
	assert.Equal(t, repository.HistoryNotificationStateProcessing, env.store.find(row.ID).State)
}

// TestHistoryDrainer_SkipsIneligibleChats: broadcast, status and newsletter
// conversations are skipped without a projection call at all.
func TestHistoryDrainer_SkipsIneligibleChats(t *testing.T) {
	env := newDrainEnv(t, chunk(
		conversation("status@broadcast", webMessage("s-1", afterFloor, "status")),
		conversation("1234@newsletter", webMessage("n-1", afterFloor, "news")),
		conversation("not a jid at all", webMessage("x-1", afterFloor, "junk")),
		conversation(testHistoryDMJID, webMessage("d-1", afterFloor, "hey")),
	))
	env.store.add(&repository.HistoryNotification{ProtocolMsgID: "pm-1"})

	require.NoError(t, env.drainer.Drain(context.Background()))

	assert.Equal(t, []string{"d-1"}, env.fetcher.projected)
	assert.Equal(t, []string{"d-1"}, env.ingestedIDs())
}

// TestHistoryDrainer_LIDDMIsKeyedOnThePhoneNumberThread: history that keys a DM
// on a LID is projected under the phone-number thread the chunk itself names,
// so it lands under the same thread_id live messages for that peer use.
func TestHistoryDrainer_LIDDMIsKeyedOnThePhoneNumberThread(t *testing.T) {
	conv := conversation(testHistoryLIDJID, webMessage("m-1", afterFloor, "hi"))
	conv.PnJID = proto.String(testHistoryDMJID)
	env := newDrainEnv(t, chunk(conv))
	env.store.add(&repository.HistoryNotification{ProtocolMsgID: "pm-1"})

	require.NoError(t, env.drainer.Drain(context.Background()))

	require.Equal(t, 1, env.ingestor.count())
	assert.Equal(t, testHistoryDMJID, env.ingestor.first().ChatJID)
}

// TestHistoryDrainer_LIDDMWithoutAPnJIDKeepsItsOwnThread records the accepted
// residual: with no pnJID in the chunk there is nothing to canonicalize to, and
// inventing one would be worse than the bounded burst-coalescing cost.
func TestHistoryDrainer_LIDDMWithoutAPnJIDKeepsItsOwnThread(t *testing.T) {
	env := newDrainEnv(t, chunk(conversation(testHistoryLIDJID, webMessage("m-1", afterFloor, "hi"))))
	env.store.add(&repository.HistoryNotification{ProtocolMsgID: "pm-1"})

	require.NoError(t, env.drainer.Drain(context.Background()))

	require.Equal(t, 1, env.ingestor.count())
	assert.Equal(t, testHistoryLIDJID, env.ingestor.first().ChatJID)
}

// --- stub and system entries ------------------------------------------------

// TestHistoryDrainer_StubAndSystemEntriesAreNeverStaged.
//
// A WebMessageInfo is also WhatsApp's envelope for NON-messages. Nothing
// downstream rejects them: the library parses them, the classifier's Drop sits
// at its zero value, and the projection returns eligible with a real id and
// timestamp. Staged, each would have become a bodiless "other" row, been swept,
// and fabricated an interaction that moves last_contacted and cadence.
func TestHistoryDrainer_StubAndSystemEntriesAreNeverStaged(t *testing.T) {
	env := newDrainEnv(t, chunk(conversation(testHistoryDMJID,
		webMessage("real-1", afterFloor, "actually said this"),
		stubEntry("revoke-1", afterFloor, waWeb.WebMessageInfo_REVOKE),
		stubEntry("call-1", afterFloor, waWeb.WebMessageInfo_CALL_MISSED_VOICE),
		stubEntry("cipher-1", afterFloor, waWeb.WebMessageInfo_CIPHERTEXT),
		stubEntry("member-1", afterFloor, waWeb.WebMessageInfo_GROUP_PARTICIPANT_ADD),
		webMessage("real-2", afterFloor, "and this"),
	)))
	env.store.add(&repository.HistoryNotification{ProtocolMsgID: "pm-1"})

	require.NoError(t, env.drainer.Drain(context.Background()))

	assert.Equal(t, []string{"real-1", "real-2"}, env.ingestedIDs())
	assert.Equal(t, []string{"real-1", "real-2"}, env.fetcher.projected,
		"a non-message is skipped before the parser, not filtered after it")
}

// TestHistoryDrainer_StubTypeWithContentIsStagedAndReported is the
// over-reach guard, and it is the falsification with an input the filter must
// MISS. Nothing in the pinned library confirms or refutes that a genuine turn
// can carry a stub type, and this payload is ONE-SHOT — so it is staged and
// reported rather than guessed away.
func TestHistoryDrainer_StubTypeWithContentIsStagedAndReported(t *testing.T) {
	env := newDrainEnv(t, chunk(conversation(testHistoryDMJID,
		withStubType(webMessage("hybrid-1", afterFloor, "a real body"), waWeb.WebMessageInfo_REVOKE),
	)))
	env.store.add(&repository.HistoryNotification{ProtocolMsgID: "pm-1"})

	require.NoError(t, env.drainer.Drain(context.Background()))

	require.Equal(t, 1, env.ingestor.count(), "a message with real content is never dropped on a stub type alone")
	assert.Equal(t, "hybrid-1", env.ingestor.first().MessageID)
}

// TestHistoryDrainer_MalformedMessageIsSkippedNotFatal: one bad row must not
// strand a chunk that is otherwise projectable.
func TestHistoryDrainer_MalformedMessageIsSkippedNotFatal(t *testing.T) {
	noID := webMessage("", afterFloor, "no key id")
	env := newDrainEnv(t, chunk(conversation(testHistoryDMJID,
		noID,
		webMessage("bad-decode", afterFloor, "will not parse"),
		webMessage("good-1", afterFloor, "fine"),
	)))
	env.fetcher.projectErr["bad-decode"] = errors.New("proto: bad wire format")
	row := env.store.add(&repository.HistoryNotification{ProtocolMsgID: "pm-1"})

	require.NoError(t, env.drainer.Drain(context.Background()))

	assert.Equal(t, []string{"good-1"}, env.ingestedIDs())
	assert.Equal(t, repository.HistoryNotificationStateDone, env.store.find(row.ID).State)
}

// TestHistoryDrainer_IneligibleProjectionIsNotStaged: a projection the parser
// declines (self-chat, non-conversational turn) is a DROP, not an error.
func TestHistoryDrainer_IneligibleProjectionIsNotStaged(t *testing.T) {
	env := newDrainEnv(t, chunk(conversation(testHistoryDMJID,
		webMessage("reaction-1", afterFloor, "unused"),
		webMessage("good-1", afterFloor, "fine"),
	)))
	env.fetcher.ineligible["reaction-1"] = true
	env.store.add(&repository.HistoryNotification{ProtocolMsgID: "pm-1"})

	require.NoError(t, env.drainer.Drain(context.Background()))

	assert.Equal(t, []string{"good-1"}, env.ingestedIDs())
}

// --- failure taxonomy -------------------------------------------------------

// TestHistoryDrainer_LIDMappingsIncompleteIsTransientAndRetries: history
// messages carry no alternative address, so LID-only peers depend entirely on
// the device store's mapping. Projecting before it verifies would attribute a
// resolvable peer as permanently unmatched.
func TestHistoryDrainer_LIDMappingsIncompleteIsTransientAndRetries(t *testing.T) {
	env := newDrainEnv(t, chunk(conversation(testHistoryDMJID, webMessage("m-1", afterFloor, "hi"))))
	row := env.store.add(&repository.HistoryNotification{ProtocolMsgID: "pm-1"})
	env.fetcher.downloadErr = fmt.Errorf("%w: no stored phone number", ErrLIDMappingsIncomplete)

	err := env.drainer.Drain(context.Background())
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrLIDMappingsIncomplete)
	stored := env.store.find(row.ID)
	assert.Equal(t, repository.HistoryPhaseRecorded, stored.Phase, "no phase advance")
	assert.Equal(t, repository.HistoryNotificationStateProcessing, stored.State, "no terminal write")
	assert.Zero(t, env.ingestor.count())

	// The retry, once the mappings land.
	env.fetcher.downloadErr = nil
	stored.State = repository.HistoryNotificationStatePending
	require.NoError(t, env.drainer.Drain(context.Background()))
	assert.Equal(t, repository.HistoryNotificationStateDone, env.store.find(row.ID).State)
	assert.Equal(t, 1, env.ingestor.count())
}

// TestHistoryDrainer_MediaMissingAtRecordedFails: nothing was ever stored and
// no retry can produce the blob, so the chunk is terminal and operator-visible.
func TestHistoryDrainer_MediaMissingAtRecordedFails(t *testing.T) {
	env := newDrainEnv(t, chunk(conversation(testHistoryDMJID, webMessage("m-1", afterFloor, "hi"))))
	row := env.store.add(&repository.HistoryNotification{ProtocolMsgID: "pm-1"})
	env.fetcher.downloadErr = fmt.Errorf("download: %w", whatsmeow.ErrMediaDownloadFailedWith404)

	require.NoError(t, env.drainer.Drain(context.Background()), "a decided failure is not the job's error")

	assert.Equal(t, repository.HistoryNotificationStateFailed, env.store.find(row.ID).State)
	assert.Equal(t, []string{"download"}, env.fetcher.calls, "no receipt for a chunk that stored nothing")
}

// TestHistoryDrainer_MediaMissingAtDownloadedWithNoCheckpointFails is ALSO the
// byte-test regression guard.
//
// The row's checkpoint is '{}' — the column is JSONB NOT NULL DEFAULT '{}' — so
// a len(Checkpoint) > 0 predicate would read it as "a conversation completed",
// complete the chunk, send its protocol receipt, and silently discard the
// one-shot history it never actually stored.
func TestHistoryDrainer_MediaMissingAtDownloadedWithNoCheckpointFails(t *testing.T) {
	env := newDrainEnv(t, chunk(conversation(testHistoryDMJID, webMessage("m-1", afterFloor, "hi"))))
	row := env.store.add(&repository.HistoryNotification{
		ProtocolMsgID: "pm-1",
		Phase:         repository.HistoryPhaseDownloaded,
		Checkpoint:    []byte("{}"),
	})
	env.fetcher.downloadErr = fmt.Errorf("download: %w", whatsmeow.ErrMediaDownloadFailedWith410)

	require.NoError(t, env.drainer.Drain(context.Background()))

	assert.Equal(t, repository.HistoryNotificationStateFailed, env.store.find(row.ID).State)
	assert.NotContains(t, env.fetcher.calls, "ack",
		"a chunk that stored nothing must not be acknowledged as handled")
}

// TestHistoryDrainer_MediaMissingAtDownloadedWithACheckpointCompletesSuccessfully:
// the content of the completed conversations is already in comms_message and no
// retry can produce the rest, so throwing the chunk away would discard real
// history to preserve a tidy failure state.
func TestHistoryDrainer_MediaMissingAtDownloadedWithACheckpointCompletesSuccessfully(t *testing.T) {
	env := newDrainEnv(t, chunk(conversation(testHistoryDMJID, webMessage("m-1", afterFloor, "hi"))))
	row := env.store.add(&repository.HistoryNotification{
		ProtocolMsgID: "pm-1",
		Phase:         repository.HistoryPhaseDownloaded,
		Checkpoint:    checkpointJSON(0, testHistoryDMJID, 3),
	})
	env.fetcher.downloadErr = fmt.Errorf("download: %w", whatsmeow.ErrMediaDownloadFailedWith403)

	require.NoError(t, env.drainer.Drain(context.Background()))

	stored := env.store.find(row.ID)
	assert.Equal(t, repository.HistoryNotificationStateDone, stored.State)
	assert.Equal(t, []string{"download", "ack", "delete"}, env.fetcher.calls)
	assert.Empty(t, env.store.failures)
}

// TestHistoryDrainer_DeleteFailureStillCompletesTheChunk.
//
// The errors are the ones whatsmeow's DeleteMedia really produces: a bare
// formatted string with no sentinel and no wrapped cause, so "already gone" and
// "request failed" are indistinguishable to the caller. Both must complete: by
// this phase the chunk is fully staged AND acknowledged, so retrying would burn
// the attempt budget and then record a COMPLETE chunk as permanently failed.
func TestHistoryDrainer_DeleteFailureStillCompletesTheChunk(t *testing.T) {
	for _, deleteErr := range []error{
		// The exact shapes upload.go produces.
		errors.New("media delete failed with status code 404"),
		errors.New("media delete failed with status code 500"),
		errors.New("failed to execute request: dial tcp: connection reset"),
	} {
		t.Run(deleteErr.Error(), func(t *testing.T) {
			env := newDrainEnv(t, chunk(conversation(testHistoryDMJID, webMessage("m-1", afterFloor, "hi"))))
			row := env.store.add(&repository.HistoryNotification{ProtocolMsgID: "pm-1"})
			env.fetcher.deleteErr = deleteErr

			require.NoError(t, env.drainer.Drain(context.Background()))

			stored := env.store.find(row.ID)
			assert.Equal(t, repository.HistoryNotificationStateDone, stored.State,
				"a fully staged, already-acknowledged chunk must never be recorded as failed")
			assert.Empty(t, env.store.failures)
			assert.Equal(t, 1, env.ingestor.count(), "and its messages are still stored")
		})
	}
}

// TestHistoryDrainer_DeleteIsNeverRetriedIntoAFailedChunk is the regression
// guard for the classification that could not fire.
//
// An earlier shape keyed the delete on whatsmeow's DOWNLOAD sentinels, which
// DeleteMedia cannot produce, so every delete failure was transient: past the
// attempt cap a complete chunk became permanently failed, and the operator
// remedy re-sent a receipt for a chunk WhatsApp had already been told about.
func TestHistoryDrainer_DeleteIsNeverRetriedIntoAFailedChunk(t *testing.T) {
	env := newDrainEnv(t, chunk(conversation(testHistoryDMJID, webMessage("m-1", afterFloor, "hi"))))
	row := env.store.add(&repository.HistoryNotification{ProtocolMsgID: "pm-1"})
	row.Attempts = maxTransientAttempts + 5 // long past the cap
	env.fetcher.deleteErr = errors.New("media delete failed with status code 410")

	require.NoError(t, env.drainer.Drain(context.Background()))

	assert.Equal(t, repository.HistoryNotificationStateDone, env.store.find(row.ID).State)
	assert.Empty(t, env.store.failures)
	assert.Equal(t, []string{"download", "ack", "delete"}, env.fetcher.calls,
		"exactly one receipt and one delete attempt")
}

// TestHistoryDrainer_MalformedNotificationIsTerminal: the stored bytes are what
// they are; no retry decodes them differently.
func TestHistoryDrainer_MalformedNotificationIsTerminal(t *testing.T) {
	env := newDrainEnv(t, chunk())
	row := env.store.add(&repository.HistoryNotification{ProtocolMsgID: "pm-1"})
	env.fetcher.downloadErr = fmt.Errorf("%w: unmarshal: bad wire", ErrHistoryNotificationMalformed)

	require.NoError(t, env.drainer.Drain(context.Background()))

	assert.Equal(t, repository.HistoryNotificationStateFailed, env.store.find(row.ID).State)
}

// TestHistoryDrainer_TransientFailuresAreCapped: without a cap, a
// permanently-transient chunk re-downloads its full payload every 15 minutes
// for the life of the deployment, visible only as a processing count that never
// moves.
func TestHistoryDrainer_TransientFailuresAreCapped(t *testing.T) {
	env := newDrainEnv(t, chunk())
	row := env.store.add(&repository.HistoryNotification{ProtocolMsgID: "pm-1"})
	env.fetcher.downloadErr = errors.New("timeout")

	// Under the cap: the failure is returned so River records it, and the row
	// stays recoverable.
	row.Attempts = maxTransientAttempts - 1
	require.Error(t, env.drainer.Drain(context.Background()))
	assert.Equal(t, repository.HistoryNotificationStateProcessing, env.store.find(row.ID).State)
	assert.Empty(t, env.store.failures)

	// Past the cap: the next transient failure becomes terminal, so it surfaces
	// in the status counts and the operator listing instead of grinding.
	row.State = repository.HistoryNotificationStatePending
	row.Attempts = maxTransientAttempts
	require.NoError(t, env.drainer.Drain(context.Background()))
	assert.Equal(t, repository.HistoryNotificationStateFailed, env.store.find(row.ID).State)
	require.Len(t, env.store.failures, 1)
	assert.Contains(t, env.store.failures[0], "gave up after")
}

// --- resume -----------------------------------------------------------------

// TestHistoryDrainer_ResumesAtEachPhase tables which steps a reclaimed worker
// re-executes and which it skips.
func TestHistoryDrainer_ResumesAtEachPhase(t *testing.T) {
	tests := []struct {
		phase     string
		wantCalls []string
	}{
		{repository.HistoryPhaseRecorded, []string{"download", "ack", "delete"}},
		{repository.HistoryPhaseDownloaded, []string{"download", "ack", "delete"}},
		{repository.HistoryPhaseProjected, []string{"ack", "delete"}},
		{repository.HistoryPhaseAcked, []string{"delete"}},
		{repository.HistoryPhaseDeleted, nil},
	}

	for _, tc := range tests {
		t.Run(tc.phase, func(t *testing.T) {
			env := newDrainEnv(t, chunk(conversation(testHistoryDMJID, webMessage("m-1", afterFloor, "hi"))))
			row := env.store.add(&repository.HistoryNotification{ProtocolMsgID: "pm-1", Phase: tc.phase})

			require.NoError(t, env.drainer.Drain(context.Background()))

			assert.Equal(t, tc.wantCalls, env.fetcher.calls)
			assert.Equal(t, repository.HistoryNotificationStateDone, env.store.find(row.ID).State)
		})
	}
}

// TestHistoryDrainer_ResumesAfterCrashBetweenDeleteAndMarkDone: a worker killed
// between the delete and the completion re-runs neither, and still completes.
func TestHistoryDrainer_ResumesAfterCrashBetweenDeleteAndMarkDone(t *testing.T) {
	env := newDrainEnv(t, chunk(conversation(testHistoryDMJID, webMessage("m-1", afterFloor, "hi"))))
	row := env.store.add(&repository.HistoryNotification{ProtocolMsgID: "pm-1", Phase: repository.HistoryPhaseDeleted})

	require.NoError(t, env.drainer.Drain(context.Background()))

	assert.Empty(t, env.fetcher.calls, "no re-download, no second receipt, no second delete")
	assert.Zero(t, env.ingestor.count())
	assert.Equal(t, repository.HistoryNotificationStateDone, env.store.find(row.ID).State)
}

// TestHistoryDrainer_ResumeSkipsCompletedConversations: the stored index is the
// LAST COMPLETED conversation, so the loop restarts at index+1. Returning the
// index unchanged would re-walk a conversation that already finished.
func TestHistoryDrainer_ResumeSkipsCompletedConversations(t *testing.T) {
	env := newDrainEnv(t, chunk(
		conversation("15551110001@s.whatsapp.net", webMessage("a-1", afterFloor, "one")),
		conversation("15551110002@s.whatsapp.net", webMessage("b-1", afterFloor, "two")),
		conversation("15551110003@s.whatsapp.net", webMessage("c-1", afterFloor, "three")),
	))
	env.store.add(&repository.HistoryNotification{
		ProtocolMsgID: "pm-1",
		Phase:         repository.HistoryPhaseDownloaded,
		Checkpoint:    checkpointJSON(1, "15551110002@s.whatsapp.net", 2),
	})

	require.NoError(t, env.drainer.Drain(context.Background()))

	assert.Equal(t, []string{"c-1"}, env.ingestedIDs())
}

// TestHistoryDrainer_CheckpointMismatchReprojectsFromTheStart turns "two
// downloads decode to the same conversation order" from an assumption into a
// verified precondition. Re-projecting is safe because the staging upsert is
// content-immutable on conflict.
func TestHistoryDrainer_CheckpointMismatchReprojectsFromTheStart(t *testing.T) {
	env := newDrainEnv(t, chunk(
		conversation("15551110001@s.whatsapp.net", webMessage("a-1", afterFloor, "one")),
		conversation("15551110002@s.whatsapp.net", webMessage("b-1", afterFloor, "two")),
	))
	env.store.add(&repository.HistoryNotification{
		ProtocolMsgID: "pm-1",
		Phase:         repository.HistoryPhaseDownloaded,
		Checkpoint:    checkpointJSON(0, "15559999999@s.whatsapp.net", 1),
	})

	require.NoError(t, env.drainer.Drain(context.Background()))

	assert.Equal(t, []string{"a-1", "b-1"}, env.ingestedIDs())
}

// TestHistoryDrainer_CheckpointIsSemanticNotBytes pins the predicate directly:
// only a checkpoint that PARSES to a conversation index counts as progress.
func TestHistoryDrainer_CheckpointIsSemanticNotBytes(t *testing.T) {
	for _, raw := range [][]byte{nil, []byte("{}"), []byte(`{"chat_jid":"x"}`), []byte("not json")} {
		_, ok := parseHistoryCheckpoint(raw)
		assert.False(t, ok, "%q must not read as a completed conversation", raw)
	}
	cp, ok := parseHistoryCheckpoint(checkpointJSON(0, testHistoryDMJID, 4))
	require.True(t, ok, "conversation 0 IS progress, which a value-typed index could not express")
	assert.Equal(t, 0, *cp.ConversationIndex)
	assert.Equal(t, testHistoryDMJID, cp.ChatJID)
	assert.Equal(t, 4, cp.Staged)
}

// TestHistoryDrainer_AbandonsTheChunkWhenTheLeaseIsLost: a lease that moved on
// means a successor is doing our work, so we write nothing further.
func TestHistoryDrainer_AbandonsTheChunkWhenTheLeaseIsLost(t *testing.T) {
	env := newDrainEnv(t, chunk(conversation(testHistoryDMJID, webMessage("m-1", afterFloor, "hi"))))
	row := env.store.add(&repository.HistoryNotification{ProtocolMsgID: "pm-1"})

	// The successor reclaims mid-flight: a fresh token lands on the row while
	// this worker is still projecting, so its checkpoint write is fenced out.
	stolen := uuid.New()
	env.store.onSaveCheckpoint = func() {
		env.store.find(row.ID).ClaimToken = &stolen
	}

	require.NoError(t, env.drainer.Drain(context.Background()), "a lost lease is not a job failure")

	stored := env.store.find(row.ID)
	assert.Equal(t, repository.HistoryNotificationStateProcessing, stored.State,
		"the successor owns the row; we wrote no terminal state")
	assert.Equal(t, repository.HistoryPhaseDownloaded, stored.Phase, "and no further phase advance")
	assert.Empty(t, env.store.failures)
	assert.Zero(t, env.store.dones)
}

// --- dropped-inline chunks --------------------------------------------------

// TestHistoryDrainer_DroppedInlineChunkSkipsDownloadAndDelete: the payload was
// discarded at capture, so there is nothing to download and nothing to delete —
// but the protocol receipt still has to go out exactly once.
func TestHistoryDrainer_DroppedInlineChunkSkipsDownloadAndDelete(t *testing.T) {
	env := newDrainEnv(t, chunk(conversation(testHistoryDMJID, webMessage("m-1", afterFloor, "hi"))))
	row := env.store.add(&repository.HistoryNotification{
		ProtocolMsgID: "pm-1",
		Disposition:   repository.HistoryDispositionDroppedInline,
		// PR3's SQL derives this starting phase from the disposition.
		Phase: repository.HistoryPhaseProjected,
	})

	require.NoError(t, env.drainer.Drain(context.Background()))

	assert.Equal(t, []string{"ack"}, env.fetcher.calls)
	assert.Zero(t, env.ingestor.count(), "a dropped chunk is never projected")
	assert.Equal(t, repository.HistoryNotificationStateDone, env.store.find(row.ID).State)
}

// --- deferral ---------------------------------------------------------------

// TestHistoryDrainer_DefersWithoutClaimingWhenNoClientIsConnected covers the
// three shapes of "nothing to drain with", including the nil FUNC VALUE the
// composition root really produces on a boot where the device store failed —
// which would otherwise panic once a minute, forever.
func TestHistoryDrainer_DefersWithoutClaimingWhenNoClientIsConnected(t *testing.T) {
	tests := []struct {
		name     string
		source   func() HistoryFetcher
		ingestor MessageIngestor
	}{
		{"nil fetcher func", nil, &fakeIngestor{}},
		{"func returning nil", func() HistoryFetcher { return nil }, &fakeIngestor{}},
		{"nil ingestor", func() HistoryFetcher { return newRecordingFetcher(chunk()) }, nil},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := &fakeHistoryStore{}
			row := store.add(&repository.HistoryNotification{ProtocolMsgID: "pm-1"})
			drainer := NewHistoryDrainer(store, tc.ingestor, nil, tc.source)

			require.NotPanics(t, func() {
				require.NoError(t, drainer.Drain(context.Background()))
			})

			assert.Zero(t, store.claims, "deferring must not claim, or the lease burns for nothing")
			assert.Equal(t, repository.HistoryNotificationStatePending, row.State)
			assert.Zero(t, row.Attempts)
		})
	}
}

// TestHistoryDrainer_StopsOnACancelledContext: the 6-minute job timeout
// cancelling us between chunks is the ordinary resume path, not a failure.
func TestHistoryDrainer_StopsOnACancelledContext(t *testing.T) {
	env := newDrainEnv(t, chunk())
	env.store.add(&repository.HistoryNotification{ProtocolMsgID: "pm-1"})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	require.NoError(t, env.drainer.Drain(ctx))
	assert.Zero(t, env.store.claims)
}

// TestHistoryDrainer_ClaimFailureIsReturned: a database failure on the claim is
// the job's error, so a stuck backfill is visible rather than silent.
func TestHistoryDrainer_ClaimFailureIsReturned(t *testing.T) {
	env := newDrainEnv(t, chunk())
	env.store.claimErr = errors.New("pool exhausted")

	require.Error(t, env.drainer.Drain(context.Background()))
}

// TestHistoryDrainer_MediaMissingAtDownloadedWithAnEmptyCheckpointFails is the
// second half of the media-gone predicate, and the reason it counts STAGED rows
// rather than completed conversations.
//
// The loop checkpoints after every TRACKED conversation, including one whose
// messages were all pre-horizon — so a chunk can hold a perfectly valid
// checkpoint while having stored NOTHING. Completing it would send a receipt
// for history that was never stored and lose the rest of a one-shot payload.
func TestHistoryDrainer_MediaMissingAtDownloadedWithAnEmptyCheckpointFails(t *testing.T) {
	env := newDrainEnv(t, chunk(conversation(testHistoryDMJID, webMessage("m-1", afterFloor, "hi"))))
	row := env.store.add(&repository.HistoryNotification{
		ProtocolMsgID: "pm-1",
		Phase:         repository.HistoryPhaseDownloaded,
		// A completed conversation that staged nothing.
		Checkpoint: checkpointJSON(0, testHistoryDMJID, 0),
	})
	env.fetcher.downloadErr = fmt.Errorf("download: %w", whatsmeow.ErrMediaDownloadFailedWith410)

	require.NoError(t, env.drainer.Drain(context.Background()))

	assert.Equal(t, repository.HistoryNotificationStateFailed, env.store.find(row.ID).State)
	assert.NotContains(t, env.fetcher.calls, "ack",
		"a chunk that stored nothing must not be acknowledged, whatever its checkpoint says")
}

// TestHistoryDrainer_CheckpointStagedCountSurvivesAResume: the count is
// CUMULATIVE, so a chunk interrupted twice does not under-report what it stored
// and then fail the media-gone predicate it should pass.
func TestHistoryDrainer_CheckpointStagedCountSurvivesAResume(t *testing.T) {
	env := newDrainEnv(t, chunk(
		conversation("15551110001@s.whatsapp.net", webMessage("a-1", afterFloor, "one")),
		conversation("15551110002@s.whatsapp.net", webMessage("b-1", afterFloor, "two")),
	))
	row := env.store.add(&repository.HistoryNotification{
		ProtocolMsgID: "pm-1",
		Phase:         repository.HistoryPhaseDownloaded,
		Checkpoint:    checkpointJSON(0, "15551110001@s.whatsapp.net", 1),
	})

	require.NoError(t, env.drainer.Drain(context.Background()))

	cp, ok := parseHistoryCheckpoint(env.store.find(row.ID).Checkpoint)
	require.True(t, ok)
	assert.Equal(t, 2, cp.Staged, "one row from the first run plus one from this one")
}

// TestHistoryDrainer_UndatedMessagesAreNotCountedAsPreHorizon.
//
// An absent timestamp reads as 0, which converts to 1970 and would be silently
// folded into the clamp's count — reporting undatable history as history the
// horizon deliberately excluded, on a one-shot payload where the two call for
// different follow-up.
func TestHistoryDrainer_UndatedMessagesAreNotCountedAsPreHorizon(t *testing.T) {
	undated := webMessage("undated-1", afterFloor, "no timestamp")
	undated.MessageTimestamp = nil

	env := newDrainEnv(t, chunk(conversation(testHistoryDMJID,
		undated,
		webMessage("old-1", beforeFloor, "genuinely ancient"),
		webMessage("good-1", afterFloor, "fine"),
	)))
	env.store.add(&repository.HistoryNotification{ProtocolMsgID: "pm-1"})

	require.NoError(t, env.drainer.Drain(context.Background()))

	assert.Equal(t, []string{"good-1"}, env.ingestedIDs(),
		"an undatable message cannot be placed on a timeline, so it is not stored either")
	assert.Equal(t, []string{"good-1"}, env.fetcher.projected)

	// Both are skipped, so the row set alone cannot tell them apart. The
	// DIAGNOSIS is the thing under test, and it is decided here.
	floor := BackfillFloorTime()
	assert.Equal(t, timestampUndated, classifyTimestamp(0, floor),
		"an absent timestamp must not be reported as history the horizon excluded")
	assert.Equal(t, timestampPreHorizon, classifyTimestamp(uint64(beforeFloor.Unix()), floor))
	assert.Equal(t, timestampInHorizon, classifyTimestamp(uint64(afterFloor.Unix()), floor))
}

// --- ordering ---------------------------------------------------------------

// sequencedGroupInfo and sequencedIngestor share one call log, which is what
// makes an ORDER assertion possible: neither fake alone can see the other.
type sequencedGroupInfo struct {
	inner *fakeGroupInfoFetcher
	log   *[]string
}

func (s *sequencedGroupInfo) GroupInfo(ctx context.Context, chatJID string) (*ChatGroupInfo, error) {
	*s.log = append(*s.log, "gate")
	return s.inner.GroupInfo(ctx, chatJID)
}
func (s *sequencedGroupInfo) AccountJID() string { return s.inner.AccountJID() }

type sequencedIngestor struct {
	inner *fakeIngestor
	log   *[]string
}

func (s *sequencedIngestor) IngestMessage(ctx context.Context, msg IngestedMessage) error {
	*s.log = append(*s.log, "ingest")
	return s.inner.IngestMessage(ctx, msg)
}

// TestHistoryDrainer_ResolvesEveryGroupBeforeTheFirstIngest asserts the ORDER
// directly: every gate lookup in the chunk precedes every ingest call.
//
// That is what "a chunk is never half-gated" means, and it is the property a
// per-conversation lazy gate would break while still producing the same rows
// and the same lookup count.
func TestHistoryDrainer_ResolvesEveryGroupBeforeTheFirstIngest(t *testing.T) {
	var log []string
	group := &fakeGroupInfoFetcher{info: &ChatGroupInfo{Title: "Book Club", MemberCount: 4}, account: testAccountJID}
	ingestor := &fakeIngestor{}

	gate := NewChatGate(&fakeChatConfigStore{}, 10)
	gate.BindGroupInfoSource(func() GroupInfoFetcher { return &sequencedGroupInfo{inner: group, log: &log} })
	gate.lookupTimeout = testLookupTimeout

	fetcher := newRecordingFetcher(chunk(
		// A DM first, so a lazy gate would ingest before it ever looked one up.
		conversation(testHistoryDMJID, webMessage("d-1", afterFloor, "hey")),
		withParticipants(conversation(testHistoryGroupJID, webMessage("g-1", afterFloor, "hi")), "Book Club", 4),
		conversation("15551110009@s.whatsapp.net", webMessage("d-2", afterFloor, "hello")),
	))
	store := &fakeHistoryStore{}
	store.add(&repository.HistoryNotification{ProtocolMsgID: "pm-1"})

	drainer := NewHistoryDrainer(store, &sequencedIngestor{inner: ingestor, log: &log},
		gate, func() HistoryFetcher { return fetcher })
	drainer.pacing = 0

	require.NoError(t, drainer.Drain(context.Background()))

	require.NotEmpty(t, log)
	lastGate := -1
	firstIngest := len(log)
	for i, entry := range log {
		if entry == "gate" {
			lastGate = i
		}
		if entry == "ingest" && i < firstIngest {
			firstIngest = i
		}
	}
	require.GreaterOrEqual(t, lastGate, 0, "the pre-pass must actually run, or the order below is vacuous")
	require.Less(t, firstIngest, len(log), "and something must actually be ingested")
	assert.Less(t, lastGate, firstIngest,
		"every group in the chunk is decided before the first message from any of it is stored")
	assert.Equal(t, 3, ingestor.count())
}
