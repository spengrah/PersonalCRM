package synthetic

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/synthetic/declare"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMapStandardWorldResult_PartialFailuresStayTruthful(t *testing.T) {
	plan := []declare.WorldStep{
		{Kind: declare.WorldStepDeclaration, Key: "DSH-001"},
		{Kind: declare.WorldStepEdge, Key: "long-history"},
		{Kind: declare.WorldStepTail, Key: standardTailName},
	}
	cases := []struct {
		name      string
		failIndex int
		partial   int
	}{
		{name: "early", failIndex: 0, partial: 1},
		{name: "middle", failIndex: 1, partial: 2},
		{name: "final", failIndex: 2, partial: 3},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			world := declare.WorldResult{
				Entities: map[string]declare.Seeded{},
				Current: &declare.WorldStepResult{
					Kind:     plan[tc.failIndex].Kind,
					Key:      plan[tc.failIndex].Key,
					Entities: tc.partial,
					Duration: time.Second,
				},
			}
			for i := 0; i < tc.failIndex; i++ {
				world.Steps = append(world.Steps, declare.WorldStepResult{
					Kind: plan[i].Kind, Key: plan[i].Key, Entities: i + 1, Duration: time.Duration(i+1) * time.Second,
				})
			}
			totalContacts := tc.partial
			for i := 0; i < tc.failIndex; i++ {
				totalContacts += i + 1
			}
			for i := 0; i < totalContacts; i++ {
				world.Order = append(world.Order, declare.Seeded{
					Kind: "contact", ID: fmt.Sprintf("contact-%d", i),
				})
			}
			world.Order = append(world.Order, declare.Seeded{Kind: "note", ID: "not-a-contact"})

			got := mapStandardWorldResult(world, ProfileResult{})

			require.Len(t, got.Timings.Phases, tc.failIndex)
			for i, phase := range got.Timings.Phases {
				assert.Equal(t, plan[i].Kind+":"+plan[i].Key, phase.Name)
			}
			assert.Equal(t, plan[tc.failIndex].Kind+":"+plan[tc.failIndex].Key, got.Timings.Current,
				"Current must name the failing phase, not the last completed phase")
			assert.Equal(t, totalContacts, got.Contacts,
				"the partial summary must count every contact the world manifest completed")
		})
	}
}

func TestRiderAccounting_PartialHelperFailuresStayTruthful(t *testing.T) {
	contact := &repository.Contact{
		ID:       uuid.MustParse("00000000-0000-0000-0000-000000000001"),
		FullName: "synthetic contact",
	}
	seedErr := errors.New("injected rider failure")
	cases := []struct {
		name            string
		kind            riderKind
		rider           riderSeedResult
		wantSettled     int
		wantWorldEntity int
	}{
		{name: "before contact", kind: riderPendingFollowUp},
		{
			name: "pending after contact", kind: riderPendingFollowUp,
			rider: riderSeedResult{contact: contact}, wantWorldEntity: 1,
		},
		{
			name: "pending after gcal", kind: riderPendingFollowUp,
			rider:       riderSeedResult{contact: contact, payloads: 1},
			wantSettled: 1, wantWorldEntity: 1,
		},
		{
			name: "outreach after contact", kind: riderOutreach,
			rider: riderSeedResult{contact: contact}, wantWorldEntity: 1,
		},
		{
			name: "response after contact", kind: riderResponse,
			rider: riderSeedResult{contact: contact}, wantWorldEntity: 1,
		},
		{
			name: "response after outbound", kind: riderResponse,
			rider: riderSeedResult{contact: contact, payloads: 1}, wantWorldEntity: 1,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var res ProfileResult
			var out []declare.Seeded

			err := accountStandardTailRider(tc.kind, tc.rider, seedErr, &res, &out)

			require.ErrorIs(t, err, seedErr)
			assert.Len(t, out, tc.wantWorldEntity)
			assert.Equal(t, tc.wantSettled, res.SettledInteractions)
			assertUncompletedRiderCounters(t, res)
		})
	}
}

func TestRiderAccounting_ClaimsWholeRiderCountersOnlyOnSuccess(t *testing.T) {
	contact := &repository.Contact{
		ID:       uuid.MustParse("00000000-0000-0000-0000-000000000001"),
		FullName: "synthetic contact",
	}
	cases := []struct {
		name          string
		kind          riderKind
		payloads      int
		wantSettled   int
		wantTasks     int
		wantFollowUps int
		wantOutbound  int
		wantMutual    int
	}{
		{
			name: "pending", kind: riderPendingFollowUp, payloads: 1,
			wantSettled: 1, wantTasks: 1, wantFollowUps: 1,
		},
		{name: "outreach", kind: riderOutreach, payloads: 1, wantOutbound: 1},
		{name: "response", kind: riderResponse, payloads: 2, wantMutual: 1},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var res ProfileResult
			var out []declare.Seeded

			err := accountStandardTailRider(
				tc.kind,
				riderSeedResult{contact: contact, payloads: tc.payloads},
				nil,
				&res,
				&out,
			)

			require.NoError(t, err)
			assert.Len(t, out, 1, "a completed rider contributes its contact to the world manifest")
			assert.Equal(t, tc.wantSettled, res.SettledInteractions)
			assert.Equal(t, tc.wantTasks, res.SeededTasks)
			assert.Equal(t, tc.wantFollowUps, res.SeededPendingFollowUps)
			assert.Equal(t, tc.wantOutbound, res.OutboundOnlyContacts)
			assert.Equal(t, tc.wantMutual, res.MutualMessageContacts)
		})
	}
}

func assertUncompletedRiderCounters(t *testing.T, res ProfileResult) {
	t.Helper()
	assert.Zero(t, res.SeededTasks)
	assert.Zero(t, res.SeededPendingFollowUps)
	assert.Zero(t, res.OutboundOnlyContacts)
	assert.Zero(t, res.MutualMessageContacts)
}
