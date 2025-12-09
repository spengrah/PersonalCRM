package repository

import (
	"context"
	"errors"
	"time"

	"personal-crm/backend/internal/db"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

type TimeEntry struct {
	ID              uuid.UUID  `json:"id"`
	Description     string     `json:"description"`
	Project         *string    `json:"project,omitempty"`
	ContactID       *uuid.UUID `json:"contact_id,omitempty"`
	StartTime       time.Time  `json:"start_time"`
	EndTime         *time.Time `json:"end_time,omitempty"`
	DurationMinutes *int32     `json:"duration_minutes,omitempty"`
	CreatedAt       time.Time  `json:"created_at"`
	UpdatedAt       time.Time  `json:"updated_at"`
}

type TimeEntryStats struct {
	TotalEntries int64 `json:"total_entries"`
	TotalMinutes int64 `json:"total_minutes"`
	TodayMinutes int64 `json:"today_minutes"`
	WeekMinutes  int64 `json:"week_minutes"`
	MonthMinutes int64 `json:"month_minutes"`
}

type CreateTimeEntryRequest struct {
	Description     string     `json:"description" validate:"required,max=500"`
	Project         *string    `json:"project,omitempty" validate:"omitempty,max=100"`
	ContactID       *uuid.UUID `json:"contact_id,omitempty"`
	StartTime       time.Time  `json:"start_time" validate:"required"`
	EndTime         *time.Time `json:"end_time,omitempty"`
	DurationMinutes *int32    `json:"duration_minutes,omitempty"`
}

type UpdateTimeEntryRequest struct {
	Description     *string    `json:"description,omitempty" validate:"omitempty,max=500"`
	Project         *string    `json:"project,omitempty" validate:"omitempty,max=100"`
	ContactID       *uuid.UUID `json:"contact_id,omitempty"`
	EndTime         *time.Time `json:"end_time,omitempty"`
	DurationMinutes *int32    `json:"duration_minutes,omitempty"`
}

type ListTimeEntriesParams struct {
	Limit  int `json:"limit" validate:"min=1,max=100"`
	Offset int `json:"offset" validate:"min=0"`
}

type TimeEntryRepository struct {
	queries db.Querier
}

func NewTimeEntryRepository(queries db.Querier) *TimeEntryRepository {
	return &TimeEntryRepository{
		queries: queries,
	}
}

// convertDbTimeEntry converts a sqlc generated time entry to our domain model
func convertDbTimeEntry(dbEntry *db.TimeEntry) TimeEntry {
	entry := TimeEntry{
		ID:          uuid.UUID(dbEntry.ID.Bytes),
		Description: dbEntry.Description,
		StartTime:   dbEntry.StartTime.Time,
		CreatedAt:   dbEntry.CreatedAt.Time,
		UpdatedAt:   dbEntry.UpdatedAt.Time,
	}

	if dbEntry.Project.Valid {
		entry.Project = &dbEntry.Project.String
	}

	if dbEntry.ContactID.Valid {
		contactID := uuid.UUID(dbEntry.ContactID.Bytes)
		entry.ContactID = &contactID
	}

	if dbEntry.EndTime.Valid {
		entry.EndTime = &dbEntry.EndTime.Time
	}

	if dbEntry.DurationMinutes.Valid {
		entry.DurationMinutes = &dbEntry.DurationMinutes.Int32
	}

	return entry
}

func (r *TimeEntryRepository) CreateTimeEntry(ctx context.Context, req CreateTimeEntryRequest) (*TimeEntry, error) {
	var project pgtype.Text
	if req.Project != nil {
		project = pgtype.Text{String: *req.Project, Valid: true}
	}

	var contactID pgtype.UUID
	if req.ContactID != nil {
		contactID = pgtype.UUID{Bytes: *req.ContactID, Valid: true}
	}

	var endTime pgtype.Timestamptz
	if req.EndTime != nil {
		endTime = pgtype.Timestamptz{Time: *req.EndTime, Valid: true}
	}

	var durationMinutes pgtype.Int4
	if req.DurationMinutes != nil {
		durationMinutes = pgtype.Int4{Int32: *req.DurationMinutes, Valid: true}
	}

	dbEntry, err := r.queries.CreateTimeEntry(ctx, db.CreateTimeEntryParams{
		Description:     req.Description,
		Project:         project,
		ContactID:       contactID,
		StartTime:       pgtype.Timestamptz{Time: req.StartTime, Valid: true},
		EndTime:         endTime,
		DurationMinutes: durationMinutes,
	})
	if err != nil {
		return nil, err
	}

	entry := convertDbTimeEntry(dbEntry)
	return &entry, nil
}

func (r *TimeEntryRepository) GetTimeEntry(ctx context.Context, id uuid.UUID) (*TimeEntry, error) {
	dbEntry, err := r.queries.GetTimeEntry(ctx, pgtype.UUID{Bytes: id, Valid: true})
	if err != nil {
		return nil, err
	}

	entry := convertDbTimeEntry(dbEntry)
	return &entry, nil
}

func (r *TimeEntryRepository) ListTimeEntries(ctx context.Context, params ListTimeEntriesParams) ([]TimeEntry, error) {
	dbEntries, err := r.queries.ListTimeEntries(ctx, db.ListTimeEntriesParams{
		Limit:  int32(params.Limit),
		Offset: int32(params.Offset),
	})
	if err != nil {
		return nil, err
	}

	entries := make([]TimeEntry, len(dbEntries))
	for i, dbEntry := range dbEntries {
		entries[i] = convertDbTimeEntry(dbEntry)
	}

	return entries, nil
}

func (r *TimeEntryRepository) ListTimeEntriesByDateRange(ctx context.Context, start, end time.Time) ([]TimeEntry, error) {
	dbEntries, err := r.queries.ListTimeEntriesByDateRange(ctx, db.ListTimeEntriesByDateRangeParams{
		StartTime:   pgtype.Timestamptz{Time: start, Valid: true},
		StartTime_2: pgtype.Timestamptz{Time: end, Valid: true},
	})
	if err != nil {
		return nil, err
	}

	entries := make([]TimeEntry, len(dbEntries))
	for i, dbEntry := range dbEntries {
		entries[i] = convertDbTimeEntry(dbEntry)
	}

	return entries, nil
}

func (r *TimeEntryRepository) ListTimeEntriesByContact(ctx context.Context, contactID uuid.UUID) ([]TimeEntry, error) {
	dbEntries, err := r.queries.ListTimeEntriesByContact(ctx, pgtype.UUID{Bytes: contactID, Valid: true})
	if err != nil {
		return nil, err
	}

	entries := make([]TimeEntry, len(dbEntries))
	for i, dbEntry := range dbEntries {
		entries[i] = convertDbTimeEntry(dbEntry)
	}

	return entries, nil
}

func (r *TimeEntryRepository) GetRunningTimeEntry(ctx context.Context) (*TimeEntry, error) {
	dbEntry, err := r.queries.GetRunningTimeEntry(ctx)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, db.ErrNotFound
		}
		return nil, err
	}

	entry := convertDbTimeEntry(dbEntry)
	return &entry, nil
}

func (r *TimeEntryRepository) UpdateTimeEntry(ctx context.Context, id uuid.UUID, req UpdateTimeEntryRequest) (*TimeEntry, error) {
	// Fetch existing entry to get current description if not provided
	var description string
	if req.Description != nil {
		description = *req.Description
	} else {
		existing, err := r.queries.GetTimeEntry(ctx, pgtype.UUID{Bytes: id, Valid: true})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, db.ErrNotFound
			}
			return nil, err
		}
		description = existing.Description
	}

	var project pgtype.Text
	if req.Project != nil {
		project = pgtype.Text{String: *req.Project, Valid: true}
	}

	var contactID pgtype.UUID
	if req.ContactID != nil {
		contactID = pgtype.UUID{Bytes: *req.ContactID, Valid: true}
	}

	var endTime pgtype.Timestamptz
	if req.EndTime != nil {
		endTime = pgtype.Timestamptz{Time: *req.EndTime, Valid: true}
	}

	var durationMinutes pgtype.Int4
	if req.DurationMinutes != nil {
		durationMinutes = pgtype.Int4{Int32: *req.DurationMinutes, Valid: true}
	}

	dbEntry, err := r.queries.UpdateTimeEntry(ctx, db.UpdateTimeEntryParams{
		ID:              pgtype.UUID{Bytes: id, Valid: true},
		Description:     description,
		Project:         project,
		ContactID:       contactID,
		EndTime:         endTime,
		DurationMinutes: durationMinutes,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, db.ErrNotFound
		}
		return nil, err
	}

	entry := convertDbTimeEntry(dbEntry)
	return &entry, nil
}

func (r *TimeEntryRepository) DeleteTimeEntry(ctx context.Context, id uuid.UUID) error {
	return r.queries.DeleteTimeEntry(ctx, pgtype.UUID{Bytes: id, Valid: true})
}

func (r *TimeEntryRepository) GetTimeEntryStats(ctx context.Context) (*TimeEntryStats, error) {
	stats, err := r.queries.GetTimeEntryStats(ctx)
	if err != nil {
		return nil, err
	}

	// Convert interface{} to int64 (PostgreSQL returns numeric as interface{})
	totalMinutes, _ := stats.TotalMinutes.(int64)
	todayMinutes, _ := stats.TodayMinutes.(int64)
	weekMinutes, _ := stats.WeekMinutes.(int64)
	monthMinutes, _ := stats.MonthMinutes.(int64)

	return &TimeEntryStats{
		TotalEntries: stats.TotalEntries,
		TotalMinutes: totalMinutes,
		TodayMinutes: todayMinutes,
		WeekMinutes:  weekMinutes,
		MonthMinutes: monthMinutes,
	}, nil
}

