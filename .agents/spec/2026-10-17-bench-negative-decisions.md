# Bench negative decisions

## Context & problem

The judgment bench (`/interactions`, designed in `designs/bench.md`) shows each thread judgment as a proposal of four lists: "I owe a reply", "Owes me a reply", Commitments and Big moment. A judgment answers one question per participant and direction (who owes a reply, whether the user committed to them, whether they committed to the user) and one big-moment question per thread. The bench compiles every answer with no effect to nothing: a list with no positive item reads "None", and the decisions behind that "None" are neither shown nor reviewable.

Two defects follow:

- **False "Reviewed".** A proposal whose answers are all negative has no items, so its count of items to review is zero, and its row reads "Reviewed" and its header "All reviewed" before the user has looked at it. The design note's rule ("once all have one") is vacuously true there.
- **Unlabeled negatives.** The scorecard scores only reviewed decisions. A negative the user never acts on is invisible to it. Today a negative becomes a label only when the user corrects it through an Add picker, so the bench can record a false negative but never a true one, and a later method that flips a correct negative into a false positive goes uncounted.

The negative decisions are already stored. Every answer is stored as an outcome with its confidence, and every judgment stores each option's probability for every question it answered. The gap is in what the bench compiles and what a review can record, not in capture.

Re-review is in scope because it is bound to the fix. The bench cannot change a review once made: a reviewed item offers no control, and the Add pickers leave out any decision that has a review. Once negatives can be confirmed in bulk, a confirmed negative could never be corrected, so a miss the user notices later could not be added.

## Terms

- **T1 Decision:** one question answered for one contact (or for the thread, for the big moment) on one judged content, identified as the scorecard identifies it: the judgment example, the question and the contact. Several methods' proposals on the same content hold outcomes of the same decision.
- **T2 Label:** the user's latest review of a decision, from whichever proposal it was made in. A decision has one label, shared by every proposal on the same content.
- **T3 Negative decision:** an outcome of a proposal whose judged value has no effect (`nobody_owes`, `none`, `not_applicable`) and which compiles to no item. An outcome with a no-effect value that compiles to a suggested removal of a live item is an item, not a negative decision. A probability stored for a sub-question that yields no outcome is not a decision.
- **T4 Item:** what the bench already shows as an entry of a list, with its own review controls.

## What matters

Confirmed by the user, in priority order:

1. **Spotting misses.** When the user opens a proposal, the question is "did the model miss anything", not "is each negative right". Each "None" shows the decision the model came closest to making, so one glance answers it.
2. **Correcting a miss with the existing Add.** The correction is inferred from what the user adds. No per-negative control.
3. **Honest review status, at the smallest UI cost.** A row or proposal reads as reviewed only after the user acted on it. Labels for negatives come from one explicit control per proposal or are inferred from the user's other labels, never from a control per list or per negative.
4. **Re-review, label only, except Add.** The user can change any label, a confirmed or inferred negative included. A re-review changes the label and nothing live, with one exception: an Add on a negative decision has the effect an Add on an unlabeled negative has, because a negative has no live effect to undo. Without the exception, whether an Add opens a task would depend on the order of the user's clicks, and a miss found after reviewing the items would never become live.

## Intent & appetite (delegation charter)

**What it is for:** the user's own labeling bench, attended, used in dedicated review sessions over historical runs and opportunistically. One user, real prod data. The labels it records are the scorecard's ground truth (decision-model extraction spec G12) and the Langfuse verdicts.

**What failure costs:** a false "Reviewed" hides threads the user has not looked at, so misses stay unlabeled and the scorecard under-counts. A wrong inferred label costs scorecard accuracy on one decision, and re-review can fix it. A negative that cannot be corrected after a Confirm costs a missed obligation the user knows about but cannot record. A bench that implies a live change that did not happen costs the user's trust in what is live.

**Appetite:** a fix to a shipped feature, not a new one. Expect one or two PRs. The user accepts inferred labels as real labels and stands behind a bulk Confirm without seeing every negative.

**UI footprint is a priority, not a detail.** The user rejected a Confirm per list as too much UI. Each addition below is the whole budget: one line where a list reads "None", the closest decision on a list's heading line where the list has items, one "Confirm Nones" per proposal, "Change" on a reviewed item, and a live-state badge beside a label mark where they disagree.

**Rabbit holes (do not enter):**

1. Re-review that undoes or reverses live state: reopening or closing tasks, clearing flags, moving due dates, re-settling claims. The user ruled re-review label only. An Add's normal effect on a negative decision (G7) is not this, since nothing is undone.
2. Listing negatives one by one, or any per-negative control.
3. Reasons or rationales for decisions. None are stored.
4. Distinguishing inferred labels from clicked ones, on the bench or in the scorecard. The user ruled them the same.
5. Scorecard or Langfuse-export changes beyond what receiving more reviews implies.
6. An undo history or review timeline UI.
7. A permanent reconciliation service for labels; making old proposals satisfy the rules is a one-time concern.

**Call-me-when triggers:**

- Inference or Confirm Nones would write a label anywhere other than the rules in G3 and G4.
- Showing the closest decision needs data the judgment does not store.
- A label and the live state disagree in a case the approved states (canvas artboard 14) do not cover.
- The UI needs anything beyond the footprint budget above.

**User-reserved decisions:** what "Reviewed" means; any new inference rule; any live-state effect of a re-review; copy the user sees on the bench.

**Tradeoff rules for a delegate:** when footprint and explicitness collide, choose footprint (fewer controls) as long as every label still traces to an explicit act of the user or to G4's inference. When a label rule and a display rule collide, keep the label rule and change the display. When showing a label would imply a live state, show the live state.

## Goals

- **G1 The closest decision.** Every list shows, for the decisions not already represented by an item in that list, the one the model came closest to answering with that list's value, named in the same form as the list's items ("Rosa Delgado", "Me → Rosa Delgado", "Big moment"), with that probability as a whole percentage. A list that reads "None" shows it on its None line; a list with items shows it on its heading line, adding no row. The candidates include positive decisions of another list (a contact the model put in "I owe a reply" is a candidate for "Owes me a reply"). A list with no candidate, or whose candidates have no stored probabilities, shows plain "None" or nothing on its heading line. The closest decision is display only and creates no labeling obligation.
- **G2 Probability per value.** The probability of a list's value is the sum of the option probabilities the decision tree maps to that value, as the tree already maps them when it decides. For the big moment, it is the probability that either flag value is chosen.
- **G3 Confirm Nones.** A proposal with no items offers one "Confirm Nones" control. It labels every negative decision of the proposal that has no label from any proposal, each with its judged value.
- **G4 Inference from item labels.** In a proposal with at least one item, once every item has a label, every negative decision of the proposal without a label from any proposal is labeled with its judged value, with no further act.
- **G5 Shared labels.** A decision's label shows wherever the decision appears: an item or negative decision of any proposal on the same content shows the label as its review, and counts as reviewed, whichever proposal the label was made in.
- **G6 Honest counts.** A proposal's header describes that proposal: "{n} to review" while it has items without a label, else "Nones to review" while it has a negative decision without a label, else "All reviewed". A row with several proposals shows "{n} to review" summed over its proposals' items while any has an item without a label, else "Nones to review" while any has a negative decision without a label, else "Reviewed".
- **G7 Re-review.** A reviewed item offers "Change", which restores its review controls; acting on them records a new label for the item's decision and changes nothing live. An Add picker offers every participant or pair not already in that list whose decision is negative, whether or not it has a label; a decision that is positive and has a label is changed only through "Change" on its item. An Add on a negative decision has the effect an Add on the same unlabeled negative has today: live where the proposal acts, a label only where it does not.
- **G8 Live state stays honest.** Where a decision's label and its live state disagree, the item shows its live state badge beside the label's mark, as canvas artboard 14 draws it: a removal that did not take effect is not struck through, a confirmation of an item whose obligation is closed reads "Not acting", and a kept date on a moved obligation shows the obligation's date beside the date the user kept. Strikethrough marks only a removal that took effect.
- **G9 All or nothing.** Confirm Nones, and a review that triggers G4, record all of their labels or none; after either succeeds or fails, a reload shows the same labels and counts.

## Non-Goals

- **NG1** Any change to how judgments are made, which questions are asked, or what is stored about an answer.
- **NG2** Live-state effects of re-review (rabbit hole 1).
- **NG3** A control per list or per negative decision.
- **NG4** Marking inferred labels as inferred anywhere.
- **NG5** Changes to the scorecard's metrics or report.

## Relation to existing & planned work

- **Builds on** the bench of the decision-model extraction arc (`.agents/spec/2026-10-14-decision-model-extraction.md`, G9 and G12; `designs/bench.md`; behaviors IXN-041 to IXN-052 in `spec/interactions.yaml`). It keeps the bench's rule that the server compiles the proposal and the client applies no judgment rule.
- **Aligns the bench with the scorecard.** The scorecard already reads one label per decision across methods; G5 makes the bench read labels the same way.
- **Revises** `designs/bench.md`: "A list with no item reads 'None'" and "An outcome with a no-effect value … produces no item" gain the closest decision; the row indicator's "once all have one" gains G6; the review marks gain G8; "Not designed: Re-reviewing an outcome" is resolved by G7. The canvas "Judgment Bench" holds the approved states as artboards 11 to 14. Its artboard 4, drawn before this spec, reads "All reviewed" over unlabeled Nones; under G4 that state becomes correct, because its one item has a label.
- **Revises** IXN-044's `collapsed-indicator` and the review behaviors that say a reviewed item offers no control and that pickers offer only unreviewed outcomes.
- **Resolves** the open revisit note on re-review in project memory, choosing label only.
- **Serves** the decision-model extraction spec's priority "recall over precision": confirmed negatives let the scorecard count a later method's false positives, and Add after a Confirm keeps misses recordable and live.
- **Must not break** the merge-proposals spec's reuse of the bench (`.agents/spec/2026-10-14-merge-proposals.md`), which assumes the bench's review path; nothing here may assume merge proposals exist.

## Hard constraints

- **HC1** Every label is a review of one decision. Confirm Nones and G4's inference record ordinary reviews, the same as a click records.
- **HC2** No new capture: the closest decision derives from the option probabilities every judgment already stores.
- **HC3** The rules hold for proposals reviewed before this change ships as they do for later ones, and making them hold never overwrites a label that exists.
- **HC4** A first review keeps today's behavior, its live effect included. A review through "Change" is label only. An Add on a negative decision keeps the effect it has today on an unlabeled negative, whether or not the negative has a label.
- **HC5** No PII enters the repository; designs and tests use synthetic people.

## Architectural direction

- **Constraint:** the server compiles the closest decisions, the counts, each control's availability and each item's live-state display into the proposal, as it compiles items today. The client applies no judgment, labeling or inference rule.
- **Constraint:** one rule decides which decisions G3 and G4 label and when, and every path that records a review passes through it, so no path (a bench click, a re-review, a proposal reviewed before this change) can leave a proposal whose items all have labels with an unlabeled negative decision.

## Success criteria

- **SC1** A proposal whose answers are all negative reads "Nones to review" on its header until a label exists for each of its negative decisions; it never reads "All reviewed" before then, and a row holding only such proposals reads "Nones to review", never "Reviewed", before then.
- **SC2** After Confirm Nones, every negative decision of that proposal has a label, the scorecard counts each as a reviewed decision, and the proposal's header reads "All reviewed".
- **SC3** In a proposal with items, giving the last unlabeled item a label labels every unlabeled negative decision of that proposal, with no further act; the same holds for a proposal whose items all had labels before this change.
- **SC4** A decision labeled in one proposal shows that label, and counts as reviewed, in every other proposal on the same content; neither Confirm Nones nor inference ever replaces it.
- **SC5** A list's closest line names the decision with the highest probability of that list's value among the decisions not represented by an item in that list, with that percentage, computed per value as G2 states; a list with items shows it on its heading line, adding no row; a list with no candidate shows plain "None".
- **SC6** After Confirm Nones or inference, adding a person or pair through Add records the addition as the decision's label, everywhere a label is read, and has the same live effect the same Add has on an unlabeled negative.
- **SC7** "Change" on a reviewed item restores its review controls; a new review there replaces the label and changes no task, flag, claim or due date; the item then shows its live state as G8 states.
- **SC8** A row with one proposal awaiting item reviews and another awaiting only negatives reads "{n} to review"; each proposal's header describes only itself.
- **SC9** A failed Confirm Nones, or a failed review that would trigger inference, leaves no partial labels: after a reload, either all of its labels exist or none do.
- **SC10** A proposal never offers Confirm Nones while it has an item; no list offers a control of its own for its negatives.

## Desired behavior sketch

A one-to-one thread where the model found nothing reads "Nones to review" on its row. Expanded, each list reads "None · closest: Rosa Delgado 9%" (or "Me → Rosa Delgado 4%", "Big moment 3%"), with Add beneath as today. The proposal's header shows "Nones to review" and one "Confirm Nones" button. After a click, each None shows "Confirmed", the header reads "All reviewed", the row reads "Reviewed", and every Add stays usable.

A 30-person meeting with nothing proposed looks the same: one line for Commitments ("None · closest: Priya Nair → Me 38%") and one for Big moment, however many decisions sit behind them.

A group thread with two suggested items reads "2 to review". Commitments shows its item and, on its heading line, "closest None: Me → Sam Okafor · 31%". There is no Confirm Nones. After the user confirms both items, every None reads "Confirmed". Each reviewed item offers "Change". The user opens Add on "Owes me a reply" and picks a participant whose None was confirmed; the participant appears as "Live · Added by you", exactly as an Add before the confirmation would have made them.

The user changes a confirmed live item to removed: the item keeps its "Live" badge beside "Removed by you", unstruck, because its task is still open.

## Assumptions & deferred questions

- **A1 Assumed:** a reply absorbed by a commitment in the same direction is a positive decision, not a negative, so neither Confirm Nones nor G4 labels it.
- **A2 Assumed:** some methods may store no option probabilities for a question; G1's fallback covers them, and the planner confirms which, if any.
- **Left to planning:** how G4's single rule is enforced, how existing proposals come to satisfy HC3, and how G9's all-or-nothing boundary is drawn.

## Engines

```json engines
{ "authorFamily": "claude", "arcPlanner": "claude-opus-5-5:high", "arcReview": "gpt-6.1-sol:high" }
```

Quota is in surplus on both subscriptions, so planners, contract reviews, approval gates and single-implementer builds use no cheaper engine unless it is genuinely the best fit, and prefer Opus 5.5 over Fable. TDD pairs are the exception: their implementer, reviewer and fixer may use cheaper engines, because the reviewed contract, the per-element reviewer and the approval gate check their work.
