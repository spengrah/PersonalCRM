//go:build integration_testdb

package tests

import (
	"context"
	"os"
	"testing"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/testdb"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// waEnv is one test's isolated view of the WhatsApp-owned tables.
//
// ClaimNextNotification is DELIBERATELY database-global: it takes the oldest
// claimable chunk anywhere, because there is exactly one history drainer. That
// makes it impossible to namespace-scope, so every test that claims gets its own
// ephemeral clone rather than sharing the package DB. The clone also removes the
// cleanup burden that 076's down-migration guards would otherwise impose (an
// outstanding notification or a chat-config row refuses the revert).
type waEnv struct {
	ctx   context.Context
	wa    *repository.WhatsAppRepository
	comms *repository.CommsMessageRepository
	ns    string
}

func setupWhatsAppRepoTest(t *testing.T) *waEnv {
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

	return &waEnv{
		ctx:   ctx,
		wa:    repository.NewWhatsAppRepository(database.Queries),
		comms: repository.NewCommsMessageRepository(database.Queries),
		ns:    syntheticNS(t),
	}
}

// record stores one media-backed chunk and returns its id.
func (e *waEnv) record(t *testing.T, protocolMsgID string, chunkOrder int32) uuid.UUID {
	t.Helper()
	id, err := e.wa.RecordNotification(e.ctx, protocolMsgID, []byte("media-pointer-"+protocolMsgID),
		"FULL", chunkOrder, nil, repository.HistoryDispositionProject)
	require.NoError(t, err)
	return id
}

func (e *waEnv) claim(t *testing.T) *repository.HistoryNotification {
	t.Helper()
	n, err := e.wa.ClaimNextNotification(e.ctx)
	require.NoError(t, err)
	require.NotNil(t, n.ClaimToken, "a claim must stamp a fencing token")
	return n
}

// walkToDeleted runs the chunk through every phase of the history protocol, so
// a test that only cares about what happens AFTER completion does not have to
// spell the four edges out. MarkNotificationDone is fenced on the terminal
// phase, so this is the only legal route to 'done'.
func (e *waEnv) walkToDeleted(t *testing.T, id, token uuid.UUID) {
	t.Helper()
	for _, edge := range [][2]string{
		{repository.HistoryPhaseRecorded, repository.HistoryPhaseDownloaded},
		{repository.HistoryPhaseDownloaded, repository.HistoryPhaseProjected},
		{repository.HistoryPhaseProjected, repository.HistoryPhaseAcked},
		{repository.HistoryPhaseAcked, repository.HistoryPhaseDeleted},
	} {
		ok, err := e.wa.AdvancePhase(e.ctx, id, token, edge[0], edge[1])
		require.NoErrorf(t, err, "%s -> %s", edge[0], edge[1])
		require.Truef(t, ok, "%s -> %s must apply", edge[0], edge[1])
	}
}

// get re-reads one notification through the list surface.
func (e *waEnv) get(t *testing.T, id uuid.UUID) repository.HistoryNotification {
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

// TestWhatsAppRepo_RecordNotificationIsIdempotentOnRedelivery pins the property
// the recording handler depends on: it withholds the protocol ack on failure, so
// WhatsApp redelivering the same protocol message is the EXPECTED path, and the
// second record must return the original row rather than duplicate the chunk.
func TestWhatsAppRepo_RecordNotificationIsIdempotentOnRedelivery(t *testing.T) {
	t.Parallel()
	env := setupWhatsAppRepoTest(t)

	first := env.record(t, "proto-"+env.ns, 0)
	second := env.record(t, "proto-"+env.ns, 0)
	assert.Equal(t, first, second, "a redelivered notification returns the original row id")

	rows, err := env.wa.ListNotifications(env.ctx, []string{repository.HistoryNotificationStatePending})
	require.NoError(t, err)
	assert.Len(t, rows, 1, "redelivery must not create a second chunk")
}

// TestWhatsAppRepo_ClaimStampsFreshTokenAndOrdersByChunk covers the claim
// contract: chunks come out in chunk order, each claim stamps a NEW token and
// bumps attempts, and an already-claimed chunk is not handed out twice.
func TestWhatsAppRepo_ClaimStampsFreshTokenAndOrdersByChunk(t *testing.T) {
	t.Parallel()
	env := setupWhatsAppRepoTest(t)

	second := env.record(t, "proto-b-"+env.ns, 2)
	first := env.record(t, "proto-a-"+env.ns, 1)

	got := env.claim(t)
	assert.Equal(t, first, got.ID, "claims follow chunk_order, not insertion order")
	assert.Equal(t, repository.HistoryNotificationStateProcessing, got.State)
	assert.Equal(t, int32(1), got.Attempts)
	assert.Equal(t, repository.HistoryPhaseRecorded, got.Phase)

	next := env.claim(t)
	assert.Equal(t, second, next.ID, "a live claim is not handed out twice")
	assert.NotEqual(t, *got.ClaimToken, *next.ClaimToken, "each claim gets its own token")

	_, err := env.wa.ClaimNextNotification(env.ctx)
	require.ErrorIs(t, err, db.ErrNotFound, "nothing claimable left")
}

// TestWhatsAppRepo_ClaimSkipsDoneAndPicksExpiredProcessing is the crash-recovery
// path: a worker that died mid-chunk leaves the row `processing` with a stale
// lease, and the next claim must reclaim it — while a completed chunk is never
// re-handed-out.
func TestWhatsAppRepo_ClaimSkipsDoneAndPicksExpiredProcessing(t *testing.T) {
	t.Parallel()
	env := setupWhatsAppRepoTest(t)

	doneID := env.record(t, "proto-done-"+env.ns, 1)
	stuckID := env.record(t, "proto-stuck-"+env.ns, 2)

	done := env.claim(t)
	require.Equal(t, doneID, done.ID)
	env.walkToDeleted(t, done.ID, *done.ClaimToken)
	ok, err := env.wa.MarkNotificationDone(env.ctx, done.ID, *done.ClaimToken)
	require.NoError(t, err)
	require.True(t, ok)

	stuck := env.claim(t)
	require.Equal(t, stuckID, stuck.ID)

	// Nothing else is claimable while the lease is live.
	_, err = env.wa.ClaimNextNotification(env.ctx)
	require.ErrorIs(t, err, db.ErrNotFound)

	// Age the lease: the abandoned chunk becomes claimable again, the done one
	// does not.
	require.NoError(t, env.wa.BackdateClaimForTest(env.ctx, stuck.ID))
	reclaimed := env.claim(t)
	assert.Equal(t, stuckID, reclaimed.ID, "the expired lease is reclaimed, the done chunk is not")
	assert.Equal(t, int32(2), reclaimed.Attempts)
	assert.NotEqual(t, *stuck.ClaimToken, *reclaimed.ClaimToken, "the reclaim fences out the old worker")
}

// TestWhatsAppRepo_StaleTokenCannotClobberSuccessor is the fencing test. An
// over-running worker whose lease was reclaimed must not be able to write ANY
// state — checkpoint, phase, or terminal outcome — over its successor's.
func TestWhatsAppRepo_StaleTokenCannotClobberSuccessor(t *testing.T) {
	t.Parallel()
	env := setupWhatsAppRepoTest(t)
	id := env.record(t, "proto-fence-"+env.ns, 1)

	stale := env.claim(t)
	require.NoError(t, env.wa.BackdateClaimForTest(env.ctx, id))
	fresh := env.claim(t)
	require.NotEqual(t, *stale.ClaimToken, *fresh.ClaimToken)

	// The successor writes real state first, so a clobber would be visible.
	ok, err := env.wa.SaveCheckpoint(env.ctx, id, *fresh.ClaimToken, []byte(`{"conversation_index":7}`))
	require.NoError(t, err)
	require.True(t, ok)
	ok, err = env.wa.AdvancePhase(env.ctx, id, *fresh.ClaimToken,
		repository.HistoryPhaseRecorded, repository.HistoryPhaseDownloaded)
	require.NoError(t, err)
	require.True(t, ok)

	t.Run("SaveCheckpoint", func(t *testing.T) {
		ok, err := env.wa.SaveCheckpoint(env.ctx, id, *stale.ClaimToken, []byte(`{"conversation_index":0}`))
		require.NoError(t, err)
		assert.False(t, ok)
	})
	t.Run("AdvancePhase", func(t *testing.T) {
		ok, err := env.wa.AdvancePhase(env.ctx, id, *stale.ClaimToken,
			repository.HistoryPhaseDownloaded, repository.HistoryPhaseProjected)
		require.NoError(t, err)
		assert.False(t, ok)
	})
	t.Run("MarkFailed", func(t *testing.T) {
		ok, err := env.wa.MarkNotificationFailed(env.ctx, id, *stale.ClaimToken, "stale worker")
		require.NoError(t, err)
		assert.False(t, ok)
	})

	// The successor's state is intact: nothing the stale worker attempted landed.
	row := env.get(t, id)
	assert.Equal(t, repository.HistoryNotificationStateProcessing, row.State)
	assert.Equal(t, repository.HistoryPhaseDownloaded, row.Phase)
	assert.JSONEq(t, `{"conversation_index":7}`, string(row.Checkpoint))
	assert.Nil(t, row.LastError)

	// MarkDone last, and only once the successor has finished the protocol, so
	// the stale token is the ONLY reason it can fail — otherwise the terminal-
	// phase fence would mask the token fence and the assertion would prove
	// nothing about fencing at all.
	t.Run("MarkDone", func(t *testing.T) {
		ok, err := env.wa.AdvancePhase(env.ctx, id, *fresh.ClaimToken,
			repository.HistoryPhaseDownloaded, repository.HistoryPhaseProjected)
		require.NoError(t, err)
		require.True(t, ok)
		ok, err = env.wa.AdvancePhase(env.ctx, id, *fresh.ClaimToken,
			repository.HistoryPhaseProjected, repository.HistoryPhaseAcked)
		require.NoError(t, err)
		require.True(t, ok)
		ok, err = env.wa.AdvancePhase(env.ctx, id, *fresh.ClaimToken,
			repository.HistoryPhaseAcked, repository.HistoryPhaseDeleted)
		require.NoError(t, err)
		require.True(t, ok)

		ok, err = env.wa.MarkNotificationDone(env.ctx, id, *stale.ClaimToken)
		require.NoError(t, err)
		assert.False(t, ok, "the phase machine is complete, so only the token can be refusing this")
		assert.Equal(t, repository.HistoryNotificationStateProcessing, env.get(t, id).State)

		// The successor completes it.
		ok, err = env.wa.MarkNotificationDone(env.ctx, id, *fresh.ClaimToken)
		require.NoError(t, err)
		assert.True(t, ok)
	})
}

// TestWhatsAppRepo_MarkDoneRequiresCompletedPhaseMachine is the other half of
// the completion fence. 'done' is unreachable by any later claim, so a chunk
// marked done before it reached 'deleted' has silently abandoned its download,
// projection, receipt and server-side media with nothing left to notice it.
// Completion is therefore gated on the END of the phase machine, not merely on
// holding the lease.
func TestWhatsAppRepo_MarkDoneRequiresCompletedPhaseMachine(t *testing.T) {
	t.Parallel()
	env := setupWhatsAppRepoTest(t)
	id := env.record(t, "proto-incomplete-"+env.ns, 1)
	claimed := env.claim(t)
	token := *claimed.ClaimToken

	for _, phase := range []string{
		repository.HistoryPhaseRecorded,
		repository.HistoryPhaseDownloaded,
		repository.HistoryPhaseProjected,
		repository.HistoryPhaseAcked,
	} {
		require.Equal(t, phase, env.get(t, id).Phase)
		ok, err := env.wa.MarkNotificationDone(env.ctx, id, token)
		require.NoError(t, err)
		assert.Falsef(t, ok, "a chunk at phase %q has not finished the protocol", phase)
		require.Equal(t, repository.HistoryNotificationStateProcessing, env.get(t, id).State,
			"and it stays claimable rather than becoming an unreachable 'done'")

		if phase == repository.HistoryPhaseAcked {
			break
		}
		next := map[string]string{
			repository.HistoryPhaseRecorded:   repository.HistoryPhaseDownloaded,
			repository.HistoryPhaseDownloaded: repository.HistoryPhaseProjected,
			repository.HistoryPhaseProjected:  repository.HistoryPhaseAcked,
		}[phase]
		ok, err = env.wa.AdvancePhase(env.ctx, id, token, phase, next)
		require.NoError(t, err)
		require.True(t, ok)
	}

	ok, err := env.wa.AdvancePhase(env.ctx, id, token,
		repository.HistoryPhaseAcked, repository.HistoryPhaseDeleted)
	require.NoError(t, err)
	require.True(t, ok)

	ok, err = env.wa.MarkNotificationDone(env.ctx, id, token)
	require.NoError(t, err)
	assert.True(t, ok, "the completed protocol closes the chunk")
	assert.Equal(t, repository.HistoryNotificationStateDone, env.get(t, id).State)
}

// TestWhatsAppRepo_AdvancePhaseRejectsIllegalJump pins BOTH halves of the phase
// machine: an edge that is not one of the protocol's own steps is refused
// outright (the DB CHECK constrains phase VALUES, not TRANSITIONS), and a legal
// edge whose predecessor no longer holds is a silent no-op. Together they are
// what stops a caller skipping the download or the receipt.
func TestWhatsAppRepo_AdvancePhaseRejectsIllegalJump(t *testing.T) {
	t.Parallel()
	env := setupWhatsAppRepoTest(t)
	id := env.record(t, "proto-phase-"+env.ns, 1)
	claimed := env.claim(t)
	token := *claimed.ClaimToken

	// Jumps that skip a step are refused, whichever step they skip.
	for _, jump := range []struct{ from, to string }{
		{repository.HistoryPhaseRecorded, repository.HistoryPhaseProjected},  // skips the download
		{repository.HistoryPhaseProjected, repository.HistoryPhaseDeleted},   // skips the receipt
		{repository.HistoryPhaseDownloaded, repository.HistoryPhaseRecorded}, // backwards
		{repository.HistoryPhaseDeleted, repository.HistoryPhaseDeleted},     // terminal has no successor
	} {
		ok, err := env.wa.AdvancePhase(env.ctx, id, token, jump.from, jump.to)
		require.ErrorIsf(t, err, repository.ErrIllegalPhaseEdge, "%s -> %s", jump.from, jump.to)
		assert.False(t, ok)
	}
	assert.Equal(t, repository.HistoryPhaseRecorded, env.get(t, id).Phase, "no illegal jump moved the row")

	// A legal edge whose predecessor does not hold changes nothing — quietly,
	// because that is the ordinary lost-race shape, not a programming error.
	ok, err := env.wa.AdvancePhase(env.ctx, id, token,
		repository.HistoryPhaseDownloaded, repository.HistoryPhaseProjected)
	require.NoError(t, err)
	assert.False(t, ok, "an edge whose predecessor no longer holds must be a no-op")
	assert.Equal(t, repository.HistoryPhaseRecorded, env.get(t, id).Phase)

	// The one edge that is legal from the row's actual phase applies.
	ok, err = env.wa.AdvancePhase(env.ctx, id, token,
		repository.HistoryPhaseRecorded, repository.HistoryPhaseDownloaded)
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, repository.HistoryPhaseDownloaded, env.get(t, id).Phase)
}

// TestWhatsAppRepo_ClaimResumesAtStoredPhase is what makes the five-step
// protocol restartable: the reclaimed row still carries the phase and checkpoint
// the crashed worker reached, so the successor skips the work already done.
func TestWhatsAppRepo_ClaimResumesAtStoredPhase(t *testing.T) {
	t.Parallel()
	env := setupWhatsAppRepoTest(t)
	id := env.record(t, "proto-resume-"+env.ns, 1)

	first := env.claim(t)
	ok, err := env.wa.AdvancePhase(env.ctx, id, *first.ClaimToken,
		repository.HistoryPhaseRecorded, repository.HistoryPhaseDownloaded)
	require.NoError(t, err)
	require.True(t, ok)
	ok, err = env.wa.SaveCheckpoint(env.ctx, id, *first.ClaimToken, []byte(`{"conversation_index":3}`))
	require.NoError(t, err)
	require.True(t, ok)

	// Worker dies; lease expires; a successor picks it up.
	require.NoError(t, env.wa.BackdateClaimForTest(env.ctx, id))
	resumed := env.claim(t)
	assert.Equal(t, repository.HistoryPhaseDownloaded, resumed.Phase, "resume point survives the crash")
	assert.JSONEq(t, `{"conversation_index":3}`, string(resumed.Checkpoint))
	assert.Equal(t, int32(2), resumed.Attempts)
}

// TestWhatsAppRepo_RequeueFailedReturnsRowToPending is the operator recovery
// path behind `crm-admin whatsapp requeue-history`: "re-drainable by hand" has
// to be a command, not a hope.
func TestWhatsAppRepo_RequeueFailedReturnsRowToPending(t *testing.T) {
	t.Parallel()
	env := setupWhatsAppRepoTest(t)
	id := env.record(t, "proto-requeue-"+env.ns, 1)

	claimed := env.claim(t)
	ok, err := env.wa.MarkNotificationFailed(env.ctx, id, *claimed.ClaimToken, "payload will not unmarshal")
	require.NoError(t, err)
	require.True(t, ok)

	failed := env.get(t, id)
	require.Equal(t, repository.HistoryNotificationStateFailed, failed.State)
	require.NotNil(t, failed.LastError)
	assert.Equal(t, "payload will not unmarshal", *failed.LastError)

	// A failed row is NOT claimable until an operator requeues it.
	_, err = env.wa.ClaimNextNotification(env.ctx)
	require.ErrorIs(t, err, db.ErrNotFound)

	require.NoError(t, env.wa.RequeueFailedNotification(env.ctx, id))
	requeued := env.get(t, id)
	assert.Equal(t, repository.HistoryNotificationStatePending, requeued.State)
	assert.Nil(t, requeued.ClaimToken)
	assert.Nil(t, requeued.LastError)

	again := env.claim(t)
	assert.Equal(t, id, again.ID)

	// Requeue only accepts a failed row.
	require.ErrorIs(t, env.wa.RequeueFailedNotification(env.ctx, id), db.ErrNotFound,
		"a processing row is not requeueable")
	require.ErrorIs(t, env.wa.RequeueFailedNotification(env.ctx, uuid.New()), db.ErrNotFound)
}

// TestWhatsAppRepo_DroppedInlineDisposition covers the chunk the server inlined
// against our explicit request. Its payload was discarded un-projected, so it
// enters the phase machine LATE — but it still runs it, because the protocol
// receipt is an acknowledgement that the protocol message was handled and the
// library would have sent one unconditionally.
func TestWhatsAppRepo_DroppedInlineDisposition(t *testing.T) {
	t.Parallel()
	env := setupWhatsAppRepoTest(t)

	droppedID, err := env.wa.RecordNotification(env.ctx, "proto-dropped-"+env.ns,
		[]byte("pointer-only"), "INITIAL_BOOTSTRAP", 0, nil, repository.HistoryDispositionDroppedInline)
	require.NoError(t, err)
	projectID := env.record(t, "proto-project-"+env.ns, 1)

	t.Run("CreatedAtProjectedPhase", func(t *testing.T) {
		row := env.get(t, droppedID)
		assert.Equal(t, repository.HistoryDispositionDroppedInline, row.Disposition)
		assert.Equal(t, repository.HistoryPhaseProjected, row.Phase,
			"nothing to download and nothing to project, so the drainer starts at the receipt")
		assert.Equal(t, repository.HistoryNotificationStatePending, row.State,
			"dropped is a disposition, not a terminal state — the receipt is still owed")

		media := env.get(t, projectID)
		assert.Equal(t, repository.HistoryDispositionProject, media.Disposition)
		assert.Equal(t, repository.HistoryPhaseRecorded, media.Phase)
	})

	t.Run("RequeueOnDroppedRowRetriesOnlyTheReceipt", func(t *testing.T) {
		claimed := env.claim(t)
		require.Equal(t, droppedID, claimed.ID, "a dropped row is claimable like any other")

		// Only the receipt can fail for a dropped row; requeueing it must not
		// rewind the phase, or the retry would try to project a chunk that has
		// no media.
		ok, err := env.wa.MarkNotificationFailed(env.ctx, droppedID, *claimed.ClaimToken, "receipt send failed")
		require.NoError(t, err)
		require.True(t, ok)
		require.NoError(t, env.wa.RequeueFailedNotification(env.ctx, droppedID))

		again := env.claim(t)
		require.Equal(t, droppedID, again.ID)
		assert.Equal(t, repository.HistoryPhaseProjected, again.Phase,
			"the retry resumes at the receipt, never at a projection")
		assert.Equal(t, repository.HistoryDispositionDroppedInline, again.Disposition)
	})

	t.Run("CountByStateAndDispositionReportsDroppedInline", func(t *testing.T) {
		counts, err := env.wa.CountByStateAndDisposition(env.ctx)
		require.NoError(t, err)
		assert.Equal(t, 1, counts["processing/dropped_inline"],
			"the accepted history gap is observable, not silent")
		assert.Equal(t, 1, counts["pending/project"])
	})
}

// TestWhatsAppRepo_ObservedFloorIsOldestStagedSentAt backs the status
// endpoint's empirical answer to how deep the one-shot history actually reached.
func TestWhatsAppRepo_ObservedFloorIsOldestStagedSentAt(t *testing.T) {
	t.Parallel()
	env := setupWhatsAppRepoTest(t)

	floor, err := env.wa.ObservedFloor(env.ctx)
	require.NoError(t, err)
	assert.Nil(t, floor, "nothing staged yet")

	oldest := accelerated.GetCurrentTime().Add(-72 * time.Hour).Truncate(time.Microsecond)
	peer := env.ns + "@s.whatsapp.net"
	body := "staged"
	for i, sentAt := range []time.Time{oldest.Add(time.Hour), oldest, oldest.Add(2 * time.Hour)} {
		_, err := env.comms.UpsertChatMessage(env.ctx, repository.UpsertChatMessageParams{
			Source:     repository.InteractionSourceWhatsApp,
			ExternalID: "floor-" + env.ns + "-" + string(rune('a'+i)),
			ThreadID:   peer,
			Body:       &body,
			PeerHandle: &peer,
			Direction:  repository.InteractionDirectionInbound,
			SentAt:     sentAt,
		})
		require.NoError(t, err)
	}

	floor, err = env.wa.ObservedFloor(env.ctx)
	require.NoError(t, err)
	require.NotNil(t, floor)
	assert.WithinDuration(t, oldest, *floor, time.Second)
}

// TestWhatsAppRepo_ChatConfig covers the persistent group gate's substrate. The
// gate LOGIC is a later PR's; what has to hold here is that a re-observation
// never overwrites the user's decision and that "unresolved" round-trips as
// NULL rather than as a zero that would read as a tiny group.
func TestWhatsAppRepo_ChatConfig(t *testing.T) {
	t.Parallel()
	env := setupWhatsAppRepoTest(t)

	t.Run("UpsertChatConfigPreservesStatusOverride", func(t *testing.T) {
		jid := env.ns + "-override@g.us"
		title := "Team Chat"
		count := int32(6)
		created, err := env.wa.UpsertChatConfig(env.ctx, repository.WhatsAppChatConfig{
			ChatJID: jid, ChatTitle: &title, ChatType: "group",
			MemberCount: &count, Status: "ignored",
		})
		require.NoError(t, err)
		require.Equal(t, "ignored", created.Status)

		// A later observation resolves a new count and passes the DEFAULT
		// status; the user's override must survive it.
		newCount := int32(9)
		updated, err := env.wa.UpsertChatConfig(env.ctx, repository.WhatsAppChatConfig{
			ChatJID: jid, ChatType: "group", MemberCount: &newCount, Status: "auto",
		})
		require.NoError(t, err)
		assert.Equal(t, "ignored", updated.Status, "a re-observation never overwrites the user's decision")
		require.NotNil(t, updated.MemberCount)
		assert.Equal(t, int32(9), *updated.MemberCount, "the freshly resolved count does land")
		require.NotNil(t, updated.ChatTitle)
		assert.Equal(t, "Team Chat", *updated.ChatTitle, "a nil title preserves the stored one")

		got, err := env.wa.GetChatConfig(env.ctx, jid)
		require.NoError(t, err)
		assert.Equal(t, "ignored", got.Status)
	})

	t.Run("NullMemberCountRoundTrips", func(t *testing.T) {
		jid := env.ns + "-unresolved@g.us"
		created, err := env.wa.UpsertChatConfig(env.ctx, repository.WhatsAppChatConfig{
			ChatJID: jid, ChatType: "group",
		})
		require.NoError(t, err)
		assert.Nil(t, created.MemberCount, "unresolved must stay NULL, never collapse to 0")
		assert.Equal(t, "auto", created.Status, "an unset status defaults to auto")

		got, err := env.wa.GetChatConfig(env.ctx, jid)
		require.NoError(t, err)
		assert.Nil(t, got.MemberCount)

		// A later lookup that resolves the count fills it in.
		count := int32(4)
		now := accelerated.GetCurrentTime()
		updated, err := env.wa.UpsertChatConfig(env.ctx, repository.WhatsAppChatConfig{
			ChatJID: jid, ChatType: "group", MemberCount: &count, LastLookupAt: &now,
		})
		require.NoError(t, err)
		require.NotNil(t, updated.MemberCount)
		assert.Equal(t, int32(4), *updated.MemberCount)
		assert.NotNil(t, updated.LastLookupAt)
	})

	t.Run("GetMissingChatIsNotFound", func(t *testing.T) {
		_, err := env.wa.GetChatConfig(env.ctx, env.ns+"-missing@g.us")
		assert.ErrorIs(t, err, db.ErrNotFound)
	})
}
