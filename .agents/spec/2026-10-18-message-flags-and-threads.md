# Message flags, windowing and the Threads tab

## Context & problem

The decision-model methods ask every question of a thread's whole content at every nightly cut-off where it gained content (`.agents/spec/2026-10-14-decision-model-extraction.md`, G3 and G10). For ball-in-court, a question about state, that is right: the same answer over different content is a different judgment. For commitments and big moments it is not. Both are stated in particular messages, yet each night re-asks "has anyone said they would do something not yet done?" and "does the thread reveal a big moment?" of every message again. The answers repeat across nights, so the user would review the same commitment once per night, and a method's later answer about an old message can contradict its earlier one. In the first January scorecard (2026-10-18, 23 reviewed threads), commitment false positives were the most common error; several were commitments made and kept within the same exchange, which by the user's rule are not commitments (T4).

The user's first review session also showed that the bench reviews the wrong unit. The all-interactions view attaches each judgment to one per-contact interaction row, so a row's time, message count and participants describe that interaction rather than the judged thread, a group thread shows one contact, and consecutive nightly judgments of one thread appear as near-duplicate blocks that replay the same history.

Long threads are withheld today when they exceed a method's limits. The user ruled that a long thread is judged over a window of its newest messages instead (decision 20261017T221940Z-b293ad9ce69e), and that windowing belongs with this change (2026-10-18), since both decide what a judgment reads and what it may speak to.

This spec changes what a method is asked about commitments and big moments, adds a flag for claims about contacts, absorbs windowing, and replaces the bench's interaction rows with a Threads tab. It amends the extraction spec's G3b, G3c, G7 (commitment obligations and the big-moment flag) and G9, and its windowing amendment drafted on branch `docs/decision-model-windowing`, which this spec supersedes.

## Terms

- **T1 Window:** the part of a thread context one method reads at one cut-off: the newest whole messages that fit the method's limits, ending at the cut-off. A context that fits whole is its own window.
- **T2 New message:** a message sent after the thread's previous scheduled cut-off and at or before the current one. Each message is new at exactly one cut-off. At a thread's first judged cut-off in a method's history, every message in the window is new, so a thread's earlier history is flagged once (backfill; user ruling, 2026-10-18).
- **T3 Flag:** a method's judgment that one message holds something of one flag kind, with its probability. A flag shows when its probability reaches the flag threshold.
- **T4 Flag kind:** one of three. A **commitment**: a message states that someone will do something for another participant, and the content up to the cut-off does not show it done; a commitment made and kept within the same judged content is not one (user ruling, 2026-10-18). In a meeting note, every note being the user's, a commitment the note reports for anyone counts. A **big moment**: a significant moment in a contact's life (a new job, an illness, a birth, a death, a wedding, a move). A **claim**: a fact about a contact or someone in a contact's life: a preference, biography, contact details or a relationship ("my sister just moved to Denver" is a claim). Information about the user is never flagged as a big moment or a claim; a commitment the user makes is flagged, since the CRM tracks the user's obligations (user ruling, 2026-10-18).
- **T5 Flag decision:** one message and one flag kind. It is the unit a label attaches to, whichever method or cut-off judged it.
- **T5a Clearing decision:** one commitment flag decision at one later cut-off: whether that commitment is done as of that cut-off. It has its own label, separate from the flag decision's.
- **T5b Unjudged message:** a new message no judgment of a method answered, because its cut-off failed or its message lay outside every window. It is neither flagged nor labeled no.
- **T6 Close:** the user finishing a thread's review page with Done or Next (the pager's next control or the right-arrow key) (amends T5 of `.agents/spec/2026-10-17-bench-negative-decisions.md`, where it was collapsing an interaction row). Leaving the page any other way (back, another link, closing the tab) is not a close (user ruling, 2026-10-18).

## What matters

Confirmed by the user, in priority order:

1. **A cheap, targeted foundation for extraction from messages.** Every new message is flagged for commitments, big moments and claims about contacts, so a later model pass reads only the flagged messages (and their neighbors) to extract and categorize what they hold. Flags carry no category: the right categories are not yet known.
2. **Each message is judged once.** Commitment, big-moment and claim flags are asked of new messages only, with the whole window as context. Ball-in-court stays a question about the thread's state at every cut-off.
3. **Windowing.** A judgment reads its window, and its flags, clearings and reply obligations speak only to messages inside it.
4. **Reviewing flags without reviewing every None.** The user reviews flags while the approach is evaluated, on the Threads tab, and never reviews unflagged messages one by one. In normal operation the user reviews no flags, only what the later pass extracts.
5. **Clear, fast review on the Threads tab.** One row per thread, reviewed on its own page, each judgment beneath the messages it read, each message shown once; never waiting before the next click.

## Intent & appetite (delegation charter)

**What it is for:** the extraction foundation behind the CRM's "Act" and "Remember" jobs (`.agents/spec/llm-extraction-program.md`), and the bench the user labels it on. One user, real prod data. Methods run nightly in production and month by month in historical runs; the user reviews historical months in attended sessions while the approach is evaluated, then stops reviewing flags.

**What failure costs:** a missed flag loses a commitment, a moment or a fact for good, since nothing reads that message again; a false flag costs the later pass one cheap read. A bench that misdescribes what was judged wastes the user's review time and corrupts labels. The extraction spec's privacy costs apply unchanged.

**Appetite:** a method change plus one bench surface, delivered in two stages (Architectural direction). January is judged again after stage 1 and reviewed after stage 2, before any later month; its earlier commitment and big-moment labels are not carried over. Labels start fresh for flags. Judgments open no commitment tasks and raise no big-moment flags with live effect; that loss is accepted. The user measured the cost model (below) and accepts asking up to three yes/no questions per new message.

**Rabbit holes (do not enter):**

1. Categorizing flags, or extracting a flag's content (who, what, when). That is the later pass.
2. Creating, closing or reconciling tasks from flags.
3. Flags for dated plans or introductions (deferred by the user).
4. Any UI that lists unflagged messages for review one by one.
5. Adopting Sage's native API (its `tags` multi-label type) or any adapter beyond the compared methods' existing ones.
6. Threshold-tuning machinery. The threshold is a setting; evaluation sets it.
7. Aggregate jobs (drift, closeness, tone).
8. The extraction spec's rabbit holes 1 to 12 still apply, with rabbit hole 4 read for the Threads tab in place of the all-interactions view.

**Call-me-when triggers:**

- A method's measured cost on real thread sizes makes it impractical (Sage documents billing each question's content separately); the user decides before any method is dropped.
- No threshold reaches useful flag recall without flagging most messages.
- Stage 1 would change anything ball-in-court does beyond the windowing rules here.
- The extraction spec's privacy triggers.

**User-reserved decisions:** the flag kinds and their definitions; the Threads tab's design, which is built only against a design the user approved on the Judgment Bench canvas; copy the user sees; what "Reviewed" means; any new labeling rule; the first production method (the extraction spec's reservation).

**Tradeoff rules for a delegate:** when a flag's recall and precision compete, recall wins. When fewer questions per call and recall compete, recall wins unless the first call-me-when trigger fires. When stage boundaries and a shared contract compete, keep the contract stable and move work across stages. The extraction spec's tradeoff rules apply otherwise.

**Unattended running:** the arc may run unattended through the questions this spec defers to the arc, deciding by the guidance given for each and logging every decision with the passage it cites.

## Goals

Stage 1 (the method change):

- **G1 Windowed contexts.** When a thread context exceeds a method's limits, the method reads its window instead of withholding the context. The window is chosen per method from the method's own limits after the thread context is built, so the example (thread, cut-off and input hash) is the same for every method, and it fits the whole request: its text and every flag, clearing and ball-in-court question. When a cut-off's questions exceed the method's question limit, the method makes the fewest further calls over the same window that fit them (user ruling, 2026-10-18). A context that fits whole is sent exactly as before. The window never splits a message; a request that cannot fit even its newest message is incomplete and reported.
- **G2 Message flags.** At each cut-off, every method asks, of each new message in its window, one yes/no question per flag kind, with the whole window as context, and records each answer's probability as a flag decision's judgment. A new message that no judgment answered is recorded as unjudged and reported with the run; rerunning its cut-off judges it.
- **G3 Clearing commitments.** At each cut-off, a method asks, for each of its own earlier commitment judgments above the clearing floor whose message lies inside its window and that it has not cleared, whether the commitment is done as of the cut-off, and records each answer as a clearing decision. The clearing floor is a low probability separate from the display threshold, so lowering the display threshold later never shows a commitment that was never re-checked. A commitment whose message lies before the window is neither cleared nor kept by that judgment.
- **G4 Ball-in-court unchanged but windowed.** Ball-in-court is asked per participant at every cut-off over the window, with today's apply rule and reply obligations. A reply obligation's evidence is the newest message of the thread at the cut-off that opened it; a judgment whose window starts after that message neither keeps nor removes the obligation, and an obligation whose evidence is unknown is preserved.
- **G5 No live effect from flags.** Flags open no obligation or task, raise no big-moment flag on the bench's live state and raise no claim. Commitments no longer absorb reply items on the bench, since they open no obligation. No production method has been designated, so no judgment has opened a commitment obligation or a live big-moment flag, and none exists to carry over.
- **G6 Records.** Each judgment records its window's first message (extraction spec G4), each flag decision's probability, each clearing decision and each unjudged message. Langfuse receives them content-free: message keys travel as opaque candidates, as source-message options do today.
- **G7 Retired questions.** The whole-thread commitment questions, their date and source-message questions, and the whole-thread big-moment question are no longer asked by new method versions. Issue #947 is resolved by G2, since each big-moment flag names its message.

Stage 2 (the Threads tab):

- **G8 Two tabs.** `/interactions` has two tabs. **Interactions** lists interaction rows for correcting frequency data (merge, delete, correct), with no judgment, review indicator or run filter until a judgment about interactions exists. **Threads** lists judged threads (decision 20261018T205040Z-774762e72772).
- **G9 One row per thread, with a Review switch.** The Threads tab is both the user's regular view of their threads and the review bench. A Review switch on the list and on each thread page shows or hides everything about judgments, and stays as the user set it. A thread row shows the thread's venue, its title where it has one, every participant contact and the time of its latest message; with Review on, it adds the number of the thread's messages judged in the chosen run and its review status. The list is ordered by latest message, newest first, with Review on or off; the switch never reorders it. The Type filter (All, Messages, Email, Events: the interaction types the Interactions tab filters by, from one source-to-type mapping, Messages covering every chat app, Google Chat included, and Events covering calendar meetings and anarlog sessions) and contact filter narrow it (decision 20261019T205343Z-afc9197af058); with Review on, the run filter does too. There is no method filter, since a run fixes its method. Opening a row opens the thread's page, which has its own URL (user rulings, 2026-10-19).
- **G10 The thread page.** The page uses the contact page's navigation bar (Back to list; a pager reading "{n} of {total}" over the list as filtered; the left- and right-arrow keys), and a header naming the thread, its venue and its participants; the bar and header stay at the top. With Review off, the page shows the thread's messages in order. With Review on, the header adds the run and the review status and offers Done, and the page shows the thread as one timeline, each message once: each judged night begins with a subtle divider naming the night and its count of new messages, its new messages carry their flags, and its judgment closes it in a tinted box that states which messages it read ("read {first message's time} to {cut-off}", the first time linking to that message) and holds the ball-in-court proposal (the "I owe a reply" and "Owes me a reply" lists, as today). Messages after the run's last night sit behind a collapsed toggle (user rulings, 2026-10-19).
- **G11 Reviewing flags.** A flag shows on its message as a chip naming its kind and probability, with confirm and remove controls and Change after review. Any new message offers a control to add a flag of any kind. A close labels "no" every unflagged flag decision without a label among the new messages of each frame that holds a label (the close rule of the negative-decisions spec, applied per flag decision and per frame; user ruling, 2026-10-18); a label on a reply item or a clearing decision counts as the frame's label (user ruling, 2026-10-19). Done and Next close the thread; Back to list and Previous do not. A shown flag without a label stays to review, and a close never labels a flag decision that any compared method's judgment (G16) shows as an unreviewed flag, whichever run or filter shows it; stored judgments of removed methods never hold a close back (decision 20261019T165236Z-24f37dd9e7bd). A frame with no shown flag and no label offers Confirm Nones. A frame lists each clearing decision its judgment made, citing the commitment's message without repeating it ("Commitment from Jan 3 · judged done"), with confirm and remove controls. Once a later frame's clearing decision judges a commitment done, the commitment's chip says so ("cleared Jan 4"). Each frame shows, per flag kind, the unflagged message the method came closest to flagging, display only, linking to that message. A thread is Reviewed when every frame holds at least one of the user's labels (user ruling, 2026-10-19). Otherwise a thread showing any flag, clearing decision or reply item reads "{n} to review", n counting its shown items without a label plus each frame that shows nothing and awaits Confirm Nones; a thread showing nothing at all reads "Nothing flagged" (decision 20261019T165456Z-e2d905494b76).
- **G12 Reuse, not new infrastructure.** The Threads tab reuses the existing review writer, controls, Change and fresh state after saves. Nothing is built whose only consumer is a future judgment about interactions.
- **G13 Flag scorecard.** The scorecard reports, per method and flag kind, precision and recall over labeled flag decisions, clearing accuracy over labeled clearing decisions, and how many new messages each method judged, left unjudged and flagged. An unjudged message is never counted as a no. Recall is recall against the user's labels, inferred labels included. Ball-in-court scoring continues as today.
- **G14 Langfuse metrics.** The export sends each scorecard metric as its own numeric score on the run's trace, content-free, so methods and versions can be charted.
- **G15 Overflowing nights in chunks.** When a thread's new messages at a cut-off do not fit one window of a method, the method judges them in the fewest chunks that fit, in order from earliest to latest. Each chunk's window is its new messages preceded by as many older messages as fit, so a later chunk reads the earlier chunks' messages as context; the new messages are spread evenly over the chunks. Each new message is flagged once, in its chunk; each chunk re-checks the method's earlier commitments inside its window, those an earlier chunk of the same night flagged included; ball-in-court is asked once, in the last chunk (decision 20261019T142354Z-73bbb941ef65).
- **G16 Compared methods.** The compared methods are clef-masked, clef-masked-reasoned and jev-masked. sage-masked and clef-flash-masked are removed from the methods a run or the scorecard can use; their stored judgments stay (decisions 20261019T135325Z-b40e61f4e0d5 and 20261019T142002Z-781d8c72e07a).
- **G17 Review runs only.** The run filter lists the historical and scheduled runs of compared methods, never the scorecard's reruns or a removed method's runs. With Review on and no run chosen, it holds the newest such run.

## Non-Goals

- **NG1** Extracting, categorizing or acting on a flag's content.
- **NG2** Flags for dated plans or introductions.
- **NG3** Reviewing unflagged messages one by one.
- **NG4** Carrying January's commitment or big-moment labels onto flag decisions.
- **NG5** Merge-proposal UI on the Interactions tab.
- **NG6** Earlier history behind a collapsed toggle above a thread's first shown frame (nice to have; first to cut).

## Relation to existing & planned work

- **Amends** `.agents/spec/2026-10-14-decision-model-extraction.md`: G3b and G3c (replaced by G2 and G3 here), G7a–G7d for commitments and G7f (replaced by G5), G9 (replaced by G8–G12), G11, HC10 and HC11 (windowing and extra calls as in G1), its Architectural direction's "only a current judgment opens an obligation", which now concerns reply obligations only, and SC2's commitment half. It retires that spec's SC4, SC5, SC6 and SC10, which describe commitment obligations and live big-moment flags. Its other goals, constraints, privacy rules and apply rule stand.
- **Supersedes** the uncommitted windowing amendment on branch `docs/decision-model-windowing` (G16, SC14–SC16 and the window-evidence constraint), whose content this spec carries.
- **Builds on** `.agents/spec/2026-10-17-bench-negative-decisions.md` (labels shared per decision, Confirm Nones, the close rule, fresh state after saves, delivered in #945 and #948).
- **Resolves** issue #947.
- **Enables** the later extraction pass over flagged messages and the merge-proposals work (`.agents/spec/2026-10-14-merge-proposals.md`), whose proposals appear on the Interactions tab.
- **Recorded rulings:** 20261018T223146Z-2d7bed0b2741 (message-relative commitments and big moments), 20261018T224232Z-6a774b7a1d8f (sequencing), 20261018T205040Z-774762e72772 (two tabs), 20261018T205038Z-5e8772eb1fb1 (close rule), 20261018T233108Z-9e3dd5183000 (Jev's terms acceptable), 20261019T162753Z-305f42dc685c (Threads design), 20261019T142354Z-73bbb941ef65 (overflow chunks), 20261019T135325Z-b40e61f4e0d5 and 20261019T142002Z-781d8c72e07a (methods dropped).

## Prior art & external constraints

- **Question types.** No compared vendor's System One format has a multi-select question: Venice and TypeSafe offer `noul`, `choice` and `score`; OpenRouter adds an image `box`; a `choice` returns one option whose probabilities sum to 1 (docs.typesafe.ai `/primitives/choice`; `api.venice.ai/doc/api/swagger.yaml`). TypeSafe and OpenRouter document one `noul` per independent label as the multi-label pattern (docs.typesafe.ai `/primitives/noul`; openrouter.ai cookbook "jev-classification"). Sage's native `tags` type is multi-label but exists only outside the System One format (docs.levanto.ai `systemone.md`), and Sage bills each question's content separately (docs.levanto.ai `decision-model/batch.md`).
- **Measured cost (spike, 2026-10-18).** One synthetic 40-message thread sent to Jev through Venice with 1, 3, 40 and 120 yes/no questions billed 922, 1,014, 2,481 and 5,943 input tokens, with latency flat at under a second. The thread is billed once per call; each question adds its own text, about 42 tokens. On the same call the flags were plausible for commitments and claims and loose for big moments.

## Hard constraints

- **HC1** Content leaves once per thread context per method per cut-off, every flag, clearing and ball-in-court question riding in that call, except when the questions exceed the method's question limit, when the fewest further calls over the same window carry the rest (G1).
- **HC2** No flag, clearing or judgment changes live state other than ball-in-court's reply obligations under the existing apply rule.
- **HC3** No content at rest off the Pi; Langfuse records stay content-free.
- **HC4** A judgment never reads a review: a method's inputs are the window and its own earlier judgments, never the user's labels, so methods stay comparable.
- **HC5** No PII in the repository; designs and tests use synthetic people.

## Architectural direction

- **AD1 Constraint: two stages.** Stage 1 delivers G1–G7; stage 2 delivers G8–G17. Each stage is its own arc and lands on its own, so January can be judged again on stage 1 while stage 2 is designed and built. Stage 1's records are the contract stage 2 reads, so stage 1 fixes the flag decision and clearing records.
- **AD2 Constraint: a flag decision is identified by its message and kind,** not by a thread and cut-off, so a label applies to every method's judgment of that message (T5).
- **AD3 Constraint: the display threshold and the clearing floor are data,** stored with the predicate catalog or the method's settings, recall-leaning; every probability is stored, so the display threshold can change without re-running.
- **AD4 Constraint: replay is chronological.** Because clearing reads a method's own earlier judgments, any rerun of a method, the scorecard's included, judges a run's cut-offs in order from its start; a judgment is reused only when what it read is the same: its window, its new messages and the earlier judgments it re-checked.
- **AD5 Leaning: one yes/no per flag kind per new message,** following the measured cost model; the planner may propose another shape only with a measurement on real thread sizes.

## Success criteria

- **SC1** A thread whose context exceeds a method's limits at a cut-off is judged over its window, not reported incomplete, and its judgment records where the window began.
- **SC2** Every method's judgment at a cut-off holds one flag decision per flag kind for each new message in its window, and none for a message that was new at an earlier cut-off.
- **SC3** A commitment stated and done within one cut-off's new messages is not flagged; a commitment flagged at one cut-off and done before a later cut-off whose window still holds its message is cleared at that later cut-off.
- **SC4** No flag or clearing opens, closes or changes a task, an obligation or a live flag; ball-in-court's reply obligations behave as today within the window rules.
- **SC5** On the Threads tab, a busy group thread judged on many nights is one row listing every participant, and its review page shows each message once with each judgment's flags and reply lists beneath the messages it read.
- **SC6** The user labels a thread's flags by confirming or removing the shown chips and adding any missed flag; one Done or Next labels every other new message of each reviewed frame, and the user never acts on an unflagged message to label it.
- **SC7** The scorecard reports flag precision and recall per method and kind over labeled flag decisions, and Langfuse charts each metric per run.
- **SC8** Each compared method's cost is measured on a sample of real thread sizes in stage 1, and the user decides on any method whose billing grows by the window per question.
- **SC9** In a January review session, no review click waits on the server before the user's next click.
- **SC10** A cut-off that failed, or a new message outside every window, shows as unjudged in the run's report and the scorecard, never as a no.
- **SC11** Re-judging January leaves no new message unjudged for lying before its window: a night whose new messages overflow a window is judged in chunks (G15).
- **SC12** With Review off, the Threads tab and a thread page show no judgment, flag or review control, and the list's order is the same as with Review on.

## Desired behavior sketch

The user opens Threads, narrowed to the new January run. A family group chat judged on twelve nights is one row, titled with the group's name, listing its four participant contacts, "3 to review". The user opens it, and its review page shows the messages in order. After Jan 3's messages, a frame: "Can you send me the recipe?" from Priya carries no chip; "I'll send it tonight" from the user carries a commitment chip at 91%; "Ana starts at the hospital Monday!" carries a big moment chip and a claim chip. Beneath, the reply lists read "I owe a reply: None · closest: Priya 22%" and "Owes me a reply: None". The user confirms the commitment, confirms the claim, removes the big moment, and adds a claim chip to "we finally adopted a dog" which the method missed. Jan 4's frame follows, with only Jan 4's messages; the commitment's message is still in its window and the method judged it done, so the chip reads cleared there. The user presses Next: every unflagged new message in the reviewed frames is labeled as holding nothing, the thread's row reads "Reviewed", and the next thread in the queue opens.

## Assumptions & deferred questions

- **A1 Assumed:** meeting notes are flagged like messages (T4); ball-in-court is not asked of meetings, as today.
- **A2 Assumed:** each method re-checks only its own earlier commitment flags (G3), so a method's judgments never depend on another method or on labels (HC4).
- **Left to planning:** the records' shape and how the bench compiles flags; how the close rule is applied per flag decision; how the list is paged.
- **Design:** the user approved the stage-2 design on the Judgment Bench canvas on 2026-10-19; `designs/threads.md` writes it up, and stage 2 builds from that note.

**Deferred to the arc:**

- **D1 Sizing the window.** Size it from tokens and questions together (G1). Each vendor's billed tokens differ from the byte count (the January runs show it overestimating Clef's and Jev's by about 2.2×, and Sage billing above it on large contexts). Measure each method's ratio from earlier runs' billed tokens and keep a margin; if a method's ratio is unstable, size its window from its own reported usage.
- **D2 Single messages over every limit.** One January email exceeded every compared method's limit alone. If such messages are rare, leave the context incomplete and reported; if common, raise it to the user as a call-me-when.
- **D3 The flag threshold.** Start recall-leaning and set it from the first labeled month: lower it while precision stays usable for the later pass; if no threshold works, it is a call-me-when.
- **D4 Wording.** Tune each flag kind's wording against labels, starting with big moments, which the spike showed loose; a wording change that leaves no labeled example worse may be adopted and logged (extraction spec's reservation).

## Engines

```json engines
{ "authorFamily": "claude", "arcPlanner": "claude-opus-5-5:high", "arcReview": "gpt-6.1-sol:high" }
```

Quota is in surplus on both subscriptions, so planners, contract reviews, approval gates and single-implementer builds use no cheaper engine unless it is genuinely the best fit, and prefer Opus 5.5 over Fable. TDD pairs are the exception: their implementer, reviewer and fixer may use cheaper engines, because the reviewed contract, the per-element reviewer and the approval gate check their work.
