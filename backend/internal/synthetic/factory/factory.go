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
// components, the RFC-2606 reserved .example TLD, the 555-01XX reserved fictional
// phone range) and namespace-scoped. STRING identifiers (contact full_name, email
// local-part, external_contact source_id, gcal_event_id, message guid, telegram
// handle) carry the 'synth-<ns>-' prefix — that prefix doubles as the isolation
// token. NUMERIC identifiers cannot be string-prefixed, so each is drawn from a
// per-namespace DISJOINT sub-block keyed by a hash bucket: telegram peer_user_id
// (1e9-wide bucket band [1e12,2e12)), telegram_message_id (2e6-wide bucket), and
// PHONE numbers, which are VALID 10-digit NANP numbers in the reserved 555-01XX
// fictional range with the namespace bucket carried in the AREA CODE
// (+1-<area>-555-01<idx2>, e.g. +1-204-555-0107) — real-shaped yet disjoint per
// namespace. Isolation matters because identity matching keys on the exact
// normalized value DB-wide with NO namespace scoping, so two namespaces sharing a
// phone/peer would cross-match. The peer band and phone area code are
// collision-checked at harness setup (resolveNamespace re-salts on collision);
// guarantee is "probabilistically disjoint + detected at setup," not a hard
// mathematical one. No external faker dependency — a curated corpus + a seeded
// math/rand/v2 PRNG.
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

	// Synthetic phone band. Phones are VALID 10-digit NANP numbers in the
	// reserved 555-01XX fictional range (spec D7): +1-<area>-555-01<idx2>, e.g.
	// +1-204-555-0107. The per-namespace disjoint sub-block is keyed by the AREA
	// CODE (a valid NANP code derived from the namespace hash); the line number
	// stays in the strict reserved 555-0100..555-0199 range (idx2 in 00..99 → 100
	// numbers/namespace). Keying the namespace bucket in the area code keeps every
	// number a real-shaped 10-digit NANP value while still giving each namespace a
	// disjoint value set, so identity matching — which keys on the exact
	// normalized value DB-wide with NO namespace scoping — can never cross
	// namespaces. Probabilistically disjoint + setup-time collision detection
	// (~792 usable area codes), not a hard guarantee. phoneLinesPerNS bounds the
	// per-namespace count; exhaustion panics (100/ns is ample for tests).
	phoneLinesPerNS int64 = 100

	// phoneAreaMin/Max bound valid NANP geographic area codes (first digit 2-9).
	phoneAreaMin int64 = 200
	phoneAreaMax int64 = 999
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
	// nsPhoneArea is the per-namespace NANP area code for synthetic phones.
	nsPhoneArea int64

	// Local monotonic counters (per generator instance). They make repeated
	// calls within ONE run distinct; combined with (seed, namespace) the full
	// sequence is reproducible.
	contactSeq  int
	peerSeq     int64
	msgSeq      int32
	phoneSeq    int64
	sourceIDSeq int
	// groupChatSeq counts group chat ids issued from the TOP of this namespace's
	// telegram peer band (growing downward) so they stay disjoint from sender
	// peer ids (issued from the bottom by peerSeq) yet remain in the same
	// collision-checked band.
	groupChatSeq int64
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
		rng:         rand.New(rand.NewPCG(seed, seedHash(namespace))),
		namespace:   namespace,
		anchor:      anchor,
		nsBucket:    int64(seedHash(namespace)%uint64(telegramPeerBucketCount)) * telegramPeerBucketWidth,
		nsMsgBucket: int32(seedHash(namespace)%uint64(telegramMsgBucketCount)) * telegramMsgBucketWidth,
		nsPhoneArea: phoneAreaForHash(seedHash(namespace)),
	}
}

// phoneAreaForHash maps a namespace hash to a VALID NANP area code: first digit
// 2-9, excluding the eight N11 service codes (211, 311, ..., 911). Deterministic
// and IMMUTABLE — same hash → same area code forever.
func phoneAreaForHash(h uint64) int64 {
	span := phoneAreaMax - phoneAreaMin + 1 // 800 candidate codes [200,999]
	area := phoneAreaMin + int64(h%uint64(span))
	// Skip N11 service codes (e.g. 211/311/.../911): last two digits "11".
	if area%100 == 11 {
		area++ // 211→212, ... 911→912; all remain valid geographic codes
	}
	return area
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

// PhoneAreaCode is this namespace's NANP area code (the per-namespace phone
// bucket key). Exposed so the harness can detect a cross-namespace area-code
// collision at setup.
func (g *Generator) PhoneAreaCode() int64 {
	return g.nsPhoneArea
}

// SyntheticPhonePrefix is the ns-scoped NORMALIZED-digit prefix shared by every
// phone this namespace issues: +1<area>55501 (everything before the 2-digit line
// index). NormalizePhoneE164("+1-204-555-0107") == "+12045550107", so this prefix
// (+1<area>55501) matches all of the namespace's normalized phones and only them.
// Identity matching / collision detection / cleanup all scope by this prefix.
func (g *Generator) SyntheticPhonePrefix() string {
	return fmt.Sprintf("+1%d55501", g.nsPhoneArea)
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
// reserved sub-block, allocated from the BOTTOM growing upward. Sender ids
// (this) and group chat ids (nextGroupChatID, top-down) share the band, so the
// guard is the COMBINED count: the two ranges must not meet. Panics on
// exhaustion — far beyond any realistic per-namespace test count.
func (g *Generator) nextPeerUserID() int64 {
	if g.peerSeq+g.groupChatSeq >= telegramPeerBucketWidth {
		panic(fmt.Sprintf("synthetic: telegram peer sub-block exhausted for namespace %q", g.namespace))
	}
	id := g.PeerBandStart() + g.peerSeq
	g.peerSeq++
	return id
}

// nextGroupChatID returns the next telegram group chat id for this namespace,
// allocated from the TOP of the reserved peer sub-block growing DOWNWARD. Sender
// peer ids grow UPWARD from PeerBandStart (nextPeerUserID), so a chat id and a
// sender id can never collide within the band until the two counters meet — far
// beyond any realistic per-namespace test count. Drawing the chat id from the
// SAME collision-checked band keeps it namespace-disjoint, which matters because
// telegram_chat_config.telegram_chat_id is unique DB-wide with no namespace
// column. The chat id never enters PeerMatcher (which keys on the SENDER peer
// id), so co-locating the two id roles in one band cannot cause a matcher
// mis-link. A group conversation reuses ONE chat id across its messages, so this
// is rarely called more than once per conversation. The guard is the COMBINED
// count (sender + chat) so the bottom-up and top-down ranges never meet. Panics
// on band exhaustion.
func (g *Generator) nextGroupChatID() int64 {
	if g.peerSeq+g.groupChatSeq >= telegramPeerBucketWidth {
		panic(fmt.Sprintf("synthetic: telegram group chat-id sub-block exhausted for namespace %q", g.namespace))
	}
	id := g.PeerBandEnd() - 1 - g.groupChatSeq
	g.groupChatSeq++
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
