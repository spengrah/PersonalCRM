# Intent gap map — expanding the agentic UX layer

Read-only analysis at `develop` / HEAD `495416a8`. No repo file was modified.

## 1. How the intent model works today, precisely

### 1.1 The SSOT side: `type: intent` behaviors

An intent is a first-class row in a `spec/<domain>.yaml` behavior list, defined at `spec/README.md:49` as "a judged experience goal: the durable purpose its serving `ux` behaviors exist for … Consumed ONLY by the Track B agentic judge — an intent is by construction not provable by a deterministic test, so **deterministic tests never cite intent IDs**". That last clause is the load-bearing definition and the exact criterion for section 3 below.

An intent row carries exactly these fields (all validated by `make spec-lint`, whose rules are enumerated at `spec/README.md:140`):

| Field | Rule |
|---|---|
| `id` | `<PREFIX>-NNN`, monotonic per domain, never reused (`spec/README.md:41`) |
| `title` | one short line (`spec/README.md:42`) |
| `type` | literally `intent` (`spec/README.md:49`) |
| `status` | `current` = achieved today → judge as **regression detection**; `proposed` = aspirational → judge as **progress detection** (`spec/README.md:50`, and the prompt renders this distinction verbatim at `frontend/tests/tours/judge/adapter/prompt.ts:190`) |
| `statement` | a single string that **replaces** `given`/`when`/`then` entirely; mutually exclusive with GWT (`spec/README.md:54`) |
| `provenance` | free list, non-load-bearing (`spec/README.md:56`) |
| `notes` | optional free text |

Intents have **no `serves` field of their own in practice** (though the schema permits an intent to serve a broader intent — `spec/README.md:55`). The edge runs the other way: a `ux` behavior declares `serves: [DSH-011]`, and the judge **inverts** those edges. See `spec/dashboard.yaml:115-137` for the canonical block — three intents at the bottom of the file under a `# --- Intents (judged experience goals — Track B agentic judge only) ---` comment — and `spec/dashboard.yaml:13` (`serves: [DSH-011]` on DSH-001) for the edge.

### 1.2 The harness side: the intent catalog

`frontend/tests/tours/judge/intent-catalog.ts:13-26` transcribes those rows into TypeScript:

```ts
export interface IntentSpec {
  id: string
  title: string
  statement: string
  status: IntentStatus            // 'current' | 'proposed'
  servedBy: string[]              // corpus-wide INVERSION of the YAML `serves:` edges
  visual?: boolean                // harness-side judgment, NOT in the SSOT
}
```

Two things to note. First, `servedBy` is the inverted edge set, computed by hand and **kept honest by a drift test** (`intent-catalog.test.ts`) that parses the YAML and asserts ids/titles/statements/status/edges all match — catalog drift fails offline tests, not a live run (`intent-catalog.ts:7-9`). Second, `visual` is a harness-only flag with no SSOT counterpart (`intent-catalog.ts:20-25`): it marks a goal as inherently visual (scannability, hierarchy, salience) so that an aria-only run carries an evidence caveat. Today only DSH-010 and CON-052 set it (`intent-catalog.ts:36`, `:77`).

All 8 intents live in `INTENT_CATALOG` at `intent-catalog.ts:28-95`.

### 1.3 How an intent binds to evidence

`bindIntentCaptures` (`frontend/tests/tours/judge/intent-input.ts:21-39`) is the whole binding rule, and it is short enough to state exactly:

1. `wanted = {intent.id} ∪ intent.servedBy` — an intent's evidence is any capture tagged with the intent's **own ID** or with **any behavior that serves it**.
2. Filter `all` captures to those whose `capture.behaviors` array intersects `wanted`.
3. Dedupe on `${tour}#${seq}`.
4. Sort by `(tour, seq)` — **alphabetical tour name first, then sequence within the tour**.
5. Cap at `INTENT_CAPTURE_CAP = 8` (`intent-input.ts:14`); the overflow count is surfaced as `droppedCount` in the grade, never silently truncated.

Tagging happens in the tours: `tour.capture(page, { behaviors: ['DSH-001'], note: '…', pair: {…} })` (`frontend/tests/tours/dashboard.tour.ts:82-88`). So an intent is bound to evidence **transitively through its serving behaviors' tags** — a tour author never names an intent, they tag the behavior and the `serves` edge does the rest. This is elegant and it is also the single biggest structural constraint (see §4.1).

Critically: the report walks **the entire run's capture tree across all tours** (`frontend/tests/tours/judge/report/render.ts:220-230` — `walk(capturesRoot)` over every per-tour subdirectory) and hands the flat array to `runIntentPass`. So cross-tour binding is *mechanically* possible today; what breaks it is entity-identity, not plumbing (§4.1).

### 1.4 What the judge is given

`buildIntentJudgeInput` (`intent-input.ts:63-88`) constructs a `JudgeInput` with an `intent: { statement, status }` marker; `buildPrompt` branches on that marker (`adapter/prompt.ts:136`) into `buildIntentPrompt` (`adapter/prompt.ts:185-209`). The rendered prompt is:

```
INTENT_PREAMBLE                      (prompt.ts:50-67 — "judge whether the captured surface,
                                      taken as a whole, ACHIEVES the goal")
INTENT_IMAGES_NOTE | INTENT_ARIA_ONLY_CAUTION   (prompt.ts:71-80 — switches on image presence)
=== INTENT ===   id, title, STATUS (+ the regression/progress framing), STATEMENT
=== CAPTURE[0] — <tour>#<seq> — <note> ===
     === URL === / === ARIA === / === API === / === SERVER_TIME === / === DIALOGS ===
=== CAPTURE[1] — … ===   … up to 8
=== ITEMS ===  [0] <the statement>
"Return ONLY JSON matching the required schema"
```

Each `CAPTURE[n]` section is one capture rendered through the same stable labeled-block protocol the item judge uses (`captureSection` at `intent-input.ts:43-54`, `renderEvidence` at `prompt.ts:116-127`). Evidence stays **sectioned** rather than merged, deliberately — a merged aria bundle "would blur which state showed what" (`intent-input.ts:41-42`).

Screenshots: every `capture()` call takes a best-effort viewport PNG into the gitignored run dir (`support/capture.ts:136-150`), recorded as `Capture.screenshot` (a run-dir-relative path — `support/types.ts:107-114`). At judge time a `ScreenshotResolver` (`intent-input.ts:61`) turns that into an absolute path, and `buildIntentJudgeInput` attaches them as model images **all-or-nothing** (`intent-input.ts:69-75`): because the prompt promises images in `CAPTURE[n]` order, one missing screenshot would silently shift every later image onto the wrong capture, so any gap drops *all* images and the run degrades to aria-only. Image attachment is further gated on the adapter — only `codex-exec` can attach files (`report/render.ts:236-237`, `canAttachImages`).

Verdict handling (`intent-runner.ts:73-128`): one judge call per intent, run **serially** (concurrent codex spawns storm a quota-limited account). A zero-evidence intent abstains **without a model call** (`intent-runner.ts:93-100`) — a freshly-minted intent is visibly unjudgeable rather than silently absent, which matters a lot for the expansion proposed below. Verdicts are `pass | fail | unsure`, categorical only, no scores. Intent grounding is **stricter than the item judge's**: a `fail` must cite an *in-range* `CAPTURE[n]` index **plus** a node label / JSON path within it, else it is downgraded to `unsure` (`isGroundedIntentCitation`, `intent-runner.ts:49-54`; the downgrade at `:116-124`).

Model: the intent pass defaults to a stronger tier than the item judge — `gpt-5.5` / `medium` effort (`intent-runner.ts:38-39`), overridable via `QA_INTENT_MODEL` / `QA_INTENT_EFFORT`.

### 1.5 What the corpus stores

`IntentCaseSchema` (`frontend/tests/tours/judge/corpus/schema.ts:123-144`):

```ts
{ id, intent_id, captures: string[], source: 'clean'|'doctored', mutation?: Mutation, expected_hypothesis: 'pass'|'fail'|'unsure', notes?: string }
```

Two intent cases exist today: `corpus/intent-cases/DSH-010-clean.json` and `DSH-011-doctored.json`. `expected_hypothesis` is explicitly **a self-labeled guess, never ground truth and never a merge gate** (`schema.ts:120-122`) — ground truth comes from the human-labeling path (`label.ts:108` runs `runIntentPass` with a stronger drafter model to pre-fill `corpus/labels/*.draft.json`, which the maintainer corrects in place into `*.labeled.json`). Six labeled files exist and are `human-confirmed` as of the 2026-07-10 label session (`judge/DEFERRED.md:22`). A doctored intent case reuses the same single-point `MutationSchema` as the item-judge corpus (8 ops — `schema.ts:22-73`: `inject_query`, `delete_endpoint`, `set_aria_disabled`, `reorder_ids`, `blank_dialog`, `remove_aria_subtree`, `set_field`, `set_json_field`).

Corpus captures are aria-only JSON — **screenshots are never committed** (`support/types.ts:110-113`: "the PII audit can grep JSON, not pixels"). This is the reason regression/comparative intents (§4.4) are hard.

### 1.6 Cost anchor

The intent pass is already the dominant cost. Measured 2026-07-10 (`judge/DEFERRED.md:28`): the intent pass is **~94% of per-run token cost** — ≈ $1.42/run API-equivalent at `gpt-5.5` versus ≈ $0.09 for the `gpt-5.4-mini` item judge. That is with 8 intents, each capped at 8 captures. The corpus's 51 committed captures average **~20 KB of JSON each**, so a capped 8-capture intent prompt is on the order of 100 KB of rendered evidence before images. Every multiplier proposed in §4 lands on top of a pass that already costs ~$1.40/run.

---

## 2. Coverage by domain

Counts are from the YAML directly (all `type:` rows, statuses read per-behavior).

| Domain | behaviors | `ux` | `ux` proposed | intents | tour | E2E specs |
|---|---|---|---|---|---|---|
| dashboard | 12 | 8 | 2 (DSH-006, DSH-009) | **3** (DSH-010/011/012) | `dashboard.tour.ts` | `dashboard.spec.ts`, `navigation.spec.ts`, `overdue-contact-updates.spec.ts` |
| contacts | 51 | 8 | 1 (CON-046) | **3** (CON-050/051/052) | `contacts.tour.ts` | `contacts.spec.ts`, `contact-navigation.spec.ts`, `contact-merge.spec.ts`, `birthdays.spec.ts` |
| cadence-followup | 36 | 7 | 0 | **2** (CAD-036/037) | `cadence-followup.tour.ts` | `contact-tasks.spec.ts`, `contact-direction.spec.ts` |
| **settings** | **34** | **10** | **0** | **0** | **none** | `settings.spec.ts` (6 tests), `settings-mac.spec.ts`, `telegram-settings.spec.ts` |
| calendar | 30 | 7 | 0 | 0 | none | `meetings.spec.ts` |
| imports-matching | 35 | 6 | 0 | 0 | none | `imports-*.spec.ts` (8 files) |
| mac-host | 36 | 4 | 0 | 0 | none | `settings-mac.spec.ts` (partial) |
| knowledge | 35 | 2 | 0 | 0 | none | — (covered incidentally by `contacts.spec.ts`) |
| notes-meetings | 25 | 2 | 0 | 0 | none | `meetings.spec.ts` |
| todoist | 34 | 2 | 0 | 0 | none | — |
| ingest | 38 | 0 | — | 0 | n/a | — |
| telegram | 37 | 0 | — | 0 | n/a | `telegram-settings.spec.ts` |
| **TOTAL** | **403** | **56** | **3** | **8** | **3 tours** | 25 spec files |

**The headline numbers.** 56 `ux` behaviors across 10 domains that have any. The 3 toured domains hold 23 of them, of which **20 are actually toured** (the 3 `proposed` ones — DSH-006, DSH-009, CON-046 — are deliberately skipped by the tours, since a proposed behavior describes a bug). So **20/56 = 36% of `ux` behaviors are toured**, and **36/56 = 64% are in a domain with no tour and no intent at all**.

**The `serves`-edge picture is even sparser.** Only 12 of the 56 `ux` behaviors carry a `serves:` edge — every one of them in the 3 toured domains. Four `ux` behaviors *inside* toured domains serve no intent (DSH-002 global nav, DSH-007 search location, CON-044 mark-as-contacted-from-list, CON-046) — so even the toured domains have unbound surface. **Zero `ux` behaviors outside the 3 toured domains carry a `serves:` edge.** That is the structural gap: growing the agentic layer is not only "write more tours", it is "mint intents and wire `serves:` edges", and the second half is currently 0%.

**Settings is the largest single hole**: 10 `ux` behaviors, all `current` (so all tourable — nothing is skipped as a known bug), zero intents, zero tour, and the thinnest deterministic coverage of any large surface (`settings.spec.ts` has 6 tests, of which one — "should have consistent form field styling" — is a styling smoke test that an intent would subsume far better).

---

## 3. Proposed intents

### 3.1 Settings — in depth (the proving ground)

Settings is the right proving ground for a reason worth stating: **it is the domain where the deterministic/judged split is sharpest.** The settings surface's whole job is to communicate *state of a connection you cannot see* — connected/not, scoped/partially-scoped, healthy/auth-expired, configured/unconfigured — and to make one irreversible action (disconnect) safe. Every one of those is a property of the *composition* of the section, not of any element. E2E can assert a badge exists; it cannot assert the user could tell what to do.

Below, `serves:` edges to add are listed per intent. Proposed IDs continue the settings sequence (last is SET-034), so intents take **SET-035 … SET-040**. All would be minted `status: current` unless noted — the surfaces exist today, so these are regression detectors, not progress detectors.

---

**SET-035 — The settings page answers "is my data flowing?" without the user opening a provider**

```yaml
  - id: SET-035
    title: The settings page answers whether data is actually flowing
    type: intent
    status: current
    statement: a user opening settings can tell, for every provider, whether their data is actually flowing right now — connected but broken, connected but unscoped, and connected and healthy are visibly different states, and a user never has to open the provider's own site to find out that a sync has been silently dead.
    provenance: ['.ai/spec/2026-07-12-intent-gap-map.md']
```

- **Serving behaviors** (add `serves: [SET-035]`): SET-019 (sections show connection state), SET-024 (per-scope affordances follow granted scopes), SET-025 (auth-failed → reconnect prompt), SET-023 (unconfigured → empty state). Cross-domain: CAL-030 (a stalled calendar sync surfaces in the settings banner) and TDS-034 already describe settings-hosted state — both should serve SET-035, which is exactly the cross-domain `serves` the SSOT explicitly permits (`spec/README.md:55`).
- **Evidence the judge needs:** captures of the settings page in ≥4 states — (a) healthy connected Google account with full scopes, (b) connected account with a partial scope grant (chat scopes missing → reconnect prompt per SET-024), (c) connected account whose sync state is in an auth error (SET-025), (d) provider unconfigured (SET-023). States (b)/(c) are unreachable from any seed and must be route-intercepted, exactly as `dashboard.tour.ts:184-219` fakes loading/error/caught-up. **Screenshots are load-bearing** (`visual: true`): the difference between "connected" and "connected but dead" is carried almost entirely by badge color and placement, and the aria tree flattens that.
- **Why it cannot be a deterministic E2E assertion:** you can assert `SyncBadge` renders with `data-state="error"`. You cannot assert that a user *looking at the page* would notice — that four green badges and one amber one, in a page that also contains an export panel, a Telegram panel, and a system-info panel, actually reads as "your Gmail sync is dead." The failure mode this intent catches is *the badge is present and correct and invisible*, which is precisely the class of bug that passes every E2E test.

---

**SET-036 — Disconnecting is deliberate, and the user knows what they are losing**

```yaml
  - id: SET-036
    title: Disconnecting a provider is deliberate and its blast radius is legible
    type: intent
    status: current
    statement: a user never disconnects a provider by accident, and before they confirm they can see which account they are disconnecting and what stops working when they do — the confirmation names the specific account and the consequence, not a generic "are you sure".
    provenance: ['.ai/spec/2026-07-12-intent-gap-map.md']
```

- **Serving behaviors:** SET-022 (disconnect requires explicit confirmation). That is the only one today, which is itself the finding — the *blast radius* half of this intent has no `ux` behavior at all. It is the sibling of the existing CON-050 ("Destructive contact actions are deliberate", `intent-catalog.ts:54-61`), and cloning CON-050's shape onto settings is the cheapest possible proof that the model generalizes.
- **Evidence:** the disconnect dialog captured via `tour.withDialog(page, { accept: false }, …)` (`support/capture.ts:157-172`) — the harness already records `dialogs: [{ type, message }]` and the prompt renders a `=== DIALOGS ===` block (`prompt.ts:123-125`). Plus the before/after account-list captures (a `pair: { id: 'disconnect', role: 'before'|'after' }`).
- **Why not E2E:** an E2E test asserts `dialog.message()` is non-empty, or matches a fixed string — and the moment it matches a fixed string it is asserting today's copy, not the property. The judged property is *"the message identifies the account and names the consequence"*, which is a semantic claim about arbitrary prose. This is the single clearest case in the domain: CON-042's E2E equivalent already exists and it proves nothing about whether the warning warns.

---

**SET-037 — A deployment that never configured a provider still reads as a working app**

```yaml
  - id: SET-037
    title: An unconfigured provider degrades gracefully, with a way forward
    type: intent
    status: current
    statement: on a deployment where a provider was never set up, its section reads as "not set up yet, here is how" rather than as a broken or errored app — the user is never shown a stack, a spinner that never resolves, or an empty box with no explanation of what it is or how to fill it.
    provenance: ['.ai/spec/2026-07-12-intent-gap-map.md']
```

- **Serving behaviors:** SET-023 (unconfigured/errored → not-connected state with setup guidance), SET-027 (Telegram not-configured state naming the required configuration). These two behaviors say the same thing about two different providers, which is the tell that they are facets of one intent.
- **Evidence:** settings captured against a backend with each provider's routes 404ing (SET-005 makes an unconfigured provider's routes return 404, so route interception to 404 is a *faithful* simulation, not a lie). Ideally three captures: Google-unconfigured, Todoist-unconfigured, Telegram-unconfigured — and one where the account *list request fails* (500), which SET-023 explicitly says should also collapse to the empty state rather than an error stack.
- **Why not E2E:** `settings.spec.ts` today asserts the section is *present*. The judged property is "reads as not-set-up rather than as broken" — a distinction between two things that are both "an empty box with text in it." A human reading the page can tell instantly; an assertion cannot express the difference without pinning the copy.
- **Note:** this is the one settings intent where I would consider `status: current` risky. SET-023's own note says treating a 4xx as an empty state is *deliberate*, so the design intent is current. But the 500-failed-to-load case collapsing to "not connected" is arguably a lie to the user (it says "not connected" when it means "we could not tell"). If the maintainer agrees, the honest move is to mint SET-037 `current` for the unconfigured half and file the failed-load half as a `proposed` `ux` behavior — which is exactly what the intent layer is for: it *surfaces* that boundary instead of freezing it.

---

**SET-038 — Connecting a provider is a round trip the user can follow**

```yaml
  - id: SET-038
    title: The connect round trip never strands the user
    type: intent
    status: current
    statement: leaving the app for a provider's consent screen and coming back is one continuous, legible act — the user knows they are being handed off, knows on return whether it worked, knows why if it did not, and a page refresh afterwards does not re-announce a stale outcome.
    provenance: ['.ai/spec/2026-07-12-intent-gap-map.md']
```

- **Serving behaviors:** SET-020 (hands off to consent screen), SET-021 (return shows outcome + strips one-time params).
- **Evidence:** the pre-handoff click state; the return with `?outcome=success&provider=google`; the return with `?outcome=error&provider=google&reason=invalid_state`; and a capture **after a reload** of the post-return URL, proving the params are gone and the toast does not re-fire. The outcome-carrying returns are directly navigable (`page.goto('/settings?outcome=error&provider=google&reason=exchange_failed')`) — no interception needed, because SET-004 pins that the callback's only job is to redirect with those params. That makes this intent unusually cheap to tour.
- **Why not E2E:** the URL-stripping half *is* deterministically assertable (and there is an established repo gotcha for exactly this — the `?action=edit` one-time-param rule in `.ai/rules/core.md`), so **that half should move to E2E**. What stays judged is: does an `error` return actually tell the user *what to do next*? `reason=invalid_state` is a typed reason (SET-004) — rendering it verbatim would satisfy any E2E assertion and would be user-hostile. The intent is the only thing that catches "we showed the user the string `invalid_state`."
- **This is the clearest illustration of the split the maintainer is making**: one behavior, two halves, one deterministic and one judged. It is worth being the worked example in the design doc.

---

**SET-039 — The Todoist configuration surface makes the incomplete state obvious**

```yaml
  - id: SET-039
    title: A half-configured integration says so
    type: intent
    status: current
    statement: an integration that is connected but not yet usable — a Todoist account with no project or no label chosen — visibly announces that it is not doing anything yet and what remains to be chosen; the user is never left believing a half-configured integration is live.
    provenance: ['.ai/spec/2026-07-12-intent-gap-map.md']
```

- **Serving behaviors:** SET-026 (project/label pickers, and the surface indicates both must be chosen before cadence sync is active). Cross-domain: TDS-034 (manual sync trigger from settings) — a sync button on an inert integration is exactly the confusion this intent guards.
- **Evidence:** connected-Todoist-with-nothing-selected; project-selected-label-not; both-selected (the live state). Requires either a Todoist-connected seed or a route-intercepted `GET /api/v1/todoist/settings`. **This is the one settings intent that is provider-blocked** — the cadence tour already skip-lists provider-dependent legs (`cadence-followup.tour.ts:6-8`), so precedent exists, but a skip-listed intent binds zero captures and abstains (`intent-runner.ts:93-100`). Route interception is the way to make it real; the API contract (SET-015 returns empty settings 200 with an account but no sync state) tells you exactly what body to fake.
- **Why not E2E:** you can assert the "select a project and a label" hint text exists. You cannot assert the user believes the integration is off. The failure this catches: both pickers empty, a big green "Connected" badge above them, and a "Sync Now" button that succeeds and does nothing.

---

**SET-040 — Export promises what it delivers** (`status: proposed`)

```yaml
  - id: SET-040
    title: The data surface promises exactly what it delivers
    type: intent
    status: proposed
    statement: what the settings data section says it will export or import is what actually happens — the user is never told they are downloading a complete backup when they are downloading part of one, nor offered an import that does nothing.
    provenance: ['.ai/spec/2026-07-12-intent-gap-map.md']
```

- **Serving behaviors:** SET-028 (`ux` — the surface presents export/import, "communicates that import does not yet modify stored data"), SET-029 (`proposed` api — import is a placeholder), SET-033 (`proposed` api — export is contacts-only while the surface frames it as a complete backup).
- **This one is a `proposed` intent and that is the point.** The SSOT *already documents the lie*: SET-030's note says "the settings surface (SET-028) frames export as a complete backup, but today the endpoint serializes contacts only." Two `proposed` api behaviors (SET-029, SET-033) exist to fix it. But nothing in the harness will ever *notice* the gap between copy and contract, because the copy is UI and the contract is API and no deterministic test compares them. A judge given the settings aria tree (which contains the copy) **and** the export response body (which the capture records under `apiResponses`) can — that is a cross-block comparison inside a single capture, and the intent judge already gets both blocks. It grades as **progress detection** and flips `current` when SET-029/SET-033 land.
- **Why not E2E:** an assertion that copy matches payload would have to encode the intended payload shape, at which point it is just SET-033's test. The judged property is *the surface's promise is honest*, which is a semantic comparison of English prose against a JSON envelope. No assertion expresses that.
- This intent is, in my judgment, **the single best argument for the whole expansion**: it catches a real, currently-shipping, user-facing dishonesty that the entire existing test suite is structurally blind to, and it costs one intent row plus three `serves:` edges.

---

**Settings summary — coverage after:**

| Behavior | Serves |
|---|---|
| SET-019 | SET-035 |
| SET-020 | SET-038 |
| SET-021 | SET-038 |
| SET-022 | SET-036 |
| SET-023 | SET-035, SET-037 |
| SET-024 | SET-035 |
| SET-025 | SET-035 |
| SET-026 | SET-039 |
| SET-027 | SET-037 |
| SET-028 | SET-040 |
| CAL-030 (cross-domain) | SET-035 |
| TDS-034 (cross-domain) | SET-035, SET-039 |

All 10 settings `ux` behaviors bound; 6 intents; 2 cross-domain edges. That is the shape the other domains should copy.

### 3.2 The other untoured domains (briefly)

**calendar (7 `ux`, 0 intents).** Two intents.
- **CAL-031 — "The meetings section answers when we last met and when we next meet"** (`current`, `visual`): served by CAL-024 (section appears only when there are events), CAL-025 (all/upcoming/past filters with live counts), CAL-026 (card summarizes time, place, size), CAL-028 (long list reveals progressively). Judged property: a contact with 40 meetings is still *scannable* — progressive reveal and filter counts either make the history navigable or bury it. Not E2E-able: "reveals progressively" is assertable; "the user can find the meeting they are thinking of" is not.
- **CAL-032 — "A dead calendar sync is visible where the user looks, not where it failed"** (`current`): served by CAL-029 (per-account manual sync with visible state), CAL-030 (stalled sync surfaces in the settings banner). This overlaps SET-035 by design — it should probably *serve* SET-035 rather than exist separately (an intent may serve a broader intent — `spec/README.md:55`). That refinement ladder has never been exercised; calendar is a good place to try it.

**imports-matching (6 `ux`, 0 intents).** Two intents.
- **IMP-032 — "The imports hub is a queue you can actually get to the bottom of"** (`current`, `visual`): served by IMP-026 (one hub, two tabs), IMP-027 (single-view modal), IMP-028 (modal pages the whole queue). Judged: after resolving one candidate, is the user's place in the queue preserved and is progress legible? This is the closest sibling of the existing CON-051 ("browsing contacts never loses the user's place") and is the strongest cross-domain evidence that the model generalizes.
- **IMP-033 — "A match suggestion tells the user how much to trust it"** (`current`): served by IMP-029 (confidence surfaces on the link affordance), IMP-030 (method suggestions resolve against a fixed contact). Judged: a 0.62 and a 0.98 must not look alike, and the user must be able to tell *why* the system thinks these are the same person. Emphatically not E2E-able — you can assert a confidence number renders; you cannot assert it is decision-supporting.
- Note IMP-031 ("resolving review items updates every dependent surface without reload") is the natural serving behavior for a **journey** intent (§4.1), not for either of these.

**knowledge (2 `ux`), notes-meetings (2 `ux`), todoist (2 `ux`).** These three are too thin to carry their own intents and they all live on the **contact detail page**. Their `ux` behaviors (KNW-034 knowledge fields shown only when known, KNW-035 year-less birthdays, NTS-007/008 notepad, TDS-035 projected task content + deep link) should **serve the existing CAD-036** ("A contact page answers where the relationship stands", `intent-catalog.ts:79-86`) — cross-domain `serves` edges into an intent that already exists, already has a tour, and already has captures. This is the cheapest coverage in the entire map: **5 behaviors, 3 domains, 0 new intents, 0 new tours** — the cadence tour's contact-detail captures already contain this evidence, they are simply not bound to it. I would do this on day one as the proof that inverted edges do real work.
- The one addition worth making: **CAD-038 — "The contact page never shows the user a hole where knowledge should be"** (`proposed`), served by KNW-034/KNW-035 — the judged property that *absent* knowledge reads as absent rather than as broken (a birthday with no year must not render as an age of 126; that bug class is live in prod per the 1900-sentinel data).

**mac-host (4 `ux`, 0 intents).** Skip. Its `ux` behaviors are CLI/daemon operator surfaces (`MAC-043` status/doctor, `MAC-044` ops refuse against a live daemon, `MAC-045` install) and one macOS notification. None of them are reachable by a Playwright tour, so an intent there would bind zero captures and permanently abstain (`intent-runner.ts:93-100`). If the maintainer wants them judged, that is a different harness (a CLI-transcript capture format), not this one. **Recommend explicitly declaring mac-host out of scope for the intent layer** rather than leaving it as an implicit gap — an unbound intent that abstains forever is worse than no intent.

---

## 4. The four candidate new intent kinds

### 4.1 Journey / cross-surface

**Expressible today? Partially — and the gap is not where it looks.**

The plumbing is already there. `report/render.ts:220-230` walks *every* tour's captures into one flat array and hands it to `runIntentPass`, and `bindIntentCaptures` (`intent-input.ts:26-35`) filters by behavior tag with no tour scoping whatsoever. Mint an intent, add `serves:` edges from behaviors in two different tours, and the union already binds across them. **Nobody has done it, but nothing forbids it.**

What actually breaks is **entity identity across tours**. `TourApi` constructs a fresh UUID mapper per test (`support/capture.ts:71`), and `createUuidMapper` (`support/normalize.ts:40-54`) assigns placeholders by *encounter order within that mapper*: `<id:1>`, `<id:2>`, … So `<id:3>` in `dashboard.tour` and `<id:3>` in `contacts.tour` are almost certainly **different contacts**. A journey intent — "from the dashboard, a user can get to the contact that needs attention and act on it without losing context" — is a claim *about the same contact appearing on two surfaces*, and the judge would be shown two captures in which that contact has two different pseudonyms and no way to know they are the same person. Worse, it would silently look like *coherent* evidence about *different* people. That is a miscited-verdict failure mode, and it is the same class of bug the all-or-nothing image rule was written to prevent (`intent-input.ts:69-72`).

Two secondary problems: (a) `bindIntentCaptures` sorts by **alphabetical tour name** then seq (`intent-input.ts:36`), so a `cadence-followup → contacts → dashboard` narrative would be presented in that order regardless of the actual user journey — the judge would read the story backwards; (b) the cap of 8 (`intent-input.ts:14`) is per-intent, and a journey spanning three surfaces with before/after pairs at each will exceed it.

**Required changes, specifically:**
1. **Run-scoped UUID mapper.** Hoist the mapper out of the per-test `TourApi` (`support/capture.ts:71`) into a worker- or run-scoped fixture backed by a file in the run dir (`support/run-dir.ts` already owns that directory). Persist the `uuid → <id:n>` map as JSON; each tour loads-and-extends it. This makes `<id:7>` mean the same contact in every tour of a run. Requires tours to run in **one worker serially** (they already do — the tours config runs them as serial tests) or a file lock. This is the load-bearing change and it is maybe 40 lines.
2. **Journey ordering.** Either add an explicit `journey: string[]` field to `IntentSpec` naming the capture order (tour#seq refs), or — cheaper and more in keeping with the existing design — add a **run-global monotonic capture ordinal** to the `Capture` record and sort by it instead of `(tour, seq)` at `intent-input.ts:36`. That gives chronological order for free and requires a `CAPTURE_FORMAT_VERSION` bump (`support/types.ts:12`).
3. **Per-intent cap override.** `INTENT_CAPTURE_CAP` is already a parameter (`intent-input.ts:24`, `intent-runner.ts:77`) — add an optional `captureCap?: number` to `IntentSpec` and let a journey intent raise it to ~12–16.
4. A **new `serves:` linting consideration**: nothing today stops a journey intent's `servedBy` set from binding captures the author did not intend. Worth a lint that a journey intent names its tours explicitly.

**Cost:** high but bounded. A 12–16 capture journey prompt is ~2× the current worst case — call it 200–250 KB of rendered evidence plus 12–16 images. At the measured $1.42/run for 8 intents × ≤8 captures, **one journey intent alone is roughly the cost of two ordinary intents** (~$0.35). Two or three journey intents is a ~50% increase on the intent pass. Note the DEFERRED `gpt-5.6-luna` swap (`DEFERRED.md:28`) cuts the intent pass ~5× — **sequence the journey work after that swap and the cost question mostly evaporates.**

**Verdict: the highest-value new kind, and the one with a real blocker (the per-tour UUID mapper) that must be fixed before anyone tries it.** Left unfixed, a journey intent does not fail loudly — it produces confidently wrong verdicts. That is worth saying out loud in the design doc.

### 4.2 State-space

**Expressible today? Yes — this is the one that already works and is simply under-used.**

`dashboard.tour.ts:179-226` is already a state-space tour: it uses `tour.holdRoute` (`support/capture.ts:197-214`) to freeze the loading state, then `page.route(… fulfill 500)` for the error state, then `fulfill 200 []` for caught-up — three synthetic states of one surface, all captured, all bound to DSH-011 ("the dashboard never dead-ends") through DSH-003/DSH-004's `serves` edges. DSH-011 **is** a state-space intent. It works. Nobody has generalized it.

What is missing is not mechanism, it is **method and convention**: there is no declared state axis, no way for an intent to say "I require these five states and I should abstain — loudly — if a state is missing," and no way to tell "the tour did not visit the empty state" apart from "the empty state is fine." Today a missing state just means fewer bound captures and a possibly-passing verdict over the happy path. **That is a silent-pass hole**, and it is the most dangerous thing in the current model: an intent that only ever sees the populated state will happily certify a surface whose empty state is a blank white page.

**Required changes:**
1. Add `states?: string[]` to `IntentSpec` (`intent-catalog.ts:13-26`) — the named states the intent requires (`empty`, `one`, `many`, `loading`, `error`, `stale`).
2. Bind them via the **existing `pair.role` field** (`support/types.ts:79-82`) — tours already stamp `pair: { id: 'dsh004', role: 'loading' }`. No capture-format change needed at all; `role` is already "a free label, NOT an enum."
3. In `runIntentPass` (`intent-runner.ts:81-100`), extend the zero-evidence abstain into a **coverage check**: if `intent.states` names a role no bound capture carries, return `unsure` with `reason: 'state <x> not captured'` — or better, grade it and *flag* the missing state in the `IntentGrade` (add `missingStates: string[]`), so the report can say "PASS over 3 of 5 required states."
4. A **tour-authoring helper** — the route-interception idiom in `dashboard.tour.ts:184-219` should be extracted into `support/` (e.g. `tour.withEmptyList(page, matcher)`, `tour.withError(page, matcher, 500)`) so every new tour gets state-space coverage by default rather than by heroics.

**Cost:** linear in states. 5 states × ~20 KB = a full-cap prompt per intent, which is already the budget. The real cost is **tour authoring time**, not tokens — each faked state is 5–10 lines of route interception. Screenshot volume grows 1:1 with captures (they are already taken per capture, best-effort, and never committed).

**Verdict: expressible today, cheapest to build, highest defect-yield per dollar.** The `states` field + the missing-state flag is maybe 60 lines across `intent-catalog.ts` / `intent-runner.ts` / `report/render.ts`. **Build this first.**

### 4.3 Global consistency

**Expressible today? No — and it is the one that needs a genuinely new binding rule.**

Every existing intent binds through `servedBy` (`intent-input.ts:26`), which requires a behavior to *declare* `serves:`. A cross-cutting invariant — "empty states speak in one voice," "dates are formatted the same way everywhere," "every destructive action confirms," "focus is never trapped" — is by definition not owned by any behavior, so there is nothing to hang a `serves:` edge on. You would end up adding `serves: [GBL-001]` to 30 behaviors across 10 domains, which is both unmaintainable and *wrong* (the invariant is not about those behaviors, it is about their union).

The right binding rule for this kind is **selection, not declaration**: bind by capture *predicate*, not by behavior tag. "Every capture in the run whose aria tree contains an empty-state region." "Every capture whose dialogs array is non-empty." "Every capture, full stop, sampled to N."

**Required changes:**
1. A new discriminated field on `IntentSpec`: `bind: { kind: 'served' } | { kind: 'all', sample: number } | { kind: 'predicate', match: … }`. `bindIntentCaptures` (`intent-input.ts:21-39`) grows a branch. The `served` branch is what exists today and stays the default.
2. **A sampling strategy**, because "all captures in the run" is ~50 today and will be ~150 once every domain is toured, against a cap of 8. Naïve truncation would silently judge consistency over the first 8 alphabetically — useless. You need *deliberate diversity*: one capture per tour, or one per distinct URL. This is a real design problem, not a parameter.
3. **A different prompt.** `buildIntentPrompt` (`prompt.ts:185-209`) frames the task as "judge whether the captured surface, taken as a whole, achieves the goal" — singular surface. A consistency prompt must frame it as "these captures are from *different* surfaces; find where they disagree with each other." That is a second preamble and a different grounding rule: a consistency `fail` must cite **two** capture indices (the two that disagree), so `isGroundedIntentCitation` (`intent-runner.ts:49-54`) needs a `minIndices` parameter.
4. Almost certainly needs the **run-scoped UUID mapper from §4.1** too, since a date-formatting comparison across tours is meaningless if the underlying entities are unrelatable.

**Cost:** the highest of the four, and non-obviously so. A consistency intent wants *breadth* — the more captures the better the comparison — which is in direct tension with the 8-cap that keeps the prompt affordable. A 16-capture consistency prompt at ~20 KB each is ~320 KB of evidence plus 16 images; that is comfortably the most expensive single call in the harness, and you would want several of them (empty-state voice, date formatting, destructive confirmation, focus). Realistically **$1–2 per consistency intent per run** at the current tier, i.e. a handful of them *doubles the harness's cost*. The luna swap (`DEFERRED.md:28`) is close to a prerequisite.

**Verdict: not expressible today; needs a new binding kind, a sampling strategy, a second prompt, and a two-citation grounding rule. Genuinely valuable — this is the kind that catches the bugs no domain owner will ever file — but it is the third thing to build, not the first.** One cheap down-payment: a **destructive-confirmation** consistency intent can bind by the existing `dialogs` field with a trivial predicate (`dialogs.length > 0`), needs no sampling strategy (there are only a handful of destructive flows), and reuses the existing prompt almost as-is. That is a 1-day proof of the `predicate` binding kind before committing to the general case.

### 4.4 Regression / comparative

**Expressible today? No — and it is blocked by a deliberate design decision, not an oversight.**

Comparative judging ("is this worse than the approved baseline?") needs a stored baseline capture *of the same surface* to diff against. Two blockers:

1. **Screenshots are never committed.** `support/types.ts:110-113` is explicit: screenshots are "live-run evidence … NEVER committed to the corpus (the PII audit can grep JSON, not pixels)." The corpus's PII discipline (`corpus/pii-audit.ts`, `corpus/scrub.ts`) is built on being able to *grep* every committed byte. A baseline screenshot is a byte-blob the audit cannot inspect. This is not an accident and it should not be casually reversed — the repo's PII rule is one of its hardest constraints, and the CRM's data is real.
2. **The corpus has no baseline concept.** `IntentCaseSchema` (`corpus/schema.ts:123-131`) stores `captures: string[]` + `expected_hypothesis` — a case is *the evidence and what the verdict should be*, not *the evidence and what it used to look like*. There is no "approved capture at SHA X" record and no diff.

But note what *is* already there: the `MutationSchema` doctoring machinery (`schema.ts:22-73`) is exactly a comparative harness pointed the other way — it takes a clean baseline and *deliberately degrades* it to check the judge notices. `DSH-011-doctored.json` is a synthetic regression, and the labeled ground truth confirms the judge catches it. So the harness can already answer "would the judge notice this specific regression?" It cannot answer "did anything get worse since last week?"

**Required changes:**
1. **Aria-only baselines** (the tractable version). Commit the *normalized aria tree + apiResponses* of an approved run as a baseline case — those are already committed, already PII-scrubbed, already diffable. Add `baseline_case?: string` to `IntentCaseSchema`, and a `buildComparativeIntentPrompt` that renders `=== BASELINE[n] ===` sections alongside `=== CAPTURE[n] ===` sections with an explicit "did this get worse?" framing. Grounding: a `fail` cites *both* a baseline index and a capture index. This is maybe 150 lines and it works for structural regressions (a nav link vanished, an empty state lost its CTA, an API response dropped a field) — which is most of them.
2. **Visual baselines** (the hard version). Requires either (a) committing screenshots, which fights the PII rule head-on, or (b) storing screenshots *outside* git in a run-artifact store keyed by SHA, which is new infrastructure. **Do not do this yet.** The aria baseline gets ~80% of the value at ~5% of the cost and zero PII risk.
3. **Baseline promotion needs a human.** "Approved baseline" is a *labeled* artifact — the maintainer must say "this run is the new good." That is the same human-in-the-loop the labeling CLI (`label.ts`) already implements, so the workflow exists; it needs a `promote` mode, not a new concept.

**Cost:** the aria-only version roughly **doubles the prompt size** for any comparative intent (baseline + current), so ~2× tokens on the intents you make comparative. But it *replaces* rather than adds — a comparative intent does not also need a from-first-principles pass. The real cost is corpus churn: every intentional UI change now requires a baseline re-approval, which is a maintenance tax the maintainer will feel every single PR. **That tax is the actual reason to be cautious here, not the tokens.** Snapshot-test fatigue is a well-known failure mode and this is snapshot testing with an LLM attached.

**Verdict: not expressible today. The aria-only baseline is a genuinely good, cheap idea. The visual baseline collides with the PII rule and should be deferred indefinitely. And the whole kind should be gated on whether the maintainer actually wants a re-approval step in their PR flow — if not, build the other three and skip this one.**

---

## 5. Recommended sequencing

The ordering principle: **prove the model generalizes before you extend it.** Every new *kind* (§4) is a bet on the model being worth expanding; the cheapest way to test that bet is to expand the *existing* kind into a domain that has never had one, and see whether it finds anything.

**Step 1 — Free coverage, zero new machinery (half a day).** Add `serves:` edges from KNW-034/035, NTS-007/008, TDS-035 to the existing **CAD-036** intent. Five behaviors, three domains, no new intents, no new tours — the cadence tour's contact-detail captures already contain this evidence. Update `intent-catalog.ts`'s `servedBy` (the drift test will tell you if you got it wrong) and re-run the report. **This is the cheapest possible test of whether inverted edges do real work, and it either finds something on the contact page or it does not.**

**Step 2 — Settings intents + `settings.tour.ts` (the proving ground).** Mint SET-035…SET-040 in `spec/settings.yaml`, wire the 12 `serves:` edges from §3.1, transcribe into `INTENT_CATALOG`, and write the tour. The tour is mostly route interception (the states that matter — partial scope, auth-failed, unconfigured, half-configured Todoist — are all unreachable from a seed), which means **it doubles as the state-space proof**. Start with **SET-036** (disconnect confirmation — a direct clone of CON-050's shape, so it proves generalization with near-zero design risk) and **SET-040** (the export-promises-what-it-delivers intent — the one that catches a real live dishonesty nothing else can see). If SET-040 does not fire, that is important information about the whole thesis.

**Step 3 — State-space, formalized (§4.2).** Add `states?: string[]` to `IntentSpec`, bind through the existing `pair.role`, and make a missing state *visible* in the grade rather than silently absent. Extract the route-interception idiom out of `dashboard.tour.ts` into `support/`. ~60 lines of harness + a helper. Do this **after** the settings tour, because writing that tour is what will tell you which state helpers you actually need. **This closes the model's most dangerous hole — an intent that only ever sees the happy path and certifies it.**

**Step 4 — Fix the run-scoped UUID mapper (§4.1), even if you build no journey intents yet.** It is ~40 lines, it is a correctness fix regardless (cross-tour evidence is currently *incoherent*, and nothing in the harness says so), and it is the prerequisite for both journey and global-consistency intents. Bump `CAPTURE_FORMAT_VERSION` while you are there and add the run-global capture ordinal.

**Step 5 — One journey intent, as a spike.** The obvious one: **"from the dashboard, a user reaches the contact that needs attention and acts, without losing context"** — served by CAD-026/CAD-028 (dashboard) + CON-038/CON-051's behaviors (list/detail) + CAD-029 (contact detail). It spans all three existing tours, so it needs no new tour — only Step 4's fix, a raised cap, and chronological ordering. If it produces a coherent verdict, the journey kind is proven for the cost of one intent row.

**Step 6 — The destructive-confirmation consistency intent, as the `predicate`-binding spike (§4.3).** Bind by `dialogs.length > 0`, no sampling strategy needed, reuse the prompt. If it works, the general consistency machinery is justified; if the verdict is mush, you learned that for one day of work instead of a week.

**Explicitly deferred:** the visual-baseline half of §4.4 (collides with the PII rule), general global-consistency binding (needs a sampling strategy that is a real design problem), and **mac-host intents** (unreachable by a browser tour — declare the domain out of scope rather than leaving a permanently-abstaining intent).

**One sequencing dependency worth flagging:** the `gpt-5.6-luna` intent-model swap already scoped in `judge/DEFERRED.md:28` cuts the intent pass ~5× ($1.42 → $0.28/run). Every kind in §4 is a token multiplier on that pass. If the swap validates, the cost objections to journey and consistency intents largely dissolve. **It is worth doing the luna evaluation before Steps 5–6, not after** — it changes what is affordable.
