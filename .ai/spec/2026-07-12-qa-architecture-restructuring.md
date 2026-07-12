# Restructuring the QA architecture — what the tours harness should actually grade

**Issues:** #380 / #606 (agentic UX QA harness), surfaced by #635 · **Date:** 2026-07-12 · **Tree:** `495416a8` · **Status:** plan, no implementation

**Evidence:** [UX behavior → E2E migration audit](./2026-07-12-ux-behavior-e2e-migration-audit.md) (all 56 `ux` behaviors, 153 then-items, bucketed with citations) · [Intent gap map](./2026-07-12-intent-gap-map.md) (how the intent model works, what it is missing, `settings` in depth) · **Unblocks:** [Langfuse as QA SSOT](./2026-07-12-langfuse-as-qa-ssot-plan.md)

## The category error

In a typical agent eval, a deterministically-verified item checks a *consequence of the agent's behavior*: the agent is the system under test, the agent is stochastic, and the verifier is the deterministic ground truth you check it against. That is the shape the hybrid grader was built in.

But this harness's system under test is **the CRM**, and the CRM is deterministic. So a verifier over a captured aria snapshot is not grading an agent — it is asserting that a button is disabled, that a URL gained a query param, that a row count dropped. That is a Playwright assertion, expressed against a serialized snapshot instead of the live DOM. It is the same assertion, with a capture step, a curation step, a PII audit, and a commit in between.

The audit settles it empirically: **of the 58 `verifier` rows in `classification.ts`, not one is secretly a semantic item. Every single one grades a DOM, URL, or network fact.** And the gap runs the *other* way — the capture-based verifier is the **less** capable harness. DSH-005[3] ("refocus refetches only after the 5-minute staleTime") is marked "not deterministically tourable → abstain", but Playwright's `page.clock` plus request counting makes it trivially deterministic. DSH-004[0]/[1] (loading and error states) are unreachable by a tour without interception, and `page.route` handles them in five lines.

The verifier lane is not a lane of hard-to-test semantics. It is a lane of ordinary E2E assertions, misfiled as grading.

## But the migration is work, not deletion

The tempting conclusion — "it's all duplicated, delete it" — is wrong, and the audit is what keeps us honest. Splitting the 58 verifier rows:

| | Count | What it means |
|---|---|---|
| **Pure duplication** — an E2E spec already asserts it, line for line | **24** | Delete the verifier row; the coverage already exists. |
| **Real coverage E2E lacks** | **34** | The verifier is the *only* thing asserting this today. Migrating means **writing new E2E**, not deleting a row. |
| **Inert — abstains, grades nothing** | **11** (of the 58) | Free to drop. Mostly blocked on a seeded provider a tour sweep can't reach — which E2E's `testApi.seed*` already solves. |
| **Misclassified as semantic** | **0** | No verifier row is secretly a judge item. |

So the honest headline is not "the verifier lane is redundant." It is: **the verifier lane holds real coverage in the wrong harness, and about a fifth of it holds nothing at all.** Roughly 34 new E2E assertions is the price of the move, and the repo's own rule applies — *delete tests in the same commit as their replacements*, never on a promissory note.

The clearest duplications are worth naming, because they show the pattern exactly. DSH-002[2] grades nav stickiness from a computed style read into `fields.navPosition`, while `navigation.spec.ts:96-98` asserts the class list *and* re-checks the nav is still pinned after a 500px scroll — strictly stronger. CAD-028[2]'s classification note says multi-surface consistency "is not toured in one flow → abstain", while `overdue-contact-updates.spec.ts:173-208` is a passing test literally titled *"all views should show consistent state after marking as contacted"*. The tour abstains on an item CI already proves green.

Two traps the audit refused to smooth over. **26 of the 64 "already covered" items are partial** — the same observable outcome is asserted, but not every clause (boundary disabling checked at the first position only, etc.). And several E2E specs are **the E2E equivalent of an abstain**: `contact-tasks.spec.ts` wraps every assertion in `if (buttonCount > 0)`, `settings.spec.ts:41-81` in `if (hasGmailBadge)`, and `dashboard.spec.ts:44-53` asserts `hasOverdue || hasCaughtUp` — which is true in every possible state. With no provider configured in the E2E env, those tests pass while asserting nothing. They were counted as gaps, not coverage.

The biggest genuine holes, as a priority order for the migration: **there is no contact-delete E2E anywhere in the suite** (CON-042 — three items, one of the riskiest flows in the app); one-time URL param stripping (CON-041[1], SET-021[2]) is never asserted despite being a documented repo gotcha; CAD-027's three dashboard orderings have no E2E at all; and `settings` is the thinnest domain in the app — 16 of 24 items uncovered, including the entire OAuth connect/disconnect lifecycle.

## What the system under test actually is

The spec has **403 behaviors**. Only **64 are UI-facing**:

| Type | Count | Whose business |
|---|---|---|
| `business-logic` (189), `data` (54), `invariant` (48), `api` (48) | **339** | Go unit / integration / API tests. Never UX QA's business, and never were. |
| `ux` | **56** (→ 153 then-items) | Split: deterministic then-items → **E2E**; semantic then-items → **judge**. |
| `intent` | **8** | The agentic layer. Judged by construction — `spec/README.md` already says an intent "is by construction not provable by a deterministic test." |

And the semantic residue at the *item* level is genuinely tiny: the audit finds **3 to 6 judge-shaped then-items in the whole 153**. Both existing `judge` rows are correctly placed (CON-042[0] "warns the action cannot be undone", DSH-004[2] "the shown reason faithfully reflects the actual failure"); three more hide outside the toured domains (IMP-027[0], CAL-026[3]'s "visually de-emphasized" clause, SET-022[0]'s "warns what access is revoked" clause). One item is misgraded *toward* the judge and should go to E2E: CON-043[5] routes to the LLM as an unbindable copy anchor, but "the outcome is reported and auto-dismissed" is fully deterministic and half-asserted already.

**This is the conclusion that matters most.** The item-judge layer was never where agentic QA lives — after the migration it is a handful of items. **Agentic UX QA lives entirely in the intent layer.** Which is exactly why the answer to "should QA get smaller?" is no: the deterministic mass leaves, and the layer that only an LLM can do has to grow to fill the surface it was always meant to cover.

## The target: four layers

| Layer | System under test | Graded by | Gate |
|---|---|---|---|
| 1. Backend behaviors (339) | Go services, repos, API | unit / integration / API tests | merge gate (**unchanged**) |
| 2. Deterministic UX then-items (~144 of 153) | the app's DOM/URL/network | **Playwright E2E**, citing the behavior id | merge gate (**grows**) |
| 3. Agentic UX (intents) | the *experience* — composition, salience, honesty of copy | tours + LLM judge | advisory (**grows a lot**) |
| 4. Judge quality (labels, doctored cases, fail-precision) | the judge itself | Langfuse | advisory |

Layer 2 needs one new convention, and it is cheap: **a deterministic test cites the `ux` behavior id it proves.** `spec/README.md` already establishes the negative half of this rule ("deterministic tests never cite intent IDs"); making the positive half explicit is what preserves spec→test traceability once the assertions leave the grader, and it is what lets a future lint find an uncovered `ux` then-item.

## How the agentic layer grows

Today: **8 intents, 3 domains** — and those 3 domains are exactly the 3 that have tours. The coverage picture underneath is worse than the headline:

- **20 of 56 `ux` behaviors are actually toured (36%).** (Three more are `proposed` — i.e. they describe known bugs — and are deliberately skipped.)
- **Only 12 of the 56 carry a `serves:` edge**, and every one of them is inside a toured domain. **Zero `ux` behaviors outside the three toured domains serve any intent.** Growing the agentic layer is not just "write more tours" — it is "mint intents and wire `serves:` edges", and that second half is currently at 0%.
- **`settings` is the largest hole**: 10 `ux` behaviors, all `current` (so all tourable — nothing skipped as a known bug), zero intents, zero tour, and the thinnest E2E coverage of any large surface.

### `settings` as the proving ground

Settings is the right place to prove the expanded model because **it is the domain where the deterministic/judged split is sharpest**. The settings surface's entire job is to communicate *state of a connection the user cannot see* — connected/broken, scoped/unscoped, configured/unconfigured — and to make one irreversible action safe. Those are properties of the *composition* of a page, not of any element in it. E2E can assert a badge exists; it cannot assert the user could tell what to do.

The gap map proposes **SET-035…SET-040** (six intents, twelve `serves:` edges, all 10 settings `ux` behaviors bound, two of them cross-domain). Full statements are in the appendix; three are worth pulling forward here:

- **SET-036 (disconnect is deliberate, blast radius legible)** is a near-direct clone of the existing CON-050's shape onto a new domain — the cheapest possible test that the model generalizes at all.
- **SET-038 (the connect round trip never strands the user)** is the worked example of the whole split: its URL-param-stripping half is deterministic and belongs in E2E, while "does an `error` return actually tell the user what to do next?" cannot be asserted — rendering the typed reason `invalid_state` verbatim would satisfy any E2E assertion and be user-hostile.
- **SET-040 (the data surface promises what it delivers)** is, in my judgment, the single best argument for the entire expansion. The spec *already documents a live dishonesty*: SET-028's own notes concede the surface "frames export as a complete backup" while the endpoint ships contacts only (SET-033 is the `proposed` fix). Nothing in the test suite will ever notice, because the promise is UI copy and the contract is an API payload and **no deterministic test compares prose to JSON**. A judge handed the aria tree *and* the recorded `apiResponses` — both already in a single capture — can. One intent row, three edges, and it catches a currently-shipping user-facing lie the whole suite is structurally blind to.

### Free coverage first

Before any of that: `knowledge` (2 `ux`), `notes-meetings` (2), and `todoist` (2) are too thin to carry their own intents, and all six behaviors live on the **contact detail page** — which the cadence tour already captures. Adding `serves: [CAD-036]` edges to KNW-034/035, NTS-007/008, TDS-035 buys coverage of five behaviors across three domains for **zero new intents, zero new tours, zero new machinery**. It is the cheapest possible test of whether inverted `serves:` edges do real work, and it should be step one.

And one domain should be **explicitly declared out of scope**: `mac-host`'s 4 `ux` behaviors are daemon CLI and macOS notification surfaces, unreachable by a browser tour. An intent there would bind zero captures and abstain forever — and a permanently-abstaining intent is worse than no intent. Their home is the Swift suite.

## The four new intent kinds

The maintainer's suspicion — that the intent concept as built does not go far enough — holds. Details and required changes are in the appendix; the verdicts:

**State-space (judge one surface across empty / one / many / loading / error / stale).** **Expressible today; already accidentally proven.** `dashboard.tour.ts:179-226` freezes a loading state, fakes a 500, and fakes a caught-up empty list — three synthetic states of one surface, all bound to DSH-011. It works. Nobody generalized it. What is missing is not mechanism but *convention*: there is no way for an intent to declare the states it requires, and therefore **no way to distinguish "the empty state is fine" from "the tour never visited the empty state."** That is a **silent-pass hole and the most dangerous thing in the current model** — an intent that only ever sees the populated surface will happily certify a page whose empty state is blank. ~60 lines (`states?: string[]` on `IntentSpec`, bound through the `pair.role` field that already exists, plus a `missingStates` flag in the grade). **Build this first.**

**Journey / cross-surface.** **Plumbing already works — and that is the trap.** The report already walks every tour's captures into one flat array, and binding filters by behavior tag with no tour scoping. Mint a cross-tour intent and it binds *today*. But **the UUID mapper is per-test**: `<id:3>` in `dashboard.tour` and `<id:3>` in `contacts.tour` are almost certainly different contacts. A journey intent is a claim *about the same contact on two surfaces*, and the judge would be shown two captures where that contact has two pseudonyms and no way to know they are the same person — which does not fail loudly, it **produces confidently wrong verdicts over evidence that looks coherent**. Fix the mapper (run-scoped, ~40 lines) before anyone tries this. It is a correctness fix regardless: cross-tour evidence is *currently incoherent and nothing in the harness says so*.

**Global consistency (one voice for empty states, one date format, every destructive action confirms).** **Not expressible.** It needs a genuinely new binding rule — every existing intent binds through a behavior *declaring* `serves:`, and a cross-cutting invariant is by definition owned by no behavior. Binding must become **selection, not declaration** (bind by capture predicate), which brings a sampling problem that is real design work, plus a second prompt ("these captures are from *different* surfaces; find where they disagree") and a two-citation grounding rule. **Cheap down-payment:** a destructive-confirmation intent bound by the existing `dialogs` field (`dialogs.length > 0`) needs no sampling strategy and reuses the prompt nearly as-is — a one-day proof of the `predicate` binding kind before committing to the general case.

**Regression / comparative (did this get worse?).** **Blocked by a deliberate decision, not an oversight.** It needs a stored baseline, and **screenshots are never committed on purpose** — the PII audit greps JSON, it cannot grep pixels. The tractable version is an **aria-only baseline** (the normalized aria tree + `apiResponses` are already committed, already scrubbed, already diffable): ~80% of the value at ~5% of the cost and zero PII risk. The visual baseline collides with the repo's hardest rule and should stay deferred. But the real objection is not technical — it is that **every intentional UI change would then need a baseline re-approval every PR.** That is snapshot-test fatigue with an LLM attached, and the maintainer should decide whether they want that tax *before* anyone builds it.

## Cost, stated plainly

The intent pass is already **~94% of per-run token cost** (≈$1.42/run at `gpt-5.5` vs ≈$0.09 for the item judge). Every kind above is a multiplier on *that* pass. The `gpt-5.6-luna` swap already scoped in `judge/DEFERRED.md` cuts it ~5× (≈$0.28/run) — so **the luna evaluation should come before the expensive kinds (journey, consistency), not after.** It changes what is affordable, and it is already waiting on exactly the human labels that now exist.

## Sequencing

1. **Free coverage.** `serves: [CAD-036]` edges from KNW-034/035, NTS-007/008, TDS-035. Half a day, no machinery. Either it finds something on the contact page or it does not — and that is informative either way.
2. **The verifier→E2E migration.** Work the audit table domain by domain. Delete each verifier row *in the same commit* as the E2E that replaces it. Drop the 11 inert rows outright. Fix CON-043[5] (judge → E2E). Close the priority gaps (contact-delete has no E2E at all).
3. **`settings` intents + `settings.tour.ts`.** Mint SET-035…SET-040, wire the edges, write the tour — which is mostly route interception, so **it doubles as the state-space proof**. Start with SET-036 (lowest design risk) and SET-040 (highest value). *If SET-040 does not fire, that is important information about the whole thesis.*
4. **Formalize state-space.** `states?: string[]`, missing-state visible in the grade, extract the route-interception idiom into `support/`. Do it *after* the settings tour, because writing that tour is what reveals which helpers are actually needed.
5. **Run-scoped UUID mapper** — even if no journey intent is built yet. It is a correctness fix.
6. **The luna swap**, then one journey intent and the destructive-confirmation predicate intent, each as a spike.

Layer 2's merge gate must be green before `make qa-eval`'s deterministic gate retires — and only then does the [Langfuse SSOT plan](./2026-07-12-langfuse-as-qa-ssot-plan.md) unlock, because that gate is the only reason the corpus had to live in git.

## Risks

- **Coverage regression during the migration.** 34 of the verifier rows are the *only* thing asserting their behavior. The audit table is the checklist; the same-commit rule is the discipline.
- **Journey intents fail silently, not loudly.** Cross-tour evidence is incoherent today. Fix the mapper first or don't build them.
- **State-space blindness certifies broken empty states.** The current model cannot tell "fine" from "never looked."
- **The agentic layer grows the token bill.** Sequence the luna swap accordingly.
- **Regression intents impose a per-PR re-approval tax.** Decide whether you want it before building it.
