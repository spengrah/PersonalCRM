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
	// Obligation is set exactly on a task of lifecycle obligation.
	Obligation *TaskObligation `json:"obligation,omitempty"`
	CreatedAt  time.Time       `json:"created_at"`
	UpdatedAt  time.Time       `json:"updated_at"`
}

// Obligation debtor and basis values (migration 094).
const (
	ObligationDebtorUser      = "user"
	ObligationDebtorContact   = "contact"
	ObligationBasisReply      = "reply"
	ObligationBasisCommitment = "commitment"
)

// TaskObligation is the obligation an obligation task keeps open: who owes
// (the user, or the task's contact) what (a reply or a commitment) on which
// thread, and the cut-off of the judgment that opened it. ContinuedBy names
// the task that continues the obligation once this task closed: the
// commitment task that superseded a reply task, or the task a contact merge
// kept when two open obligations became one.
type TaskObligation struct {
	ThreadID    uuid.UUID  `json:"thread_id"`
	Debtor      string     `json:"debtor"`
	Basis       string     `json:"basis"`
	CutOff      time.Time  `json:"cut_off"`
	ContinuedBy *uuid.UUID `json:"continued_by,omitempty"`
}

// IsLive reports whether the task is live: managed, or awaiting its remote
// create. An obligation is open exactly while its task is live.
func (t *ContactTask) IsLive() bool {
	return t.State == ContactTaskStateManaged || t.State == ContactTaskStatePendingRemoteCreate
}

// CreateObligationTaskRequest is the input for opening an obligation's task.
type CreateObligationTaskRequest struct {
	ContactID  uuid.UUID
	Provider   string
	Kind       string
	State      ContactTaskState
	Metadata   map[string]any
	Obligation TaskObligation
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

	task.ID = dbTask.ID
	task.ContactID = dbTask.ContactID
	if dbTask.CreatedAt != nil {
		task.CreatedAt = *dbTask.CreatedAt
	}
	if dbTask.UpdatedAt != nil {
		task.UpdatedAt = *dbTask.UpdatedAt
	}

	if len(dbTask.Metadata) > 0 {
		var metadata map[string]any
		if err := json.Unmarshal(dbTask.Metadata, &metadata); err == nil {
			task.Metadata = metadata
		}
	}

	task.IdempotencyKey = dbTask.IdempotencyKey
	if dbTask.ObligationThreadID != nil && dbTask.ObligationDebtor != nil &&
		dbTask.ObligationBasis != nil && dbTask.ObligationCutOff != nil {
		task.Obligation = &TaskObligation{
			ThreadID:    *dbTask.ObligationThreadID,
			Debtor:      *dbTask.ObligationDebtor,
			Basis:       *dbTask.ObligationBasis,
			CutOff:      *dbTask.ObligationCutOff,
			ContinuedBy: dbTask.ObligationContinuedBy,
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
			Lifecycle:      dbRow.Lifecycle,
			ExternalTaskID: dbRow.ExternalTaskID,
			State:          ContactTaskState(dbRow.State),
		},
		FullName: dbRow.FullName,
	}

	result.ID = dbRow.ID
	result.ContactID = dbRow.ContactID
	if dbRow.CreatedAt != nil {
		result.CreatedAt = *dbRow.CreatedAt
	}
	if dbRow.UpdatedAt != nil {
		result.UpdatedAt = *dbRow.UpdatedAt
	}

	if len(dbRow.Metadata) > 0 {
		var metadata map[string]any
		if err := json.Unmarshal(dbRow.Metadata, &metadata); err == nil {
			result.Metadata = metadata
		}
	}

	result.Cadence = dbRow.Cadence
	result.ContactBy = dbRow.ContactBy
	result.LastContacted = dbRow.LastContacted

	return result
}

// GetContactTask retrieves a contact task by ID
func (r *ContactTaskRepository) GetContactTask(ctx context.Context, id uuid.UUID) (*ContactTask, error) {
	dbTask, err := r.queries.GetContactTask(ctx, id)
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
		ContactID: contactID,
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
		ContactID: contactID,
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
		ContactID: contactID,
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
		State:    state,
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
	dbTasks, err := r.queries.ListContactTasksByContact(ctx, contactID)
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
		ContactID: contactID,
		State:     state,
		Kind:      kind,
		Lifecycle: lifecycle,
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
		ContactID:      req.ContactID,
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
		ContactID:      req.ContactID,
		Provider:       req.Provider,
		Kind:           req.Kind,
		Lifecycle:      req.Lifecycle,
		ExternalTaskID: req.ExternalTaskID,
		State:          state,
		Metadata:       metadataBytes,
		CreatedAt:      &createdAt,
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
		ContactID:      req.ContactID,
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
		ID:    id,
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
		ID:             id,
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
		ID:       id,
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
	return r.queries.DeleteContactTask(ctx, id)
}

// DeleteContactTaskByContactCadenceDue removes the cadence-due task link
// for a contact+provider (e.g., when cadence is disabled). The lifecycle
// predicate matches the post-046 schema; cadence-due rows are unique per
// (contact_id, provider).
func (r *ContactTaskRepository) DeleteContactTaskByContactCadenceDue(ctx context.Context, contactID uuid.UUID, provider string) error {
	return r.queries.DeleteContactTaskByContactCadenceDue(ctx, db.DeleteContactTaskByContactCadenceDueParams{
		ContactID: contactID,
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
	dbTask, err := r.queries.FindPendingFollowUp(ctx, contactID)
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
	dbTasks, err := r.queries.CompleteFollowUpForContact(ctx, contactID)
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
	if tx == nil {
		return nil, fmt.Errorf("FindPendingFollowUpTx: %w", errNilTx)
	}
	q := db.New(tx)
	dbTask, err := q.FindPendingFollowUpTx(ctx, contactID)
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
		ContactID:      contactID,
		IdempotencyKey: &key,
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
	if tx == nil {
		return nil, fmt.Errorf("GetContactTaskByIdempotencyKeyTx: %w", errNilTx)
	}
	q := db.New(tx)
	dbTask, err := q.GetContactTaskByIdempotencyKey(ctx, db.GetContactTaskByIdempotencyKeyParams{
		ContactID:      contactID,
		IdempotencyKey: &key,
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
	if tx == nil {
		return nil, fmt.Errorf("GetContactTaskTx: %w", errNilTx)
	}
	q := db.New(tx)
	dbTask, err := q.GetContactTask(ctx, id)
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
	if tx == nil {
		return nil, fmt.Errorf("CreateContactTaskTx: %w", errNilTx)
	}
	q := db.New(tx)
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
	dbTask, err := q.CreateContactTaskWithIdempotencyKey(ctx, db.CreateContactTaskWithIdempotencyKeyParams{
		ContactID:      req.ContactID,
		Provider:       req.Provider,
		Kind:           req.Kind,
		Lifecycle:      req.Lifecycle,
		ExternalTaskID: req.ExternalTaskID,
		State:          state,
		Metadata:       metadataBytes,
		IdempotencyKey: idempotencyKey,
	})
	if err != nil {
		return nil, err
	}
	task := convertDbContactTask(dbTask)
	return &task, nil
}

// UpdateContactTaskStateTx is the tx-threaded variant of UpdateContactTaskState.
func (r *ContactTaskRepository) UpdateContactTaskStateTx(ctx context.Context, tx pgx.Tx, id uuid.UUID, state ContactTaskState) (*ContactTask, error) {
	if tx == nil {
		return nil, fmt.Errorf("UpdateContactTaskStateTx: %w", errNilTx)
	}
	q := db.New(tx)
	dbTask, err := q.UpdateContactTaskState(ctx, db.UpdateContactTaskStateParams{
		ID:    id,
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
	if tx == nil {
		return nil, fmt.Errorf("UpdateContactTaskMetadataTx: %w", errNilTx)
	}
	q := db.New(tx)
	var metadataBytes []byte
	if metadata != nil {
		var err error
		metadataBytes, err = json.Marshal(metadata)
		if err != nil {
			return nil, err
		}
	}
	dbTask, err := q.UpdateContactTaskMetadata(ctx, db.UpdateContactTaskMetadataParams{
		ID:       id,
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
	if tx == nil {
		return nil, fmt.Errorf("UpdateContactTaskExternalIDTx: %w", errNilTx)
	}
	q := db.New(tx)
	dbTask, err := q.UpdateContactTaskExternalID(ctx, db.UpdateContactTaskExternalIDParams{
		ID:             id,
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
	if tx == nil {
		return nil, fmt.Errorf("CompleteLiveTasksForContactTx: %w", errNilTx)
	}
	q := db.New(tx)
	rows, err := q.CompleteLiveContactTasksForContact(ctx, db.CompleteLiveContactTasksForContactParams{
		ContactID:        contactID,
		PendingTempIDKey: pendingTempIDKey,
	})
	if err != nil {
		return nil, err
	}
	refs := make([]CompletedTaskRef, 0, len(rows))
	for _, row := range rows {
		ref := CompletedTaskRef{
			ID:             row.ID,
			ExternalTaskID: row.ExternalTaskID,
			Provider:       row.Provider,
			PendingTempID:  row.PendingTempID,
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
		SourceContactID: sourceID,
		TargetContactID: targetID,
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
		ref := TransferredTaskRef{ID: row.ID, Lifecycle: row.Lifecycle}
		refs = append(refs, ref)
	}
	return refs, nil
}

// TransferObligationTasksForMergeTx moves every obligation task of the source,
// in every state, onto the target. Where the source and the target each hold
// an open obligation a participant owes on one thread, the two become one
// obligation with one task (G7a): the task due earlier stays open (the
// target's on a tie), takes the commitment basis when either was a
// commitment, and the other task closes, continued by the one kept. It
// returns refs for the tasks it closed; pendingTempIDKey is as
// CompleteLiveTasksForContactTx takes it.
func (r *ContactTaskRepository) TransferObligationTasksForMergeTx(
	ctx context.Context, tx pgx.Tx, sourceID, targetID uuid.UUID, pendingTempIDKey string,
) ([]CompletedTaskRef, error) {
	if tx == nil {
		return nil, fmt.Errorf("TransferObligationTasksForMergeTx: %w", errNilTx)
	}
	q := db.New(tx)
	pairs, err := q.ListObligationMergeCollisions(ctx, db.ListObligationMergeCollisionsParams{
		SourceContactID: sourceID,
		TargetContactID: targetID,
	})
	if err != nil {
		return nil, fmt.Errorf("list obligation merge collisions: %w", err)
	}
	var closed []CompletedTaskRef
	for _, pair := range pairs {
		source, target := convertDbContactTask(&pair.ContactTask), convertDbContactTask(&pair.ContactTask_2)
		keep, drop := target, source
		if dueBefore(source, target) {
			keep, drop = source, target
		}
		if drop.Obligation.Basis == ObligationBasisCommitment && keep.Obligation.Basis != ObligationBasisCommitment {
			if err := q.SetObligationBasis(ctx, db.SetObligationBasisParams{ID: keep.ID, Basis: &drop.Obligation.Basis}); err != nil {
				return nil, fmt.Errorf("supersede obligation %s: %w", keep.ID, err)
			}
		}
		if _, err := q.UpdateContactTaskState(ctx, db.UpdateContactTaskStateParams{ID: drop.ID, State: string(ContactTaskStateCompleted)}); err != nil {
			return nil, fmt.Errorf("close merged obligation %s: %w", drop.ID, err)
		}
		if err := q.SetObligationContinuedBy(ctx, db.SetObligationContinuedByParams{ID: drop.ID, ContinuedBy: &keep.ID}); err != nil {
			return nil, fmt.Errorf("continue merged obligation %s: %w", drop.ID, err)
		}
		pendingTempID, _ := drop.Metadata[pendingTempIDKey].(string)
		closed = append(closed, CompletedTaskRef{
			ID:             drop.ID,
			ExternalTaskID: drop.ExternalTaskID,
			Provider:       drop.Provider,
			PendingTempID:  pendingTempID,
		})
	}
	if err := q.RepointObligationContactTasksToContact(ctx, db.RepointObligationContactTasksToContactParams{
		SourceContactID: sourceID,
		TargetContactID: targetID,
	}); err != nil {
		return nil, fmt.Errorf("repoint obligation tasks: %w", err)
	}
	return closed, nil
}

// dueBefore reports whether a's due date is known and earlier than b's.
func dueBefore(a, b ContactTask) bool {
	aDue, _ := a.Metadata["due_date"].(string)
	bDue, _ := b.Metadata["due_date"].(string)
	return aDue != "" && (bDue == "" || aDue < bDue)
}

// SetContactTaskExternalIDOnlyTx persists external_task_id WITHOUT
// touching state. Used only by the close-while-pending race path in the
// cutover follow-up create worker — the row is already in state='completed'
// (an inbound arrived while step-2 was in flight) and we record the
// Todoist-returned ID so the subsequent close can target it.
func (r *ContactTaskRepository) SetContactTaskExternalIDOnlyTx(ctx context.Context, tx pgx.Tx, id uuid.UUID, externalTaskID string) error {
	if tx == nil {
		return fmt.Errorf("SetContactTaskExternalIDOnlyTx: %w", errNilTx)
	}
	q := db.New(tx)
	return q.SetContactTaskExternalIDOnly(ctx, db.SetContactTaskExternalIDOnlyParams{
		ID:             id,
		ExternalTaskID: externalTaskID,
	})
}

// CreateObligationTaskTx inserts an obligation's one task. The partial unique
// index idx_contact_task_obligation_open refuses a second open obligation for
// the same thread and owing party.
func (r *ContactTaskRepository) CreateObligationTaskTx(ctx context.Context, tx pgx.Tx, req CreateObligationTaskRequest) (*ContactTask, error) {
	if tx == nil {
		return nil, fmt.Errorf("CreateObligationTaskTx: %w", errNilTx)
	}
	metadata, err := json.Marshal(req.Metadata)
	if err != nil {
		return nil, fmt.Errorf("marshal obligation task metadata: %w", err)
	}
	dbTask, err := db.New(tx).CreateObligationTask(ctx, db.CreateObligationTaskParams{
		ContactID:          req.ContactID,
		Provider:           req.Provider,
		Kind:               req.Kind,
		State:              string(req.State),
		Metadata:           metadata,
		ObligationThreadID: &req.Obligation.ThreadID,
		ObligationDebtor:   &req.Obligation.Debtor,
		ObligationBasis:    &req.Obligation.Basis,
		ObligationCutOff:   &req.Obligation.CutOff,
	})
	if err != nil {
		return nil, err
	}
	task := convertDbContactTask(dbTask)
	return &task, nil
}

// SetObligationContinuedByTx records that the task by continues the closed
// task id's obligation.
func (r *ContactTaskRepository) SetObligationContinuedByTx(ctx context.Context, tx pgx.Tx, id, by uuid.UUID) error {
	if tx == nil {
		return fmt.Errorf("SetObligationContinuedByTx: %w", errNilTx)
	}
	return db.New(tx).SetObligationContinuedBy(ctx, db.SetObligationContinuedByParams{ID: id, ContinuedBy: &by})
}

// ListObligationTasksByThreadTx returns every obligation task on a thread, open
// or closed, oldest first.
func (r *ContactTaskRepository) ListObligationTasksByThreadTx(ctx context.Context, tx pgx.Tx, threadID uuid.UUID) ([]ContactTask, error) {
	return listObligationTasksByThread(ctx, db.New(tx), threadID)
}

// ListObligationTasksByThread is the pool-bound variant of
// ListObligationTasksByThreadTx.
func (r *ContactTaskRepository) ListObligationTasksByThread(ctx context.Context, threadID uuid.UUID) ([]ContactTask, error) {
	return listObligationTasksByThread(ctx, r.queries, threadID)
}

func listObligationTasksByThread(ctx context.Context, q db.Querier, threadID uuid.UUID) ([]ContactTask, error) {
	rows, err := q.ListObligationTasksByThread(ctx, &threadID)
	if err != nil {
		return nil, err
	}
	tasks := make([]ContactTask, 0, len(rows))
	for _, row := range rows {
		tasks = append(tasks, convertDbContactTask(row))
	}
	return tasks, nil
}

// ListOpenReplyObligationsForContactTx returns the open reply obligations on
// the threads the contact takes part in or whose task the contact holds.
func (r *ContactTaskRepository) ListOpenReplyObligationsForContactTx(ctx context.Context, tx pgx.Tx, contactID uuid.UUID) ([]ContactTask, error) {
	rows, err := db.New(tx).ListOpenReplyObligationsForContact(ctx, contactID)
	if err != nil {
		return nil, err
	}
	tasks := make([]ContactTask, 0, len(rows))
	for _, row := range rows {
		tasks = append(tasks, convertDbContactTask(row))
	}
	return tasks, nil
}
