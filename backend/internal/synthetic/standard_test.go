package synthetic

import (
	"fmt"
	"testing"
	"time"

	"personal-crm/backend/internal/synthetic/declare"

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
