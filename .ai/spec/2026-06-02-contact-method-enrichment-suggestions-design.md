# Contact-Method Enrichment Suggestions — Functional & Technical Design

**Status:** Draft v1
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
| Surface | Fold into the existing People-import tab + `ImportLinkModal` | The modal already does link-to-existing + multi-method selection + conflict resolution; reuse muscle memory and code |
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
            └─ ImportLinkModal: link | import | ignore
                 ├─ MethodSelector (multi-select emails/phones — multiple methods native)
                 ├─ ConflictResolver (method/profile conflicts)
                 └─ link → EnrichContactFromExternalWithSelections → CreateContactMethod
                      └─ KindContactMethodsAdded → RematchService fan-out → GmailRematchHandler backfill
```

**Reused as-is:** `external_contact` table, `ImportMatchService`, the People-tab candidate list + `CandidateCard`, `ImportLinkModal` (+ `MethodSelector`, `ConflictResolver`, `ContactSelector`), `EnrichmentService`, `CreateContactMethod`, `KindContactMethodsAdded` + rematch backfill, sticky ignore (`match_status='ignored'`).

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

- **`external_contact`** — reused, additive columns only (still no new table). New `source` value `'gmail_correspondence'`. `match_status`, `crm_contact_id`, `emails[]`, `phones[]`, `metadata` (evidence) all reused. `(host_id, source, source_id)` uniqueness gives per-address dedup; `match_status='ignored'` gives row-level sticky ignore. Two additive nullable JSONB columns — `pending_method_suggestions` and `dismissed_method_suggestions` — carry the address-book leak fix's suggestion/dismissal STATE; they are deliberately absent from the producer `UpsertExternalContact` column list so address-book resyncs never overwrite them (the `metadata` column, by contrast, is replaced wholesale on every upsert).
- **No new table.** The earlier "suggestion table" idea is dropped in favor of the existing candidate model plus the two additive columns above.

### New components

| Component | Location (new unless noted) | Responsibility |
|-----------|------------------------------|----------------|
| `GmailCorrespondenceSuggester` | `backend/internal/google/` | Reads `comms_message` participants, gates on the name-match, upserts `gmail_correspondence` external_contact rows |
| Re-enrich-on-resync | `backend/internal/google/contacts.go` + `internal/service/enrichment.go` (modify) | Auto-propagate for matched; emit suggestions for imported |
| Source label + link-only | frontend `ImportLinkModal` / source-display (modify) | Hide import for `gmail_correspondence`; label + evidence display |

### Reused components

`ImportMatchService`, `ImportLinkModal` / `MethodSelector` / `ConflictResolver` / `ContactSelector`, `EnrichmentService` (`EnrichContactFromExternal*`), `CreateContactMethod`, `KindContactMethodsAdded` + `RematchService` + `GmailRematchHandler`, the People-tab candidate list and ignore flow.

---

## 6. UX

`gmail_correspondence` candidates appear in the existing People-import tab as cards showing the suggested contact, confidence, and evidence (observed display name, co-occurring known contact, message count), resolved through the existing modal as **link** or **ignore** (no import). Address-book user-imported-gap suggestions appear similarly, as "missing methods" on an already-linked contact (link → `MethodSelector` pre-selects the gap methods).

This is a **UI-bearing** feature — unlike the Gmail backend phases, its PR requires human review and must not be auto-merged.

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
