package unit

import (
	"testing"

	"personal-crm/backend/internal/scheduler"

	"github.com/stretchr/testify/assert"
)

func TestGetExternalSyncCronSpec(t *testing.T) {
	cronSpec := scheduler.GetExternalSyncCronSpec()
	assert.Equal(t, "0 */5 * * * *", cronSpec)
}
