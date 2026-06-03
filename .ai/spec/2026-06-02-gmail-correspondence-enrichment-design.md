# Gmail Correspondence Enrichment — Functional & Technical Design

**Status:** Draft v1
**Last Updated:** 2026-06-02
**Part of:** Contact-method enrichment suggestions (`.ai/spec/2026-06-02-contact-method-enrichment-suggestions-design.md`) — that umbrella doc owns the shared, pluggable spine (a producer writes `external_contact` rows with a `source` tag → `ImportMatchService` computes `suggested_match` → People-tab `CandidateCard` → `ImportLinkModal` link/import/ignore + `MethodSelector` + `ConflictResolver` → `EnrichContactFromExternalWithSelections` → `CreateContactMethod` → `KindContactMethodsAdded` → `GmailRematchHandler` backfill). This doc details the `gmail_correspondence` **source** only.
**Depends on:** the Gmail category-filter fix shipped AND a `comms_message` backfill run — until then this source has no input data (see §2 / §11).

---

## 1. Overview

### What

A producer that mines the **participants of already-ingested known-contact email threads** to discover email addresses that belong to **existing** CRM contacts but aren't on their record, and surfaces each as a **link** candidate in the existing People-import tab. It never creates new contacts; it never fetches mail beyond what the sync already pulled.

### Why

The Gmail integration ingests mail for **known addresses only** (match-or-skip), so a contact who emails from an address not yet on their record is invisible — and the structured address books (gcontacts/icloud) can't supply addresses that were never saved. Mining the mail stream is the only signal for these. Validated on the user's main account: a 217-message known-contact sample surfaced ~30 strong candidates, ~28 of them real.

### Non-goals

- No new-contact creation (addresses whose display name does **not** strong-match an existing contact are out of scope — those net-new people are the separate future discovery program).
- No new Gmail fetching, no broad scan, no per-contact name search, no AI/signature parsing.

---

## 2. Input & dependency

Reads the `to` / `cc` / `bcc` participant lists already stored in `comms_message.source_metadata` for ingested known-contact messages (plus the `From`). **No new Gmail API calls** — it rides whatever the email sync already fetched.

Hard dependency: `comms_message` is empty until the **category-filter fix** (`category:primary` → `-category:promotions -category:social -category:updates -category:forums`) ships and a backfill runs. Until then this producer is correct but yields nothing.

---

## 3. Algorithm

1. Load the **known-address set** (every email `contact_method.value_normalized`) and the **own-account set** (connected-account addresses from `external_sync_state` where `source='email'`).
2. Over ingested known-contact messages (bounded by a watermark or a recent window), collect every participant `(display_name, address)` from From/To/Cc/Bcc.
3. Drop addresses that are known or own-account.
4. For each remaining **unknown** address whose display name has **≥2 tokens** (a full name, not a bare first name), trigram-match the display name to CRM contacts (reuse `contactRepo.FindSimilarContacts`). If the best similarity is **≥ 0.60**, it qualifies.
5. Skip if the address is already a `contact_method` on any contact, or an `ignored` `gmail_correspondence` row already exists for it (sticky ignore).
6. Upsert an `external_contact`: `source='gmail_correspondence'`, `source_id=<normalized address>`, `display_name=<observed name>`, `emails=[address]`, `host_id=<Pi host>`, `metadata={ display_names_seen, co_occurring_contact, message_count }`.

`source_id = address` gives natural per-address dedup. The existing `ImportMatchService` recomputes `suggested_match` at list time from the same trigram + method-overlap scoring used by every source, so no bespoke suggestion plumbing is needed.

---

## 4. Matching gate (empirically calibrated)

**sim ≥ 0.60 AND display name has ≥2 tokens.** On the validation sample this gate gave ~93% precision (28/30 strong candidates real); everything below 0.60 was a *net-new* person spuriously partial-matching an existing contact, and bare first names are the main noise source. A regression test pins this gate so any future loosening is a conscious choice. Co-occurrence of the unknown address with the *matched contact's own* known address (strongest "this is their alternate address" signal) is computed as a confidence/sort booster, not a gate.

---

## 5. Resolution flow

The candidate appears in the People tab with its auto-computed suggested contact and evidence (observed display name, co-occurring contact, message count). The user **links** it → `MethodSelector` adds the email → `KindContactMethodsAdded` → `GmailRematchHandler` backfills that address's history from the backfill floor. **Import is hidden** for this source (no new contacts). **Ignore** is sticky.

Multiple addresses for one person produce multiple rows, each independently confirmable (native to per-address rows + `MethodSelector`).

---

## 6. Generation cadence

A producer that runs after the email sync (or on a periodic tick) over recently-ingested `comms_message`. Idempotent: re-runs upsert the same `(host_id, source, source_id)` rows and skip already-known / already-ignored addresses. Decision deferred to planning: an email-ingestion consumer vs. a scheduled reconciliation pass — both read the same stored participants; a scheduled pass is simpler to re-tune and decouples discovery from the ingestion hot path.

---

## 7. Components

| Component | Location (new unless noted) | Responsibility |
|-----------|------------------------------|----------------|
| `GmailCorrespondenceSuggester` | `backend/internal/google/` | Reads `comms_message` participants, applies the §4 gate, upserts `gmail_correspondence` `external_contact` rows |
| Link-only restriction + source label | frontend `ImportLinkModal` / source-display (modify) | Hide `import` for this source; label + evidence display |

Reused (no change): `ImportMatchService`, `ImportLinkModal` / `MethodSelector` / `ConflictResolver`, `EnrichmentService`, `CreateContactMethod`, `KindContactMethodsAdded` + `RematchService` + `GmailRematchHandler`, sticky ignore.

---

## 8. Data

`external_contact` reused unchanged: new `source` value `'gmail_correspondence'`; `source_id` = normalized address; evidence in `metadata`. No schema change, no new table.

---

## 9. Guardrails

sim/token gate (§4); skip addresses already on a contact; sticky ignore (`match_status='ignored'`); link-only (no new contacts); dedup by `source_id`; fully reversible (unlink). The producer writes only `external_contact` candidate rows — it never mutates a contact directly; all contact changes go through the user-confirmed link flow.

---

## 10. Testing strategy

- **Unit:** the §4 gate (sim ≥ 0.60 and ≥2 tokens; reject bare first names and sub-threshold); dedup by address; skip-known / skip-already-ignored; participant extraction from `comms_message.source_metadata`.
- **Integration:** producer over seeded `comms_message` → `gmail_correspondence` rows with the expected `suggested_match`; link → `EnrichContactFromExternalWithSelections` → `CreateContactMethod` → `KindContactMethodsAdded` → `GmailRematchHandler` backfill; sticky-ignore prevents re-suggestion; `import` not offered.
- **E2E:** a `gmail_correspondence` candidate appears in the People tab and link adds the method.
- Repo rules: `accelerated.GetCurrentTime()`, sqlc-only (incl. fixtures), no PII in tests/fixtures.

---

## 11. Sequencing

This source is **gated on the category-filter fix being shipped and a `comms_message` backfill having run** (otherwise no input). It reuses the suggestion spine, so landing the address-book source/leak-fix first proves that spine end-to-end before this plugs in. It is **UI-bearing** (People-tab cards + the link-only restriction), so its PR requires human review and must not be auto-merged.

---

## 12. Open questions & future work

- Generation cadence: ingestion consumer vs. scheduled pass (§6).
- Confidence display / queue sorting and how prominently to show evidence.
- Whether co-occurrence-with-the-matched-contact's-own-address should graduate from a sort booster to a gate if precision needs tightening.
- This source's gate is the bridge to the future **new-contact discovery program**: the sub-0.60 / no-match correspondents (net-new people) would feed that program's `import` path, reusing this same producer with a different resolution action.
