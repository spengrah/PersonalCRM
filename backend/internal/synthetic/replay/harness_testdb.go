package replay

import (
	"sync"
	"testing"

	"personal-crm/backend/internal/db"
)

// testClones maps each running test to its clone. The entry is removed when the
// test ends, after the clone itself is closed and dropped.
var testClones sync.Map // *testing.T -> *testClone

type testClone struct {
	once     sync.Once
	database *db.Database
}

// testDatabase returns the calling test's own database clone, opening it the
// first time the test builds a harness. Every harness the test builds shares
// the clone; the clone is dropped when the test ends.
//
// The clone is structural, not optional: every harness starts a River client,
// and River's leader maintenance moves retryable and scheduled jobs to
// available across ALL queues, so the private fetch queue (SyntheticQueueName)
// does not isolate a harness on a database another test shares.
func testDatabase(t *testing.T) *db.Database {
	t.Helper()
	v, loaded := testClones.LoadOrStore(t, &testClone{})
	clone := v.(*testClone)
	if !loaded {
		t.Cleanup(func() { testClones.Delete(t) })
	}
	clone.once.Do(func() { clone.database = openTestClone(t) })
	return clone.database
}
