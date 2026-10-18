# Judgment bench: the all-interactions view

The bench is one new page, the all-interactions view at `/interactions`, where the user reviews the judgments decision models make about threads. It lists every interaction across contacts, one full-width row each, narrows to one judgment run, and shows each thread judgment once, as a proposal organized by answers, directly beneath the content it read. The list never enumerates judgments: a row carries one review indicator, and the proposal appears only in the row's drill-down. The user corrects the proposal inline. PR5 builds it from this note and from the proposed behaviors IXN-041…IXN-050 in `spec/interactions.yaml`, without opening the canvas.

The canvas is "Judgment Bench" (https://claude.ai/artifact/1d1TcFH9iXtdtRABK6n4iX), a Design canvas of fourteen artboards. Every artboard after the two lists shows one thread. Where an artboard shows several states of one proposal, each state is labeled as a step of that proposal ("Step 1 of 3 · The proposal as judged"). Every person, message and date on it is synthetic.

The user approved this design on 2026-10-16, with the rulings on the four options from the reference artifacts recorded below (see "Options from the reference artifacts"). PR5 builds against this note. Artboards 11 to 14 draw the approved states of `.agents/spec/2026-10-17-bench-negative-decisions.md`: the closest decision behind each None, honest review counts, Confirm Nones, Change and the live state beside a mark. Artboard 4, drawn before that spec, reads "All reviewed" over Nones without a label; that state is correct, because its one item has a label and the bench then labels the proposal's Nones (see "Nones").

Sources: `.agents/spec/2026-10-14-decision-model-extraction.md` (G3, G5, G7f, G8, G9, G11, SC1, SC4, SC5, SC7, SC10, NG3, NG4, rabbit hole 4), the bench routes of the decision-model foundation plan (S4): `GET /api/v1/judgment-runs`, `GET /api/v1/judgments` and `POST /api/v1/judgment-outcomes/:id/review`, PR5's `GET /api/v1/interactions`, and the bench read PR5 adds, which returns each judged thread's proposal already compiled, each commitment with its source message when the judgment holds one. Wire fields are snake_case (`right_outcome`, `cut_off`, `run_id`, `contact_id`).

## The proposal

Each thread judgment is shown as one proposal of four lists in three sections, stacked top to bottom: the replies section, holding "I owe a reply" and "Owes me a reply" side by side; Commitments; and Big moment. The questions sent to the decision model never appear.

| List | Holds | Item text |
|---|---|---|
| I owe a reply | each participant contact the user owes a reply | the contact's name |
| Owes me a reply | each participant contact who owes the user a reply | the contact's name |
| Commitments | every open commitment, the user's own and the participants', in one list | "{debtor} → {creditor}", with "due {date}" or "no stated date", and its source message beneath |
| Big moment | the thread's big-moment flag, at most one | "Big moment" |

The user is "Me" in a commitment's text. A reply list holds at most one person in a one-to-one thread and possibly several in a group thread. A meeting context's proposal has no reply lists, since ball-in-court is not asked of meetings. A list with no item reads "None", beside the decision the judgment came closest to making (see "Nones"). Big moments stay a flag on the thread, attached to no contact, until generative inference can attach them to contacts. The flag reads "Big moment" whether the judgment found a participant's own moment (`participant_moment`) or another person's moment mentioned in passing (`other_person_moment`): the distinction stays in the data and appears in no label.

The server compiles the question tree into these lists, so the client applies no judgment rule. In particular, a party's commitment absorbs that party's reply item in the same direction: when the user committed to Sam, the proposal shows "Me → Sam" and no "I owe Sam a reply" item, since the two are one obligation (G3b, SC4). An outcome with a no-effect value (`nobody_owes`, `not_applicable`, `none`) produces no item, unless it is a suggested change to a live item. It is a negative decision: its list's None stands for it, and it can be the list's closest decision.

Each item stands for exactly one judged outcome, and each user act on an item reviews that one outcome.

### A commitment's source message

Each commitment carries the message that states it. The judgment names that message by answering a choice question whose options are the thread's messages, so no text is generated (NG3). Under the commitment's text the item quotes the message: its sender, its time, the message text in quotation marks, and a "Show in thread ↑" link that scrolls the drill-down to that message and highlights it. A commitment whose judgment holds no source-message answer shows no quote, and neither does a commitment the user added.

### A commitment's date

A commitment reads "due {date}" when the judgment selected a stated date, and "no stated date" otherwise. A commitment the user adds reads "no stated date" and takes the timing of any commitment without a stated date: the contact's cadence watchdog days, else 7 days. The bench shows the item's stated date only, never the date of its task.

### Nones

A list's None is every negative decision behind it, and the bench shows them as one line, however many there are. The server names each list's closest decision: among the decisions no item of the list represents, the one the judgment gave the highest probability of the list's value, which may be a decision another list holds as an item. The closest decision is display only and asks for no review.

- A list with no item reads "None", "closest: {name}" in gray-500 and "{n}%", on one line in a white box, the name in the form of the list's items ("Rosa Delgado", "Me → Rosa Delgado", "Rosa Delgado → Me", "Big moment"). The box's border is dashed gray-300 while a negative decision behind the list has no label, and solid gray-200 once all have one.
- A list with items shows "closest None: {name} · {n}%" in gray-500 at the right of its heading line, adding no row.
- A list with no closest decision reads plain "None", or shows nothing on its heading line.
- Once every negative decision behind a list has a label, its None line or heading line adds "Confirmed", green-800 with a check.

A proposal with no items whose negative decisions have no label offers one "Confirm Nones" button, an outline button with a green-800 check, at the right of its header beside "Nones to review". It labels every negative decision of the proposal that has no label with its judged value; then each None reads "Confirmed", the header "All reviewed" and the row "Reviewed", and every Add stays usable. In a proposal with items, the review that gives its last item a label labels its Nones the same way, with no further act. No list offers a control of its own for its Nones. If Confirm Nones fails, the proposal stays as it was and shows "Couldn't save. Try again." under its header.

A decision has one label, shared by every proposal on the same content: a review made in one proposal shows on the same decision's item in every other, and counts as reviewed there.

### Item states

Each item shows its state on the item itself, where the decision sits.

| State | Meaning | Look |
|---|---|---|
| Live | Acting: its obligation is open and its task created, or its flag raised | blue-50 fill, blue-200 border, "Live" in blue-700 |
| Suggested | Waiting on the user's OK: an addition below the confidence threshold | amber-50 fill, dashed amber-700 border, "Suggested" in amber-800 |
| Not acting | Evidence: reviewable as an eval label, changing nothing live. Every judgment of a non-production method is evidence, and so is every judgment made while no production method is designated | gray-50 fill, gray-300 border, "Not acting" in gray-600 |
| Live with a suggested change | A live item the judgment would remove or move to a later date | the Live look, with an attached amber dashed strip "Suggested: remove" or "Suggested: due {later date}" |

Each item shows its outcome's confidence as a whole percentage. A live item with a suggested change shows the suggestion's confidence on its strip, and a live item from an earlier state, such as the timestamp rule's wait, shows none of its own.

### Review marks

After a review an item stays in place, never hidden, and its controls give way to a mark and a "Change" text button in blue-600 (see "Changing a review"):

- "Confirmed", green-800 with a check, when the user confirmed the item or kept a live item against a suggested change.
- "Kept {date} by you", green-800 with a check, in place of "Confirmed" on a commitment whose label kept a date its obligation no longer holds; the item's text then reads the obligation's live date.
- "Removed by you", when the user removed the item or accepted a suggested removal. Where the removal took effect, the item's text is struck through in gray-500 and its state label is gone. Strikethrough marks nothing else.
- "Added by you", green-800 with a plus, on an item the user added.

Every item but a removal that took effect keeps its live state beside its mark: a confirmed suggestion of the production method reads Live and Confirmed, and a confirmed item of a non-production method reads Not acting and Confirmed. Where the label and the live state disagree, the live state shows, as artboard 14 draws: a removal whose obligation stays open reads Live and Removed by you, unstruck; a confirmation of an item whose obligation is closed reads Not acting and Confirmed. The verdicts the store derives (`confirmed`, `rejected`, `corrected`) are eval data and never appear as words on the bench.

### User acts and the reviews they record

Every act is inline, on the item or at the foot of its list, with no modal and no form. Each act sends one `POST /api/v1/judgment-outcomes/:id/review` with a `right_outcome`; the client never sends a verdict.

| Act | Control | Outcome reviewed | `right_outcome` |
|---|---|---|---|
| Confirm an item | check button on the item | the item's outcome | the judged outcome |
| Remove a reply item | × button on the item | that contact's `ball_in_court` | `nobody_owes` |
| Add a person to "I owe a reply" | "Add", then the person's chip | that contact's `ball_in_court` | `i_owe_reply` |
| Add a person to "Owes me a reply" | "Add", then the person's chip | that contact's `ball_in_court` | `they_owe_reply` |
| Remove a commitment | × button on the item | that contact's `commitment_mine` or `commitment_theirs` | `none` |
| Add a commitment | "Add commitment", then a "{debtor} → {creditor}" chip | `commitment_mine` for "Me → X", `commitment_theirs` for "X → Me" | `open` |
| Remove the flag | × button on the item | the thread's `big_moment` | `none` |
| Add the flag | "Add" | the thread's `big_moment` | `participant_moment` |
| Accept a suggested change | "Accept" on the strip | the item's outcome | the judged outcome |
| Keep a live item against a suggested removal | "Keep" on the strip | the item's outcome | the live value, such as `they_owe_reply` for a wait |
| Keep a commitment's date against a suggested later date | "Keep original" on the strip | the item's outcome | `open`, at the obligation's current date (see "Not designed") |

An item's controls are a check button and a × button, both 32 px icon buttons with labels for assistive technology ("Confirm Sam Okafor", "Remove Sam Okafor"; on the flag, "Confirm big moment" and "Remove big moment"). A live item with a suggested change shows "Accept" and "Keep" on a removal's strip, or "Accept" and "Keep original" on a later date's strip, instead. "Keep original" leaves the obligation's date as it was. Adding a person to one reply list moves the person out of the other, since both lists answer the same question.

The reply lists' "Add" and "Add commitment" open an inline picker under the list: one chip per choice and a close button. The flag's "Add" records the review at once, with no picker: the flag carries one label, so the bench offers one choice, and an added flag records `participant_moment`, the big moment G3c defines. The reply lists' picker offers each participant contact not already in that list whose `ball_in_court` decision has no label or is negative, labeled or not. The commitments picker offers each "Me → X" and "X → Me" pair that is not already an item and whose commitment decision has no label or is negative. A positive decision with a label changes only through "Change" on its item. Choosing a chip records the review and closes the picker. Only one picker is open at a time. A list whose picker would offer nothing shows no Add control, and the flag's "Add" shows only while the thread has no flag and its `big_moment` decision has no label or is negative. The server compiles the pickers' choices, as it compiles every list.

Removing a commitment that absorbed a reply item reviews only the commitment, so the reply item can appear on the next read.

### Changing a review

"Change" on a reviewed item hides its mark and restores its review controls, with a "Cancel" text button beside them: the check and × buttons, or "Accept" and "Keep" or "Keep original" for an item that had a suggested change. A review there replaces the decision's label and changes nothing live: no task opens, closes or moves, no flag is raised or withdrawn, and no claim is settled again. The item then shows its new mark and its live state as "Review marks" describes, and offers "Change" again. An Add on a negative decision is not a change of review: it has the effect the same Add has on a None without a label, live where the proposal acts, whether or not its None was confirmed.

A review shows on its item at once, without a reload. If the request fails, the item keeps its controls and shows "Couldn't save. Try again." beneath them.

## Styling

The bench uses the app's shipped styling and components: the Tailwind gray and blue palette, Geist, the white card with a light shadow, the shipped row's parts (direction glyph, source badge, label, expand and actions buttons) and `TimelineDrillDown`, the shipped `Button` variants, native selects as on the contact history's venue filter, and the shipped `Pagination`. It also takes the two styles the user accepted from the reference artifacts: the type filter as a segmented control, and the user's own sender name, "Me", in blue-700 in the drill-down (O4 and O2). It adds the item states above, the row's review indicator, and the big-moment flag in pink (pink-50 fill, pink-300 border, pink-800 text, with a flag icon). The states differ in lightness and border style as well as hue.

The page sits in the app's list-page container, `max-w-7xl mx-auto py-6 px-4 sm:px-6 lg:px-8`, as the dashboard and the main navigation do, and the canvas draws it at that width: 1280 px, 1216 px of content. Rows and drill-downs span the whole card. The inbound (←), outbound (→) and mutual (↔) direction glyphs lead each row as the shipped row draws them.

The bench carries no explanatory copy. Artboard 10 records the user's ruling on each style the reference artifacts carry that the app has not shipped, and artboards 1–9 draw the accepted ones (see "Options from the reference artifacts").

## The list row

Each row is one line of a grid across the card's full width, in this order:

1. Direction and source: the direction glyph, the source badge ("WhatsApp", "Gmail", "Calendar"), and the purple "Group" chip on a group thread.
2. Participants: a link to each participant contact, on one line, truncated with an ellipsis.
3. Preview: the row's label on its first line and a one-line preview of its content beneath, both truncated. A message row previews its latest message as "{sender}: {text}"; a meeting row previews "Meeting note · {first words of the note}"; a call row shows its call summary ("Voice call · Connected · 12m 4s"); a manual entry shows "No content". This column takes all width the others leave.
4. Date: the row's time, and beneath it its message count or attendee count when it has one.
5. Review indicator, right-aligned, from the server's review status: "{n} to review", amber-800 on an amber-50 pill with an amber-300 border, while {n} items of the proposals the row shows have no label; else "Nones to review" on the same pill while a negative decision of any of its proposals has no label; else the quiet settled mark, a gray-500 check and "Reviewed" in gray-500. A row that holds no judgment leaves the cell empty.
6. Actions: the expand button for a row with content, and the "More actions" menu with the actions its kind offers.

The grid's fixed columns are 170 px, 200 px, the preview, 168 px, 104 px and 72 px, with 16 px gaps. The row names no judgment, outcome or flag; those appear only in the drill-down.

## States the canvas shows

Artboards 1, 3, 4 and 5 are the view narrowed to one run, so each row shows only that run's judgments. Artboards 2 and 8 are the view without a run filter, so a row shows every judgment it holds.

### 1. Reviewing a run: the list narrowed to one run

The main navigation gains an "Interactions" entry between Contacts and Birthdays. The page header reads "Interactions" with the subtitle "Every interaction across your contacts". One card holds the filter bar, the run summary, the rows and the pagination.

The filter bar holds, from the left, the type filter, two labelled selects and a "Clear filters" link:

- Type: a labelled segmented control, the first filter, with "All" and then one segment per interaction type the list route offers. The selected segment is gray-900 with white text; the others are gray-600 on white. The canvas draws "All", "Messages", "Email", "Calendar", "Calls" and "Other" for illustration.
- Contact: "All contacts", then every contact.
- Run: "Any run, or none", then every run from `GET /api/v1/judgment-runs`, newest first, each named "{kind} · {content window} · {method key} {method version}", for example "Historical · Jan 1–31, 2026 · clef-masked v1".

A select whose value narrows the list carries the blue border; the segmented control shows its choice by its selected segment. The filters live in the page's query string, so a link opens the view filtered.

When a run is chosen, a blue-50 strip under the filters states the run: its kind, its content window, its method and version, "Finished {time}" or "Running", the number of threads judged, and, in amber, the number incomplete. Below it an "Incomplete threads" disclosure, open by default, lists each incomplete thread with its label, its cut-off and its reason in words: `over_limit` "Over the model's limit", `call_failed` "The call failed", `input_changed` "Content changed during the run".

Rows follow, newest first, as "The list row" describes. The artboard draws six: a settled row, four rows with items to review, and a row of a judged thread that holds none of its judgments and so shows no indicator. The shipped `Pagination` closes the card: "Page {p} of {pages} ({total} total interactions)" with Previous, page numbers and Next.

### 2. A group thread: its proposal beneath the content it read

Expanding a judged row opens the drill-down, laid out in content order:

1. A frame with a blue-200 border holding, in order, the messages the judgment read, as the shipped drill-down shows them, with the user's own sender name "Me" in blue-700; the cut-off rule; and the proposal, directly under the rule.
2. The cut-off rule: a blue line across the thread, broken by an up arrow and "judged up to {cut-off date and time}", so the arrow points at the content the judgment read.
3. The proposal's header: "Proposal", the method key and version, "production method" when it is the designated one, and at its right what remains to review in that proposal alone: "{n} to review" in amber while its items have no label, else "Nones to review" in amber with the "Confirm Nones" button where the proposal has no items, else "All reviewed" in green. Under it the proposal's three sections stack, each across the proposal's full width: the replies section, with "I owe a reply" and "Owes me a reply" side by side in two equal columns; Commitments; and Big moment. A gray-200 rule separates each section from the one above it. Each list's items stack in it, with its Add control at its foot.
4. Each other judgment of the same content, by another method, inside the same frame under the first proposal, collapsed to one line: "Proposal", its method and version, "Not acting · {n} items", and a "Show proposal" button. Opened, it shows its lists as the first proposal does, each item still reviewable.
5. Content after the cut-off, below the frame, at reduced opacity, each message tagged "After the judgment".

The artboard draws a group thread of three participant contacts: "I owe a reply" holds one suggested item, "Owes me a reply" holds none, Commitments holds two live items and one suggested item from the participants, each quoting its source message, and Big moment holds a live participant's moment. A second method's proposal sits collapsed beneath the first, and the row counts the unreviewed items of both.

### 3. The group thread's proposal, corrected step by step

The artboard shows the thread's row, a line "↑ judged up to {cut-off}" naming the content artboard 2 draws, and three steps of the one proposal:

- Step 1 of 3, the proposal as judged, as on artboard 2.
- Step 2 of 3: the user confirmed the suggested reply item, which now reads Live and Confirmed, and opened the "Add commitment" picker, which offers "Me → {name}" for each participant contact.
- Step 3 of 3, after more corrections and a reload: the reply item reads Live and Confirmed; a person the user added to "I owe a reply" reads Live and Added by you; one commitment reads Confirmed; the suggested commitment the user removed is struck through and reads Removed by you, without its quote; one live commitment still offers its controls; the commitment the user added reads "Me → {name} · no stated date", Live and Added by you, with no quote; the flag reads Live and Confirmed. The header counts the one item left to review, and so does the row.

### 4. "thanks, sounds good!": a suggested removal, step by step

A one-to-one email thread whose last message is the user's "thanks, sounds good!" (SC7):

- Step 1 of 2, the proposal as judged: "Owes me a reply" holds the contact as Live, the timestamp rule's wait, with the strip "Suggested: remove", its confidence, "Accept" and "Keep".
- Step 2 of 2, after Accept and a reload: the item is struck through and reads Removed by you, and the header reads "All reviewed".

### 5. A suggested later date: Keep original, step by step

A one-to-one thread whose contact moved a promised date:

- Step 1 of 2, the proposal as judged: Commitments holds "{name} → Me · due Fri, Jan 16" as Live, quoting the message that made the promise, with the strip "Suggested: due Fri, Jan 23", its confidence, "Accept" and "Keep original".
- Step 2 of 2, after Keep original and a reload: the commitment reads "due Fri, Jan 16", Live and Confirmed.

Accepting instead would leave the item reading "due Fri, Jan 23", Live and Confirmed.

### 6 and 7. Big-moment flags on the thread, beneath the content judged

The flag is the Big moment list's one item, inside the proposal directly beneath the content the judgment read. It reads "Big moment", with a pink flag icon, its confidence, its state and its review mark, whichever of the two big-moment values the judgment holds. It never summarizes the moment. A flag is accepted when its item is Live, which includes a flag the user confirmed or added under the production method. Artboard 6 draws a participant's new job, Live and awaiting review; artboard 7 draws a sibling's wedding mentioned in passing, Live and Confirmed.

### 8. Without a run: every interaction, by contact and type

With the Run select at "Any run, or none", the list shows every interaction, and each judged row counts the items of its proposals from every run. The artboard draws the list filtered to one contact on page 2 of 3, with a call, a manual entry and a message burst beside judged group-thread and meeting rows.

### 9. Contact page: link out to the view

The contact's Interactions section header gains a link at its right, "Review in all interactions" with an arrow, that opens `/interactions?contact={id}`. The contact page gains no review control.

### 10. Options from the reference artifacts

Four options, each drawn beside the shipped style it would replace and labelled with the user's ruling: "Kept" and "Rejected" where the shipped style stays, "Replaced" and "Accepted" where the option became the design's default. See "Options from the reference artifacts" below.

### 11. Nothing proposed: Confirm Nones

A one-to-one thread whose every answer is None. Step 1 of 2: the row reads "Nones to review"; each list reads "None", "closest: {name}" and "{n}%" in a dashed box, and the header reads "Nones to review" beside "Confirm Nones". Step 2 of 2, after Confirm Nones: each None adds "Confirmed" in a solid box, the header reads "All reviewed" and the row "Reviewed", and every Add stays.

### 12. A large meeting with nothing proposed

A meeting of thirty attendees, sixty commitment decisions and the flag, all None. The proposal has no reply lists and shows one None line for Commitments and one for Big moment, each with its closest decision, and one "Confirm Nones".

### 13. Items and Nones together

A group thread with two suggested items. Step 1 of 4: the row and header read "2 to review", and there is no Confirm Nones; Commitments shows its item and, on its heading line, "closest None: Me → {name} · {n}%". Step 2 of 4, after confirming both items: every None reads "Confirmed" and each reviewed item offers "Change". Step 3 of 4: "Owes me a reply"'s Add offers every participant whose decision is None, confirmed or not, and not the person confirmed in "I owe a reply". Step 4 of 4: the person chosen reads Live and Added by you; "Change" on the confirmed item brings back its controls and "Cancel".

### 14. Labels that differ from live state

Three changes of review, each before and after: a confirmed live commitment changed to removed reads Live and Removed by you, unstruck, its task still open; an item whose removal took effect, struck, changed to confirmed reads Not acting and Confirmed; an accepted later date changed to Keep original reads the obligation's later date and "Kept {original date} by you".

## The bench presentation questions, settled

- **How claim review controls sit on the bench.** Inline, on each item of the proposal and at the foot of each list, as "User acts and the reviews they record" lists. There is no review queue, modal, form or second page, and the list rows carry no review control.
- **Display thresholds for low-confidence claims.** None. Every item shows, whatever its confidence, with the confidence as a whole percentage. Recall wins over precision, and reviewing a run needs every suggestion in sight.
- **Shared interactions: participant contexts or participation rows.** Participant contexts. An interaction several contacts take part in is one row, naming each participant contact. A group chat's messages, which the store holds once per member, form one row too: copies of the same content held for several contacts are one row. PR5 decides how it recognizes copies; the content reference IXN-034 keeps stable across copies is one way. The row reads its content in the participation context of its first participant contact, since every copy shows the same messages.
- **Where a thread judgment appears.** On exactly one row: the row holding the thread's latest content at or before the judgment's cut-off. A thread with three participant contacts therefore shows its proposal once. A meeting context's judgment sits on the meeting's row. A judgment a later run reused keeps the cut-off of its first appearance for its content, and in a view narrowed to the reusing run its proposal header adds "Reused at {run's cut-off}".
- **The view's ordering.** Newest first, with the shipped row's tie-breaker, in every filter state. Proposals on one row: the production method's first, then the others by method key; with no production method designated, by method key. Items within a list: by the participant contact's name, and for one contact the user's own commitment before the contact's.
- **The filter set.** Contact, interaction type and run, combinable, and no other (rabbit hole 4).
- **The route.** `/interactions` for the view and `GET /api/v1/interactions` for its list, as the plan allocates.
- **The contact timeline.** It links out (state 9) and carries no review controls.
- **Empty states.** A filter matching nothing shows "No interactions match these filters" with the Clear filters link. A run with no judged thread shows its summary and "This run judged no threads".

## Options from the reference artifacts

The design starts from the shipped app. The references are `designs/ui-components.pen`, `.agents/spec/wireframes/` and the user's artifacts "Interactions Section", "Skip This Cycle" and "Back to list — CON-051 prototype". Each style a reference artifact carries that the app has not shipped is listed here. Where the bench has an element it applies to, artboard 10 draws it as an option beside the shipped style, and the user ruled on each (arc ruling R24, read from where the user's comments sit on artboard 10). An accepted option becomes the design's default; a rejected one leaves the shipped style in place.

| Option | Source | Default | Option | Ruling |
|---|---|---|---|---|
| O1 Row layout | Interactions Section | The shipped row: direction glyph, source badge first, label below | Kind icon leading the row, label first, outlined venue chip, gray Group chip with an icon | Rejected: the shipped row stays, on the bench and on the contact page |
| O2 "Me" in blue | Interactions Section | Every sender in gray-700 | The user's own sender name in blue-700 in the drill-down | Accepted: "Me" is blue-700 in the drill-down. The drill-down is shared, so the contact page's drill-down changes too |
| O3 Secondary controls as text buttons | Skip This Cycle | Outline buttons for "Keep", "Keep original", "Add" and "Add commitment" | Those four as gray underlined text buttons | Rejected: the outline buttons stay |
| O4 Type filter as a segmented control | Interactions Section | A select, like the contact filter | A segmented control with a dark selected segment | Accepted: the type filter is a segmented control, the first filter on the left |

These reference styles are not offered, because the bench has no element they apply to:

- The "Back to list" layouts C (a header link) and D (a full-bleed bar) of the CON-051 prototype: the bench has no record pager. Layout A shipped.
- The segmented date presets of Interactions Section: the bench has no date filter.
- The uppercase captions of Skip This Cycle: they label artboards, not product UI.

## Behaviors

Each behavior of the bench is one row in `spec/interactions.yaml`, cited by the E2E tests that prove it:

| ID | Behavior | States |
|---|---|---|
| IXN-041 | One full-width row per interaction across contacts, paginated, reachable from the main navigation | 1, 8 |
| IXN-042 | Contact, type and run filters, combinable, in the address, and no other | 1, 8 |
| IXN-043 | A run's summary and its incomplete threads | 1 |
| IXN-044 | A thread judgment appears once, on the row holding the latest content it read, which carries one review indicator | 1, 2, 8 |
| IXN-045 | The proposal sits beneath the content as of its cut-off, the cut-off pointing at that content and later content marked after the judgment | 2 |
| IXN-046 | The proposal lists answers, not questions, each item with its state and each commitment with its source message | 2, 3, 5 |
| IXN-047 | The user corrects the proposal inline, each act one review | 3, 4, 5 |
| IXN-048 | Each item shows its review in place after a reload | 3, 4, 5 |
| IXN-049 | An accepted big-moment flag on the thread, beneath the content judged | 6, 7 |
| IXN-050 | The contact history links out to the view filtered to that contact | 9 |
| IXN-061 | Each list names the decision the judgment came closest to making | 11, 12, 13 |
| IXN-062 | Confirm Nones labels a proposal's Nones in one act | 11, 12 |
| IXN-063 | Each proposal's header states what remains to review in it | 11, 13 |
| IXN-064 | Reviewing a proposal's last item confirms its Nones | 13 |
| IXN-065 | Change re-reviews an item, which shows its live state beside the new mark | 13, 14 |
| IXN-066 | Add still corrects a miss after the Nones are confirmed | 11, 13 |
| IXN-067 | A review shows in every proposal on the same content | 13 |

## Not designed

- How the review route records "Keep original". S4's review body carries `right_outcome` alone, and against a suggested later date both "Accept" and "Keep original" state `open`, the judged outcome, so the body S4 pins cannot tell them apart. PR5 extends the review route with an optional flag that keeps the obligation's current date (arc ruling R23).
- The type filter's values, which PR5 settles from its list route.
- Any surface beyond the view and its navigation entry (NG4).
