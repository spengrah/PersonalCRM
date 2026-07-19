package repository

import (
	"context"
	"errors"
	"strings"
	"time"

	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/identity"

	"github.com/google/uuid"
	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
)

type ContactMethodType string

const (
	ContactMethodEmail    ContactMethodType = "email"
	ContactMethodPhone    ContactMethodType = "phone"
	ContactMethodTelegram ContactMethodType = "telegram"
	ContactMethodDiscord  ContactMethodType = "discord"
	ContactMethodTwitter  ContactMethodType = "twitter"
	ContactMethodSignal   ContactMethodType = "signal"
	ContactMethodGChat    ContactMethodType = "gchat"
	ContactMethodWhatsApp ContactMethodType = "whatsapp"
)

var ContactMethodTypes = []ContactMethodType{
	ContactMethodEmail,
	ContactMethodPhone,
	ContactMethodTelegram,
	ContactMethodSignal,
	ContactMethodDiscord,
	ContactMethodTwitter,
	ContactMethodGChat,
	ContactMethodWhatsApp,
}

type ContactMethod struct {
	ID              uuid.UUID `json:"id"`
	ContactID       uuid.UUID `json:"contact_id"`
	Type            string    `json:"type"`
	Value           string    `json:"value"`
	ValueNormalized string    `json:"value_normalized,omitempty"`
	IsPrimary       bool      `json:"is_primary"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
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
	method.ValueNormalized = dbMethod.ValueNormalized

	return method
}

// NormalizeContactMethodValue normalizes a contact method value for uniqueness checks.
// Keep this aligned with identity.Normalize and migration 019 normalization rules.
func NormalizeContactMethodValue(methodType, value string) string {
	identifierType := mapMethodTypeToIdentifier(methodType)
	return identity.Normalize(value, identifierType)
}

func mapMethodTypeToIdentifier(methodType string) identity.IdentifierType {
	switch ContactMethodType(methodType) {
	case ContactMethodEmail, ContactMethodGChat:
		return identity.IdentifierTypeEmail
	case ContactMethodPhone, ContactMethodSignal:
		return identity.IdentifierTypePhone
	case ContactMethodWhatsApp:
		return identity.IdentifierTypeWhatsApp
	case ContactMethodTelegram, ContactMethodDiscord, ContactMethodTwitter:
		return identity.IdentifierTypeTelegram
	default:
		return identity.IdentifierTypeEmail
	}
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
	normalized := NormalizeContactMethodValue(req.Type, req.Value)
	dbMethod, err := r.queries.CreateContactMethod(ctx, db.CreateContactMethodParams{
		ContactID:       uuidToPgUUID(req.ContactID),
		Type:            req.Type,
		Value:           req.Value,
		ValueNormalized: normalized,
		IsPrimary:       pgtype.Bool{Bool: req.IsPrimary, Valid: true},
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
	// Type is required to normalize the updated value consistently.
	Type  string `json:"type"`
	Value string `json:"value"`
}

// UpdateContactMethod updates a contact method's value
func (r *ContactMethodRepository) UpdateContactMethod(ctx context.Context, id uuid.UUID, req UpdateContactMethodRequest) error {
	normalized := NormalizeContactMethodValue(req.Type, req.Value)
	_, err := r.queries.UpdateContactMethodValue(ctx, db.UpdateContactMethodValueParams{
		ID:              uuidToPgUUID(id),
		Value:           req.Value,
		ValueNormalized: normalized,
	})
	return err
}

// SetPrimary updates the is_primary flag for a contact method
func (r *ContactMethodRepository) SetPrimary(ctx context.Context, id uuid.UUID, isPrimary bool) error {
	return r.queries.SetContactMethodPrimary(ctx, db.SetContactMethodPrimaryParams{
		ID:        uuidToPgUUID(id),
		IsPrimary: pgtype.Bool{Bool: isPrimary, Valid: true},
	})
}

// ListCanonicalIdentifiersByType returns the deduplicated, alphabetically
// sorted set of canonical contact-method values for the given types,
// scoped to non-deleted contacts. Backs GET /api/v1/host/:id/known-
// identifiers — the daemon uses this to filter incoming Apple Messages
// senders against the user's known contact set.
//
// types is typically ["email"] or ["phone"]. Empty value_normalized
// entries (legacy rows / null normalization) are excluded.
func (r *ContactMethodRepository) ListCanonicalIdentifiersByType(
	ctx context.Context,
	types []string,
) ([]string, error) {
	return r.queries.ListCanonicalIdentifiersByType(ctx, types)
}

// ErrMethodValueConflict reports that a write violated
// idx_contact_method_unique_value — the (contact_id, type, value_normalized)
// uniqueness rule.
//
// This is the database BACKSTOP behind the fold's own conflict validation, not
// the primary mechanism. The operations endpoint computes the intended final
// state and rejects duplicates before issuing any statement, so in normal
// operation this error never fires. It exists because the fold's uniqueness key
// and the trigger that actually writes value_normalized are two different
// functions (see NormalizeContactMethodValueForUniqueness), and "the database
// can never raise this" is not a claim a two-normalizer system can honestly
// make.
//
// Classified here, in the repository, rather than at the handler: translating a
// PostgreSQL constraint name in the HTTP layer would leak repository detail
// across the service boundary, and would leave the classification testable only
// through HTTP.
var ErrMethodValueConflict = errors.New("contact method value conflicts with an existing method")

// classifyContactMethodWriteError maps a PostgreSQL unique violation on
// idx_contact_method_unique_value to ErrMethodValueConflict.
//
// The ConstraintName scoping is load-bearing: contact_method also carries
// idx_contact_method_primary, and mapping every 23505 to a value conflict would
// report "duplicate value" for what is actually a two-primaries bug. Any other
// constraint passes through unwrapped.
func classifyContactMethodWriteError(err error) error {
	if err == nil {
		return nil
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) &&
		pgErr.Code == pgerrcode.UniqueViolation &&
		pgErr.ConstraintName == "idx_contact_method_unique_value" {
		// The sentinel ONLY. PostgreSQL's Detail for this violation reads
		// "Key (contact_id, type, value_normalized)=(<uuid>, email, <value>)
		// already exists.", so returning it puts a real contact's email address
		// or phone number — plus the contact id and the column layout — into an
		// error that reaches the HTTP response body. Nothing downstream needs
		// it: the sentinel carries the meaning, the service maps it to an
		// invalid-payload error, and the handler maps that to a 400.
		//
		// The underlying pgconn error is not wrapped either: it carries the same
		// Detail string, so wrapping would reintroduce the leak through
		// errors.Unwrap and any handler that prints the chain.
		return ErrMethodValueConflict
	}
	return err
}

// InsertContactMethodWithIdentityRequest carries an explicit ID and CreatedAt so
// a row deleted during the apply stage can be reinserted with its identity
// intact. Genuinely new rows pass freshly generated values.
type InsertContactMethodWithIdentityRequest struct {
	ID        uuid.UUID
	ContactID uuid.UUID
	Type      string
	Value     string
	IsPrimary bool
	CreatedAt time.Time
}

// InsertContactMethodWithIdentity inserts a method with a caller-supplied id and
// created_at. See the query comment for why there is no ON CONFLICT clause.
func (r *ContactMethodRepository) InsertContactMethodWithIdentity(
	ctx context.Context,
	req InsertContactMethodWithIdentityRequest,
) (*ContactMethod, error) {
	dbMethod, err := r.queries.InsertContactMethodWithIdentity(ctx, db.InsertContactMethodWithIdentityParams{
		ID:        uuidToPgUUID(req.ID),
		ContactID: uuidToPgUUID(req.ContactID),
		Type:      req.Type,
		Value:     req.Value,
		IsPrimary: pgtype.Bool{Bool: req.IsPrimary, Valid: true},
		CreatedAt: timeToPgTimestamptz(&req.CreatedAt),
	})
	if err != nil {
		return nil, classifyContactMethodWriteError(err)
	}
	method := convertDbContactMethod(dbMethod)
	return &method, nil
}

// UpdateContactMethodByContact updates a row's type/value in place, scoped to
// the owning contact. Used only for rows whose (type, value_normalized) key is
// unchanged; key changes go through delete-and-reinsert.
func (r *ContactMethodRepository) UpdateContactMethodByContact(
	ctx context.Context,
	contactID, id uuid.UUID,
	methodType, value string,
) (*ContactMethod, error) {
	dbMethod, err := r.queries.UpdateContactMethodByContact(ctx, db.UpdateContactMethodByContactParams{
		ID:        uuidToPgUUID(id),
		ContactID: uuidToPgUUID(contactID),
		Type:      methodType,
		Value:     value,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, db.ErrNotFound
		}
		return nil, classifyContactMethodWriteError(err)
	}
	method := convertDbContactMethod(dbMethod)
	return &method, nil
}

// DeleteContactMethodByContact deletes one method of one contact.
func (r *ContactMethodRepository) DeleteContactMethodByContact(ctx context.Context, contactID, id uuid.UUID) error {
	return r.queries.DeleteContactMethodByContact(ctx, db.DeleteContactMethodByContactParams{
		ID:        uuidToPgUUID(id),
		ContactID: uuidToPgUUID(contactID),
	})
}

// DemoteContactMethodPrimaryByContact clears is_primary on one NAMED row.
// Scoped by method id as well as contact so it can never demote a row the
// caller did not name.
func (r *ContactMethodRepository) DemoteContactMethodPrimaryByContact(ctx context.Context, contactID, id uuid.UUID) error {
	return r.queries.DemoteContactMethodPrimaryByContact(ctx, db.DemoteContactMethodPrimaryByContactParams{
		ID:        uuidToPgUUID(id),
		ContactID: uuidToPgUUID(contactID),
	})
}

// PromoteContactMethodPrimaryByContact sets is_primary on one named row.
func (r *ContactMethodRepository) PromoteContactMethodPrimaryByContact(ctx context.Context, contactID, id uuid.UUID) error {
	err := r.queries.PromoteContactMethodPrimaryByContact(ctx, db.PromoteContactMethodPrimaryByContactParams{
		ID:        uuidToPgUUID(id),
		ContactID: uuidToPgUUID(contactID),
	})
	return classifyContactMethodWriteError(err)
}

// LookupContactMethodOwner returns the contact that owns a method id.
// Returns db.ErrNotFound when no such method exists anywhere.
//
// This distinguishes "owned by another contact" from "does not exist", which
// the caller's own pre-state cannot tell apart: the first is rejected outright
// because a method id is not a capability, while the second lets a retried
// removal succeed as a no-op.
func (r *ContactMethodRepository) LookupContactMethodOwner(ctx context.Context, id uuid.UUID) (uuid.UUID, error) {
	owner, err := r.queries.LookupContactMethodOwner(ctx, uuidToPgUUID(id))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return uuid.Nil, db.ErrNotFound
		}
		return uuid.Nil, err
	}
	if !owner.Valid {
		return uuid.Nil, db.ErrNotFound
	}
	return uuid.UUID(owner.Bytes), nil
}

// NormalizeContactMethodValueViaTrigger calls the live SQL normalization
// function. TEST-ONLY — it exists so the parity test can compare the Go mirror
// below against the function the unique index is actually enforced over.
// Production code reads the trigger's output from the stored column.
func (r *ContactMethodRepository) NormalizeContactMethodValueViaTrigger(
	ctx context.Context,
	methodType, rawValue string,
) (string, error) {
	return r.queries.NormalizeContactMethodValueViaTrigger(ctx, db.NormalizeContactMethodValueViaTriggerParams{
		MethodType: methodType,
		RawValue:   rawValue,
	})
}

// NormalizeContactMethodValueForUniqueness reproduces the SQL function
// normalize_contact_method_value (migration 022) so the operations endpoint can
// compute its uniqueness key in Go and reject duplicates before issuing any
// statement.
//
// THIS IS A DELIBERATE DUPLICATE OF SQL LOGIC. It must not be merged into
// NormalizeContactMethodValue, and must not be "simplified" to call
// identity.Normalize: those answer a different question (identity matching) and
// are provably different functions from the trigger. Two verified divergences,
// both pre-existing and both affecting the create path identically:
//
//   - Handles: the trigger strips ALL leading '@' ('^@+'); identity.Normalize
//     strips exactly one via strings.TrimPrefix.
//   - Phones: see the quirk note below.
//
// value_normalized is written by a BEFORE INSERT OR UPDATE trigger, so the
// trigger is the SOLE authority — whatever Go passes for that column is
// overwritten, and the unique index is always enforced over the trigger's
// output. A mirror that disagrees turns a deterministic 400 into a 500.
//
// QUIRK, INTENTIONALLY REPRODUCED: the trigger's leading-plus test is
// btrim(raw_value) ~ '^\\+', which in POSIX matches one or more literal
// BACKSLASHES, never a real '+'. Every '+'-prefixed number therefore falls
// through to the US-country-code branch, so '+1234567890' and '1234567890' both
// normalize to '+11234567890'. That is a real production normalization bug, and
// it is OUT OF SCOPE here: correcting it changes value_normalized for existing
// rows and needs a migration plus its own analysis of the unique index, identity
// matching, and rematch.
//
// This mirror must reproduce the behavior that ACTUALLY RUNS, quirk included. A
// mirror written against the regex's evident intent would be wrong. If the
// trigger is ever corrected, this function and its parity corpus are the
// checklist for what must move with it.
func NormalizeContactMethodValueForUniqueness(methodType, value string) string {
	// btrim() with no character set removes SPACES only — not tabs, not
	// newlines. strings.TrimSpace would trim all Unicode whitespace and
	// silently disagree with the trigger on a tab-padded value.
	trimmed := strings.Trim(value, " ")

	switch methodType {
	case string(ContactMethodEmail), string(ContactMethodGChat):
		return strings.ToLower(trimmed)

	case string(ContactMethodTelegram), string(ContactMethodTwitter), string(ContactMethodDiscord):
		// '^@+' — ALL leading '@', not just the first.
		return strings.ToLower(strings.TrimLeft(trimmed, "@"))

	case string(ContactMethodPhone), string(ContactMethodSignal), string(ContactMethodWhatsApp):
		if trimmed == "" {
			return ""
		}
		digits := digitsOnly(trimmed)
		if digits == "" {
			return ""
		}
		// The backslash quirk, reproduced exactly: '^\\+' matches leading
		// literal backslashes. A real '+' does NOT reach this branch.
		if strings.HasPrefix(trimmed, `\`) {
			return "+" + digits
		}
		if len(digits) == 10 {
			return "+1" + digits
		}
		if len(digits) == 11 && digits[0] == '1' {
			return "+" + digits
		}
		return "+" + digits

	default:
		return trimmed
	}
}

// digitsOnly mirrors regexp_replace(value, '[^0-9]', ”, 'g').
func digitsOnly(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}
