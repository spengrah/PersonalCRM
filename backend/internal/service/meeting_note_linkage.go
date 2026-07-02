package service

import (
	"context"

	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// meetingNoteLinkageTargetReader adapts the calendar + phone_call
// repositories into the polymorphic LinkageTargetReader interface that
// MeetingNoteService consumes (event vs phone_call lookup by UUID).
type meetingNoteLinkageTargetReader struct {
	calendarRepo  *repository.CalendarEventRepository
	phoneCallRepo *repository.PhoneCallRepository
}

// NewLinkageTargetReader adapts the calendar + phone-call repositories into the
// polymorphic LinkageTargetReader that MeetingNoteService consumes.
func NewLinkageTargetReader(calendarRepo *repository.CalendarEventRepository, phoneCallRepo *repository.PhoneCallRepository) LinkageTargetReader {
	return &meetingNoteLinkageTargetReader{calendarRepo: calendarRepo, phoneCallRepo: phoneCallRepo}
}

// GetEventByID satisfies LinkageTargetReader.
func (r *meetingNoteLinkageTargetReader) GetEventByID(ctx context.Context, id uuid.UUID) (*repository.CalendarEvent, error) {
	return r.calendarRepo.GetByID(ctx, id)
}

// GetPhoneCallByID satisfies LinkageTargetReader.
func (r *meetingNoteLinkageTargetReader) GetPhoneCallByID(ctx context.Context, id uuid.UUID) (*repository.PhoneCall, error) {
	return r.phoneCallRepo.GetCallByID(ctx, id)
}

// GetEventByIDTx satisfies LinkageTargetReader for the tx-bound resolve flow.
func (r *meetingNoteLinkageTargetReader) GetEventByIDTx(ctx context.Context, tx pgx.Tx, id uuid.UUID) (*repository.CalendarEvent, error) {
	return r.calendarRepo.GetByIDTx(ctx, tx, id)
}

// GetPhoneCallByIDTx satisfies LinkageTargetReader for the tx-bound resolve flow.
func (r *meetingNoteLinkageTargetReader) GetPhoneCallByIDTx(ctx context.Context, tx pgx.Tx, id uuid.UUID) (*repository.PhoneCall, error) {
	return r.phoneCallRepo.GetCallByIDTx(ctx, tx, id)
}
