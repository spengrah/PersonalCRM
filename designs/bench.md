# Judgment bench: the all-interactions view

The bench is one new page, the all-interactions view at `/interactions`, where the user reviews the judgments decision models make about threads. It lists every interaction across contacts with the contact history's row and drill-down, narrows to one judgment run, and shows each thread judgment once, as a proposal organized by answers, directly beneath the content it read. The user corrects the proposal inline. PR5 builds it from this note and from the proposed behaviors IXN-041…IXN-050 in `spec/interactions.yaml`, without opening the canvas.

The canvas is "Judgment Bench" (https://claude.ai/artifact/1d1TcFH9iXtdtRABK6n4iX), a Design canvas of eight artboards. Every person, message and date on it is synthetic.

The user's approval of this design is open, and so are the user's rulings on the four options below. PR5 builds against this note only once the user approves it.

Sources: `.agents/spec/2026-10-14-decision-model-extraction.md` (G3, G5, G7f, G8, G9, G11, SC1, SC4, SC7, SC10, NG4, rabbit hole 4), the bench routes of the decision-model foundation plan (S4): `GET /api/v1/judgment-runs`, `GET /api/v1/judgments` and `POST /api/v1/judgment-outcomes/:id/review`, PR5's `GET /api/v1/interactions`, and the bench read PR5 adds, which returns each judged thread's proposal already compiled. Wire fields are snake_case (`right_outcome`, `cut_off`, `run_id`, `contact_id`).

## The proposal

Each thread judgment is shown as one proposal of four lists. The questions sent to the decision model never appear.

| List | Holds | Item text |
|---|---|---|
| I owe a reply | each participant contact the user owes a reply | the contact's name |
| Owes me a reply | each participant contact who owes the user a reply | the contact's name |
| Commitments | every open commitment, the user's own and the participants', in one list | "{debtor} → {creditor}", with "due {date}" or "no date" |
| Big moment | the thread's big-moment flag, at most one | "Participant's own" or "Someone else's" |

The user is "Me" in a commitment's text. A reply list holds at most one person in a one-to-one thread and possibly several in a group thread. A meeting context's proposal has no reply lists, since ball-in-court is not asked of meetings. A list with no item reads "None". Big moments stay a flag on the thread, attached to no contact.

The server compiles the question tree into these lists, so the client applies no judgment rule. In particular, a party's commitment absorbs that party's reply item in the same direction: when the user committed to Sam, the proposal shows "Me → Sam" and no "I owe Sam a reply" item, since the two are one obligation (G3b, SC4). An outcome with a no-effect value (`nobody_owes`, `not_applicable`, `none`) produces no item, unless it is a suggested change to a live item.

Each item stands for exactly one judged outcome, and each user act on an item reviews that one outcome.

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

After a review an item stays in place, never hidden, and its controls give way to a mark:

- "Confirmed", green-800 with a check, when the user confirmed the item or kept a live item against a suggested change.
- "Removed by you", with the item's text struck through in gray-500 and its state label gone, when the user removed the item or accepted a suggested removal.
- "Added by you", green-800 with a plus, on an item the user added.

An item keeps its state beside its mark: a confirmed suggestion of the production method reads Live and Confirmed, and a confirmed item of a non-production method reads Not acting and Confirmed. The verdicts the store derives (`confirmed`, `rejected`, `corrected`) are eval data and never appear as words on the bench.

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
| Add a flag | "Add", then "Participant's own" or "Someone else's" | the thread's `big_moment` | `participant_moment` or `other_person_moment` |
| Accept a suggested change | "Accept" on the strip | the item's outcome | the judged outcome |
| Keep a live item against a suggested removal | "Keep" on the strip | the item's outcome | the live value, such as `they_owe_reply` for a wait |

An item's controls are a check button and a × button, both 32 px icon buttons with labels for assistive technology ("Confirm Sam Okafor", "Remove Sam Okafor"). A live item with a suggested change shows "Accept" and, for a removal, "Keep" on its strip instead. Adding a person to one reply list moves the person out of the other, since both lists answer the same question.

"Add" and "Add commitment" open an inline picker under the list: one chip per choice and a close button. The reply lists' picker offers each participant contact not already in that list whose `ball_in_court` outcome has no review. The commitments picker offers each "Me → X" and "X → Me" pair that is not already an item and whose commitment outcome has no review. The flag's picker offers its two values while the thread has no flag. Choosing a chip records the review and closes the picker. Only one picker is open at a time. A list whose picker would offer nothing shows no Add control.

An added commitment has no date, since `right_outcome` carries none, and reads "no date". Removing a commitment that absorbed a reply item reviews only the commitment, so the reply item can appear on the next read.

A review shows on its item at once, without a reload. If the request fails, the item keeps its controls and shows "Couldn't save. Try again." beneath them.

## Styling

The bench uses the app's shipped styling and components: the Tailwind gray and blue palette, Geist, the white card with a light shadow, the shipped `TimelineRow` and `TimelineDrillDown`, the shipped `Button` variants, native selects as on the contact history's venue filter, and the shipped `Pagination`. It adds the item states above and the big-moment flag in pink (pink-50 fill, pink-300 border, pink-800 text, with a flag icon). The states differ in lightness and border style as well as hue.

The page and its rows use the full width of the window: the page has no max-width container, and each row places its proposal summary at its right. The inbound (←), outbound (→) and mutual (↔) direction glyphs lead each row as the shipped row draws them.

The bench carries no explanatory copy. The styles the reference artifacts carry that the app has not shipped are options on artboard 8, each off by default (see "Options from the reference artifacts").

## States the canvas shows

### 1. Reviewing a run: the list narrowed to one run

The main navigation gains an "Interactions" entry between Contacts and Birthdays. The page header reads "Interactions" with the subtitle "Every interaction across your contacts". One card holds the filter bar, the run summary, the rows and the pagination.

The filter bar holds three labelled selects and a "Clear filters" link:

- Contact: "All contacts", then every contact.
- Type: "All types", then the interaction types the list route offers.
- Run: "Any run, or none", then every run from `GET /api/v1/judgment-runs`, newest first, each named "{kind} · {content window} · {method key} {method version}", for example "Historical · Jan 1–31, 2026 · clef-masked v1".

A select whose value narrows the list carries the blue border. The filters live in the page's query string, so a link opens the view filtered.

When a run is chosen, a blue-50 strip under the filters states the run: its kind, its content window, its method and version, "Finished {time}" or "Running", the number of threads judged, and, in amber, the number incomplete. Below it an "Incomplete threads" disclosure, open by default, lists each incomplete thread with its label, its cut-off and its reason in words: `over_limit` "Over the model's limit", `call_failed` "The call failed", `input_changed` "Content changed during the run".

Rows are the shipped `TimelineRow`, newest first. Each row adds one line under its label: "With" and a link to each participant contact. A judged row adds, at its right, its proposal summary:

- "Judged as of {cut-off date} · {n} to review" in amber-800 while {n} items have no review, or "Judged as of {cut-off date} · all reviewed" in green-800 once every item has one.
- One compact chip per item of the leading proposal, in the item's state look: "I owe {name} a reply", "{name} owes me a reply", "{debtor} → {creditor}", and the pink "Big moment" chip. A confirmed item's chip carries a check, a removed item's chip is struck through, and a live item with a suggested change adds "· suggested removal" or "· suggested later date".

A row of a judged thread that holds none of its judgments has no summary. The shipped `Pagination` closes the card: "Page {p} of {pages} ({total} total interactions)" with Previous, page numbers and Next.

### 2. A group thread's proposal beneath the content it read

Expanding a judged row opens the shipped drill-down, laid out in content order:

1. A frame with a blue-200 border holding, in order, the messages the judgment read, as the shipped drill-down shows them; the cut-off line, a blue rule with "Read up to here · {cut-off date and time}"; and the proposal, directly under the line.
2. The proposal's header: "Proposal", the method key and version, "production method" when it is the designated one, and at its right "{n} to review" in amber or "All reviewed" in green. Under it the four lists side by side (I owe a reply, Owes me a reply, Commitments, Big moment), Commitments twice as wide as the others, wrapping to a stack at narrow widths. Each list's items stack in it, with its Add control at its foot.
3. Each other judgment of the same content, by another method, inside the same frame under the first proposal, collapsed to one line: "Proposal", its method and version, "Not acting · {n} items", and a "Show proposal" button. Opened, it shows its lists as the first proposal does, each item still reviewable.
4. Content after the cut-off, below the frame, at reduced opacity, each message tagged "Later".

The artboard draws a group thread of three participant contacts: "I owe a reply" holds one suggested item, "Owes me a reply" holds none, Commitments holds two live items and one suggested item from the participants, and Big moment holds a live participant's moment.

### 3. Correcting a proposal inline

The artboard draws three proposals mid-review:

- The group thread of state 2 after the user confirmed the suggested reply item, which now reads Live and Confirmed, with the "Add commitment" picker open, offering "Me → {name}" for each participant contact.
- A one-to-one email thread whose last message is the user's "thanks, sounds good!" (SC7): "Owes me a reply" holds the contact as Live, the timestamp rule's wait, with the strip "Suggested: remove", its confidence, "Accept" and "Keep".
- A one-to-one thread whose contact moved a promised date: Commitments holds "{name} → Me · due Fri, Jan 16" as Live with the strip "Suggested: due Fri, Jan 23", its confidence and "Accept".

### 4. After a reload: each item shows its review in place

The artboard draws the three proposals of state 3 after more acts and a reload. In the group thread the reply item reads Live and Confirmed; a person the user added to "Owes me a reply" reads Live and Added by you; one commitment reads Confirmed, the suggested one the user removed is struck through and reads Removed by you, one live commitment still offers its controls, and a commitment the user added reads "Me → {name} · no date", Live and Added by you; the flag reads Live and Confirmed. The header counts the one item left to review. In the email thread the accepted removal is struck through and reads Removed by you. In the moved-date thread the commitment reads "due Fri, Jan 23", Live and Confirmed.

### 5. Big-moment flags on the thread, beneath the content judged

The flag is the Big moment list's one item, inside the proposal directly beneath the content the judgment read. It reads "Participant's own" or "Someone else's", with a pink flag icon, its confidence, its state and its review mark. It never summarizes the moment. A flag is accepted, and so carries the row's pink "Big moment" chip, when its item is Live, which includes a flag the user confirmed or added under the production method. The artboard draws a participant's new job, Live, and a sibling's wedding mentioned in passing, Live and Confirmed.

### 6. Without a run: every interaction, by contact and type

With the Run select at "Any run, or none", the list shows every interaction, and each judged row shows its summary and proposals from every run. The artboard draws the list filtered to one contact on page 2 of 3, with a call, a manual entry and a message burst beside judged group-thread and meeting rows.

### 7. Contact page: link out to the view

The contact's Interactions section header gains a link at its right, "Review in all interactions" with an arrow, that opens `/interactions?contact={id}`. The contact page gains no review control.

### 8. Options from the reference artifacts

Four options, each drawn beside its default. See "Options from the reference artifacts" below for each ruling.

## The bench presentation questions, settled

- **How claim review controls sit on the bench.** Inline, on each item of the proposal and at the foot of each list, as "User acts and the reviews they record" lists. There is no review queue, modal, form or second page.
- **Display thresholds for low-confidence claims.** None. Every item shows, whatever its confidence, with the confidence as a whole percentage. Recall wins over precision, and reviewing a run needs every suggestion in sight.
- **Shared interactions: participant contexts or participation rows.** Participant contexts. An interaction several contacts take part in is one row, naming each participant contact. A group chat's messages, which the store holds once per member, form one row too: copies of the same content held for several contacts are one row. PR5 decides how it recognizes copies; the content reference IXN-034 keeps stable across copies is one way. The row reads its content in the participation context of its first participant contact, since every copy shows the same messages.
- **Where a thread judgment appears.** On exactly one row: the row holding the thread's latest content at or before the judgment's cut-off. A thread with three participant contacts therefore shows its proposal once. A meeting context's judgment sits on the meeting's row. A judgment a later run reused keeps the cut-off of its first appearance for its content, and in a view narrowed to the reusing run its proposal header adds "Reused at {run's cut-off}".
- **The view's ordering.** Newest first, with the shipped row's tie-breaker, in every filter state. Proposals on one row: the production method's first, then the others by method key; with no production method designated, by method key. Items within a list: by the participant contact's name, and for one contact the user's own commitment before the contact's.
- **The filter set.** Contact, interaction type and run, combinable, and no other (rabbit hole 4).
- **The route.** `/interactions` for the view and `GET /api/v1/interactions` for its list, as the plan allocates.
- **The contact timeline.** It links out (state 7) and carries no review controls.
- **Empty states.** A filter matching nothing shows "No interactions match these filters" with the Clear filters link. A run with no judged thread shows its summary and "This run judged no threads".

## Options from the reference artifacts

The design starts from the shipped app. The references are `designs/ui-components.pen`, `.agents/spec/wireframes/` and the user's artifacts "Interactions Section", "Skip This Cycle" and "Back to list — CON-051 prototype". Each style a reference artifact carries that the app has not shipped is listed here. Where the bench has an element it applies to, artboard 8 draws it as an option beside the default, and the default stays the shipped style.

| Option | Source | Default | Option | Ruling |
|---|---|---|---|---|
| O1 Row layout | Interactions Section | The shipped row: direction glyph, source badge first, label below | Kind icon leading the row, label first, outlined venue chip, gray Group chip with an icon. Accepting changes the shared row, so the contact page changes too | Open: the default holds until the user rules |
| O2 "Me" in blue | Interactions Section | Every sender in gray-700 | The user's own sender name in blue-700 in the drill-down. Accepting changes the shared drill-down | Open: the default holds until the user rules |
| O3 Secondary controls as text buttons | Skip This Cycle | Outline buttons for "Keep", "Add" and "Add commitment" | Those three as gray underlined text buttons, so "Accept" and the item buttons stand alone as buttons | Open: the default holds until the user rules |
| O4 Type filter as a segmented control | Interactions Section | A select, like the contact filter | A segmented control with a dark selected segment | Open: the default holds until the user rules |

These reference styles are not offered, because the bench has no element they apply to:

- The "Back to list" layouts C (a header link) and D (a full-bleed bar) of the CON-051 prototype: the bench has no record pager. Layout A shipped.
- The segmented date presets of Interactions Section: the bench has no date filter.
- The uppercase captions of Skip This Cycle: they label artboards, not product UI.

## Behaviors

Each behavior the design asks PR5 to build is one `status: proposed` row in `spec/interactions.yaml`, which PR5 flips to `current` with citing tests:

| ID | Behavior | States |
|---|---|---|
| IXN-041 | One row per interaction across contacts, paginated, reachable from the main navigation | 1, 6 |
| IXN-042 | Contact, type and run filters, combinable, in the address, and no other | 1, 6 |
| IXN-043 | A run's summary and its incomplete threads | 1 |
| IXN-044 | A thread judgment appears once, on the row holding the latest content it read | 1, 2 |
| IXN-045 | The proposal sits beneath the content as of its cut-off, later content marked later | 2 |
| IXN-046 | The proposal lists answers, not questions, each item with its state | 2, 3 |
| IXN-047 | The user corrects the proposal inline, each act one review | 3 |
| IXN-048 | Each item shows its review in place after a reload | 4 |
| IXN-049 | An accepted big-moment flag on the thread, beneath the content judged | 5 |
| IXN-050 | The contact history links out to the view filtered to that contact | 7 |

## Not designed

- Re-reviewing an outcome. A reviewed item offers no control, and the pickers leave out a person or pair whose outcome has a review, since S4 does not say whether a second review replaces the first. Changing a flag between "Participant's own" and "Someone else's" after a review falls under the same gap.
- Keeping a commitment's earlier date against a suggested later date. `right_outcome` carries no date, so no review states "open, at the earlier date"; the strip offers Accept only, and an unaccepted later date leaves the obligation's date as it was (G6).
- Dating a commitment the user adds, for the same reason.
- The type filter's values, which PR5 settles from its list route.
- Any surface beyond the view and its navigation entry (NG4).
