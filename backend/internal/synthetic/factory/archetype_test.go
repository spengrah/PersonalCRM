package factory

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"personal-crm/backend/internal/cadence"

	"github.com/stretchr/testify/require"
)

// updateArchetypeGolden regenerates the committed per-archetype snapshot from the
// CURRENT generator output (run with `-update`). Off by default so a normal run
// asserts against the committed file and fails loudly on any drift the range
// assertions are too loose to notice.
var updateArchetypeGolden = flag.Bool("update", false, "regenerate the archetype golden snapshot")

const archetypeGoldenPath = "testdata/archetype_golden.txt"

// archetypeAnchor is the pinned anchor for these tests. Timelines are
// anchor-relative durations, so its value only matters where a test resolves an
// age into an instant.
var archetypeAnchor = time.Date(2026, time.March, 4, 9, 30, 0, 0, time.UTC)

// allArchetypes is the closed catalog, in a fixed order so table-driven tests and
// the golden snapshot are stable.
var allArchetypes = []Archetype{
	ArchetypeMutualRegular,
	ArchetypeMutualDrifting,
	ArchetypeOutboundHeavy,
	ArchetypeInboundOnly,
	ArchetypeDormant,
	ArchetypeBurstThenQuiet,
	ArchetypeNeverContacted,
}

// rangedArchetypes are the six that produce history; never-contacted is excluded
// wherever a test is about the SHAPE of a history.
var rangedArchetypes = allArchetypes[:len(allArchetypes)-1]

// The archetype parameter table, restated as LITERALS.
//
// The structural assertions below deliberately do NOT read the implementation's
// own constants. An assertion written against the constant it exists to guard
// moves with that constant, so widening a bound would silently widen its own
// test — a gate that cannot fail. These values are the documented table; editing
// one is a deliberate decision about what the seeded world looks like, and it
// should have to be made twice.
const (
	dayT = 24 * time.Hour

	wantMeetingGapMin = 21 * dayT
	wantMeetingGapMax = 35 * dayT

	wantRegularEntriesMin  = 6
	wantRegularEntriesMax  = 10
	wantRegularNewestMin   = 3 * dayT
	wantRegularNewestMax   = 14 * dayT
	wantRegularMeetingsMin = 7
	wantRegularMeetingsMax = 8
	wantRegularSpanMin     = 180 * dayT
	wantRegularSpanMax     = 270 * dayT

	wantDriftingEntriesMin = 5
	wantDriftingEntriesMax = 8
	wantDriftingNewestMin  = 70 * dayT
	wantDriftingNewestMax  = 112 * dayT

	wantOutboundEntriesMin = 3
	wantOutboundEntriesMax = 6
	wantOutboundNewestMin  = 2 * dayT
	wantOutboundNewestMax  = 20 * dayT

	wantInboundEntriesMin = 2
	wantInboundEntriesMax = 5
	wantInboundNewestMin  = 1 * dayT
	wantInboundNewestMax  = 15 * dayT

	wantDormantEntriesMin = 4
	wantDormantEntriesMax = 9
	wantDormantNewestMin  = 120 * dayT
	wantDormantOldestMax  = 240 * dayT
	// wantDormantSpanMin is the minimum depth of a dormant history: a two-way
	// relationship that ended has to have lasted long enough to be one.
	wantDormantSpanMin = 45 * dayT
	// wantDormantMeetingGapMax bounds dormant's meeting spacing, which is
	// deliberately looser than the 21-35d of the calendar-led archetypes — the
	// archetype's shape is "it ended", not "it had a rhythm".
	wantDormantMeetingGapMax = 120 * dayT

	wantBurstEntriesMin = 5
	wantBurstEntriesMax = 12
	wantBurstNewestMin  = 30 * dayT
	wantBurstNewestMax  = 90 * dayT
	wantBurstWidthMin   = 2 * dayT
	wantBurstWidthMax   = 5 * dayT
	// wantBurstWindow is the aggregation burst window the archetype exists to
	// exercise; at least two of its messages must fall inside one.
	wantBurstWindow = 2 * time.Hour

	// wantMessagingPairGap is the deterministic offset between the halves of a
	// messaging promotion pair, outbound strictly older.
	wantMessagingPairGap = 6 * time.Hour

	// wantMailMaxSpan is how far one Gmail sync reaches; mail entries past it are
	// silently dropped by the provider.
	wantMailMaxSpan = 150 * dayT
)

var (
	emailCaps    = MethodSet{Email: true}
	telegramCaps = MethodSet{Telegram: true}
	phoneCaps    = MethodSet{Phone: true}
	fullCaps     = MethodSet{Email: true, Telegram: true, Phone: true}
)

// timelineSamples draws one timeline per namespace, each from a FRESH generator,
// so every sample sits at the same draw position and differs only by namespace
// jitter. That is the shape a seed profile produces (one call per contact) and it
// gives the structural assertions a population rather than a single point.
func timelineSamples(t *testing.T, a Archetype, caps MethodSet, n int) []Timeline {
	t.Helper()
	out := make([]Timeline, 0, n)
	for i := 0; i < n; i++ {
		g := NewGeneratorAt(DefaultSeed, fmt.Sprintf("arch%02d", i), archetypeAnchor)
		out = append(out, g.TimelineFor(a, caps))
	}
	return out
}

const sampleCount = 64

// --- source families --------------------------------------------------------

func sourcesOf(tl Timeline) map[Source]int {
	out := map[Source]int{}
	for _, e := range tl.Entries {
		out[e.Source]++
	}
	return out
}

func entriesFrom(tl Timeline, src Source) []TimelineEntry {
	var out []TimelineEntry
	for _, e := range tl.Entries {
		if e.Source == src {
			out = append(out, e)
		}
	}
	return out
}

func pairGroups(tl Timeline) map[int][]TimelineEntry {
	out := map[int][]TimelineEntry{}
	for _, e := range tl.Entries {
		if e.PairKey == 0 {
			continue
		}
		out[e.PairKey] = append(out[e.PairKey], e)
	}
	return out
}

// --- method awareness -------------------------------------------------------

// An unmatchable source does not produce an interaction — it produces a stranded
// row and a gate timeout that blames the wrong thing. These three tests are what
// make that impossible by construction rather than by convention.

func TestTimelineFor_EmailCapsEmitOnlyEmailMatchableSources(t *testing.T) {
	t.Parallel()

	allowed := map[Source]bool{SourceGCal: true, SourceEmail: true, SourceGChat: true}
	for _, a := range allArchetypes {
		for _, tl := range timelineSamples(t, a, emailCaps, sampleCount) {
			for _, e := range tl.Entries {
				require.Truef(t, allowed[e.Source],
					"%s emitted %q for an email-only target (allowed: gcal/email/gchat)", a, e.Source)
			}
		}
	}
}

func TestTimelineFor_EmptyMethodSetYieldsEmptyTimeline(t *testing.T) {
	t.Parallel()

	for _, a := range allArchetypes {
		for _, tl := range timelineSamples(t, a, MethodSet{}, 8) {
			require.NotNil(t, tl.Entries, "%s: empty caps must still yield a non-nil slice", a)
			require.Emptyf(t, tl.Entries, "%s emitted %d entries for a target with no contact methods", a, len(tl.Entries))
		}
	}
}

func TestTimelineFor_TelegramCapsNeverEmitEmailOrCalendar(t *testing.T) {
	t.Parallel()

	for _, a := range rangedArchetypes {
		nonEmpty := 0
		for _, tl := range timelineSamples(t, a, telegramCaps, sampleCount) {
			for _, e := range tl.Entries {
				require.Equalf(t, SourceTelegram, e.Source,
					"%s emitted %q for a telegram-only target", a, e.Source)
			}
			if len(tl.Entries) > 0 {
				nonEmpty++
			}
		}
		// A telegram-only contact is seeded deliberately, to get telegram coverage.
		// Degrading its history to nothing would satisfy the source restriction
		// above while silently removing the coverage it exists for.
		require.Equalf(t, sampleCount, nonEmpty, "%s produced an empty timeline for a telegram-only target", a)
	}
}

func TestTimelineFor_PhoneCapsEmitOnlyMessages(t *testing.T) {
	t.Parallel()

	for _, a := range rangedArchetypes {
		for _, tl := range timelineSamples(t, a, phoneCaps, sampleCount) {
			require.NotEmptyf(t, tl.Entries, "%s produced an empty timeline for a phone-only target", a)
			for _, e := range tl.Entries {
				require.Equalf(t, SourceMessages, e.Source, "%s emitted %q for a phone-only target", a, e.Source)
			}
		}
	}
}

// TestTimelineFor_ChatRolePrefersDeliberateSources pins the resolution order. A
// contact only carries a telegram handle or a phone because someone wanted that
// source exercised; every email-bearing contact would otherwise fall to GChat and
// the other two would never be reached from a mixed-method target.
func TestTimelineFor_ChatRolePrefersDeliberateSources(t *testing.T) {
	t.Parallel()

	for _, tl := range timelineSamples(t, ArchetypeBurstThenQuiet, fullCaps, 8) {
		for _, e := range tl.Entries {
			require.Equal(t, SourceTelegram, e.Source, "a telegram-bearing target's burst must ride telegram")
		}
	}
	for _, tl := range timelineSamples(t, ArchetypeBurstThenQuiet, MethodSet{Email: true, Phone: true}, 8) {
		for _, e := range tl.Entries {
			require.Equal(t, SourceMessages, e.Source, "a phone-bearing target's burst must ride messages")
		}
	}
}

// --- ordering and basic well-formedness -------------------------------------

func TestTimelineFor_OldestFirstWithNonNegativeAges(t *testing.T) {
	t.Parallel()

	for _, caps := range []MethodSet{emailCaps, telegramCaps, phoneCaps, fullCaps} {
		for _, a := range allArchetypes {
			for _, tl := range timelineSamples(t, a, caps, 16) {
				for i, e := range tl.Entries {
					require.GreaterOrEqualf(t, e.Age, time.Duration(0), "%s entry %d has a negative age", a, i)
					if i == 0 {
						continue
					}
					require.LessOrEqualf(t, e.Age, tl.Entries[i-1].Age,
						"%s entries are not oldest-first: entry %d (%s) is older than entry %d (%s)",
						a, i, e.Age, i-1, tl.Entries[i-1].Age)
				}
			}
		}
	}
}

func TestTimelineFor_NeverContactedIsEmptyButNotNil(t *testing.T) {
	t.Parallel()

	for _, caps := range []MethodSet{emailCaps, telegramCaps, phoneCaps, fullCaps} {
		for _, tl := range timelineSamples(t, ArchetypeNeverContacted, caps, 8) {
			require.NotNil(t, tl.Entries, "a nil slice would make no-history indistinguishable from a bug")
			require.Empty(t, tl.Entries)
			require.Equal(t, ArchetypeNeverContacted, tl.Archetype)
		}
	}
}

func TestTimelineFor_ArchetypeIsEchoed(t *testing.T) {
	t.Parallel()

	for _, a := range allArchetypes {
		for _, tl := range timelineSamples(t, a, emailCaps, 4) {
			require.Equal(t, a, tl.Archetype)
		}
	}
}

// --- promotion pairs --------------------------------------------------------

// TestTimelineFor_PairGroupsAreWellFormed pins the shape the batch replay
// partition is defined on: exactly two members with differing directions, both
// inside one conversation, outbound first in slice order. A reversed group would
// silently fail to promote at replay time and land two one-sided interactions.
func TestTimelineFor_PairGroupsAreWellFormed(t *testing.T) {
	t.Parallel()

	for _, caps := range []MethodSet{emailCaps, telegramCaps, phoneCaps, fullCaps} {
		for _, a := range allArchetypes {
			for _, tl := range timelineSamples(t, a, caps, 16) {
				groups := pairGroups(tl)
				for key, members := range groups {
					require.Lenf(t, members, 2, "%s PairKey %d has %d members", a, key, len(members))
					require.NotEqualf(t, members[0].Outbound, members[1].Outbound,
						"%s PairKey %d members share a direction", a, key)
					require.Truef(t, members[0].Outbound, "%s PairKey %d does not put its outbound half first", a, key)
					require.NotZerof(t, members[0].ConversationKey, "%s PairKey %d is not inside a conversation", a, key)
					require.Equalf(t, members[0].ConversationKey, members[1].ConversationKey,
						"%s PairKey %d spans two conversations", a, key)
				}
			}
		}
	}
}

// TestTimelineFor_ThreadPairsShareOneInstant pins the Gmail timing rule. Gmail's
// aggregation key includes a LOCAL day, so the assertion that matters is about
// the resolved local dates, not about a gap bound.
func TestTimelineFor_ThreadPairsShareOneInstant(t *testing.T) {
	t.Parallel()

	checked := 0
	for _, a := range rangedArchetypes {
		for _, tl := range timelineSamples(t, a, emailCaps, sampleCount) {
			for key, members := range pairGroups(tl) {
				if members[0].Source != SourceEmail {
					continue
				}
				require.Equalf(t, members[0].Age, members[1].Age,
					"%s mail PairKey %d does not share an instant (%s vs %s)", a, key, members[0].Age, members[1].Age)
				checked++
			}
		}
	}
	require.Positive(t, checked, "no mail promotion pairs were exercised")
}

// TestTimelineFor_ThreadPairsLandOnOneLocalDay resolves the pair's ages into
// instants across a 48-hour anchor sweep and asserts the ACTUAL local dates are
// equal — proving the same-local-day property directly rather than inferring it.
// The counterfactual half is what gives the test teeth: a twelve-hour gap, which
// "under 24 hours" would have permitted, straddles local midnight for some
// anchors in the same sweep.
func TestTimelineFor_ThreadPairsLandOnOneLocalDay(t *testing.T) {
	t.Parallel()

	// The replay layer dates a mail payload at anchor - providerLag - age. The lag
	// is constant across a pair, so only the ages matter; it is modelled here so
	// the resolved instants are the ones the pipeline would actually see.
	const providerLag = 2 * time.Hour
	resolve := func(anchor time.Time, age time.Duration) time.Time {
		return anchor.Add(-providerLag - age).Local()
	}

	straddled := 0
	pairsChecked := 0
	for hour := 0; hour < 48; hour++ {
		anchor := time.Date(2026, time.March, 4, 0, 0, 0, 0, time.UTC).Add(time.Duration(hour) * time.Hour)
		for i := 0; i < 12; i++ {
			g := NewGeneratorAt(DefaultSeed, fmt.Sprintf("localday%02d", i), anchor)
			tl := g.TimelineFor(ArchetypeMutualRegular, emailCaps)
			for key, members := range pairGroups(tl) {
				if members[0].Source != SourceEmail {
					continue
				}
				gotY, gotM, gotD := resolve(anchor, members[0].Age).Date()
				wantY, wantM, wantD := resolve(anchor, members[1].Age).Date()
				require.Equalf(t, [3]int{wantY, int(wantM), wantD}, [3]int{gotY, int(gotM), gotD},
					"mail PairKey %d resolved onto two local days at anchor %s", key, anchor)
				pairsChecked++

				// Counterfactual: the rejected "small gap" formulation.
				aY, aM, aD := resolve(anchor, members[0].Age).Date()
				bY, bM, bD := resolve(anchor, members[0].Age-12*time.Hour).Date()
				if [3]int{aY, int(aM), aD} != [3]int{bY, int(bM), bD} {
					straddled++
				}
			}
		}
	}
	require.Positive(t, pairsChecked, "no mail pairs were resolved")
	require.Positivef(t, straddled,
		"the 12h-gap counterfactual never straddled local midnight across the sweep — this test cannot detect the failure it exists for")
}

// TestTimelineFor_MessagingPairsUseADeterministicGap pins the other half of the
// timing rule. Messaging sources order eligible rows only by sent_at and the
// engine's sort is stable relative to an unspecified DB order, so an equal-aged
// pair could nondeterministically become one mutual or two one-sided sessions.
func TestTimelineFor_MessagingPairsUseADeterministicGap(t *testing.T) {
	t.Parallel()

	messaging := map[Source]bool{SourceGChat: true, SourceTelegram: true, SourceMessages: true}
	checked := 0
	for _, caps := range []MethodSet{emailCaps, telegramCaps, phoneCaps, fullCaps} {
		for _, a := range rangedArchetypes {
			for _, tl := range timelineSamples(t, a, caps, 16) {
				for key, members := range pairGroups(tl) {
					if !messaging[members[0].Source] {
						continue
					}
					require.NotEqualf(t, members[0].Age, members[1].Age,
						"%s messaging PairKey %d is equal-aged, which leaves its order to a DB tie-break", a, key)
					require.Equalf(t, time.Duration(wantMessagingPairGap), members[0].Age-members[1].Age,
						"%s messaging PairKey %d gap is %s, want %s", a, key, members[0].Age-members[1].Age, time.Duration(wantMessagingPairGap))
					require.Truef(t, members[0].Outbound, "%s messaging PairKey %d: the older half must be the outbound one", a, key)
					checked++
				}
			}
		}
	}
	require.Positive(t, checked, "no messaging promotion pairs were exercised")
}

// --- per-archetype structure ------------------------------------------------

func TestTimelineFor_MutualRegularShape(t *testing.T) {
	t.Parallel()

	pairCounts := map[int]bool{}
	for _, tl := range timelineSamples(t, ArchetypeMutualRegular, emailCaps, sampleCount) {
		require.GreaterOrEqual(t, len(tl.Entries), wantRegularEntriesMin)
		require.LessOrEqual(t, len(tl.Entries), wantRegularEntriesMax)
		require.GreaterOrEqual(t, tl.Entries[len(tl.Entries)-1].Age, time.Duration(wantRegularNewestMin))
		require.LessOrEqual(t, tl.Entries[len(tl.Entries)-1].Age, time.Duration(wantRegularNewestMax))

		meetings := entriesFrom(tl, SourceGCal)
		require.GreaterOrEqual(t, len(meetings), wantRegularMeetingsMin)
		require.LessOrEqual(t, len(meetings), wantRegularMeetingsMax)

		// Consecutive meeting spacing, and the total span it sums to.
		for i := 1; i < len(meetings); i++ {
			gap := meetings[i-1].Age - meetings[i].Age
			require.GreaterOrEqualf(t, gap, time.Duration(wantMeetingGapMin), "meeting gap %s below the spacing floor", gap)
			require.LessOrEqualf(t, gap, time.Duration(wantMeetingGapMax), "meeting gap %s above the spacing ceiling", gap)
		}
		span := meetings[0].Age - meetings[len(meetings)-1].Age
		require.GreaterOrEqualf(t, span, time.Duration(wantRegularSpanMin), "calendar span %s is under six months", span)
		require.LessOrEqualf(t, span, time.Duration(wantRegularSpanMax), "calendar span %s is over nine months", span)

		groups := pairGroups(tl)
		require.GreaterOrEqual(t, len(groups), 1)
		require.LessOrEqual(t, len(groups), 2)
		pairCounts[len(groups)] = true
	}
	require.Equal(t, map[int]bool{1: true}, pairCounts,
		"mutual-regular carries exactly one correspondence pair — two would need more meetings than its entry budget allows")
}

func TestTimelineFor_MutualDriftingShape(t *testing.T) {
	t.Parallel()

	pairCounts := map[int]bool{}
	for _, tl := range timelineSamples(t, ArchetypeMutualDrifting, emailCaps, sampleCount) {
		require.GreaterOrEqual(t, len(tl.Entries), wantDriftingEntriesMin)
		require.LessOrEqual(t, len(tl.Entries), wantDriftingEntriesMax)

		newest := tl.Entries[len(tl.Entries)-1].Age
		require.GreaterOrEqualf(t, newest, time.Duration(wantDriftingNewestMin), "newest entry %s breaks the drifting overdue floor", newest)
		require.LessOrEqualf(t, newest, time.Duration(wantDriftingNewestMax), "newest entry %s is older than sixteen weeks", newest)

		meetings := entriesFrom(tl, SourceGCal)
		require.GreaterOrEqual(t, len(meetings), 3, "a series needs enough meetings for its spacing to mean anything")
		for i := 1; i < len(meetings); i++ {
			gap := meetings[i-1].Age - meetings[i].Age
			require.GreaterOrEqual(t, gap, time.Duration(wantMeetingGapMin))
			require.LessOrEqual(t, gap, time.Duration(wantMeetingGapMax))
		}

		groups := pairGroups(tl)
		require.GreaterOrEqual(t, len(groups), 1)
		require.LessOrEqual(t, len(groups), 2)
		pairCounts[len(groups)] = true
	}
	require.Equal(t, map[int]bool{1: true, 2: true}, pairCounts,
		"mutual-drifting must exercise both one and two correspondence pairs")
}

func TestTimelineFor_OneSidedArchetypesCannotPromote(t *testing.T) {
	t.Parallel()

	cases := []struct {
		archetype Archetype
		outbound  bool
		countMin  int
		countMax  int
		newestMin time.Duration
		newestMax time.Duration
	}{
		{ArchetypeOutboundHeavy, true, wantOutboundEntriesMin, wantOutboundEntriesMax, wantOutboundNewestMin, wantOutboundNewestMax},
		{ArchetypeInboundOnly, false, wantInboundEntriesMin, wantInboundEntriesMax, wantInboundNewestMin, wantInboundNewestMax},
	}
	for _, tc := range cases {
		for _, tl := range timelineSamples(t, tc.archetype, emailCaps, sampleCount) {
			require.GreaterOrEqual(t, len(tl.Entries), tc.countMin)
			require.LessOrEqual(t, len(tl.Entries), tc.countMax)

			newest := tl.Entries[len(tl.Entries)-1].Age
			require.GreaterOrEqual(t, newest, tc.newestMin)
			require.LessOrEqual(t, newest, tc.newestMax)

			for i, e := range tl.Entries {
				require.Equalf(t, tc.outbound, e.Outbound, "%s entry %d has the wrong direction", tc.archetype, i)
				require.Zerof(t, e.PairKey, "%s entry %d is paired — it must not be able to promote", tc.archetype, i)
				require.Zerof(t, e.ConversationKey, "%s entry %d shares a conversation — it must not be able to promote", tc.archetype, i)
			}
		}
	}
}

func TestTimelineFor_DormantShape(t *testing.T) {
	t.Parallel()

	for _, tl := range timelineSamples(t, ArchetypeDormant, emailCaps, sampleCount) {
		require.GreaterOrEqual(t, len(tl.Entries), wantDormantEntriesMin)
		require.LessOrEqual(t, len(tl.Entries), wantDormantEntriesMax)

		newest := tl.Entries[len(tl.Entries)-1].Age
		require.GreaterOrEqualf(t, newest, time.Duration(wantDormantNewestMin), "newest entry %s breaks the dormant overdue floor", newest)
		require.LessOrEqualf(t, tl.Entries[0].Age, time.Duration(wantDormantOldestMax),
			"oldest entry %s predates the dormant window — dormant, not prehistoric", tl.Entries[0].Age)

		span := tl.Entries[0].Age - newest
		require.GreaterOrEqualf(t, span, time.Duration(wantDormantSpanMin),
			"dormant history is only %s deep — too shallow to be a relationship that ended", span)

		counts := sourcesOf(tl)
		require.Positivef(t, counts[SourceGCal], "dormant needs at least one meeting")
		require.Positivef(t, counts[SourceGChat], "dormant needs at least one chat message")

		// Dormant's meetings are stepped across the window rather than held to the
		// 21-35d rhythm of the live archetypes, so the gap is bounded by the window
		// itself. Unbounded is the thing to avoid, not wide.
		meetings := entriesFrom(tl, SourceGCal)
		for i := 1; i < len(meetings); i++ {
			gap := meetings[i-1].Age - meetings[i].Age
			require.Positivef(t, gap, "dormant meeting %d does not advance", i)
			require.LessOrEqualf(t, gap, time.Duration(wantDormantMeetingGapMax),
				"dormant meeting gap %s exceeds the dormant window", gap)
		}
	}
}

func TestTimelineFor_BurstThenQuietShape(t *testing.T) {
	t.Parallel()

	for _, tl := range timelineSamples(t, ArchetypeBurstThenQuiet, emailCaps, sampleCount) {
		require.GreaterOrEqual(t, len(tl.Entries), wantBurstEntriesMin)
		require.LessOrEqual(t, len(tl.Entries), wantBurstEntriesMax)

		newest := tl.Entries[len(tl.Entries)-1].Age
		require.GreaterOrEqual(t, newest, time.Duration(wantBurstNewestMin))
		require.LessOrEqual(t, newest, time.Duration(wantBurstNewestMax))

		// ONE conversation, and the entry set IS the cluster — there is no entry
		// outside it, which is what "then quiet" means.
		conv := tl.Entries[0].ConversationKey
		require.NotZero(t, conv, "a zero key would give every message its own space: unrelated interactions, no burst")
		for i, e := range tl.Entries {
			require.Equalf(t, conv, e.ConversationKey, "entry %d left the burst's conversation", i)
			require.Equal(t, SourceGChat, e.Source)
		}

		width := tl.Entries[0].Age - newest
		require.GreaterOrEqualf(t, width, time.Duration(wantBurstWidthMin), "cluster width %s is too tight", width)
		require.LessOrEqualf(t, width, time.Duration(wantBurstWidthMax), "cluster width %s is not a cluster", width)

		// At least two messages inside ONE aggregation burst window. A cluster
		// spread evenly over days would land N separate sessions instead.
		require.GreaterOrEqual(t, maxWithinWindow(tl.Entries, wantBurstWindow), 2,
			"no two messages fall inside a single 2h burst window")

		groups := pairGroups(tl)
		require.Len(t, groups, 1, "the burst promotes through exactly one pair")
		for _, members := range groups {
			require.True(t, members[0].Outbound)
			require.False(t, members[1].Outbound)
		}

		// The conversation CLOSES with that pair: its two entries are the newest
		// two in the timeline. Without this, a filler entry drifting newer than the
		// pair would leave the archetype still reporting one well-formed pair and a
		// legal cluster width, having quietly stopped being "burst then quiet".
		last := tl.Entries[len(tl.Entries)-1]
		secondLast := tl.Entries[len(tl.Entries)-2]
		require.NotZero(t, last.PairKey, "the newest entry is not part of the closing pair")
		require.Equalf(t, last.PairKey, secondLast.PairKey,
			"the two newest entries are not the closing pair (%d, %d)", secondLast.PairKey, last.PairKey)

		directions := map[bool]int{}
		for _, e := range tl.Entries {
			directions[e.Outbound]++
		}
		require.Positive(t, directions[true], "the burst must be mixed-direction")
		require.Positive(t, directions[false], "the burst must be mixed-direction")
	}
}

// maxWithinWindow returns the largest number of entries whose ages all fall
// inside one window of the given length.
func maxWithinWindow(entries []TimelineEntry, window time.Duration) int {
	ages := make([]time.Duration, len(entries))
	for i, e := range entries {
		ages[i] = e.Age
	}
	sort.Slice(ages, func(i, k int) bool { return ages[i] < ages[k] })
	best, start := 0, 0
	for end := range ages {
		for ages[end]-ages[start] > window {
			start++
		}
		if n := end - start + 1; n > best {
			best = n
		}
	}
	return best
}

// TestTimelineFor_MailEntriesStayInsideProviderReach pins the span bound FOR ONE
// TIMELINE. The Gmail provider reaches a bounded window forward from one sync's
// backfill point and SILENTLY drops anything past it — a missing row and a gate
// timeout naming the wrong cause — which is why deep history is carried by the
// calendar.
//
// The scope matters and is easy to misread: the replay layer's limit applies to
// a whole BATCH, over every instant in it across all contacts, whereas what a
// timeline can promise is per contact. Pooled across archetypes, mail spans up
// to 179d against a 150d limit, so a caller batching mail across contacts must
// bucket by age — see addPairsBetweenMeetings for that arithmetic.
func TestTimelineFor_MailEntriesStayInsideProviderReach(t *testing.T) {
	t.Parallel()

	checked := 0
	for _, a := range allArchetypes {
		for _, tl := range timelineSamples(t, a, emailCaps, sampleCount) {
			mail := entriesFrom(tl, SourceEmail)
			if len(mail) == 0 {
				continue
			}
			span := mail[0].Age - mail[len(mail)-1].Age
			require.LessOrEqualf(t, span, time.Duration(wantMailMaxSpan), "%s mail entries span %s, past one sync's reach", a, span)
			checked++
		}
	}
	require.Positive(t, checked, "no mail entries were exercised")
}

// TestTimelineFor_MessagingConversationAgesAreDistinct pins, for EVERY entry of a
// shared messaging conversation, the property the pair rule only pins for the
// two members of a pair: no two ages may coincide. Messaging sources order
// eligible rows by sent_at alone and the aggregation engine's sort is stable
// relative to an unspecified DB order, so two messages at the same instant in
// one conversation leave their order to a tie-break the database is free to
// resolve either way — which is the nondeterminism this whole timing design
// exists to remove. burst-then-quiet puts five to twelve entries in one
// conversation, so the guarantee has to hold beyond the pair.
//
// Today it holds by arithmetic (opening stride, filler step, pair placement).
// This makes it hold by assertion, so a future bound change that collided two
// ages fails here instead of shipping.
func TestTimelineFor_MessagingConversationAgesAreDistinct(t *testing.T) {
	t.Parallel()

	messaging := map[Source]bool{SourceGChat: true, SourceTelegram: true, SourceMessages: true}
	checked := 0
	for _, caps := range []MethodSet{emailCaps, telegramCaps, phoneCaps, fullCaps} {
		for _, a := range allArchetypes {
			for _, tl := range timelineSamples(t, a, caps, 16) {
				seen := map[int]map[time.Duration]bool{}
				for i, e := range tl.Entries {
					if e.ConversationKey == 0 || !messaging[e.Source] {
						continue
					}
					ages, ok := seen[e.ConversationKey]
					if !ok {
						ages = map[time.Duration]bool{}
						seen[e.ConversationKey] = ages
					}
					require.Falsef(t, ages[e.Age],
						"%s entry %d repeats age %s inside conversation %d — the order of the two would fall to a DB tie-break",
						a, i, e.Age, e.ConversationKey)
					ages[e.Age] = true
					checked++
				}
			}
		}
	}
	require.Positive(t, checked, "no shared messaging conversations were exercised")
}

// TestTimelineFor_ChatOnlyTargetsHoldOneConversation pins the other half of that
// rule. A contact has ONE private chat with you, so every chat exchange a
// timeline emits for a calendar-less target belongs to the same conversation;
// two would ask the replay layer to mint two separate private chats with the
// same person.
func TestTimelineFor_ChatOnlyTargetsHoldOneConversation(t *testing.T) {
	t.Parallel()

	// Archetypes whose history is two-way, and therefore chat-carried when the
	// target has no calendar. The one-sided archetypes deliberately use distinct
	// conversations so nothing can bridge.
	twoWay := []Archetype{ArchetypeMutualRegular, ArchetypeMutualDrifting, ArchetypeDormant, ArchetypeBurstThenQuiet}
	for _, caps := range []MethodSet{telegramCaps, phoneCaps} {
		for _, a := range twoWay {
			for _, tl := range timelineSamples(t, a, caps, 16) {
				keys := map[int]bool{}
				for _, e := range tl.Entries {
					keys[e.ConversationKey] = true
				}
				require.Lenf(t, keys, 1, "%s gave a chat-only target %d conversations", a, len(keys))
				require.NotContains(t, keys, 0, "%s left a chat exchange outside a conversation", a)
			}
		}
	}
}

// TestTimelineFor_CalendarEntriesAreNeverGrouped pins the seam property that
// makes the batch layer's calendar item type safe without a pair field: a
// matched calendar event is always mutual, so it is never half of a promotion
// pair and never part of a conversation. The grouping guards elsewhere catch a
// violation only incidentally — a construction that produced a calendar pair
// with genuinely differing directions would slip past all of them.
func TestTimelineFor_CalendarEntriesAreNeverGrouped(t *testing.T) {
	t.Parallel()

	checked := 0
	for _, a := range allArchetypes {
		for _, tl := range timelineSamples(t, a, emailCaps, sampleCount) {
			for i, e := range tl.Entries {
				if e.Source != SourceGCal {
					continue
				}
				require.Zerof(t, e.PairKey, "%s calendar entry %d carries a PairKey", a, i)
				require.Zerof(t, e.ConversationKey, "%s calendar entry %d carries a ConversationKey", a, i)
				checked++
			}
		}
	}
	require.Positive(t, checked, "no calendar entries were exercised")
}

// TestTimelineFor_OneSidedHistoryPrefersMail pins the other half of the source
// resolution. One-sided history rides the threaded mail source whenever the
// target has one — even for a contact whose two-way conversation rides telegram
// — because a thread is the better carrier for correspondence that never got a
// reply. Only the chat half of the resolution was pinned before.
func TestTimelineFor_OneSidedHistoryPrefersMail(t *testing.T) {
	t.Parallel()

	oneSided := []Archetype{ArchetypeOutboundHeavy, ArchetypeInboundOnly}
	cases := []struct {
		caps MethodSet
		want Source
	}{
		{fullCaps, SourceEmail},
		{MethodSet{Email: true, Telegram: true}, SourceEmail},
		{MethodSet{Email: true, Phone: true}, SourceEmail},
		{telegramCaps, SourceTelegram},
		{phoneCaps, SourceMessages},
	}
	for _, tc := range cases {
		for _, a := range oneSided {
			for _, tl := range timelineSamples(t, a, tc.caps, 8) {
				require.NotEmpty(t, tl.Entries)
				for _, e := range tl.Entries {
					require.Equalf(t, tc.want, e.Source, "%s at caps %+v rode %q", a, tc.caps, e.Source)
				}
			}
		}
	}
}

// --- determinism and variation ----------------------------------------------

func TestTimelineFor_DeterministicForOneSeedNamespaceAndPosition(t *testing.T) {
	t.Parallel()

	for _, a := range allArchetypes {
		gA := NewGeneratorAt(DefaultSeed, "determinism", archetypeAnchor)
		gB := NewGeneratorAt(DefaultSeed, "determinism", archetypeAnchor)
		require.Equal(t, gA.TimelineFor(a, emailCaps), gB.TimelineFor(a, emailCaps), "%s is not reproducible", a)

		// Same generators, one draw further along: still in lockstep with each
		// other, which is the property a seed profile relies on.
		require.Equal(t, gA.TimelineFor(a, emailCaps), gB.TimelineFor(a, emailCaps), "%s drifts at the second draw position", a)
	}
}

func TestTimelineFor_DistinctNamespacesDrawDistinctJitter(t *testing.T) {
	t.Parallel()

	for _, a := range rangedArchetypes {
		gA := NewGeneratorAt(DefaultSeed, "nsA", archetypeAnchor)
		gB := NewGeneratorAt(DefaultSeed, "nsB", archetypeAnchor)
		require.NotEqual(t, gA.TimelineFor(a, emailCaps), gB.TimelineFor(a, emailCaps),
			"%s produced identical timelines for two namespaces", a)
	}
}

// TestTimelineFor_MultiSampleVariation is what makes "enough samples at varied
// points" mechanically checkable at this layer. One sample cannot demonstrate
// jitter by construction, so the assertion is that two draws from the SAME
// generator differ in a way a reader would notice: entry count or newest age.
func TestTimelineFor_MultiSampleVariation(t *testing.T) {
	t.Parallel()

	for _, a := range rangedArchetypes {
		g := NewGeneratorAt(DefaultSeed, "variation", archetypeAnchor)
		first := g.TimelineFor(a, emailCaps)
		second := g.TimelineFor(a, emailCaps)

		countDiffers := len(first.Entries) != len(second.Entries)
		newestDiffers := first.Entries[len(first.Entries)-1].Age != second.Entries[len(second.Entries)-1].Age
		require.Truef(t, countDiffers || newestDiffers,
			"%s: two draws produced the same entry count (%d) and the same newest age (%s) — the jitter is not observable",
			a, len(first.Entries), first.Entries[len(first.Entries)-1].Age)
	}
}

// --- overdue floors at PRODUCTION cadence durations -------------------------

// Cadence durations are environment-dependent: under CRM_ENV=test/testing annual
// is two hours and every archetype is trivially overdue, so a floor asserted
// under the ambient integration environment proves nothing. These tests pin the
// production table in BOTH directions — the cadences the archetype stays overdue
// on, and the ones where it silently becomes contacted-but-not-overdue.
func TestTimelineFor_OverdueFloorsUnderProductionCadences(t *testing.T) {
	t.Setenv("CRM_ENV", "production")

	cases := []struct {
		archetype    Archetype
		floor        time.Duration
		compatible   []cadence.CadenceType
		incompatible []cadence.CadenceType
	}{
		{
			archetype:    ArchetypeMutualDrifting,
			floor:        wantDriftingNewestMin,
			compatible:   []cadence.CadenceType{cadence.CadenceWeekly, cadence.CadenceBiweekly, cadence.CadenceMonthly},
			incompatible: []cadence.CadenceType{cadence.CadenceQuarterly, cadence.CadenceBiannual, cadence.CadenceAnnual},
		},
		{
			archetype:    ArchetypeDormant,
			floor:        wantDormantNewestMin,
			compatible:   []cadence.CadenceType{cadence.CadenceWeekly, cadence.CadenceBiweekly, cadence.CadenceMonthly, cadence.CadenceQuarterly},
			incompatible: []cadence.CadenceType{cadence.CadenceBiannual, cadence.CadenceAnnual},
		},
	}

	for _, tc := range cases {
		// The table is a statement about the FLOOR, because the floor is the only
		// thing the archetype guarantees: a cadence is compatible exactly when
		// every legal draw outlives its period. Asserting it per-sample would let
		// a lucky draw certify a cadence the archetype cannot actually hold.
		for _, ct := range tc.compatible {
			period := cadence.GetCadenceDuration(ct)
			require.Greaterf(t, tc.floor, period,
				"%s on %s: the %s floor does not clear the %s cadence period, so a floor-hugging draw would land contacted and NOT overdue",
				tc.archetype, ct, tc.floor, period)
		}
		for _, ct := range tc.incompatible {
			period := cadence.GetCadenceDuration(ct)
			require.LessOrEqualf(t, tc.floor, period,
				"%s on %s: the %s floor already clears the %s cadence period, so this cadence belongs in the compatible column",
				tc.archetype, ct, tc.floor, period)
		}

		for _, tl := range timelineSamples(t, tc.archetype, emailCaps, sampleCount) {
			newest := tl.Entries[len(tl.Entries)-1].Age
			require.GreaterOrEqualf(t, newest, tc.floor, "%s newest entry %s breaks its floor", tc.archetype, newest)

			// Every sample therefore stays overdue on every compatible cadence.
			for _, ct := range tc.compatible {
				period := cadence.GetCadenceDuration(ct)
				require.Greaterf(t, newest, period,
					"%s on %s: newest entry %s is inside the %s cadence period, so the replayed mutual would clear the overdue state",
					tc.archetype, ct, newest, period)
			}
		}
	}
}

// TestProductionCadenceDurationsAreRead guards the test above: if the env pin
// silently stopped working, every floor would pass against test-mode durations
// (annual = 2h) while proving nothing.
func TestProductionCadenceDurationsAreRead(t *testing.T) {
	t.Setenv("CRM_ENV", "production")

	require.Equal(t, time.Duration(7*dayT), cadence.GetCadenceDuration(cadence.CadenceWeekly))
	require.Equal(t, time.Duration(90*dayT), cadence.GetCadenceDuration(cadence.CadenceQuarterly))
	require.Equal(t, time.Duration(365*dayT), cadence.GetCadenceDuration(cadence.CadenceAnnual))
}

// --- parameter-table feasibility --------------------------------------------

// TestMeetingGapBoundsAreFeasible proves the narrowed spacing range is non-empty
// for every permitted meeting count AND that any draw inside it keeps the total
// span within bounds. Entry count, spacing and span are not independent — the
// span is the sum of count-1 gaps — so a count whose feasible range is empty
// would make the archetype's own documented bounds unsatisfiable.
func TestMeetingGapBoundsAreFeasible(t *testing.T) {
	t.Parallel()

	for count := wantRegularMeetingsMin; count <= wantRegularMeetingsMax; count++ {
		lo, hi := meetingGapBounds(count, wantRegularSpanMin, wantRegularSpanMax)
		require.LessOrEqualf(t, lo, hi, "meeting count %d has an empty feasible spacing range", count)
		require.GreaterOrEqualf(t, lo, time.Duration(wantMeetingGapMin), "count %d floor %s is under the spacing floor", count, lo)
		require.LessOrEqualf(t, hi, time.Duration(wantMeetingGapMax), "count %d ceiling %s is over the spacing ceiling", count, hi)

		gaps := time.Duration(count - 1)
		require.GreaterOrEqualf(t, gaps*lo, time.Duration(wantRegularSpanMin), "count %d at minimum spacing spans %s, under the floor", count, gaps*lo)
		require.LessOrEqualf(t, gaps*hi, time.Duration(wantRegularSpanMax), "count %d at maximum spacing spans %s, over the ceiling", count, gaps*hi)
	}
}

// --- golden snapshot --------------------------------------------------------

// The range assertions above describe intent; a generator returning different-but
// -legal garbage would still satisfy them. The snapshot is the drift signal that
// does not.
func TestTimelineFor_GoldenSnapshot(t *testing.T) {
	live := buildArchetypeGolden()

	if *updateArchetypeGolden {
		require.NoError(t, os.MkdirAll(filepath.Dir(archetypeGoldenPath), 0o755))
		require.NoError(t, os.WriteFile(archetypeGoldenPath, []byte(strings.Join(live, "\n")+"\n"), 0o644))
		t.Logf("wrote %s (%d lines)", archetypeGoldenPath, len(live))
		return
	}

	wantBytes, err := os.ReadFile(archetypeGoldenPath)
	require.NoError(t, err, "missing snapshot — regenerate with `go test ./internal/synthetic/factory -run TestTimelineFor_GoldenSnapshot -update`")
	want := strings.Split(strings.TrimRight(string(wantBytes), "\n"), "\n")
	require.Equal(t, want, live,
		"archetype timelines drifted from the committed snapshot; if the change is deliberate, regenerate with -update")
}

// buildArchetypeGolden renders every archetype under the two palettes that
// matter — the email-only one the frozen catalog uses, and a telegram-only one
// that exercises the no-calendar degradation — each from a fresh generator at a
// pinned (seed, namespace, anchor).
func buildArchetypeGolden() []string {
	palettes := []struct {
		label string
		caps  MethodSet
	}{
		{"email", emailCaps},
		{"telegram", telegramCaps},
	}
	var lines []string
	for _, p := range palettes {
		for _, a := range allArchetypes {
			g := NewGeneratorAt(DefaultSeed, "archetype-golden", archetypeAnchor)
			tl := g.TimelineFor(a, p.caps)
			lines = append(lines, fmt.Sprintf("%s/%s entries=%d", p.label, a, len(tl.Entries)))
			for i, e := range tl.Entries {
				lines = append(lines, fmt.Sprintf("%s/%s [%02d] source=%s age=%s outbound=%t conv=%d pair=%d",
					p.label, a, i, e.Source, e.Age, e.Outbound, e.ConversationKey, e.PairKey))
			}
		}
	}
	return lines
}
