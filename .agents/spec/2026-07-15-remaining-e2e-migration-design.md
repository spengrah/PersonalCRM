# E2E surface settlement: relax + cite the whole Playwright suite to unblock #425

Status: DESIGN (2026-07-15). Owner: maintainer. Parent: #380 (Piece 3 + residual Track A). Unblocks: #425.

## Why now

#425 (E2E test parallelization) is blocked on the E2E surface being settled — parallelizing a suite whose assertions are about to be relaxed means sharding work that's about to be deleted. #425 says so directly: "a full spec follows once #380's E2E surface settles." This is that spec.

#380 Track A (Piece 2, #596–604) is checked done, but it delivered the API-level handler tests and only partial Playwright relaxation. The systematic per-domain citation reached only CON/DSH/CAD (via the 2026-07-15 verifier→E2E migration, #659–662). The rest of the suite — imports (9 specs), settings (3), meetings, birthdays, navigation, etc. — is neither relaxed nor cited. And critically, cited ≠ relaxed: even migrated files retain brittle assertions (contacts.spec.ts still asserts `toHaveClass(/leading-normal/)`). So the surface is not settled, and #425 cannot proceed against it.

## Current inventory (measured 2026-07-15)

- 25 spec files, ~199 `test(` blocks.
- Assertion style, suite-wide: ~50 `waitForResponse` (data-asserting), ~237 `getByText`/`toHaveText` (copy), ~28 `toHaveClass` (CSS). Brittle density remains high — roughly consistent with #380's original "~70% UI-regression, ~5% resilient" classification (which was measured at ~21 specs; it has grown, not shrunk, and was never systematically relaxed).
- `// spec:` citations exist for CON (29), CAD (22), DSH (11), and token IMP (3), CAL (1) — 26 distinct behavior IDs. Everything else is uncited.

The dominant fact: the suite already exercises the uncovered domains (imports has 9 specs; settings, meetings, telegram-settings, settings-mac all exist). The work is overwhelmingly **relax existing tests + cite them**, not author from scratch.

## The settled target shape (definition of done)

An E2E spec is "settled" when every one of its `test(` blocks satisfies all of:

1. **Data-asserting, not UI-asserting.** Its assertions check API responses, network request params, route/URL state, DB state (via TestAPI), seeded data appearing in the right place (a contact's name/date located via `getByText` inside a role-scoped region is data, not copy), or element state via aria (`toBeDisabled`, `aria-pressed`/`aria-current` via `toHaveAttribute`) — all reached through role-based drivers (`getByRole`, accessible names). It does not assert on CSS classes, static UI copy (labels, headings, marketing/toast strings), DOM structure, element counts-in-labels, or ordering by visual position. The smell is asserting static presentation, not the `getByText` selector itself — locating or asserting dynamic seeded data by its text is fine and expected.
2. **Cited.** It carries a `// spec: <ID>[<then-index>]` comment naming the SSOT behavior then-item(s) it covers — the granularity the #659–661 migration established (e.g. `// spec: CON-045[3]`, multi-cite `// spec: CON-043[0], CON-043[1]`), not a bare behavior ID. A test that covers no SSOT behavior is either miscategorized (should be a unit/integration test) or reveals a missing behavior (file it in `spec/`).
3. **Mapped.** Its file has a `frontend/tests/e2e/test-map.json` entry binding it to an `@area`.

The suite is settled when all specs are settled AND every UI-observable SSOT behavior has ≥1 citing test (or an explicit, recorded waiver).

## The relaxation rubric (per-assertion triage)

Operationalizes #380 Track A (spec lines 81–85), refined by the cross-check against the #659–661 precedent (below). For each existing assertion, one of four verdicts:

| Verdict | When | Action |
|---|---|---|
| DELETE | Pure UI-regression: `toHaveClass`, static copy (labels/headings/toast strings), DOM structure, count-in-label, visual ordering — re-checks through the UI what a backend/unit test already covers, or checks presentation the judge should own. | Remove it. If it's the test's only assertion, remove the test. |
| MOVE | Asserts business-logic correctness that shouldn't depend on the browser (e.g. a computed score, a state transition). | Ensure an API-level/Go test covers it (author if missing); delete the E2E assertion. |
| KEEP | Genuinely resilient: the browser adds coverage no other layer has — asserts on API response / network param / route / DB state / seeded-data-in-place / aria state, for a flow that needs the frontend wired in (rematch end-to-end job→poll→refresh, overdue cross-view consistency, import state transitions). | Rewrite to role-based + data/aria-asserting if not already; add the `// spec: <ID>[<n>]` citation. |
| DROP | The then-item is neither deterministically E2E-provable NOR worth a judge intent (the DSH-005[1..3] precedent — merge/meeting-note/refocus freshness sub-items). | Record as a waived orphan under the advisory framing (a `waiver:` note on the behavior); the coverage scanner excludes it loudly, with the reason. |

**Element state → aria, and fix the app where it's missing** (maintainer decision, 2026-07-15). The #659–661 migration fell back to `toHaveClass(/bg-blue-600/)` on toggle/filter buttons because the app exposes selection only via the class — precisely the restyle-brittleness this initiative targets (rename the active class and every filter test breaks). The rubric requires selection/active state be exposed via `aria-pressed`/`aria-current` and asserted with `toHaveAttribute`, so targeted frontend a11y changes are in scope: where a behavior's state has no aria surface, add the small aria surface (which also serves the MCP/accessibility goals) rather than asserting the class. Existing `toHaveClass` assertions from #659–661 are reworked to aria as their domains are settled.

**Sanctioned technique — route-mocked frontend tests for provider-dependent states.** #661 covered the seven Todoist-backed CAD rows by mocking the provider route (`route`/`fulfill`) and asserting the real UI branch deterministically — not by dropping them. This is a KEEP technique: mock the external boundary, drive the real frontend, assert on the resulting request/UI state. Use it wherever a state depends on a live external provider.

**Deliberate exception — thin visual-regression residue that nothing else can own.** A very small number of assertions guard a real, already-bitten UI bug with no data or aria surface (descender clipping → `leading-normal`; context-menu clipping on bottom rows). These may KEEP as explicitly-commented visual guards, but they are the rare exception (see the visual-guard budget), not a license to retain copy/class assertions broadly. Prefer moving genuinely-visual concerns to the agentic judge (Track B).

## Grounding: cross-check against the #659–661 precedent

The rubric was validated against what the verifier→E2E migration (#659 CON, #660 DSH, #661 CAD) actually did (measured 2026-07-15). Confirmed as the target style: role-based drivers dominate (`getByRole` is the top locator, 55/18/27 added lines), and a real data-asserting layer exists (`waitForResponse`/`toHaveURL`/`request()`: 19/7/21). Corrections the precedent forced, now folded above:

- Citation is per-then-item (`CON-045[3]`), not a bare behavior ID.
- `getByText` is not uniformly brittle — the migration used it overwhelmingly to locate/assert seeded data (a contact's name/date in a role-scoped region); only static-copy assertions are the DELETE target.
- `toHaveClass` on toggles (8/2/0 — trending to zero) was a fallback for missing aria; the decision is to fix the app (add `aria-pressed`/`aria-current`) rather than bless the class.
- Route-mocking (`route`/`fulfill`: 4/6/13, heaviest in CAD's provider rows) is a sanctioned KEEP technique, not a reason to drop provider-dependent rows.
- DROP exists as a real category — #660 documented DSH-005[1..3] as not-deterministically-provable, accepted under the advisory framing.

## Citation + coverage-check contract (Piece 3)

- **Citation format:** `// spec: <ID>[<then-index>]` (per then-item), immediately above the `test(` or the assertion block it justifies — the exact convention #659–661 established (`// spec: CON-045[3]`; multi-cite `// spec: CON-043[0], CON-043[1]`). One test may cite multiple then-items; one then-item may be cited by multiple tests.
- **Coverage derivation:** a scanner cross-references cited `<ID>[<n>]` pairs against the SSOT (`spec/*.yaml`) and produces: **covered** (then-item cited by ≥1 test), **orphan** (UI-observable then-item, no citing test, no waiver), **waived** (a DROP with a recorded reason), **dead ID** (cited ID/then-index not in the SSOT).
- **Enforcement (the anti-drift check):** CI fails on a dead ID (always — it's a rotted reference). An orphan UI-observable behavior produces a loud warning by default and hard-fails once a domain is declared "settled" (a per-domain flag flips it from warn to block, so backfill can land incrementally without the check being toothless). Backend-only behaviors (no UI surface) are exempt by type and never counted as orphans. This mirrors the existing `test-map-coverage-check.sh` pattern (which already hard-blocks unmapped spec files).
- **"UI-observable"** is a property of the behavior, derived from its SSOT type + then-clauses: `type: ux` is always UI-observable; business-logic/api/data/invariant are UI-observable only if a then-clause asserts something a user can see in the browser. The audit classifies each behavior once; the classification is recorded in the SSOT (a `surface: ui|api|none` tag) so the scanner reads it rather than re-deriving.

## The audit (first child — produces the work-list)

A one-time pass, per domain, producing a coverage matrix:

1. Tag every SSOT behavior with `surface: ui|api|none` (UI-observable classification above).
2. For each `ui` behavior, find citing/covering E2E tests (some already exist uncited) → bucket: cited / tested-but-uncited / uncovered.
3. For each existing E2E test, run the per-assertion rubric triage → count DELETE/MOVE/KEEP/DROP.
4. Output: the per-domain relax+cite work-list + the genuine-gap backfill list + any MOVE targets needing new API/Go tests + any aria surfaces the app must add for state assertions.

The audit is where the ~70%/25%/5% split gets re-measured against the current 25-spec suite; the per-domain children execute against its output.

## Scope + sequencing

Parent (sub-issue of #380). Children, in priority order:

1. **Audit + surface tagging + scanner skeleton** — the coverage matrix and the Piece 3 scanner (warn-only initially). Produces every other child's work-list.
2. **imports relax+cite** (9 specs — the largest surface; also the intent-priority domain, so its deterministic + judge halves settle together).
3. **settings relax+cite** (settings, settings-mac, telegram-settings — includes the todoist/telegram/calendar/mac config UIs).
4. **notes-meetings-knowledge relax+cite** (meetings.spec.ts + notepad + year-less-birthday display — the repurposed #666; E2E-only, no intents).
5. **contacts/dashboard/cadence residual relaxation** — strip the brittle assertions the verifier→E2E migration left behind in the already-cited files (cited ≠ relaxed).
6. **remaining specs** — navigation, error-boundary, gchat-contact-signal, birthdays, rematch, overdue, etc.
7. **Piece 3 enforcement flip** — turn the scanner from warn to block per settled domain; wire into CI + pre-push LINT.

Each per-domain child lands independently (specs are disjoint by file). The parent is "settled" — and #425 unblocks — when children 1–7 are done and every domain's flag is flipped to block.

## Relationship to the other workstreams

- **#425** consumes the settled suite: once the surface stops moving, parallelization scopes a stable target. #425 stays a separate follow-on (it's sharding/worker/DB-isolation work, not relaxation).
- **Intents** (#663/#664/#665, Track B judge) run after / lower priority than this per the maintainer (2026-07-15). The refined intent statements are parked; imports/settings intents land alongside or after their E2E children so both halves settle together. #666 is repurposed into child 4 (E2E-only).
- **#380 Piece 4** (#606, agentic QA harness) owns the genuinely-visual quality this suite deliberately stops asserting — the two are complementary: deterministic E2E owns data correctness, the judge owns UX quality.

## Non-goals / YAGNI

- Not rewriting the suite from scratch — it already covers these domains; this relaxes + cites + gap-fills.
- Not parallelizing E2E — that's #425, which this unblocks.
- Not deleting all UI coverage — a thin resilient data-asserting layer remains (the KEEP bucket + the rare visual-guard exception).
- Not authoring new intents here — Track B, separate priority.
- Not purely test-only — it deliberately includes the small, targeted frontend a11y changes (aria state surfaces) that let state be asserted resiliently. It does not include broader refactors or unrelated a11y work.

## Resolved decisions

- **Toggle/selection state → aria, fix the app** (2026-07-15). Assert `aria-pressed`/`aria-current` via `toHaveAttribute`; add the aria surface where the app lacks it. Targeted a11y changes are in scope. (Chosen over blessing bounded class assertions.)

## Open questions

- **`surface` tag home:** a new SSOT field vs. a derived classification the scanner computes from type + then-clause heuristics. Leaning explicit field (auditable, stable) — confirm at implementation.
- **Visual-guard exception budget:** how many KEEP-as-visual-guard assertions are acceptable before the concern belongs to the judge instead. Leaning "count them; if a domain has >2, revisit." Confirm.
