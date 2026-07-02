package consumer

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"personal-crm/backend/internal/consumer/consumerjobs"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/todoist"

	"github.com/google/uuid"
)

// taskOpCommandUUID derives the deterministic Todoist Sync command UUID
// (v5) for a task op. Computed at execution time over the value actually
// being pushed: the fingerprint is the deadline string for
// update_deadline, sha256(description) for update_description, and empty
// for create/close/delete. Retries of an unchanged push reuse the UUID
// (safe server-side dedup); a later push of a DIFFERENT value gets a
// DIFFERENT UUID, so Todoist never dedups a genuinely new command away.
func taskOpCommandUUID(op string, taskID uuid.UUID, fingerprint string) string {
	return uuid.NewSHA1(
		uuid.NameSpaceURL,
		[]byte("todoist_op:"+op+":"+taskID.String()+":"+fingerprint),
	).String()
}

// descriptionFingerprint returns the payload fingerprint for an
// update_description op: sha256 of the exact description string being
// pushed, hex-encoded.
func descriptionFingerprint(description string) string {
	sum := sha256.Sum256([]byte(description))
	return hex.EncodeToString(sum[:])
}

// buildItemAddFromMetadata reconstructs the Todoist item_add command from
// the snapshot stored in contact_task.metadata at row-creation time. The
// temp_id is deterministic (contact_task.id), so crash-retries emit the
// same temp_id and Todoist's server-side dedup returns the same real id.
// The command UUID is left as the default create-derivation; callers that
// need a different derivation (the legacy create adapter) override it.
func buildItemAddFromMetadata(task *repository.ContactTask) (todoist.SyncCommand, error) {
	if task.Metadata == nil {
		return todoist.SyncCommand{}, fmt.Errorf("contact_task %s: missing metadata for item_add", task.ID)
	}
	content, _ := task.Metadata["content"].(string)
	dueDate, _ := task.Metadata["due_date"].(string)
	markerJSON, _ := task.Metadata["marker_json"].(string)
	projectID, _ := task.Metadata["project_id"].(string)
	labelName, _ := task.Metadata["label_name"].(string)
	if content == "" || dueDate == "" {
		return todoist.SyncCommand{}, fmt.Errorf("contact_task %s: incomplete metadata for item_add (content/due_date)", task.ID)
	}
	labels := []string{}
	if labelName != "" {
		labels = append(labels, labelName)
	}
	cmd := todoist.NewItemAddCommand(content, markerJSON, projectID, labels, &dueDate)
	// Deterministic temp_id + UUID so crash-retries dedup server-side.
	cmd.TempID = task.ID.String()
	cmd.UUID = taskOpCommandUUID(consumerjobs.TaskOpCreate, task.ID, "")
	return cmd, nil
}
