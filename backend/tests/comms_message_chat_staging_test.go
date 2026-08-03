package tests

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// chatStagingEnv is one test's view of the chat-staging surface: a
// namespace-scoped external-id prefix and peer handles, so assertions over the
// shared package DB only ever see this test's own rows.
type chatStagingEnv struct {
	ctx         context.Context
	comms       *repository.CommsMessageRepository
	contactRepo *repository.ContactRepository
	ns          string
}

// setupChatStagingTest opens the shared package DB, wires the pool (the attach
// pair runs in its own transaction) and registers the cleanup that hard-deletes
// this test's whatsapp rows. The cleanup is NOT optional: an unmatched row has
// no contact for the usual HardDeleteByContact sweep to reach, and 076's down
// migration refuses to revert while any NULL-contact row survives.
func setupChatStagingTest(t *testing.T) *chatStagingEnv {
	t.Helper()
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set")
	}
	ctx := context.Background()
	cfg := config.TestConfig()
	cfg.Database.URL = databaseURL
	database, err := db.NewDatabase(ctx, cfg.Database)
	require.NoError(t, err)
	// Registered first so it runs LAST (t.Cleanup is LIFO) — the row sweeps
	// below need the pool open.
	t.Cleanup(database.Close)

	env := &chatStagingEnv{
		ctx:         ctx,
		comms:       repository.NewCommsMessageRepository(database.Queries),
		contactRepo: repository.NewContactRepository(database.Queries),
		ns:          syntheticNS(t),
	}
	env.comms.SetPool(database.Pool)
	t.Cleanup(func() {
		_ = env.comms.HardDeleteBySourceAndExternalIDPrefix(ctx,
			repository.InteractionSourceWhatsApp, env.extIDPrefix()+"%")
	})
	return env
}

func (e *chatStagingEnv) extIDPrefix() string { return "wa-" + e.ns + "-" }

func (e *chatStagingEnv) extID(n int) string { return fmt.Sprintf("%s%d", e.extIDPrefix(), n) }

func (e *chatStagingEnv) peer(name string) string {
	return e.ns + "-" + name + "@s.whatsapp.net"
}

func (e *chatStagingEnv) newContact(t *testing.T, name string) *repository.Contact {
	t.Helper()
	contact, err := e.contactRepo.CreateContact(e.ctx, repository.CreateContactRequest{
		FullName: name + " " + e.ns,
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = e.comms.HardDeleteByContact(e.ctx, contact.ID)
		_ = e.contactRepo.SoftDeleteContact(e.ctx, contact.ID)
	})
	return contact
}

// stageOpt mutates the params of a staged row.
type stageOpt func(*repository.UpsertChatMessageParams)

func withContact(id uuid.UUID) stageOpt {
	return func(p *repository.UpsertChatMessageParams) { p.MatchedContactID = &id }
}
func withBody(body string) stageOpt {
	return func(p *repository.UpsertChatMessageParams) { p.Body = &body }
}
func withNormalized(v string) stageOpt {
	return func(p *repository.UpsertChatMessageParams) { p.PeerNormalized = &v }
}
func withDirection(d string) stageOpt {
	return func(p *repository.UpsertChatMessageParams) { p.Direction = d }
}
func withSentAt(t time.Time) stageOpt {
	return func(p *repository.UpsertChatMessageParams) { p.SentAt = t }
}
func withPushName(name string) stageOpt {
	return func(p *repository.UpsertChatMessageParams) {
		raw, err := json.Marshal(map[string]string{"push_name": name})
		if err != nil {
			panic(err)
		}
		p.SourceMetadata = raw
	}
}
func withSource(source string) stageOpt {
	return func(p *repository.UpsertChatMessageParams) { p.Source = source }
}

// stage upserts one chat message with sensible whatsapp defaults.
func (e *chatStagingEnv) stage(t *testing.T, externalID, peerHandle string, opts ...stageOpt) (*repository.CommsMessage, error) {
	t.Helper()
	thread := peerHandle
	body := "chat body"
	params := repository.UpsertChatMessageParams{
		Source:     repository.InteractionSourceWhatsApp,
		ExternalID: externalID,
		ThreadID:   &thread,
		Body:       &body,
		PeerHandle: &peerHandle,
		Direction:  repository.InteractionDirectionInbound,
		SentAt:     accelerated.GetCurrentTime().Add(-time.Hour).Truncate(time.Microsecond),
	}
	for _, o := range opts {
		o(&params)
	}
	return e.comms.UpsertChatMessage(e.ctx, params)
}

func (e *chatStagingEnv) mustStage(t *testing.T, externalID, peerHandle string, opts ...stageOpt) *repository.CommsMessage {
	t.Helper()
	msg, err := e.stage(t, externalID, peerHandle, opts...)
	require.NoError(t, err)
	return msg
}

// TestUpsertChatMessage_MatchedIsIdempotent pins the content-immutable
// semantics the live path and the history backfill both rely on: re-staging the
// same (source, external_id, contact) returns the stored row unchanged rather
// than overwriting it or reporting a spurious not-found.
func TestUpsertChatMessage_MatchedIsIdempotent(t *testing.T) {
	t.Parallel()
	env := setupChatStagingTest(t)
	contact := env.newContact(t, "Matched Idempotent")
	peer := env.peer("matched")

	first := env.mustStage(t, env.extID(1), peer, withContact(contact.ID), withBody("first"))
	second := env.mustStage(t, env.extID(1), peer, withContact(contact.ID), withBody("second"))

	assert.Equal(t, first.ID, second.ID, "re-stage must return the stored row, not a new one")
	require.NotNil(t, second.Body)
	assert.Equal(t, "first", *second.Body, "content is immutable on conflict (first writer wins)")

	rows, err := env.comms.ListByContact(env.ctx, contact.ID)
	require.NoError(t, err)
	require.Len(t, rows, 1, "exactly one live row for the message")
}

// TestUpsertChatMessage_UnmatchedIsIdempotent is the test that proves
// idx_comms_message_dedup_unmatched exists and that the ON CONFLICT inference
// actually resolves to it — the matched dedup index can never match a NULL
// contact, so without the new partial index this would insert twice.
func TestUpsertChatMessage_UnmatchedIsIdempotent(t *testing.T) {
	t.Parallel()
	env := setupChatStagingTest(t)
	peer := env.peer("unmatched")

	first := env.mustStage(t, env.extID(1), peer, withBody("first"))
	second := env.mustStage(t, env.extID(1), peer, withBody("second"))

	require.Nil(t, first.MatchedContactID, "an unmatched stage writes a NULL contact")
	assert.Equal(t, first.ID, second.ID, "re-stage must return the stored row, not a new one")
	require.NotNil(t, second.Body)
	assert.Equal(t, "first", *second.Body)

	counts, err := env.comms.ListUnmatchedPeerCounts(env.ctx, repository.InteractionSourceWhatsApp, &peer, 1)
	require.NoError(t, err)
	require.Len(t, counts, 1)
	assert.Equal(t, int64(1), counts[0].TotalCount, "one message, one row")
}

// TestUpsertChatMessage_MatchedAndUnmatchedCoexist pins the boundary between
// the two dedup indexes: the same external_id may legitimately be staged once
// unmatched and once against a contact (the LID-then-phone sequence), and
// neither write collides with the other.
func TestUpsertChatMessage_MatchedAndUnmatchedCoexist(t *testing.T) {
	t.Parallel()
	env := setupChatStagingTest(t)
	contact := env.newContact(t, "Coexist")
	peer := env.peer("coexist")

	unmatched := env.mustStage(t, env.extID(1), peer)
	matched := env.mustStage(t, env.extID(1), peer, withContact(contact.ID))

	assert.NotEqual(t, unmatched.ID, matched.ID, "the two rows are distinct")
	require.Nil(t, unmatched.MatchedContactID)
	require.NotNil(t, matched.MatchedContactID)
	assert.Equal(t, contact.ID, *matched.MatchedContactID)
}

// TestCommsMessage_NullContactRejectedForNonWhatsAppSource is the enforcement
// test for the invariant: the nullable column is bounded by a DB CHECK, not by
// an audit of today's writers. A future source that tries to stage a
// contactless row is rejected by the database.
func TestCommsMessage_NullContactRejectedForNonWhatsAppSource(t *testing.T) {
	t.Parallel()
	env := setupChatStagingTest(t)

	for _, source := range []string{repository.InteractionSourceGChat, repository.InteractionSourceEmail} {
		t.Run(source, func(t *testing.T) {
			_, err := env.stage(t, env.extID(1), env.peer("rejected"), withSource(source))
			require.Error(t, err, "a NULL contact must be rejected for source %q", source)
			assert.Contains(t, err.Error(), "comms_message_contact_source_check",
				"the rejection must come from the source-scoped CHECK, not some other constraint")
		})
	}
}

// TestAttachUnmatchedByPeer_ByNormalizedPhone covers the ordinary link: every
// unmatched row whose resolved phone matches the peer flips to the contact, and
// nothing else moves.
func TestAttachUnmatchedByPeer_ByNormalizedPhone(t *testing.T) {
	t.Parallel()
	env := setupChatStagingTest(t)
	contact := env.newContact(t, "Attach By Phone")
	peer := env.peer("phone")
	other := env.peer("other")
	phone := "+1555" + env.ns[:6]

	env.mustStage(t, env.extID(1), peer, withNormalized(phone))
	env.mustStage(t, env.extID(2), peer, withNormalized(phone))
	env.mustStage(t, env.extID(3), other)

	attached, deduped, err := env.comms.AttachUnmatchedByPeer(env.ctx,
		repository.InteractionSourceWhatsApp, &phone, nil, contact.ID)
	require.NoError(t, err)
	assert.Equal(t, int64(2), attached)
	assert.Equal(t, int64(0), deduped)

	rows, err := env.comms.ListByContact(env.ctx, contact.ID)
	require.NoError(t, err)
	assert.Len(t, rows, 2, "exactly the peer's two rows attached")

	otherCounts, err := env.comms.ListUnmatchedPeerCounts(env.ctx, repository.InteractionSourceWhatsApp, &other, 1)
	require.NoError(t, err)
	require.Len(t, otherCounts, 1)
	assert.Equal(t, int64(1), otherCounts[0].TotalCount, "another peer's row is untouched")
}

// TestAttachUnmatchedByPeer_ByPeerHandle covers the LID-only peer: no phone was
// ever resolvable, so the attach must match on the raw peer handle or the
// hand-imported contact would never pick up its staged history.
func TestAttachUnmatchedByPeer_ByPeerHandle(t *testing.T) {
	t.Parallel()
	env := setupChatStagingTest(t)
	contact := env.newContact(t, "Attach By Handle")
	peer := env.ns + "-lid@lid"

	env.mustStage(t, env.extID(1), peer)
	env.mustStage(t, env.extID(2), peer)

	attached, deduped, err := env.comms.AttachUnmatchedByPeer(env.ctx,
		repository.InteractionSourceWhatsApp, nil, &peer, contact.ID)
	require.NoError(t, err)
	assert.Equal(t, int64(2), attached)
	assert.Equal(t, int64(0), deduped)

	rows, err := env.comms.ListByContact(env.ctx, contact.ID)
	require.NoError(t, err)
	assert.Len(t, rows, 2)
}

// TestAttachUnmatchedByPeer_NoSelectorIsANoop guards the degenerate call: with
// neither selector the predicate would match nothing anyway, but the method
// must not issue a query that could be misread as "attach this source's whole
// unmatched backlog".
func TestAttachUnmatchedByPeer_NoSelectorIsANoop(t *testing.T) {
	t.Parallel()
	env := setupChatStagingTest(t)
	contact := env.newContact(t, "Attach No Selector")
	peer := env.peer("noselector")
	env.mustStage(t, env.extID(1), peer)

	attached, deduped, err := env.comms.AttachUnmatchedByPeer(env.ctx,
		repository.InteractionSourceWhatsApp, nil, nil, contact.ID)
	require.NoError(t, err)
	assert.Equal(t, int64(0), attached)
	assert.Equal(t, int64(0), deduped)

	counts, err := env.comms.ListUnmatchedPeerCounts(env.ctx, repository.InteractionSourceWhatsApp, &peer, 1)
	require.NoError(t, err)
	require.Len(t, counts, 1, "the unmatched row is still unmatched")
}

// TestAttachUnmatchedByPeer_ReconcilesDuplicateUnmatchedRow is the realistic
// collision: a LID-only peer was staged unmatched, the phone later resolved, and
// the same message re-staged matched. Attaching the unmatched copy would violate
// idx_comms_message_dedup and abort the caller's transaction; SKIPPING it would
// strand a permanent unmatched duplicate. The reconciliation soft-deletes it and
// attaches the rest.
func TestAttachUnmatchedByPeer_ReconcilesDuplicateUnmatchedRow(t *testing.T) {
	t.Parallel()
	env := setupChatStagingTest(t)
	contact := env.newContact(t, "Attach Reconcile")
	peer := env.peer("reconcile")

	// Already staged against the contact.
	env.mustStage(t, env.extID(1), peer, withContact(contact.ID), withBody("canonical"))
	// The unmatched duplicate of that same message, plus one the contact does
	// not have yet.
	env.mustStage(t, env.extID(1), peer, withBody("canonical"))
	env.mustStage(t, env.extID(2), peer)

	attached, deduped, err := env.comms.AttachUnmatchedByPeer(env.ctx,
		repository.InteractionSourceWhatsApp, nil, &peer, contact.ID)
	require.NoError(t, err, "the collision must be reconciled, not raised as a 23505")
	assert.Equal(t, int64(1), attached, "only the non-colliding row attaches")
	assert.Equal(t, int64(1), deduped, "the colliding unmatched row is reconciled away")

	// The surviving copy of the colliding message is the matched one, with the
	// content the first writer stored.
	survivor, err := env.comms.GetMessage(env.ctx, repository.InteractionSourceWhatsApp, env.extID(1), contact.ID)
	require.NoError(t, err)
	require.NotNil(t, survivor.Body)
	assert.Equal(t, "canonical", *survivor.Body)

	// Nothing is left unmatched for the peer — the defect this replaces was a
	// permanently stranded duplicate.
	counts, err := env.comms.ListUnmatchedPeerCounts(env.ctx, repository.InteractionSourceWhatsApp, &peer, 1)
	require.NoError(t, err)
	assert.Empty(t, counts, "no unmatched row may survive the attach")

	rows, err := env.comms.ListByContact(env.ctx, contact.ID)
	require.NoError(t, err)
	assert.Len(t, rows, 2, "the contact holds one row per distinct message")
}

// TestAttachUnmatchedByPeer_IsAtomic proves the reconcile and the attach commit
// together. The failpoint fires between the two statements; neither may be
// visible afterwards, because a soft-delete that survived a failed attach would
// silently destroy staged history.
func TestAttachUnmatchedByPeer_IsAtomic(t *testing.T) {
	t.Parallel()
	env := setupChatStagingTest(t)
	contact := env.newContact(t, "Attach Atomic")
	peer := env.peer("atomic")

	env.mustStage(t, env.extID(1), peer, withContact(contact.ID))
	env.mustStage(t, env.extID(1), peer)
	env.mustStage(t, env.extID(2), peer)

	boom := errors.New("injected failure between the two statements")
	env.comms.SetAttachFailpointForTest(func() error { return boom })
	t.Cleanup(func() { env.comms.SetAttachFailpointForTest(nil) })

	attached, deduped, err := env.comms.AttachUnmatchedByPeer(env.ctx,
		repository.InteractionSourceWhatsApp, nil, &peer, contact.ID)
	require.ErrorIs(t, err, boom)
	assert.Zero(t, attached)
	assert.Zero(t, deduped)

	// Both unmatched rows survive, un-attached and un-deleted.
	counts, err := env.comms.ListUnmatchedPeerCounts(env.ctx, repository.InteractionSourceWhatsApp, &peer, 1)
	require.NoError(t, err)
	require.Len(t, counts, 1)
	assert.Equal(t, int64(2), counts[0].TotalCount,
		"the soft-delete must roll back with the failed attach")
}

// TestListUnmatchedPeerCounts_ReportsNewestPushName pins the display-name rule:
// the newest KNOWN push name wins. A newer row that carries no name must not
// blank out a name an older row already reported.
func TestListUnmatchedPeerCounts_ReportsNewestPushName(t *testing.T) {
	t.Parallel()
	env := setupChatStagingTest(t)
	peer := env.peer("pushname")
	base := accelerated.GetCurrentTime().Add(-4 * time.Hour).Truncate(time.Microsecond)

	env.mustStage(t, env.extID(1), peer, withSentAt(base), withPushName("Old Name"))
	env.mustStage(t, env.extID(2), peer, withSentAt(base.Add(time.Hour)), withPushName("New Name"))
	env.mustStage(t, env.extID(3), peer, withSentAt(base.Add(2*time.Hour)))

	counts, err := env.comms.ListUnmatchedPeerCounts(env.ctx, repository.InteractionSourceWhatsApp, &peer, 1)
	require.NoError(t, err)
	require.Len(t, counts, 1)
	require.NotNil(t, counts[0].LastPushName)
	assert.Equal(t, "New Name", *counts[0].LastPushName)
}

// TestListUnmatchedPeerCounts_ThresholdAndFilter covers the discovery aggregate:
// the threshold, the direction split, the newest timestamp, the single-peer
// filter, and — the load-bearing one — that a row which already has a contact is
// never counted as a discovery candidate.
func TestListUnmatchedPeerCounts_ThresholdAndFilter(t *testing.T) {
	t.Parallel()
	env := setupChatStagingTest(t)
	contact := env.newContact(t, "Discovery Counted")
	peerA := env.peer("a")
	peerB := env.peer("b")
	base := accelerated.GetCurrentTime().Add(-5 * time.Hour).Truncate(time.Microsecond)
	newest := base.Add(2 * time.Hour)

	env.mustStage(t, env.extID(1), peerA, withSentAt(base), withDirection(repository.InteractionDirectionInbound))
	env.mustStage(t, env.extID(2), peerA, withSentAt(base.Add(time.Hour)), withDirection(repository.InteractionDirectionInbound))
	env.mustStage(t, env.extID(3), peerA, withSentAt(newest), withDirection(repository.InteractionDirectionOutbound))
	env.mustStage(t, env.extID(4), peerB, withSentAt(base))
	// A matched row for peer A: content exists, but the peer is already a
	// contact, so it must not inflate the discovery count.
	env.mustStage(t, env.extID(5), peerA, withSentAt(base), withContact(contact.ID))

	mine := func(t *testing.T, peer *string, min int) map[string]repository.UnmatchedPeerCount {
		t.Helper()
		rows, err := env.comms.ListUnmatchedPeerCounts(env.ctx, repository.InteractionSourceWhatsApp, peer, min)
		require.NoError(t, err)
		out := map[string]repository.UnmatchedPeerCount{}
		for _, r := range rows {
			// The shared package DB carries other tests' whatsapp rows; scope
			// to this namespace's peers.
			if len(r.PeerHandle) > len(env.ns) && r.PeerHandle[:len(env.ns)] == env.ns {
				out[r.PeerHandle] = r
			}
		}
		return out
	}

	all := mine(t, nil, 1)
	require.Contains(t, all, peerA)
	require.Contains(t, all, peerB)
	a := all[peerA]
	assert.Equal(t, int64(3), a.TotalCount, "the matched row is not a discovery candidate")
	assert.Equal(t, int64(2), a.InboundCount)
	assert.Equal(t, int64(1), a.OutboundCount)
	assert.WithinDuration(t, newest, a.LastMessageAt, time.Second)

	aboveThreshold := mine(t, nil, 2)
	assert.Contains(t, aboveThreshold, peerA)
	assert.NotContains(t, aboveThreshold, peerB, "a below-threshold peer is excluded")

	only := mine(t, &peerA, 1)
	assert.Len(t, only, 1)
	assert.Contains(t, only, peerA)
}

// TestCommsMessage_UnmatchedRowsAreNotEligible is the guard for the invariant
// that makes the nullable column safe: an unmatched row is invisible to the
// claim/aggregation path, so the engine can never be asked to create an
// interaction with no contact. The before/after around the attach is what makes
// the assertion discriminating rather than vacuous.
func TestCommsMessage_UnmatchedRowsAreNotEligible(t *testing.T) {
	t.Parallel()
	env := setupChatStagingTest(t)
	contact := env.newContact(t, "Not Eligible")
	peer := env.peer("eligible")

	env.mustStage(t, env.extID(1), peer)
	env.mustStage(t, env.extID(2), peer)

	contactIDs, err := env.comms.ListUnprocessedContactIDsForSource(env.ctx, repository.InteractionSourceWhatsApp)
	require.NoError(t, err)
	assert.NotContains(t, contactIDs, contact.ID, "an unmatched row must not surface a contact")

	byContact, err := env.comms.ListUnprocessedByContactForSource(env.ctx, repository.InteractionSourceWhatsApp, contact.ID)
	require.NoError(t, err)
	assert.Empty(t, byContact, "an unmatched row must not be eligible for any contact")

	// After the attach the very same rows DO become eligible — proof the
	// emptiness above is the NULL contact, not a broken query.
	attached, _, err := env.comms.AttachUnmatchedByPeer(env.ctx,
		repository.InteractionSourceWhatsApp, nil, &peer, contact.ID)
	require.NoError(t, err)
	require.Equal(t, int64(2), attached)

	contactIDs, err = env.comms.ListUnprocessedContactIDsForSource(env.ctx, repository.InteractionSourceWhatsApp)
	require.NoError(t, err)
	assert.Contains(t, contactIDs, contact.ID)

	byContact, err = env.comms.ListUnprocessedByContactForSource(env.ctx, repository.InteractionSourceWhatsApp, contact.ID)
	require.NoError(t, err)
	assert.Len(t, byContact, 2)
}
