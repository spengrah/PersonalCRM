package declare

import (
	"testing"

	"personal-crm/backend/internal/spec"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// specDir is the real corpus, relative to this package directory
// (backend/internal/synthetic/declare → repo root).
const specDir = "../../../../spec"

// TestDeclareCompleteness is the arc's completeness gate: EVERY ui-surface,
// non-retired spec behavior must be resolved exactly once — by a declaration,
// by RegisterNone, or by a waiver — and no resolution may name an id outside
// that universe. It rides the existing `make test-unit` lane; no new gate.
func TestDeclareCompleteness(t *testing.T) {
	files, violations, err := spec.ParseDir(specDir)
	require.NoError(t, err)
	require.Empty(t, violations, "the spec corpus does not parse cleanly; fix spec-lint first")

	problems := CheckCompleteness(files, DeclaredIDs(), NoneReasons(), Waivers)
	if len(problems) > 0 {
		for _, p := range problems {
			t.Errorf("completeness: %s", p)
		}
	}
}

// The universe should be the 82 ui-surface behaviors the arc counted; a drift
// here means behaviors landed or retired upstream and the resolution tables
// need a conscious reconciliation (which the test above already forces — this
// one just names the cause).
func TestCompletenessUniverseAccounting(t *testing.T) {
	files, _, err := spec.ParseDir(specDir)
	require.NoError(t, err)

	var universe int
	for _, f := range files {
		for i := range f.Behaviors {
			b := &f.Behaviors[i]
			if b.Surface == "ui" && b.Status != "retired" {
				universe++
			}
		}
	}
	assert.Equal(t, len(DeclaredIDs())+len(NoneReasons())+len(Waivers), universe,
		"declarations + none + waivers must exactly partition the ui-surface universe")
}

// --- pure-checker table tests: the gate must be able to FAIL ----------------

func uiFile(behaviors ...spec.Behavior) []*spec.File {
	return []*spec.File{{Domain: "t", Behaviors: behaviors}}
}

func TestCheckCompletenessDetectsProblems(t *testing.T) {
	ui := spec.Behavior{ID: "AAA-001", Surface: "ui", Status: "current"}
	uiProposed := spec.Behavior{ID: "AAA-002", Surface: "ui", Status: "proposed"}
	retired := spec.Behavior{ID: "AAA-003", Surface: "ui", Status: "retired"}
	apiSurface := spec.Behavior{ID: "AAA-004", Surface: "api", Status: "current"}

	cases := []struct {
		name     string
		files    []*spec.File
		declared map[string]bool
		none     map[string]string
		waivers  map[string]string
		wantID   string
		wantKind string
	}{
		{
			name:     "unresolved ui behavior",
			files:    uiFile(ui),
			wantID:   "AAA-001",
			wantKind: ProblemUnresolved,
		},
		{
			name:     "unresolved proposed ui behavior (proposed is in the universe)",
			files:    uiFile(uiProposed),
			wantID:   "AAA-002",
			wantKind: ProblemUnresolved,
		},
		{
			name:     "waiver for a retired id",
			files:    uiFile(retired),
			waivers:  map[string]string{"AAA-003": "stale"},
			wantID:   "AAA-003",
			wantKind: ProblemStale,
		},
		{
			name:     "declared and waived at once",
			files:    uiFile(ui),
			declared: map[string]bool{"AAA-001": true},
			waivers:  map[string]string{"AAA-001": "also waived"},
			wantID:   "AAA-001",
			wantKind: ProblemDoubleResolved,
		},
		{
			name:     "none and waived at once",
			files:    uiFile(ui),
			none:     map[string]string{"AAA-001": "no fixture"},
			waivers:  map[string]string{"AAA-001": "also waived"},
			wantID:   "AAA-001",
			wantKind: ProblemDoubleResolved,
		},
		{
			name:     "api-surface id in waivers",
			files:    uiFile(apiSurface),
			waivers:  map[string]string{"AAA-004": "wrong surface"},
			wantID:   "AAA-004",
			wantKind: ProblemStale,
		},
		{
			name:     "declaration for an id that no longer exists",
			files:    uiFile(ui),
			declared: map[string]bool{"AAA-001": true, "GONE-001": true},
			wantID:   "GONE-001",
			wantKind: ProblemStale,
		},
		{
			name:     "RegisterNone for an id that no longer exists",
			files:    uiFile(ui),
			none:     map[string]string{"AAA-001": "ok", "GONE-002": "stale"},
			wantID:   "GONE-002",
			wantKind: ProblemStale,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			problems := CheckCompleteness(tc.files, tc.declared, tc.none, tc.waivers)
			require.NotEmpty(t, problems, "the gate must FAIL on this input")
			var found bool
			for _, p := range problems {
				if p.BehaviorID == tc.wantID && p.Kind == tc.wantKind {
					found = true
				}
			}
			assert.True(t, found, "want a %s problem for %s, got %v", tc.wantKind, tc.wantID, problems)
		})
	}
}

func TestCheckCompletenessAcceptsAFullyResolvedUniverse(t *testing.T) {
	files := uiFile(
		spec.Behavior{ID: "AAA-001", Surface: "ui", Status: "current"},
		spec.Behavior{ID: "AAA-002", Surface: "ui", Status: "proposed"},
		spec.Behavior{ID: "AAA-003", Surface: "ui", Status: "retired"},
		spec.Behavior{ID: "AAA-004", Surface: "api", Status: "current"},
		spec.Behavior{ID: "AAA-005", Surface: "none", Status: "current"},
	)
	problems := CheckCompleteness(files,
		map[string]bool{"AAA-001": true},
		map[string]string{"AAA-002": "no fixture"},
		nil,
	)
	assert.Empty(t, problems)
}
