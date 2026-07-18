# Contact-method lost update: stale-cache clobber on PUT /contacts/:id

Status: design, ready to plan
Date: 2026-07-18

## Summary

A contact method added server-side by import-link enrichment can be silently and permanently destroyed by a subsequent contact edit from the UI. Two independent defects compound: the contact detail cache is not invalidated after a link, so the UI shows a stale method list; and `PUT /contacts/:id` treats a supplied `methods` array as a wholesale replace with no concurrency control, so saving that stale list deletes anything the client did not know about.

This document specifies the fix for both, plus a related gap in reconcile coverage found during the investigation.

## Observed incident

Reconstructed from prod DB state (request logs for the window were lost to a container restart). One contact, one email method:

| Time (UTC) | Event | Evidence |
|---|---|---|
| 20:12:52.201 | Contact created from a telegram import; telegram method inserted | `contact_methods.added` event, telegram |
| 20:12:59.074 | Calendar candidate linked; email method inserted and committed | `contact_methods.added` event, email + `contact_enrichment` audit row |
| 20:13:31.661 | Contact edited to add a handle; all methods deleted and recreated from the submitted list | contact `updated_at` and both surviving methods' `created_at` identical to the microsecond |

The email was absent from the recreated set and was permanently lost. The telegram method's `created_at` (20:13:31) not matching its own `contact_methods.added` event (20:12:52) is the tell: the row was destroyed and reinserted.

The user reported the email as missing *before* the edit at 20:13:31, at which point it was present in the database. That report was the stale cache (defect 1). The remediation action it prompted then triggered the destructive write (defect 2).

## Defect 1: contact detail not invalidated after link

`frontend/src/lib/query-invalidation.ts` maps `'import:linked'` to `[importKeys.lists(), importKeys.suggestionsLists(), contactKeys.lists()]`. The contact *detail* key is absent, so a contact page already in cache does not refetch after a link enriches it.

Every sibling rule that enriches an existing contact already includes the detail key via the factory pattern: `method-suggestion:resolved`, `meeting-note:resolved`, `name-candidate:resolved`. `import:linked` is the outlier, and its own comment ("Linking enriches an existing contact") describes behavior it does not implement.

### Fix

Add the per-contact detail key to the `import:linked` rule and pass the contact id at the call site. The link mutation already holds `crm_contact_id`, so no new plumbing is needed — this is the same shape as `method-suggestion:resolved`.

## Defect 2: PUT /contacts/:id is a destructive replace with no concurrency control

`UpdateContact` (`backend/internal/service/contact.go`) branches on a `replaceMethods` flag:

```go
if replaceMethods {
    contactMethodRepo.DeleteContactMethodsByContact(ctx, id)  // deletes ALL methods
    createContactMethods(ctx, contactMethodRepo, id, methods) // recreates from payload
}
```

The handler sets `replaceMethods` to `req.Methods != nil`. Any client that submits a `methods` array built from a stale read destroys every method absent from that array, with no error and no signal to the user. The only surviving trace is the `contact_enrichment` audit row, which lives in a different table and is not consulted anywhere.

The stale cache is one trigger. Others remain: background sync, enrichment from any source, a second browser tab. Fixing defect 1 alone leaves the destructive primitive loaded.

### Why `contact.updated_at` is not a sufficient precondition

Adding a contact method does not change `contact.updated_at`. Two independent reasons, both verified:

1. `EnrichContactFromExternalWithSelections` gates the contact-row UPDATE behind a `needsUpdate` flag covering only photo, birthday, location, cadence, and name. A methods-only enrichment leaves the contact row untouched.
2. The only triggers on `contact_method` are `set_contact_method_value_normalized` and `update_contact_method_updated_at`, both of which write to `contact_method` itself. Nothing propagates to `contact`.

An `If-Match` on `contact.updated_at` would therefore have passed cleanly in the observed incident. The precondition must cover the method set.

### Fix: optimistic concurrency scoped to PUT-writable fields

**ETag.** `GET /contacts/:id` returns an `ETag` header computed over exactly the state `PUT` writes: `full_name`, `cadence`, and the normalized method set (type, value, `is_primary`), order-independent. Deriving it from writable fields only means an incoming interaction moving `last_contacted` does not produce a spurious conflict.

**Precondition.** `PUT /contacts/:id` accepts `If-Match`:

| Request | Behavior |
|---|---|
| `If-Match` present, matches current ETag | Apply |
| `If-Match` present, stale | `409` with the current representation in the body |
| `If-Match` absent | Apply unconditionally (unchanged from today) |

Returning the current representation on 409 lets the client replay without a second round-trip. Treating an absent `If-Match` as unconditional keeps non-browser clients (mac-daemon, scripts) working without modification: protection is opt-in per client, and the web UI opts in.

**Client.** The edit form captures the ETag it loaded and sends it on save. On 409 it re-applies the user's dirty fields onto the returned fresh representation and resubmits once. A second 409 surfaces a message asking the user to review. One retry, never a loop.

For a single-user CRM the overwhelmingly common conflict is benign — the server added a method the client had not seen, which does not overlap the field the user edited — so a silent successful save is the correct outcome and a conflict dialog would be noise.

## Related finding: calendar-sourced links are outside reconcile coverage

`addressBookReconcileSources` in `backend/internal/service/address_book_reconcile.go` is `{gcontacts, icloud_contacts}`. Calendar-attendee rows are explicitly out of scope.

That means for a calendar-sourced link, neither reconcile branch ever runs: no `autoPropagate` for a `matched` link and no `recordSuggestions` for an `imported` one. There is no safety net — a method missed at link time is missed permanently, and never even surfaces as a pending suggestion.

A prod audit of linked calendar rows carrying an email found 10 whose email is absent from the contact. Triaged via the `contact_enrichment` audit table, which records a row only after a successful method insert:

- **1** had an audit row: the incident above. Data loss. Restored manually.
- **6** point at soft-deleted contacts. Expected — the methods were removed with the contact.
- **2** are `matched` links with no audit row. These are genuine unfilled gaps: the system's stated semantics for a `matched` link are that an unapplied method is "a genuine gap → auto-propagate," but the source exclusion means that never happens.
- **1** is an `imported` (curated) link on a contact with several other methods. Consistent with a deliberate deselection at curation time; no action.

### Scope decision

Extending reconcile coverage to calendar-attendee rows is deliberately **out of scope** for this work. It is a behavior change to a matching subsystem with its own semantics, and it is not on the path of the reported bug. It should be filed separately, along with a decision on whether to backfill the 2 `matched` rows.

## Testing

**Regression (integration).** Link a candidate so enrichment adds a method, then `PUT` with an `If-Match` captured before the link and a method list omitting it. Assert `409` and that the enriched method survives. This reproduces the incident exactly and fails against current `main`.

**Compatibility (integration).** A `PUT` with no `If-Match` still applies, so existing non-browser clients are unaffected.

**ETag (unit).** Order-independent across the method set; changes on method add, remove, and `is_primary` flip; does not change when `last_contacted` moves.

**E2E.** Link a candidate and assert the contact detail surface shows the new method with no manual refresh. Covers defect 1, and is required regardless: the behavior is `surface: ui` in an `e2e_settled` domain, so it cannot land uncited.

## Spec updates

Both domains are `e2e_settled: true`; per `spec/README.md` a new or changed `surface: ui` then-item must land with its citing E2E test in the same PR.

- `spec/imports-matching.yaml` — linking a candidate refreshes the contact detail surface.
- `spec/contacts.yaml` — conditional-update semantics for `PUT /contacts/:id`, including the absent-`If-Match` fallback.

## Out of scope

- Reconcile coverage for calendar-attendee rows, and backfill of the 2 `matched` gaps (file separately).
- Replacing the wholesale-replace primitive with additive method endpoints. Considered and rejected for now: optimistic concurrency closes the lost-update class at a fraction of the cost, and additive endpoints would require rewriting the edit form's submit path.
- Extending `If-Match` to other write endpoints. Same pattern will apply if wanted later; this work establishes it on the endpoint with demonstrated data loss.
