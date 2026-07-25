package tests

import (
	"context"
	"fmt"
	"sort"
	"testing"
	"time"

	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/synthetic"
	"personal-crm/backend/internal/synthetic/factory"
	"personal-crm/backend/tests/testsupport"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// This file covers the seam nothing else reaches: archetype assignment →
// timeline → payload construction → batch adapter → matching and aggregation →
// the interaction rows and cadence columns a user actually sees. The batch tests
// a package below drive HAND-AUTHORED batches; the generator tests stop at
// abstract timelines. Only a real profile run crosses all of it at once.
//
// It uses newIsolatedRiverTestDB for the same reason the batch suite does: the
// harness starts a live River client, and namespace scoping does not isolate
// river_job CONSUMPTION. The clone additionally makes the calendar drain
// meaningful — its past-event read takes a DB-wide LIMIT with the namespace
// filter applied after it, so on a shared database another namespace owning the
// oldest page is deterministic starvation rather than a flake.
//
// It does not call t.Parallel(): one clone costs about seven connections, the
// subtests share it, and running them serially keeps concurrent clones near one.

// archetypeExpectation is what one archetype's landed rows must look like: the
// sources they may come from, the directions they may carry, and — where the
// archetype earns exactly one promotion — how many must be mutual.
type archetypeExpectation struct {
	Sources      []string
	Directions   []string
	MutualRows   int // -1 when every row is mutual and the count is not fixed
	CadenceShape string
}

const (
	// cadenceShapeAllFour — a contacting archetype whose newest row is MUTUAL, so
	// the direction→column table writes all four cadence columns together.
	cadenceShapeAllFour = "all-four"
	// cadenceShapeInbound — inbound writes last_contacted / last_interaction_at /
	// last_response_at but NOT last_outreach_at: it is the response, not the
	// outreach.
	cadenceShapeInbound = "inbound"
	// cadenceShapeOutreachOnly — outbound writes last_outreach_at and nothing else.
	cadenceShapeOutreachOnly = "outreach-only"
	// cadenceShapeNone — no history, so no column moves.
	cadenceShapeNone = "none"
)

// archetypeExpectations is the per-archetype contract the pipeline must produce.
// It is the direction→column table observed end to end rather than restated:
// nothing here writes a contact column, so every one of these values is the
// result of a source payload having been classified.
var archetypeExpectations = map[factory.Archetype]archetypeExpectation{
	factory.ArchetypeMutualRegular: {
		Sources:      []string{repository.InteractionSourceGCal, repository.InteractionSourceEmail},
		Directions:   []string{repository.InteractionDirectionMutual},
		MutualRows:   -1,
		CadenceShape: cadenceShapeAllFour,
	},
	factory.ArchetypeMutualDrifting: {
		Sources:      []string{repository.InteractionSourceGCal, repository.InteractionSourceEmail},
		Directions:   []string{repository.InteractionDirectionMutual},
		MutualRows:   -1,
		CadenceShape: cadenceShapeAllFour,
	},
	factory.ArchetypeDormant: {
		Sources:      []string{repository.InteractionSourceGCal, repository.InteractionSourceGChat},
		Directions:   []string{repository.InteractionDirectionMutual},
		MutualRows:   -1,
		CadenceShape: cadenceShapeAllFour,
	},
	factory.ArchetypeOutboundHeavy: {
		Sources:      []string{repository.InteractionSourceEmail},
		Directions:   []string{repository.InteractionDirectionOutbound},
		MutualRows:   0,
		CadenceShape: cadenceShapeOutreachOnly,
	},
	factory.ArchetypeInboundOnly: {
		Sources:      []string{repository.InteractionSourceEmail},
		Directions:   []string{repository.InteractionDirectionInbound},
		MutualRows:   0,
		CadenceShape: cadenceShapeInbound,
	},
	factory.ArchetypeBurstThenQuiet: {
		Sources:      []string{repository.InteractionSourceGChat},
		Directions:   []string{repository.InteractionDirectionOutbound, repository.InteractionDirectionMutual},
		MutualRows:   1,
		CadenceShape: cadenceShapeAllFour,
	},
	factory.ArchetypeNeverContacted: {
		CadenceShape: cadenceShapeNone,
	},
}

// TestSyntheticProfile_ArchetypeReplayOutcomes runs both catalog profiles and
// asserts the whole archetype seam from the database side.
func TestSyntheticProfile_ArchetypeReplayOutcomes(t *testing.T) {
	testsupport.RequireLongTests(t)
	ctx := context.Background()
	database, _ := newIsolatedRiverTestDB(t, ctx)

	t.Run("dev", func(t *testing.T) {
		params, err := synthetic.ProfileParams(synthetic.ProfileDev)
		require.NoError(t, err)
		params.Namespace = syntheticNS(t)
		assertArchetypeReplayOutcomes(t, ctx, database, params)
	})

	t.Run("prod_shaped", func(t *testing.T) {
		params, err := synthetic.ProfileParams(synthetic.ProfileProdShaped)
		require.NoError(t, err)
		params.Namespace = syntheticNS(t)
		// The CI-safe knobs the prod-shaped coverage test uses, at the SMALLEST
		// catalog on the full assignment rung that still satisfies the multi-sample
		// floor. That exercises the prod-shaped orchestration without a second
		// full-scale run: the assignment is the same algorithm at every size, and
		// the 150-contact evidence is the staging reseed.
		params.Counts.SeededContacts = 13
		params.Counts.UnmatchedExternal = 2
		params.Counts.StrandedTelegram = 1
		params.Counts.StrandedMessages = 1
		params.Counts.UnmatchedCalendar = 1
		params.Counts.OrphanMeetingNote = 1
		params.Counts.SeededAssertions = 5
		params.Counts.SeededBoolFacts = 3
		params.Counts.SeededRelationships = 3
		params.Counts.SeededTasks = 1
		params.Counts.SeededEntities = 3
		params.Counts.SeededEntityEdges = 3
		params.Counts.SeededSignals = 3
		params.Counts.MessagesPerContact = 2
		params.Counts.SeededSoftDeleted = 1
		params.Counts.SeededMerged = 1
		params.Counts.ImportCandidatesPerSource = 1
		assertArchetypeReplayOutcomes(t, ctx, database, params)
	})
}

func assertArchetypeReplayOutcomes(t *testing.T, ctx context.Context, database *db.Database, params synthetic.SeedParams) {
	t.Helper()

	h := synthetic.NewHarnessForNamespace(t, ctx, database, params.Namespace, params.Seed)
	res, err := synthetic.RunProfile(ctx, h, params)
	require.NoError(t, err)

	n := params.Counts.SeededContacts
	support := repository.NewSyntheticSupportRepository(database.Queries)
	// The reference instant for every overdue question below: the seeded world's
	// OWN anchor, so the prediction and the measurement answer the same question
	// about the same moment instead of racing the wall clock between them.
	anchor := h.Generator().Anchor()

	// --- 1. the assignment the seed actually applied ---------------------------
	samples := h.ArchetypeSamples()
	require.Len(t, samples, n, "one sample per catalog slot, including the history-free ones")
	cohorts := map[factory.Archetype][]uuid.UUID{}
	for i, sample := range samples {
		require.Equal(t, i, sample.SlotIndex, "samples are recorded in frozen catalog order")
		require.Equal(t, synthetic.ArchetypeForIndex(i, n), sample.Archetype,
			"slot %d must carry the archetype the assignment independently derives", i)
		cohorts[sample.Archetype] = append(cohorts[sample.Archetype], sample.ContactID)
	}
	for archetype := range archetypeExpectations {
		require.NotEmpty(t, cohorts[archetype], "every archetype must have a cohort at n=%d", n)
	}

	// --- the world, read once --------------------------------------------------
	buckets, err := support.ListContactBucketsByNamePrefix(ctx, h.Generator().Prefix())
	require.NoError(t, err)
	bucketByID := map[uuid.UUID]repository.ContactBucket{}
	for _, b := range buckets {
		bucketByID[b.ID] = b
	}

	occurredAtByContact := map[uuid.UUID][]time.Time{}
	totalExpected := 0

	for _, sample := range samples {
		want := archetypeExpectations[sample.Archetype]
		rows, err := h.InteractionRepo().ListContactInteractions(ctx, sample.ContactID, 500, 0)
		require.NoError(t, err)

		label := fmt.Sprintf("slot %d (%s)", sample.SlotIndex, sample.Archetype)

		// --- 6. the payload → interaction collapse -----------------------------
		// The expectation is derived from the timeline the seed built and from the
		// pipeline's aggregation semantics; the measurement comes from the pipeline
		// itself. They must agree EXACTLY — a disagreement means the derivation is
		// wrong and has to be re-derived, never relaxed to an inequality.
		require.Len(t, rows, sample.ExpectedInteractions,
			"%s: %d payloads must land exactly %d interaction rows", label, sample.Payloads, sample.ExpectedInteractions)
		totalExpected += sample.ExpectedInteractions

		occurred := make([]time.Time, 0, len(rows))
		sources := map[string]bool{}
		directions := map[string]bool{}
		mutualRows := 0
		var newest repository.Interaction
		for i, row := range rows {
			occurred = append(occurred, row.OccurredAt)
			sources[row.Source] = true
			directions[row.Direction] = true
			if row.Direction == repository.InteractionDirectionMutual {
				mutualRows++
			}
			if i == 0 || row.OccurredAt.After(newest.OccurredAt) {
				newest = row
			}
		}
		sort.Slice(occurred, func(i, j int) bool { return occurred[i].Before(occurred[j]) })
		occurredAtByContact[sample.ContactID] = occurred

		if sample.Archetype == factory.ArchetypeNeverContacted {
			require.Empty(t, rows, "%s: the empty timeline IS the archetype", label)
		} else {
			require.NotEmpty(t, rows, "%s: a history archetype must land rows", label)
		}

		// --- 2. source + direction per archetype --------------------------------
		for source := range sources {
			require.Contains(t, want.Sources, source, "%s: unexpected interaction source %q", label, source)
		}
		for direction := range directions {
			require.Contains(t, want.Directions, direction, "%s: unexpected interaction direction %q", label, direction)
		}
		for _, source := range want.Sources {
			require.True(t, sources[source], "%s: expected at least one %q interaction", label, source)
		}
		if want.MutualRows >= 0 {
			require.Equal(t, want.MutualRows, mutualRows,
				"%s: mutuality is EARNED — exactly %d row(s) may be promoted", label, want.MutualRows)
		} else {
			require.Equal(t, len(rows), mutualRows, "%s: every row must be mutual", label)
		}

		// --- 3. the four-column cadence signature -------------------------------
		ts, err := support.ContactCadenceTimestampsForContact(ctx, sample.ContactID)
		require.NoError(t, err)
		assertCadenceShape(t, label, want.CadenceShape, ts, newest)

		// --- 4. overdue preservation under PRODUCTION durations -----------------
		// The suite runs under CRM_ENV=test, where annual is two hours and every
		// contact is trivially overdue, so reading the ambient config would prove
		// nothing. The durations are constructed explicitly and evaluated at the
		// seeded world's own anchor.
		bucket, ok := bucketByID[sample.ContactID]
		require.True(t, ok, "%s: the catalog contact must be live in the namespace", label)
		require.NotNil(t, bucket.Cadence, "%s: every catalog slot is cadence-bearing", label)
		require.NotNil(t, bucket.CreatedAt, "%s: every catalog slot carries created_at", label)
		overdue := synthetic.OverdueAtProduction(*bucket.Cadence, bucket.LastContacted, *bucket.CreatedAt, anchor)
		switch sample.Archetype {
		case factory.ArchetypeMutualDrifting, factory.ArchetypeDormant:
			require.True(t, overdue,
				"%s: this archetype exists to supply CONTACTED-AND-OVERDUE — its newest two-way entry must stay older than the %s period", label, *bucket.Cadence)
		case factory.ArchetypeMutualRegular, factory.ArchetypeInboundOnly, factory.ArchetypeBurstThenQuiet:
			require.False(t, overdue,
				"%s: this archetype is deliberately NOT overdue on a %s cadence, and the compatibility margin is what keeps that true for days after the reseed", label, *bucket.Cadence)
		}
	}

	// --- 5. multi-sample variation ---------------------------------------------
	// Presence is not the commitment: every ranged archetype must carry at least
	// two samples whose timelines DIFFER. The distinguisher is the full per-contact
	// occurred_at multiset rather than a (row count, newest age) projection — two
	// legitimately jittered samples collide on that projection often enough to be a
	// designed-in flake, and the multiset is strictly stronger, needs no extra read,
	// and collides only if two independent jitter vectors produce identical
	// timelines.
	if n >= 13 {
		for archetype, contactIDs := range cohorts {
			if archetype == factory.ArchetypeNeverContacted {
				continue
			}
			require.GreaterOrEqual(t, len(contactIDs), 2,
				"%s must carry >=2 samples at n=%d", archetype, n)
			distinct := map[string]bool{}
			for _, id := range contactIDs {
				distinct[occurredAtFingerprint(occurredAtByContact[id])] = true
			}
			require.GreaterOrEqual(t, len(distinct), 2,
				"%s must carry >=2 samples with DIFFERENT interaction timelines — jitter has to be observable, not merely drawn", archetype)
		}
	}

	// --- 6 (aggregate). the two counters and the collapse between them ---------
	require.Equal(t, totalExpected, res.ArchetypeInteractions,
		"the block's landed-row counter must equal the sum of the per-contact expectations")
	require.Greater(t, res.ArchetypePayloads, res.ArchetypeInteractions,
		"payloads must exceed rows: a mail promotion pair collapses to one mutual and a chat burst to one session")
	assertArchetypeSettleBudget(t, res)
	burstCohort := cohorts[factory.ArchetypeBurstThenQuiet]
	require.NotEmpty(t, burstCohort, "the burst archetype must have a cohort")
	for _, sample := range samples {
		if sample.Archetype != factory.ArchetypeBurstThenQuiet {
			continue
		}
		require.Equal(t, sample.Payloads-3, sample.ExpectedInteractions,
			"slot %d: K messages in ONE space must collapse to K-3 rows — one opening burst, K-5 fillers, one closing mutual", sample.SlotIndex)
	}

	// --- 7. isolation ----------------------------------------------------------
	// The archetype rows get their OWN counters and are never folded into the
	// settled-interaction accounting, whose equality is exact.
	settledContacts := res.GmailSettled + res.TelegramSettled + res.GCalSettled + res.GChatSettled + res.IMessageSettled
	require.Equal(t, settledContacts*params.Counts.MessagesPerContact+res.SeededPendingFollowUps, res.SettledInteractions,
		"the archetype block must not touch the per-source settled accounting")

	// No pinned fixture may pick up archetype history: the fixtures are seeded
	// outside the catalog and the block addresses catalog contacts only.
	noActivityID, ok := h.PinnedFixtureIDs()[synthetic.FixtureMarkerNoActivity]
	require.True(t, ok, "the no-activity fixture must be recorded")
	noActivityRows, err := h.InteractionRepo().ListContactInteractions(ctx, noActivityID, 100, 0)
	require.NoError(t, err)
	assert.Empty(t, noActivityRows, "the no-activity fixture must carry no interaction at all")
	noActivityTS, err := support.ContactCadenceTimestampsForContact(ctx, noActivityID)
	require.NoError(t, err)
	assert.Nil(t, noActivityTS.LastContacted, "no-activity fixture: last_contacted")
	assert.Nil(t, noActivityTS.LastOutreachAt, "no-activity fixture: last_outreach_at")
	assert.Nil(t, noActivityTS.LastResponseAt, "no-activity fixture: last_response_at")
	assert.Nil(t, noActivityTS.LastInteractionAt, "no-activity fixture: last_interaction_at")
}

// assertCadenceShape checks the four cadence columns against the
// direction→column table, observed rather than restated: outbound writes only
// last_outreach_at, inbound writes the other three, mutual writes all four.
func assertCadenceShape(t *testing.T, label, shape string, ts *repository.ContactCadenceTimestamps, newest repository.Interaction) {
	t.Helper()
	switch shape {
	case cadenceShapeAllFour:
		require.Equal(t, repository.InteractionDirectionMutual, newest.Direction,
			"%s: the NEWEST row must be the mutual one — it is what writes all four columns", label)
		require.NotNil(t, ts.LastContacted, "%s: last_contacted", label)
		require.NotNil(t, ts.LastInteractionAt, "%s: last_interaction_at", label)
		require.NotNil(t, ts.LastOutreachAt, "%s: last_outreach_at", label)
		require.NotNil(t, ts.LastResponseAt, "%s: last_response_at", label)
		require.True(t, ts.LastContacted.Equal(newest.OccurredAt), "%s: last_contacted == the newest mutual's occurred_at", label)
		require.True(t, ts.LastInteractionAt.Equal(newest.OccurredAt), "%s: last_interaction_at == the newest mutual's occurred_at", label)
		require.True(t, ts.LastOutreachAt.Equal(newest.OccurredAt), "%s: last_outreach_at == the newest mutual's occurred_at", label)
		require.True(t, ts.LastResponseAt.Equal(newest.OccurredAt), "%s: last_response_at == the newest mutual's occurred_at", label)
	case cadenceShapeInbound:
		require.NotNil(t, ts.LastContacted, "%s: last_contacted", label)
		require.NotNil(t, ts.LastInteractionAt, "%s: last_interaction_at", label)
		require.NotNil(t, ts.LastResponseAt, "%s: last_response_at", label)
		require.Nil(t, ts.LastOutreachAt,
			"%s: an inbound is the RESPONSE, not the outreach — last_outreach_at must stay unset", label)
		require.True(t, ts.LastContacted.Equal(newest.OccurredAt), "%s: last_contacted == the newest inbound's occurred_at", label)
		require.True(t, ts.LastInteractionAt.Equal(newest.OccurredAt), "%s: last_interaction_at == the newest inbound's occurred_at", label)
		require.True(t, ts.LastResponseAt.Equal(newest.OccurredAt), "%s: last_response_at == the newest inbound's occurred_at", label)
	case cadenceShapeOutreachOnly:
		require.NotNil(t, ts.LastOutreachAt, "%s: last_outreach_at", label)
		require.True(t, ts.LastOutreachAt.Equal(newest.OccurredAt), "%s: last_outreach_at == the newest outbound's occurred_at", label)
		require.Nil(t, ts.LastContacted, "%s: an outbound is not a connection — last_contacted must stay unset", label)
		require.Nil(t, ts.LastInteractionAt, "%s: last_interaction_at must stay unset", label)
		require.Nil(t, ts.LastResponseAt, "%s: last_response_at must stay unset", label)
	case cadenceShapeNone:
		require.Nil(t, ts.LastContacted, "%s: last_contacted", label)
		require.Nil(t, ts.LastInteractionAt, "%s: last_interaction_at", label)
		require.Nil(t, ts.LastOutreachAt, "%s: last_outreach_at", label)
		require.Nil(t, ts.LastResponseAt, "%s: last_response_at", label)
	}
}

// occurredAtFingerprint renders a contact's full interaction-instant MULTISET as
// a comparable string. It is the multi-sample distinguisher: strictly stronger
// than a (count, newest) projection, which two legitimately jittered samples
// collide on often enough to ship a designed-in flake.
func occurredAtFingerprint(occurred []time.Time) string {
	parts := make([]string, 0, len(occurred))
	for _, at := range occurred {
		parts = append(parts, at.UTC().Format(time.RFC3339Nano))
	}
	return fmt.Sprint(parts)
}
