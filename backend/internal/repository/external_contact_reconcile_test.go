package repository

import (
	"testing"

	"github.com/google/uuid"
)

// TestResolveEffectiveReconcileState exercises the effective-status
// precedence (ignored > imported > matched) for a (possibly duplicate)
// address-book row. Pure function — no DB.
func TestResolveEffectiveReconcileState(t *testing.T) {
	selfID := uuid.New()
	canonID := uuid.New()

	cases := []struct {
		name        string
		selfID      *uuid.UUID
		selfStatus  MatchStatus
		canonID     *uuid.UUID
		canonStatus MatchStatus
		wantOK      bool
		wantStatus  MatchStatus
		wantContact *uuid.UUID
	}{
		{
			name:        "self matched, no canonical -> matched on self contact",
			selfID:      &selfID,
			selfStatus:  MatchStatusMatched,
			wantOK:      true,
			wantStatus:  MatchStatusMatched,
			wantContact: &selfID,
		},
		{
			name:        "self imported, no canonical -> imported on self contact",
			selfID:      &selfID,
			selfStatus:  MatchStatusImported,
			wantOK:      true,
			wantStatus:  MatchStatusImported,
			wantContact: &selfID,
		},
		{
			name:       "self ignored -> skip",
			selfID:     &selfID,
			selfStatus: MatchStatusIgnored,
			wantOK:     false,
		},
		{
			name:       "self unmatched, no canonical -> skip",
			selfID:     nil,
			selfStatus: MatchStatusUnmatched,
			wantOK:     false,
		},
		{
			// Dup with stale matched, canonical imported: imported dominates,
			// effective contact is the canonical's.
			name:        "dup matched of imported canonical -> imported on canonical contact",
			selfID:      &selfID,
			selfStatus:  MatchStatusMatched,
			canonID:     &canonID,
			canonStatus: MatchStatusImported,
			wantOK:      true,
			wantStatus:  MatchStatusImported,
			wantContact: &canonID,
		},
		{
			// Dup matched of matched canonical: matched, canonical contact.
			name:        "dup matched of matched canonical -> matched on canonical contact",
			selfID:      &selfID,
			selfStatus:  MatchStatusMatched,
			canonID:     &canonID,
			canonStatus: MatchStatusMatched,
			wantOK:      true,
			wantStatus:  MatchStatusMatched,
			wantContact: &canonID,
		},
		{
			// Ignored dup of a matched canonical: ignored dominates -> skip.
			name:        "ignored dup of matched canonical -> skip",
			selfID:      &selfID,
			selfStatus:  MatchStatusIgnored,
			canonID:     &canonID,
			canonStatus: MatchStatusMatched,
			wantOK:      false,
		},
		{
			// Matched dup of ignored canonical: ignored dominates -> skip.
			name:        "matched dup of ignored canonical -> skip",
			selfID:      &selfID,
			selfStatus:  MatchStatusMatched,
			canonID:     &canonID,
			canonStatus: MatchStatusIgnored,
			wantOK:      false,
		},
		{
			// Dup pointing at a gone canonical (canonID nil) falls back to
			// its own contact/status.
			name:        "dup matched, canonical gone -> matched on self contact",
			selfID:      &selfID,
			selfStatus:  MatchStatusMatched,
			canonID:     nil,
			canonStatus: "",
			wantOK:      true,
			wantStatus:  MatchStatusMatched,
			wantContact: &selfID,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotContact, gotStatus, gotOK := resolveEffectiveReconcileState(
				tc.selfID, tc.selfStatus, tc.canonID, tc.canonStatus,
			)
			if gotOK != tc.wantOK {
				t.Fatalf("ok = %v, want %v", gotOK, tc.wantOK)
			}
			if !tc.wantOK {
				return
			}
			if gotStatus != tc.wantStatus {
				t.Errorf("status = %q, want %q", gotStatus, tc.wantStatus)
			}
			if tc.wantContact != nil && gotContact != *tc.wantContact {
				t.Errorf("contact = %s, want %s", gotContact, *tc.wantContact)
			}
		})
	}
}
