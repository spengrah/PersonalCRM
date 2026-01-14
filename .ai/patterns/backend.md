# Backend Patterns

Reusable Go patterns for consistency across the backend codebase.

## Error Response Pattern

Use the standardized API response helpers in all handlers:

```go
// Success
api.SendSuccess(c, http.StatusOK, data, nil)

// Success with metadata (pagination)
api.SendSuccess(c, http.StatusOK, data, &api.Meta{
    Pagination: &api.PaginationMeta{
        Page:  1,
        Limit: 20,
        Total: 100,
    },
})

// Validation error
api.SendValidationError(c, "Invalid input", err.Error())

// Not found
api.SendNotFound(c, "Contact")

// Internal error
api.SendInternalError(c, "Failed to process request")

// Conflict (duplicate)
api.SendConflict(c, "Email already exists")
```

## Repository Conversion Pattern

Convert between sqlc-generated DB types and clean domain types:

```go
// Domain model (no pgtype, clean types)
type Contact struct {
    ID            uuid.UUID
    FullName      string
    Email         *string  // nullable
    LastContacted *time.Time
    CreatedAt     time.Time
    UpdatedAt     time.Time
}

// Convert DB type to domain type
func convertDbContact(dbContact *db.Contact) Contact {
    contact := Contact{
        ID:       uuid.UUID(dbContact.ID.Bytes),
        FullName: dbContact.FullName,
    }

    // Handle nullable string - copy value before taking address
    if dbContact.Email.Valid {
        emailStr := dbContact.Email.String
        contact.Email = &emailStr
    }

    // Handle nullable time - copy value before taking address
    if dbContact.LastContacted.Valid {
        lastContactedTime := dbContact.LastContacted.Time
        contact.LastContacted = &lastContactedTime
    }

    // Handle timestamps
    if dbContact.CreatedAt.Valid {
        contact.CreatedAt = dbContact.CreatedAt.Time
    }

    if dbContact.UpdatedAt.Valid {
        contact.UpdatedAt = dbContact.UpdatedAt.Time
    }

    return contact
}

// Helper: string pointer to pgtype.Text
func stringToNullString(s *string) pgtype.Text {
    if s == nil {
        return pgtype.Text{Valid: false}
    }
    return pgtype.Text{String: *s, Valid: true}
}

// Helper: uuid.UUID to pgtype.UUID
func uuidToPgUUID(id uuid.UUID) pgtype.UUID {
    return pgtype.UUID{
        Bytes: [16]byte(id),
        Valid: true,
    }
}

// Helper: time pointer to pgtype.Timestamptz
func timeToNullTime(t *time.Time) pgtype.Timestamptz {
    if t == nil {
        return pgtype.Timestamptz{Valid: false}
    }
    return pgtype.Timestamptz{Time: *t, Valid: true}
}
```

## Handler Validation Pattern

```go
func (h *ContactHandler) CreateContact(c *gin.Context) {
    // 1. Parse request body
    var req CreateContactRequest
    if err := c.ShouldBindJSON(&req); err != nil {
        api.SendValidationError(c, "Invalid request body", err.Error())
        return
    }

    // 2. Additional validation (if needed)
    if req.Email != nil && !isValidEmail(*req.Email) {
        api.SendValidationError(c, "Invalid email format", "")
        return
    }

    // 3. Call repository/service
    contact, err := h.repo.CreateContact(c.Request.Context(), repository.CreateContactRequest{
        FullName: req.FullName,
        Email:    req.Email,
    })
    if err != nil {
        api.SendInternalError(c, "Failed to create contact")
        return
    }

    // 4. Convert to response model
    response := convertToContactResponse(contact)

    // 5. Send response
    api.SendSuccess(c, http.StatusCreated, response, nil)
}
```

## Identity Matching Pattern

Use `IdentityService.MatchOrCreate` to match external identifiers to CRM contacts:

**Contact-driven sync (Gmail, iMessage, Calendar):**
```go
// When you already know which contact the identifier belongs to
result, err := identityService.MatchOrCreate(ctx, service.MatchRequest{
    RawIdentifier:  "john@example.com",
    Type:           identity.IdentifierTypeEmail,
    Source:         "gmail",
    DisplayName:    &senderName,
    KnownContactID: &contactID,  // Fast path: skips search
})
```

**Discovery sync (Google Contacts, iCloud Contacts):**
```go
// When you need to find if an identifier matches any CRM contact
result, err := identityService.MatchOrCreate(ctx, service.MatchRequest{
    RawIdentifier: "unknown@example.com",
    Type:          identity.IdentifierTypeEmail,
    Source:        "google_contacts",
    DisplayName:   &contactName,
    // No KnownContactID → searches contact_method table
})

if result.ContactID != nil {
    // Matched to CRM contact
} else {
    // Stored as "unmatched" for manual review
}
```

**Available identifier types:**
- `identity.IdentifierTypeEmail`
- `identity.IdentifierTypePhone`
- `identity.IdentifierTypeTelegram`
- `identity.IdentifierTypeIMessageEmail`
- `identity.IdentifierTypeIMessagePhone`
- `identity.IdentifierTypeWhatsApp`

**Normalization is automatic** — the service normalizes all identifiers before matching.

## Fuzzy Matching Utilities

Use shared helpers in `backend/internal/matching` instead of re-implementing normalization or scoring:

- `matching.ImportConfig` and `matching.CalendarConfig` define weights and thresholds.
- `matching.NormalizeEmail` handles lowercasing + trim.
- `matching.NormalizePhoneE164` is for identity matching and storage.
- `matching.NormalizePhoneLoose` is for heuristic import matching.

## Error Wrapping Pattern

```go
// Always wrap errors with context
if err != nil {
    return fmt.Errorf("create contact: %w", err)
}

// For multiple operations
contact, err := r.contactRepo.GetContact(ctx, id)
if err != nil {
    return nil, fmt.Errorf("get contact: %w", err)
}

reminder, err := r.reminderRepo.CreateReminder(ctx, req)
if err != nil {
    return nil, fmt.Errorf("create reminder: %w", err)
}
```

## Context Timeout Pattern

```go
// For database operations with timeout
ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
defer cancel()

contact, err := repo.GetContact(ctx, id)
if err != nil {
    // Handle error
}
```

## Time Handling Pattern

**Always use accelerated time for testing:**

```go
// ❌ WRONG
now := time.Now()

// ✅ CORRECT
import "personal-crm/backend/internal/accelerated"

now := accelerated.GetCurrentTime()
```
