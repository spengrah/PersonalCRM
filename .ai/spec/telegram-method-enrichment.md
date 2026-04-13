# Telegram Method Enrichment on Link and Auto-Match — Spec

**Issue:** none (bug discovered in session; no dedicated issue filed)
**Related:** #272 (username-name match), #97 (import/link method selection UI), #182 (rematch-service, parallel work)
**Status:** Draft v5 (Codex round 4 import-path typo fixed)
**Branch:** `feat/telegram-method-enrichment`
**Last Updated:** 2026-04-13

---

## 1. Overview

### What

When a Telegram `external_contact` (or identity-matched peer) is bound to a CRM contact — whether via the user-initiated link flow (`POST /imports/:id/link`) or the identity-based auto-match in the Telegram peer matcher — add the peer's Telegram `@username` as a `contact_method` on the bound CRM contact.

Today this only happens on the "import as new contact" path. The two bind-to-existing paths silently drop the Telegram handle.

### Why

Discovered during session investigation of contact `ea365ac2-d337-470f-b3c9-77bb03b75301` ("Jack Laing"). His `external_contact` (`source_id=479625218`, `display_name="Jackal"`, `metadata.username="@JackALaing"`) was `match_status='matched'` with `crm_contact_id` set, and an `external_identity` row existed linking `jackalaing` to him — but no `contact_method` row of type `telegram`. The Telegram handle was invisible on his contact page.

Root cause (three gaps):

1. `service/enrichment.go` — `enrichContactMethods` and `enrichContactMethodsWithSelections` build their lookup maps only from `external.Emails` and `external.Phones`. In the Selections variant, telegram selections sent by the frontend fail the `externalValues` validation at line 229 and are swallowed by `logger.Warn` in `LinkContact` (`api/handlers/import.go:564`), so the link succeeds but telegram is silently dropped.
2. `telegram/matcher.go` — `MatchPeer` (after identity match) calls `markExternalContactMatched`, which only updates `external_contact.match_status` and **early-returns on two conditions** — no existing `external_contact` row (below-threshold peers have none) and already-matched/imported status (no repair for past matches). Neither path calls any method-level enrichment.
3. `api/handlers/import.go` — `buildMethodsAuto` (lines 470-479) already extracts `metadata.username` correctly on the import-new path. The logic exists; it's just not reused.

### Key Decisions

| Decision | Choice | Rationale |
|---|---|---|
| Shared helper | Package-level function `BuildMethodsFromExternal(*repository.ExternalContact) []service.ContactMethodInput` in a new file `backend/internal/service/external_methods.go` | Single source of truth; `buildMethodsAuto` becomes a one-line wrapper; both enrichment paths call it; future sources (discord/whatsapp/etc.) plug in here |
| Telegram in auto mode (`enrichContactMethods`) | Auto-add telegram if helper surfaces one and no existing telegram method with matching normalized value | Consistent with emails/phones auto behavior: fill-in-the-blanks, never overwrite |
| Telegram in selections mode (`enrichContactMethodsWithSelections`) | Build both `existingNormalized` AND the external-value-admission lookup as type-scoped normalized keys. Selection's `OriginalValue` is compared via the same `methodDedupKey`, so `"@JackALaing"` and `"JackALaing"` both match. Stored `contact_method.value` is canonicalized (strips leading `@` for telegram handles) before insert, matching import-new behavior | User can send `@handle` or bare handle from frontend; both admit. Storage stays in canonical bare form. Dedup key matches the DB unique index shape |
| Matcher → enrichment (placement) | New matcher helper `ensureMethodsOnMatch(ctx, contactID, peerUserID, peerUsername)` called **from `MatchPeer` after each successful username-match and phone-match path** (not inside `markExternalContactMatched`) | `markExternalContactMatched` early-returns when no `external_contact` row exists (below-threshold peers) and when status is already `matched`/`imported` (no repair path). Codex-round-1 finding. `MatchPeer` is the orchestration point that reaches every matched peer |
| Matcher data source | `ensureMethodsOnMatch` reads the persisted `external_contact` if present, then **merges** the current-message `peerUsername` over any `metadata.username` from storage (current wins, because it's freshest). If no persisted row exists, synthesizes a minimal `*repository.ExternalContact` from peer fields | Stored `external_contact` may pre-date username capture or lag behind a Telegram handle rename. The current message is authoritative for the peer's live username — merging ensures we add the right telegram method in all three drift cases. Audit `external_contact_id` still references the persisted row when one exists |
| Narrow enrichment API | `EnrichmentService.SyncMethodsFromExternal(ctx, crmContactID, *ExternalContact)` — delegates to the refactored `enrichContactMethods`; does NOT overwrite `profile_photo`, `birthday`, `location`, `full_name`, `cadence` | Auto-match fires silently on every peer; narrow scope prevents rewriting profile fields behind the user's back |
| Google Contacts auto-match | **Unchanged** — keeps calling full `EnrichContactFromExternal` | Google contacts carry real profile data (not auto-discovered peers); current behavior is correct. Benefits from the shared helper for email/phone; telegram clause is inert since Google does not populate `metadata.username` |
| Duplicate-on-race handling | Treat `pgconn.PgError.Code == "23505"` (unique violation on `contact_method_unique_value`) as success; log at debug | Two concurrent MatchPeer calls for the same peer could race; idempotency must win over warning spam |
| Dedup-key shape | `type + ":" + identity.Normalize(value, mapMethodTypeToIdentifier(type))` in both enrichment paths | Matches the DB unique index `(contact_id, type, value_normalized)` exactly. Value-only keys would falsely dedupe cross-type collisions (e.g., `telegram:foo` vs `twitter:foo`, which normalize to the same handle but live in different type buckets) |
| Error handling in matcher | `ensureMethodsOnMatch` errors are logged at warn level; do NOT fail `MatchPeer` or the match bind | Match integrity > enrichment completeness; matches existing `logger.Warn` pattern elsewhere |
| `recordEnrichment` audit | Every telegram method added via any path writes a `contact_enrichment` row. `recordEnrichment` is updated to pass `ExternalContactID=nil` when `external.ID == uuid.Nil`, so matcher's synthesized `ExternalContact` yields SQL NULL (not a zero-UUID) in the audit row | Audit consistency; distinguishes synthesized (no persisted row) from persisted sources |
| `conflictResolutions` dead param | Leave in place | Separate refactor; out of scope |
| Backfill script | **Not included** | User is running one-off SQL backfill separately after fix lands + after #182 rematch-service finishes |
| Frontend changes | **None** — `import-link-modal.tsx` already pre-selects telegram from `metadata.username`; `SelectedMethodInput` validator already accepts `telegram` | Exploration confirmed full frontend wiring exists; only backend enrichment was broken |

### Non-Goals

- Rematching historical messages/events when a new contact_method is added — that's #182 (rematch-service).
- Handling discord/twitter/signal/whatsapp metadata extraction. Shared helper is structured so they can be added with a one-line addition each when a source populates them.
- Changing Google Contacts auto-match to the narrow enrichment variant.
- Removing the dead `conflictResolutions` parameter.
- Adding contact_method for peer `phone` during phone-match (phones are already on the contact — that's what matched).
- E2E tests (frontend already wires telegram selections; exploration confirmed).
- A user-triggered "ensure methods on already-matched contacts" button (covered by user's separate backfill SQL).

---

## 2. Functional Specification

### 2.1 User-Visible Behavior

1. **User-initiated link with telegram selected:** Import UI → "Link to Existing" → pick contact → leave telegram checked → confirm. Target contact's detail page shows new telegram `contact_method` row rendered as `@JackALaing`.
2. **User deselects telegram in picker:** Same flow, telegram unchecked. No telegram `contact_method` is added. External_contact is still bound, external_identity still linked.
3. **Link without any selections (auto fallback path):** `POST /imports/:id/link` with empty `selected_methods` and no cadence/name overrides → calls `EnrichContactFromExternal` (auto mode). Telegram method IS auto-added from `metadata.username`.
4. **Auto-match via identity, above-threshold peer:** Telegram message arrives from peer whose `@username` matches an existing CRM contact (via #272 name match, existing `external_identity`, or another path). Matcher binds. Target contact gets telegram `contact_method` auto-added. Profile fields unchanged.
5. **Auto-match via identity, below-threshold peer (no persisted `external_contact`):** Same as above. Method is added from synthesized peer data (no `external_contact` row needed).
6. **Re-match (matcher runs again for same peer):** No duplicate `contact_method` — dedup via normalized value; PG unique-violation on race is treated as success.

### 2.2 Acceptance Criteria

- [ ] `POST /imports/:id/link` with `selected_methods=[{type:"telegram", original_value:"JackALaing"}]` produces exactly one `contact_method` row: `type='telegram'`, `value='JackALaing'`, `value_normalized='jackalaing'`, `is_primary=false`. A matching `contact_enrichment` audit row is written.
- [ ] `POST /imports/:id/link` with `selected_methods=[{type:"telegram", original_value:"@JackALaing"}]` (leading `@` preserved from frontend) results in the same row — `value='JackALaing'` (bare), matching `buildMethodsAuto` behavior today.
- [ ] `POST /imports/:id/link` with no telegram entry in `selected_methods` does NOT create a telegram `contact_method`.
- [ ] `POST /imports/:id/link` when the target already has a matching-normalized telegram method is a no-op (no duplicate, no error, no warn).
- [ ] `POST /imports/:id/link` with empty `selected_methods`, no cadence, no name overrides → fallback `EnrichContactFromExternal` auto-adds telegram from `metadata.username` if present.
- [ ] `PeerMatcher.MatchPeer` matching a peer via username — when `external_contact` exists for that peer — produces a telegram `contact_method` on the bound contact, sourced from `external_contact.metadata.username`.
- [ ] `PeerMatcher.MatchPeer` matching a below-threshold peer via username (no `external_contact` row) — produces a telegram `contact_method` using the synthesized `ExternalContact`.
- [ ] `PeerMatcher.MatchPeer` matching via phone when `peerUsername` is present — produces a telegram `contact_method`.
- [ ] `PeerMatcher.MatchPeer` matching via phone when `peerUsername` is nil/empty — does NOT produce a telegram `contact_method` (nothing to add).
- [ ] After matcher enrichment, bound CRM contact's `profile_photo`, `birthday`, `location`, `full_name`, `cadence` are unchanged.
- [ ] `contact_enrichment` audit row is present after matcher enrichment — `external_contact_id` populated when a persisted row exists, NULL when synthesized.
- [ ] `ensureMethodsOnMatch` error (simulated via a failing `methodRepo`) does NOT fail `MatchPeer` — `MatchPeer` returns the matched `ContactID` normally, a warn is logged.
- [ ] Running `MatchPeer` twice for the same peer yields one `contact_method` (idempotency). A concurrent-insert PG `23505` unique violation is treated as success, not as a warn.
- [ ] `buildMethodsAuto` in `import.go` is deleted; its caller uses `service.BuildMethodsFromExternal`. Import-new behavior is byte-identical.
- [ ] Google Contacts auto-match (`google/contacts.go:370,399`) continues to pass existing tests.
- [ ] `backend/internal/telegram/matcher_test.go` tests that use struct-literal `PeerMatcher` construction with only a subset of fields — either (a) don't trigger the new enrichment code path, or (b) are updated with a mock `contactEnricher`. Nil-safety in the matcher ensures no panic.
- [ ] `backend/tests/telegram_discovery_upsert_test.go:363` calling `tgpkg.NewPeerMatcher` with the old signature is updated to the new signature.

---

## 3. Technical Specification

### 3.1 Shared Helper — `BuildMethodsFromExternal`

**New file:** `backend/internal/service/external_methods.go`

```go
package service

import (
    "strings"

    "personal-crm/backend/internal/repository"
)

// BuildMethodsFromExternal converts an ExternalContact into the canonical
// list of ContactMethodInput entries. Single source of truth for translating
// source-specific fields (emails, phones, metadata.username) into the shape
// the contact / enrichment services consume.
//
// Callers are responsible for deduplicating against existing contact methods
// on the target contact — this function returns everything the external
// contact carries, in a stable order.
func BuildMethodsFromExternal(external *repository.ExternalContact) []ContactMethodInput {
    if external == nil {
        return nil
    }
    methods := make([]ContactMethodInput, 0, len(external.Emails)+len(external.Phones)+1)

    for _, email := range external.Emails {
        methods = append(methods, ContactMethodInput{Type: "email", Value: email.Value})
    }
    for _, phone := range external.Phones {
        methods = append(methods, ContactMethodInput{Type: "phone", Value: phone.Value})
    }

    if external.Source == "telegram" {
        if username, ok := external.Metadata["username"].(string); ok {
            canonical := canonicalizeMethodValue("telegram", username)
            if canonical != "" {
                methods = append(methods, ContactMethodInput{Type: "telegram", Value: canonical})
            }
        }
    }

    return methods
}
```

**Normalization:** `Value` is the canonicalized handle (stripped `@`, trimmed whitespace, case preserved). `canonicalizeMethodValue` (defined in §3.3 Change D) is the single source of truth for that canonicalization — used here for the auto path AND by `enrichContactMethodsWithSelections` for the selections path. DB trigger (migration 021) lowercases into `value_normalized`. Unique index on `(contact_id, type, value_normalized)` catches case-variant duplicates.

### 3.2 Refactor — `enrichContactMethods` (auto mode)

**File:** `backend/internal/service/enrichment.go` (modify)

```go
func (s *EnrichmentService) enrichContactMethods(
    ctx context.Context,
    contact *repository.Contact,
    external *repository.ExternalContact,
) error {
    existingMethods, err := s.methodRepo.ListContactMethodsByContact(ctx, contact.ID)
    if err != nil {
        return err
    }

    existingSet := make(map[string]bool)
    for _, m := range existingMethods {
        existingSet[methodDedupKey(m.Type, m.Value)] = true
    }

    for _, input := range BuildMethodsFromExternal(external) {
        key := methodDedupKey(input.Type, input.Value)
        if existingSet[key] {
            continue
        }
        _, err := s.methodRepo.CreateContactMethod(ctx, repository.CreateContactMethodRequest{
            ContactID: contact.ID,
            Type:      input.Type,
            Value:     input.Value,
            IsPrimary: false,
        })
        if err != nil {
            if isUniqueViolation(err) {
                // concurrent insert — already there, treat as success
                existingSet[key] = true
                continue
            }
            logger.Warn().Err(err).
                Str("type", input.Type).
                Str("value", input.Value).
                Msg("failed to add method from enrichment")
            continue
        }
        s.recordEnrichment(ctx, contact.ID, external,
            "method:"+input.Type+":"+identity.Normalize(input.Value, mapMethodTypeToIdentifier(input.Type)),
            input.Value)
        existingSet[key] = true
    }
    return nil
}

// methodDedupKey returns the dedup key used to compare an incoming method
// against existing methods on a contact. Mirrors the DB unique index shape
// (contact_id, type, value_normalized) — without type scoping, cross-type
// duplicates like telegram:foo vs twitter:foo would collide incorrectly.
func methodDedupKey(methodType, value string) string {
    return methodType + ":" + identity.Normalize(value, mapMethodTypeToIdentifier(methodType))
}

// isUniqueViolation returns true if err is a PostgreSQL unique_violation (23505).
// Used to treat concurrent-insert races as idempotent no-ops.
func isUniqueViolation(err error) bool {
    var pgErr *pgconn.PgError
    return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
```

Add imports: `github.com/jackc/pgx/v5/pgconn`, `errors`.

### 3.3 Refactor — `enrichContactMethodsWithSelections` (selections mode)

**File:** `backend/internal/service/enrichment.go` (modify)

Four changes required:

**Change A — `existingNormalized` + `existingMethodByKey` lookups use type-scoped keys:**

```go
// replaces current lines 201-208
existingKeys := make(map[string]bool)
existingMethodByKey := make(map[string]*repository.ContactMethod)
for i := range existingMethods {
    m := &existingMethods[i]
    key := methodDedupKey(m.Type, m.Value)
    existingKeys[key] = true
    existingMethodByKey[key] = m
}
```

Renamed `existingNormalized` → `existingKeys` to reflect the new key shape.

**Change B — admissible-external lookup is built from the shared helper output, keyed by `methodDedupKey`:**

```go
// replaces current lines 211-217
externalKeys := make(map[string]bool)
for _, m := range BuildMethodsFromExternal(external) {
    externalKeys[methodDedupKey(m.Type, m.Value)] = true
}
```

Renamed `externalValues` → `externalKeys`. Using the type-scoped normalized key lets both `@handle` and bare-handle forms from the frontend match a single entry emitted by the helper (`telegram:<lowercased-bare-handle>`).

**Change C — selection validation and existing-method lookup use `methodDedupKey`; stored value is canonicalized:**

```go
// current lines 229-254 become:
for _, sel := range selectedMethods {
    selKey := methodDedupKey(sel.Type, sel.OriginalValue)

    if !externalKeys[selKey] {
        methodErrors = append(methodErrors,
            fmt.Sprintf("value %q not found in external contact", sel.OriginalValue))
        continue
    }

    if existingKeys[selKey] {
        if sel.IsPrimary {
            if existingMethod := existingMethodByKey[selKey]; existingMethod != nil {
                existingPrimaryMethodID = &existingMethod.ID
            }
        }
        continue
    }

    // Canonicalize the stored value to match storage convention across paths:
    //   - telegram/twitter/discord: strip leading @ and trim whitespace (bare handle)
    //   - email/phone: preserve as-is
    // This mirrors buildMethodsAuto's import-new behavior so link-flow storage
    // is consistent with import-new storage.
    storedValue := canonicalizeMethodValue(sel.Type, sel.OriginalValue)

    newMethod, err := s.methodRepo.CreateContactMethod(ctx, repository.CreateContactMethodRequest{
        ContactID: contact.ID,
        Type:      sel.Type,
        Value:     storedValue,
        IsPrimary: false,
    })
    if err != nil {
        if isUniqueViolation(err) {
            existingKeys[selKey] = true
            continue
        }
        methodErrors = append(methodErrors,
            fmt.Sprintf("failed to add method %s: %v", storedValue, err))
        continue
    }

    if sel.IsPrimary {
        newPrimaryMethodID = &newMethod.ID
    }

    s.recordEnrichment(ctx, contact.ID, external,
        "method:"+sel.Type+":"+identity.Normalize(storedValue, mapMethodTypeToIdentifier(sel.Type)),
        storedValue)
    existingKeys[selKey] = true
}
```

**Change D — new helper `canonicalizeMethodValue`:**

```go
// canonicalizeMethodValue converts a user- or frontend-supplied raw method
// value into the canonical form stored in contact_method.value. For
// handle-based types (telegram/twitter/discord), strips leading '@' and
// trimmed whitespace. Does NOT lowercase — storage preserves user casing;
// value_normalized in the DB carries the lowercased form via the trigger.
func canonicalizeMethodValue(methodType, rawValue string) string {
    switch methodType {
    case "telegram", "twitter", "discord":
        return strings.TrimPrefix(strings.TrimSpace(rawValue), "@")
    default:
        return rawValue
    }
}
```

This is also useful in `BuildMethodsFromExternal` — refactor §3.1 to call `canonicalizeMethodValue("telegram", raw)` in its telegram branch so the canonicalization rule lives in one place.

### 3.3.5 Adjust `recordEnrichment` for synthesized ExternalContact

**File:** `backend/internal/service/enrichment.go` (modify)

Change `recordEnrichment` (currently at line 375) to pass `ExternalContactID = nil` when the input `external.ID == uuid.Nil`. This ensures the matcher's synthesized `*ExternalContact` (which has no persisted ID) produces a SQL `NULL` in `contact_enrichment.external_contact_id`, not a zero-UUID.

```go
func (s *EnrichmentService) recordEnrichment(
    ctx context.Context,
    contactID uuid.UUID,
    external *repository.ExternalContact,
    field string,
    value string,
) {
    var externalContactID *uuid.UUID
    if external.ID != uuid.Nil {
        externalContactID = &external.ID
    }
    _, err := s.enrichmentRepo.Create(ctx, repository.CreateEnrichmentRequest{
        ContactID:         contactID,
        Source:            external.Source,
        AccountID:         external.AccountID,
        Field:             field,
        ExternalContactID: externalContactID,
        OriginalValue:     &value,
    })
    if err != nil {
        logger.Warn().Err(err).Str("field", field).Msg("failed to record enrichment audit row")
    }
}
```

Existing callers (Google sync, import link flow) always pass persisted `*ExternalContact` instances with real UUIDs, so they are unaffected by this change. Only the matcher's synthesized path (when `external_contact` was absent) triggers the `NULL` branch.

### 3.4 New method — `EnrichmentService.SyncMethodsFromExternal`

**File:** `backend/internal/service/enrichment.go` (modify)

```go
// SyncMethodsFromExternal adds any missing contact methods from an
// ExternalContact to the given CRM contact. Unlike EnrichContactFromExternal,
// it does NOT touch profile fields (photo, birthday, location, name, cadence).
// Intended for auto-match flows where silent profile overwrites are undesirable.
//
// Audit rows are written via recordEnrichment, same as EnrichContactFromExternal.
// Idempotent: duplicate methods (either via normalized-value dedup or PG
// unique-violation race) are no-ops.
func (s *EnrichmentService) SyncMethodsFromExternal(
    ctx context.Context,
    crmContactID uuid.UUID,
    external *repository.ExternalContact,
) error {
    contact, err := s.contactRepo.GetContact(ctx, crmContactID)
    if err != nil {
        return fmt.Errorf("get contact for method sync: %w", err)
    }
    return s.enrichContactMethods(ctx, contact, external)
}
```

### 3.5 Refactor — `buildMethodsAuto` in import handler

**File:** `backend/internal/api/handlers/import.go` (modify)

Delete `buildMethodsAuto` (lines 449-482). Replace the caller with:

```go
methods := service.BuildMethodsFromExternal(external)
```

### 3.6 Inject `EnrichmentService` into `PeerMatcher`

**File:** `backend/internal/telegram/matcher.go` (modify)

```go
// contactEnricher performs narrow method-only enrichment on a bound CRM
// contact. Satisfied by *service.EnrichmentService. Can be nil (tests).
type contactEnricher interface {
    SyncMethodsFromExternal(ctx context.Context, crmContactID uuid.UUID, external *repository.ExternalContact) error
}

type PeerMatcher struct {
    identityService     identityMatcher
    messageRepo         *repository.TelegramMessageRepository
    messageCounter      peerMessageCounter
    externalContactRepo externalContactUpserter
    enricher            contactEnricher  // new; nil-safe
    discoveryMinMsgs    int
}

func NewPeerMatcher(
    identityService identityMatcher,
    messageRepo *repository.TelegramMessageRepository,
    externalContactRepo externalContactUpserter,
    enricher contactEnricher,
    discoveryMinMsgs int,
) *PeerMatcher {
    return &PeerMatcher{
        identityService:     identityService,
        messageRepo:         messageRepo,
        externalContactRepo: externalContactRepo,
        enricher:            enricher,
        discoveryMinMsgs:    discoveryMinMsgs,
    }
}
```

Existing unit tests that construct `PeerMatcher` directly with struct literals leave `enricher` zero-valued (nil). The matcher's call site MUST nil-guard (see §3.7).

### 3.7 Invoke enrichment from `MatchPeer` (not `markExternalContactMatched`)

**File:** `backend/internal/telegram/matcher.go` (modify)

Add a private helper:

```go
// ensureMethodsOnMatch syncs contact methods from the known peer data onto
// the matched CRM contact. Called after every successful identity match in
// MatchPeer, regardless of discovery threshold or prior match status.
//
// Data sourcing: reads the persisted external_contact if present (preserves
// audit linkage + any emails/phones). Whether persisted or synthesized, the
// current-message peerUsername is overlaid onto metadata.username if present
// and non-empty — the current message is the freshest source of the peer's
// handle, and wins over any stored value (handles handle-renames and fills
// gaps in rows that predate username capture).
//
// All errors are logged at warn and do NOT fail the match.
func (m *PeerMatcher) ensureMethodsOnMatch(ctx context.Context, contactID uuid.UUID, peerUserID int64, peerUsername *string) {
    if m.enricher == nil {
        return
    }
    peerIDStr := strconv.FormatInt(peerUserID, 10)

    external, err := m.externalContactRepo.GetBySource(ctx, "telegram", peerIDStr, nil)
    if err != nil {
        log.Warn().Err(err).Int64("peer_user_id", peerUserID).Msg("telegram: get external_contact for method sync failed")
        // fall through — synthesize below
    }
    if external == nil {
        external = &repository.ExternalContact{
            Source:   "telegram",
            SourceID: peerIDStr,
            Metadata: map[string]interface{}{},
        }
    } else if external.Metadata == nil {
        external.Metadata = map[string]interface{}{}
    }

    // Merge: current-message peerUsername wins over any stored metadata.username.
    // Safe to mutate in place — GetBySource returns a fresh instance per call.
    if peerUsername != nil && *peerUsername != "" {
        external.Metadata["username"] = "@" + *peerUsername
    }

    if err := m.enricher.SyncMethodsFromExternal(ctx, contactID, external); err != nil {
        log.Warn().Err(err).
            Str("contact_id", contactID.String()).
            Int64("peer_user_id", peerUserID).
            Msg("telegram: sync methods from external failed")
    }
}
```

**Call sites inside `MatchPeer`:**

```go
// After username match (current line 85):
m.markExternalContactMatched(ctx, peerUserID, *result.ContactID)
m.ensureMethodsOnMatch(ctx, *result.ContactID, peerUserID, peerUsername)  // NEW
log.Info()...

// After phone match (current line 113):
m.markExternalContactMatched(ctx, peerUserID, *result.ContactID)
// (existing telegram-identity-linking block stays)
m.ensureMethodsOnMatch(ctx, *result.ContactID, peerUserID, peerUsername)  // NEW
log.Info()...
```

`markExternalContactMatched` itself is unchanged.

**Why `MatchPeer`-level, not `markExternalContactMatched`-level:**

1. `markExternalContactMatched` returns early when no `external_contact` exists for the peer (matcher.go:337) — below-threshold peers (fewer than `discoveryMinMsgs` messages) have no row, but can still be identity-matched via username.
2. `markExternalContactMatched` returns early when status is already `matched`/`imported` (matcher.go:339) — re-match scenarios can't repair a missing method through that path.
3. `MatchPeer` is the single point that reaches every successful identity match — live (`handlers.go:96`) and batch (`manager.go:573`) both flow through it.

### 3.8 Wire through `TelegramManager` → `main.go`

**File:** `backend/internal/telegram/manager.go` (modify)

Add `enricher contactEnricher` parameter to `NewTelegramManager`; thread to `NewPeerMatcher`.

**File:** `backend/cmd/crm-api/main.go` (modify)

Pass existing `enrichmentService` (constructed at line 164) into `NewTelegramManager` at line 263. `*service.EnrichmentService` satisfies `contactEnricher` via implicit interface satisfaction.

### 3.9 Update integration-test call site

**File:** `backend/tests/telegram_discovery_upsert_test.go:363` (modify)

```go
// Current: tgpkg.NewPeerMatcher(identitySvc, messageRepo, externalRepo, 2)
// New:
tgpkg.NewPeerMatcher(identitySvc, messageRepo, externalRepo, nil, 2)
```

Passing `nil` for the enricher is safe — this test asserts discovery upsert behavior, not method enrichment. If test goals expand later, inject a real `EnrichmentService` built from the existing test DB wiring.

---

## 4. File Inventory

### New Files

| File | Purpose |
|---|---|
| `backend/internal/service/external_methods.go` | `BuildMethodsFromExternal` shared helper |
| `backend/internal/service/external_methods_test.go` | Unit tests for helper |
| `backend/tests/telegram_matcher_enrichment_test.go` | Integration: matcher auto-match → telegram method appears (all sub-cases) |

### Modified Files

| File | Change |
|---|---|
| `backend/internal/service/enrichment.go` | Refactor both `enrichContactMethods` funcs to use shared helper + `methodDedupKey` + `isUniqueViolation` + `canonicalizeMethodValue`; rename `existingMethodByNormalized` → `existingMethodByKey` and `existingNormalized` → `existingKeys`, `externalValues` → `externalKeys`; add `SyncMethodsFromExternal`; update `recordEnrichment` to emit SQL NULL for `ExternalContactID` when `external.ID == uuid.Nil` |
| `backend/internal/api/handlers/import.go` | Delete `buildMethodsAuto`; caller switches to `service.BuildMethodsFromExternal` |
| `backend/internal/telegram/matcher.go` | Add `contactEnricher` interface + field; update `NewPeerMatcher` signature; add `ensureMethodsOnMatch`; call it from `MatchPeer` username-path and phone-path |
| `backend/internal/telegram/manager.go` | Thread `enricher` through `NewTelegramManager` to `NewPeerMatcher` |
| `backend/cmd/crm-api/main.go` | Pass existing `enrichmentService` into `NewTelegramManager` |
| `backend/tests/telegram_discovery_upsert_test.go` | Update `NewPeerMatcher` call site |
| `backend/internal/telegram/matcher_test.go` | Add matcher-level tests (see §5.2) |
| `backend/tests/api/import_with_selections_test.go` | Add/extend test cases covering telegram in `SelectedMethods` (see §5.3) |

---

## 5. Test Plan

### 5.1 Unit — `BuildMethodsFromExternal`

**File:** `backend/internal/service/external_methods_test.go` (new)

- Nil external → nil slice.
- Empty external_contact → empty slice.
- Emails only → email methods in order.
- Phones only → phone methods.
- Telegram source with `metadata.username="@DaleDobeck"` → telegram method, `Value="DaleDobeck"`.
- Telegram source with `metadata.username="DaleDobeck"` (no `@`) → same.
- Telegram source with `metadata.username=""` → no telegram method.
- Telegram source with `metadata.username="@  "` (whitespace-only after strip) → no telegram method.
- Telegram source with no `metadata.username` key → no telegram method.
- Telegram source with `metadata.username` as non-string (e.g., `int`) → no telegram method (safe type-assert).
- Non-telegram source with `metadata.username` set → no telegram method (source guard).
- Telegram with emails + phones + username → all three, emitted in stable order (email → phone → telegram).

### 5.2 Unit — Matcher Enrichment Invocation

**File:** `backend/internal/telegram/matcher_test.go` (modify)

New `mockContactEnricher` recording calls.

- **Username match fires enricher:** `MatchPeer` with matching `peerUsername` → `mockEnricher.SyncMethodsFromExternal` called once with correct `contactID`. Assert `external.Metadata["username"]` carries the `@`-prefixed username.
- **Phone match with username fires enricher:** `peerUsername` present + `peerPhone` present + identity returns match on phone (not username) → enricher called once. Username is available, so synthesized metadata includes it.
- **Phone match without username does not call enricher with username metadata:** `peerUsername=nil`, phone matches → enricher is called (match succeeded) but synthesized `external.Metadata` has no `username` key → `BuildMethodsFromExternal` emits no telegram method → no contact_method written. (This verifies via asserting `mockEnricher.captured[0].Metadata`.)
- **Unmatched peer — no enricher call:** identity returns no ContactID for both username and phone → `MatchPeer` returns nil, enricher not called.
- **Nil enricher is safe:** `PeerMatcher{enricher: nil}` — `MatchPeer` runs to completion, no panic.
- **Enricher error does not fail match:** `mockEnricher.SyncMethodsFromExternal` returns error → `MatchPeer` returns the matched `contactID` and no error.
- **External_contact present is preferred as the base for enrichment:** mock `externalContactRepo.GetBySource` returns a real `*ExternalContact` (with extra emails and a stored `metadata.username="@Old"`) → enricher receives that pointer. When `peerUsername=ptr("New")` is also provided, the received struct's `Metadata["username"]` is `"@New"` (current wins); the original emails list is preserved.
- **External_contact absent uses synthesis:** `GetBySource` returns `(nil, nil)` → enricher receives a fresh `*ExternalContact` with `Source="telegram"`, `SourceID` set, `Metadata["username"]` from current `peerUsername`.
- **External_contact present but `Metadata` is nil:** `GetBySource` returns a row with `Metadata: nil` → helper initializes the map safely before writing.
- **External_contact present with empty metadata, current peerUsername present:** current message fills the gap; telegram method is created.
- **External_contact present with stale `metadata.username="@Old"`, current peerUsername="New":** current wins; helper passes `"@New"` to enricher; resulting contact_method `value="New"`.
- **Both external_contact and current peerUsername missing username:** no telegram method written (helper writes nothing to `Metadata["username"]`; `BuildMethodsFromExternal` emits no telegram entry).

### 5.3 Integration — Link Flow with Telegram Selection

**File:** `backend/tests/api/import_with_selections_test.go` (modify)

- **Primary case:** seed telegram external_contact (`source_id='tg-link-test-1'`, `metadata.username='@TestLink'`) and a CRM contact with only an email. `POST /imports/:id/link` with `selected_methods=[{type:"telegram", original_value:"TestLink", is_primary:false}]`. Assert: one new `contact_method` (`type=telegram`, `value=TestLink`, `value_normalized=testlink`, `is_primary=false`). `contact_enrichment` row present.
- **Leading-`@` passthrough:** same but `original_value:"@TestLink"` — `value` stored as `TestLink` (bare), `value_normalized` as `testlink`. Confirms consistency with import-new behavior.
- **Telegram deselected:** same seed, `selected_methods=[{type:"email", ...}]` → no telegram method created.
- **Idempotency on link:** target already has `contact_method(type=telegram, value_normalized=testlink)` → request succeeds, no duplicate, no warn.
- **Auto-fallback path:** `POST /imports/:id/link` with empty body (no selected_methods, cadence, name) → hits `EnrichContactFromExternal` (not Selections) → telegram is auto-added from `metadata.username`.
- **Profile fields unchanged:** verify `profile_photo`, `birthday`, `location` on target contact unchanged after all of the above cases (controls for "narrow enrichment" claim when applied to selections variant; the Selections variant DOES overwrite profile fields in its own right, but only when `name`/`cadence` are explicitly sent — these cases send neither).

### 5.4 Integration — Matcher Auto-Match

**File:** `backend/tests/telegram_matcher_enrichment_test.go` (new)

All cases use a real `PeerMatcher` with a real `EnrichmentService` (method repo + enrichment repo + contact repo wired against test DB). `identityService` is also real, pre-seeded with the external_identity that produces each match.

- **Above-threshold username match:** seed `external_contact(source_id='tg-auto-1', metadata.username='@AutoUser', match_status='unmatched')`, seed `external_identity` linking `autouser` (telegram) to a CRM contact, call `MatchPeer(peerUserID=..., peerUsername=ptr("AutoUser"), ...)`. Assert: `contact_method(type=telegram, value=AutoUser)` exists; `contact_enrichment.external_contact_id` is the seeded ID; profile fields unchanged.
- **Below-threshold username match (no external_contact row):** seed only `external_identity` linking `autouser` to a CRM contact — do NOT seed `external_contact`. `MatchPeer(peerUsername=ptr("AutoUser"))`. Assert: `contact_method` still created (from synthesized ExternalContact); `contact_enrichment.external_contact_id` is NULL.
- **Phone match with username present:** seed identity by `phone`; call `MatchPeer(peerUsername=ptr("PhoneMatched"), peerPhone=ptr("+15551234"))`. Assert telegram method created as `PhoneMatched`.
- **Phone match without username:** seed identity by phone; call `MatchPeer(peerUsername=nil, peerPhone=ptr("+15551234"))`. Assert: NO telegram `contact_method` on the matched contact (nothing to add); match itself succeeded (`external_contact.match_status='matched'` if a row existed).
- **Idempotency — run twice:** call `MatchPeer` twice back-to-back for the same peer. Assert: exactly one `contact_method` exists; no warn-level log from the second call for duplicate insertion.
- **Profile fields untouched:** in the above-threshold case, seed the external_contact with `first_name='WrongName'`, `photo_url='http://example.com/wrong.jpg'`. Assert: CRM contact's `full_name` and `profile_photo` unchanged after `MatchPeer`.
- **Enricher error does not break match bind:** use a fault-injecting method repo (e.g., wrap with a failing-on-Create decorator) and verify `MatchPeer` returns the matched `contactID` and `external_contact.match_status='matched'` even when CreateContactMethod fails.

### 5.5 Regression — Import-new Path

**File:** `backend/tests/api/import_with_selections_test.go` (modify, or add if missing)

- Import a telegram candidate via `POST /imports/:id/import` (new contact path). Assert telegram `contact_method` created with bare handle — proves `BuildMethodsFromExternal` output matches the old `buildMethodsAuto` output.

### 5.6 Google Auto-Match

No new test. Existing Google sync integration tests (if any) must pass unchanged. Relies on CI to catch regressions.

---

## 6. Rollout and Validation

### 6.1 Order of Work

1. Add `BuildMethodsFromExternal` + unit tests (§5.1).
2. Add `methodDedupKey` + `isUniqueViolation` helpers in `enrichment.go`.
3. Refactor `enrichContactMethods` to use the helper and new dedup key (§3.2).
4. Refactor `enrichContactMethodsWithSelections` (§3.3) — both lookup maps type-scoped.
5. Add `SyncMethodsFromExternal` (§3.4).
6. Delete `buildMethodsAuto`; switch caller (§3.5).
7. Add `contactEnricher` interface + field + `ensureMethodsOnMatch` helper in matcher (§3.6, §3.7).
8. Update `NewPeerMatcher` signature; thread through `TelegramManager` + `main.go` (§3.8).
9. Update existing call site in `telegram_discovery_upsert_test.go` (§3.9).
10. Add unit tests (§5.2).
11. Add integration tests (§5.3, §5.4).
12. `go build ./cmd/crm-api`, `make lint`, `make test`, `make test-e2e-diff`. No `make sqlc` needed.

### 6.2 Verification

- Manual spot-check after deploy: pick a recently-matched Telegram peer, confirm telegram method appears. Jack Laing (`ea365ac2-d337-470f-b3c9-77bb03b75301`) is a good post-fix check target after the user's separate backfill.
- The user's one-shot backfill SQL (external to this PR) handles historical matched-but-unenriched peers.

### 6.3 Observability

- `ensureMethodsOnMatch` logs at warn on any non-retriable error. Success path is silent (no extra log spam on every match).
- `enrichContactMethods` logs warn on non-unique-violation errors. Unique violations now produce no log (debug-level at most).
- No new info-level logging — existing "peer matched" info log already marks the success point.

---

## 7. Risks and Open Questions

### Risks

1. **Existing matcher unit tests constructing `PeerMatcher` via struct literals** leave `enricher` nil. Nil-guard in `ensureMethodsOnMatch` prevents panic. Tests that assert no enrichment side-effect are unaffected. New tests (§5.2) that DO assert enrichment use an explicit `mockContactEnricher`.
2. **Dedup-key change in `enrichContactMethods` and Selections variant** from value-only to `type+normalized`. Codex-round-1 review confirmed this matches the DB unique index and the merge-dedup query in `contact_merge.sql:91`. No known caller relies on cross-type collisions — the value-only keying was arguably buggy for edge cases like `telegram:foo` vs `twitter:foo`.
3. **PG `23505` handling.** Races between two `MatchPeer` calls for the same peer are rare but possible (live + batch overlap). `isUniqueViolation` check catches them; treat as success, no warn.
4. **Synthesized `ExternalContact` in matcher** has nil `ID`. `recordEnrichment` stores `external_contact_id` as a pointer, so a nil `ID` becomes NULL in the audit row. Confirmed via `CreateEnrichmentRequest` signature — `ExternalContactID *uuid.UUID`. Integration test (§5.4 below-threshold case) explicitly asserts the NULL outcome.
5. **Google Contacts path regression.** Google uses `EnrichContactFromExternal` (auto variant) — now uses the new `methodDedupKey`. Since Google populates only emails/phones, the telegram branch in the helper is inert. Existing integration tests must pass unchanged.
6. **`messageCounter` default wiring.** `NewPeerMatcher` currently defaults `messageCounter` to `messageRepo` implicitly (if the constructor does so); must verify the refactored constructor preserves this. If `messageCounter` is assigned in the constructor body, add back the assignment.

### Open Questions

1. **(Resolved in round 1.)** Matcher enrichment call-site: moved to `MatchPeer` per Codex.
2. **Should `methodDedupKey` be exported?** Not yet — no cross-package consumer. If #182 rematch-service needs it, promote later.
3. **Should `SyncMethodsFromExternal` skip `contact_enrichment` audit rows in the auto-match path?** Currently inherited from `enrichContactMethods`. Pro: auditability. Con: every automatic match writes an audit row. Keeping for now; revisit if noise is a problem post-deploy.

---

## 8. Review Log

**Round 1 (Codex):** FAIL — 5 findings.
1. Matcher call-site placement inside `markExternalContactMatched` was wrong (early-returns miss below-threshold peers and re-matches). **Fixed:** moved to `MatchPeer` orchestration in §3.7 with synthesized ExternalContact fallback.
2. `enrichContactMethodsWithSelections` also has an `existingNormalized` value-only map at line 201 that needed updating alongside `externalValues`. **Fixed:** §3.3 Change A now updates both maps to type-scoped keys.
3. Dedup-key rollout was partial across the two enrichment paths. **Fixed:** §3.3 Change A makes both paths use `methodDedupKey` consistently.
4. Nil-safety + existing `matcher_test.go` struct-literal construction risked panics. **Fixed:** §3.6 documents nil-safety; §5.2 explicitly covers the nil-enricher case.
5. Test plan needed phone-match (with/without username), raw `@`-handle consistency, idempotency under race. **Fixed:** §5.2 adds four new matcher-unit cases; §5.3 adds leading-`@` passthrough; §5.4 adds idempotency-under-re-run and enricher-error-does-not-break-match.
Plus: race-condition PG `23505` handling was added via `isUniqueViolation` helper.

**Round 2 (Codex):** FAIL — 1 remaining design gap.
6. `ensureMethodsOnMatch` framed persisted-vs-synthesized as choose-one. If a stored `external_contact` pre-dates username capture or the user renamed their Telegram handle, the persisted row's `metadata.username` can be missing or stale while the current message carries the fresh value. Choose-one would suppress the telegram method creation in those cases. **Fixed:** §3.7 now merges — current-message `peerUsername` always overlays onto `metadata.username` when present, regardless of whether `external_contact` was persisted or synthesized. §5.2 adds three new sub-cases covering nil Metadata, gap-fill, and rename-overlay.
(Round 2 also noted: file path was not on disk at review time — plan had been written to the main repo tree, not the worktree. Moved before round 3.)

**Round 3 (Codex):** FAIL — 2 findings.
7. Leading-`@` in link-flow selections was still broken because the helper emits bare handles and the validator compared raw `OriginalValue` strings. **Fixed:** §3.3 Change B uses `methodDedupKey` on both sides of the admission check so `"@JackALaing"` and `"JackALaing"` produce the same key. §3.3 Change C adds `canonicalizeMethodValue` to strip `@` from stored values, matching import-new behavior. §3.3 Change D defines the helper. §3.1 now calls the same helper internally, single source of truth.
8. Synthesized-matcher audit row would have emitted a zero-UUID (not SQL NULL) because `recordEnrichment` unconditionally passes `&external.ID`. **Fixed:** §3.3.5 updates `recordEnrichment` to pass `nil` when `external.ID == uuid.Nil`. Existing callers (Google sync, link handler) always pass persisted IDs so their behavior is unchanged.

**Round 4 (Codex):** FAIL — 1 trivial finding.
9. Wrong Go module path in §3.1 import block (`github.com/spengrah/personalcrm/...`). **Fixed:** corrected to `personal-crm/backend/internal/repository` per `backend/go.mod`.
