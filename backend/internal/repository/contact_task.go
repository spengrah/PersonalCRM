package repository

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"personal-crm/backend/internal/db"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ContactTaskState represents the management state of a contact task
type ContactTaskState string

const (
	ContactTaskStateManaged   ContactTaskState = "managed"
	ContactTaskStateUnmanaged ContactTaskState = "unmanaged"
	ContactTaskStateCompleted ContactTaskState = "completed"
	ContactTaskStateDismissed ContactTaskState = "dismissed"
)

// ContactTask represents a link between a contact and an external task provider
type ContactTask struct {
	ID             uuid.UUID        `json:"id"`
	ContactID      uuid.UUID        `json:"contact_id"`
	Provider       string           `json:"provider"`
	Kind           string           `json:"kind"`
	ExternalTaskID string           `json:"external_task_id"`
	State          ContactTaskState `json:"state"`
	Metadata       map[string]any   `json:"metadata,omitempty"`
	CreatedAt      time.Time        `json:"created_at"`
	UpdatedAt      time.Time        `json:"updated_at"`
}

// ContactTaskWithContact extends ContactTask with contact information for reconciliation
type ContactTaskWithContact struct {
	ContactTask
	FullName      string     `json:"full_name"`
	Cadence       *string    `json:"cadence,omitempty"`
	ContactBy     *time.Time `json:"contact_by,omitempty"`
	LastContacted *time.Time `json:"last_contacted,omitempty"`
}

// CreateContactTaskRequest holds parameters for creating a contact task
type CreateContactTaskRequest struct {
	ContactID      uuid.UUID      `json:"contact_id"`
	Provider       string         `json:"provider"`
	Kind           string         `json:"kind"`
	ExternalTaskID string         `json:"external_task_id"`
	State          string         `json:"state,omitempty"`
	Metadata       map[string]any `json:"metadata,omitempty"`
}

// ContactTaskRepository handles contact task persistence
type ContactTaskRepository struct {
	queries db.Querier
}

// NewContactTaskRepository creates a new contact task repository
func NewContactTaskRepository(queries db.Querier) *ContactTaskRepository {
	return &ContactTaskRepository{queries: queries}
}

// convertDbContactTask converts a database contact task to a repository contact task
func convertDbContactTask(dbTask *db.ContactTask) ContactTask {
	task := ContactTask{
		Provider:       dbTask.Provider,
		Kind:           dbTask.Kind,
		ExternalTaskID: dbTask.ExternalTaskID,
		State:          ContactTaskState(dbTask.State),
	}

	if dbTask.ID.Valid {
		task.ID = uuid.UUID(dbTask.ID.Bytes)
	}
	if dbTask.ContactID.Valid {
		task.ContactID = uuid.UUID(dbTask.ContactID.Bytes)
	}
	if dbTask.CreatedAt.Valid {
		task.CreatedAt = dbTask.CreatedAt.Time
	}
	if dbTask.UpdatedAt.Valid {
		task.UpdatedAt = dbTask.UpdatedAt.Time
	}

	if len(dbTask.Metadata) > 0 {
		var metadata map[string]any
		if err := json.Unmarshal(dbTask.Metadata, &metadata); err == nil {
			task.Metadata = metadata
		}
	}

	return task
}

// convertDbContactTaskWithContact converts a database contact task with contact info
func convertDbContactTaskWithContact(dbRow *db.ListManagedContactTasksRow) ContactTaskWithContact {
	result := ContactTaskWithContact{
		ContactTask: ContactTask{
			Provider:       dbRow.Provider,
			Kind:           dbRow.Kind,
			ExternalTaskID: dbRow.ExternalTaskID,
			State:          ContactTaskState(dbRow.State),
		},
		FullName: dbRow.FullName,
	}

	if dbRow.ID.Valid {
		result.ID = uuid.UUID(dbRow.ID.Bytes)
	}
	if dbRow.ContactID.Valid {
		result.ContactID = uuid.UUID(dbRow.ContactID.Bytes)
	}
	if dbRow.CreatedAt.Valid {
		result.CreatedAt = dbRow.CreatedAt.Time
	}
	if dbRow.UpdatedAt.Valid {
		result.UpdatedAt = dbRow.UpdatedAt.Time
	}

	if len(dbRow.Metadata) > 0 {
		var metadata map[string]any
		if err := json.Unmarshal(dbRow.Metadata, &metadata); err == nil {
			result.Metadata = metadata
		}
	}

	if dbRow.Cadence.Valid {
		result.Cadence = &dbRow.Cadence.String
	}
	if dbRow.ContactBy.Valid {
		// ContactBy is a DATE type, convert to time.Time
		t := dbRow.ContactBy.Time
		result.ContactBy = &t
	}
	if dbRow.LastContacted.Valid {
		t := dbRow.LastContacted.Time
		result.LastContacted = &t
	}

	return result
}

// GetContactTask retrieves a contact task by ID
func (r *ContactTaskRepository) GetContactTask(ctx context.Context, id uuid.UUID) (*ContactTask, error) {
	dbTask, err := r.queries.GetContactTask(ctx, uuidToPgUUID(id))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, db.ErrNotFound
		}
		return nil, err
	}

	task := convertDbContactTask(dbTask)
	return &task, nil
}

// GetContactTaskByContact retrieves a contact task by contact, provider, and kind
func (r *ContactTaskRepository) GetContactTaskByContact(ctx context.Context, contactID uuid.UUID, provider, kind string) (*ContactTask, error) {
	dbTask, err := r.queries.GetContactTaskByContact(ctx, db.GetContactTaskByContactParams{
		ContactID: uuidToPgUUID(contactID),
		Provider:  provider,
		Kind:      kind,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, db.ErrNotFound
		}
		return nil, err
	}

	task := convertDbContactTask(dbTask)
	return &task, nil
}

// GetContactTaskByExternalID retrieves a contact task by external task ID
func (r *ContactTaskRepository) GetContactTaskByExternalID(ctx context.Context, provider, externalTaskID string) (*ContactTask, error) {
	dbTask, err := r.queries.GetContactTaskByExternalID(ctx, db.GetContactTaskByExternalIDParams{
		Provider:       provider,
		ExternalTaskID: externalTaskID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, db.ErrNotFound
		}
		return nil, err
	}

	task := convertDbContactTask(dbTask)
	return &task, nil
}

// ListContactTasksByProvider retrieves all contact tasks for a provider
func (r *ContactTaskRepository) ListContactTasksByProvider(ctx context.Context, provider string, state *string) ([]ContactTask, error) {
	dbTasks, err := r.queries.ListContactTasksByProvider(ctx, db.ListContactTasksByProviderParams{
		Provider: provider,
		State:    stringToPgText(state),
	})
	if err != nil {
		return nil, err
	}

	tasks := make([]ContactTask, len(dbTasks))
	for i, dbTask := range dbTasks {
		tasks[i] = convertDbContactTask(dbTask)
	}

	return tasks, nil
}

// ListContactTasksByContact retrieves all contact tasks for a contact
func (r *ContactTaskRepository) ListContactTasksByContact(ctx context.Context, contactID uuid.UUID) ([]ContactTask, error) {
	dbTasks, err := r.queries.ListContactTasksByContact(ctx, uuidToPgUUID(contactID))
	if err != nil {
		return nil, err
	}

	tasks := make([]ContactTask, len(dbTasks))
	for i, dbTask := range dbTasks {
		tasks[i] = convertDbContactTask(dbTask)
	}

	return tasks, nil
}

// ListContactTasksFiltered retrieves contact tasks for a contact with optional filters
func (r *ContactTaskRepository) ListContactTasksFiltered(ctx context.Context, contactID uuid.UUID, state *string, kind *string) ([]ContactTask, error) {
	dbTasks, err := r.queries.ListContactTasksByContactFiltered(ctx, db.ListContactTasksByContactFilteredParams{
		ContactID: uuidToPgUUID(contactID),
		State:     stringToPgText(state),
		Kind:      stringToPgText(kind),
	})
	if err != nil {
		return nil, err
	}

	tasks := make([]ContactTask, len(dbTasks))
	for i, dbTask := range dbTasks {
		tasks[i] = convertDbContactTask(dbTask)
	}

	return tasks, nil
}

// ListManagedContactTasks retrieves all managed tasks for a provider with contact info
func (r *ContactTaskRepository) ListManagedContactTasks(ctx context.Context, provider string) ([]ContactTaskWithContact, error) {
	dbRows, err := r.queries.ListManagedContactTasks(ctx, provider)
	if err != nil {
		return nil, err
	}

	tasks := make([]ContactTaskWithContact, len(dbRows))
	for i, dbRow := range dbRows {
		tasks[i] = convertDbContactTaskWithContact(dbRow)
	}

	return tasks, nil
}

// CreateContactTask creates a new contact task
func (r *ContactTaskRepository) CreateContactTask(ctx context.Context, req CreateContactTaskRequest) (*ContactTask, error) {
	state := req.State
	if state == "" {
		state = string(ContactTaskStateManaged)
	}

	var metadataBytes []byte
	if req.Metadata != nil {
		var err error
		metadataBytes, err = json.Marshal(req.Metadata)
		if err != nil {
			return nil, err
		}
	}

	dbTask, err := r.queries.CreateContactTask(ctx, db.CreateContactTaskParams{
		ContactID:      uuidToPgUUID(req.ContactID),
		Provider:       req.Provider,
		Kind:           req.Kind,
		ExternalTaskID: req.ExternalTaskID,
		State:          state,
		Metadata:       metadataBytes,
	})
	if err != nil {
		return nil, err
	}

	task := convertDbContactTask(dbTask)
	return &task, nil
}

// UpsertContactTask creates or updates a contact task
func (r *ContactTaskRepository) UpsertContactTask(ctx context.Context, req CreateContactTaskRequest) (*ContactTask, error) {
	state := req.State
	if state == "" {
		state = string(ContactTaskStateManaged)
	}

	var metadataBytes []byte
	if req.Metadata != nil {
		var err error
		metadataBytes, err = json.Marshal(req.Metadata)
		if err != nil {
			return nil, err
		}
	}

	dbTask, err := r.queries.UpsertContactTask(ctx, db.UpsertContactTaskParams{
		ContactID:      uuidToPgUUID(req.ContactID),
		Provider:       req.Provider,
		Kind:           req.Kind,
		ExternalTaskID: req.ExternalTaskID,
		State:          state,
		Metadata:       metadataBytes,
	})
	if err != nil {
		return nil, err
	}

	task := convertDbContactTask(dbTask)
	return &task, nil
}

// UpdateContactTaskState updates the state of a contact task
func (r *ContactTaskRepository) UpdateContactTaskState(ctx context.Context, id uuid.UUID, state ContactTaskState) (*ContactTask, error) {
	dbTask, err := r.queries.UpdateContactTaskState(ctx, db.UpdateContactTaskStateParams{
		ID:    uuidToPgUUID(id),
		State: string(state),
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, db.ErrNotFound
		}
		return nil, err
	}

	task := convertDbContactTask(dbTask)
	return &task, nil
}

// UpdateContactTaskExternalID updates the external task ID of a contact task
func (r *ContactTaskRepository) UpdateContactTaskExternalID(ctx context.Context, id uuid.UUID, externalTaskID string) (*ContactTask, error) {
	dbTask, err := r.queries.UpdateContactTaskExternalID(ctx, db.UpdateContactTaskExternalIDParams{
		ID:             uuidToPgUUID(id),
		ExternalTaskID: externalTaskID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, db.ErrNotFound
		}
		return nil, err
	}

	task := convertDbContactTask(dbTask)
	return &task, nil
}

// UpdateContactTaskMetadata updates the metadata of a contact task
func (r *ContactTaskRepository) UpdateContactTaskMetadata(ctx context.Context, id uuid.UUID, metadata map[string]any) (*ContactTask, error) {
	var metadataBytes []byte
	if metadata != nil {
		var err error
		metadataBytes, err = json.Marshal(metadata)
		if err != nil {
			return nil, err
		}
	}

	dbTask, err := r.queries.UpdateContactTaskMetadata(ctx, db.UpdateContactTaskMetadataParams{
		ID:       uuidToPgUUID(id),
		Metadata: metadataBytes,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, db.ErrNotFound
		}
		return nil, err
	}

	task := convertDbContactTask(dbTask)
	return &task, nil
}

// DeleteContactTask deletes a contact task by ID
func (r *ContactTaskRepository) DeleteContactTask(ctx context.Context, id uuid.UUID) error {
	return r.queries.DeleteContactTask(ctx, uuidToPgUUID(id))
}

// DeleteContactTaskByContact deletes a contact task by contact, provider, and kind
func (r *ContactTaskRepository) DeleteContactTaskByContact(ctx context.Context, contactID uuid.UUID, provider, kind string) error {
	return r.queries.DeleteContactTaskByContact(ctx, db.DeleteContactTaskByContactParams{
		ContactID: uuidToPgUUID(contactID),
		Provider:  provider,
		Kind:      kind,
	})
}

// DeleteContactTasksByProvider deletes all contact tasks for a provider
func (r *ContactTaskRepository) DeleteContactTasksByProvider(ctx context.Context, provider string) error {
	return r.queries.DeleteContactTasksByProvider(ctx, provider)
}

// CountContactTasksByProvider counts contact tasks by provider and state
func (r *ContactTaskRepository) CountContactTasksByProvider(ctx context.Context, provider, state string) (int64, error) {
	return r.queries.CountContactTasksByProvider(ctx, db.CountContactTasksByProviderParams{
		Provider: provider,
		State:    state,
	})
}

// GetContactTaskByPendingTempID finds a contact task by its pending temp ID in metadata
func (r *ContactTaskRepository) GetContactTaskByPendingTempID(ctx context.Context, provider, tempID string) (*ContactTask, error) {
	dbTask, err := r.queries.GetContactTaskByPendingTempID(ctx, db.GetContactTaskByPendingTempIDParams{
		Provider: provider,
		TempID:   tempID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, db.ErrNotFound
		}
		return nil, err
	}

	task := convertDbContactTask(dbTask)
	return &task, nil
}

// ListFollowUpsWithPendingClose finds completed follow-up tasks where Todoist close failed
func (r *ContactTaskRepository) ListFollowUpsWithPendingClose(ctx context.Context) ([]ContactTask, error) {
	dbTasks, err := r.queries.ListFollowUpsWithPendingClose(ctx)
	if err != nil {
		return nil, err
	}

	tasks := make([]ContactTask, len(dbTasks))
	for i, dbTask := range dbTasks {
		tasks[i] = convertDbContactTask(dbTask)
	}

	return tasks, nil
}

// FindPendingFollowUp finds a pending follow-up task for a contact
func (r *ContactTaskRepository) FindPendingFollowUp(ctx context.Context, contactID uuid.UUID) (*ContactTask, error) {
	dbTask, err := r.queries.FindPendingFollowUp(ctx, uuidToPgUUID(contactID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, db.ErrNotFound
		}
		return nil, err
	}

	task := convertDbContactTask(dbTask)
	return &task, nil
}

// CompleteFollowUpForContact marks all pending follow-up tasks as completed for a contact
func (r *ContactTaskRepository) CompleteFollowUpForContact(ctx context.Context, contactID uuid.UUID) ([]ContactTask, error) {
	dbTasks, err := r.queries.CompleteFollowUpForContact(ctx, uuidToPgUUID(contactID))
	if err != nil {
		return nil, err
	}

	tasks := make([]ContactTask, len(dbTasks))
	for i, dbTask := range dbTasks {
		tasks[i] = convertDbContactTask(dbTask)
	}

	return tasks, nil
}

// Note: Helper functions (stringToPgText, uuidToPgUUID, timeToPgTimestamptz)
// are defined in conversions.go
