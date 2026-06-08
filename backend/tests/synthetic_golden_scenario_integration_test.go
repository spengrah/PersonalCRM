package tests

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"personal-crm/backend/internal/synthetic"
	"personal-crm/backend/internal/synthetic/factory"
	"personal-crm/backend/tests/testsupport"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// updateGolden regenerates the committed golden snapshot from the CURRENT
// factory output (run with `-update`). Off by default so a normal run asserts
// against the committed file and FAILS loudly on any seed/factory drift.
var updateGolden = flag.Bool("update", false, "regenerate the golden generator-stream snapshot")

// goldenStreamPath is the committed snapshot the drift test pins against.
const goldenStreamPath = "testdata/golden_stream.txt"

// goldenStreamSeed/Namespace/Anchor are the FIXED inputs the snapshot is pinned
// to. The anchor is fixed so the (anchor-relative) timestamps the factory
// derives are reproducible; the stream itself records only NON-timestamp fields.
const goldenStreamNamespace = "golden"

var goldenStreamAnchor = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

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
//   - PRIMARY (DB-independent): the generator stream for a FIXED (seed,
//     namespace, anchor) must equal a COMMITTED snapshot. This is the
//     load-bearing anti-drift signal — any factory change that perturbs a name,
//     email, handle, source-id, peer id, or the call ordering changes the live
//     stream and the test FAILS against the pinned file. Regenerate intentionally
//     with `-update` after a deliberate factory change.
//   - SECONDARY (scoped DB graph): SeedAll with a fixed namespace + seed settles
//     to the expected per-(tracked-contact) graph. All reads are contact-id /
//     namespace-prefix scoped, never global table counts, so they are robust to
//     the shared accumulating test DB.
//
// Slow-gated (TestSynthetic prefix + RequireLongTests) for cohesion (the SECONDARY
// half drives the real pipeline + River); E1's synthetic_factory_test.go remains
// the fast pure-determinism unit test.

// buildGoldenStream issues a FIXED representative factory-call sequence (chosen
// to exercise every source factory + both id roles, NOT a literal mirror of
// SeedAll's call order/counts) and records every non-timestamp identifier.
// Timestamps are excluded so the snapshot is about the wall-clock-independent
// determinism claim; the anchor is fixed regardless so anchor-derived ids stay
// reproducible.
func buildGoldenStream(seed uint64, namespace string, anchor time.Time) []string {
	gen := factory.NewGeneratorAt(seed, namespace, anchor)
	var lines []string
	add := func(label, value string) { lines = append(lines, label+"="+value) }

	add("prefix", gen.Prefix())
	add("peerBandStart", fmt.Sprint(gen.PeerBandStart()))
	add("phoneArea", fmt.Sprint(gen.PhoneAreaCode()))

	// Two email contacts + their Gmail/GChat/GCal payloads, two telegram contacts
	// + their private + group messages.
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
	return lines
}

func TestSyntheticGolden_GeneratorStreamIsStable(t *testing.T) {
	testsupport.RequireLongTests(t)

	live := buildGoldenStream(factory.DefaultSeed, goldenStreamNamespace, goldenStreamAnchor)

	if *updateGolden {
		require.NoError(t, os.MkdirAll(filepath.Dir(goldenStreamPath), 0o755))
		require.NoError(t, os.WriteFile(goldenStreamPath, []byte(strings.Join(live, "\n")+"\n"), 0o644))
		t.Logf("wrote golden snapshot %s (%d lines)", goldenStreamPath, len(live))
		return
	}

	// PRIMARY drift signal: the live stream must equal the COMMITTED snapshot. A
	// factory change that perturbs any identifier or the call ordering fails here.
	wantBytes, err := os.ReadFile(goldenStreamPath)
	require.NoError(t, err, "missing golden snapshot — regenerate with `go test -run TestSyntheticGolden_GeneratorStreamIsStable -update`")
	want := strings.Split(strings.TrimRight(string(wantBytes), "\n"), "\n")
	require.Equal(t, want, live,
		"generator stream drifted from the committed snapshot; if intentional, regenerate with `-update`")

	// SUPPLEMENTARY: intra-run determinism (same inputs → byte-identical) ...
	again := buildGoldenStream(factory.DefaultSeed, goldenStreamNamespace, goldenStreamAnchor)
	require.Equal(t, live, again, "the generator stream must be byte-identical for the same (seed, namespace, anchor)")

	// ... and namespace disjointness (a different namespace perturbs the stream).
	other := buildGoldenStream(factory.DefaultSeed, "golden2", goldenStreamAnchor)
	require.NotEqual(t, live, other, "a different namespace must perturb the stream")
}

func TestSyntheticGolden_SeedAllScopedGraph(t *testing.T) {
	testsupport.RequireLongTests(t)
	database, ctx := newSyntheticDB(t)

	params := synthetic.DefaultParams() // DefaultParams: namespace "seedall" + DefaultSeed, 1 contact/source
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
