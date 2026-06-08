// Package factory holds deterministic, dependency-light constructors for the
// synthetic-seed toolkit: domain-entity specs (contact, contact_method, note)
// and per-source payloads (gmail/gchat/calendar/telegram/ingest/todoist) that
// the replay adapters feed through the REAL ingestion pipeline.
//
// Determinism is split into two clearly-bounded claims (the helper API and tests
// assert only the appropriate one):
//
//  1. Wall-clock-independent (strong determinism): every NON-timestamp output —
//     names, emails, handles, source-ids, guids, numeric ids, ordering — is a
//     pure function of (seed, namespace). Idempotent re-seed rides on these
//     stable source-ids (upserts/event-dedup key on them, never on timestamps).
//  2. Anchor-relative (reproducible given a fixed anchor): timestamps are
//     anchor.Add(offset) where offset is itself a pure function of
//     (seed, namespace). The anchor defaults to accelerated.GetCurrentTime() but
//     is injectable via NewGeneratorAt so determinism tests can pin it.
//
// PII hygiene: all generated data is OBVIOUSLY synthetic (curated non-real name
// components, the RFC-2606 reserved .example TLD, the 555 fictional phone
// exchange) and namespace-scoped. STRING identifiers (contact full_name, email
// local-part, external_contact source_id, gcal_event_id, message guid, telegram
// handle) carry the 'synth-<ns>-' prefix — that prefix doubles as the isolation
// token. NUMERIC identifiers cannot be string-prefixed, so each is drawn from a
// per-namespace DISJOINT sub-block keyed by a hash bucket: telegram peer_user_id
// (1e9-wide bucket band [1e12,2e12)), telegram_message_id (2e6-wide bucket), and
// PHONE numbers (+1-555-<bucket7>-<index3>, a 1e7-wide bucket). Isolation matters
// because identity matching keys on the exact normalized value DB-wide with NO
// namespace scoping, so two namespaces sharing a phone/peer would cross-match.
// The peer band and phone band are collision-checked at harness setup
// (resolveNamespace re-salts on collision); guarantee is "probabilistically
// disjoint + detected at setup," not a hard mathematical one. No external faker
// dependency — a curated corpus + a seeded math/rand/v2 PRNG.
//
// This package imports only leaf/type packages (repository, events,
// accelerated, tg, todoist types, calendar/gmail/chat API structs); it never
// imports service/provider packages, so nothing in those packages can form an
// import cycle with synthetic.
package factory

import (
	"fmt"
	"hash/fnv"
	"math/rand/v2"
	"time"

	"personal-crm/backend/internal/accelerated"
)

const (
	// SyntheticSourcePrefix is the leading token on every generated string
	// identifier. The namespace is appended to form 'synth-<ns>-'.
	SyntheticSourcePrefix = "synth-"

	// DefaultSeed is the fixed seed used when a caller does not override it.
	// Callers vary it for variation; the same (seed, namespace) yields
	// byte-identical non-timestamp output.
	DefaultSeed uint64 = 0x5EED

	// telegramPeerBandStart is the bottom of the reserved synthetic
	// peer_user_id band [1e12, 2e12). Real Telegram user ids are far below
	// 1e12 and the band fits in int64.
	telegramPeerBandStart int64 = 1_000_000_000_000

	// telegramPeerBucketWidth scopes each namespace to a disjoint 1000-id
	// sub-block keyed by a 1e9-wide hash bucket.
	telegramPeerBucketWidth int64 = 1_000
	telegramPeerBucketCount int64 = 1_000_000_000

	// telegramMsgBandWidth reserves a 2e6 * 1e3 = 2e9 (< 2^31) space for
	// telegram_message_id (int32 is too narrow for the 1e12 peer band).
	telegramMsgBucketCount int32 = 2_000_000
	telegramMsgBucketWidth int32 = 1_000

	// Synthetic phone band. Phones are obviously-fictional strings of the form
	// +1555<bucket7><index3>, where bucket7 is a per-namespace 1e7-wide hash
	// bucket and index3 is the per-namespace contact index (< 1000). This gives
	// each namespace a DISJOINT 1000-value phone sub-block (mirroring the
	// telegram peer-band approach) so identity matching — which keys on the exact
	// normalized phone value DB-wide with NO namespace scoping — can never match
	// across namespaces. The 555 exchange keeps every value obviously synthetic.
	// Probabilistically disjoint + setup-time collision detection, not a hard
	// guarantee (birthday-bound ~3700 namespaces for 50% on the 1e7 bucket space).
	phoneBucketCount int64 = 10_000_000
	phoneBucketWidth int64 = 1_000
)

// MatchIntent controls whether a source payload targets a seeded (known)
// contact or an unknown sender. Honored ONLY where the source has a pending
// equivalent (see the D4 applicability matrix); ignored by Todoist and treated
// as match-only by Gmail/GChat for the unknown case.
type MatchIntent int

const (
	// MatchSeeded targets a seeded contact → settled / flowed-through graph.
	MatchSeeded MatchIntent = iota
	// MatchUnknown targets an unknown sender → pending row (where applicable)
	// or match-only (Gmail/GChat).
	MatchUnknown
)

// Generator is the deterministic source of all synthetic data for one seed run.
// It holds a seeded PRNG, the namespace token, an anchor time, and per-namespace
// monotonic local counters so repeated calls produce distinct-but-deterministic
// identifiers within the run.
type Generator struct {
	rng       *rand.Rand
	namespace string
	anchor    time.Time

	// nsBucket is the 1e9-wide telegram peer bucket for this namespace.
	nsBucket int64
	// nsMsgBucket is the telegram message-id bucket for this namespace.
	nsMsgBucket int32
	// nsPhoneBucket is the 1e7-wide synthetic-phone bucket for this namespace.
	nsPhoneBucket int64

	// Local monotonic counters (per generator instance). They make repeated
	// calls within ONE run distinct; combined with (seed, namespace) the full
	// sequence is reproducible.
	contactSeq  int
	peerSeq     int64
	msgSeq      int32
	phoneSeq    int64
	sourceIDSeq int
}

// NewGenerator builds a Generator with the live accelerated anchor. Use
// NewGeneratorAt to pin the anchor for determinism assertions.
func NewGenerator(seed uint64, namespace string) *Generator {
	return NewGeneratorAt(seed, namespace, accelerated.GetCurrentTime())
}

// NewGeneratorAt builds a Generator with an explicit anchor. Tests pass a fixed
// anchor and assert byte-identical timestamped output; production seeds pass the
// live anchor so cadence/overdue states track the configured time.
func NewGeneratorAt(seed uint64, namespace string, anchor time.Time) *Generator {
	return &Generator{
		rng:           rand.New(rand.NewPCG(seed, seedHash(namespace))),
		namespace:     namespace,
		anchor:        anchor,
		nsBucket:      int64(seedHash(namespace)%uint64(telegramPeerBucketCount)) * telegramPeerBucketWidth,
		nsMsgBucket:   int32(seedHash(namespace)%uint64(telegramMsgBucketCount)) * telegramMsgBucketWidth,
		nsPhoneBucket: int64(seedHash(namespace)%uint64(phoneBucketCount)) * phoneBucketWidth,
	}
}

// Namespace returns the generator's namespace token.
func (g *Generator) Namespace() string { return g.namespace }

// Anchor returns the generator's anchor time.
func (g *Generator) Anchor() time.Time { return g.anchor }

// Prefix is the full namespace prefix ('synth-<ns>-') on every string id.
func (g *Generator) Prefix() string {
	return SyntheticSourcePrefix + g.namespace + "-"
}

// PeerBandStart is the first peer_user_id this namespace can issue; the band is
// [PeerBandStart, PeerBandStart+telegramPeerBucketWidth). Exposed so the harness
// can detect a cross-namespace band collision at setup and the cleanup can scope
// telegram deletes to this namespace's peers.
func (g *Generator) PeerBandStart() int64 {
	return telegramPeerBandStart + g.nsBucket
}

// PeerBandEnd is the exclusive upper bound of this namespace's peer sub-block.
func (g *Generator) PeerBandEnd() int64 {
	return g.PeerBandStart() + telegramPeerBucketWidth
}

// PhoneBandStartIndex / PhoneBandEndIndex bound this namespace's synthetic-phone
// sub-block within the 1e7-bucket * 1e3-index reserved space. They are flat
// indices into that space (bucket*width + localIndex), exposed so the harness can
// detect a cross-namespace phone-band collision at setup. The corresponding phone
// STRINGS are produced by phoneFor; SyntheticPhonePrefix gives the digit prefix
// for prefix-scoped identity cleanup.
func (g *Generator) PhoneBandStartIndex() int64 {
	return g.nsPhoneBucket
}

// PhoneBandEndIndex is the exclusive upper bound of this namespace's phone block.
func (g *Generator) PhoneBandEndIndex() int64 {
	return g.nsPhoneBucket + phoneBucketWidth
}

// SyntheticPhonePrefix is the ns-scoped digit prefix shared by every phone this
// namespace issues (normalized form: +1555<bucket7>...). Identity cleanup deletes
// phone external_identity rows by this normalized prefix.
func (g *Generator) SyntheticPhonePrefix() string {
	return fmt.Sprintf("+1555%07d", g.nsPhoneBucket/phoneBucketWidth)
}

// seedHash hashes a namespace string into a uint64 for PRNG seeding and bucket
// derivation. STRICT and IMMUTABLE: same input → same output forever.
func seedHash(namespace string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(namespace))
	return h.Sum64()
}

// --- deterministic timestamp helpers ---------------------------------------

// at returns anchor + offset. Used by source factories so every timestamp is a
// deterministic offset from the (injectable) anchor.
func (g *Generator) at(offset time.Duration) time.Time {
	return g.anchor.Add(offset)
}

// recentOffset returns a deterministic negative offset within the last `window`
// (a pure function of the PRNG, so reproducible given the seed).
func (g *Generator) recentOffset(window time.Duration) time.Duration {
	if window <= 0 {
		return 0
	}
	return -time.Duration(g.rng.Int64N(int64(window)))
}

// --- deterministic id helpers ----------------------------------------------

// nextPeerUserID returns the next telegram peer_user_id within this namespace's
// reserved sub-block. Panics if the sub-block (1000 ids) is exhausted — far
// beyond any realistic per-namespace test count.
func (g *Generator) nextPeerUserID() int64 {
	if g.peerSeq >= telegramPeerBucketWidth {
		panic(fmt.Sprintf("synthetic: telegram peer sub-block exhausted for namespace %q", g.namespace))
	}
	id := g.PeerBandStart() + g.peerSeq
	g.peerSeq++
	return id
}

// nextTelegramMessageID returns the next telegram_message_id within this
// namespace's reserved message-id band.
func (g *Generator) nextTelegramMessageID() int32 {
	if g.msgSeq >= telegramMsgBucketWidth {
		panic(fmt.Sprintf("synthetic: telegram message-id sub-block exhausted for namespace %q", g.namespace))
	}
	id := g.nsMsgBucket + g.msgSeq
	g.msgSeq++
	return id
}
