package factory

import (
	"sort"
	"time"
)

// An ARCHETYPE is a relationship shape — "we meet every few weeks", "I keep
// reaching out and hear nothing back", "we talked a lot in March and then it went
// quiet". A TIMELINE is that shape resolved into an ordered list of source
// payloads to replay: which source, how long before the seed anchor, in which
// direction, and which payloads belong to one conversation.
//
// The type is deliberately a PLAN and not a world. It carries no DB handle, no
// replay adapter and no contact row; everything here is a pure function of
// (seed, namespace, draw position) plus the archetype and the target's method
// set. That is what makes the interesting part — the shape — unit-testable
// without a River queue, and it is why the replay layer, not this one, owns the
// mapping from Source onto the repository/provider constants.

// Archetype names one relationship shape. The catalog is closed: these seven are
// the shapes the synthetic seed models.
type Archetype string

const (
	// ArchetypeMutualRegular is a live two-way relationship: a recurring meeting
	// series still running, with occasional correspondence alongside it.
	ArchetypeMutualRegular Archetype = "mutual-regular"
	// ArchetypeMutualDrifting is a genuinely two-way relationship whose meeting
	// series STOPPED a few months ago. It is the archetype that supplies
	// "contacted AND overdue" — a state the seed produces nowhere else.
	ArchetypeMutualDrifting Archetype = "mutual-drifting"
	// ArchetypeOutboundHeavy is one-sided outreach: everything sent, nothing
	// received. It must never promote to mutual.
	ArchetypeOutboundHeavy Archetype = "outbound-heavy"
	// ArchetypeInboundOnly is the mirror: messages arriving, nothing sent back.
	ArchetypeInboundOnly Archetype = "inbound-only"
	// ArchetypeDormant is a real two-way history that ended months ago. Like
	// mutual-drifting it stays overdue, but from further back.
	ArchetypeDormant Archetype = "dormant"
	// ArchetypeBurstThenQuiet is one dense conversation in one place, followed by
	// silence — the shape that exercises message aggregation's burst window.
	ArchetypeBurstThenQuiet Archetype = "burst-then-quiet"
	// ArchetypeNeverContacted has no history at all. It is the archetype that
	// keeps a no-signal contact in the world.
	ArchetypeNeverContacted Archetype = "never-contacted"
)

// Source names the payload family a timeline entry is replayed through. It is a
// LOCAL enum on purpose. Naming google.CalendarSourceName here would pull
// internal/google into this package's dependency graph, and internal/google
// imports internal/service — which is exactly the property that lets the
// synthetic factory live below the service layer without an import cycle. It
// would compile, which is what makes the mistake easy to miss. The replay layer
// maps these onto the repository/provider constants.
type Source string

const (
	// SourceGCal is a past calendar meeting. It always yields a MUTUAL
	// interaction, so a timeline never sets a direction on it.
	SourceGCal Source = "gcal"
	// SourceEmail is a Gmail message. Direction comes from the payload, and two
	// messages in one thread on one local day promote to mutual.
	SourceEmail Source = "email"
	// SourceGChat is a Google Chat message.
	SourceGChat Source = "gchat"
	// SourceTelegram is a private Telegram message.
	SourceTelegram Source = "telegram"
	// SourceMessages is an iMessage.
	SourceMessages Source = "messages"
)

// MethodSet is what identifiers the target contact actually OWNS. A timeline may
// only emit sources this set can match:
//
//	Email    → gcal / email / gchat
//	Telegram → telegram
//	Phone    → messages
//
// This is structural, not advisory. Passing a contact id to a replay adapter
// does not force a match — the payload's identifier does — so a timeline that
// emitted Telegram for an email-only contact would produce a stranded row and a
// gate timeout blaming the wrong thing. An empty MethodSet therefore yields an
// empty timeline for EVERY archetype, which is what makes a contact with no
// methods at all safe by construction rather than by convention.
type MethodSet struct {
	Email    bool
	Telegram bool
	Phone    bool
}

// TimelineEntry is ONE source payload at ONE anchor-relative age.
type TimelineEntry struct {
	// Source is the payload family. Always one the MethodSet can match.
	Source Source
	// Age is how far BEFORE the seed anchor the payload is dated. Always >= 0,
	// and always a duration — never a calendar position, so the shape survives a
	// moving anchor.
	Age time.Duration
	// Outbound is the direction the payload expresses. Ignored for SourceGCal,
	// which is always mutual.
	Outbound bool
	// ConversationKey groups entries that share ONE source conversation — a Gmail
	// ThreadId, a Telegram peer+chat, a GChat SpaceName. Any size: two for a
	// promotion pair, five to twelve for a burst. Zero means this entry gets its
	// own conversation.
	//
	// Sharing a key is a PLAN-level statement. At replay time the payloads must
	// also share the source's conversation identifier, and the factory mints a
	// fresh one per call — so the caller clones the first spec and overwrites only
	// the message id, direction and timestamp. A group built from independent
	// factory calls is not a conversation and will never bridge or burst.
	ConversationKey int
	// PairKey marks a PROMOTION PAIR: exactly two entries with differing
	// Outbound, always inside one ConversationKey, carrying the timing contract
	// below. Zero means this entry is not part of a pair.
	//
	// Mutuality is EARNED, not declared. Direction is expressed in source terms
	// and the interaction model classifies, so a timeline cannot say "make this
	// mutual" — it constructs the conditions under which the pipeline promotes.
	PairKey int
}

// Timeline is one contact's replay plan in CHRONOLOGICAL REPLAY ORDER.
// Entries[0] is the OLDEST (largest Age); the last entry is the newest.
//
// The ordering is a correctness contract, not a presentation choice. The reply
// bridge promotes only when an inbound session finds an already-existing
// outbound interaction, so replaying newest-first makes the inbound land first
// as a plain inbound and the outbound arriving second can never bridge it — two
// one-sided interactions where a mutual was intended, which is precisely the
// false "one-sided conversation" signal the archetypes exist to stop producing.
type Timeline struct {
	Archetype Archetype
	Entries   []TimelineEntry
}

// --- promotion-pair timing --------------------------------------------------
//
// The pair timing rule is SOURCE-SPECIFIC, and the single rule underneath both
// halves is: the pair's ordering must be fixed by CONSTRUCTION, never left to a
// tie-break the database is free to resolve either way.

const (
	// messagingPairGap separates the two halves of a messaging promotion pair,
	// outbound strictly older. Equal timestamps are unsafe on this path: the
	// aggregation reads order eligible rows only by sent_at, the engine's
	// defensive sort is stable and therefore preserves an unspecified DB tie
	// order, and session promotion requires the outbound to precede the inbound —
	// so an equal-timestamp pair could nondeterministically become one mutual or
	// two one-sided sessions. Six hours is the gap the existing working fixture
	// uses, comfortably inside the 48h bridge window and far from any tie. There
	// is no local-day key on this path, so a moving anchor is harmless.
	messagingPairGap = 6 * time.Hour

	// threadPairGap separates the two halves of a Gmail promotion pair: ZERO, so
	// both are dated at the same instant. Gmail's aggregation key includes a local
	// day computed in the machine's local zone, so ANY nonzero gap can straddle
	// local midnight depending on where the moving anchor lands — a clock-anchored
	// fixture that passes until it doesn't. The same instant is the same local
	// day, unconditionally.
	threadPairGap = 0
)

// --- archetype parameter table ----------------------------------------------
//
// Every bound is relative to the seed anchor, and every one is a judgement call
// about what a realistic history looks like rather than a derived constant. The
// two exceptions — the overdue floors and the mail span bound — are load-bearing
// and are called out where they are declared.

const timelineDay = 24 * time.Hour

const (
	// meetingSpacingMin/Max bound the gap between consecutive meetings in a
	// recurring series. A series is N independent single events (the real fetcher
	// pre-expands recurrence), so "meets every few weeks" IS N entries at
	// increasing multiples of a jittered spacing.
	meetingSpacingMin = 21 * timelineDay
	meetingSpacingMax = 35 * timelineDay

	// mailPairInset keeps a correspondence pair away from the meetings on either
	// side of it, so the interleaving is visible rather than coincident.
	mailPairInset = 2 * timelineDay
)

const (
	// mutual-regular: a still-running series with one correspondence pair beside
	// it. The meeting count is narrow because the count, the spacing bounds and
	// the total span are not independent: the span is the sum of count-1 gaps, so
	// only counts whose gap range intersects [regularSpanMin, regularSpanMax]
	// inside [meetingSpacingMin, meetingSpacingMax] can satisfy all three at once.
	regularMeetingsMin = 7
	regularMeetingsMax = 8
	regularNewestMin   = 3 * timelineDay
	regularNewestMax   = 14 * timelineDay
	// regularSpanMin/Max bound the CALENDAR span — oldest meeting to newest.
	// Deep history is carried by calendar rather than mail because one Gmail sync
	// reaches only a bounded window forward from its backfill point and silently
	// drops anything past it, so the mail entries have to stay recent. The
	// causality is admittedly backwards — a provider bound driving a realism
	// decision — but it happens to align with reality: a long relationship really
	// does show up as recurring meetings more than as one continuous thread.
	regularSpanMin = 180 * timelineDay
	regularSpanMax = 270 * timelineDay
	// regularPairs is fixed at one: two correspondence pairs would need at least
	// nine meetings to keep the span above regularSpanMin, and eleven entries is
	// past what this archetype should cost.
	regularPairs = 1
)

const (
	// mutual-drifting: the same series, stopped 10-16 weeks ago. The floor is the
	// point of the archetype — a replayed inbound or mutual writes both
	// last_contacted and contact_by, so a contact only stays OVERDUE if its newest
	// two-way entry is older than its cadence period. 70 days keeps it overdue on
	// weekly, biweekly and monthly cadences at production durations.
	driftingNewestMin  = 70 * timelineDay
	driftingNewestMax  = 112 * timelineDay
	driftingMeetingMin = 3
	// driftingMeetingMax is lower when the timeline carries two correspondence
	// pairs, so the entry count stays inside the archetype's budget either way.
	driftingMeetingMaxOnePair = 6
	driftingMeetingMaxTwoPair = 4
)

const (
	// outbound-heavy: everything sent, nothing received, and structurally unable
	// to promote — no pairs and no shared conversation.
	outboundCountMin  = 3
	outboundCountMax  = 6
	outboundNewestMin = 2 * timelineDay
	outboundNewestMax = 20 * timelineDay
	outboundGapMin    = 5 * timelineDay
	outboundGapMax    = 25 * timelineDay
)

const (
	// inbound-only: the mirror of outbound-heavy, on distinct conversations.
	inboundCountMin  = 2
	inboundCountMax  = 5
	inboundNewestMin = 1 * timelineDay
	inboundNewestMax = 15 * timelineDay
	inboundGapMin    = 6 * timelineDay
	inboundGapMax    = 30 * timelineDay
)

const (
	// dormant: real two-way history that ended 4-8 months ago. The 120-day floor
	// extends the overdue-compatible cadences up to quarterly at production
	// durations; the oldest bound keeps it dormant rather than prehistoric.
	dormantNewestMin  = 120 * timelineDay
	dormantNewestMax  = 150 * timelineDay
	dormantOldestMax  = 240 * timelineDay
	dormantSpanMin    = 45 * timelineDay
	dormantMeetingMin = 2
	dormantMeetingMax = 7
	// dormantPairInset separates the closing conversation from the last meeting.
	dormantPairInset = 5 * timelineDay
)

const (
	// burst-then-quiet: one dense conversation in ONE place, then silence. The
	// entries all share a single conversation because K independent conversations
	// would be K unrelated interactions that never exercise the burst window at
	// all — and, for GChat, would cost roughly 3K pages of a 100-page sync budget
	// instead of 3.
	burstCountMin  = 5
	burstCountMax  = 12
	burstNewestMin = 30 * timelineDay
	burstNewestMax = 90 * timelineDay
	// burstWidthMin/Max bound the cluster: oldest entry to newest.
	burstWidthMin = 2 * timelineDay
	burstWidthMax = 5 * timelineDay
	// burstOpeningMessages is how many messages open the conversation inside ONE
	// aggregation burst window. Aggregation groups messages into a session by a
	// 2h idle gap, so a cluster spread evenly across days would produce N separate
	// sessions rather than the burst this archetype exists to exercise.
	burstOpeningMessages = 3
	// burstOpeningStride spaces those opening messages. Three of them span twice
	// the stride, which must stay well inside the 2h window.
	burstOpeningStride = 40 * time.Minute
)

// --- per-contact jitter -----------------------------------------------------

// Slot layout of one call's draw vector. The vector is FIXED-SIZE and drawn
// up-front, before any branch on archetype or method set, so every TimelineFor
// call costs exactly timelineJitterDraws draws no matter what it returns. That
// keeps the shared generator's draw accounting independent of which archetype
// lands on which contact: reassigning archetypes cannot shift the stream, and
// the draw cost is one number to pin rather than seven.
const (
	jitterCount     = 0 // primary count (meetings, or entries)
	jitterNewestAge = 1 // age of the newest entry
	jitterPairCount = 2 // how many correspondence pairs
	jitterSpan      = 3 // total span / cluster width

	jitterGapBase    = 4
	timelineGapSlots = 16

	jitterPairBase    = jitterGapBase + timelineGapSlots
	timelinePairSlots = 4

	// timelineJitterDraws is the exact number of PRNG draws one TimelineFor call
	// consumes. Pinned by the draw-order test.
	timelineJitterDraws = jitterPairBase + timelinePairSlots
)

// timelineJitter is one call's draw vector.
type timelineJitter [timelineJitterDraws]uint64

// gap returns the draw backing the i-th inter-entry gap. The slot pool wraps, so
// a timeline with more gaps than slots reuses earlier draws rather than
// consuming a variable number of them.
func (j timelineJitter) gap(i int) uint64 { return j[jitterGapBase+i%timelineGapSlots] }

// pairSlot returns the draw backing the i-th correspondence pair's placement.
func (j timelineJitter) pairSlot(i int) uint64 { return j[jitterPairBase+i%timelinePairSlots] }

// drawTimelineJitter fills the vector from the shared PRNG.
func (g *Generator) drawTimelineJitter() timelineJitter {
	var j timelineJitter
	for i := range j {
		j[i] = g.rng.Uint64()
	}
	return j
}

// pickInt maps a raw draw onto the inclusive range [lo, hi].
func pickInt(u uint64, lo, hi int) int {
	if hi <= lo {
		return lo
	}
	return lo + int(u%uint64(hi-lo+1))
}

// pickDuration maps a raw draw onto [lo, hi] at WHOLE-HOUR granularity, so ages
// stay legible in the golden snapshots and every derived span is exact.
func pickDuration(u uint64, lo, hi time.Duration) time.Duration {
	lo, hi = ceilHours(lo), floorHours(hi)
	if hi <= lo {
		return lo
	}
	steps := uint64((hi-lo)/time.Hour) + 1
	return lo + time.Duration(u%steps)*time.Hour
}

func ceilHours(d time.Duration) time.Duration {
	h := d / time.Hour
	if h*time.Hour < d {
		h++
	}
	return h * time.Hour
}

func floorHours(d time.Duration) time.Duration {
	return (d / time.Hour) * time.Hour
}

// --- source selection -------------------------------------------------------

// sourcePlan is the concrete source assignment for one timeline, resolved from
// the target's method set. An empty field means the role has no source the
// target can match, and the archetype degrades around it.
type sourcePlan struct {
	// meeting is the always-mutual calendar source. Only an email-bearing contact
	// can be a matched attendee, so this is empty for everyone else.
	meeting Source
	// thread is the source whose promotion pair lands at the SAME instant.
	thread Source
	// chat is the messaging-aggregation source: burst windows, reply bridge, and
	// a promotion pair separated by messagingPairGap.
	chat Source
}

func (p sourcePlan) empty() bool { return p.meeting == "" && p.thread == "" && p.chat == "" }

// planFor resolves the roles from what the contact owns. The chat role prefers
// the sources a contact can only have deliberately — a synthetic contact carries
// a Telegram handle or a phone because someone wanted that source exercised,
// whereas every email-bearing contact would otherwise fall to GChat and the
// other two would never be reached. Email-only targets, which is what the frozen
// catalog is, resolve to exactly {gcal, email, gchat}.
func planFor(caps MethodSet) sourcePlan {
	var p sourcePlan
	if caps.Email {
		p.meeting = SourceGCal
		p.thread = SourceEmail
		p.chat = SourceGChat
	}
	switch {
	case caps.Telegram:
		p.chat = SourceTelegram
	case caps.Phone:
		p.chat = SourceMessages
	}
	return p
}

// directional returns the source to use for one-sided history: the threaded mail
// source when the target has one, otherwise the chat source.
func (p sourcePlan) directional() Source {
	if p.thread != "" {
		return p.thread
	}
	return p.chat
}

// --- timeline construction --------------------------------------------------

// timelineBuilder accumulates entries and hands out conversation and pair keys.
// Keys are only ever compared for equality, so a per-timeline counter is enough.
type timelineBuilder struct {
	entries  []TimelineEntry
	convSeq  int
	pairSeq  int
	fallback int // shared conversation for chat-carried mutual exchanges
}

func (b *timelineBuilder) conversation() int {
	b.convSeq++
	return b.convSeq
}

func (b *timelineBuilder) pair() int {
	b.pairSeq++
	return b.pairSeq
}

func (b *timelineBuilder) add(e TimelineEntry) { b.entries = append(b.entries, e) }

// sorted returns the entries OLDEST FIRST. The sort is stable and compares Age
// strictly, so a same-instant pair keeps the insertion order that puts its
// outbound half first — which is the order the replay layer needs.
func (b *timelineBuilder) sorted() []TimelineEntry {
	out := make([]TimelineEntry, len(b.entries))
	copy(out, b.entries)
	sort.SliceStable(out, func(i, k int) bool { return out[i].Age > out[k].Age })
	return out
}

// addMutualExchange records ONE two-way event at the given age.
//
// With a calendar it is a single meeting, which the pipeline always classifies
// as mutual. Without one — a Telegram- or phone-only contact — the same
// two-wayness has to be EARNED, so the exchange becomes a promotion pair on the
// chat source: outbound first, inbound messagingPairGap later, both inside the
// one conversation the timeline holds with that contact.
func (b *timelineBuilder) addMutualExchange(p sourcePlan, age time.Duration) {
	if p.meeting != "" {
		b.add(TimelineEntry{Source: p.meeting, Age: age})
		return
	}
	if b.fallback == 0 {
		b.fallback = b.conversation()
	}
	key := b.pair()
	b.add(TimelineEntry{Source: p.chat, Age: age + messagingPairGap, Outbound: true, ConversationKey: b.fallback, PairKey: key})
	b.add(TimelineEntry{Source: p.chat, Age: age, ConversationKey: b.fallback, PairKey: key})
}

// addThreadPair records a correspondence pair in its own thread: two messages at
// the SAME instant with differing directions, which is what makes the pipeline
// promote them to a single mutual. A target with no mail source gets nothing —
// it has no threaded promotion available, and its two-way history is already
// carried by addMutualExchange.
func (b *timelineBuilder) addThreadPair(p sourcePlan, age time.Duration) {
	if p.thread == "" {
		return
	}
	conv := b.conversation()
	key := b.pair()
	b.add(TimelineEntry{Source: p.thread, Age: age + threadPairGap, Outbound: true, ConversationKey: conv, PairKey: key})
	b.add(TimelineEntry{Source: p.thread, Age: age, ConversationKey: conv, PairKey: key})
}

// addOneSided records count entries in one direction, each on its own
// conversation and none of them paired, so nothing can promote.
func (b *timelineBuilder) addOneSided(src Source, outbound bool, ages []time.Duration) {
	for _, age := range ages {
		b.add(TimelineEntry{Source: src, Age: age, Outbound: outbound})
	}
}

// TimelineFor builds one contact's replay plan.
//
// It is deterministic in (seed, namespace, draw position) and draws its
// per-contact jitter from the SHARED generator PRNG, so it must only ever be
// called from the append-last block of a seed profile. Inserting a call ahead of
// an existing generator consumer shifts every later allocation by one, and a
// shifted numeric identifier can land on an id another contact already owns —
// which surfaces as a cross-match and a gate timeout, far from the edit that
// caused it.
//
// caps constrains which sources may be emitted, and the constraint is
// structural: the returned timeline never contains a source caps cannot match,
// and an empty caps yields an empty timeline for every archetype. The returned
// Entries slice is OLDEST FIRST and is empty-but-non-nil rather than nil when
// there is no history, so "no history" and "bug" stay distinguishable at the
// call site.
func (g *Generator) TimelineFor(a Archetype, caps MethodSet) Timeline {
	// Drawn unconditionally, before any branch: see the slot layout above.
	j := g.drawTimelineJitter()

	tl := Timeline{Archetype: a, Entries: []TimelineEntry{}}
	plan := planFor(caps)
	if plan.empty() {
		// No role has a source this target can match, which is exactly the case an
		// empty MethodSet produces.
		return tl
	}

	var b timelineBuilder
	switch a {
	case ArchetypeMutualRegular:
		buildMutualRegular(&b, plan, j)
	case ArchetypeMutualDrifting:
		buildMutualDrifting(&b, plan, j)
	case ArchetypeOutboundHeavy:
		buildOutboundHeavy(&b, plan, j)
	case ArchetypeInboundOnly:
		buildInboundOnly(&b, plan, j)
	case ArchetypeDormant:
		buildDormant(&b, plan, j)
	case ArchetypeBurstThenQuiet:
		buildBurstThenQuiet(&b, plan, j)
	case ArchetypeNeverContacted:
		// No history at all — the empty timeline IS the archetype.
	}

	tl.Entries = b.sorted()
	return tl
}

// meetingGapBounds narrows the spacing range so that ANY draw inside it keeps
// the total series span within [spanMin, spanMax]. The span is the sum of
// count-1 independently jittered gaps, so bounding each gap by spanMin/gaps from
// below and spanMax/gaps from above bounds the sum — without that narrowing,
// "spacing 21-35d" and "span 6-9 months" are two constraints that a given
// meeting count can satisfy separately and violate together. A zero span bound
// means the archetype does not constrain the total.
func meetingGapBounds(count int, spanMin, spanMax time.Duration) (time.Duration, time.Duration) {
	lo, hi := meetingSpacingMin, meetingSpacingMax
	gaps := count - 1
	if gaps < 1 {
		return lo, hi
	}
	if spanMin > 0 {
		if need := ceilHours(ceilDivDuration(spanMin, gaps)); need > lo {
			lo = need
		}
	}
	if spanMax > 0 {
		if allow := floorHours(spanMax / time.Duration(gaps)); allow < hi {
			hi = allow
		}
	}
	if hi < lo {
		// Unreachable for the declared table — the feasibility test proves the
		// narrowed range is non-empty for every permitted meeting count — but a
		// crossed range would otherwise silently invert the bounds.
		hi = lo
	}
	return lo, hi
}

func ceilDivDuration(d time.Duration, n int) time.Duration {
	q := d / time.Duration(n)
	if q*time.Duration(n) < d {
		q++
	}
	return q
}

// seriesAges returns count ages NEWEST FIRST, starting at newest and stepping
// back by an independently jittered gap each time.
func seriesAges(count int, newest, gapLo, gapHi time.Duration, j timelineJitter) []time.Duration {
	if count < 1 {
		return nil
	}
	ages := make([]time.Duration, count)
	ages[0] = newest
	for i := 1; i < count; i++ {
		ages[i] = ages[i-1] + pickDuration(j.gap(i-1), gapLo, gapHi)
	}
	return ages
}

// addPairsBetweenMeetings places correspondence pairs INSIDE the newest gaps of
// a meeting series — pair k between meeting k and meeting k+1. Keeping them at
// the recent end is what holds the mail entries inside mailMaxSpan while the
// calendar carries the deep history.
func addPairsBetweenMeetings(b *timelineBuilder, p sourcePlan, ages []time.Duration, pairs int, j timelineJitter) {
	for k := 0; k < pairs && k+1 < len(ages); k++ {
		gap := ages[k+1] - ages[k]
		offset := pickDuration(j.pairSlot(k), mailPairInset, gap-mailPairInset)
		b.addThreadPair(p, ages[k]+offset)
	}
}

func buildMutualRegular(b *timelineBuilder, p sourcePlan, j timelineJitter) {
	meetings := pickInt(j[jitterCount], regularMeetingsMin, regularMeetingsMax)
	newest := pickDuration(j[jitterNewestAge], regularNewestMin, regularNewestMax)
	gapLo, gapHi := meetingGapBounds(meetings, regularSpanMin, regularSpanMax)

	ages := seriesAges(meetings, newest, gapLo, gapHi, j)
	for _, age := range ages {
		b.addMutualExchange(p, age)
	}
	addPairsBetweenMeetings(b, p, ages, regularPairs, j)
}

func buildMutualDrifting(b *timelineBuilder, p sourcePlan, j timelineJitter) {
	pairs := pickInt(j[jitterPairCount], 1, 2)
	meetingMax := driftingMeetingMaxOnePair
	if pairs == 2 {
		meetingMax = driftingMeetingMaxTwoPair
	}
	meetings := pickInt(j[jitterCount], driftingMeetingMin, meetingMax)
	newest := pickDuration(j[jitterNewestAge], driftingNewestMin, driftingNewestMax)
	gapLo, gapHi := meetingGapBounds(meetings, 0, 0)

	ages := seriesAges(meetings, newest, gapLo, gapHi, j)
	for _, age := range ages {
		b.addMutualExchange(p, age)
	}
	addPairsBetweenMeetings(b, p, ages, pairs, j)
}

func buildOutboundHeavy(b *timelineBuilder, p sourcePlan, j timelineJitter) {
	count := pickInt(j[jitterCount], outboundCountMin, outboundCountMax)
	newest := pickDuration(j[jitterNewestAge], outboundNewestMin, outboundNewestMax)
	b.addOneSided(p.directional(), true, seriesAges(count, newest, outboundGapMin, outboundGapMax, j))
}

func buildInboundOnly(b *timelineBuilder, p sourcePlan, j timelineJitter) {
	count := pickInt(j[jitterCount], inboundCountMin, inboundCountMax)
	newest := pickDuration(j[jitterNewestAge], inboundNewestMin, inboundNewestMax)
	b.addOneSided(p.directional(), false, seriesAges(count, newest, inboundGapMin, inboundGapMax, j))
}

func buildDormant(b *timelineBuilder, p sourcePlan, j timelineJitter) {
	meetings := pickInt(j[jitterCount], dormantMeetingMin, dormantMeetingMax)
	newest := pickDuration(j[jitterNewestAge], dormantNewestMin, dormantNewestMax)
	oldest := pickDuration(j[jitterSpan], newest+dormantSpanMin, dormantOldestMax)

	// The relationship closes with a two-way exchange on the chat source, which
	// is what makes the history genuinely two-way on more than the calendar.
	conv := b.conversation()
	key := b.pair()
	b.add(TimelineEntry{Source: p.chat, Age: newest + messagingPairGap, Outbound: true, ConversationKey: conv, PairKey: key})
	b.add(TimelineEntry{Source: p.chat, Age: newest, ConversationKey: conv, PairKey: key})

	// Meetings fill the rest of the window, evenly, back to the oldest bound. The
	// step is floored to whole hours so the oldest entry can never overshoot it.
	first := newest + messagingPairGap + dormantPairInset
	var step time.Duration
	if meetings > 1 {
		step = floorHours((oldest - first) / time.Duration(meetings-1))
	}
	for i := 0; i < meetings; i++ {
		b.addMutualExchange(p, first+time.Duration(i)*step)
	}
}

func buildBurstThenQuiet(b *timelineBuilder, p sourcePlan, j timelineJitter) {
	count := pickInt(j[jitterCount], burstCountMin, burstCountMax)
	newest := pickDuration(j[jitterNewestAge], burstNewestMin, burstNewestMax)
	width := pickDuration(j[jitterSpan], burstWidthMin, burstWidthMax)

	conv := b.conversation()
	oldest := newest + width

	// The conversation OPENS with a handful of messages inside one aggregation
	// burst window, which is the behaviour this archetype exists to exercise.
	for i := 0; i < burstOpeningMessages; i++ {
		b.add(TimelineEntry{
			Source:          p.chat,
			Age:             oldest - time.Duration(i)*burstOpeningStride,
			Outbound:        true,
			ConversationKey: conv,
		})
	}

	// It CLOSES with a promotion pair, so the whole conversation lands as mutual
	// rather than collapsing into a one-sided session.
	key := b.pair()
	pairOutbound := newest + messagingPairGap
	b.add(TimelineEntry{Source: p.chat, Age: pairOutbound, Outbound: true, ConversationKey: conv, PairKey: key})
	b.add(TimelineEntry{Source: p.chat, Age: newest, ConversationKey: conv, PairKey: key})

	// Whatever is left fills the middle, thinning out towards the close.
	filler := count - burstOpeningMessages - 2
	if filler <= 0 {
		return
	}
	openingEnd := oldest - time.Duration(burstOpeningMessages-1)*burstOpeningStride
	step := floorHours((openingEnd - pairOutbound) / time.Duration(filler+1))
	if step < time.Hour {
		step = time.Hour
	}
	for i := 1; i <= filler; i++ {
		b.add(TimelineEntry{
			Source:          p.chat,
			Age:             openingEnd - time.Duration(i)*step,
			Outbound:        true,
			ConversationKey: conv,
		})
	}
}
