# Cross-Platform Identity Matching

This document describes the identity matching system that connects external communication data to CRM contacts.

---

## Overview

When syncing data from external sources (Gmail, iMessage, Google Calendar, etc.), each message or event contains participant identifiers like email addresses, phone numbers, or platform handles. The identity matching system:

1. **Normalizes** identifiers to a canonical form
2. **Matches** them to existing CRM contacts via their contact methods
3. **Caches** matches for fast future lookups
4. **Surfaces** unmatched identities for manual review

---

## Architecture

```
External Source (Gmail, iMessage, etc.)
         │
         │ Raw identifier: "John Doe <john@example.com>"
         ▼
┌─────────────────────────────────────────────────────────┐
│                  IdentityService                        │
│                                                         │
│  1. Normalize: "john@example.com"                       │
│  2. Check cache (external_identity table)               │
│  3. Search contact_method table (if not cached)         │
│  4. Store result for future lookups                     │
└─────────────────────────────────────────────────────────┘
         │
         ▼
┌─────────────────────────────────────────────────────────┐
│               Result: MatchResult                       │
│                                                         │
│  - ContactID: uuid (or nil if unmatched)                │
│  - MatchType: exact | fuzzy | manual | unmatched        │
│  - Cached: true if from cache                           │
└─────────────────────────────────────────────────────────┘
```

---

## Two Sync Strategies

The system supports two fundamentally different sync approaches:

### 1. Contact-Driven Sync (Gmail, iMessage, Calendar)

**Discovery: DISABLED** — Only syncs data for known CRM contacts.

The sync provider first gets all CRM contacts, then queries the external source specifically for those contacts' identifiers. This avoids noise from newsletters, spam, and unknown senders.

```go
// Provider already knows john@example.com belongs to John
// because it queried Gmail: "from:john@example.com OR to:john@example.com"
result, _ := identityService.MatchOrCreate(ctx, service.MatchRequest{
    RawIdentifier:  "john@example.com",
    Type:           identity.IdentifierTypeEmail,
    Source:         "gmail",
    KnownContactID: &johnContactID,  // Skip search, just cache the mapping
})
```

**When to use:** Sources with high noise (email, messages, calendar events).

### 2. Discovery Sync (Google Contacts, iCloud Contacts)

**Discovery: ENABLED** — Syncs everything, matches what it can.

The sync provider fetches all contacts from the external source and uses identity matching to find corresponding CRM contacts. Unmatched entries are stored for manual review.

```go
// Provider doesn't know if this person exists in CRM
result, _ := identityService.MatchOrCreate(ctx, service.MatchRequest{
    RawIdentifier: "unknown@example.com",
    Type:          identity.IdentifierTypeEmail,
    Source:        "google_contacts",
    // No KnownContactID → searches contact_method table
})

if result.ContactID == nil {
    // Stored as "unmatched" for manual review via API
}
```

**When to use:** Contact directories where discovery is valuable.

---

## Identifier Normalization

All identifiers are normalized before matching to ensure consistency:

| Type | Raw Input | Normalized |
|------|-----------|------------|
| Email | `John.Doe@Example.COM` | `john.doe@example.com` |
| Phone | `(555) 123-4567` | `+15551234567` |
| Phone | `+1-555-123-4567` | `+15551234567` |
| Phone | `555.123.4567` | `+15551234567` |
| Telegram | `@JohnDoe` | `johndoe` |
| iMessage (email) | `JOHN@iCloud.COM` | `john@icloud.com` |
| iMessage (phone) | `+1 (555) 123-4567` | `+15551234567` |
| WhatsApp | `+1 555 123 4567` | `+15551234567` |

### Phone Normalization Rules

Phone numbers are normalized to E.164 format:

1. Strip all non-digit characters
2. If 10 digits without country code, assume US (`+1`)
3. If 11 digits starting with `1`, treat as US with country code
4. Otherwise, preserve the digits with `+` prefix

---

## Database Schema

### `external_identity` Table

Stores matched and unmatched identities:

```sql
CREATE TABLE external_identity (
    id UUID PRIMARY KEY,

    -- The identifier
    identifier TEXT NOT NULL,           -- Normalized value
    identifier_type TEXT NOT NULL,      -- 'email', 'phone', 'telegram', etc.
    raw_identifier TEXT,                -- Original format

    -- Source tracking
    source TEXT NOT NULL,               -- 'gmail', 'imessage', 'telegram'
    source_id TEXT,                     -- Platform-specific ID

    -- CRM contact link
    contact_id UUID REFERENCES contact(id),
    match_type TEXT,                    -- 'exact', 'fuzzy', 'manual', 'unmatched'
    match_confidence FLOAT,             -- 0.0-1.0

    -- Metadata
    display_name TEXT,                  -- Name from external source
    last_seen_at TIMESTAMPTZ,
    message_count INTEGER DEFAULT 0,

    UNIQUE (identifier, identifier_type, source)
);
```

### Indexes

```sql
-- Fast lookup during sync
CREATE INDEX idx_external_identity_lookup
    ON external_identity(identifier_type, identifier);

-- Find unmatched for review
CREATE INDEX idx_external_identity_unmatched
    ON external_identity(contact_id) WHERE contact_id IS NULL;

-- Find identities for a contact
CREATE INDEX idx_external_identity_contact
    ON external_identity(contact_id) WHERE contact_id IS NOT NULL;
```

---

## API Endpoints

All endpoints require the `EnableExternalSync` feature flag.

### List Unmatched Identities

```
GET /api/v1/identities/unmatched?page=1&limit=50
```

Returns identities that couldn't be matched to CRM contacts, sorted by message count (most active first).

### Get Identity

```
GET /api/v1/identities/:id
```

### Link Identity to Contact

```
POST /api/v1/identities/:id/link
Content-Type: application/json

{"contact_id": "uuid"}
```

Manually links an unmatched identity to a contact. Sets `match_type` to `manual`.

### Unlink Identity

```
POST /api/v1/identities/:id/unlink
```

Removes the contact link. Useful if an identity was incorrectly matched.

### Delete Identity

```
DELETE /api/v1/identities/:id
```

### List Identities for Contact

```
GET /api/v1/contacts/:id/identities
```

Returns all external identities linked to a specific contact.

---

## Usage Patterns

### During Sync (Contact-Driven)

```go
// Gmail sync provider
func (p *GmailProvider) processMessage(ctx context.Context, msg Message, contact Contact) {
    // We already know which contact this is for
    result, err := p.identityService.MatchOrCreate(ctx, service.MatchRequest{
        RawIdentifier:  msg.FromAddress,
        Type:           identity.IdentifierTypeEmail,
        Source:         "gmail",
        DisplayName:    &msg.FromName,
        KnownContactID: &contact.ID,  // Fast path
    })

    // Update last_contacted, create interaction, etc.
    p.contactService.UpdateLastContacted(ctx, contact.ID, msg.SentAt)
}
```

### During Sync (Discovery)

```go
// Google Contacts sync provider
func (p *GoogleContactsProvider) processContact(ctx context.Context, gContact GoogleContact) {
    for _, email := range gContact.Emails {
        result, err := p.identityService.MatchOrCreate(ctx, service.MatchRequest{
            RawIdentifier: email.Address,
            Type:          identity.IdentifierTypeEmail,
            Source:        "google_contacts",
            DisplayName:   &gContact.Name,
            // No KnownContactID → will search for match
        })

        if result.ContactID != nil {
            // Found match! Can enrich CRM contact with Google data
        } else {
            // Stored as unmatched for manual review
        }
    }
}
```

### Manual Linking (API)

```bash
# List unmatched identities
curl http://localhost:8080/api/v1/identities/unmatched

# Link an identity to a contact
curl -X POST http://localhost:8080/api/v1/identities/abc-123/link \
  -H "Content-Type: application/json" \
  -d '{"contact_id": "def-456"}'
```

---

## Identifier Types

| Type | Description | Normalization |
|------|-------------|---------------|
| `email` | Standard email address | Lowercase, trim |
| `phone` | Phone number | E.164 format |
| `telegram` | Telegram handle | Remove @, lowercase |
| `imessage_email` | iMessage via email | Lowercase, trim |
| `imessage_phone` | iMessage via phone | E.164 format |
| `whatsapp` | WhatsApp number | E.164 format |

### Type Mapping to Contact Methods

When searching the `contact_method` table, identifier types map to contact method types:

| Identifier Type | Contact Method Types |
|-----------------|---------------------|
| `email` | `email` |
| `phone` | `phone` |
| `telegram` | `telegram` |
| `imessage_email` | `email` |
| `imessage_phone` | `phone` |
| `whatsapp` | `whatsapp`, `phone` |

---

## Performance Considerations

### Caching

The `external_identity` table acts as a cache. After the first match, subsequent lookups for the same identifier+source are O(1) index lookups instead of searching the `contact_method` table.

### Contact-Driven Efficiency

For sources like Gmail with high message volumes, using `KnownContactID` avoids:
- Searching `contact_method` table
- Handling ambiguous matches (multiple contacts with same email)
- Processing spam/unknown senders

### Batch Operations

Use `BulkLinkIdentities` for linking multiple identities at once:

```go
err := identityService.BulkLinkIdentities(ctx,
    []uuid.UUID{id1, id2, id3},
    contactID,
)
```

---

## Testing

### Unit Tests

See `backend/internal/identity/normalize_test.go` for normalization tests:

```bash
go test ./internal/identity/... -v
```

### Integration Tests

Identity matching integration requires a running database. See the test infrastructure in `backend/tests/`.

---

## Related Documentation

- [External Sync Infrastructure](../docs/PLAN.md) - Sync framework overview
- [Contact Methods](../docs/contact-methods-plan.md) - How contact methods are stored
- [Architecture](../.ai/architecture.md) - System architecture
- [Patterns](../.ai/patterns.md) - Identity matching code patterns
