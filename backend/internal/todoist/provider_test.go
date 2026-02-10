package todoist

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestTryMatchByCRMMarker_SkipsCompletedAndDeleted(t *testing.T) {
	// Provider with nil repos — completed/deleted items return before any repo call.
	p := &CadenceSyncProvider{}
	ctx := context.Background()

	validMarker := `{"crm":true,"contact_id":"aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"}`

	t.Run("completed item returns nil", func(t *testing.T) {
		item := SyncItem{
			ID:          "task_completed",
			Description: validMarker,
			Checked:     true,
		}
		result := p.tryMatchByCRMMarker(ctx, item)
		assert.Nil(t, result)
	})

	t.Run("deleted item returns nil", func(t *testing.T) {
		item := SyncItem{
			ID:          "task_deleted",
			Description: validMarker,
			IsDeleted:   true,
		}
		result := p.tryMatchByCRMMarker(ctx, item)
		assert.Nil(t, result)
	})

	t.Run("completed and deleted item returns nil", func(t *testing.T) {
		item := SyncItem{
			ID:          "task_both",
			Description: validMarker,
			Checked:     true,
			IsDeleted:   true,
		}
		result := p.tryMatchByCRMMarker(ctx, item)
		assert.Nil(t, result)
	})

	t.Run("active item with valid marker proceeds past guard", func(t *testing.T) {
		// Active item with a valid CRM marker will try to look up the contact task
		// via the repo. With a nil repo this panics, so we verify it doesn't
		// return early by recovering the panic — proving the guard was passed.
		item := SyncItem{
			ID:          "task_active",
			Description: validMarker,
			Checked:     false,
			IsDeleted:   false,
		}
		panicked := false
		func() {
			defer func() {
				if r := recover(); r != nil {
					panicked = true
				}
			}()
			p.tryMatchByCRMMarker(ctx, item)
		}()
		assert.True(t, panicked, "active item with valid marker should reach repo call (panic with nil repo)")
	})
}
