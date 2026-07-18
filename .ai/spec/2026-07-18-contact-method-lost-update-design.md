# Contact-method lost update: stale-cache clobber on PUT /contacts/:id

Status: design, ready to plan
Date: 2026-07-18 (revised — see Design history)

## Summary

A contact method added server-side by import-link enrichment can be silently and permanently destroyed by a subsequent contact edit from the UI. Two independent defects compound: the contact detail cache is not invalidated after a link, so the UI shows a stale method list; and `PUT /contacts/:id` treats a supplied `methods` array as a wholesale delete-and-recreate, so saving that stale list deletes anything the client did not know about.

The fix is to invalidate the cache, and to retire the wholesale-replace primitive by moving contact methods onto dedicated sub-resource endpoints with explicit operations — the pattern this codebase already uses for every other child collection on a contact.

## Observed incident

Reconstructed from prod DB state (request logs for the window were lost to a container restart). One contact, one email method:

| Time (UTC) | Event | Evidence |
|---|---|---|
| 20:12:52.201 | Contact created from a telegram import; telegram method inserted | `contact_methods.added` event, telegram |
| 20:12:59.074 | Calendar candidate linked; email method inserted and committed | `contact_methods.added` event, email + `contact_enrichment` audit row |
| 20:13:31.661 | Contact edited to add a handle; all methods deleted and recreated from the submitted list | contact `updated_at` and both surviving methods' `created_at` identical to the microsecond |

The email was absent from the recreated set and was permanently lost. The telegram method's `created_at` (20:13:31) not matching its own `contact_methods.added` event (20:12:52) is the tell: the row was destroyed and reinserted.

The user reported the email as missing *before* the edit at 20:13:31, at which point it was present in the database. That report was the stale cache (defect 1). The remediation action it prompted then triggered the destructive write (defect 2).

## Premises

These are assumptions the design rests on. If one stops holding, the reasoning must be revisited.

1. **There will be multiple writing clients.** Today the web form is the only one — verified: mac-daemon touches only `/ingest/events` and `/host/*`, scripts only issue unauthenticated GET health probes, and crm-admin operates at the service layer. But a future MCP server is expected to write contacts. The design must therefore be safe for a client that has never read this document.
2. **There will not be concurrent writes from multiple sources.** Single-user CRM, one human, no simultaneous editors. This premise is what retires optimistic concurrency (see Rejected alternatives).
3. **`PUT /contacts/:id` will eventually be atomized** into per-property endpoints for MCP-client flexibility. That work is out of scope here, but among fixes that work, this design prefers the one that moves toward it.

## Defect 1: contact detail not invalidated after link

`frontend/src/lib/query-invalidation.ts` maps `'import:linked'` to `[importKeys.lists(), importKeys.suggestionsLists(), contactKeys.lists()]`. The contact *detail* key is absent, so a contact page already in cache does not refetch after a link enriches it.

Every sibling rule that enriches an existing contact already includes the detail key via the factory pattern: `method-suggestion:resolved`, `meeting-note:resolved`, `name-candidate:resolved`. `import:linked` is the outlier, and its own comment ("Linking enriches an existing contact") describes behavior it does not implement.

### Fix

Add the per-contact detail key to the `import:linked` rule and pass the contact id at the call site. The link mutation already holds `crm_contact_id`, so no new plumbing is needed — this is the same shape as `method-suggestion:resolved`.

This is independent of defect 2 and correct regardless of how the write path is fixed.

## Defect 2: `PUT /contacts/:id` destroys methods it was not told to destroy

`UpdateContact` (`backend/internal/service/contact.go`) branches on a `replaceMethods` flag:

```go
if replaceMethods {
    contactMethodRepo.DeleteContactMethodsByContact(ctx, id)  // deletes ALL methods
    createContactMethods(ctx, contactMethodRepo, id, methods) // recreates from payload
}
```

The handler sets `replaceMethods` to `req.Methods != nil`. Any client that submits a `methods` array built from a stale read destroys every method absent from that array, with no error and no signal to the user. The only surviving trace is the `contact_enrichment` audit row, which lives in a different table and is not consulted anywhere.

The stale cache is one trigger. Others remain: background sync, enrichment from any source, a second browser tab. Fixing defect 1 alone leaves the destructive primitive loaded.

Premise 1 sharpens this. A future MCP server told "add a phone number for this contact" would naturally send `methods: [{phone}]` — and silently destroy every other method. Under the current contract, every new client re-earns this bug. **"Absent means delete" must stop being expressible, not merely stop being triggered.**

### Fix: dedicated contact-method endpoints

Methods move to a sub-resource with explicit operations, so that removal requires *naming* what to remove. A client cannot delete what it never saw.

The codebase already uses this pattern for every other child collection on a contact — notes, interactions, tasks, identities. `contact_task_routes.go` is a near-exact template:

```
GET    /contacts/:id/tasks
POST   /contacts/:id/tasks
DELETE /contacts/:id/tasks/:taskId
```

Contact methods are the last child collection still managed by wholesale replacement through the parent PUT, and the only one that has lost data. This design brings them into line with the existing convention rather than introducing a new one.

Two consequences follow, and the plan must decide both explicitly:

- **`methods` on `PUT /contacts/:id`.** Once methods have their own endpoints, the PUT's methods branch has no remaining caller. Retiring it removes the destructive primitive outright rather than guarding it, and retires `CON-008`. Whether that retirement lands in this work or immediately after is a planning decision; leaving a destructive branch reachable is not.
- **The multi-request save.** The edit form currently saves contact and notepad together via `Promise.all` (`frontend/src/app/contacts/[id]/page.tsx:198`), and the "contact saved, note failed" direction is already unhandled. Per-method operations make partial failure more likely, not less. This needs deliberate state handling and a named test, not discovery in review.

## Rejected alternatives

**Optimistic concurrency (`ETag` / `If-Match` on `PUT /contacts/:id`).** This was the original design and was abandoned after two review rounds. Versioning a full representation requires enumerating every field the endpoint writes, which pulled in the knowledge writer (`location`, `birthday`, `how_met`), the cadence updater (`contact_by`), and `profile_photo`; and making the precondition atomic required a lock protocol honored by every `contact_method` writer, which pulled in enrichment, merge, and calendar-decline lock ordering. The plan reached ~1059 lines across five subsystems with five open P0 findings, to defend a primitive that should be removed instead. Premise 2 retires its core benefit: with no concurrent writers, there is no lost update for a precondition to catch that explicit operations do not already prevent. A further argument against: its compatibility carve-out ("absent `If-Match` keeps non-browser clients working") was written to protect a client population that does not exist.

**Dirty-field partial submit.** Having the form send only fields the user touched. This does not fix the incident: the user edited a *method*, so `methods` is dirty and the full stale array ships regardless. It would also require inverting `CON-006` ("Contact update is full-replace for managed profile fields", `status: current`) across four fields, relocating complexity into the knowledge writer rather than removing it.

**Additive `methods` plus an explicit removals field.** Structurally sound and cheaper than the above — a removal set derived from the client's own baseline cannot name a method the client never saw. Rejected only because it adds a *third* convention for mutating methods alongside the PUT and the existing suggestions endpoints, where dedicated endpoints collapse toward one and move with premise 3.

## Related finding: calendar-sourced links are outside reconcile coverage

`addressBookReconcileSources` in `backend/internal/service/address_book_reconcile.go` is `{gcontacts, icloud_contacts}`. Calendar-attendee rows are explicitly out of scope.

That means for a calendar-sourced link, neither reconcile branch ever runs: no `autoPropagate` for a `matched` link and no `recordSuggestions` for an `imported` one. There is no safety net — a method missed at link time is missed permanently, and never even surfaces as a pending suggestion.

A prod audit of linked calendar rows carrying an email found 10 whose email is absent from the contact. Triaged via the `contact_enrichment` audit table, which records a row only after a successful method insert:

- **1** had an audit row: the incident above. Data loss. Restored manually.
- **6** point at soft-deleted contacts. Expected — the methods were removed with the contact.
- **2** are `matched` links with no audit row. These are genuine unfilled gaps: the system's stated semantics for a `matched` link are that an unapplied method is "a genuine gap → auto-propagate," but the source exclusion means that never happens.
- **1** is an `imported` (curated) link on a contact with several other methods. Consistent with a deliberate deselection at curation time; no action.

## Testing

**Regression (integration).** The acceptance bar from defect 2: a client that has never seen a method must not be able to destroy it. Add a method server-side, then exercise the write path a naive client would use, and assert the unseen method survives. This reproduces the incident and fails against current `main`.

**Endpoint behavior (integration).** Add, remove, and primary-flag changes through the new endpoints, including removal of a method the client did name (which must succeed — the guarantee is about *unnamed* methods, not immutability).

**Partial failure (frontend).** Both directions of the multi-request save, including "contact saved, method operation failed". State handling must be defined, not incidental.

**E2E.** Link a candidate and assert the contact detail surface shows the new method with no manual refresh. Covers defect 1, and is required regardless: the behavior is `surface: ui` in an `e2e_settled` domain, so it cannot land uncited.

Two known E2E vacuity traps, both verified, that a test here must avoid:
- `frontend/src/lib/query-client.ts` sets `staleTime: 0` under the custom Playwright fixture, which makes a stale-cache assertion vacuous. Specs that import ordinary `@playwright/test` retain the production five-minute cache.
- `RematchJobWatcher` independently invalidates `contactKeys.detail` on job completion, which can mask a missing invalidation. The test must avoid triggering a rematch, or assert none occurred.

## Spec updates

Both domains are `e2e_settled: true`; per `spec/README.md` a new or changed `surface: ui` then-item must land with its citing E2E test in the same PR.

- `spec/imports-matching.yaml` — linking a candidate refreshes the contact detail surface.
- `spec/contacts.yaml` — contact-method mutation semantics move to the new endpoints; `CON-008` (method replacement gated on the `methods` field being present) is retired or amended in step with the PUT branch.

## Out of scope

- **Atomizing `PUT /contacts/:id`** into per-property endpoints. Directionally intended (premise 3) but not this work; this design only ensures methods move the right way.
- **Reconcile coverage for calendar-attendee rows**, and backfill of the 2 `matched` gaps. A behavior change to a matching subsystem with its own semantics, not on the path of the reported bug. File separately.
- **`profile_photo` being nulled on every edit-form save** — same defect class, same endpoint, tracked as issue #691 and deliberately descoped by the user. `how_met` is unaffected: `applyUpdate` deliberately no-ops it when absent, pinned by `CON-006`.
- **Optimistic concurrency on any endpoint.** Retired by premise 2. If concurrent writers ever appear, this is where to start.

## Design history

The original version of this document specified optimistic concurrency (`ETag` + `If-Match`) on `PUT /contacts/:id`. Two rounds of plan review showed the approach expanding across five subsystems to defend a primitive that should be removed, and surfaced two errors in the design itself: an ETag enumerated over an incomplete set of PUT-written fields, and a compatibility carve-out protecting a client population that does not exist. The approach was re-evaluated at the design level and replaced with the present one. The rejected alternatives above record what was considered and why it lost, so the reasoning is not re-litigated from scratch.
