# Gmail Correspondence Discovery — Run the Gate on the Fetched Set (Between Fetch and Storage)

**Status:** Draft v1 — ready for /plan-and-ship
**Date:** 2026-06-03
**Builds on (all merged):** the `gmail_correspondence` source (#398), the backfill-cursor fix (#401), and the People-tab suggestion surface / spine (#397). This is a follow-on fix to make that source actually yield candidates.

---

## Problem (verified this session)

The `gmail_correspondence` producer (`backend/internal/google/gmail_correspondence.go`) mines **stored `comms_message` rows** for unknown participants whose display name trigram-matches an existing contact (gate: similarity ≥ 0.60 AND ≥2-token name). In production it surfaces **0 candidates**.

Root cause — the Gmail sync applies **two filters**, and the producer only sees the output of the second:
1. **Fetch query** (`gmail.go` `buildORChunks`/`buildWindowORChunks` + `gmailCategoryFilter`): `-category:promotions -social -updates -forums after/before` + `(from:a OR to:a OR cc:a OR bcc:a)` over all contact email addresses. For the main account this returns **~221 messages** over the Jan→now window.
2. **Storage gate** (`gmail.go` `processMessage`, ~L997-1044): stores only **clean you↔contact** mail (inbound: sender is a contact AND a own-account address is a recipient; outbound: you're the sender). This drops **~85%** (221 → ~32 stored for the main account).

The discovery candidates live in the **multi-party threads the storage gate discards**. **Validated empirically:** running C's exact gate against the full 221-message fetch set yields **27 net-new candidate addresses across 26 existing contacts** (24/27 exact name matches, sim = 1.000), vs **0** from the stored 32. (Detail in the git-ignored `temp/gmail-221-discovery.txt`.)

So the discovery logic is correct; it's pointed at the wrong data. **It must run between fetch and storage, not on the stored remnant.**

---

## The fix

Run the discovery gate on **every fetched message's participants**, independent of the storage gate.

**Preferred design — in-sync hook (no extra Gmail fetching):** in the sync's per-message process loop, where each fetched message is already parsed, additionally run the discovery gate on its From/To/Cc participants (the message is already fetched + parsed for `processMessage`). This is the literal "between fetch and storage" placement and adds **zero** new Gmail API calls. It covers steady-state (future windows) automatically and the historical backlog *whenever the windowed sync replays that range*.

**Backlog caveat (resolved in planning):** the in-sync hook does NOT auto-replay the Jan→now backlog for accounts whose cursor has already advanced to ~now (the production main accounts are there). The hook only examines what the fetch loop actually fetches, and at steady state that's only new leading-edge windows. To surface the historical backlog, the operator runs the **already-existing** `crm-admin --reset-gmail-backfill-cursors` ONCE after deploy — it rewinds enabled Gmail cursors to the 2026-01-01 floor so the windowed catch-up replays the full range with the hook live. This is a one-time runbook step (no new code); see the execution plan's deploy runbook for the exact sequence (including the retired-River-kind drain).

The existing `GmailCorrespondenceSuggester` (which mines `comms_message`) is **superseded** as the candidate source — planning to decide whether to remove it or keep a thin fallback.

---

## Reuse (no new surface, no new fetching)

- **Gate (unchanged):** sim ≥ 0.60 AND ≥2-token display name; trigram via `contactRepo.FindSimilarContacts` (`db/queries/contact.sql:481-503`). Drop known `contact_method` addresses + own-account addresses.
- **Output (unchanged):** upsert `gmail_correspondence` `external_contact` candidate rows (per-address dedup; sticky-ignore via `match_status='ignored'`).
- **Surface (unchanged):** candidates flow through the existing People-tab suggestion surface (#397) → link → `EnrichContactFromExternalWithSelections` → `KindContactMethodsAdded` → `GmailRematchHandler` backfill. **No UI change.**

---

## Scope & explicit non-goals

- **No new Gmail fetching** — piggyback the sync's existing fetch. Same query, same mail; only the *examination* widens (all fetched participants, not just stored ones). No privacy expansion beyond what the sync already fetches.
- **From/To/Cc only.** **BCC is out of scope and structurally unrecoverable here:** Gmail strips the `Bcc` header from the stored sent copy — confirmed empirically (a sent message returned To only at `Format("full")`/`raw`/`metadata`). BCC recipients can only be recovered via Google Workspace delivery logs (Reports/Audit API or BigQuery export, ~6-month retention, super-admin) or a forward-only admin routing rule — a **separate future effort**, not this fix.
- **Pure 1:1 net-new mail** (a contact emailing from a new address with no other known contact on the message) is never *fetched* (no on-file address to match the query) → also out of reach here. The 27 validated candidates all co-occur with a known contact in a fetched thread.

---

## Acceptance

After deploy **plus the one-time `crm-admin --reset-gmail-backfill-cursors` runbook step** (which rewinds the already-advanced cursors so the windowed catch-up replays Jan→now with the hook live), the catch-up surfaces on the order of the validated **~27 candidates / 26 contacts** for the main account (it will differ as mail changes; the signal is "tens, not zero"). Steady-state ticks thereafter discover from new windows automatically. Candidates appear in the People-tab suggestion surface for confirm/dismiss.

---

## Tests

- **Unit:** gate over fetched participants including a multi-party message (known contact + an unknown address whose ≥2-token name matches a different contact → candidate). Reject sub-0.60 and single-token names. Dedup by address. Skip known / own-account / sticky-ignored.
- **Integration (key regression):** a fetched multi-party message that does **NOT** pass the storage gate (so it isn't stored in `comms_message`) STILL yields a `gmail_correspondence` candidate — proving discovery runs between fetch and storage, not on stored rows. Plus: link a produced candidate → method added → `KindContactMethodsAdded`/rematch dispatched.
- Repo rules: sqlc-only (incl. fixtures), `accelerated.GetCurrentTime()`, layered, no PII in fixtures/tests.

---

## Open questions for planning

1. **In-sync hook vs. a separate scheduled discovery scan** (the spec prefers the in-sync hook — no extra fetch; the planner should confirm against the sync loop's structure and the windowed-cursor interaction).
2. **Remove vs. keep** the existing `comms_message`-mining `GmailCorrespondenceSuggester` once discovery moves into the fetch loop.
3. **Idempotency / cadence:** the in-sync hook re-examines each window's fetched mail; ensure upserts are idempotent and the per-sync cost is bounded (it already iterates these messages for `processMessage`).
4. Whether the all-accounts run (not just the main account) needs any per-account guards.

---

## References

- Validation artifacts (git-ignored, PII): `temp/gmail-221-discovery.txt` (the 27 candidates + all 221 fetched messages), `temp/gmail-comms-32.txt` (the stored 32).
- Code: `backend/internal/google/gmail.go` (`gmailCategoryFilter`, `buildORChunks`/`buildWindowORChunks`, `processMessage` storage gate ~L997-1044, the fetch loop); `backend/internal/google/gmail_correspondence.go` (current producer + gate); `db/queries/contact.sql:481-503` (name-trigram match).
- Prior specs: `.ai/spec/2026-06-02-gmail-correspondence-enrichment-design.md`, `.ai/spec/2026-06-02-contact-method-enrichment-suggestions-design.md`.
- BCC-recovery (separate future effort): Workspace Reports/Audit API (`Activities.list`, application `gmail`) or BigQuery Gmail log export expose envelope recipients incl. BCC; ~6-month retention; super-admin + `admin.reports.audit.readonly`; or a forward-only admin routing/compliance rule. Stored-message routes (Gmail API any format, Vault, Takeout, IMAP) cannot recover BCC.
