package replay

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"personal-crm/backend/internal/synthetic/factory"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	gmailapi "google.golang.org/api/gmail/v1"
)

// The batch preflight is what makes `count == len(items)` a sound terminal
// condition: without it, legal-looking input can never satisfy the gate and the
// adapter reports a 30-second timeout instead of the real cause. These tests pin
// the STRUCTURAL tier, which needs no DB. The ownership tier cannot be tested
// here — the spec types carry wire payloads, not the expected target, so it has
// to resolve through the contact-method repository — and is covered by the
// integration suite.

func batchEntryFixture(id string, opts ...func(*batchEntry)) batchEntry {
	e := batchEntry{contactID: uuid.New(), identifier: id, seeded: true}
	for _, o := range opts {
		o(&e)
	}
	return e
}

func withContact(c uuid.UUID) func(*batchEntry) {
	return func(e *batchEntry) { e.contactID = c }
}

func withPair(key int, outbound bool) func(*batchEntry) {
	return func(e *batchEntry) {
		e.pairKey = key
		e.outbound = outbound
	}
}

func TestValidateBatchStructureRejectsEmptyBatch(t *testing.T) {
	err := validateBatchStructure("gmail", nil)
	require.ErrorIs(t, err, ErrBatchEmpty)
}

func TestValidateBatchStructureRejectsDuplicateIdentifier(t *testing.T) {
	// Source-message replay is idempotent (the bus dedups on (source, source_id)),
	// so two items on one identifier land ONE row and the count can never reach
	// the batch size — the gate would hang to its timeout.
	err := validateBatchStructure("gmail", []batchEntry{
		batchEntryFixture("m-1"),
		batchEntryFixture("m-2"),
		batchEntryFixture("m-1"),
	})
	require.ErrorIs(t, err, ErrBatchDuplicateIdentifier)
	require.Contains(t, err.Error(), "m-1")
}

func TestValidateBatchStructureRejectsNonSeededIntent(t *testing.T) {
	// An unknown-intent payload produces a pending/stranded row and no settled
	// interaction BY DESIGN, so it can never satisfy a settled-count gate.
	err := validateBatchStructure("gmail", []batchEntry{
		batchEntryFixture("m-1"),
		batchEntryFixture("m-2", func(e *batchEntry) { e.seeded = false }),
	})
	require.ErrorIs(t, err, ErrBatchIntentNotSeeded)
}

func TestValidateBatchStructureAcceptsWellFormedBatch(t *testing.T) {
	require.NoError(t, validateBatchStructure("gmail", []batchEntry{
		batchEntryFixture("m-1", withPair(7, true)),
		batchEntryFixture("m-2"),
		batchEntryFixture("m-3", withPair(7, false)),
	}))
}

func TestValidatePairKeysRejectsWrongGroupSize(t *testing.T) {
	// The generation partition is "the inbound member of each PairKey group",
	// which is undefined for a group that is not exactly one outbound and one
	// inbound.
	err := validateBatchStructure("telegram", []batchEntry{
		batchEntryFixture("m-1", withPair(3, true)),
		batchEntryFixture("m-2", withPair(3, false)),
		batchEntryFixture("m-3", withPair(3, false)),
	})
	require.ErrorIs(t, err, ErrBatchPairKeyMalformed)
	require.Contains(t, err.Error(), "3 members")
}

func TestValidatePairKeysRejectsSameDirectionPair(t *testing.T) {
	err := validateBatchStructure("telegram", []batchEntry{
		batchEntryFixture("m-1", withPair(3, true)),
		batchEntryFixture("m-2", withPair(3, true)),
	})
	require.ErrorIs(t, err, ErrBatchPairKeyMalformed)
	require.Contains(t, err.Error(), "share direction")
}

func TestPartitionGenerationsSplitsPromotionPairs(t *testing.T) {
	// generation 0 is everything that is not the inbound member of a pair —
	// including every non-pair member of a burst conversation; generation 1 is
	// the inbound members, which need their outbound's interaction to already
	// exist before they aggregate.
	entries := []batchEntry{
		batchEntryFixture("burst-1"),
		batchEntryFixture("out-1", withPair(1, true)),
		batchEntryFixture("burst-2"),
		batchEntryFixture("in-1", withPair(1, false)),
		batchEntryFixture("out-2", withPair(2, true)),
		batchEntryFixture("in-2", withPair(2, false)),
	}

	gens := partitionGenerations(entries)

	require.Len(t, gens, 2, "a PairKey group has exactly two members, so at most two generations")
	require.Equal(t, []int{0, 1, 2, 4}, gens[0], "generation 0 keeps chronological order and holds both outbounds")
	require.Equal(t, []int{3, 5}, gens[1], "generation 1 is exactly the inbound halves")
}

func TestPartitionGenerationsWithoutPairsIsSingleGeneration(t *testing.T) {
	entries := []batchEntry{
		batchEntryFixture("m-1"),
		batchEntryFixture("m-2", func(e *batchEntry) { e.outbound = true }),
		batchEntryFixture("m-3"),
	}

	gens := partitionGenerations(entries)

	require.Len(t, gens, 1, "no PairKey means no barrier and a single Settle")
	require.Equal(t, []int{0, 1, 2}, gens[0])
}

func TestPartitionGenerationsIgnoresOutboundWithoutPairKey(t *testing.T) {
	// A lone outbound is not a promotion pair; only PairKey membership creates a
	// dependency, so an unpaired inbound stays in generation 0.
	entries := []batchEntry{
		batchEntryFixture("m-1", func(e *batchEntry) { e.outbound = false }),
	}
	gens := partitionGenerations(entries)
	require.Len(t, gens, 1)
	require.Equal(t, []int{0}, gens[0])
}

func TestDistinctContactIDsDedupsInFirstSeenOrder(t *testing.T) {
	a, b := uuid.New(), uuid.New()
	entries := []batchEntry{
		batchEntryFixture("m-1", withContact(a)),
		batchEntryFixture("m-2", withContact(b)),
		batchEntryFixture("m-3", withContact(a)),
		batchEntryFixture("m-4", withContact(b)),
	}

	require.Equal(t, []uuid.UUID{a, b}, distinctContactIDs(entries),
		"per-contact work runs once per contact, not once per payload")
}

// --- Gmail span bound --------------------------------------------------------

func gmailSpecAt(id string, sentAt time.Time, outbound bool) factory.GmailMessageSpec {
	label := "INBOX"
	from, to := "peer@synthetic.example", "me@synthetic.example"
	if outbound {
		label = "SENT"
		from, to = to, from
	}
	return factory.GmailMessageSpec{
		AccountID: "me@synthetic.example",
		Message: &gmailapi.Message{
			Id:           id,
			ThreadId:     "thr-" + id,
			InternalDate: sentAt.UnixMilli(),
			LabelIds:     []string{label},
			Payload: &gmailapi.MessagePart{Headers: []*gmailapi.MessagePartHeader{
				{Name: "From", Value: from},
				{Name: "To", Value: to},
			}},
		},
		ExternalID: id + "@synthetic.example",
		Intent:     factory.MatchSeeded,
	}
}

func gmailItemsSpanning(span time.Duration) []GmailBatchItem {
	base := time.Unix(1_700_000_000, 0).UTC()
	return []GmailBatchItem{
		{ContactID: uuid.New(), Spec: gmailSpecAt("a", base.Add(-span), false)},
		{ContactID: uuid.New(), Spec: gmailSpecAt("b", base.Add(-span/2), false)},
		{ContactID: uuid.New(), Spec: gmailSpecAt("c", base, false)},
	}
}

func TestAgeSpanMeasuresOldestToNewest(t *testing.T) {
	base := time.Unix(1_700_000_000, 0).UTC()
	instants := []time.Time{base.Add(-30 * 24 * time.Hour), base, base.Add(-90 * 24 * time.Hour)}

	require.Equal(t, 90*24*time.Hour, ageSpan(instants), "span is oldest to newest regardless of input order")
	require.Equal(t, base.Add(-90*24*time.Hour), oldestInstant(instants))
	require.Zero(t, ageSpan(nil))
}

func TestGmailBatchSpanWithinReachAcceptsUnderBound(t *testing.T) {
	require.NoError(t, gmailBatchSpanWithinReach(gmailItemsSpanning(gmailBatchMaxSpan)))
	require.NoError(t, gmailBatchSpanWithinReach(gmailItemsSpanning(100*24*time.Hour)))
}

func TestGmailBatchSpanWithinReachRejectsOverBound(t *testing.T) {
	// One Sync reaches 168 days forward from the OLDEST payload; anything past
	// that is never listed and is dropped SILENTLY, which is what makes an error
	// here better than an auto-split.
	err := gmailBatchSpanWithinReach(gmailItemsSpanning(200 * 24 * time.Hour))

	require.ErrorIs(t, err, ErrBatchGmailSpanExceeded)
	require.Contains(t, err.Error(), gmailBatchMaxSpan.String(), "the error names the bound")
	require.Contains(t, err.Error(), (200 * 24 * time.Hour).String(), "the error names the actual span")
}

func TestGmailBatchSpanBoundSitsUnderTheProviderHardLimit(t *testing.T) {
	// The bound is only meaningful if it is strictly under what one Sync reaches:
	// 7-day windows × 24 windows = 168 days, minus the 2-day floor the scan window
	// is dropped to.
	const providerReach = 168 * 24 * time.Hour
	require.Less(t, gmailBatchMaxSpan, providerReach-2*24*time.Hour)
}

func TestGmailBatchAccountRejectsMixedAccounts(t *testing.T) {
	items := gmailItemsSpanning(24 * time.Hour)
	items[1].Spec.AccountID = "other@synthetic.example"

	_, err := gmailBatchAccount(items)

	require.ErrorIs(t, err, ErrBatchMixedAccounts)
}

func TestGmailSpecOutboundReadsTheSentLabel(t *testing.T) {
	require.True(t, gmailSpecOutbound(gmailSpecAt("a", time.Unix(0, 0), true)))
	require.False(t, gmailSpecOutbound(gmailSpecAt("a", time.Unix(0, 0), false)))
}

func TestGmailBatchEntriesAddressesThePeerSide(t *testing.T) {
	// The addressed identifier is the value that decides whether the contact
	// matches, so it must follow the direction: the sender for an inbound, the
	// recipient for an outbound.
	inbound := gmailBatchEntries([]GmailBatchItem{{ContactID: uuid.New(), Spec: gmailSpecAt("a", time.Unix(0, 0), false)}})
	outbound := gmailBatchEntries([]GmailBatchItem{{ContactID: uuid.New(), Spec: gmailSpecAt("b", time.Unix(0, 0), true)}})

	require.Equal(t, "peer@synthetic.example", inbound[0].addressed)
	require.Equal(t, "peer@synthetic.example", outbound[0].addressed)
}

// --- drive loops -------------------------------------------------------------

func TestDriveUntilCountStopsOnCompletion(t *testing.T) {
	// The GCal drain shape: the same input each iteration, the provider advancing
	// irreversibly, so re-driving makes progress.
	landed := int64(0)
	calls, err := driveUntilCount(context.Background(), 250, 5,
		func(context.Context) error { landed += 100; return nil },
		func(context.Context) (int64, error) { return landed, nil })

	require.NoError(t, err)
	require.Equal(t, 3, calls, "250 payloads at 100 per drive needs three iterations")
}

func TestDriveUntilCountFailsLoudlyOnStall(t *testing.T) {
	// A stall is deterministic starvation, not a flake a more generous cap could
	// absorb — so it must name the shortfall rather than spin.
	landed := int64(0)
	calls, err := driveUntilCount(context.Background(), 250, 20,
		func(context.Context) error {
			if landed < 100 {
				landed = 100
			}
			return nil
		},
		func(context.Context) (int64, error) { return landed, nil })

	require.ErrorIs(t, err, ErrBatchDrainIncomplete)
	require.Contains(t, err.Error(), "stalled at 100 of 250")
	require.Equal(t, 2, calls, "the stall is detected on the first iteration that made no progress")
}

func TestDriveUntilCountFailsAtTheIterationCap(t *testing.T) {
	landed := int64(0)
	_, err := driveUntilCount(context.Background(), 250, 2,
		func(context.Context) error { landed += 10; return nil },
		func(context.Context) (int64, error) { return landed, nil })

	require.ErrorIs(t, err, ErrBatchDrainIncomplete)
	require.Contains(t, err.Error(), "reached 20 of 250")
}

func TestDriveUntilCountPropagatesDriveError(t *testing.T) {
	sentinel := errors.New("provider exploded")
	_, err := driveUntilCount(context.Background(), 10, 5,
		func(context.Context) error { return sentinel },
		func(context.Context) (int64, error) { return 0, nil })

	require.ErrorIs(t, err, sentinel)
}

func TestChunkStringsPartitionsAndDisablesCleanly(t *testing.T) {
	spaces := make([]string, 40)
	for i := range spaces {
		spaces[i] = fmt.Sprintf("spaces/S-%d", i)
	}

	buckets := chunkStrings(spaces, 16)
	require.Len(t, buckets, 3)
	require.Len(t, buckets[0], 16)
	require.Len(t, buckets[2], 8)

	// A non-positive size is how the negative control disables bucketing: one
	// bucket holding everything, which is exactly what the shared page budget
	// cannot survive.
	require.Equal(t, [][]string{spaces}, chunkStrings(spaces, 0))
}

func TestGChatBucketSizeFitsTheSharedPageBudget(t *testing.T) {
	// The size is arithmetic, not a round number: a first-sight space costs 3
	// pages (membership + content + edit), and the SAME budget also funds the
	// reverse email→id warm-up, which the provider bounds at 50 fresh resolves per
	// sweep. Both come out of the one 100-page budget.
	const (
		pageBudgetPerSweep      = 100
		pagesPerFirstSightSpace = 3
		maxResolvePagesPerSweep = 50
	)
	worstCase := gchatBatchDefaultSpacesPerSync*pagesPerFirstSightSpace + maxResolvePagesPerSweep
	require.LessOrEqual(t, worstCase, pageBudgetPerSweep,
		"a bucket must complete inside one sweep's budget or its trailing spaces are silently never presented")
}

func TestChatFilterCreateTimeFloorExtractsTheCursor(t *testing.T) {
	require.Equal(t, "2026-07-01T10:00:00Z", chatFilterCreateTimeFloor(`create_time > "2026-07-01T10:00:00Z"`))
	require.Equal(t, "", chatFilterCreateTimeFloor("create_time > unquoted"), "an unparseable filter lets everything through")
}

func TestLaterRFC3339ComparesByInstantNotString(t *testing.T) {
	// Differing fractional-second precision would mis-order these lexically.
	require.True(t, laterRFC3339("2026-07-01T10:00:00.5Z", "2026-07-01T10:00:00.10Z"))
	require.False(t, laterRFC3339("2026-07-01T10:00:00Z", "2026-07-01T10:00:00Z"))
	require.True(t, laterRFC3339("2026-07-01T10:00:00Z", ""), "no floor lets anything through")
}
