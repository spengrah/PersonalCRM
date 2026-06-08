package tests

import (
	"fmt"
	"testing"
	"time"

	"personal-crm/backend/internal/synthetic"
	"personal-crm/backend/internal/synthetic/factory"
	"personal-crm/backend/tests/testsupport"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// mustUUIDFromString parses a UUID string SeedAll returned, failing the test on
// a malformed value.
func mustUUIDFromString(t *testing.T, s string) uuid.UUID {
	t.Helper()
	id, err := uuid.Parse(s)
	require.NoError(t, err)
	return id
}

// The golden-scenario regression test catches silent seed drift. It has two
// halves with a clear priority:
//
//   - PRIMARY (DB-independent): a fixed (seed, namespace, pinned anchor) must
//     produce a byte-identical NON-timestamp generator stream for a fixed call
//     sequence mirroring SeedAll. This is the load-bearing anti-drift signal — it
//     fails loudly and deterministically if a factory change perturbs the seed
//     stream, with no dependence on co-resident DB state.
//   - SECONDARY (scoped DB graph): SeedAll with a fixed namespace + seed settles
//     to the expected per-(tracked-contact) graph. All reads are contact-id /
//     namespace-prefix scoped, never global table counts, so they are robust to
//     the shared accumulating test DB.
//
// Slow-gated (TestSynthetic prefix + RequireLongTests) for cohesion (the SECONDARY
// half drives the real pipeline + River); E1's synthetic_factory_test.go remains
// the fast pure-determinism unit test.

// goldenStreamSig is the stable (non-timestamp) signature of one generator
// stream. Comparing two of these byte-for-byte is the drift signal.
type goldenStreamSig struct {
	lines []string
}

// buildGoldenStream issues the SAME fixed call sequence a small SeedAll-shaped
// scenario uses and records every non-timestamp identifier. Timestamps are
// deliberately excluded (they are anchor-relative; the anchor is pinned here so
// even they would match, but excluding them keeps the signature about the
// wall-clock-independent determinism claim).
func buildGoldenStream(seed uint64, namespace string, anchor time.Time) goldenStreamSig {
	gen := factory.NewGeneratorAt(seed, namespace, anchor)
	var sig goldenStreamSig
	add := func(label, value string) { sig.lines = append(sig.lines, label+"="+value) }

	add("prefix", gen.Prefix())
	add("peerBandStart", fmt.Sprint(gen.PeerBandStart()))
	add("phoneArea", fmt.Sprint(gen.PhoneAreaCode()))

	// Two email contacts + their Gmail/GChat/GCal payloads, two telegram contacts
	// + their private + group messages — the representative mix SeedAll seeds.
	for i := 0; i < 2; i++ {
		emailSpec := gen.Contact(factory.WithEmail())
		add("contact.email.name", emailSpec.FullName)
		add("contact.email.email", emailSpec.Email)

		gmail := gen.GmailMessage(emailSpec, factory.MatchSeeded)
		add("gmail.externalID", gmail.ExternalID)
		add("gmail.account", gmail.AccountID)

		gchat := gen.GChatMessage(emailSpec, factory.MatchSeeded)
		add("gchat.externalID", gchat.ExternalID)
		add("gchat.space", gchat.SpaceName)

		gcal := gen.GCalEvent(emailSpec, factory.MatchSeeded)
		add("gcal.eventID", gcal.GcalEventID)

		tgSpec := gen.Contact(factory.WithTelegram())
		add("contact.tg.name", tgSpec.FullName)
		add("contact.tg.handle", tgSpec.TelegramHandle)

		tg := gen.TelegramMessage(tgSpec, factory.MatchSeeded)
		add("tg.peerUserID", fmt.Sprint(tg.PeerUserID))
		add("tg.messageID", fmt.Sprint(tg.TelegramMessageID))
		add("tg.username", tg.PeerUsername)

		group := gen.TelegramGroupMessage(tgSpec, factory.MatchSeeded, 5)
		add("group.chatID", fmt.Sprint(group.ChatID))
		add("group.sender", fmt.Sprint(group.SenderUserID))
		add("group.messageID", fmt.Sprint(group.TelegramMessageID))
		add("group.title", group.ChatTitle)
	}
	return sig
}

func TestSyntheticGolden_GeneratorStreamIsStable(t *testing.T) {
	testsupport.RequireLongTests(t)

	// PRIMARY drift signal: identical (seed, namespace, anchor) → byte-identical
	// non-timestamp stream. Pure; no DB.
	anchor := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	a := buildGoldenStream(factory.DefaultSeed, "golden", anchor)
	b := buildGoldenStream(factory.DefaultSeed, "golden", anchor)
	require.Equal(t, a.lines, b.lines, "the generator stream must be byte-identical for the same (seed, namespace, anchor)")

	// A different namespace must produce a DIFFERENT stream (the namespace is a
	// real isolation dimension, not a no-op).
	c := buildGoldenStream(factory.DefaultSeed, "golden2", anchor)
	require.NotEqual(t, a.lines, c.lines, "a different namespace must perturb the stream")
}

func TestSyntheticGolden_SeedAllScopedGraph(t *testing.T) {
	testsupport.RequireLongTests(t)
	database, ctx := newSyntheticDB(t)

	params := synthetic.DefaultParams() // fixed namespace "golden" + DefaultSeed, 1 contact/source
	// The harness's resolveNamespace may re-salt a colliding fixed namespace, so
	// read the EFFECTIVE namespace from the harness rather than hard-coding it.
	h := synthetic.NewHarnessForNamespace(t, ctx, database, params.Namespace, params.Seed)
	params.Namespace = h.Namespace() // align SeedAll's scope with the (possibly re-salted) harness

	res, err := synthetic.SeedAll(ctx, h, params)
	require.NoError(t, err)

	// SeedAll seeds one Gmail-settled and one Telegram-settled contact per count.
	require.Len(t, res.GmailContactIDs, 1)
	require.Len(t, res.TelegramContactIDs, 1)

	// Scoped per-contact graph (never a global table count).
	gmailContact := mustUUIDFromString(t, res.GmailContactIDs[0])
	require.Equal(t, 1, countInteractionsBySource(t, ctx, h, gmailContact, "email"),
		"the gmail-seeded contact must have exactly one email interaction")
	// The gmail contact's comms_message row is linked.
	commsRows, err := h.CommsRepo().ListByContact(ctx, gmailContact)
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(commsRows), 1, "gmail contact must have a linked comms_message")

	tgContact := mustUUIDFromString(t, res.TelegramContactIDs[0])
	require.Equal(t, 1, countInteractionsBySource(t, ctx, h, tgContact, "telegram"),
		"the telegram-seeded contact must have exactly one telegram interaction")
}
