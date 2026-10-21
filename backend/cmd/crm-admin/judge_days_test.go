package main

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// A historical run's range flags that are malformed or incomplete are refused
// before any call: runMain returns the refusal before it loads the
// configuration or opens the database. DATABASE_URL is blanked, so flags
// wrongly accepted fail at the configuration instead of reaching a database.
// spec: KNW-054.malformed-range-refused
func TestRunMain_RefusesMalformedDayRanges(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"the month flag the day flags replaced, an unknown flag", []string{"--judge-month", "2026-01", "--method", "jev-masked"}, "flag provided but not defined: -judge-month"},
		{"a start day after the end day", []string{"--judge-start-day", "2026-01-07", "--judge-end-day", "2026-01-01", "--method", "jev-masked"}, "the last day is before the first"},
		{"only the start day", []string{"--judge-start-day", "2026-01-01", "--method", "jev-masked"}, "needs both --judge-start-day and --judge-end-day"},
		{"only the end day", []string{"--judge-end-day", "2026-01-07", "--method", "jev-masked"}, "needs both --judge-start-day and --judge-end-day"},
		{"a malformed day", []string{"--judge-start-day", "2026-1-01", "--judge-end-day", "2026-01-07", "--method", "jev-masked"}, `day "2026-1-01": want YYYY-MM-DD`},
		{"a day that is no calendar day", []string{"--judge-start-day", "2026-01-01", "--judge-end-day", "2026-02-30", "--method", "jev-masked"}, `day "2026-02-30": want YYYY-MM-DD`},
		{"a range without --method", []string{"--judge-start-day", "2026-01-01", "--judge-end-day", "2026-01-07"}, "a historical run requires --method <key>"},
		{"--measure and --method alone", []string{"--measure", "--method", "jev-masked"}, "no subcommand specified"},
		{"--measure with another subcommand", []string{"--list-hosts", "--measure"}, "--measure applies only to a historical run"},
		{"--method with a subcommand other than --scorecard", []string{"--list-hosts", "--method", "jev-masked"}, "--method applies only to a historical run"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.ErrorContains(t, runMain(tc.args), tc.want)
		})
	}

	t.Run("--scorecard with --method, accepted by the flags and stopped only by the blank configuration", func(t *testing.T) {
		require.ErrorContains(t, runMain([]string{"--scorecard", "--method", "jev-masked"}), "load config")
	})
}
