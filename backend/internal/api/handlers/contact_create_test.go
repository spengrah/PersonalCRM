package handlers

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// spec: CON-001[0]
// last_contacted records a two-way connection (CAD-006). A brand-new contact has
// had none, so the column stays NULL rather than being seeded with the creation
// instant — contact_by falls back to created_at (CAD-002[0]). Seeding it made the
// value indistinguishable from a real connection, so every consumer that read it
// asserted contact that never happened.
func TestCreateRequestToRepo_LeavesLastContactedUnset(t *testing.T) {
	weekly := "weekly"
	repoReq := createRequestToRepo(CreateContactRequest{
		FullName: "Test Contact",
		Cadence:  &weekly,
	})

	assert.Nil(t, repoReq.LastContacted)
}
