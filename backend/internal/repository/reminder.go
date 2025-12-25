package repository

import (
	"context"
	"time"

	"personal-crm/backend/internal/db"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
)

type Reminder struct {
	ID          uuid.UUID  `json:"id"`
	ContactID   *uuid.UUID `json:"contact_id"`
	Title       string     `json:"title"`
	Description *string    `json:"description"`
	DueDate     time.Time  `json:"due_date"`
	Completed   bool       `json:"completed"`
	CompletedAt *time.Time `json:"completed_at"`
	CreatedAt   time.Time  `json:"created_at"`
	DeletedAt   *time.Time `json:"deleted_at"`
}

type DueReminder struct {
	Reminder
	ContactName  *string `json:"contact_name"`
	ContactEmail *string `json:"contact_email"`
}

type CreateReminderRequest struct {
	ContactID   *uuid.UUID `json:"contact_id" validate:"omitempty"`
	Title       string     `json:"title" validate:"required,max=255"`
	Description *string    `json:"description" validate:"omitempty,max=1000"`
	DueDate     time.Time  `json:"due_date" validate:"required"`
}

type UpdateReminderRequest struct {
	Title       *string    `json:"title" validate:"omitempty,max=255"`
	Description *string    `json:"description" validate:"omitempty,max=1000"`
	DueDate     *time.Time `json:"due_date"`
}

type ListRemindersParams struct {
	Limit  int `json:"limit" validate:"min=1,max=100"`
	Offset int `json:"offset" validate:"min=0"`
}

type ReminderRepository struct {
	queries db.Querier
}

func NewReminderRepository(queries db.Querier) *ReminderRepository {
	return &ReminderRepository{
		queries: queries,
	}
}

// convertDbReminder converts a sqlc generated reminder to our domain model
func convertDbReminder(dbReminder *db.Reminder) Reminder {
	reminder := Reminder{
		ID:        uuid.UUID(dbReminder.ID.Bytes),
		Title:     dbReminder.Title,
		Completed: dbReminder.Completed.Bool,
		CreatedAt: dbReminder.CreatedAt.Time,
	}

	if dbReminder.ContactID.Valid {
		contactID := uuid.UUID(dbReminder.ContactID.Bytes)
		reminder.ContactID = &contactID
	}

	if dbReminder.Description.Valid {
		reminder.Description = &dbReminder.Description.String
	}

	if dbReminder.DueDate.Valid {
		reminder.DueDate = dbReminder.DueDate.Time
	}

	if dbReminder.CompletedAt.Valid {
		reminder.CompletedAt = &dbReminder.CompletedAt.Time
	}

	if dbReminder.DeletedAt.Valid {
		reminder.DeletedAt = &dbReminder.DeletedAt.Time
	}

	return reminder
}

// convertDbDueReminder converts a sqlc generated due reminder to our domain model
func convertDbDueReminder(dbReminder db.ListDueRemindersRow) DueReminder {
	due := DueReminder{
		Reminder: Reminder{
			ID:        uuid.UUID(dbReminder.ID.Bytes),
			Title:     dbReminder.Title,
			Completed: dbReminder.Completed.Bool,
			CreatedAt: dbReminder.CreatedAt.Time,
		},
	}

	if dbReminder.ContactID.Valid {
		contactID := uuid.UUID(dbReminder.ContactID.Bytes)
		due.ContactID = &contactID
	}

	if dbReminder.ContactName.Valid {
		due.ContactName = &dbReminder.ContactName.String
	}

	if dbReminder.Description.Valid {
		due.Description = &dbReminder.Description.String
	}

	if dbReminder.DueDate.Valid {
		due.DueDate = dbReminder.DueDate.Time
	}

	if dbReminder.CompletedAt.Valid {
		due.Reminder.CompletedAt = &dbReminder.CompletedAt.Time
	}

	if dbReminder.DeletedAt.Valid {
		due.Reminder.DeletedAt = &dbReminder.DeletedAt.Time
	}

	if dbReminder.ContactEmail.Valid {
		due.ContactEmail = &dbReminder.ContactEmail.String
	}

	return due
}

func (r *ReminderRepository) CreateReminder(ctx context.Context, req CreateReminderRequest) (*Reminder, error) {
	var description pgtype.Text
	if req.Description != nil {
		description = pgtype.Text{String: *req.Description, Valid: true}
	}

	var contactID pgtype.UUID
	if req.ContactID != nil {
		contactID = pgtype.UUID{Bytes: *req.ContactID, Valid: true}
	}

	dbReminder, err := r.queries.CreateReminder(ctx, db.CreateReminderParams{
		ContactID:   contactID,
		Title:       req.Title,
		Description: description,
		DueDate:     pgtype.Timestamptz{Time: req.DueDate, Valid: true},
	})
	if err != nil {
		return nil, err
	}

	reminder := convertDbReminder(dbReminder)
	return &reminder, nil
}

func (r *ReminderRepository) GetReminder(ctx context.Context, id uuid.UUID) (*Reminder, error) {
	dbReminder, err := r.queries.GetReminder(ctx, pgtype.UUID{Bytes: id, Valid: true})
	if err != nil {
		return nil, err
	}

	reminder := convertDbReminder(dbReminder)
	return &reminder, nil
}

func (r *ReminderRepository) ListReminders(ctx context.Context, params ListRemindersParams) ([]Reminder, error) {
	dbReminders, err := r.queries.ListReminders(ctx, db.ListRemindersParams{
		Limit:  int32(params.Limit),
		Offset: int32(params.Offset),
	})
	if err != nil {
		return nil, err
	}

	reminders := make([]Reminder, len(dbReminders))
	for i, dbReminder := range dbReminders {
		reminders[i] = convertDbReminder(dbReminder)
	}

	return reminders, nil
}

func (r *ReminderRepository) ListDueReminders(ctx context.Context, dueBy time.Time) ([]DueReminder, error) {
	dbReminders, err := r.queries.ListDueReminders(ctx, pgtype.Timestamptz{Time: dueBy, Valid: true})
	if err != nil {
		return nil, err
	}

	reminders := make([]DueReminder, len(dbReminders))
	for i, dbReminder := range dbReminders {
		reminders[i] = convertDbDueReminder(*dbReminder)
	}

	return reminders, nil
}

func (r *ReminderRepository) ListRemindersByContact(ctx context.Context, contactID uuid.UUID) ([]Reminder, error) {
	dbReminders, err := r.queries.ListRemindersByContact(ctx, pgtype.UUID{Bytes: contactID, Valid: true})
	if err != nil {
		return nil, err
	}

	reminders := make([]Reminder, len(dbReminders))
	for i, dbReminder := range dbReminders {
		reminders[i] = convertDbReminder(dbReminder)
	}

	return reminders, nil
}

func (r *ReminderRepository) CompleteReminder(ctx context.Context, id uuid.UUID) (*Reminder, error) {
	dbReminder, err := r.queries.CompleteReminder(ctx, pgtype.UUID{Bytes: id, Valid: true})
	if err != nil {
		return nil, err
	}

	reminder := convertDbReminder(dbReminder)
	return &reminder, nil
}

func (r *ReminderRepository) UpdateReminder(ctx context.Context, id uuid.UUID, req UpdateReminderRequest) (*Reminder, error) {
	var title string
	if req.Title != nil {
		title = *req.Title
	}

	var description pgtype.Text
	if req.Description != nil {
		description = pgtype.Text{String: *req.Description, Valid: true}
	}

	var dueDate pgtype.Timestamptz
	if req.DueDate != nil {
		dueDate = pgtype.Timestamptz{Time: *req.DueDate, Valid: true}
	}

	dbReminder, err := r.queries.UpdateReminder(ctx, db.UpdateReminderParams{
		ID:          pgtype.UUID{Bytes: id, Valid: true},
		Title:       title,
		Description: description,
		DueDate:     dueDate,
	})
	if err != nil {
		return nil, err
	}

	reminder := convertDbReminder(dbReminder)
	return &reminder, nil
}

func (r *ReminderRepository) SoftDeleteReminder(ctx context.Context, id uuid.UUID) error {
	return r.queries.SoftDeleteReminder(ctx, pgtype.UUID{Bytes: id, Valid: true})
}

func (r *ReminderRepository) HardDeleteReminder(ctx context.Context, id uuid.UUID) error {
	return r.queries.HardDeleteReminder(ctx, pgtype.UUID{Bytes: id, Valid: true})
}

func (r *ReminderRepository) CountReminders(ctx context.Context) (int64, error) {
	return r.queries.CountReminders(ctx)
}

func (r *ReminderRepository) CountDueReminders(ctx context.Context, dueBy time.Time) (int64, error) {
	return r.queries.CountDueReminders(ctx, pgtype.Timestamptz{Time: dueBy, Valid: true})
}
