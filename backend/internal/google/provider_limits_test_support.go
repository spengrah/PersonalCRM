package google

import "time"

// Read-only views of the per-Sync budgets each provider enforces. They exist so
// a caller that has to SIZE ITS INPUT to a budget — the synthetic batch replay
// adapters bucket GChat spaces and bound Gmail's age span — can assert its
// derived constants against the real values instead of mirroring them as local
// literals. A mirrored literal cannot fail when the provider's budget moves,
// which is precisely the drift these guard against.
//
// Test-support only: nothing in production reads these.

// GChatPageBudgetPerSyncForTest is the total list-page allowance one GChat sweep
// may consume across the membership, content, and edit passes of ALL spaces.
func GChatPageBudgetPerSyncForTest() int { return gchatMaxWindowsPerSync }

// GChatMemberResolveCapForTest is the number of FRESH members.get calls one
// GChat sweep may issue. Each one also decrements the shared page budget, so it
// bounds how much of that budget the reverse email→id warm-up can take.
func GChatMemberResolveCapForTest() int { return gchatMaxMemberResolvesPerSync }

// GmailScanReachForTest is how far forward from a Sync's backfill_since the
// Gmail provider can scan: the per-window span times the per-Sync window cap.
// A message newer than that is never listed, and is dropped silently.
func GmailScanReachForTest() time.Duration {
	return gmailWindowSpan * time.Duration(gmailMaxWindowsPerSync)
}

// CalendarPastEventPageLimitForTest is how many unprocessed past events one
// calendar Sync publishes for.
func CalendarPastEventPageLimitForTest() int { return calendarPastEventPageLimit }
