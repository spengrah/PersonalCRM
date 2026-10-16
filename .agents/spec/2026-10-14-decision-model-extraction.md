# Decision-model extraction

Use decision models to answer batches of categorical questions over interaction content, write every answer as an SP1 assertion, and let the user confirm or correct those answers on the bench. It covers the thread questions: ball-in-court, commitments and big moments. Merge proposals for duplicate interactions reuse this machinery and have their own spec, `.agents/spec/2026-10-14-merge-proposals.md`. The user's reviews are the product's correction path and, as a byproduct, the eval material that compares methods.

This spec replaces the in-progress ball-in-court eval spec recorded in `.agents/spec/2026-10-12-decision-models-session-notes.md`. Those notes stay the research record; their two open rulings are settled here (see Relation to existing & planned work).

## Terminology

- **Decision model**: a model that reads one shared context (its "state") and answers typed questions (yes/no, choice, score) with a probability per option, in one forward pass, generating no text.
- **Thread**: one conversation container with content: a one-to-one chat, a group chat or an email thread. A thread is a venue node in SP1.
- **Cut-off**: the time up to which a thread's content is read for one judgment.
- **Thread context**: a thread's whole content up to a cut-off, with speakers labeled by role (`ME`, `THEM`, `PERSON_2`, …) and identifiers masked.
- **Question tree**: the full set of questions asked of one thread context, arranged so that some answers make others irrelevant.
- **Method**: everything that produces a judgment and can be swapped: masking variant, access layer, adapter, endpoint, model and question wording.
- **Production method**: the one method whose judgments may change live state.
- **Judgment**: the answers one method gives to one question tree for one thread at one cut-off.
- **Current judgment**: a thread's judgment from the production method whose cut-off is at or after the thread's latest content.
- **Obligation**: an open item of who owes whom what on one thread: a reply or a commitment, owed by the user or by another participant. Once opened, an obligation stays open until something listed under Architectural direction closes it.
- **Addition**: a judgment outcome that opens an obligation or raises a flag.
- **Removal**: a judgment outcome that would close an obligation, end an awaiting-reply wait, or move an obligation's date later.
- **Claim**: a proposed assertion awaiting the user's confirmation.
- **Apply rule**: additions at or above the confidence threshold take effect at once; additions below it, removals and merges stay claims until the user confirms them.
- **Effective correspondence state**: the deterministic timestamp rule's awaiting-reply state, composed with open obligations and confirmed claims.
- **Historical run**: running the jobs over one past month of content, at cut-offs that simulate the production schedule.
- **Bench**: where the user reviews judgments: the all-interactions view this spec adds, rendering each interaction's drill-down with its raw content beside the assertions that cite it.

## Context & problem

An interaction is metadata: who, when, direction, source. The CRM derives one correspondence signal from it. A contact is awaiting a reply when the user's last outreach is later than the contact's last response and the waiting period has not expired (CAD-040). That covers "they owe a reply", contact-wide, and nothing else:

- No state represents "I owe a reply". An inbound message updates cadence fields only, so a question the user never answered leaves no trace.
- Commitments ("I'll send you the deck Friday", "I'll ask my sister") are invisible in both directions.
- Significant moments in a contact's life (a new job, an illness, a birth, a move) sit unread in message bodies.
- The timestamp rule cannot see a thread close: when the user's last message is "thanks, sounds good!", the contact stays in an awaiting-reply wait although nobody owes anything.
- Duplicate interactions (overlapping calendar series, a call and a meeting that were one event) are fixed only by a manual merge, which shipped in #918 and which the user has not yet used. #372 left automated merge proposals to a later decision model; they have their own spec.

The interaction-review-readiness arc made the evidence fit to judge: message bodies, group-chat completeness, corrections as SP1 assertions, and assertions shown beside the content they cite. Decision models became practical in October 2026: Clef launched on Workers AI on 2026-10-01, Jev became self-serve, and Sage's terms took effect on 2026-10-05. They are cheap per token and answer many questions over one shared context in one call. The workloads here read roughly the same content, which is why they belong in one design.

The earlier eval spec framed this work around evals. This spec frames it around the production workload, with evals as a byproduct of the user's reviews.

## What matters

Confirmed by the user, in priority order:

1. **Extraction quality before surfaces.** Get the data right first; showing it on a new surface is cheap later.
2. **Recall over precision for actionable state.** A missed "I owe", a missed commitment, a missed big moment or an unflagged duplicate is worse than a false positive, because the user corrects false positives on the bench.
3. **Privacy.** No real content at rest off the Pi in the CRM or its tooling. Each hosted vendor's terms are recorded for the user's choice of production method (HC3). Two exposures are protected separately: the identities of the people in the content, and the link between the content and the user. Masking protects the first; identity-unlinkable access is an optional layer for the second.
4. **One act of judgment.** Model answers are SP1 assertions. The user's confirmations and corrections on the bench are the product's correction path and the eval labels at once.
5. **Trust grows through historical runs.** Run the jobs one past month at a time from January 2026, review and correct on the bench, improve the jobs, run the next month. The same loop is what later loosens the apply rule.
6. **Batched questions over shared context.** One call per thread context asks every question. The context unit is chosen per question kind, never as a per-contact bundle that resends a group thread once per member.
7. **Small and swappable.** Any part of a method can be replaced; nothing depends on one vendor or on the user's Mac being awake.

Secondary: cost, latency and calibration.

## Intent & appetite (delegation charter)

**What it is for:** the user's own correspondence management: never drop a thread, and know what is going on with people. It serves this goal, not cadence. Judgments never write cadence state; existing rules that read correspondence state (the cadence task waiting out an awaiting-reply window, merge recomputation) keep working as they do. One user, on real prod data. In production the jobs run on a schedule (nightly by default), not on every ingest event. During the historical runs the user reviews a month of judgments and claims in attended sessions on the all-interactions view, then the jobs improve and the next month runs.

**What failure costs:** a missed obligation costs a relationship moment the user cares about, and it is silent. A false positive costs one correction on the bench. Content reaching a vendor that trains on it or keeps it unmasked, or unmasked content reaching any hosted endpoint, exposes third parties who never agreed to it and cannot be undone. A model output that silently takes a task away from the user costs trust in the whole system, which is why removals start as claims.

**Appetite:** small. Expected inference cost is single-digit dollars a month at production volume. The eval harness compares methods on judgments the user already reviewed; it is small and shared with later extractors. Build adapters only for methods actually being compared. Calibration and other secondary metrics earn their effort only when choosing a production method. One new UI surface, the all-interactions view, which is the bench; no other new surface. No persistent tests of the harness, scorers or runner (throwaway tests are fine); the masker is the exception and is production code.

**Rabbit holes (do not enter, even in service of a priority):**

1. Message-burst identity, segmentation or merging. Bursts stay outside the graph; threads, not bursts, are the context unit.
2. Splitting one interaction into two.
3. Generating text from a judgment (a task's wording, a big moment's summary). A flag may later trigger generative inference; that is a later arc.
4. Surfacing flags anywhere beyond the bench, and filters or views on the all-interactions view beyond what reviewing a run needs.
5. Unmerge, unless the historical runs show merges need reversal by correction.
6. Overfitting machinery, Langfuse datasets or annotation queues.
7. Per-contact context bundles.
8. Adapters or access layers for methods nobody is comparing, including identity-unlinkable access before a compared method uses it.
9. Event-triggered extraction on each ingest.
10. A segmentation engine for contexts over a model's limits.
11. Pulling transcript ingestion or a new content reader into this arc to cover meeting content.
12. Tuning the reasoned-options experiment beyond comparing it as one method variant.

**Call-me-when:**

- A production method would send unmasked content to any hosted endpoint.
- Any path would let a model output close an obligation, end a wait or move a date later without a claim.
- A historical run's live task effects are more than the user can review in one sitting.
- Thread contexts over a candidate model's limits are common enough that the one-batch constraint and recall conflict.
- Inference cost for production volume lands an order of magnitude above the researched estimates.
- A correspondence effect would change cadence state other than through the existing rules named in "What it is for".

**User-reserved decisions:**

- Loosening the apply rule: letting any removal or merge apply without confirmation.
- Adopting a hosted vendor or access layer for production use on real content.
- The first production method, and any later change of its vendor, model or access layer. The arc may adopt a change of question wording or confidence threshold without asking when the scorecard shows no example getting worse on thread contexts the user already reviewed, logging each change.
- The bench's design: the all-interactions view is built only against a design the user approved (user ruling, 2026-10-15).
- Any cadence effect beyond the existing rules named in "What it is for".

**Tradeoff rules for a delegate:**

- When recall and precision of an addition compete, recall wins.
- When privacy and accuracy compete, privacy wins below the call-me-when threshold above.
- When extraction quality and surface polish compete for budget, extraction quality wins.
- When a smaller harness and a more general one compete, the smaller wins.
- When a cheaper path would store a judgment where a later graph migration must re-home it, take the path that does not.

**Unattended running:** the arc may run unattended through the questions this spec defers to the arc, deciding by the guidance given for each and logging every decision with the passage it cites.

## Goals

- **G1** A thread-context builder: a thread's whole content up to a cut-off, deduplicated across per-contact copies, with participants labeled from source identity rather than display names.
- **G2** A masker on the Pi covering the whole hosted request, including sender headers and any metadata: role labels, regexes for formatted identifiers, the CRM's own contact names and nicknames as a dictionary, consistent placeholders within a thread, and a stated treatment of names the dictionary does not cover. Weekday and relative-time words survive. The placeholder map never leaves the Pi.
- **G3** One question tree per thread context, asked in one batch, about obligations that involve the user only. Participants who are not CRM contacts are context only: obligations open only between the user and matched contacts (user ruling).
  - **G3a** **Ball-in-court**, per participant: does the user owe them a reply, do they owe the user a reply, or does nobody owe one; plus an option for a thread the questions do not apply to (an automated message, a broadcast).
  - **G3b** **Commitments**, per participant, as two directional questions: has the user said they would do something for that participant not yet done, and has that participant said they would do something for the user not yet done. Both can hold at once. A commitment supersedes a reply obligation for the same party: the reply that states a commitment and the commitment yield one obligation.
  - **G3c** **Big moments**: does the thread reveal a significant moment in a participant's life (a new job, an illness, a birth, a move, or similar).
- **G4** Every judgment recorded in SP1 with at least: the method and its version, the run, the cut-off, the input hash, the probability of every option, and the model's original answer. Two methods that agree remain two records; the accepted fact is separate from the judgments behind it.
- **G5** The user's reviewed outcome recorded for every judgment the user reviews, auto-applied additions included, with the right answer when the user rejects one.
- **G6** The apply rule enforced in the shared assertion write path, with the predicate catalog as its data, including a confidence threshold for auto-applied additions.
- **G7** Obligations and their tasks:
  - **G7a** Each open obligation has one task, keyed by thread and owing party, under a lifecycle of its own (distinct from the contact-wide follow-up loop). When a party has several open commitments on one thread, the one obligation covers them all and takes the earliest known date; completing its task closes it, and a commitment still open reappears at the next judgment as a new obligation (user ruling; see Assumptions & deferred questions for the later improvement).
  - **G7b** "I owe a reply": a reach-out task; completing it records outreach and closes the obligation.
  - **G7c** "I owe" a commitment: a reminder task; completing it closes the obligation and records no correspondence.
  - **G7d** "They owe" a commitment: a reach-out (nudge) task, timed by the commitment's own timing when known, else the cadence's watchdog days, else 7 days for a contact without a cadence.
  - **G7e** "They owe a reply" stays with the timestamp rule's contact-wide wait and follow-up; a judgment of "they owe a reply" where the rule shows no wait opens a thread obligation.
  - **G7f** A big moment is an accepted flag, visible on the bench.
- **G8** Claims reviewable on the bench: the user confirms, rejects or corrects each, and each act is an SP1 assertion with user provenance.
- **G9** An all-interactions view: every interaction across contacts, paginated, filterable at least by contact and interaction type, and able to narrow to the interactions that carry judgments or claims from one run. It reuses the row and drill-down components the readiness arc built to render outside the contact page and keeps each item's typed identity and actions. One thread judgment has one review, not one per participant row. For a historical judgment, the drill-down shows the content as of its cut-off and marks later content as later.
- **G10** Historical runs: for a chosen month, judge a thread at a scheduled cut-off only when it gained content since the previous scheduled cut-off, the same eligibility rule production uses, with pre-month content included in each context.
- **G11** Incomplete work is visible: a context over a model's limits or a failed scheduled call is reported, never silently truncated or skipped.
- **G12** A scorecard: re-run any method over the thread contexts the user reviewed and report, without content, per-outcome recall and precision, a confusion matrix, and which examples flipped against another method. The timestamp rule is the reference for "they owe a reply" only, since it never produces "I owe".
- **G13** Masker regression tests that gate merges, over synthetic fixtures.
- **G14** Identity-unlinkable access as an optional method setting, built when a compared method uses it.
- **G15** Content-free records reach Langfuse, in its own extraction project (user ruling, 2026-10-15): each judgment's method and version, outcome, option probabilities, cut-off, token counts and cost, latency, context size, question count and question-wording version; the user's verdicts; scorecard metrics per run; and run summaries (counts, incomplete threads, cost). Records carry an opaque example ID that only the Pi maps back to a thread, never a thread or contact ID. Planners and implementers may propose further content-free fields once they see what each source returns.

## Non-goals

- **NG1** Message bursts as interactions, burst segmentation and burst merging.
- **NG2** Splitting interactions.
- **NG3** Text generation from any judgment.
- **NG4** New surfaces for flags or judgments other than the all-interactions view.
- **NG5** Unmerge and undo history.
- **NG6** Call, voicemail and attachment content (extraction program non-goals).
- **NG7** Living profile, briefing, drift and dynamic cadence (later program phases).
- **NG8** Judgments writing cadence state.
- **NG9** Removals or merges that apply without the user's confirmation.
- **NG10** Obligations between two participants other than the user.
- **NG11** Merge proposals (`.agents/spec/2026-10-14-merge-proposals.md`).

## Relation to existing & planned work

- **LLM extraction program** (`.agents/spec/llm-extraction-program.md`, #379): this is the first SP3 slice, widened from ball-in-court alone to every decision-model workload over interaction content. The program's task-extraction and big-moment goals arrive here as obligations and flags; their generative halves stay with the program. The program's "visible why from day one" ruling is met by the bench showing the raw content beside each judgment.
- **Decision-model session notes** (`.agents/spec/2026-10-12-decision-models-session-notes.md`): the research record. Its open rulings are settled: judgments are SP1 assertions (its Shape A), and the masker is production code with regression tests while scorers, metrics and the runner get throwaway tests only. Its eval priorities are absorbed into What matters; its masking-cost method (one model, raw and masked) is not used, since no local model is compared (user ruling, 2026-10-16).
- **Interaction review readiness** (`.agents/spec/2026-10-13-interaction-review-readiness.md`, #918): provides interaction nodes, corrections as SP1 assertions, the interaction-node merge, reusable row and drill-down components, and the read of assertions beside cited content (KNW-038). This spec builds the claim review that arc designed for but did not build, and the all-interactions view takes the place of its deferred cross-contact review queue. Its deferral of chat-burst identity stands.
- **Awaiting-reply derivation and cadence skip** (`.agents/spec/2026-09-15-awaiting-reply-derivation-and-cadence-skip.md`, CAD-040…CAD-046): the timestamp rule and its contact-wide wait stay, and compose with obligations. CAD-040's rule that no Todoist-side action changes the awaiting-reply state still holds for that wait; obligation tasks are new, and completing one closes its own obligation. Contacts whose threads have no judgment behave exactly as today.
- **Task kinds and lifecycles** (CAD-022, `.agents/spec/task-direction-taxonomy.md`): obligation tasks reuse the reach-out and reminder kinds for what completion records, and add a lifecycle for obligations.
- **Skip and follow-up decoupling** (owed, no spec): confirming a "nobody owes" claim ends a wait without a cadence skip. This spec builds that act only for claims; the decoupling arc decides whether it becomes the general "stop waiting" action.
- **Merge proposals** (`.agents/spec/2026-10-14-merge-proposals.md`, #372's deferred automated half): reuses this spec's masker, methods, scorecard, claims and bench. Nothing here may assume merge proposals exist.
- **#834** (Anarlog raw transcripts): a later content source; meeting content enters this arc only through an existing reader (see Deferred to the arc).
- **#842, #843**: shipped evidence fixes the thread context depends on.
- **Observability decision** (`.agents/spec/2026-07-11-llm-observability-platform-spike-decision.md`): Langfuse is a metrics plane only, in its own extraction project, with content-free records keyed by example ID. That decision permits more than this spec does, so no existing exporter is reused unchanged.

## Prior art & external constraints

Research ran on 2026-10-04…2026-10-12 (session notes) and 2026-10-14 (below). Claims from secondary sources are marked "reported".

- **Batch interface.** Clef takes 1–64 questions over one state; Jev sets no question cap; each question is answered independently, so adding one does not change the others ([Cloudflare Clef](https://developers.cloudflare.com/workers-ai/models/clef/), [Vercel Jev guide](https://vercel.com/kb/guide/typesafe-jev-and-ai-sdk)). Whether hosted vendors bill the shared state once or per question is undocumented. Kev, run locally, caches the state and pays only for added questions ([Kev](https://github.com/jaredpalmer/kev)). This is what makes a full question tree in one batch, with irrelevant answers ignored, cheap.
- **Reasons in options.** No vendor documents options that carry a reason. Vendors recommend per-option descriptions. One paper on small open decision models found that option labels dominate definitions (a label contradicting its definition collapsed accuracy; dropping definitions changed little) ([arXiv 2610.02586](https://arxiv.org/abs/2610.02586), figures reported). A reason has effect only when written into the label itself ("Yes, because the last message asks a question"), so reasoned options are an experiment, not a design. Several reasoned "yes" options split one action's probability, so the comparison sums them.
- **Vendors and retention.**
  - Jev (TypeSafe): zero data retention and no training on prompts, per Vercel AI Gateway's ZDR table quoting TypeSafe's terms; self-serve access ([Vercel ZDR](https://vercel.com/docs/ai-gateway/security-and-compliance/zdr.md)).
  - Clef and Clef-Flash (Cloudflare Workers AI): no training, no storage unless the caller uses R2 or KV ([Workers AI data usage](https://developers.cloudflare.com/workers-ai/platform/data-usage/)).
  - Sage (Levanto Labs, proprietary): no retention of prompts or results and no training, except full payloads kept 30 days for failed or abnormally slow requests ([Levanto privacy](https://levanto.ai/privacy)).
  - OpenAI Decisions API: public beta, account-based, ZDR for eligible customers ([OpenAI](https://developers.openai.com/api/docs/guides/decisions)).
- **Local option.** MLX builds of Clef and Clef-Flash ship the decision head; community GGUF builds reportedly do not. Kev on an M5 answered five questions over a short thread in 0.15–0.7 s; Clef-Flash at 4 bits reportedly takes about 0.3 s per 1,000 tokens on an M5 Max ([mlx-community/clef-8bit](https://huggingface.co/mlx-community/clef-8bit), [Jundot/clef-flash-oQ4e](https://huggingface.co/Jundot/clef-flash-oQ4e)). The VPS CPU is too slow (8 tokens/s prefill, session notes). No local model is compared (user ruling, 2026-10-16).
- **Identity-unlinkable access.** zkAPI (Ethereum Foundation and Open Anonymity, mainnet 2026-10-01) unlinks payment and account from requests through a zero-knowledge proof of a funded deposit, issuing short-lived OpenRouter keys. The provider still sees prompts and responses in full; content, IP addresses and timing can re-link sessions; deposits are public onchain; the repo calls the protocol experimental ([EF blog](https://blog.ethereum.org/2026/10/01/introducing-zkapi), [repo](https://github.com/ethereum/zkapi)). No source shows a zkAPI key reaching a decision model. Enclave-based services (Venice, NEAR AI, Tinfoil, Phala) keep the operator from reading content but stay account-based. Unlinkable access therefore complements masking and never replaces it.
- **Masking.** Ball-in-court depends on who spoke last and the act of the message (question, request, promise, closer), not on who the people are, so masking costs it little in expectation. Passing the regression fixtures proves the listed identifiers are caught, not that a context is anonymous (session notes).

## Hard constraints

- **HC1** **No real content at rest off the Pi** in the CRM or its tooling. The runner, the scorecard and every tool keep no copy elsewhere; standard output is content-free. Transit to an inference endpoint is allowed under the next two constraints.
- **HC2** **Masking before any hosted call,** over the whole request. No unmasked content or metadata reaches a hosted endpoint. The placeholder map never leaves the Pi.
- **HC3** **Vendor terms recorded.** Each hosted method's vendor terms are read at the primary source and recorded: what they show about training on request content and retaining it. The record informs the user's choice of production method and excludes no method; which terms are acceptable is the user's decision (user rulings, 2026-10-15 and 2026-10-16). Every hosted call carries masked content only (HC2).
- **HC4** **One store for judgments.** Model answers, claims and the user's confirmations and corrections are SP1 assertions; no parallel judgment store.
- **HC5** **Model output never removes actionable state without confirmation** under the starting apply rule.
- **HC6** **The user's verdict wins.** A later judgment on the same input never re-proposes an answer the user rejected and never overwrites the user's correction. Only changed content reopens a question.
- **HC7** **Only the production method changes live state.** Every other method's judgments are evidence only.
- **HC8** **Judgments never write cadence state.**
- **HC9** **Raw evidence untouched.** No judgment changes an observation or a content row.
- **HC10** **Content leaves once per thread context per method.** A group thread is sent once per cut-off for each method evaluated, never once per member. Method comparisons and retries of a failed call are allowed.
- **HC11** **No silent truncation or skipping** of a thread context.

## Architectural direction

- **Constraint: the thread is the context unit for the thread questions.** Questions name participants by role ("Does PERSON_2 owe ME a reply?"); the Pi maps placeholders back to contacts.
- **Constraint: one batch per thread context holds the full question tree.** Answers made irrelevant by an answer higher in the tree are ignored, not avoided.
- **Constraint: judgments are SP1 records with extractor provenance** that keep each method's answer and probabilities distinct from the accepted fact (user ruling on the minimum each record keeps; Goals lists it).
- **Constraint: the apply rule's data lives in the predicate catalog and its enforcement in the shared write path.** Additions auto-apply at or above a recall-oriented confidence threshold, and below it land as claims with no task until confirmed (user ruling); removals and merges are always claims. Loosening the rule is a data change, not new code.
- **Constraint: only a current judgment opens an obligation** (user ruling). A judgment whose cut-off precedes later content in the same thread is history and eval material only. So a historical run over January opens nothing for a thread the user answered in February, and a thread quiet since January still opens its obligation.
- **Constraint: obligations are durable** (user ruling). An open obligation closes only through:
  - the user's act on its task (complete or dismiss),
  - a deterministic event: the user's own new outbound message on that thread closes "I owe a reply" (user ruling), mirroring how the contact's response closes "they owe a reply" in the timestamp rule; neither closes a commitment,
  - or a claim the user confirms.
  A later judgment that omits an obligation, or moves its date later, raises a claim.
- **Constraint: one task per open obligation,** keyed by thread and owing party (user ruling), with a lifecycle of its own and a kind chosen by what completion should record. A confirmed closure on one thread never closes an obligation on another.
- **Constraint: effective correspondence state composes the timestamp rule with obligations and confirmed claims.** The contact-wide wait stays as today. A judgment of "nobody owes" on a thread whose outreach started the contact's wait is a removal and stays a claim; confirming it ends the wait.
- **Constraint: indirect cadence effects through existing rules are allowed** (user ruling). Ending a wait lets the deferred cadence task proceed (CAD-042); an accepted merge recomputes cadence as a manual merge does.
- **Constraint: a commitment's timing comes from the commitment when known,** falling back to the cadence's watchdog days, then to 7 days for a contact without a cadence (user ruling). Relative timing resolves against the message's own timestamp, never the run's execution time.
- **Constraint: historical runs simulate the production schedule** and write as production does. Their accepted additions may open live obligations.
- **Constraint: the bench is an all-interactions view** across contacts (user ruling), paginated and filterable, reusing the readiness arc's row and drill-down components.
- **Constraint: the masker is production code** with regression tests that gate merges.
- **Constraint: identity-unlinkable access is an optional layer of a method,** never a substitute for masking.
- **Leaning: nightly cut-offs** by default; the plan or implementation picks the starting interval values.
- **Leaning: reasoned options** ("Yes, because X") as one method variant compared on the scorecard.

## Success criteria

- **SC1** A historical run over January 2026 produces a judgment for every thread at each simulated cut-off where it gained content since the previous one, or reports the thread as incomplete, and the user reviews them on the all-interactions view seeing each thread as of its cut-off.
- **SC2** On the same reviewed thread contexts, the scorecard reports how many of the user's "I owe" outcomes (reply or commitment) each method missed and how many corrections it caused; the user judges whether the correction burden fits an attended session before the next month runs.
- **SC3** After each historical month, the next month's run is fresh evidence: the scorecard shows the improved method against the previous one on that month's reviewed contexts.
- **SC4** A thread where the user said they would do something has one open obligation and one task, not a reply task plus a commitment task.
- **SC5** A thread where the contact committed to something gets a nudge task, timed from the commitment's stated date when the arc built the timing search (Deferred to the arc) and it selected that date; otherwise from the watchdog days, or 7 days for a contact without a cadence.
- **SC6** An accepted commitment stays open when an unrelated message arrives, and a later judgment that omits it raises a claim instead of closing it.
- **SC7** When the user's last message on a thread is "thanks, sounds good!", a "nobody owes" judgment leaves the contact's wait in place as a claim; confirming the claim ends the wait without a cadence skip.
- **SC8** No path lets a judgment close an obligation, end a wait or move a date later without the user's confirmation.
- **SC9** A rerun on an input the user already reviewed changes nothing the user decided.
- **SC10** A message about a contact's new job raises a big-moment flag on that thread, visible on the bench beside the message.
- **SC11** Retired (user ruling, 2026-10-16): no local model is compared, because in production it would run on the Pi, which cannot serve one, and measuring the masking cost needs one model run both masked and unmasked.
- **SC12** A masker regression test fails when a synthetic contact's name, phone number or email survives masking anywhere in a hosted request.
- **SC13** No thread content is found at rest off the Pi after a scorecard run.

## Desired behavior sketch

After a nightly run, a thread where a friend asked "are you free Saturday?" and the user never answered gets a reach-out task, and the thread's drill-down on the bench shows the "I owe a reply" judgment beside the messages it read. When the user replies "Yes! I'll bring the wine", the reply closes the reply obligation at once, and the next run opens a commitment obligation with a reminder task.

When the user ends a thread with "thanks, sounds good!", the timestamp rule still shows the contact as awaiting a reply. The bench shows a "nobody owes" claim; the wait stays until the user confirms the claim, and confirming ends it.

A contact who wrote "I'll send the photos by Friday" gets a nudge task dated Friday; if the photos never arrive, the task is there.

A message about a contact's new job shows a big-moment flag beside that message on the bench.

During the historical runs, the user opens the all-interactions view narrowed to one month's run, filters by contact or interaction type as needed, confirms or corrects each judgment against the content as of its cut-off, and the next month's run reflects the improved jobs.


## Assumptions & deferred questions

- **Assumed:** most thread contexts fit decision-model limits (32K–65K tokens, Clef's 64 questions); real sizes and question counts are measured before the first historical month.
- **Assumed:** production volume is in the hundreds of thread contexts a day, so cost stays in single-digit dollars a month on hosted decision models.
- **Later improvement (user ruling):** one obligation per thread and owing party, covering several commitments, is the first version. Separate obligations per commitment are a candidate for a later version, once a method can tell commitments apart.
- **Assumed:** historical runs starting in January 2026 open few live obligations, rising as the runs approach the present; this is measured before each month advances, not guarded.
- **Deferred to planning:** the predicate names and catalog entries; the confidence threshold; any display thresholds deciding whether and how the bench shows low-confidence claims (the user expects some, in the database or the UI); the shape of the judgment and verdict records within the minimum Goals lists; the obligation lifecycle's name and schema; how claim review controls sit on the bench; whether the all-interactions view lists shared interactions with participant contexts or participation rows; the view's filter set, ordering and route within rabbit hole 4; whether the contact interactions timeline links out to the all-interactions view filtered to that contact (the user is content with linking out) or carries review controls; whether extraction behaviors get their own spec domain; how the runner reads content from the Pi; the input-hash definition; cut-off timezone, boundary inclusion and missed-tick handling; how the masker treats names outside the dictionary.

**Deferred to the arc:**

- **Finding commitment timing.** If a deterministic search for date and timing expressions is simple and light (a few rules, or a dependency already installed), the Pi collects candidate expressions and a choice question picks the one bound to the commitment. If not, use the fallback timing until generative inference arrives in a later arc.
- **Timing of an "I owe" task.** If the commitment states a date or timing and the search above exists, use it. Otherwise the arc picks a default, logs it, and the historical runs judge it.
- **Others' big moments mentioned in passing** ("my sister's getting married"). The user is unsure. Ask it as a separate option in the tree, so the flag can be counted with or without it; the historical runs show whether it is noise.
- **Meeting content in the thread context** (Anarlog sessions, meeting notes). If an existing content reader returns that content, ask the commitment and big-moment questions of it (ball-in-court does not apply to a meeting). Otherwise leave it to a later arc.
- **Which methods to compare first.** Compare every decent option (user ruling, 2026-10-15): each hosted decision model worth comparing, masked. No local model is compared (user ruling, 2026-10-16): in production it would run on the Pi, which cannot serve one. OpenAI Decisions is not compared (user ruling, 2026-10-16): the user's OpenRouter account cannot reach closed-source models. Each method's vendor terms are recorded (HC3) and exclude no method. Proposing the first production method is the arc's; choosing it is the user's (see User-reserved decisions).
- **Interval values for cut-offs.** Nightly unless the historical runs show a shorter interval changes recall.

## Spec-item IDs (pre-allocated)

This spec's own claims carry stable IDs for arc and contract citations: goals `G<n>` (sub-goals `G<n><letter>`), non-goals `NG<n>`, hard constraints `HC<n>` and success criteria `SC<n>`. The behavior rows below cite them in provenance.

Ranges pre-allocated to this spec; planners mint within them only. Rows may land as `status: proposed` ahead of implementation and flip to `current` with citing tests in the implementing PR.

- **CAD-054…CAD-069**: obligations and their tasks, correspondence state composition, claim confirmation ending a wait.
- **KNW-043…KNW-054**: judgment and verdict records, the predicate catalog's review data and its enforcement, claim confirm, reject and correct, verdict precedence.
- **IXN-041…IXN-055**: the all-interactions view, judgments and claims on the bench.

## Engines

```json engines
{ "authorFamily": "claude", "arcPlanner": "claude-opus-5-5:high", "arcReview": "gpt-6.1-sol:high" }
```

Quota is in surplus on both subscriptions, so planners, contract reviews, approval gates and single-implementer builds use no cheaper engine unless it is genuinely the best fit, and prefer Opus 5.5 over Fable. TDD pairs are the exception: their implementer, reviewer and fixer may use cheaper engines, because the reviewed contract, the per-element reviewer and the approval gate check their work.
