# LLM Extraction Program — North Star

A program-level spec sitting *above* its implementation sub-projects. It defines the value, jobs-to-be-done, functional goals, principles, and decomposition for using LLM inference to extract relevant and valuable information from interaction data — including message and session/transcript content. Individual sub-projects (and the data-model work) get their own specs; this document is the shared reference they implement against.

Related dependencies: Gmail integration ([#70](https://github.com/spengrah/PersonalCRM/issues/70)), gchat integration ([#337](https://github.com/spengrah/PersonalCRM/issues/337)), and the Anarlog session pipeline ([`mac-daemon-phase-2-anarlog-matching.md`](./mac-daemon-phase-2-anarlog-matching.md), which already anticipates a "future transcript pointer" on `meeting_note`).

## North Star

The CRM already collects a pile of conversational content — text messages, Telegram, calendar descriptions, and Anarlog meeting summaries — and extracts almost no meaning from it. An `interaction` is metadata-only (who, when, direction, source); a `contact` gets derived *timestamps* but nothing about *what was said, what matters to this person, or what you owe them*. This program turns that raw content into a **stateful, trustworthy understanding of each relationship** that informs how you show up — by reading content (not just metadata) to correct what the system gets wrong, surface what you should act on, and maintain a living sense of each person.

The single moment this is all in service of: **the briefing** — "I'm about to call/meet/text someone; catch me up." Every extraction below feeds it.

## Jobs to be done

Five families of value, spanning two distinct kinds of work — **correction** (fix a signal the system already produces, badly) and **enrichment** (derive knowledge that exists nowhere today):

- **Interpret / Correct** — read content to get the *interaction itself* right. The system flattens every message into a timestamped, directional event; conversations have *state* the metadata can't see. Correction jobs are self-validating: you feel it immediately when the CRM stops nagging you for a reply you don't owe.
- **Act** — surface things you should *do*: commitments you made, open loops, follow-ups. Bidirectional — tasks in *your* court (your todo) and tasks in *their* court (a reason to nudge them).
- **Prioritize** — make outreach smarter than fixed cadence: who's actually drifting and *why*, whether your set cadence matches reality.
- **Remember** — facts and context about a person so you show up informed, especially *significant* moments (a job loss, an illness, a move, a milestone).
- **Understand** — higher-altitude relationship signal: closeness, recurring themes, how a relationship is trending.

The center of gravity is **action-producing** extraction (emit a corrected state, a flag, a task, a nudge), not passive trivia. A "knows their kid's name" file is only valuable insofar as it surfaces into the briefing.

## Two modes + the keystone

Everything produces value in one of two ways, consumed by one on-demand surface:

- **Discrete extraction** — new content arrives → infer candidate item(s) → (review) → commit. Event-triggered, one-shot per piece of content. (Ball-in-court, substance filter, task extraction, big-things, drift.)
- **Living-profile maintenance** — content continuously folds into an accumulated, kept-current understanding of each person. Stateful, never "done."
- **The briefing (keystone)** — on-demand, reads *both* layers: the living profile plus the live open loops, at the moment before you reconnect.

The shared backbone under every discrete job: **content → infer candidate items → surface for review → commit to state or fire an action.** Build that pipeline once; the jobs are different extractors plugged into it.

## Functional goals

Each goal names the content unit it operates on (the "unit-per-job" — extraction window is a property of the extractor, not a global choice) and the flow from what it reads to where it surfaces.

| Goal | Family | Mode | Content unit | Reads → Emits → Surfaces |
|------|--------|------|--------------|--------------------------|
| Ball-in-court | Interpret/Correct | Discrete | Per-thread/conversation | Thread text → corrected "reply owed?" state → outreach queue (stops false nags) |
| Substance vs. noise | Interpret/Correct | Discrete | Per-message/short-burst | Interaction content → a "meaningfulness" weight → cadence math, what counts as real contact |
| Surface "big" moments | Remember + Prioritize | Discrete | Per-message / per-session | Messages, transcripts → flagged life-event → briefing, check-in prompt, profile |
| Task extraction (bidirectional) | Act | Discrete | Per-conversation / per-meeting | Messages, transcripts → my-tasks **+** nudge-them tasks → Todoist, profile open-threads, briefing |
| Dynamic cadence | Prioritize | Aggregate | Per-contact, time-windowed | Interaction pattern + substance → cadence-change suggestion → re-cadence prompt |
| Drift-with-why | Prioritize + Understand | Aggregate | Per-contact, time-windowed | Recent history + content → drift alert **with reason** → outreach queue, check-in prompt |
| Living relationship profile | Remember + Understand | Continuous | Continuous fold | All content over time → maintained profile buckets → contact page, briefing |
| The briefing (keystone) | — consumer — | On-demand | Per-contact | Profile + open items → pre-interaction summary → MCP server (primary) + in-app surfaces |

The living profile is made of a handful of buckets: **context/biography** (job, family, location, health, what's going on), **what they care about** (interests, values, recurring topics), **relationship texture** (closeness, how you met, shared history, inside references, the *real* cadence vs. the set one), **open threads** (where the discrete layer surfaces into the profile), and, eventually, **their network** (who they know — the connector angle).

## Principles

- **Ingest the rawest available signal.** Source-side summaries (e.g. Anarlog's local Qwen-generated summary) are ingested as *bonus metadata* — a cheap pre-filter, a display fallback — but never as the substrate. The transcript is the substrate. A summary is a lossy projection, and different jobs want different projections of the same content.
- **Extract centrally.** One engine, one set of extractors, one review/provenance model across all sources. Sources are producers of raw content; the CRM is the single extraction brain.
- **Provenance to raw.** Every emitted item — fact, task, state correction — links back to the source content that produced it, so it can be verified and trusted.
- **Re-extractable / backfill-first.** Because raw is retained, a better extractor (or a new job invented later) can be re-run across the existing corpus. Upstream lossy extraction would forfeit this; we do not.
- **Private, configurable inference endpoint.** Extractors call a configurable model API endpoint (OpenAI-compatible), with **per-job model selection** (a cheap model for the substance filter, a stronger one for profile synthesis). **Venice (private model endpoint — no logging, no retention) is the expected default**, and is what reconciles "send content to an LLM" with "local-first." The endpoint is swappable (local model, others). **Source-of-truth ≠ inference-location:** where the LLM runs is an independent, swappable choice; Anarlog's local model is potentially just one more endpoint the engine can call.
- **Human review builds trust.** LLMs are wrong sometimes, and a wrong auto-correction erodes trust faster than no correction. Each job declares whether it auto-applies (self-validating corrections) or requires confirmation (tasks, profile facts, re-cadencing). The rollout/testing strategy is first-class, not an afterthought.
- **Unit-per-job.** Each extractor declares its natural content window (per-message, per-thread, per-meeting, per-contact-aggregate, or continuous fold); the runtime batches accordingly.
- **The briefing is structured/MCP-first.** It is queryable, composable data served to an agent — not primarily a rendered page. In-app UI is a secondary consumer of the same structured data.

## Non-goals

- **No phone-call/voicemail content extraction.** `phone_call` is metadata-only (direction, duration, answered/voicemail flags); no transcript or audio is ingested. Calls still inform cadence as metadata.
- **No attachment/media-content extraction.** Only type/metadata tags, as today.
- **No autonomous outward action.** The system surfaces tasks, nudges, and corrections for the user; it does not send messages or take outward action on the user's behalf. (Internal state corrections may auto-apply per the review model.)
- **Not a generic chatbot over your data.** Scoped to the defined jobs and the briefing.
- **No vendor lock-in and no logging/training endpoints.** Content only goes to a privacy-preserving, configurable endpoint.

## Program decomposition

Four sub-projects plus one cross-cutting pillar. Each sub-project gets its own spec.

- **SP1 · Relationship data model (graph foundation).** Where extracted knowledge lives: the living-profile buckets, person↔person edges (the network/connector angle), fact provenance, and the staleness/contradiction/supersession model. Aligns with the planned graph re-architecture.
- **SP2 · Ingestion / source completion.** The "fuel" foundation: Gmail ([#70](https://github.com/spengrah/PersonalCRM/issues/70)), Google Chat ([#337](https://github.com/spengrah/PersonalCRM/issues/337)), and Anarlog *raw transcripts* (extend the Mac daemon ingest to carry the transcript alongside the existing summary/memo). Messages and Telegram content already land today.
- **SP3 · Extraction engine + extractors.** The shared backbone (*content → infer candidates → review → commit*), the configurable model-endpoint integration (Venice default, per-job model selection), the unit-per-job runtime, the individual extractors, and backfill.
- **SP4 · Consuming surfaces.** The briefing served via an **MCP server (primary consumer)**, outreach-queue corrections, task surfacing into Todoist, the contact-page living profile, and big-thing / drift flags (likely via the existing needs-attention surface).
- **Cross-cutting pillar (not a sub-project): trust / review / provenance.** The review model, auto-apply-vs-confirm thresholds, and rollout & testing strategy. Constrains SP3 and SP4 throughout.

## Build sequence

An A/B hybrid: front-load the *input* foundations (more fuel benefits everything, and these are already-planned dependencies) while proving the engine on a single self-validating job before investing in the data model and the diffuse-value enrichment layers.

1. **Foundations + first proof.** Finish SP2 ingestion (Gmail, gchat, Anarlog transcripts) **and** ship the **ball-in-court** extractor on the existing relational model. Full fuel in; engine, configurable endpoint, and the review loop proven on a self-validating correction that earns trust cheaply.
2. **SP1 — graph data model.** Profile buckets, edges, provenance, staleness/contradiction.
3. **Discrete extractors + surfaces.** Substance filter (correction), task extraction (→ Todoist), big-things (enrichment).
4. **Living profile + briefing + MCP server.** The capstone.
5. **Aggregate jobs.** Dynamic cadence, drift-with-why.

## Open questions

Deferred to the relevant sub-project specs, but tracked here so they are not sleepwalked past:

- **Staleness & contradiction model** (SP1): fact validity, supersession, decay. "Job-hunting" two years ago is stale; "lives in NYC" → "moved to LA" is a contradiction.
- **Group / multi-party attribution**: who-said-what in group threads and multi-person meetings; whether a group message counts as contact with each participant. (May be eased by the graph model.)
- **Review granularity & thresholds**: which jobs auto-apply vs. confirm; batch-review UX; whether review can happen via the MCP surface.
- **Task reconciliation**: how extracted tasks/nudges reconcile with the existing Todoist outreach-task model (dedup, lifecycle, ownership).
- **Profile representation**: regenerate-from-scratch vs. incremental fold; how the graph stores the buckets and their provenance.
- **Relationship-type classification** (work / friend / family): needed to keep work noise out of a personal tool — in scope or out?
- **Cost & scheduling**: real-time vs. nightly extraction; first-run backfill batch cost; per-job model matrix against Venice rate limits.

## Shared instrumentation with the agentic QA harness (added 2026-07-08)

The Piece 4 QA-harness design (`.ai/spec/2026-07-08-piece4-track-b-agentic-qa-harness-design.md`, #380) established an instrumentation layer deliberately shared with this program: both are LLM pipelines needing evals (golden corpora, regression gating), tracing, and dataset/labeling workflow, while their brains differ by design (QA judge = codex under subscription economics; extractors = Venice under this spec's privacy principles). SP3 should adopt rather than reinvent: OTel GenAI span conventions at the inference adapter, the same eval-runner pattern (promptfoo or the shared fallback runner — decided by a spike in the harness's PR2), and the planned self-hosted observability platform (Langfuse or Phoenix on the VPS; a follow-up issue shared by both programs). Self-hosting is a hard requirement here, not a preference — extraction traces carry real message content, and a hosted trace platform is a logging endpoint excluded by the no-logging/no-retention principle. Raindrop's OSS Workshop (MIT, local-only trace debugger) is a candidate dev-loop viewer for SP3 extractor iteration; if used, its optional cloud connect stays off.
