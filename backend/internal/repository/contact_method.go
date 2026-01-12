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

type ContactMethodType string

const (
	ContactMethodEmailPersonal ContactMethodType = "email_personal"
	ContactMethodEmailWork     ContactMethodType = "email_work"
	ContactMethodPhone         ContactMethodType = "phone"
	ContactMethodTelegram      ContactMethodType = "telegram"
	ContactMethodDiscord       ContactMethodType = "discord"
	ContactMethodTwitter       ContactMethodType = "twitter"
	ContactMethodSignal        ContactMethodType = "signal"
	ContactMethodGChat         ContactMethodType = "gchat"
	ContactMethodWhatsApp      ContactMethodType = "whatsapp"
)

var ContactMethodTypes = []ContactMethodType{
	ContactMethodEmailPersonal,
	ContactMethodEmailWork,
	ContactMethodPhone,
	ContactMethodTelegram,
	ContactMethodSignal,
	ContactMethodDiscord,
	ContactMethodTwitter,
	ContactMethodGChat,
	ContactMethodWhatsApp,
}

type ContactMethod struct {
	ID        uuid.UUID `json:"id"`
	ContactID uuid.UUID `json:"contact_id"`
	Type      string    `json:"type"`
	Value     string    `json:"value"`
	IsPrimary bool      `json:"is_primary"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type ContactMethodSummary struct {
	Type  string `json:"type"`
	Value string `json:"value"`
}

type CreateContactMethodRequest struct {
	ContactID uuid.UUID `json:"contact_id"`
	Type      string    `json:"type"`
	Value     string    `json:"value"`
	IsPrimary bool      `json:"is_primary"`
}

type ContactMethodRepository struct {
	queries db.Querier
}

func NewContactMethodRepository(queries db.Querier) *ContactMethodRepository {
	return &ContactMethodRepository{queries: queries}
}

func convertDbContactMethod(dbMethod *db.ContactMethod) ContactMethod {
	method := ContactMethod{
		Type:  dbMethod.Type,
		Value: dbMethod.Value,
	}

	if dbMethod.ID.Valid {
		method.ID = uuid.UUID(dbMethod.ID.Bytes)
	}
	if dbMethod.ContactID.Valid {
		method.ContactID = uuid.UUID(dbMethod.ContactID.Bytes)
	}
	if dbMethod.IsPrimary.Valid {
		method.IsPrimary = dbMethod.IsPrimary.Bool
	}
	if dbMethod.CreatedAt.Valid {
		method.CreatedAt = dbMethod.CreatedAt.Time
	}
	if dbMethod.UpdatedAt.Valid {
		method.UpdatedAt = dbMethod.UpdatedAt.Time
	}

	return method
}

func (r *ContactMethodRepository) ListContactMethodsByContact(ctx context.Context, contactID uuid.UUID) ([]ContactMethod, error) {
	dbMethods, err := r.queries.ListContactMethodsByContact(ctx, uuidToPgUUID(contactID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return []ContactMethod{}, nil
		}
		return nil, err
	}

	methods := make([]ContactMethod, len(dbMethods))
	for i, dbMethod := range dbMethods {
		methods[i] = convertDbContactMethod(dbMethod)
	}

	return methods, nil
}

func (r *ContactMethodRepository) CreateContactMethod(ctx context.Context, req CreateContactMethodRequest) (*ContactMethod, error) {
	dbMethod, err := r.queries.CreateContactMethod(ctx, db.CreateContactMethodParams{
		ContactID: uuidToPgUUID(req.ContactID),
		Type:      req.Type,
		Value:     req.Value,
		IsPrimary: pgtype.Bool{Bool: req.IsPrimary, Valid: true},
	})
	if err != nil {
		return nil, err
	}

	method := convertDbContactMethod(dbMethod)
	return &method, nil
}

func (r *ContactMethodRepository) DeleteContactMethodsByContact(ctx context.Context, contactID uuid.UUID) error {
	return r.queries.DeleteContactMethodsByContact(ctx, uuidToPgUUID(contactID))
}

// UpdateContactMethodRequest holds parameters for updating a contact method
type UpdateContactMethodRequest struct {
	Value string `json:"value"`
}

// UpdateContactMethod updates a contact method's value
func (r *ContactMethodRepository) UpdateContactMethod(ctx context.Context, id uuid.UUID, req UpdateContactMethodRequest) error {
	_, err := r.queries.UpdateContactMethodValue(ctx, db.UpdateContactMethodValueParams{
		ID:    uuidToPgUUID(id),
		Value: req.Value,
	})
	return err
}
