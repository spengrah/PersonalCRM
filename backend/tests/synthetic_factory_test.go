package tests

import (
	"strings"
	"testing"
	"time"

	"personal-crm/backend/internal/synthetic/factory"

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

func TestSyntheticFactory_EdgeCaseOptions(t *testing.T) {
	g := factory.NewGeneratorAt(factory.DefaultSeed, "edge", fixedAnchor)

	// 1900 birthday sentinel.
	bday := g.Contact(factory.WithBirthday1900Sentinel(time.March, 14))
	require.NotNil(t, bday.Birthday)
	assert.Equal(t, 1900, bday.Birthday.Year())
	assert.Equal(t, time.March, bday.Birthday.Month())

	// Unicode name.
	uni := g.Contact(factory.WithUnicodeName())
	assert.True(t, strings.ContainsAny(uni.FullName, "Ünïcödé"), "unicode name should carry non-ASCII")

	// Overdue contact has a past last_contacted.
	od := g.Contact(factory.WithEmail(), factory.WithCadence("weekly"), factory.WithOverdue())
	require.NotNil(t, od.LastContacted)
	assert.True(t, od.LastContacted.Before(fixedAnchor))
}

func TestSyntheticFactory_MatchIntentAddressesDistinctIdentifiers(t *testing.T) {
	g := factory.NewGeneratorAt(factory.DefaultSeed, "intent", fixedAnchor)
	target := g.Contact(factory.WithEmail())

	seeded := g.GmailMessage(target, factory.MatchSeeded)
	unknown := g.GmailMessage(target, factory.MatchUnknown)

	seededFrom := gmailHeaderValue(seeded.Message, "From")
	unknownFrom := gmailHeaderValue(unknown.Message, "From")
	assert.Equal(t, target.Email, seededFrom, "seeded message should come from the target's email")
	assert.NotEqual(t, target.Email, unknownFrom, "unknown message must not address the seeded contact")
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
