package whatsapp

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// TestWhatsAppConstants_FloorAndWindow pins both one-way values. They are spec
// hard constraints and are unrecoverable once wrong — the history request is
// baked into the pairing payload, and a floor below the CRM horizon stages data
// nothing else carries. This test is what stops a later PR quietly retuning
// either one.
func TestWhatsAppConstants_FloorAndWindow(t *testing.T) {
	assert.Equal(t, "2026-01-01", BackfillFloor)
	assert.Equal(t, uint32(365), HistorySyncDaysLimit)

	want := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	assert.Equal(t, want, BackfillFloorTime(),
		"BackfillFloor must parse to the same date it spells")
}
