package repository

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/logger"

	"github.com/google/uuid"
	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
)

// ContactTaskState represents the management state of a contact task
type ContactTaskState string

const (
	ContactTaskStateManaged             ContactTaskState = "managed"
	ContactTaskStateUnmanaged           ContactTaskState = "unmanaged"
	ContactTaskStateCompleted           ContactTaskState = "completed"
	ContactTaskStateDismissed           ContactTaskState = "dismissed"
	ContactTaskStatePendingRemoteCreate ContactTaskState = "pending_remote_create"
	// ContactTaskStateSuperseded marks an old generation retired by the
	// reconciler itself (deadline-drift close+recreate, skip replacement) —
	// distinct from completed (a real engagement was recorded) and
	// dismissed (user opted out). The migration adding it to the DB CHECK
	// constraint lands with the cadence cutover; no code path inserts this
	// state yet, but the op executor's finalize dispatch handles it for
	// forward compatibility (a row can go superseded-mid-create).
	ContactTaskStateSuperseded ContactTaskState = "superseded"
)

// ContactTask represents a link between a contact and an external task provider
type ContactTask struct {
	ID             uuid.UUID        `json:"id"`
	ContactID      uuid.UUID        `json:"contact_id"`
	Provider       string           `json:"provider"`
	Kind           string           `json:"kind"`
	Lifecycle      string           `json:"lifecycle"`
	ExternalTaskID string           `json:"external_task_id"`
	State          ContactTaskState `json:"state"`
	Metadata       map[string]any   `json:"metadata,omitempty"`
	IdempotencyKey *string          `json:"idempotency_key,omitempty"`
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

// CreateContactTaskRequest holds parameters for creating a contact task.
// Both Kind and Lifecycle are required; the service layer rejects empty
// values before reaching the repository. The DB-level DEFAULT on lifecycle
// is a floor for any insert path that bypasses validation; producer paths
// must set both fields explicitly to avoid silently routing into the
// (kind=*, lifecycle='manual') cell of the state space.
type CreateContactTaskRequest struct {
	ContactID      uuid.UUID      `json:"contact_id"`
	Provider       string         `json:"provider"`
	Kind           string         `json:"kind"`
	Lifecycle      string         `json:"lifecycle"`
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
		Lifecycle:      dbTask.Lifecycle,
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

	if dbTask.IdempotencyKey.Valid {
		key := dbTask.IdempotencyKey.String
		task.IdempotencyKey = &key
	}

	return task
}

// convertDbContactTaskWithContact converts a database contact task with contact info
func convertDbContactTaskWithContact(dbRow *db.ListManagedContactTasksRow) ContactTaskWithContact {
	result := ContactTaskWithContact{
		ContactTask: ContactTask{
			Provider:       dbRow.Provider,
			Kind:           dbRow.Kind,
			Lifecycle:      dbRow.Lifecycle,
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

// GetContactTaskByContactCadenceDue retrieves the cadence-due task for a
// contact+provider. Backed by the partial unique index
// unique_contact_provider_cadence (lifecycle='cadence_due', no state
// filter), so at most one row exists.
func (r *ContactTaskRepository) GetContactTaskByContactCadenceDue(ctx context.Context, contactID uuid.UUID, provider string) (*ContactTask, error) {
	dbTask, err := r.queries.GetContactTaskByContactCadenceDue(ctx, db.GetContactTaskByContactCadenceDueParams{
		ContactID: uuidToPgUUID(contactID),
		Provider:  provider,
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

// GetContactTaskByContactFollowUpLive retrieves the live follow-up task
// for a contact+provider. Backed by the partial unique index
// idx_contact_task_followup_unique_live (lifecycle='followup_loop' AND
// state IN live states).
func (r *ContactTaskRepository) GetContactTaskByContactFollowUpLive(ctx context.Context, contactID uuid.UUID, provider string) (*ContactTask, error) {
	dbTask, err := r.queries.GetContactTaskByContactFollowUpLive(ctx, db.GetContactTaskByContactFollowUpLiveParams{
		ContactID: uuidToPgUUID(contactID),
		Provider:  provider,
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

// GetLegacyActionTaskByContact retrieves the most-recent legacy action
// task for a contact+provider. Multiple action rows are possible per
// contact/provider (no uniqueness ever existed); this returns the most
// recent. The signature is intentionally narrow — callers cannot
// accidentally pass kind='reach_out' here.
func (r *ContactTaskRepository) GetLegacyActionTaskByContact(ctx context.Context, contactID uuid.UUID, provider string) (*ContactTask, error) {
	dbTask, err := r.queries.GetLegacyActionTaskByContact(ctx, db.GetLegacyActionTaskByContactParams{
		ContactID: uuidToPgUUID(contactID),
		Provider:  provider,
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
func (r *ContactTaskRepository) ListContactTasksFiltered(ctx context.Context, contactID uuid.UUID, state *string, kind *string, lifecycle *string) ([]ContactTask, error) {
	dbTasks, err := r.queries.ListContactTasksByContactFiltered(ctx, db.ListContactTasksByContactFilteredParams{
		ContactID: uuidToPgUUID(contactID),
		State:     stringToPgText(state),
		Kind:      stringToPgText(kind),
		Lifecycle: stringToPgText(lifecycle),
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
		Lifecycle:      req.Lifecycle,
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

// CreateContactTaskAtTime is a seed-only variant of CreateContactTask that sets
// created_at explicitly (production always defaults it to NOW()). It lets the
// synthetic seeder vary a linked task's created_at — its "link age" — relative to
// the generator anchor. No request path calls this.
func (r *ContactTaskRepository) CreateContactTaskAtTime(ctx context.Context, req CreateContactTaskRequest, createdAt time.Time) (*ContactTask, error) {
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

	dbTask, err := r.queries.CreateContactTaskAtTime(ctx, db.CreateContactTaskAtTimeParams{
		ContactID:      uuidToPgUUID(req.ContactID),
		Provider:       req.Provider,
		Kind:           req.Kind,
		Lifecycle:      req.Lifecycle,
		ExternalTaskID: req.ExternalTaskID,
		State:          state,
		Metadata:       metadataBytes,
		CreatedAt:      timeToPgTimestamptz(&createdAt),
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
		Lifecycle:      req.Lifecycle,
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

// DeleteContactTaskByContactCadenceDue removes the cadence-due task link
// for a contact+provider (e.g., when cadence is disabled). The lifecycle
// predicate matches the post-046 schema; cadence-due rows are unique per
// (contact_id, provider).
func (r *ContactTaskRepository) DeleteContactTaskByContactCadenceDue(ctx context.Context, contactID uuid.UUID, provider string) error {
	return r.queries.DeleteContactTaskByContactCadenceDue(ctx, db.DeleteContactTaskByContactCadenceDueParams{
		ContactID: uuidToPgUUID(contactID),
		Provider:  provider,
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

// FindPendingFollowUpTx finds a pending follow-up task for a contact using
// the provided transaction. Matches both 'managed' and
// 'pending_remote_create' live states so a two-step creation flow is
// visible to callers running inside the consumer worker tx.
func (r *ContactTaskRepository) FindPendingFollowUpTx(ctx context.Context, tx pgx.Tx, contactID uuid.UUID) (*ContactTask, error) {
	q := r.queries
	if tx != nil {
		q = db.New(tx)
	}
	dbTask, err := q.FindPendingFollowUpTx(ctx, uuidToPgUUID(contactID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, db.ErrNotFound
		}
		return nil, err
	}

	task := convertDbContactTask(dbTask)
	return &task, nil
}

// GetContactTaskByIdempotencyKey performs the partial-index lookup on
// (contact_id, idempotency_key) WHERE lifecycle='followup_loop'. Used by
// the crash-safe two-step follow-up creation flow to detect a
// pre-existing local row before inserting a new one. The lifecycle
// constraint lives in the partial unique index, so a coincidentally-
// keyed (reach_out, manual) row will not collide.
func (r *ContactTaskRepository) GetContactTaskByIdempotencyKey(ctx context.Context, contactID uuid.UUID, key string) (*ContactTask, error) {
	dbTask, err := r.queries.GetContactTaskByIdempotencyKey(ctx, db.GetContactTaskByIdempotencyKeyParams{
		ContactID:      uuidToPgUUID(contactID),
		IdempotencyKey: pgtype.Text{String: key, Valid: true},
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

// GetContactTaskByIdempotencyKeyTx is the tx-threaded variant of
// GetContactTaskByIdempotencyKey. Used by the FollowUpManager consumer
// inside the event-processing tx to detect a prior step-1 insert.
func (r *ContactTaskRepository) GetContactTaskByIdempotencyKeyTx(ctx context.Context, tx pgx.Tx, contactID uuid.UUID, key string) (*ContactTask, error) {
	q := r.queries
	if tx != nil {
		q = db.New(tx)
	}
	dbTask, err := q.GetContactTaskByIdempotencyKey(ctx, db.GetContactTaskByIdempotencyKeyParams{
		ContactID:      uuidToPgUUID(contactID),
		IdempotencyKey: pgtype.Text{String: key, Valid: true},
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

// CountRiverJobsByContactTask returns the number of river_job rows of
// the given kind whose args contain contact_task_id = id. Test-only
// helper backing integration-test assertions; the SQL lives in sqlc so
// Go tests don't inline raw queries (core.md rule 2).
func (r *ContactTaskRepository) CountRiverJobsByContactTask(ctx context.Context, kind string, contactTaskID uuid.UUID) (int64, error) {
	return r.queries.CountRiverJobsByContactTask(ctx, db.CountRiverJobsByContactTaskParams{
		Kind:          kind,
		ContactTaskID: contactTaskID.String(),
	})
}

// CountTodoistOpJobsByOp is a test-only count of todoist_task_op river_job
// rows for a contact_task id and op verb. Wraps the sqlc query so cutover
// integration tests can assert a specific op was (or was not) enqueued
// without inlining raw SQL.
func (r *ContactTaskRepository) CountTodoistOpJobsByOp(ctx context.Context, contactTaskID uuid.UUID, op string) (int64, error) {
	return r.queries.CountTodoistOpJobsByOp(ctx, db.CountTodoistOpJobsByOpParams{
		ContactTaskID: contactTaskID.String(),
		Op:            op,
	})
}

// GetContactTaskTx is the tx-threaded variant of GetContactTask. Used by
// the Todoist follow-up create worker's phase-3 re-read so the write
// phase sees any state transition that happened during the HTTP phase.
func (r *ContactTaskRepository) GetContactTaskTx(ctx context.Context, tx pgx.Tx, id uuid.UUID) (*ContactTask, error) {
	q := r.queries
	if tx != nil {
		q = db.New(tx)
	}
	dbTask, err := q.GetContactTask(ctx, uuidToPgUUID(id))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, db.ErrNotFound
		}
		return nil, err
	}
	task := convertDbContactTask(dbTask)
	return &task, nil
}

// CreateContactTaskTx is the tx-threaded variant of CreateContactTask
// plus an optional idempotency_key argument. Used by the cutover
// FollowUpManager to insert step-1 pending_remote_create rows with a
// deterministic local idempotency key. Callers outside the follow-up
// flow pass idempotencyKey=nil.
func (r *ContactTaskRepository) CreateContactTaskTx(ctx context.Context, tx pgx.Tx, req CreateContactTaskRequest, idempotencyKey *string) (*ContactTask, error) {
	q := r.queries
	if tx != nil {
		q = db.New(tx)
	}
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
	key := pgtype.Text{Valid: false}
	if idempotencyKey != nil {
		key = pgtype.Text{String: *idempotencyKey, Valid: true}
	}
	dbTask, err := q.CreateContactTaskWithIdempotencyKey(ctx, db.CreateContactTaskWithIdempotencyKeyParams{
		ContactID:      uuidToPgUUID(req.ContactID),
		Provider:       req.Provider,
		Kind:           req.Kind,
		Lifecycle:      req.Lifecycle,
		ExternalTaskID: req.ExternalTaskID,
		State:          state,
		Metadata:       metadataBytes,
		IdempotencyKey: key,
	})
	if err != nil {
		return nil, err
	}
	task := convertDbContactTask(dbTask)
	return &task, nil
}

// UpdateContactTaskStateTx is the tx-threaded variant of UpdateContactTaskState.
func (r *ContactTaskRepository) UpdateContactTaskStateTx(ctx context.Context, tx pgx.Tx, id uuid.UUID, state ContactTaskState) (*ContactTask, error) {
	q := r.queries
	if tx != nil {
		q = db.New(tx)
	}
	dbTask, err := q.UpdateContactTaskState(ctx, db.UpdateContactTaskStateParams{
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

// UpdateContactTaskMetadataTx is the tx-threaded variant of
// UpdateContactTaskMetadata.
func (r *ContactTaskRepository) UpdateContactTaskMetadataTx(ctx context.Context, tx pgx.Tx, id uuid.UUID, metadata map[string]any) (*ContactTask, error) {
	q := r.queries
	if tx != nil {
		q = db.New(tx)
	}
	var metadataBytes []byte
	if metadata != nil {
		var err error
		metadataBytes, err = json.Marshal(metadata)
		if err != nil {
			return nil, err
		}
	}
	dbTask, err := q.UpdateContactTaskMetadata(ctx, db.UpdateContactTaskMetadataParams{
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

// UpdateContactTaskExternalIDTx is the tx-threaded variant of
// UpdateContactTaskExternalID. The underlying query atomically sets
// external_task_id, transitions state to 'managed', and bumps
// updated_at — this is the step-2 finalize path for the cutover
// follow-up flow.
func (r *ContactTaskRepository) UpdateContactTaskExternalIDTx(ctx context.Context, tx pgx.Tx, id uuid.UUID, externalTaskID string) (*ContactTask, error) {
	q := r.queries
	if tx != nil {
		q = db.New(tx)
	}
	dbTask, err := q.UpdateContactTaskExternalID(ctx, db.UpdateContactTaskExternalIDParams{
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

// CompletedTaskRef identifies a contact_task row the merge-time close
// transitioned to 'completed', with the fields the service needs to decide
// remote-close enqueue eligibility (a real external id — non-empty AND not a
// pending temp id).
type CompletedTaskRef struct {
	ID             uuid.UUID
	ExternalTaskID string
	Provider       string
	// PendingTempID is metadata->>'pending_temp_id' ('' when absent). When it
	// equals ExternalTaskID the row still carries a Todoist temp id, so no
	// close job may be enqueued for it (mirrors todoist.isPendingTempID).
	PendingTempID string
}

// CompleteLiveTasksForContactTx closes (state='completed') the contact's live
// AUTOMATED tasks (lifecycle cadence_due/followup_loop, state managed/
// pending_remote_create) inside the caller's tx and returns refs for the
// closed rows. Manual-lifecycle rows are untouched — the merge repoints them
// instead (RepointManualContactTasksToContact). pendingTempIDKey is the
// metadata key holding the provider's pending temp id
// (todoist.MetadataKeyPendingTempID), passed in so the repository stays
// provider-blind.
func (r *ContactTaskRepository) CompleteLiveTasksForContactTx(ctx context.Context, tx pgx.Tx, contactID uuid.UUID, pendingTempIDKey string) ([]CompletedTaskRef, error) {
	q := r.queries
	if tx != nil {
		q = db.New(tx)
	}
	rows, err := q.CompleteLiveContactTasksForContact(ctx, db.CompleteLiveContactTasksForContactParams{
		ContactID:        uuidToPgUUID(contactID),
		PendingTempIDKey: pendingTempIDKey,
	})
	if err != nil {
		return nil, err
	}
	refs := make([]CompletedTaskRef, 0, len(rows))
	for _, row := range rows {
		ref := CompletedTaskRef{
			ExternalTaskID: row.ExternalTaskID,
			Provider:       row.Provider,
			PendingTempID:  row.PendingTempID,
		}
		if row.ID.Valid {
			ref.ID = uuid.UUID(row.ID.Bytes)
		}
		refs = append(refs, ref)
	}
	return refs, nil
}

// transferAutomatedTaskConstraints are the partial unique indexes
// TransferAutomatedTasksForMergeTx's query guards against with its NOT
// EXISTS clauses. A unique-violation on any other constraint is an
// unexpected integrity defect, not a lost race, and must not be swallowed.
var transferAutomatedTaskConstraints = map[string]bool{
	"unique_contact_provider_cadence":       true,
	"idx_contact_task_followup_unique_live": true,
	"idx_contact_task_followup_idempotency": true,
}

// classifyTransferConflict decides whether an error from the transfer query
// is a recoverable race — a concurrent insert winning against one of the
// three indexes the query's own NOT EXISTS clauses guard — or an unexpected
// integrity defect that must propagate. Matching on the SQLSTATE alone would
// swallow an unrelated integrity defect and silently degrade every merge to
// close-everything, which is why ConstraintName is checked too. Extracted
// from TransferAutomatedTasksForMergeTx so the classification can be unit
// tested without a database.
func classifyTransferConflict(err error) (pgErr *pgconn.PgError, recoverable bool) {
	if !errors.As(err, &pgErr) {
		return nil, false
	}
	recoverable = pgErr.Code == pgerrcode.UniqueViolation && transferAutomatedTaskConstraints[pgErr.ConstraintName]
	return pgErr, recoverable
}

// TransferredTaskRef identifies a contact_task row
// TransferAutomatedTasksForMergeTx moved onto the target.
type TransferredTaskRef struct {
	ID        uuid.UUID
	Lifecycle string
}

// TransferAutomatedTasksForMergeTx moves the source's transferable automated
// rows onto the target inside a savepoint. A concurrent reconcile insert can
// win the race between the query's NOT EXISTS guard and its UPDATE; the
// resulting 23505 would abort the caller's whole merge tx, so it is absorbed
// here and the rows are left for the close path instead.
func (r *ContactTaskRepository) TransferAutomatedTasksForMergeTx(
	ctx context.Context, tx pgx.Tx, sourceID, targetID uuid.UUID,
) ([]TransferredTaskRef, error) {
	sp, err := tx.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("open automated task transfer savepoint: %w", err)
	}

	rows, err := db.New(sp).TransferAutomatedContactTasksToContact(ctx, db.TransferAutomatedContactTasksToContactParams{
		SourceContactID: uuidToPgUUID(sourceID),
		TargetContactID: uuidToPgUUID(targetID),
	})
	if err != nil {
		pgErr, recoverable := classifyTransferConflict(err)

		if rbErr := sp.Rollback(ctx); rbErr != nil {
			return nil, fmt.Errorf("rollback automated task transfer savepoint: %w (transfer: %w)", rbErr, err)
		}

		if !recoverable {
			return nil, fmt.Errorf("transfer automated tasks for merge: %w", err)
		}

		logger.Warn().
			Str("source_contact_id", sourceID.String()).
			Str("target_contact_id", targetID.String()).
			Str("constraint", pgErr.ConstraintName).
			Msg("merge: automated task transfer lost a race; falling back to close")
		return nil, nil
	}

	if err := sp.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit automated task transfer savepoint: %w", err)
	}

	refs := make([]TransferredTaskRef, 0, len(rows))
	for _, row := range rows {
		ref := TransferredTaskRef{Lifecycle: row.Lifecycle}
		if row.ID.Valid {
			ref.ID = uuid.UUID(row.ID.Bytes)
		}
		refs = append(refs, ref)
	}
	return refs, nil
}

// SetContactTaskExternalIDOnlyTx persists external_task_id WITHOUT
// touching state. Used only by the close-while-pending race path in the
// cutover follow-up create worker — the row is already in state='completed'
// (an inbound arrived while step-2 was in flight) and we record the
// Todoist-returned ID so the subsequent close can target it.
func (r *ContactTaskRepository) SetContactTaskExternalIDOnlyTx(ctx context.Context, tx pgx.Tx, id uuid.UUID, externalTaskID string) error {
	q := r.queries
	if tx != nil {
		q = db.New(tx)
	}
	return q.SetContactTaskExternalIDOnly(ctx, db.SetContactTaskExternalIDOnlyParams{
		ID:             uuidToPgUUID(id),
		ExternalTaskID: externalTaskID,
	})
}

// Note: Helper functions (stringToPgText, uuidToPgUUID, timeToPgTimestamptz)
// are defined in conversions.go
