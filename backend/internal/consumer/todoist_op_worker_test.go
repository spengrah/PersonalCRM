package consumer

import (
	"testing"

	"personal-crm/backend/internal/consumer/consumerjobs"
	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// -----------------------------------------------------------------------------
// Derivation helpers (step 2).
// -----------------------------------------------------------------------------

func TestTaskOpCommandUUID_DeterministicAndDistinct(t *testing.T) {
	taskA := uuid.New()
	taskB := uuid.New()

	// Same (op, taskID, fingerprint) → same UUID.
	require.Equal(t,
		taskOpCommandUUID(consumerjobs.TaskOpUpdateDeadline, taskA, "2026-01-01"),
		taskOpCommandUUID(consumerjobs.TaskOpUpdateDeadline, taskA, "2026-01-01"),
	)

	// Different fingerprint (two deadlines for the SAME row) → different UUID.
	// This is the round-1 review defect class: reusing one UUID across
	// different pushed values would let Todoist dedup the second away.
	require.NotEqual(t,
		taskOpCommandUUID(consumerjobs.TaskOpUpdateDeadline, taskA, "2026-01-01"),
		taskOpCommandUUID(consumerjobs.TaskOpUpdateDeadline, taskA, "2026-02-01"),
	)

	// Different op → different UUID.
	require.NotEqual(t,
		taskOpCommandUUID(consumerjobs.TaskOpCreate, taskA, ""),
		taskOpCommandUUID(consumerjobs.TaskOpClose, taskA, ""),
	)

	// Different task → different UUID.
	require.NotEqual(t,
		taskOpCommandUUID(consumerjobs.TaskOpCreate, taskA, ""),
		taskOpCommandUUID(consumerjobs.TaskOpCreate, taskB, ""),
	)
}

func TestDescriptionFingerprint_DeterministicPerValue(t *testing.T) {
	require.Equal(t, descriptionFingerprint("hello world"), descriptionFingerprint("hello world"))
	require.NotEqual(t, descriptionFingerprint("hello world"), descriptionFingerprint("hello worlx"))
}

func TestBuildItemAddFromMetadata_TempIDIsRowID(t *testing.T) {
	taskID := uuid.New()
	task := &repository.ContactTask{
		ID: taskID,
		Metadata: map[string]any{
			"content":     "Follow up: contact",
			"due_date":    "2026-01-01",
			"marker_json": `{"k":"v"}`,
			"project_id":  "proj",
			"label_name":  "followup",
		},
	}
	cmd, err := buildItemAddFromMetadata(task)
	require.NoError(t, err)
	require.Equal(t, "item_add", cmd.Type)
	require.Equal(t, taskID.String(), cmd.TempID, "temp_id must be the row id for server-side create dedup")
	require.Equal(t, taskOpCommandUUID(consumerjobs.TaskOpCreate, taskID, ""), cmd.UUID)
	require.Equal(t, "Follow up: contact", cmd.Args["content"])
}

func TestBuildItemAddFromMetadata_MissingMetadataErrors(t *testing.T) {
	taskID := uuid.New()

	_, err := buildItemAddFromMetadata(&repository.ContactTask{ID: taskID})
	require.Error(t, err, "nil metadata is a permanent error")

	_, err = buildItemAddFromMetadata(&repository.ContactTask{
		ID:       taskID,
		Metadata: map[string]any{"content": "x"}, // no due_date
	})
	require.Error(t, err, "missing due_date is a permanent error")
}
