package declare

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// registerPanics runs Register(d) and returns the recovered panic value, or nil.
func registerPanics(d Declaration) (msg any) {
	defer func() { msg = recover() }()
	Register(d)
	return nil
}

func TestRegisterRejectsMalformedDeclarations(t *testing.T) {
	cases := []struct {
		name string
		d    Declaration
		want string
	}{
		{"empty behavior", Declaration{Entities: []Entity{Contact("a")}}, "empty Behavior id"},
		{"no entities", Declaration{Behavior: "ZZZ-001"}, "no entities"},
		{"nil entity", Declaration{Behavior: "ZZZ-002", Entities: []Entity{nil}}, "is nil"},
		{"blank handle", Declaration{Behavior: "ZZZ-003", Entities: []Entity{Contact("  ")}}, "handle must be non-empty"},
		{"duplicate handle", Declaration{Behavior: "ZZZ-004", Entities: []Entity{Contact("a"), Contact("a")}}, "duplicate entity handle"},
		{"unknown cadence", Declaration{Behavior: "ZZZ-005", Entities: []Entity{Contact("a", Cadence("fortnightly"))}}, "unknown cadence"},
		{"overdue without cadence", Declaration{Behavior: "ZZZ-006", Entities: []Entity{Contact("a", OverdueBy(Days(1)))}}, "OverdueBy requires Cadence"},
		{"overdue with never-contacted", Declaration{Behavior: "ZZZ-007", Entities: []Entity{
			Contact("a", Cadence("weekly"), OverdueBy(Days(1)), NeverContacted()),
		}}, "mutually exclusive"},
		{"overdue with no methods", Declaration{Behavior: "ZZZ-008", Entities: []Entity{
			Contact("a", Cadence("weekly"), OverdueBy(Days(1)), NoMethods()),
		}}, "must carry an email method"},
		{"overdue with phone-only methods", Declaration{Behavior: "ZZZ-009", Entities: []Entity{
			Contact("a", Cadence("weekly"), OverdueBy(Days(1)), Methods("phone")),
		}}, "must carry an email method"},
		{"methods and no-methods", Declaration{Behavior: "ZZZ-010", Entities: []Entity{
			Contact("a", Methods("email"), NoMethods()),
		}}, "mutually exclusive"},
		{"unknown method kind", Declaration{Behavior: "ZZZ-011", Entities: []Entity{Contact("a", Methods("fax"))}}, "unknown method kind"},
		{"duplicate method kind", Declaration{Behavior: "ZZZ-012", Entities: []Entity{Contact("a", Methods("email", "email"))}}, "duplicate method kind"},
		{"negative amount", Declaration{Behavior: "ZZZ-013", Entities: []Entity{
			Contact("a", Cadence("weekly"), OverdueBy(Days(-1))),
		}}, "is negative"},
		{"period amount without cadence", Declaration{Behavior: "ZZZ-014", Entities: []Entity{
			Contact("a", NeverContacted(), CreatedAgo(Periods(2))),
		}}, "declares no cadence"},
		{"overdue with explicit created-ago", Declaration{Behavior: "ZZZ-015", Entities: []Entity{
			Contact("a", Cadence("weekly"), OverdueBy(Days(1)), CreatedAgo(Days(2))),
		}}, "DERIVES the creation age"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msg := registerPanics(tc.d)
			require.NotNil(t, msg, "expected a panic")
			assert.Contains(t, msg, tc.want)
		})
	}
}

func TestRegisterRejectsDuplicateResolution(t *testing.T) {
	// The real registry already holds DSH-005 (a declaration) and DSH-001
	// (RegisterNone) from this package's init, so re-registering either in any
	// combination must panic.
	msg := registerPanics(Declaration{Behavior: "DSH-005", Entities: []Entity{Contact("x")}})
	require.NotNil(t, msg)
	assert.Contains(t, msg, "already registered as a declaration")

	msg = registerPanics(Declaration{Behavior: "DSH-001", Entities: []Entity{Contact("x")}})
	require.NotNil(t, msg)
	assert.Contains(t, msg, "already registered as no-fixture")

	func() {
		defer func() {
			r := recover()
			require.NotNil(t, r)
			assert.Contains(t, r, "already registered as a declaration")
		}()
		RegisterNone("DSH-005", "should panic")
	}()

	func() {
		defer func() {
			r := recover()
			require.NotNil(t, r)
			assert.Contains(t, r, "already registered as no-fixture")
		}()
		RegisterNone("DSH-001", "should panic")
	}()
}

func TestRegisterNoneRejectsEmptyInputs(t *testing.T) {
	for _, tc := range []struct{ id, reason, want string }{
		{"", "why", "empty behavior id"},
		{"ZZZ-100", "", "empty reason"},
	} {
		func() {
			defer func() {
				r := recover()
				require.NotNil(t, r)
				assert.Contains(t, r, tc.want)
			}()
			RegisterNone(tc.id, tc.reason)
		}()
	}
}

func TestRegisteredIsSortedAndLookupWorks(t *testing.T) {
	all := Registered()
	require.NotEmpty(t, all)
	for i := 1; i < len(all); i++ {
		assert.Less(t, all[i-1].Behavior, all[i].Behavior, "Registered() must be sorted by behavior id")
	}

	d, ok := Lookup("CAD-026")
	require.True(t, ok)
	assert.Len(t, d.Entities, 3)
	assert.Equal(t, []string{"card-a", "card-b", "card-c"}, handlesOf(d))

	_, ok = Lookup("DSH-001")
	assert.False(t, ok, "a RegisterNone behavior must not resolve as a declaration")

	reason, ok := IsNone("DSH-001")
	assert.True(t, ok)
	assert.NotEmpty(t, reason)
}

func handlesOf(d Declaration) []string {
	out := make([]string, 0, len(d.Entities))
	for _, e := range d.Entities {
		out = append(out, e.handle())
	}
	return out
}

func TestDashboardDeclarationsAreWellFormed(t *testing.T) {
	dsh, ok := Lookup("DSH-005")
	require.True(t, ok)
	assert.Equal(t, []string{"refresh-target", "refresh-sentinel"}, handlesOf(dsh))
	for _, e := range dsh.Entities {
		assert.Equal(t, "contact", e.kind())
		require.NoError(t, e.validate())
	}
}
