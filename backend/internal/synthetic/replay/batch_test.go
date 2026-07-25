package replay

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"personal-crm/backend/internal/google"
	"personal-crm/backend/internal/identity"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/synthetic/factory"

	"personal-crm/backend/internal/events"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	calendarapi "google.golang.org/api/calendar/v3"
	chat "google.golang.org/api/chat/v1"
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
	inbound, err := gmailBatchEntries([]GmailBatchItem{{ContactID: uuid.New(), Spec: gmailSpecAt("a", time.Unix(0, 0), false)}})
	require.NoError(t, err)
	outbound, err := gmailBatchEntries([]GmailBatchItem{{ContactID: uuid.New(), Spec: gmailSpecAt("b", time.Unix(0, 0), true)}})
	require.NoError(t, err)

	require.Equal(t, "peer@synthetic.example", inbound[0].addressed)
	require.Equal(t, "peer@synthetic.example", outbound[0].addressed)
}

func TestGmailBatchEntriesRejectsNilMessage(t *testing.T) {
	// A nil message would otherwise panic inside the span check, before any
	// validation error could name the offending item.
	_, err := gmailBatchEntries([]GmailBatchItem{{ContactID: uuid.New(), Spec: factory.GmailMessageSpec{Intent: factory.MatchSeeded}}})
	require.Error(t, err)
	require.Contains(t, err.Error(), "item 0")
}

// TestGCalBatchEntriesRejectsNilEvent / TestGChatBatchEntriesRejectsNilMessage
// are the symmetric guards. They matter now that a caller builds these items
// programmatically from a generated plan rather than by hand: without them a
// mapper bug becomes a nil dereference deep inside a provider instead of a named
// preflight error at the batch boundary.
func TestGCalBatchEntriesRejectsNilEvent(t *testing.T) {
	_, err := gcalBatchEntries([]GCalBatchItem{{ContactID: uuid.New(), Spec: factory.GCalEventSpec{Intent: factory.MatchSeeded}}})
	require.Error(t, err)
	require.Contains(t, err.Error(), "item 0")
}

func TestGChatBatchEntriesRejectsNilMessage(t *testing.T) {
	_, err := gchatBatchEntries([]GChatBatchItem{{ContactID: uuid.New(), Spec: factory.GChatMessageSpec{Intent: factory.MatchSeeded}}})
	require.Error(t, err)
	require.Contains(t, err.Error(), "item 0")
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
	// The size is arithmetic, not a round number: a first-sight space costs three
	// pages (membership + content + edit), and the SAME budget also funds the
	// reverse email→id warm-up, which the provider bounds by its member-resolve
	// cap. Both come out of the one shared page budget.
	//
	// The budgets are read from the PROVIDER, not mirrored as local literals — a
	// mirrored copy cannot fail when the provider's budget moves, which is the
	// only drift this test exists to catch.
	budget := google.GChatPageBudgetPerSyncForTest()
	resolveCap := google.GChatMemberResolveCapForTest()

	worstCase := gchatBatchDefaultSpacesPerSync*gchatBatchPagesPerFirstSightSpace + resolveCap
	require.LessOrEqual(t, worstCase, budget,
		"a bucket must complete inside one sweep's budget or its trailing spaces are silently never presented")

	// And it must be the LARGEST such size: a bucket one space bigger has to
	// overflow, or the constant is leaving reachable spaces on the table.
	oneMore := (gchatBatchDefaultSpacesPerSync+1)*gchatBatchPagesPerFirstSightSpace + resolveCap
	require.Greater(t, oneMore, budget, "the bucket size should be the maximum the budget admits")
}

func TestGmailSpanBoundSitsUnderTheProviderReach(t *testing.T) {
	// Read from the provider for the same reason as the GChat budget: a local
	// copy of 168 days could not fail if the window span or the window cap moved.
	require.Less(t, gmailBatchMaxSpan, google.GmailScanReachForTest(),
		"a batch inside the bound must be inside what one Sync can actually list")
}

func TestGCalPageSizeMatchesTheProviderPageLimit(t *testing.T) {
	// The drain-loop iteration cap derives from this; if it drifted above the
	// provider's real page the cap would be too low and a valid batch would fail.
	require.Equal(t, google.CalendarPastEventPageLimitForTest(), gcalPastEventPageSize)
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

// --- direction and peer projections -----------------------------------------
//
// These decide BOTH the generation partition and the ownership preflight, so a
// wrong direction silently changes the drive order and a wrong peer silently
// changes which contact the batch claims to be addressing. Each source projects
// from a different field of a different wire shape, so each needs its own case.

func TestIdentifierTypeForMethodNormalizesGChatLikeEmail(t *testing.T) {
	// A GChat sender address IS an email, and the batch compares on the
	// normalized STRING rather than the type — which is what lets a payload
	// addressed as gchat match a contact carrying only an email method.
	const addr = "  Peer@Synthetic.Example  "
	viaEmail := identity.Normalize(addr, identifierTypeForMethod(string(repository.ContactMethodEmail)))
	viaGChat := identity.Normalize(addr, identifierTypeForMethod(string(repository.ContactMethodGChat)))

	require.Equal(t, "peer@synthetic.example", viaEmail)
	require.Equal(t, viaEmail, viaGChat, "gchat and email must normalize identically or the ownership check splits them")
}

func TestIdentifierTypeForMethodCoversEveryMatchableType(t *testing.T) {
	for methodType, want := range map[repository.ContactMethodType]identity.IdentifierType{
		repository.ContactMethodEmail:    identity.IdentifierTypeEmail,
		repository.ContactMethodGChat:    identity.IdentifierTypeGChat,
		repository.ContactMethodPhone:    identity.IdentifierTypePhone,
		repository.ContactMethodTelegram: identity.IdentifierTypeTelegram,
		repository.ContactMethodWhatsApp: identity.IdentifierTypeWhatsApp,
	} {
		require.Equal(t, want, identifierTypeForMethod(string(methodType)), "method type %q", methodType)
	}
}

func TestGChatSpecPeerReadsTheSenderForDirection(t *testing.T) {
	const account = "me@synthetic.example"
	const peerEmail = "peer@synthetic.example"
	base := factory.GChatMessageSpec{
		AccountID: account,
		SpaceName: "spaces/SP-1",
		Members: []*chat.Membership{
			{State: "JOINED", Member: &chat.User{Name: "users/sender", Type: "HUMAN"}},
			{State: "JOINED", Member: &chat.User{Name: "users/me", Type: "HUMAN"}},
		},
		EmailByUser: map[string]string{"users/sender": peerEmail, "users/me": account},
		Intent:      factory.MatchSeeded,
	}

	base.Message = &chat.Message{Name: "m-in", Sender: &chat.User{Name: "users/sender"}}
	peer, outbound := gchatSpecPeer(base)
	require.Equal(t, peerEmail, peer)
	require.False(t, outbound, "a message sent by the co-member is inbound")

	// Outbound: the account is the sender, so the peer is the OTHER co-member —
	// not the sender, which is the account itself.
	base.Message = &chat.Message{Name: "m-out", Sender: &chat.User{Name: "users/me"}}
	peer, outbound = gchatSpecPeer(base)
	require.Equal(t, peerEmail, peer)
	require.True(t, outbound)
}

func TestGCalSpecPeerAttendeeSkipsSelf(t *testing.T) {
	spec := factory.GCalEventSpec{Event: &calendarapi.Event{Attendees: []*calendarapi.EventAttendee{
		{Email: "me@synthetic.example", Self: true},
		{Email: "peer@synthetic.example"},
	}}}
	require.Equal(t, "peer@synthetic.example", gcalSpecPeerAttendee(spec))

	require.Empty(t, gcalSpecPeerAttendee(factory.GCalEventSpec{}), "a nil event addresses nothing")
	require.Empty(t, gcalSpecPeerAttendee(factory.GCalEventSpec{Event: &calendarapi.Event{
		Attendees: []*calendarapi.EventAttendee{{Email: "me@synthetic.example", Self: true}},
	}}), "a self-only event has no peer to own the identifier")
}

func imessageSpecFor(t *testing.T, guid, handle string, outbound bool) factory.IMessageSpec {
	t.Helper()
	payload := events.RawMessageReceivedPayload{
		Version: 1, HostID: uuid.New(), Source: "messages",
		Guid: guid, ChatID: "chat-" + guid, PeerHandle: handle,
		MessageType: "text", SentAt: time.Unix(1_700_000_000, 0).UTC(),
	}
	kind := events.KindRawMessageReceived
	var raw []byte
	var err error
	if outbound {
		kind = events.KindRawMessageSent
		raw, err = events.Marshal(kind, events.RawMessageSentPayload(payload))
	} else {
		raw, err = events.Marshal(kind, payload)
	}
	require.NoError(t, err)
	return factory.IMessageSpec{
		Envelope: &events.Envelope{Source: "messages", SourceID: guid, Kind: kind, Payload: raw},
		Guid:     guid,
		Intent:   factory.MatchSeeded,
	}
}

func TestIMessageBatchEntriesReadsDirectionFromTheKind(t *testing.T) {
	// The sent and received kinds share a field shape but decode to distinct
	// types, so the peer handle can only be read by dispatching on the kind — and
	// the kind is also what the inline ingest handler reads for direction.
	h := &Harness{}
	entries, err := h.imessageBatchEntries([]IMessageBatchItem{
		{ContactID: uuid.New(), Spec: imessageSpecFor(t, "g-in", "+15550001111", false)},
		{ContactID: uuid.New(), Spec: imessageSpecFor(t, "g-out", "+15550001111", true)},
	})
	require.NoError(t, err)

	require.False(t, entries[0].outbound)
	require.True(t, entries[1].outbound)
	require.Equal(t, "+15550001111", entries[0].addressed)
	require.Equal(t, identity.IdentifierTypePhone, entries[0].addressedType)
}

func TestIMessageBatchEntriesRejectsNilEnvelope(t *testing.T) {
	h := &Harness{}
	_, err := h.imessageBatchEntries([]IMessageBatchItem{{ContactID: uuid.New(), Spec: factory.IMessageSpec{Guid: "g"}}})
	require.Error(t, err)
	require.Contains(t, err.Error(), "nil envelope")
}

// --- mixed-account rejection -------------------------------------------------

func TestGChatBatchAccountRejectsMixedAccounts(t *testing.T) {
	items := []GChatBatchItem{
		{Spec: factory.GChatMessageSpec{AccountID: "a@synthetic.example"}},
		{Spec: factory.GChatMessageSpec{AccountID: "b@synthetic.example"}},
	}
	_, err := gchatBatchAccount(items)
	require.ErrorIs(t, err, ErrBatchMixedAccounts)
	require.Contains(t, err.Error(), "item 1")
}

func TestGCalBatchAccountRejectsMixedAccounts(t *testing.T) {
	items := []GCalBatchItem{
		{Spec: factory.GCalEventSpec{AccountID: "a@synthetic.example"}},
		{Spec: factory.GCalEventSpec{AccountID: "b@synthetic.example"}},
	}
	_, err := gcalBatchAccount(items)
	require.ErrorIs(t, err, ErrBatchMixedAccounts)
	require.Contains(t, err.Error(), "item 1")
}

// --- why a Gmail promotion pair must share an Age ----------------------------

// TestGmailPairSameAgeSharesLocalDayAcrossAnchors is the clock-anchored-fixture
// discipline applied to the Gmail promotion rule. The consumer's aggregation key
// includes a LOCAL day, so the two halves of a pair must land on the same one.
//
// The test proves BOTH directions across a sweep of anchors that steps through a
// whole local day (so local midnight falls inside the sweep):
//
//   - equal Age always lands both halves on the same local day, for every
//     anchor — because equal Age means the identical instant, unconditionally;
//   - a nonzero gap does NOT, for at least one anchor in the sweep — which is
//     what makes "same Age" a requirement rather than a stylistic choice.
//
// A rule of "a gap under 24 hours" would pass on most anchors and fail on the
// ones near midnight: the failure class this repo has been bitten by twice.
func TestGmailPairSameAgeSharesLocalDayAcrossAnchors(t *testing.T) {
	localDay := func(spec factory.GmailMessageSpec) (int, time.Month, int) {
		return time.UnixMilli(spec.Message.InternalDate).Local().Date()
	}

	// Step through 24 hours in 30-minute increments, starting from a local
	// midnight, so every part of the day — including the boundary — is covered.
	midnight := time.Date(2026, time.July, 24, 0, 0, 0, 0, time.Local)
	gapStraddledSomewhere := false

	for step := 0; step < 48; step++ {
		anchor := midnight.Add(time.Duration(step) * 30 * time.Minute)
		gen := factory.NewGeneratorAt(factory.DefaultSeed, "anchorsweep", anchor)
		target := gen.Contact(factory.WithEmail())

		const age = 4 * 24 * time.Hour
		outbound := gen.GmailMessage(target, factory.MatchSeeded, factory.WithOutbound(), factory.WithMessageAge(age))
		inbound := gen.GmailMessage(target, factory.MatchSeeded, factory.WithMessageAge(age))

		require.Equal(t, outbound.Message.InternalDate, inbound.Message.InternalDate,
			"anchor %s: equal Age must mean the identical instant", anchor)
		oy, om, od := localDay(outbound)
		iy, im, id := localDay(inbound)
		require.Equal(t, [3]int{oy, int(om), od}, [3]int{iy, int(im), id},
			"anchor %s: the pair must share a local day", anchor)

		// The counterfactual: the same pair with a 12h gap, which is well under a
		// day and would look safe to anyone reasoning in durations.
		gapped := gen.GmailMessage(target, factory.MatchSeeded, factory.WithMessageAge(age+12*time.Hour))
		gy, gm, gd := localDay(gapped)
		if [3]int{gy, int(gm), gd} != [3]int{oy, int(om), od} {
			gapStraddledSomewhere = true
		}
	}

	require.True(t, gapStraddledSomewhere,
		"a sub-day gap must straddle local midnight for SOME anchor — otherwise this test is not "+
			"demonstrating why equal Age is required, and the pair rule could be silently loosened")
}
