package tests

import (
	"strings"
	"testing"
	"time"

	"personal-crm/backend/internal/events"
	"personal-crm/backend/internal/matching"
	"personal-crm/backend/internal/synthetic"
	"personal-crm/backend/internal/synthetic/factory"
	"personal-crm/backend/internal/synthetic/replay"

	"github.com/google/uuid"
	"github.com/gotd/td/tg"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	gmailapi "google.golang.org/api/gmail/v1"
)

// These factory tests are PURE (no DB) so they run in both make test-unit
// (-short) and the fast integration set. They assert the two determinism claims
// (wall-clock-independent + anchor-relative) and the namespace isolation
// primitives (string prefixes + telegram numeric sub-blocks).

var fixedAnchor = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

func TestSyntheticFactory_StrongDeterminism_NonTimestampOutput(t *testing.T) {
	t.Parallel()
	g1 := factory.NewGeneratorAt(factory.DefaultSeed, "ns", fixedAnchor)
	g2 := factory.NewGeneratorAt(factory.DefaultSeed, "ns", fixedAnchor)

	// Same (seed, namespace) ⇒ byte-identical non-timestamp output.
	c1 := g1.Contact(factory.WithEmail())
	c2 := g2.Contact(factory.WithEmail())
	assert.Equal(t, c1.FullName, c2.FullName)
	assert.Equal(t, c1.Email, c2.Email)
	require.NotEmpty(t, c1.Email)
}

func TestSyntheticFactory_AnchorRelativeTimestamps(t *testing.T) {
	t.Parallel()
	anchorA := fixedAnchor
	anchorB := fixedAnchor.Add(100 * time.Hour)

	gA := factory.NewGeneratorAt(factory.DefaultSeed, "ns", anchorA)
	gB := factory.NewGeneratorAt(factory.DefaultSeed, "ns", anchorB)

	specA := gA.GCalEvent(gA.Contact(factory.WithEmail()), factory.MatchSeeded)
	specB := gB.GCalEvent(gB.Contact(factory.WithEmail()), factory.MatchSeeded)

	// Non-timestamp output identical given same (seed, ns); the gcal id is stable.
	assert.Equal(t, specA.GcalEventID, specB.GcalEventID)

	// The event start times differ by exactly the anchor delta (anchor-relative).
	startA, err := time.Parse(time.RFC3339, specA.Event.Start.DateTime)
	require.NoError(t, err)
	startB, err := time.Parse(time.RFC3339, specB.Event.Start.DateTime)
	require.NoError(t, err)
	assert.Equal(t, 100*time.Hour, startB.Sub(startA))
}

func TestSyntheticFactory_NamespaceStringPrefixesDisjoint(t *testing.T) {
	t.Parallel()
	gA := factory.NewGeneratorAt(factory.DefaultSeed, "alpha", fixedAnchor)
	gB := factory.NewGeneratorAt(factory.DefaultSeed, "beta", fixedAnchor)

	assert.NotEqual(t, gA.Prefix(), gB.Prefix())
	assert.True(t, strings.HasPrefix(gA.Prefix(), factory.SyntheticSourcePrefix+"alpha-"))

	cA := gA.Contact(factory.WithEmail())
	cB := gB.Contact(factory.WithEmail())
	assert.True(t, strings.HasPrefix(cA.FullName, gA.Prefix()))
	assert.True(t, strings.HasPrefix(cA.Email, gA.Prefix()))
	assert.False(t, strings.HasPrefix(cB.FullName, gA.Prefix()), "namespace B must not collide with A's prefix")
}

func TestSyntheticFactory_TelegramPeerSubBlocksDisjoint(t *testing.T) {
	t.Parallel()
	gA := factory.NewGeneratorAt(factory.DefaultSeed, "alpha", fixedAnchor)
	gB := factory.NewGeneratorAt(factory.DefaultSeed, "beta", fixedAnchor)

	// Distinct namespaces must occupy distinct peer sub-blocks (the bands must
	// not overlap), so cleanup keyed on peer never wipes another namespace's row.
	aStart, aEnd := gA.PeerBandStart(), gA.PeerBandEnd()
	bStart, bEnd := gB.PeerBandStart(), gB.PeerBandEnd()
	require.NotEqual(t, aStart, bStart, "distinct namespaces should derive distinct peer bands")
	// No overlap.
	overlap := aStart < bEnd && bStart < aEnd
	assert.False(t, overlap, "peer sub-blocks must be disjoint: [%d,%d) vs [%d,%d)", aStart, aEnd, bStart, bEnd)

	// Issued peer ids fall inside the namespace's own band.
	spec := gA.TelegramMessage(gA.Contact(factory.WithTelegram()), factory.MatchSeeded)
	assert.GreaterOrEqual(t, spec.PeerUserID, aStart)
	assert.Less(t, spec.PeerUserID, aEnd)
}

func TestSyntheticFactory_PhoneSubBlocksDisjoint(t *testing.T) {
	t.Parallel()
	gA := factory.NewGeneratorAt(factory.DefaultSeed, "alpha", fixedAnchor)
	gB := factory.NewGeneratorAt(factory.DefaultSeed, "beta", fixedAnchor)

	// Distinct namespaces must get distinct phone-digit prefixes, so identity
	// matching (which keys on the exact normalized value DB-wide) can never
	// cross namespaces — the P1 isolation defect this guards against.
	assert.NotEqual(t, gA.SyntheticPhonePrefix(), gB.SyntheticPhonePrefix(),
		"distinct namespaces must derive distinct phone-digit prefixes")

	// Every phone a namespace issues (seeded contact + unknown sender) shares
	// that namespace's prefix and differs from the other namespace's values.
	cA1 := gA.Contact(factory.WithPhone())
	cA2 := gA.Contact(factory.WithPhone())
	cB1 := gB.Contact(factory.WithPhone())
	require.NotEmpty(t, cA1.Phone)
	assert.NotEqual(t, cA1.Phone, cA2.Phone, "within a namespace, phones are distinct per contact")

	// Synthetic phones are valid 10-digit NANP numbers in the 555-01XX reserved
	// range (e.g. +1-204-555-0107); the production normalizer is the source of
	// truth the cleanup prefix + matching depend on, so assert against it.
	assert.Regexp(t, `^\+1-\d{3}-555-01\d{2}$`, cA1.Phone, "synthetic phone must be a valid NANP 555-01XX number")
	normA := matching.NormalizePhoneE164(cA1.Phone)
	normB := matching.NormalizePhoneE164(cB1.Phone)
	assert.True(t, strings.HasPrefix(normA, gA.SyntheticPhonePrefix()),
		"A's normalized phone %q must carry A's prefix %q", normA, gA.SyntheticPhonePrefix())
	assert.False(t, strings.HasPrefix(normB, gA.SyntheticPhonePrefix()),
		"namespace B's phone %q must not fall in namespace A's block", normB)
}

func TestSyntheticNamespaceValidation(t *testing.T) {
	t.Parallel()
	// Safe tokens (lowercase alnum + hyphen) are accepted; cleanup deletes by
	// LIKE 'synth-<ns>-%', so anything with a LIKE metacharacter is rejected.
	for _, ns := range []string{"alpha", "qa-1", "h1234567890", "r0a1b2c3", "seedall"} {
		require.NoError(t, synthetic.ValidateNamespace(ns), "namespace %q should be valid", ns)
	}
	for _, ns := range []string{"qa_1", "a%b", "QA1", "ns space", "ns.dot", "", "ns/slash"} {
		require.Error(t, synthetic.ValidateNamespace(ns), "namespace %q must be rejected (LIKE-unsafe / out of charset)", ns)
	}
}

func TestSyntheticFactory_EdgeCaseOptions(t *testing.T) {
	t.Parallel()
	g := factory.NewGeneratorAt(factory.DefaultSeed, "edge", fixedAnchor)

	// 1900 birthday sentinel.
	bday := g.Contact(factory.WithBirthday1900Sentinel(time.March, 14))
	require.NotNil(t, bday.Birthday)
	assert.Equal(t, 1900, bday.Birthday.Year())
	assert.Equal(t, time.March, bday.Birthday.Month())

	// Unicode name.
	uni := g.Contact(factory.WithUnicodeName())
	assert.True(t, strings.ContainsAny(uni.FullName, "Ünïcödé"), "unicode name should carry non-ASCII")

	// Backdated (created-long-ago) contact: created_at and last_contacted are both
	// stamped to the same past instant (anchor − age).
	const age = 90 * 24 * time.Hour
	od := g.Contact(factory.WithEmail(), factory.WithCadence("weekly"), factory.WithCreatedAge(age))
	require.NotNil(t, od.CreatedAt)
	require.NotNil(t, od.LastContacted)
	assert.Equal(t, *od.CreatedAt, *od.LastContacted, "created_at and last_contacted must be identical")
	assert.Equal(t, fixedAnchor.Add(-age), *od.CreatedAt, "backdated instant must be anchor − age")

	// Recently-created contact: both columns set, equal, and within the window.
	const window = 48 * time.Hour
	rc := g.Contact(factory.WithEmail(), factory.WithCadence("weekly"), factory.WithRecentCreation(window))
	require.NotNil(t, rc.CreatedAt)
	require.NotNil(t, rc.LastContacted)
	assert.Equal(t, *rc.CreatedAt, *rc.LastContacted, "created_at and last_contacted must be identical")
	assert.False(t, rc.CreatedAt.After(fixedAnchor), "recent creation must not be after the anchor")
	assert.False(t, rc.CreatedAt.Before(fixedAnchor.Add(-window)), "recent creation must be within the window")
}

func TestSyntheticFactory_MatchIntentAddressesDistinctIdentifiers(t *testing.T) {
	t.Parallel()
	g := factory.NewGeneratorAt(factory.DefaultSeed, "intent", fixedAnchor)
	target := g.Contact(factory.WithEmail())

	seeded := g.GmailMessage(target, factory.MatchSeeded)
	unknown := g.GmailMessage(target, factory.MatchUnknown)

	seededFrom := gmailHeaderValue(seeded.Message, "From")
	unknownFrom := gmailHeaderValue(unknown.Message, "From")
	assert.Equal(t, target.Email, seededFrom, "seeded message should come from the target's email")
	assert.NotEqual(t, target.Email, unknownFrom, "unknown message must not address the seeded contact")
}

// TestSyntheticFactory_WithOutboundFlipsDirectionMarker asserts WithOutbound()
// produces each source's OUTBOUND payload marker (the field the real provider reads
// for direction), leaving the default inbound marker otherwise. This is a
// payload-marker assertion only — the factories provably never touch the PRNG (only
// deterministic counters), so there is no stream to preserve; counter-order is
// guarded by the determinism fingerprint + the LAST-append placement. The adapter
// wiring (Out → outbound interaction) is covered behaviorally by the mutual-promote
// gate in the profile coverage tests.
func TestSyntheticFactory_WithOutboundFlipsDirectionMarker(t *testing.T) {
	t.Parallel()
	g := factory.NewGeneratorAt(factory.DefaultSeed, "outbound", fixedAnchor)
	emailTarget := g.Contact(factory.WithEmail())
	phoneTarget := g.Contact(factory.WithPhone())
	tgTarget := g.Contact(factory.WithTelegram())
	hostID := uuid.New()

	// Gmail: inbound is From=contact + INBOX; outbound swaps to From=account (≠
	// contact), To=contact, and a SENT label.
	gmIn := g.GmailMessage(emailTarget, factory.MatchSeeded)
	assert.Equal(t, emailTarget.Email, gmailHeaderValue(gmIn.Message, "From"), "inbound gmail is from the contact")
	assert.Equal(t, []string{"INBOX"}, gmIn.Message.LabelIds, "inbound gmail carries the INBOX label")
	gmOut := g.GmailMessage(emailTarget, factory.MatchSeeded, factory.WithOutbound())
	assert.NotEqual(t, emailTarget.Email, gmailHeaderValue(gmOut.Message, "From"), "outbound gmail is from the account, not the contact")
	assert.Equal(t, emailTarget.Email, gmailHeaderValue(gmOut.Message, "To"), "outbound gmail is addressed to the contact")
	assert.Equal(t, []string{"SENT"}, gmOut.Message.LabelIds, "outbound gmail carries the SENT label (provider derives outbound)")

	// GChat: inbound sender resolves to the contact's email; outbound sets the sender
	// to the me-user (resolves to the account, ≠ contact) while the contact stays a
	// resolvable co-member for the outbound fan-out.
	gcIn := g.GChatMessage(emailTarget, factory.MatchSeeded)
	assert.Equal(t, emailTarget.Email, gcIn.EmailByUser[gcIn.Message.Sender.Name], "inbound gchat sender is the contact")
	gcOut := g.GChatMessage(emailTarget, factory.MatchSeeded, factory.WithOutbound())
	assert.NotEqual(t, emailTarget.Email, gcOut.EmailByUser[gcOut.Message.Sender.Name], "outbound gchat sender is the account, not the contact")
	assert.Contains(t, gcOut.EmailByUser, gcOut.Message.Sender.Name, "outbound gchat sender must be resolvable")
	assert.Contains(t, mapStringValues(gcOut.EmailByUser), emailTarget.Email, "the contact stays a resolvable co-member for the outbound fan-out")

	// Telegram: the Out marker the adapter maps to tg.Message.Out, plus the FromID
	// the adapter sets via SetFromID (so GetFromID round-trips) — the peer for
	// inbound, self for outbound.
	tgIn := g.TelegramMessage(tgTarget, factory.MatchSeeded)
	assert.False(t, tgIn.Out, "inbound telegram is incoming")
	_, inUpdate := replay.BuildPrivateUpdate(tgIn)
	inMsg := inUpdate.Message.(*tg.Message)
	assert.False(t, inMsg.Out)
	inFrom, inOK := inMsg.GetFromID()
	require.True(t, inOK, "inbound telegram FromID must round-trip via GetFromID (SetFromID sets the flag bit)")
	assert.Equal(t, &tg.PeerUser{UserID: tgIn.PeerUserID}, inFrom, "inbound telegram is from the peer")

	tgOut := g.TelegramMessage(tgTarget, factory.MatchSeeded, factory.WithOutbound())
	assert.True(t, tgOut.Out, "outbound telegram sets Out")
	_, outUpdate := replay.BuildPrivateUpdate(tgOut)
	outMsg := outUpdate.Message.(*tg.Message)
	assert.True(t, outMsg.Out)
	outFrom, outOK := outMsg.GetFromID()
	require.True(t, outOK, "outbound telegram FromID must round-trip via GetFromID (SetFromID sets the flag bit)")
	outPeer, isUser := outFrom.(*tg.PeerUser)
	require.True(t, isUser, "outbound telegram FromID must be a user peer")
	assert.NotEqual(t, tgOut.PeerUserID, outPeer.UserID, "outbound telegram FromID is self, not the peer")

	// iMessage: the envelope kind (received vs sent) the inline handler reads.
	imIn, err := g.IMessage(phoneTarget, factory.MatchSeeded, hostID)
	require.NoError(t, err)
	assert.Equal(t, events.KindRawMessageReceived, imIn.Envelope.Kind, "inbound imessage is raw_message.received")
	imOut, err := g.IMessage(phoneTarget, factory.MatchSeeded, hostID, factory.WithOutbound())
	require.NoError(t, err)
	assert.Equal(t, events.KindRawMessageSent, imOut.Envelope.Kind, "outbound imessage is raw_message.sent")
}

// mapStringValues returns the values of a string→string map (test helper — the
// stdlib maps.Values returns an iterator, not a slice, in this Go version).
func mapStringValues(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	return out
}

// gmailHeaderValue returns the value of the named header on a gmail message.
func gmailHeaderValue(msg *gmailapi.Message, name string) string {
	if msg == nil || msg.Payload == nil {
		return ""
	}
	for _, h := range msg.Payload.Headers {
		if h.Name == name {
			return h.Value
		}
	}
	return ""
}
