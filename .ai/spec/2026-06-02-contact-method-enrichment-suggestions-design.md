# Contact-Method Enrichment Suggestions — Functional & Technical Design

**Status:** Draft v2
**Last Updated:** 2026-06-02
**Related:** Gmail integration (`.ai/spec/2026-06-01-gmail-integration-design.md`, issue #70); this is a follow-on. The "new-contact discovery" / auto-contact-enrichment program is a SEPARATE future thread.

---

## 1. Overview

### What

A pluggable, multi-source system that proposes additional contact **methods** (email / phone) for **existing** CRM contacts and lets the user confirm them — folded into the **existing People-import tab + import/link modal**, not a new surface. It **never creates new contacts**. v1 ships two signal sources plus a root-cause fix:

1. **`gmail_correspondence`** — addresses people actually email the user from that aren't on the contact's record (and aren't in any address book).
2. **Address-book gap + leak fix** — methods already imported into gcontacts/icloud that never propagated to the linked CRM contact, plus a fix for *why* that gap forms.

### Why

- The Gmail integration ingests mail for **known addresses only** (match-or-skip), so a contact who corresponds from an address not yet on their record is invisible. Empirically validated on the user's main account: a 217-message sample surfaced ~30 strong candidates, ~28 of them real — addresses the structured address books structurally cannot provide.
- The gcontacts/icloud importer enriches a contact **once, at first match**, then never re-propagates. ~13 email + ~14 phone methods already sit un-applied on linked contacts today, and more accrue every time an address book gains a method for an already-linked person.

### Key decisions

| Decision | Choice | Rationale |
|----------|--------|-----------|
| Surface | Fold into the existing People-import tab + `SuggestionModal` (renamed from `ImportLinkModal`) | The modal already does link-to-existing + multi-method selection + conflict resolution; reuse muscle memory and code |
| Resolution surface | **One review surface (the People tab) fed by two producers** — method suggestions render as a group **above** the existing confidence-ranked candidates, in the same list | Preserves the shipped candidate confidence-sort and needs no new timestamp. Accepted consequence: B's suggestions cluster at top while C's correspondence candidates stay interspersed by confidence, so the two sources don't render identically (chosen over a recency-ordered queue, which would change candidate ordering). Detailed in §6. |
| Read-model | A new `SuggestionItem` union (`kind: 'contact' \| 'method'`) at the **read layer** that *wraps* the existing `ImportCandidate` as the `contact` variant; `ImportCandidate` / `CandidateCard` / `useImportCandidates` unchanged | A method suggestion is not an unmatched candidate (fixed contact, no import, per-method dismiss, different lifecycle); bolting `kind` onto `ImportCandidate` would overload it. The union keeps the contact path untouched. |
| Endpoint | **New** composing read endpoint `GET /imports/suggestions` for the unified list; the shipped `/imports/candidates` list + its `{id}` import/link/ignore action routes are **left untouched**; method actions are new routes under `/imports/suggestions` | Avoids dragging stable, E2E-covered import/link/ignore behavior into the blast radius for a cosmetic gain; method suggestions need their own actions regardless. |
| Modal | `SuggestionModal` = a thin **shell** (renamed from `ImportLinkModal`) hosting two resolver bodies (contact-candidate, method-suggestion) that share `MethodSelector` / `ConflictResolver` | One mode-heavy component would leak controls (cadence / name / import) into modes where they don't apply; separate bodies keep the boundary clean. |
| Link-only enforcement | Allowed actions per source/kind enforced **server-side**, not just hidden in the UI | The import endpoint would otherwise import any unmatched external_contact, violating C's "never create new contacts" — that non-goal is data-model policy, not presentation. |
| Storage | Reuse `external_contact` (`source` tag) — **no new table** | `ImportMatchService` already computes `suggested_match` for any external_contact via trigram + method-overlap; a new source is just rows with a new `source` |
| Extensibility | New enrichment sources = producers that write `external_contact` rows with a new `source` | Future sources (calendar, signatures, manual, AI) plug in without touching the surface or resolve flow |
| Scope | Methods for **existing** contacts only | Gmail is not a new-contact discovery source; that is the separate future program |
| Gmail signal | Co-participants of already-ingested known-contact threads (stored `comms_message`) | Zero new fetching; rides the sync; privacy-pure |
| Gmail match gate | Display-name trigram sim ≥ **0.60** AND display name has **≥2 tokens** | Validated ~93% precision (28/30) at this gate; everything below 0.60 was a net-new person, not the matched contact |
| Gmail resolution | **Link-only** (import/new-contact hidden for this source) | Honors "Gmail never creates new contacts" |
| Address-book auto-matched gap | **Auto-propagate** new methods on resync | No human ever made a selection on auto-matched entries, so an un-applied method is a genuine gap — safe, zero friction |
| Address-book user-imported gap | **Suggest** (sticky-ignore) | A missing method may be an intentional deselection; let the user decide once |
| Confirm → enrich → backfill | Link adds the method → `KindContactMethodsAdded` → `GmailRematchHandler` backfills that email's history | Reuses the existing rematch machinery; a confirmed address auto-pulls its past correspondence |

### Non-goals (v1)

- Creating new contacts from any source (the medium/weak Gmail matches that are net-new people seed the *future* program, not this one).
- Per-contact "find this person's addresses" name search (the future program's mechanism 2).
- Broad metadata scan of all mail.
- AI / signature parsing.

### Production scale (measured)

451 contacts; 331 distinct known email addresses; 4 connected Google accounts. Gmail: a 217-message known-contact sample (5 months) yielded ~30 strong / 4 medium / 2 weak candidates. Address-book gap: ~13 email + ~14 phone methods un-applied across ~20 contacts, splitting roughly evenly between auto-`matched` entries (safe to auto-apply) and user-`imported` entries (suggested). (The per-status tallies overlap slightly where a person has entries in both address books, so they do not sum to the totals.)

---

## 2. The pluggable spine (already built — reuse)

The entire candidate → suggest → resolve → enrich pipeline already exists for gcontacts/icloud/gcal_attendee/telegram. A new enrichment source plugs into it by writing `external_contact` rows with a new `source` value; everything downstream is unchanged:

```
producer writes external_contact (source=<new>, display_name, emails[]/phones[])
  └─ ImportMatchService.FindBestMatchesBatch  → suggested_match (trigram name sim + method overlap)
       └─ People-tab CandidateCard (suggested contact + confidence + evidence)
            └─ SuggestionModal: link | import | ignore
                 ├─ MethodSelector (multi-select emails/phones — multiple methods native)
                 ├─ ConflictResolver (method/profile conflicts)
                 └─ link → EnrichContactFromExternalWithSelections → CreateContactMethod
                      └─ KindContactMethodsAdded → RematchService fan-out → GmailRematchHandler backfill
```

**Reused as-is:** `external_contact` table, `ImportMatchService`, the People-tab candidate list + `CandidateCard`, `SuggestionModal` (+ `MethodSelector`, `ConflictResolver`, `ContactSelector`), `EnrichmentService`, `CreateContactMethod`, `KindContactMethodsAdded` + rematch backfill, sticky ignore (`match_status='ignored'`).

**Genuinely new code:** (a) the `gmail_correspondence` producer + its match gate; (b) the re-enrich-on-resync path + imported-gap suggestion emission in the contacts provider/enrichment; (c) a source-display label and the link-only restriction for `gmail_correspondence` in the modal.

---

## 3. Source: `gmail_correspondence`

This source — mining co-participants of already-ingested known-contact threads to discover existing contacts' un-recorded email addresses — has its own implementation-ready spec: **`.ai/spec/2026-06-02-gmail-correspondence-enrichment-design.md`**. In brief: it reads `from`/`to`/`cc`/`bcc` from stored `comms_message` (no new fetching), gates on a display-name trigram sim ≥ 0.60 with a ≥2-token name (validated ~93% precision), upserts `external_contact` rows (`source='gmail_correspondence'`, `source_id=`address), and resolves **link-only** (no new contacts) through the spine in §2. It is gated on the category-filter fix shipping + a `comms_message` backfill (see §7).

---

## 4. Source: address-book gap + leak fix

### Root cause

`ContactsProvider.attemptMatch` enriches a contact **once**, at the moment of first auto-match (`EnrichContactFromExternal`, all methods), then early-returns on every subsequent sync (`if contact.CRMContactID != nil { return nil }`). So when an address book later adds a method for an already-linked person, the resync updates `external_contact.emails[]/phones[]` but **never re-propagates** it. A second, *intentional* population exists: user-driven link/import via the modal uses `EnrichContactFromExternalWithSelections`, so a method the user **deselected** is "missing" by design and must not be re-pushed.

### Forward fix (closes the gap going forward)

On resync, after upserting an already-linked `external_contact`, reconcile its methods against the CRM contact:

- **Auto-`matched` entries:** auto-propagate any `external_contact` method missing from the contact. No human ever made a selection here, so an un-applied method is a genuine gap — safe and frictionless. (Reuses the auto enrichment path — `EnrichContactFromExternal`, NOT `SyncMethodsFromExternal`, so the newly-added method fires `KindContactMethodsAdded` → rematch like any other.)
- **User-`imported` entries:** emit the missing method as a **suggestion**, recorded in a dedicated `external_contact.pending_method_suggestions` JSONB column (NOT `metadata` — the producer upsert replaces `metadata` wholesale on every resync, so suggestion state stored there would be erased by the next address-book sync). The user confirms or dismisses once. A dismissal is recorded **per-(type,value)** in a second dedicated column `dismissed_method_suggestions` (NOT row-level `match_status='ignored'`, which would unlink the whole entry and suppress all future legit suggestions); the forward reconcile and the suggestion list both subtract dismissed entries, so a deselected method never nags again.

To make `match_status` a sound auto-vs-suggest discriminator, `LinkContact` is changed to set `match_status='imported'` whenever the link request engaged the modal's curation controls (method selections, cadence, name, or conflict resolutions) — the same signal the handler already uses to pick the `WithSelections` enrichment path. A bare link / auto-match with no curation signal stays `matched`. (One rare residual — opening the modal, deselecting ALL methods, and linking with no other curation signal — is closed in the suggestions-surface PR via an explicit-empty-selection payload.)

### One-time catchup

Running the fixed logic once over existing data auto-applies the auto-matched portion and surfaces the user-imported portion as suggestions — ~13 emails + ~14 phones total, split roughly evenly between the two.

### Note

This leak fix is **separable** — it touches only the contacts provider / enrichment and can ship independently of (and ahead of) the Gmail source.

---

## 5. Data & components

### Data model

- **`external_contact`** — reused, additive columns only (still no new table). New `source` value `'gmail_correspondence'`. `match_status`, `crm_contact_id`, `emails[]`, `phones[]`, `metadata` (evidence) all reused. The existing unique index `(source, source_id, COALESCE(account_id, ''))` gives per-address dedup; `match_status='ignored'` gives row-level sticky ignore. Two additive nullable JSONB columns — `pending_method_suggestions` and `dismissed_method_suggestions` — carry the address-book leak fix's suggestion/dismissal STATE; they are deliberately absent from the producer `UpsertExternalContact` column list so address-book resyncs never overwrite them (the `metadata` column, by contrast, is replaced wholesale on every upsert).
- **JSONB is v1-scoped.** The two columns model address-book imported-gaps only. They are deliberately NOT a general suggestion store — once method suggestions go multi-source or need independent ordering / provenance / undo, promote to a normalized `method_suggestion` table. `SuggestionItem` (§6) is a **composed read-model**, not a persisted entity.
- **No new table.** The earlier "suggestion table" idea is dropped in favor of the existing candidate model plus the two additive columns above (within the v1 scope noted above).

### New components

| Component | Location (new unless noted) | Responsibility |
|-----------|------------------------------|----------------|
| `GmailCorrespondenceSuggester` | `backend/internal/google/` | Reads `comms_message` participants, gates on the name-match, upserts `gmail_correspondence` external_contact rows |
| Re-enrich-on-resync | `backend/internal/google/contacts.go` + `internal/service/enrichment.go` (modify) | Auto-propagate for matched; emit suggestions for imported |
| `SuggestionItem` read-model + composing endpoint | `backend/internal/repository` + `internal/service` + `api/handlers` (new) | New sqlc query for already-linked external_contacts with non-empty `pending_method_suggestions` (minus dismissed); new `GET /imports/suggestions` composing the method-suggestion group **above** the existing confidence-ranked candidates, returning a `SuggestionItem` union (`kind: 'contact' \| 'method'`); `ImportCandidate` and `/imports/candidates` unchanged |
| Resolve / dismiss method routes | `backend/internal/api/handlers` + service (new) | New routes under `/imports/suggestions`; confirm selected pending methods (→ `EnrichContactFromExternalWithSelections`, clear from pending) and dismiss selected/all (→ `dismissed_method_suggestions`, sticky) — idempotent, re-checking current contact methods + liveness, pending/dismissed as set ops |
| Server-side link-only policy | `backend/internal/api/handlers` + service (modify) | Allowed actions per source/kind enforced on the backend (`gmail_correspondence` cannot be imported as a new contact); UI reflects server-declared capability |
| `SuggestionModal` shell + two resolver bodies | frontend `imports/page.tsx` (`CandidateCard`) + `ImportLinkModal`→`SuggestionModal` (modify) | Thin shell hosting the contact-candidate body (today's import/link/ignore + link-only config) and the method-suggestion body (enrich-locked: contact fixed, no import/selector), sharing `MethodSelector` / `ConflictResolver`; method-suggestion card (Review/Dismiss); explicit empty-selection payload for the §4 residual |
| `useSuggestions` hook + suggestions API | frontend `imports-api.ts` + new hook (new) | Fetch `/imports/suggestions` and render the grouped list; `useImportCandidates` / `/imports/candidates` / query keys unchanged |

### Reused components

`ImportMatchService`, `SuggestionModal` / `MethodSelector` / `ConflictResolver` / `ContactSelector`, `EnrichmentService` (`EnrichContactFromExternal*`), `CreateContactMethod`, `KindContactMethodsAdded` + `RematchService` + `GmailRematchHandler`, the People-tab candidate list and ignore flow.

---

## 6. UX — one review surface, two producers

The People-import tab is **one review surface fed by two producers**. A new read-model **`SuggestionItem` union** represents a queue entry; it *wraps* the existing `ImportCandidate` rather than bolting a field onto it (the candidate path stays exactly as-is):

- **`kind: 'contact'`** — the existing unmatched-candidate flow (**import / link / ignore**); payload = today's `ImportCandidate`. Every current source (gcontacts, icloud_contacts, gcal_attendee, telegram, anarlog_humans) is this kind. `gmail_correspondence` (C) is this kind too, flagged **link-only** — it already lives in the candidate path natively via `ImportMatchService.suggested_match`.
- **`kind: 'method'`** — a method suggestion for an **already-linked** contact (**confirm / dismiss**); payload = the linked contact (id + name), the originating address book (`source`), and the pending `(type, value)` methods (from `pending_method_suggestions` minus `dismissed_method_suggestions`).

**Ordering.** Candidates keep their existing **confidence ranking** (shipped behavior, unchanged). Method suggestions — 100% certain, since the contact is already linked — render as a **group at the top** of the same list, above the confidence-ranked candidates. Accepted consequence: B's method suggestions cluster at the top while C's correspondence candidates stay interspersed among candidates by confidence, so the two method-discovery sources don't render identically. This was chosen over a recency-ordered review queue, which would have changed candidate ordering and required a per-suggestion timestamp the data model doesn't have. Existing source-filter chips still apply; the card's kind-variant is what visually distinguishes a suggestion. No new filter chip (YAGNI).

**The card.** A `method`-kind card reads "‹contact name› — N new methods", lists the discovered values and the originating address book, and exposes **Review** (opens the modal) and **Dismiss** (sticky). The `contact`-kind card is unchanged.

**Endpoint.** A **new** composing read endpoint `GET /imports/suggestions` returns the unified list (the method-suggestion group + the existing candidates, as a `SuggestionItem` union). The shipped `/imports/candidates` list and its `{id}` **import / link / ignore** action routes are **left untouched** — off the blast radius. Method-suggestion actions are **new** routes under `/imports/suggestions/{id}/methods/{resolve,dismiss}` (exact shapes a planning detail). Frontend gains a `useSuggestions` hook against the new endpoint; `useImportCandidates` and its query keys are unchanged.

**The modal — a shell with two resolver bodies.** `ImportLinkModal` is **renamed `SuggestionModal`** and becomes a thin **shell** hosting two resolver bodies that share `MethodSelector` / `ConflictResolver`, rather than one mode-heavy component branching internally (which would leak controls like cadence / name / import into modes where they don't apply):

- **Contact-candidate body** — today's import / link / ignore flow, including the **link-only** config for `gmail_correspondence` (Import hidden, `suggested_match` pre-selected but changeable). Resolves via the existing `/imports/candidates` link endpoint.
- **Method-suggestion body** — **enrich-locked**: the target contact is fixed (no `ContactSelector`; a static "Adding to ‹contact›" header), Import is absent. The pending methods feed `detectMethodConflicts` against the contact's current methods, so the three-bucket comparison (Will be added / Already in CRM / Conflicts to resolve) and `ConflictResolver` work unchanged. **Confirm** → `/imports/suggestions/{id}/methods/resolve` → `EnrichContactFromExternalWithSelections` on the already-linked contact → `CreateContactMethod` → `KindContactMethodsAdded` → rematch backfill, then clear those entries from `pending_method_suggestions`. **Dismiss** (whole-card or per-method) → `/imports/suggestions/{id}/methods/dismiss` → record the `(type, value)` in `dismissed_method_suggestions` and drop from pending; the forward reconcile (§4) and the listing both subtract dismissed, so a deselected method never nags again.

**Link-only is backend policy, not just hidden UI.** Allowed actions per source/kind are enforced **server-side**: the import endpoint would otherwise import any unmatched external_contact, so hiding the Import button alone would not stop a crafted request from creating a new contact from a `gmail_correspondence` row — violating C's core non-goal. The UI reflects server-declared capabilities.

**Resolve/dismiss lifecycle.** Both actions are **idempotent** and re-check live state at resolve time — the contact's current methods (a method may have been added manually or via the normal link flow while the suggestion sat pending) and contact liveness (skip if soft-deleted) — and update `pending` / `dismissed` as **set operations**, never trusting a stale client row.

**Residual `match_status` fix.** The one rare gap from §4 — open the modal, deselect **all** methods, link with no other curation signal → stays `matched` when it should be `imported` — is closed here: the frontend sends an explicit empty-selection marker so the handler distinguishes "no curation" from "explicitly selected nothing."

This is a **UI-bearing** feature — unlike the Gmail backend phases, its PR requires human review and must **not** be auto-merged.

---

## 7. Sequencing & dependencies

1. **Gmail category-filter fix** (replace `category:primary` with `-category:promotions -category:social -category:updates -category:forums`) — already designed and stashed; ship it, then run a backfill so `comms_message` is populated. The `gmail_correspondence` source has no data until this lands.
2. **Address-book leak fix + one-time catchup** — separable; can ship independently (and first).
3. **`gmail_correspondence` source + UI** — after (1).

Each is its own PR through the plan → implement → review cycle; UI PRs are human-reviewed.

---

## 8. Testing strategy

- **Unit:** name-match gate (sim ≥ 0.60 and ≥2 tokens; reject bare first names / sub-threshold); dedup by address; skip-known / skip-already-ignored; the auto-vs-suggest branch by `match_status`.
- **Integration:** producer over seeded `comms_message` → `gmail_correspondence` external_contact rows with the expected `suggested_match`; re-enrich-on-resync auto-propagates for `matched` and emits a suggestion for `imported`; link → `EnrichContactFromExternalWithSelections` → `CreateContactMethod` → `KindContactMethodsAdded` → rematch backfill; sticky-ignore prevents re-suggestion.
- **E2E:** a `gmail_correspondence` candidate appears in the People tab, link adds the method, import is not offered for this source.
- **Precision guardrail:** a test pinning the 0.60 / ≥2-token gate so a future loosening is a conscious change.
- Repo rules: `accelerated.GetCurrentTime()`, sqlc-only (incl. fixtures), no PII in tests.

---

## 9. Open questions & future work

- **New-contact discovery program** (the medium/weak net-new matches; per-contact name search / mechanism 2) — a separate future thread that reuses this same suggest/resolve spine but adds the `import` action and a name-search producer.
- **Confidence display / queue sorting** — how prominently to show match confidence and evidence in the card.
- **Phone normalization** for the gap match (the catchup used last-10-digit matching; the production reconciliation should use the repo's phone normalizer).
- **AI / signature parsing** as a later high-signal source plugging into the same spine.
