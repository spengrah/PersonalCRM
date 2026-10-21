# Threads review fixes

Fix three problems the user hit while reviewing the January runs on the Threads tab (2026-10-21): a commitment re-check whose review controls do not say which answer they record, re-checks shown for commitments the user already rejected, and reply lists whose Add picker leaves out a participant once that participant's ball-in-court decision has a label. This spec amends `.agents/spec/2026-10-18-message-flags-and-threads.md` (G3, G11, G12, SC7) and `.agents/spec/2026-10-17-bench-negative-decisions.md` (G7); everything those specs say that this one does not change still holds.

## Context & problem

**Re-checks.** A night's judgment box lists, under "Commitments re-checked", each clearing decision its judgment made: the method's answer to whether an earlier commitment is done as of the cut-off. Each line reads "judged done" or "judged not done" and offers a ✓ that labels the decision with the method's answer and an ✗ that labels it with the opposite, then marks it "Confirmed" or "Removed". The user cannot tell from the controls which answer each one records. The confusion is worst when the user already removed the commitment's chip: a night shows a re-check whenever the commitment's probability reaches the display threshold (`commitmentShows` in `backend/internal/judgment/review_state.go`), whatever the user's label on the commitment, so the user is asked whether a non-commitment is done. The scorecard's clearing accuracy then counts any label the user gave such a re-check, which is noise.

**Add picker.** Ball-in-court is one decision per participant, with the values "I owe a reply", "they owe a reply" and "nobody owes". The reply lists' Add pickers offer a participant only while the participant's decision has no label or is negative (`additions` in `backend/internal/judgment/proposal.go`). Once the user removes or confirms a participant's "I owe" item, the "Owes me" picker no longer offers that participant, so a judgment that is the inverse of the right one cannot be corrected. In a one-to-one thread the participant is the only choice, so the picker offers nothing and the empty list shows no Add button.

## Terms

- **Rejected commitment:** a commitment whose message holds a commitment label saying it holds no commitment, whichever route wrote the label (the chip removed, a close or Confirm Nones).
- **Clearing decision, re-check:** as in the message-flags spec (T5a): a method's answer to whether an earlier commitment is done as of a cut-off.

## What matters

1. The user can tell what each review control records before pressing it.
2. The user is never asked to review a re-check of a commitment they rejected, and such a re-check never counts in the scorecard.
3. Any wrong reply-list judgment can be corrected on the box, including one that is the inverse of the right one.

## Intent & appetite (delegation charter)

**What it is for:** the user's January review sessions on the Threads tab, which produce the labels the scorecard compares methods on. The parent spec's charter (`.agents/spec/2026-10-18-message-flags-and-threads.md`, Intent & appetite) applies unchanged except where this section says otherwise.

**What failure costs:** confusing controls corrupt labels, and labels are the eval material. A reply-list error the user cannot correct stays as a wrong label in the scorecard.

**Appetite:** one PR. Small changes to the review state, the scorecard's clearing count, the Add picker rule and the clearing line's controls, with their behavior items and tests.

**Rabbit holes (do not enter):**

1. Changing what methods ask, the clearing floor or the display threshold.
2. Migrating, deleting or rewriting stored labels. A label on a hidden re-check stays stored and shows again if the user later labels the commitment held.
3. Redesigning the judgment box or the reply lists beyond the controls named here.
4. Changing the live effect of any review. A review keeps the live effect the existing rules give it.

**Call-me-when triggers:**

- A fix would change what methods ask or the stored labels.
- G4's rule would let a single participant show in both reply lists of one judgment.

**User-reserved decisions:** copy the user sees beyond the copy this spec fixes; any further labeling rule. The user approved this spec's three fixes on 2026-10-21, including the Done and Not done controls of G3.

**Tradeoff rules for a delegate:** prefer the rule that removes a special case over one that adds a guard. Where the parent spec's tradeoff rules apply, they still apply.

**Unattended running:** the arc may run unattended, deciding questions by citing this spec or the parent spec and logging each decision with the passage it cites.

## Goals

- **G1 Re-checks of rejected commitments are hidden.** A night carries a clearing decision only when its commitment shows as a chip under the existing rule (its probability reaches the display threshold, or the user labeled it held) and the user has not rejected the commitment. A hidden re-check counts in no to-review count and no thread status, and does not stop a night from offering Confirm Nones. Its stored label, if any, is kept.
- **G2 The scorecard skips re-checks of rejected commitments.** Clearing accuracy, in `crm-admin --scorecard` and in the Langfuse export that reads the same metrics, counts a labeled clearing decision only when the user has not rejected its commitment.
- **G3 Re-check controls state the answer.** Each re-check line keeps the method's answer and probability, and offers two controls, "Done" and "Not done", each labeling the decision with that answer. A labeled re-check shows the answer the user gave ("Marked done" or "Marked not done") with Change, and Change offers the same two controls.
- **G4 The Add picker offers every participant a list does not hold.** Each reply list's Add picker offers every participant of the judgment whose ball-in-court decision the list holds no item for, whatever the decision's label. An Add on a participant whose decision has a label relabels the decision with the list's value, so the participant's item moves to that list, marked "Added by you", and the list the item left no longer holds it. Its effect on live state is the effect the existing rules give a review of a decision in that label state.

## Non-Goals

- **NG1** No change to the commitment chips, flag labels, close rule or Confirm Nones beyond G1.
- **NG2** No change to the reply lists' items, marks, states or Change controls beyond G4.

## Relation to existing & planned work

- Amends the message-flags spec's G3 and G11 (what a night shows), SC7 (clearing accuracy) and G12 (the reply lists' pickers), and the negative-decisions spec's G7 (which decisions the pickers offer).
- The behavior items that state today's rules are `spec/interactions.yaml` IXN-059 (`offers-negatives`), IXN-082 (`confirm-remove`), IXN-086 (`add-picker-negatives`) and IXN-089 (its clearing keys), and the scorecard's clearing item; the PR updates each under the spec maintenance rule (`spec/README.md`).
- PR #959 added the display filter G1 extends (`commitmentShows`).

## Hard constraints

- **HC1** No live state changes that the existing rules do not already make for the same review.
- **HC2** No content at rest off the Pi; the scorecard and the export stay content-free.

## Success criteria

- **SC1** A night whose judgment re-checked a commitment the user rejected shows no re-check for it, and the thread's to-review count does not include it; labeling the commitment held again shows the re-check with any label it had.
- **SC2** The scorecard's clearing accuracy over a run where the user labeled a re-check of a rejected commitment excludes that re-check.
- **SC3** On a re-check line, "Done" labels the decision done and "Not done" labels it not done, whatever the method answered, and the line then shows the user's answer.
- **SC4** In a one-to-one thread where the user removed the participant's "I owe" item, the "Owes me" list offers Add with that participant, and adding them moves the participant's item to "Owes me", marked "Added by you".
- **SC5** In a one-to-one thread where the user confirmed the participant's "I owe" item, the "Owes me" list offers Add with that participant.

## Assumptions & deferred questions

- **A1 Assumed:** a participant shows in at most one reply list of a judgment, since ball-in-court is one decision per participant; G4 keeps that true.

**Deferred to the arc:** none.

## Engines

```json engines
{ "authorFamily": "claude", "arcPlanner": "claude-opus-5-5:high", "arcReview": "gpt-6.1-sol:high" }
```

The parent spec's engine guidance applies: quota is in surplus on both subscriptions, so planners, contract reviews, approval gates and single-implementer builds use no cheaper engine unless it is the best fit, and prefer Opus 5.5 over Fable. TDD pairs' implementer, reviewer and fixer may use cheaper engines.
