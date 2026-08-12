package tests

import (
	"context"
	"os"
	"testing"

	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestContactTask_NilIdempotencyKey_AllowsRepeatedCreates guards the sqlc
// type-override flip's handling of contact_task.idempotency_key (repo-shrink
// §5.2). Post-migration-046, idx_contact_task_followup_idempotency is a
// partial unique index on (contact_id, idempotency_key) scoped WHERE
// lifecycle = 'followup_loop' AND idempotency_key IS NOT NULL, so two rows
// sharing contact_id with a NULL key never collide there — NULL != NULL is
// invisible to the index. The regression this guards against is the
// zero-value-means-NULL trap: a conversion that silently substitutes
// an empty string for a nil key would make BOTH rows carry an empty-string
// idempotency_key, which the index treats as a real match and rejects on
// the second insert with a unique_violation (23505).
func TestContactTask_NilIdempotencyKey_AllowsRepeatedCreates(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}

	t.Parallel()
	ctx := context.Background()
	cfg := config.TestConfig()
	cfg.Database.URL = databaseURL

	database, err := db.NewDatabase(ctx, cfg.Database)
	if err != nil {
		t.Skipf("Could not connect to database: %v", err)
	}
	t.Cleanup(database.Close)

	taskRepo := repository.NewContactTaskRepository(database.Queries)

	gen, _ := migrationGenerator(t)
	contact, cleanupContact := seedMigrationContact(ctx, t, database, gen)
	t.Cleanup(cleanupContact)

	// lifecycle='followup_loop' is required to land inside
	// idx_contact_task_followup_idempotency's scope at all (the index this
	// test targets) — kind must then be 'reach_out', the only kind valid
	// with that lifecycle per contact_task_kind_lifecycle_check.
	// state='completed' keeps this pair OUT of
	// idx_contact_task_followup_unique_live's scope (contact_id, provider)
	// WHERE lifecycle='followup_loop' AND state IN ('managed',
	// 'pending_remote_create') — that index only fires for the two live
	// states, so a terminal state avoids colliding there instead of the
	// index under test. external_task_id='' is exempt from
	// unique_external_task_id, which is partial WHERE external_task_id <>
	// ''.
	req := repository.CreateContactTaskRequest{
		ContactID:      contact.ID,
		Provider:       "todoist",
		Kind:           "reach_out",
		Lifecycle:      "followup_loop",
		ExternalTaskID: "",
		State:          string(repository.ContactTaskStateCompleted),
	}

	tx1, err := database.Pool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx1.Rollback(ctx) }() // no-op once committed below; guards a failed create
	first, err := taskRepo.CreateContactTaskTx(ctx, tx1, req, nil)
	require.NoError(t, err, "first nil-idempotency-key create must succeed")
	require.NoError(t, tx1.Commit(ctx))
	t.Cleanup(func() { _ = taskRepo.DeleteContactTask(ctx, first.ID) })

	assert.Nil(t, first.IdempotencyKey, "stored key must round-trip as NULL, not an empty string")

	tx2, err := database.Pool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx2.Rollback(ctx) }() // no-op once committed below; guards a failed create
	second, err := taskRepo.CreateContactTaskTx(ctx, tx2, req, nil)
	require.NoError(t, err, "second nil-idempotency-key create must ALSO succeed: NULL != NULL under idx_contact_task_followup_idempotency")
	require.NoError(t, tx2.Commit(ctx))
	t.Cleanup(func() { _ = taskRepo.DeleteContactTask(ctx, second.ID) })

	assert.Nil(t, second.IdempotencyKey)
	assert.NotEqual(t, first.ID, second.ID)
}
