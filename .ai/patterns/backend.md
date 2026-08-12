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

// Validation error (struct binding failed)
api.SendValidationError(c, "Invalid input", err.Error())

// Bad request (invalid parameters)
api.SendBadRequest(c, "Invalid date format")

// Not found
api.SendNotFound(c, "Contact")

// Internal error
api.SendInternalError(c, "Failed to process request")

// Conflict (duplicate)
api.SendConflict(c, "Email already exists")
```

## Repository Conversion Pattern

sqlc emits domain-shaped types directly (`uuid.UUID`, `time.Time`, `string`, and pointer variants for nullable columns) via the global override block in `backend/sqlc.yaml`, so the repository layer no longer unwraps driver types. Its remaining conversion job is mapping generated row structs onto domain structs: naming, grouping, JSONB unmarshalling.

```go
// Domain model — same shapes the generated code now uses
type Contact struct {
    ID            uuid.UUID
    FullName      string
    Email         *string  // nullable column -> pointer, nil = SQL NULL
    LastContacted *time.Time
    CreatedAt     time.Time
    UpdatedAt     time.Time
}

// Convert DB row to domain struct: mostly direct assignment
func convertDbContact(dbContact *db.Contact) Contact {
    return Contact{
        ID:            dbContact.ID,
        FullName:      dbContact.FullName,
        Email:         dbContact.Email,
        LastContacted: utcPtr(dbContact.LastContacted),
        CreatedAt:     dbContact.CreatedAt.UTC(),
        UpdatedAt:     dbContact.UpdatedAt.UTC(),
    }
}
```

The four helpers in `repository/conversions.go` cover the residual semantics:

- `deref(p)` — zero value on nil, for reads that historically flattened SQL NULL to the zero value.
- `utcPtr(t)` — UTC-normalises a nullable timestamp without changing the instant (pgx scans timestamptz into local time; `==` and `reflect.DeepEqual` are Location-sensitive).
- `nilIfEmpty(s)` — empty string to nil, so `''` never reaches a WHERE clause expecting NULL semantics.
- `jsonbOrEmpty(b)` — `'{}'` for a nil patch, preserving NOT NULL JSONB contracts.

Two things the compiler cannot check: a nullable column written from a value type silently stores the zero value instead of NULL (keep nullable params as pointers end to end), and a `db_type` in `sqlc.yaml` that sqlc does not recognise is silently ignored (the residual-selector check catches it, eyes do not). Computed/aggregate outputs that can be NULL are generated as `interface{}` — sqlc\'s static analyzer cannot type them nullable — and get a small typed adapter at the repository (see `health.go`, `whatsapp.go`).

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

    // 3. Call the repository directly — simple CRUD adds nothing but a
    //    rename through a service (rule 3, .ai/rules/core.md)
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
