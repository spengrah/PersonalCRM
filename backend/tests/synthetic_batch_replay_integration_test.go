package tests

import (
	"context"
	"fmt"
	"sort"
	"testing"
	"time"

	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/synthetic"
	"personal-crm/backend/internal/synthetic/factory"
	"personal-crm/backend/internal/synthetic/replay"
	"personal-crm/backend/tests/testsupport"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	chat "google.golang.org/api/chat/v1"
)

// Batch replay drives N payloads through one provider pass and settles once per
// dependency generation, where the single adapters settle once per payload. The
// adapters are only trustworthy if that is a pure cost change, so the load-
// bearing test here is ROW-FOR-ROW EQUIVALENCE: one batch of N must leave the
// same interactions, the same directions, the same relative timing, and the same
// contact cadence columns as N sequential single replays.
//
// Every test in this file uses newIsolatedRiverTestDB. The harness starts a live
// River client, and namespace scoping does not isolate river_job CONSUMPTION —
// a client draining on a shared database steals sibling tests' jobs. The clone
// additionally makes the GCal drain loop testable at all: its past-event read
// takes a DB-wide LIMIT with the namespace filter applied after it, so on a
// shared database another namespace owning the oldest page is deterministic
// starvation rather than a flake. (The neighbouring synthetic profile suite uses
// the shared package database for its long test; that is a pre-existing
// precedent, not one to copy.)
//
// None of these call t.Parallel(). Each clone costs about seven connections
// against a 100-connection stock local container, and this file mints more than
// a dozen; they are slow-lane routed, where wall-clock is already accepted, and
// running serially keeps concurrent clones near one.

// batchTestHarness builds a harness for a fresh namespace on the given clone.
func batchTestHarness(t *testing.T, ctx context.Context, database *db.Database) *synthetic.Harness {
	t.Helper()
	return synthetic.NewHarnessForNamespace(t, ctx, database, syntheticNS(t), factory.DefaultSeed)
}

// --- equivalence fingerprints ------------------------------------------------

// contactFingerprint is everything about a contact's replayed state that must
// survive the switch from N single replays to one batch. Timestamps are recorded
// RELATIVE to a namespace-wide base, because the two namespaces are anchored at
// the moment their harness was built and their absolute instants therefore
// legitimately differ; every value inside a namespace is anchor-relative, so the
// offsets are exactly comparable.
type contactFingerprint struct {
	Count      int
	Sources    []string
	Directions []string
	Offsets    []time.Duration

	HasLastContacted     bool
	HasLastInteractionAt bool
	HasLastOutreachAt    bool
	HasLastResponseAt    bool

	LastContactedOffset     time.Duration
	LastInteractionAtOffset time.Duration
	LastOutreachAtOffset    time.Duration
	LastResponseAtOffset    time.Duration
}

// namespaceBase is the oldest interaction instant across the given contacts —
// the origin the fingerprints' offsets are measured from.
func namespaceBase(t *testing.T, ctx context.Context, h *synthetic.Harness, contactIDs []uuid.UUID) time.Time {
	t.Helper()
	var base time.Time
	for _, id := range contactIDs {
		rows, err := h.InteractionRepo().ListContactInteractions(ctx, id, 500, 0)
		require.NoError(t, err)
		for _, r := range rows {
			if base.IsZero() || r.OccurredAt.Before(base) {
				base = r.OccurredAt
			}
		}
	}
	require.False(t, base.IsZero(), "expected at least one interaction to anchor the fingerprint")
	return base
}

func fingerprintContact(t *testing.T, ctx context.Context, h *synthetic.Harness, contactID uuid.UUID, base time.Time) contactFingerprint {
	t.Helper()
	rows, err := h.InteractionRepo().ListContactInteractions(ctx, contactID, 500, 0)
	require.NoError(t, err)

	fp := contactFingerprint{Count: len(rows)}
	for _, r := range rows {
		fp.Sources = append(fp.Sources, r.Source)
		fp.Directions = append(fp.Directions, r.Direction)
		fp.Offsets = append(fp.Offsets, r.OccurredAt.Sub(base))
	}
	sort.Strings(fp.Sources)
	sort.Strings(fp.Directions)
	sort.Slice(fp.Offsets, func(i, j int) bool { return fp.Offsets[i] < fp.Offsets[j] })

	contact, err := h.ContactRepo().GetContact(ctx, contactID)
	require.NoError(t, err)
	fp.HasLastContacted, fp.LastContactedOffset = offsetFrom(contact.LastContacted, base)
	fp.HasLastInteractionAt, fp.LastInteractionAtOffset = offsetFrom(contact.LastInteractionAt, base)
	fp.HasLastOutreachAt, fp.LastOutreachAtOffset = offsetFrom(contact.LastOutreachAt, base)
	fp.HasLastResponseAt, fp.LastResponseAtOffset = offsetFrom(contact.LastResponseAt, base)
	return fp
}

func offsetFrom(at *time.Time, base time.Time) (bool, time.Duration) {
	if at == nil {
		return false, 0
	}
	return true, at.Sub(base)
}

// requireEquivalent compares the two namespaces' fingerprints contact by
// contact. The contacts are matched POSITIONALLY: both sides seed the same
// number in the same order with the same payload plan.
func requireEquivalent(t *testing.T, singles, batched []contactFingerprint) {
	t.Helper()
	require.Len(t, batched, len(singles), "both sides must cover the same contacts")
	for i := range singles {
		assert.Equal(t, singles[i], batched[i],
			"contact %d: one batch of N must leave the same rows as N sequential single replays", i)
	}
}

// --- Gmail -------------------------------------------------------------------

// gmailPlan is the per-contact payload plan both sides of an equivalence test
// follow: an age and a direction, oldest first.
type payloadPlan struct {
	Age      time.Duration
	Outbound bool
}

func equivalencePlan() []payloadPlan {
	return []payloadPlan{
		{Age: 20 * 24 * time.Hour},
		{Age: 12 * 24 * time.Hour, Outbound: true},
		{Age: 3 * 24 * time.Hour},
	}
}

func messageOptions(p payloadPlan) []factory.MessageOption {
	opts := []factory.MessageOption{factory.WithMessageAge(p.Age)}
	if p.Outbound {
		opts = append(opts, factory.WithOutbound())
	}
	return opts
}

func TestSyntheticBatchReplay_Gmail_EquivalentToSingles(t *testing.T) {
	testsupport.RequireLongTests(t)
	ctx := context.Background()
	database, _ := newIsolatedRiverTestDB(t, ctx)

	singles := func() []contactFingerprint {
		h := batchTestHarness(t, ctx, database)
		gen := h.Generator()
		var ids []uuid.UUID
		for i := 0; i < 3; i++ {
			spec := gen.Contact(factory.WithEmail())
			contact, err := h.SeedContact(ctx, spec)
			require.NoError(t, err)
			ids = append(ids, contact.ID)
			for _, p := range equivalencePlan() {
				_, err := h.ReplayGmail(ctx, contact.ID, gen.GmailMessage(spec, factory.MatchSeeded, messageOptions(p)...))
				require.NoError(t, err)
			}
		}
		return fingerprintAll(t, ctx, h, ids)
	}()

	batched := func() []contactFingerprint {
		h := batchTestHarness(t, ctx, database)
		gen := h.Generator()
		var ids []uuid.UUID
		var items []replay.GmailBatchItem
		for i := 0; i < 3; i++ {
			spec := gen.Contact(factory.WithEmail())
			contact, err := h.SeedContact(ctx, spec)
			require.NoError(t, err)
			ids = append(ids, contact.ID)
			for _, p := range equivalencePlan() {
				items = append(items, replay.GmailBatchItem{
					ContactID: contact.ID,
					Spec:      gen.GmailMessage(spec, factory.MatchSeeded, messageOptions(p)...),
				})
			}
		}
		sortItemsOldestFirst(items, func(i int) time.Duration { return gmailAge(items[i]) })

		res, err := h.ReplayGmailBatch(ctx, items)
		require.NoError(t, err)
		assert.Equal(t, len(items), res.Payloads)
		assert.Equal(t, 3, res.Contacts)
		assert.Equal(t, 1, res.SettleCalls, "no promotion pairs means one dependency generation")
		assert.Equal(t, 1, res.SyncCalls, "gmail carries the whole batch in one Sync")
		assert.Equal(t, len(items), res.Interactions, "distinct threads never collapse")
		return fingerprintAll(t, ctx, h, ids)
	}()

	requireEquivalent(t, singles, batched)
}

func fingerprintAll(t *testing.T, ctx context.Context, h *synthetic.Harness, ids []uuid.UUID) []contactFingerprint {
	t.Helper()
	base := namespaceBase(t, ctx, h, ids)
	out := make([]contactFingerprint, 0, len(ids))
	for _, id := range ids {
		out = append(out, fingerprintContact(t, ctx, h, id, base))
	}
	return out
}

// sortItemsOldestFirst puts a batch into the chronological replay order the
// adapters require, using a caller-supplied age projection (larger age = older).
func sortItemsOldestFirst[T any](items []T, ageOf func(int) time.Duration) {
	idx := make([]int, len(items))
	for i := range idx {
		idx[i] = i
	}
	ages := make([]time.Duration, len(items))
	for i := range items {
		ages[i] = ageOf(i)
	}
	sort.SliceStable(idx, func(a, b int) bool { return ages[idx[a]] > ages[idx[b]] })
	reordered := make([]T, len(items))
	for n, i := range idx {
		reordered[n] = items[i]
	}
	copy(items, reordered)
}

func gmailAge(it replay.GmailBatchItem) time.Duration {
	return time.Duration(-it.Spec.Message.InternalDate) * time.Millisecond
}

// --- GChat -------------------------------------------------------------------

func TestSyntheticBatchReplay_GChat_EquivalentToSingles(t *testing.T) {
	testsupport.RequireLongTests(t)
	ctx := context.Background()
	database, _ := newIsolatedRiverTestDB(t, ctx)

	singles := func() []contactFingerprint {
		h := batchTestHarness(t, ctx, database)
		gen := h.Generator()
		var ids []uuid.UUID
		for i := 0; i < 3; i++ {
			spec := gen.Contact(factory.WithEmail())
			contact, err := h.SeedContact(ctx, spec)
			require.NoError(t, err)
			ids = append(ids, contact.ID)
			for _, p := range equivalencePlan() {
				_, err := h.ReplayGChat(ctx, contact.ID, gen.GChatMessage(spec, factory.MatchSeeded, messageOptions(p)...))
				require.NoError(t, err)
			}
		}
		return fingerprintAll(t, ctx, h, ids)
	}()

	batched := func() []contactFingerprint {
		h := batchTestHarness(t, ctx, database)
		gen := h.Generator()
		var ids []uuid.UUID
		var items []replay.GChatBatchItem
		var ages []time.Duration
		for i := 0; i < 3; i++ {
			spec := gen.Contact(factory.WithEmail())
			contact, err := h.SeedContact(ctx, spec)
			require.NoError(t, err)
			ids = append(ids, contact.ID)
			for _, p := range equivalencePlan() {
				items = append(items, replay.GChatBatchItem{
					ContactID: contact.ID,
					Spec:      gen.GChatMessage(spec, factory.MatchSeeded, messageOptions(p)...),
				})
				ages = append(ages, p.Age)
			}
		}
		sortItemsOldestFirst(items, func(i int) time.Duration { return ages[i] })

		res, err := h.ReplayGChatBatch(ctx, items)
		require.NoError(t, err)
		assert.Equal(t, 1, res.SettleCalls, "no promotion pairs means one dependency generation")
		assert.Equal(t, 3, res.Contacts)
		return fingerprintAll(t, ctx, h, ids)
	}()

	requireEquivalent(t, singles, batched)
}

// --- GCal --------------------------------------------------------------------

func TestSyntheticBatchReplay_GCal_EquivalentToSingles(t *testing.T) {
	testsupport.RequireLongTests(t)
	ctx := context.Background()
	database, _ := newIsolatedRiverTestDB(t, ctx)

	singles := func() []contactFingerprint {
		h := batchTestHarness(t, ctx, database)
		gen := h.Generator()
		var ids []uuid.UUID
		for i := 0; i < 3; i++ {
			spec := gen.Contact(factory.WithEmail())
			contact, err := h.SeedContact(ctx, spec)
			require.NoError(t, err)
			ids = append(ids, contact.ID)
			for _, p := range equivalencePlan() {
				_, err := h.ReplayGCal(ctx, contact.ID, gen.GCalEvent(spec, factory.MatchSeeded, factory.WithMessageAge(p.Age)))
				require.NoError(t, err)
			}
		}
		return fingerprintAll(t, ctx, h, ids)
	}()

	batched := func() []contactFingerprint {
		h := batchTestHarness(t, ctx, database)
		gen := h.Generator()
		var ids []uuid.UUID
		var items []replay.GCalBatchItem
		var ages []time.Duration
		for i := 0; i < 3; i++ {
			spec := gen.Contact(factory.WithEmail())
			contact, err := h.SeedContact(ctx, spec)
			require.NoError(t, err)
			ids = append(ids, contact.ID)
			for _, p := range equivalencePlan() {
				items = append(items, replay.GCalBatchItem{
					ContactID: contact.ID,
					Spec:      gen.GCalEvent(spec, factory.MatchSeeded, factory.WithMessageAge(p.Age)),
				})
				ages = append(ages, p.Age)
			}
		}
		sortItemsOldestFirst(items, func(i int) time.Duration { return ages[i] })

		res, err := h.ReplayGCalBatch(ctx, items)
		require.NoError(t, err)
		assert.Equal(t, 1, res.SettleCalls, "calendar has no promotion mechanic, so never more than one generation")
		assert.Equal(t, 1, res.SyncCalls, "a batch under the past-event page size drains in one Sync")
		return fingerprintAll(t, ctx, h, ids)
	}()

	requireEquivalent(t, singles, batched)
}

// --- Telegram ----------------------------------------------------------------

func TestSyntheticBatchReplay_Telegram_EquivalentToSingles(t *testing.T) {
	testsupport.RequireLongTests(t)
	ctx := context.Background()
	database, _ := newIsolatedRiverTestDB(t, ctx)

	singles := func() []contactFingerprint {
		h := batchTestHarness(t, ctx, database)
		gen := h.Generator()
		var ids []uuid.UUID
		for i := 0; i < 3; i++ {
			spec := gen.Contact(factory.WithTelegram())
			contact, err := h.SeedContact(ctx, spec)
			require.NoError(t, err)
			ids = append(ids, contact.ID)
			for _, p := range equivalencePlan() {
				_, err := h.ReplayTelegram(ctx, contact.ID, gen.TelegramMessage(spec, factory.MatchSeeded, messageOptions(p)...))
				require.NoError(t, err)
			}
		}
		return fingerprintAll(t, ctx, h, ids)
	}()

	batched := func() []contactFingerprint {
		h := batchTestHarness(t, ctx, database)
		gen := h.Generator()
		var ids []uuid.UUID
		var items []replay.TelegramBatchItem
		var ages []time.Duration
		for i := 0; i < 3; i++ {
			spec := gen.Contact(factory.WithTelegram())
			contact, err := h.SeedContact(ctx, spec)
			require.NoError(t, err)
			ids = append(ids, contact.ID)
			for _, p := range equivalencePlan() {
				items = append(items, replay.TelegramBatchItem{
					ContactID: contact.ID,
					Spec:      gen.TelegramMessage(spec, factory.MatchSeeded, messageOptions(p)...),
				})
				ages = append(ages, p.Age)
			}
		}
		sortItemsOldestFirst(items, func(i int) time.Duration { return ages[i] })

		res, err := h.ReplayTelegramBatch(ctx, items)
		require.NoError(t, err)
		assert.Equal(t, 1, res.SettleCalls)
		assert.Equal(t, 1, res.SyncCalls, "one handler pass drives the whole generation")
		return fingerprintAll(t, ctx, h, ids)
	}()

	requireEquivalent(t, singles, batched)
}

// --- iMessage ----------------------------------------------------------------

func TestSyntheticBatchReplay_IMessage_EquivalentToSingles(t *testing.T) {
	testsupport.RequireLongTests(t)
	ctx := context.Background()
	database, _ := newIsolatedRiverTestDB(t, ctx)

	singles := func() []contactFingerprint {
		h := batchTestHarness(t, ctx, database)
		gen := h.Generator()
		var ids []uuid.UUID
		for i := 0; i < 3; i++ {
			spec := gen.Contact(factory.WithPhone())
			contact, err := h.SeedContact(ctx, spec)
			require.NoError(t, err)
			ids = append(ids, contact.ID)
			for _, p := range equivalencePlan() {
				imsg, err := gen.IMessage(spec, factory.MatchSeeded, h.MacHostID(), messageOptions(p)...)
				require.NoError(t, err)
				_, err = h.ReplayIMessage(ctx, contact.ID, imsg)
				require.NoError(t, err)
			}
		}
		return fingerprintAll(t, ctx, h, ids)
	}()

	batched := func() []contactFingerprint {
		h := batchTestHarness(t, ctx, database)
		gen := h.Generator()
		var ids []uuid.UUID
		var items []replay.IMessageBatchItem
		var ages []time.Duration
		for i := 0; i < 3; i++ {
			spec := gen.Contact(factory.WithPhone())
			contact, err := h.SeedContact(ctx, spec)
			require.NoError(t, err)
			ids = append(ids, contact.ID)
			for _, p := range equivalencePlan() {
				imsg, err := gen.IMessage(spec, factory.MatchSeeded, h.MacHostID(), messageOptions(p)...)
				require.NoError(t, err)
				items = append(items, replay.IMessageBatchItem{ContactID: contact.ID, Spec: imsg})
				ages = append(ages, p.Age)
			}
		}
		sortItemsOldestFirst(items, func(i int) time.Duration { return ages[i] })

		res, err := h.ReplayIMessageBatch(ctx, items)
		require.NoError(t, err)
		assert.Equal(t, 1, res.SettleCalls)
		assert.Equal(t, 1, res.SyncCalls, "one IngestBatch call drives the whole generation")
		return fingerprintAll(t, ctx, h, ids)
	}()

	requireEquivalent(t, singles, batched)
}

// --- preflight ---------------------------------------------------------------

// TestSyntheticBatchReplay_RejectsInvalidInput proves the STRUCTURAL rejections
// happen before anything is driven: each returns its own named error and leaves
// zero rows behind. The rejection logic itself is unit-tested; what needs a
// database is the "nothing was written" half.
func TestSyntheticBatchReplay_RejectsInvalidInput(t *testing.T) {
	testsupport.RequireLongTests(t)
	ctx := context.Background()
	database, _ := newIsolatedRiverTestDB(t, ctx)

	h := batchTestHarness(t, ctx, database)
	gen := h.Generator()
	spec := gen.Contact(factory.WithEmail())
	contact, err := h.SeedContact(ctx, spec)
	require.NoError(t, err)

	requireNoRows := func(t *testing.T, externalIDs ...string) {
		t.Helper()
		for _, id := range externalIDs {
			exists, err := h.CommsRowExists(ctx, "email", id)
			require.NoError(t, err)
			assert.False(t, exists, "a rejected batch must not have driven %s", id)
		}
	}

	t.Run("empty", func(t *testing.T) {
		_, err := h.ReplayGmailBatch(ctx, nil)
		require.ErrorIs(t, err, replay.ErrBatchEmpty)
	})

	t.Run("duplicate_identifier", func(t *testing.T) {
		msg := gen.GmailMessage(spec, factory.MatchSeeded)
		_, err := h.ReplayGmailBatch(ctx, []replay.GmailBatchItem{
			{ContactID: contact.ID, Spec: msg},
			{ContactID: contact.ID, Spec: msg},
		})
		require.ErrorIs(t, err, replay.ErrBatchDuplicateIdentifier)
		requireNoRows(t, msg.ExternalID)
	})

	t.Run("intent_not_seeded", func(t *testing.T) {
		seeded := gen.GmailMessage(spec, factory.MatchSeeded)
		unknown := gen.GmailMessage(spec, factory.MatchUnknown)
		_, err := h.ReplayGmailBatch(ctx, []replay.GmailBatchItem{
			{ContactID: contact.ID, Spec: seeded},
			{ContactID: contact.ID, Spec: unknown},
		})
		require.ErrorIs(t, err, replay.ErrBatchIntentNotSeeded)
		requireNoRows(t, seeded.ExternalID, unknown.ExternalID)
	})

	t.Run("malformed_pair", func(t *testing.T) {
		a := gen.GmailMessage(spec, factory.MatchSeeded)
		b := gen.GmailMessage(spec, factory.MatchSeeded)
		_, err := h.ReplayGmailBatch(ctx, []replay.GmailBatchItem{
			{ContactID: contact.ID, Spec: a, PairKey: 1},
			{ContactID: contact.ID, Spec: b, PairKey: 1},
		})
		require.ErrorIs(t, err, replay.ErrBatchPairKeyMalformed, "both halves are inbound")
		requireNoRows(t, a.ExternalID, b.ExternalID)
	})
}

// TestSyntheticBatchReplay_RejectsUnownedIdentifier covers the OWNERSHIP
// preflight tier, which is separate because it needs a database read: the spec
// types carry wire payloads, not the expected target, so the check has to
// resolve the contact's methods. Passing a ContactID does not force a match —
// the payload's identifier does — so an unowned identifier would strand and
// surface 30 seconds later as a Gate A timeout naming the wrong cause.
func TestSyntheticBatchReplay_RejectsUnownedIdentifier(t *testing.T) {
	testsupport.RequireLongTests(t)
	ctx := context.Background()
	database, _ := newIsolatedRiverTestDB(t, ctx)

	h := batchTestHarness(t, ctx, database)
	gen := h.Generator()

	ownerSpec := gen.Contact(factory.WithEmail())
	_, err := h.SeedContact(ctx, ownerSpec)
	require.NoError(t, err)
	bystanderSpec := gen.Contact(factory.WithEmail())
	bystander, err := h.SeedContact(ctx, bystanderSpec)
	require.NoError(t, err)

	// The payload addresses the OWNER's email but names the BYSTANDER as target.
	msg := gen.GmailMessage(ownerSpec, factory.MatchSeeded)
	_, err = h.ReplayGmailBatch(ctx, []replay.GmailBatchItem{{ContactID: bystander.ID, Spec: msg}})

	require.ErrorIs(t, err, replay.ErrBatchIdentifierNotOwned)
	assert.Contains(t, err.Error(), ownerSpec.Email, "the error names the identifier the batch actually addressed")

	exists, err := h.CommsRowExists(ctx, "email", msg.ExternalID)
	require.NoError(t, err)
	assert.False(t, exists, "the rejection happens before anything is driven")
}

// --- promotion barrier -------------------------------------------------------

// cloneTelegramReply builds the INBOUND half of a promotion pair: the same peer
// and chat as its outbound (so the reply bridge can fire at all), a distinct
// message id, and a deterministic 6h POSITIVE gap with the outbound strictly
// older. Equal timestamps would be unsafe here — these sources order eligible
// rows only by sent_at and the engine's sort is merely stable, so an equal-
// timestamp pair could nondeterministically become one mutual or two one-sided
// sessions.
func cloneTelegramReply(outbound factory.TelegramMessageSpec) factory.TelegramMessageSpec {
	reply := outbound
	reply.TelegramMessageID = outbound.TelegramMessageID + 1
	reply.Out = false
	reply.SentAt = outbound.SentAt.Add(6 * time.Hour)
	return reply
}

// TestSyntheticBatchReplay_PromotionBarrier pins the dependency-generation
// split. An inbound driven straight after its outbound races: aggregation only
// claims rows and publishes an envelope, and the outbound's INTERACTION is
// written later by a River consumer, so the inbound can aggregate first, find
// nothing to promote, and land a second one-sided row. The single-replay path
// never saw this because its per-payload Settle was the barrier.
//
// The generation-scoping rule is asserted by construction rather than by
// inspection: generation 0's Gate A is scoped to generation 0's identifiers, so
// a whole-batch-scoped gate would demand the inbound's row before the inbound
// has been driven and MUST time out at 30 seconds. The call returning without
// error is therefore the proof that the gate is generation-scoped.
func TestSyntheticBatchReplay_PromotionBarrier(t *testing.T) {
	testsupport.RequireLongTests(t)
	ctx := context.Background()
	database, _ := newIsolatedRiverTestDB(t, ctx)

	const repetitions = 3

	withBarrier := func(t *testing.T) int {
		h := batchTestHarness(t, ctx, database)
		gen := h.Generator()
		spec := gen.Contact(factory.WithTelegram())
		contact, err := h.SeedContact(ctx, spec)
		require.NoError(t, err)

		outbound := gen.TelegramMessage(spec, factory.MatchSeeded, factory.WithOutbound(), factory.WithMessageAge(5*24*time.Hour))
		reply := cloneTelegramReply(outbound)

		res, err := h.ReplayTelegramBatch(ctx, []replay.TelegramBatchItem{
			{ContactID: contact.ID, Spec: outbound, PairKey: 1},
			{ContactID: contact.ID, Spec: reply, PairKey: 1},
		})
		require.NoError(t, err)
		assert.Equal(t, 2, res.SettleCalls, "a promotion pair is two dependency generations")
		assert.Equal(t, 2, res.SyncCalls, "one handler pass per generation, not one per batch")
		assert.Equal(t, 1, res.Interactions, "the pair collapses to ONE promoted row")

		rows, err := h.InteractionRepo().ListContactInteractions(ctx, contact.ID, 100, 0)
		require.NoError(t, err)
		require.Len(t, rows, 1, "outbound + reply must promote in place, not land two one-sided rows")
		assert.Equal(t, "mutual", rows[0].Direction)
		return len(rows)
	}

	// withoutBarrier reports whether the pair COLLAPSED to a single promoted
	// mutual. Row count alone would not distinguish that from a regression that
	// drops the inbound outright, which also leaves one row; counting the two as
	// one would corrupt the very rate the repetition count below is sized
	// against. Only two outcomes are legitimate — the collapse, or the two
	// one-sided rows the race normally leaves — so any other shape fails here
	// rather than being silently bucketed into either.
	withoutBarrier := func(t *testing.T) bool {
		h := batchTestHarness(t, ctx, database)
		gen := h.Generator()
		spec := gen.Contact(factory.WithTelegram())
		contact, err := h.SeedContact(ctx, spec)
		require.NoError(t, err)

		outbound := gen.TelegramMessage(spec, factory.MatchSeeded, factory.WithOutbound(), factory.WithMessageAge(5*24*time.Hour))
		reply := cloneTelegramReply(outbound)

		// PairKey 0 on both forces a SINGLE generation — no settle between the
		// halves. This is the shape the barrier exists to prevent.
		res, err := h.ReplayTelegramBatch(ctx, []replay.TelegramBatchItem{
			{ContactID: contact.ID, Spec: outbound},
			{ContactID: contact.ID, Spec: reply},
		})
		require.NoError(t, err)
		assert.Equal(t, 1, res.SettleCalls)

		rows, err := h.InteractionRepo().ListContactInteractions(ctx, contact.ID, 100, 0)
		require.NoError(t, err)
		if len(rows) == 1 {
			require.Equal(t, "mutual", rows[0].Direction,
				"a lone row must be the promoted mutual — a one-sided one means a half was dropped, not promoted")
			return true
		}
		require.Len(t, rows, 2, "the un-promoted outcome is exactly the two one-sided rows")
		require.ElementsMatch(t, []string{"outbound", "inbound"},
			[]string{rows[0].Direction, rows[1].Direction},
			"the two rows must be one outbound and one inbound — direction admits only these three "+
				"values, so any other pair (a promoted row left beside a duplicate inbound above all) "+
				"is a regression, not the race")
		return false
	}

	// Both variants are repeated: the failure mode is a timing window, so one
	// green run of either would prove very little on its own.
	for i := 0; i < repetitions; i++ {
		assert.Equal(t, 1, withBarrier(t), "run %d: the barrier must be reliable, not lucky", i)
	}

	// The no-barrier variant is UNRELIABLE, not impossible. The outbound's
	// claimed rows are excluded from the inbound's aggregation read for a
	// five-minute TTL, and the outbound's INTERACTION is written later by a River
	// consumer, so the inbound usually has nothing to promote against and lands
	// its own row — but when the consumer wins the race the pair does collapse to
	// one mutual.
	//
	// Two measurements exist, of DIFFERENT shapes; they are not interchangeable
	// and a re-measurement should say which it took. A tight loop of 50
	// consecutive executions inside one warm process collapsed 60 times in 400
	// (15.0%) over eight batches spanning idle to heavily loaded, worst batch 13
	// of 50 (26%). A colder shape — twenty fresh processes, three executions each
	// — collapsed 4 times in 60 (6.7%), which was already enough to fail a strict
	// "never collapses" on 4 of those 20 runs. The loop below is the warm shape
	// and is sized against the higher 15%.
	//
	// What is true, and what the barrier's justification rests on, is that the
	// un-barriered path cannot be RELIED on to promote: over noBarrierRepetitions
	// runs at least one must decline to. That is wrong only if EVERY run
	// collapses. Under independence that is p^10 — 5.8e-9 at 15%, 1.4e-6 at the
	// worst batch's 26%. Independence is ASSUMED, not established: 12 adjacent
	// collapse pairs against 8.8 expected is 1.1 sigma, which fails to refute
	// independence rather than evidencing it, and only the longest streak (3
	// observed, ~3 expected) genuinely matches it. Taken as real clustering the
	// conditional rate is 12/58 = 0.21, a 1.38x lift; carrying that lift onto the
	// worst batch's marginal gives the pessimistic figure, 0.26 * 0.359^9 =
	// 2.6e-5. A second assumption rides underneath: the ten runs share one
	// process, one database and one load state, so the honest quantity is E[p^10]
	// across environments, dominated by the worst one. The batches above bound
	// that on a developer machine; they cannot bound an arbitrary CI runner.
	// Even the pessimistic 2.6e-5 is four orders of magnitude below the ~19% at
	// which the strict form false-positived, which is what makes 10 the count.
	//
	// The guard is deliberately weak against PARTIAL reliability shifts: it fires
	// only 10.7% of the time if the collapse rate reached 80%, 34.9% at 90%. That
	// band was never usefully covered — the strict form false-positived on a
	// healthy engine roughly one run in five — and the shift worth catching is
	// the total one: an engine change that makes the un-barriered path reliable
	// promotes every run here, and the barrier's justification then needs
	// re-deriving, not deleting.
	const noBarrierRepetitions = 10
	collapsed := 0
	for i := 0; i < noBarrierRepetitions; i++ {
		if withoutBarrier(t) {
			collapsed++
		}
	}
	t.Logf("no-barrier: %d of %d runs collapsed to a single mutual", collapsed, noBarrierRepetitions)
	assert.Less(t, collapsed, noBarrierRepetitions,
		"every one of %d runs forcing both halves into one generation collapsed to a single mutual — "+
			"the un-barriered path has become reliable, so the barrier's justification has changed "+
			"and needs re-deriving, not deleting", noBarrierRepetitions)
}

// TestSyntheticBatchReplay_TelegramOrderPromotesMutual proves the TIMING
// contract is real and not incidental. Session promotion requires the outbound
// to precede the inbound in time, so a pair whose inbound carries the older
// timestamp does not promote — even though the adapter drives it in the same
// outbound-first order, since partitionGenerations always defers the inbound
// half.
func TestSyntheticBatchReplay_TelegramOrderPromotesMutual(t *testing.T) {
	testsupport.RequireLongTests(t)
	ctx := context.Background()
	database, _ := newIsolatedRiverTestDB(t, ctx)

	t.Run("chronological_promotes", func(t *testing.T) {
		h := batchTestHarness(t, ctx, database)
		gen := h.Generator()
		spec := gen.Contact(factory.WithTelegram())
		contact, err := h.SeedContact(ctx, spec)
		require.NoError(t, err)

		outbound := gen.TelegramMessage(spec, factory.MatchSeeded, factory.WithOutbound(), factory.WithMessageAge(5*24*time.Hour))
		reply := cloneTelegramReply(outbound)

		_, err = h.ReplayTelegramBatch(ctx, []replay.TelegramBatchItem{
			{ContactID: contact.ID, Spec: outbound, PairKey: 1},
			{ContactID: contact.ID, Spec: reply, PairKey: 1},
		})
		require.NoError(t, err)

		rows, err := h.InteractionRepo().ListContactInteractions(ctx, contact.ID, 100, 0)
		require.NoError(t, err)
		require.Len(t, rows, 1)
		assert.Equal(t, "mutual", rows[0].Direction)
	})

	t.Run("inbound_timestamped_first_does_not_promote", func(t *testing.T) {
		// The drive ORDER is identical here — partitionGenerations always defers a
		// pair's inbound half, so both subtests drive outbound-then-inbound. What
		// differs is the TIMESTAMPS: this pair's inbound is the older half, and the
		// reply bridge requires the outbound to precede the inbound in time. So
		// what this proves is that the timing contract is load-bearing on its own,
		// independent of drive order.
		h := batchTestHarness(t, ctx, database)
		gen := h.Generator()
		spec := gen.Contact(factory.WithTelegram())
		contact, err := h.SeedContact(ctx, spec)
		require.NoError(t, err)

		// The INBOUND is the older half, so there is no prior outbound interaction
		// for it to promote against — the pair stays two one-sided rows.
		inbound := gen.TelegramMessage(spec, factory.MatchSeeded, factory.WithMessageAge(5*24*time.Hour))
		outbound := inbound
		outbound.TelegramMessageID = inbound.TelegramMessageID + 1
		outbound.Out = true
		outbound.SentAt = inbound.SentAt.Add(6 * time.Hour)

		_, err = h.ReplayTelegramBatch(ctx, []replay.TelegramBatchItem{
			{ContactID: contact.ID, Spec: inbound, PairKey: 1},
			{ContactID: contact.ID, Spec: outbound, PairKey: 1},
		})
		require.NoError(t, err)

		rows, err := h.InteractionRepo().ListContactInteractions(ctx, contact.ID, 100, 0)
		require.NoError(t, err)
		directions := map[string]int{}
		for _, r := range rows {
			directions[r.Direction]++
		}
		assert.Zero(t, directions["mutual"], "an inbound with no PRIOR outbound interaction cannot promote")
	})
}

// TestSyntheticBatchReplay_GmailSameLocalDayPromotion pins the Gmail promotion
// rule, which is source-specific: Gmail's aggregation key includes a local day
// computed with time.Local, so the two halves of a pair must land on the SAME
// local day. Equal Age is the only construction that guarantees that
// unconditionally — any nonzero gap can straddle local midnight depending on
// where the moving anchor falls.
//
// It also establishes empirically whether Gmail needs the settle barrier at all:
// its promotion keys on source_ref in the consumer rather than on finding a
// prior interaction, so it MAY be order-insensitive. Both shapes are driven.
func TestSyntheticBatchReplay_GmailSameLocalDayPromotion(t *testing.T) {
	testsupport.RequireLongTests(t)
	ctx := context.Background()
	database, _ := newIsolatedRiverTestDB(t, ctx)

	// pair builds an outbound/inbound Gmail pair at the SAME age sharing one
	// thread — the two conditions the consumer's aggregation key requires.
	pair := func(h *synthetic.Harness, spec factory.ContactSpec) (factory.GmailMessageSpec, factory.GmailMessageSpec) {
		gen := h.Generator()
		const age = 4 * 24 * time.Hour
		outbound := gen.GmailMessage(spec, factory.MatchSeeded, factory.WithOutbound(), factory.WithMessageAge(age))
		inbound := gen.GmailMessage(spec, factory.MatchSeeded, factory.WithMessageAge(age))
		// A fresh ThreadId is minted per call, so the caller must thread them. The
		// timestamps are deliberately NOT copied: equal Age already yields the
		// identical instant off the shared anchor, and asserting that rather than
		// forcing it is what makes assertSameLocalDay able to fail.
		inbound.Message.ThreadId = outbound.Message.ThreadId
		return outbound, inbound
	}

	// The aggregation key includes a LOCAL day, so both halves must land on one.
	// Equal Age is what guarantees that unconditionally; the anchor sweep in the
	// replay package's unit tests proves the counterfactual (a sub-day gap DOES
	// straddle local midnight for some anchors), which is why the rule is equality
	// rather than "a small gap".
	assertSameLocalDay := func(t *testing.T, a, b factory.GmailMessageSpec) {
		t.Helper()
		require.Equal(t, a.Message.InternalDate, b.Message.InternalDate,
			"equal Age must yield the identical instant off the shared anchor")
		at := time.UnixMilli(a.Message.InternalDate).Local()
		bt := time.UnixMilli(b.Message.InternalDate).Local()
		ay, am, ad := at.Date()
		by, bm, bd := bt.Date()
		require.Equal(t, [3]int{ay, int(am), ad}, [3]int{by, int(bm), bd},
			"the pair must land on the same LOCAL day (%s vs %s) — the aggregation key includes it", at, bt)
	}

	t.Run("with_barrier", func(t *testing.T) {
		h := batchTestHarness(t, ctx, database)
		spec := h.Generator().Contact(factory.WithEmail())
		contact, err := h.SeedContact(ctx, spec)
		require.NoError(t, err)
		outbound, inbound := pair(h, spec)
		assertSameLocalDay(t, outbound, inbound)

		res, err := h.ReplayGmailBatch(ctx, []replay.GmailBatchItem{
			{ContactID: contact.ID, Spec: outbound, PairKey: 1},
			{ContactID: contact.ID, Spec: inbound, PairKey: 1},
		})
		require.NoError(t, err)
		assert.Equal(t, 2, res.SettleCalls)
		assert.Equal(t, 2, res.SyncCalls,
			"SyncCalls is per GENERATION: a pair-bearing gmail batch drives twice, not once")
		assert.Equal(t, 1, res.Interactions, "two payloads on one thread and one local day collapse to one row")

		rows, err := h.InteractionRepo().ListContactInteractions(ctx, contact.ID, 100, 0)
		require.NoError(t, err)
		require.Len(t, rows, 1)
		assert.Equal(t, "mutual", rows[0].Direction)
	})

	t.Run("without_barrier", func(t *testing.T) {
		h := batchTestHarness(t, ctx, database)
		spec := h.Generator().Contact(factory.WithEmail())
		contact, err := h.SeedContact(ctx, spec)
		require.NoError(t, err)
		outbound, inbound := pair(h, spec)

		res, err := h.ReplayGmailBatch(ctx, []replay.GmailBatchItem{
			{ContactID: contact.ID, Spec: outbound},
			{ContactID: contact.ID, Spec: inbound},
		})
		require.NoError(t, err)
		assert.Equal(t, 1, res.SettleCalls, "no PairKey means one generation")
		assert.Equal(t, 1, res.SyncCalls, "one generation is one Sync")

		rows, err := h.InteractionRepo().ListContactInteractions(ctx, contact.ID, 100, 0)
		require.NoError(t, err)
		// Gmail promotes on a source_ref the consumer derives from the message
		// itself — contact, thread, local day — rather than by finding a prior
		// interaction, so unlike the messaging sources it does NOT depend on the
		// barrier. The adapter still splits a Gmail PairKey into two generations:
		// that costs one extra Settle per batch carrying a pair, which is O(1),
		// and a per-source exception rests on this one observation while
		// uniformity does not.
		require.Len(t, rows, 1, "gmail promotion is order-insensitive: one generation still yields one mutual")
		assert.Equal(t, "mutual", rows[0].Direction)
	})
}

// --- Gmail span bound --------------------------------------------------------

// TestSyntheticBatchReplay_GmailSpanBoundRejected proves the 168-day reach is
// enforced rather than silently truncated, and that the rejected payloads settle
// fine once the caller buckets them.
func TestSyntheticBatchReplay_GmailSpanBoundRejected(t *testing.T) {
	testsupport.RequireLongTests(t)
	ctx := context.Background()
	database, _ := newIsolatedRiverTestDB(t, ctx)

	h := batchTestHarness(t, ctx, database)
	gen := h.Generator()
	spec := gen.Contact(factory.WithEmail())
	contact, err := h.SeedContact(ctx, spec)
	require.NoError(t, err)

	oldest := gen.GmailMessage(spec, factory.MatchSeeded, factory.WithMessageAge(200*24*time.Hour))
	newest := gen.GmailMessage(spec, factory.MatchSeeded, factory.WithMessageAge(1*24*time.Hour))
	wide := []replay.GmailBatchItem{
		{ContactID: contact.ID, Spec: oldest},
		{ContactID: contact.ID, Spec: newest},
	}

	_, err = h.ReplayGmailBatch(ctx, wide)
	require.ErrorIs(t, err, replay.ErrBatchGmailSpanExceeded)
	assert.Contains(t, err.Error(), (199 * 24 * time.Hour).String(), "the error names the actual span")

	for _, spanned := range []string{oldest.ExternalID, newest.ExternalID} {
		exists, err := h.CommsRowExists(ctx, "email", spanned)
		require.NoError(t, err)
		assert.False(t, exists, "the rejection leaves no partial rows")
	}

	// Bucketed into two batches, the same payloads both settle.
	for _, bucket := range [][]replay.GmailBatchItem{{wide[0]}, {wide[1]}} {
		res, err := h.ReplayGmailBatch(ctx, bucket)
		require.NoError(t, err)
		assert.Equal(t, 1, res.Payloads)
	}
	for _, spanned := range []string{oldest.ExternalID, newest.ExternalID} {
		exists, err := h.CommsRowExists(ctx, "email", spanned)
		require.NoError(t, err)
		assert.True(t, exists, "bucketing is the caller-side answer the error asks for")
	}
}

// --- GCal drain --------------------------------------------------------------

// TestSyntheticBatchReplay_GCalDrainsOverLimit proves the drain loop. The
// provider's past-event publish loop reads one page of 100 per Sync, so a larger
// batch needs several — and MarkLastContactedUpdated is what makes re-Syncing
// make progress rather than repeat. Settle still runs exactly once.
func TestSyntheticBatchReplay_GCalDrainsOverLimit(t *testing.T) {
	testsupport.RequireLongTests(t)
	ctx := context.Background()
	database, _ := newIsolatedRiverTestDB(t, ctx)

	const events = 120

	h := batchTestHarness(t, ctx, database)
	gen := h.Generator()
	spec := gen.Contact(factory.WithEmail())
	contact, err := h.SeedContact(ctx, spec)
	require.NoError(t, err)

	items := make([]replay.GCalBatchItem, 0, events)
	for i := 0; i < events; i++ {
		items = append(items, replay.GCalBatchItem{
			ContactID: contact.ID,
			Spec:      gen.GCalEvent(spec, factory.MatchSeeded, factory.WithMessageAge(time.Duration(events-i)*24*time.Hour)),
		})
	}

	res, err := h.ReplayGCalBatch(ctx, items)
	require.NoError(t, err)
	assert.Greater(t, res.SyncCalls, 1, "more than one past-event page needs more than one Sync")
	assert.Equal(t, 1, res.SettleCalls, "the drain polls a plain count between iterations, and settles once at the end")
	assert.Equal(t, events, res.Payloads)

	support := repository.NewSyntheticSupportRepository(h.Database().Queries)
	gcalIDs := make([]string, 0, events)
	contactIDs := make([]uuid.UUID, 0, events)
	for _, it := range items {
		gcalIDs = append(gcalIDs, it.Spec.GcalEventID)
		contactIDs = append(contactIDs, contact.ID)
	}
	settled, err := support.CountMatchedCalendarEventsByGcalIDs(ctx, gcalIDs, contactIDs)
	require.NoError(t, err)
	assert.Equal(t, int64(events), settled, "every event must reach the matched + published state")
}

// --- GChat bucketing ---------------------------------------------------------

// cloneGChatMessage builds another message in the SAME space as base — the
// clone discipline that makes a group of items one conversation. A group built
// from independent factory calls is not a conversation: each call mints a fresh
// space, which can never bridge and costs the page budget a fresh three pages.
func cloneGChatMessage(base factory.GChatMessageSpec, suffix string, outbound bool, createTime time.Time) factory.GChatMessageSpec {
	// Pick by MEMBERSHIP order rather than by ranging the map: Go randomizes map
	// iteration, so a space with more than two members would otherwise clone a
	// nondeterministic sender.
	sender, me := "", ""
	for _, member := range base.Members {
		if member == nil || member.Member == nil {
			continue
		}
		user := member.Member.Name
		if base.EmailByUser[user] == base.AccountID {
			if me == "" {
				me = user
			}
			continue
		}
		if sender == "" {
			sender = user
		}
	}
	from := sender
	if outbound {
		from = me
	}
	name := base.SpaceName + "/messages/" + suffix
	clone := base
	clone.Message = &chat.Message{
		Name:       name,
		Sender:     &chat.User{Name: from, Type: "HUMAN"},
		Text:       "synthetic chat message",
		CreateTime: createTime.UTC().Format(time.RFC3339Nano),
	}
	clone.ExternalID = name
	return clone
}

// TestSyntheticBatchReplay_GChatBucketsAcrossBudget proves bucketing is
// load-bearing, not incidental. GChat's page budget is shared across the
// membership, content, and edit passes of every space in a sweep, and — unlike
// GCal — it never drains: a fully processed space still costs pages on every
// later sweep. So re-presenting the same space list converges to a permanent
// stall, and only partitioning the input gets a large batch through.
func TestSyntheticBatchReplay_GChatBucketsAcrossBudget(t *testing.T) {
	testsupport.RequireLongTests(t)
	ctx := context.Background()
	database, _ := newIsolatedRiverTestDB(t, ctx)

	const spaces = 60

	buildBatch := func(h *synthetic.Harness) []replay.GChatBatchItem {
		gen := h.Generator()
		spec := gen.Contact(factory.WithEmail())
		contact, err := h.SeedContact(ctx, spec)
		require.NoError(t, err)
		items := make([]replay.GChatBatchItem, 0, spaces)
		for i := 0; i < spaces; i++ {
			items = append(items, replay.GChatBatchItem{
				ContactID: contact.ID,
				Spec:      gen.GChatMessage(spec, factory.MatchSeeded, factory.WithMessageAge(time.Duration(spaces-i)*24*time.Hour)),
			})
		}
		return items
	}

	t.Run("bucketed_completes", func(t *testing.T) {
		h := batchTestHarness(t, ctx, database)
		items := buildBatch(h)

		res, err := h.ReplayGChatBatch(ctx, items)
		require.NoError(t, err)
		assert.Greater(t, res.SyncCalls, 1, "60 spaces do not fit one sweep's page budget")
		assert.Equal(t, 1, res.SettleCalls)

		support := repository.NewSyntheticSupportRepository(h.Database().Queries)
		externalIDs := make([]string, 0, len(items))
		for _, it := range items {
			externalIDs = append(externalIDs, it.Spec.ExternalID)
		}
		settled, err := support.CountSettledGChatMessagesByExternalIDs(ctx, externalIDs)
		require.NoError(t, err)
		assert.Equal(t, int64(spaces), settled, "every space's message must settle")
	})

	t.Run("unbucketed_stalls", func(t *testing.T) {
		// The negative control. With bucketing disabled the adapter degenerates to
		// a pure drain over the whole space list, which is exactly the shape the
		// shared page budget defeats: each sweep re-pays for the spaces it already
		// processed and reaches a little further, until it reaches no further.
		h := batchTestHarness(t, ctx, database)
		items := buildBatch(h)

		// Bucketing off, and generous drain slack: the point is to reach the
		// PLATEAU and have it reported as one. With only a couple of iterations the
		// loop can still be inching forward when it hits its cap, and a cap error
		// is ambiguous between "needed one more pass" and "will never finish" —
		// which is exactly the diagnosis this control exists to make unambiguous.
		res, err := h.ReplayGChatBatch(ctx, items,
			replay.WithGChatSpacesPerSync(0),
			replay.WithGChatDrainSlackSyncs(20))
		require.ErrorIs(t, err, replay.ErrBatchDrainIncomplete,
			"without bucketing the sweep cannot present every space, and that must fail loudly")
		assert.Contains(t, err.Error(), "stalled at",
			"the shortfall is a PLATEAU, not a batch that needed one more pass — "+
				"re-presenting a processed space still costs pages, so progress decays to nothing")

		support := repository.NewSyntheticSupportRepository(h.Database().Queries)
		externalIDs := make([]string, 0, len(items))
		for _, it := range items {
			externalIDs = append(externalIDs, it.Spec.ExternalID)
		}
		present, cErr := support.CountGChatMessagesByExternalIDs(ctx, externalIDs)
		require.NoError(t, cErr)
		assert.Less(t, present, int64(spaces), "the stall is short of the batch")
		t.Logf("unbucketed: stalled at %d of %d spaces after %d syncs", present, spaces, res.SyncCalls)
	})

	t.Run("one_cloned_space_settles_in_one_sync", func(t *testing.T) {
		// The contrast case: K messages sharing ONE cloned space cost one space's
		// pages, not K spaces' — which is what makes a burst conversation
		// affordable at all.
		h := batchTestHarness(t, ctx, database)
		gen := h.Generator()
		spec := gen.Contact(factory.WithEmail())
		contact, err := h.SeedContact(ctx, spec)
		require.NoError(t, err)

		base := gen.GChatMessage(spec, factory.MatchSeeded, factory.WithMessageAge(10*24*time.Hour))
		items := []replay.GChatBatchItem{{ContactID: contact.ID, Spec: base}}
		baseTime, err := time.Parse(time.RFC3339Nano, base.Message.CreateTime)
		require.NoError(t, err)
		for k := 1; k < 6; k++ {
			items = append(items, replay.GChatBatchItem{
				ContactID: contact.ID,
				Spec:      cloneGChatMessage(base, fmt.Sprintf("burst-%d", k), false, baseTime.Add(time.Duration(k)*20*time.Minute)),
			})
		}

		res, err := h.ReplayGChatBatch(ctx, items)
		require.NoError(t, err)
		assert.Equal(t, 1, res.SyncCalls, "one space is one bucket")
		assert.Equal(t, 6, res.Payloads)
		assert.Less(t, res.Interactions, res.Payloads,
			"a burst inside one space's aggregation window collapses into fewer rows than payloads")
	})
}

// TestSyntheticBatchReplay_GChatPromotionBarrier exercises the GChat
// MULTI-GENERATION path, which no other test reaches: the Telegram barrier tests
// drive telegram and the local-day test drives gmail. It matters because a
// declared world that gives one contact a gchat history with a promotion pair
// would be this path's first real caller.
//
// A second generation re-enters the bucket loop, re-points the fake world at a
// different message set, re-reads the sync state, and re-presents an
// ALREADY-SWEPT space to a provider holding a persistent per-space cursor, a
// membership cache and a shared page budget. Each of those could plausibly
// swallow the reply; asserting the settled outcome is what proves none of them
// does.
func TestSyntheticBatchReplay_GChatPromotionBarrier(t *testing.T) {
	testsupport.RequireLongTests(t)
	ctx := context.Background()
	database, _ := newIsolatedRiverTestDB(t, ctx)

	h := batchTestHarness(t, ctx, database)
	gen := h.Generator()
	spec := gen.Contact(factory.WithEmail())
	contact, err := h.SeedContact(ctx, spec)
	require.NoError(t, err)

	// One space, two messages: an outbound and its reply 6h later. The clone is
	// what makes them one conversation — independently generated specs would mint
	// separate spaces and could never bridge.
	outbound := gen.GChatMessage(spec, factory.MatchSeeded, factory.WithOutbound(), factory.WithMessageAge(5*24*time.Hour))
	outboundAt, err := time.Parse(time.RFC3339Nano, outbound.Message.CreateTime)
	require.NoError(t, err)
	reply := cloneGChatMessage(outbound, "reply", false, outboundAt.Add(6*time.Hour))

	res, err := h.ReplayGChatBatch(ctx, []replay.GChatBatchItem{
		{ContactID: contact.ID, Spec: outbound, PairKey: 1},
		{ContactID: contact.ID, Spec: reply, PairKey: 1},
	})
	require.NoError(t, err)
	assert.Equal(t, 2, res.SettleCalls, "a promotion pair is two dependency generations")
	assert.Equal(t, 2, res.SyncCalls, "one bucket per generation, so one Sync each")
	assert.Equal(t, 1, res.Interactions, "the pair collapses to ONE promoted row")

	rows, err := h.InteractionRepo().ListContactInteractions(ctx, contact.ID, 100, 0)
	require.NoError(t, err)
	require.Len(t, rows, 1, "re-presenting the space in generation 1 must deliver the reply, not swallow it")
	assert.Equal(t, "mutual", rows[0].Direction)
	assert.Equal(t, "gchat", rows[0].Source)
}

// --- cleanup -----------------------------------------------------------------

// TestSyntheticBatchReplay_CleanupParity proves a batch leaves the teardown
// exactly as much to reclaim as N singles would. The batch's ledger writes are
// the same as the single adapters' — interactions and the venue nodes the real
// recorders minted, which have no prefix-cleanup fallback of their own.
func TestSyntheticBatchReplay_CleanupParity(t *testing.T) {
	testsupport.RequireLongTests(t)
	ctx := context.Background()
	database, _ := newIsolatedRiverTestDB(t, ctx)

	h, teardown, err := synthetic.NewHarnessWithDBForNamespace(ctx, database, syntheticNS(t), factory.DefaultSeed)
	require.NoError(t, err)
	gen := h.Generator()

	emailSpec := gen.Contact(factory.WithEmail())
	emailContact, err := h.SeedContact(ctx, emailSpec)
	require.NoError(t, err)
	tgSpec := gen.Contact(factory.WithTelegram())
	tgContact, err := h.SeedContact(ctx, tgSpec)
	require.NoError(t, err)

	var gmailItems []replay.GmailBatchItem
	var tgItems []replay.TelegramBatchItem
	for _, p := range equivalencePlan() {
		gmailItems = append(gmailItems, replay.GmailBatchItem{
			ContactID: emailContact.ID,
			Spec:      gen.GmailMessage(emailSpec, factory.MatchSeeded, messageOptions(p)...),
		})
		tgItems = append(tgItems, replay.TelegramBatchItem{
			ContactID: tgContact.ID,
			Spec:      gen.TelegramMessage(tgSpec, factory.MatchSeeded, messageOptions(p)...),
		})
	}
	_, err = h.ReplayGmailBatch(ctx, gmailItems)
	require.NoError(t, err)
	_, err = h.ReplayGCalBatch(ctx, []replay.GCalBatchItem{{
		ContactID: emailContact.ID,
		Spec:      gen.GCalEvent(emailSpec, factory.MatchSeeded, factory.WithMessageAge(6*24*time.Hour)),
	}})
	require.NoError(t, err)
	_, err = h.ReplayTelegramBatch(ctx, tgItems)
	require.NoError(t, err)

	venuesBefore, err := h.VenueNodesRemaining(ctx)
	require.NoError(t, err)
	require.Positive(t, venuesBefore, "the recorders must have minted venue nodes for the batch to reclaim")

	require.NoError(t, teardown(ctx))
	requireNamespaceReclaimed(t, ctx, h)

	// Telegram rows are reclaimed by the EXACT peer ids the batch tracked, not by
	// a namespace prefix — telegram_message has no prefix-bearing column — so they
	// are asserted separately.
	support := repository.NewSyntheticSupportRepository(h.Database().Queries)
	for _, it := range tgItems {
		n, err := support.CountTelegramMessagesByChatAndMessageID(ctx, it.Spec.TelegramChatID, it.Spec.TelegramMessageID)
		require.NoError(t, err)
		assert.Zero(t, n, "teardown must reclaim the batch's telegram rows by tracked peer id")
	}
}

// TestSyntheticBatchReplay_MidBatchFailureIsReclaimable exercises drainPartial
// through a REAL failure rather than an injected one: a GCal batch above the
// past-event page size with the drain cap lowered to a single iteration writes
// its calendar events and lands a page of interactions, then fails.
//
// Registering source identifiers per payload is not enough for that world to be
// reclaimable. Interaction and venue ids are captured only AFTER a Settle that
// never happened; venue nodes have no prefix-cleanup fallback; and teardown
// skips EVERY delete when Gate B has not cleared — precisely the state a
// mid-batch failure leaves. drainPartial is what closes all three.
func TestSyntheticBatchReplay_MidBatchFailureIsReclaimable(t *testing.T) {
	testsupport.RequireLongTests(t)
	ctx := context.Background()
	database, _ := newIsolatedRiverTestDB(t, ctx)

	const events = 120

	h, teardown, err := synthetic.NewHarnessWithDBForNamespace(ctx, database, syntheticNS(t), factory.DefaultSeed)
	require.NoError(t, err)
	gen := h.Generator()
	spec := gen.Contact(factory.WithEmail())
	contact, err := h.SeedContact(ctx, spec)
	require.NoError(t, err)

	items := make([]replay.GCalBatchItem, 0, events)
	for i := 0; i < events; i++ {
		items = append(items, replay.GCalBatchItem{
			ContactID: contact.ID,
			Spec:      gen.GCalEvent(spec, factory.MatchSeeded, factory.WithMessageAge(time.Duration(events-i)*24*time.Hour)),
		})
	}

	// One drive iteration for a batch that needs two: the first Sync writes every
	// calendar_event and publishes a full page of attended interactions, then the
	// loop runs out of iterations. A real failure shape, not an injected one.
	_, err = h.ReplayGCalBatch(ctx, items, replay.WithGCalMaxSyncs(1))
	require.Error(t, err)
	require.ErrorIs(t, err, replay.ErrBatchDrainIncomplete, "the ORIGINAL error survives the partial-drain wrapping")
	assert.Contains(t, err.Error(), "partial drain after failure")
	assert.Contains(t, err.Error(), fmt.Sprintf("of %d", events), "the shortfall error names the count, rather than hanging")

	venuesBefore, err := h.VenueNodesRemaining(ctx)
	require.NoError(t, err)
	require.Positive(t, venuesBefore, "the failed batch must have left venue nodes for the drain to make reclaimable")

	require.NoError(t, teardown(ctx))
	requireNamespaceReclaimed(t, ctx, h)
}

// requireNamespaceReclaimed asserts a teardown reclaimed everything it tracked.
func requireNamespaceReclaimed(t *testing.T, ctx context.Context, h *synthetic.Harness) {
	t.Helper()
	contacts, err := h.ContactsRemaining(ctx)
	require.NoError(t, err)
	assert.Zero(t, contacts, "teardown must reclaim every seeded contact")

	venues, err := h.VenueNodesRemaining(ctx)
	require.NoError(t, err)
	assert.Zero(t, venues, "venue nodes have no prefix-cleanup fallback, so they must be reclaimed by tracked id")
}
