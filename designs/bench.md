# Judgment bench: the all-interactions view

The bench is one new page, the all-interactions view at `/interactions`, where the user reviews the judgments decision models make about threads. It lists every interaction across contacts with the contact history's row and drill-down, narrows to one judgment run, and shows each thread judgment once, at its cut-off, beside the content it read, with controls to confirm an outcome or state its right answer. PR5 builds it from this note and from the proposed behaviors IXN-041…IXN-050 in `spec/interactions.yaml`, without opening the canvas.

The canvas is "Judgment Bench" (https://claude.ai/artifact/1d1TcFH9iXtdtRABK6n4iX), a Design canvas of eight artboards. Every person, message and date on it is synthetic.

The user's approval of this design is open, and so are the user's rulings on the four options below. PR5 builds against this note only once the user approves it.

Sources: `.agents/spec/2026-10-14-decision-model-extraction.md` (G5, G7f, G8, G9, G11, SC1, SC7, SC10, NG4, rabbit hole 4) and the bench routes of the decision-model foundation plan (S4): `GET /api/v1/judgment-runs`, `GET /api/v1/judgments` and `POST /api/v1/judgment-outcomes/:id/review`. The design shows only what those routes and PR5's `GET /api/v1/interactions` carry.

## Styling

The bench uses the app's shipped styling and components: the Tailwind gray and blue palette, Geist, the white card with a light shadow, the shipped `TimelineRow` and `TimelineDrillDown`, the shipped `Button` variants, native selects as on the contact history's venue filter, and the shipped `Pagination`. It adds four colored labels, each a light fill with a darker text and border of one hue, so the states differ in lightness as well as hue:

| Label | Fill / text / border | Used for |
|---|---|---|
| Claim | amber-50 / amber-800 / amber-300 | an outcome in state `claim`; the row marker while claims await review |
| Applied | blue-50 / blue-700 / blue-200 | an outcome in state `applied` |
| Evidence | gray-100 / gray-600 / none | an outcome in state `evidence`; an evidence-only judgment |
| Confirmed | green-50 / green-800 / green-300 | verdict `confirmed`; the row marker once nothing awaits review |
| Rejected | gray-100 / gray-700 / gray-300 | verdict `rejected` |
| Corrected | violet-50 / violet-800 / violet-300 | verdict `corrected` |
| Big moment | pink-50 / pink-800 / pink-300 | an accepted big-moment flag and its row marker |

The styles the reference artifacts carry that the app has not shipped are options on artboard 8, each off by default (see "Options from the reference artifacts").

## States the canvas shows

### 1. Reviewing a run: the list narrowed to one run

The main navigation gains an "Interactions" entry between Contacts and Birthdays. The page header reads "Interactions" with the subtitle "Every interaction across your contacts". One card holds the filter bar, the run summary, the rows and the pagination.

The filter bar holds three labelled selects and a "Clear filters" link:

- Contact: "All contacts", then every contact.
- Type: "All types", then the interaction types the list route offers.
- Run: "Any run, or none", then every run from `GET /api/v1/judgment-runs`, newest first, each named "{kind} · {content window} · {method key} {method version}", for example "Historical · Jan 1–31, 2026 · clef-masked v1".

A select whose value narrows the list carries the blue border. The filters live in the page's query string, so a link opens the view filtered.

When a run is chosen, a blue-50 strip under the filters states the run: its kind, its content window, its method and version, "Finished {time}" or "Running", the number of threads judged, and, in amber, the number incomplete. Below it an "Incomplete threads" disclosure, open by default, lists each incomplete thread with its label, its cut-off and its reason in words: `over_limit` "Over the model's limit", `call_failed` "The call failed", `input_changed` "Content changed during the run".

Rows are the shipped `TimelineRow`, newest first. Each row adds one line under its label: "With" and a link to each participant contact. A judged row adds a marker line:

- "Judged as of {cut-off date} · {n} claims to review" in the Claim label while any claim of its judgments has no review.
- "Judged as of {cut-off date} · all reviewed" in the Confirmed label once every claim has a review, and "· nothing to review" when the judgments raised no claim.
- "Big moment" in the Big moment label when an accepted big-moment flag sits on the row.

A row of a judged thread that holds none of its judgments has no marker. The shipped `Pagination` closes the card: "Page {p} of {pages} ({total} total interactions)" with Previous, page numbers and Next.

### 2. A group thread's drill-down as of its cut-off

Expanding a judged row opens the shipped drill-down, laid out in content order:

1. The messages the judgment read, as the shipped drill-down shows them.
2. When the judgment's big-moment outcome is accepted, the flag (state 5), right-aligned under the last message read.
3. The cut-off line: a blue rule across the drill-down with the text "Judged up to here · cut-off {date and time}".
4. The judgment, a white card at the cut-off line. Its header reads "Judgment", the method key and version, "production method" when it is the designated one, and "Read the thread up to {cut-off}"; at its right, the count of claims to review in amber. The body groups outcomes by participant contact, alphabetically by name, each under the contact's name, then a group headed "The thread" for the big-moment outcome. Within a participant group the order is reply owed, my commitment, their commitment. Each outcome is one line: the question, the answer with its confidence as a whole percentage and, for a commitment, its due date or "no date", the state label, and the review controls (state 3).
5. Each other judgment of the same content, by another method, as a dashed card: "Judgment", its method and version, "same content, up to {cut-off}", and "Evidence · {n} answers", collapsed behind "Show answers". Opened, it lists its outcomes as the production judgment does, each still reviewable.
6. Content after the cut-off, at reduced opacity, each message tagged "Later", followed by "Content after the cut-off arrived later. The judgment did not read it."

A judgment card with claims to review opens expanded. One with none opens collapsed to its header and a "Show {n} answers" button, as state 5 draws it.

### 3. Confirming, and stating a different right answer

Every outcome without a review offers two outline buttons, "Confirm" and "Change answer", whatever its state. Confirm sends the judged answer as `rightOutcome`. Change answer opens an inline form under the outcome line, and only one form is open at a time. The form asks "What is the right answer?" and offers the question's other answers as radio buttons; "Save answer" (primary) stays disabled until one is chosen, and "Cancel" closes the form. Save sends the chosen answer as `rightOutcome`. The client never sends a verdict. If the request fails, the form stays open with the error "Couldn't save the review. Try again." under its buttons.

### 4. After a reload: each outcome shows its review

A reviewed outcome replaces its controls with its verdict label and review time. A confirmed outcome keeps its answer and adds "Judged the same · {confidence} · was {state}". A rejected or corrected outcome leads with "Right answer: {answer}" and adds "Judged: {answer} · {confidence} · was {state}". A reviewed outcome offers no further review control. The row marker's count leaves out the reviewed claims.

### 5. Big-moment flags beside the content judged

A judgment answers for the thread up to its cut-off, not for one message, so its flag sits beside the content it read: right-aligned under the last message at or before the cut-off, above the cut-off line. The flag reads "Big moment", then "A participant's own, in the messages above" or "Someone else's, mentioned in the messages above", the confidence, and the outcome's state or verdict label. A flag is accepted, and so shown, when its outcome is `applied` or reviewed as `confirmed` or `corrected` to a moment value. The flag never summarizes the moment.

### 6. Without a run: every interaction, by contact and type

With the Run select at "Any run, or none", the list shows every interaction, and each judged row shows its marker and judgments from every run. The artboard draws the list filtered to one contact on page 2 of 3, with a shared calendar event, a call, a manual entry and a message burst beside judged group-thread and meeting rows.

### 7. Contact page: link out to the view

The contact's Interactions section header gains a link at its right, "Review in all interactions" with an arrow, that opens `/interactions?contact={id}`. The contact page gains no review control.

### 8. Options from the reference artifacts

Four options, each drawn beside its default. See "Options from the reference artifacts" below for each ruling.

## Answer wording

The bench names participants by contact name. The Pi's role labels never appear.

| Question | Question label | Answer labels |
|---|---|---|
| `ball_in_court` | Reply owed | `i_owe_reply` "I owe {name} a reply"; `they_owe_reply` "{name} owes me a reply"; `nobody_owes` "Nobody owes a reply"; `not_applicable` "Doesn't apply: an automated message or a broadcast" |
| `commitment_mine` | My commitment to {name} | `open` "I committed to something"; `none` "Nothing promised" |
| `commitment_theirs` | {name}'s commitment to me | `open` "{name} committed to something"; `none` "Nothing promised" |
| `big_moment` | Big moment | `participant_moment` "A participant's own"; `other_person_moment` "Someone else's, mentioned"; `none` "No big moment" |

## The bench presentation questions, settled

- **How claim review controls sit on the bench.** Inline, on each outcome line of the judgment card in the drill-down: Confirm, and Change answer with its inline form (states 3 and 4). There is no review queue, modal or second page.
- **Display thresholds for low-confidence claims.** None. Every outcome shows, whatever its confidence, with the confidence as a whole percentage. Recall wins over precision, and reviewing a run needs every claim in sight.
- **Shared interactions: participant contexts or participation rows.** Participant contexts. An interaction several contacts take part in is one row, naming each participant contact. A group chat's messages, which the store holds once per member, form one row too: copies of the same content held for several contacts are one row. PR5 decides how it recognizes copies; the content reference IXN-034 keeps stable across copies is one way. The row reads its content in the participation context of its first participant contact, since every copy shows the same messages.
- **Where a thread judgment appears.** On exactly one row: the row holding the thread's latest content at or before the judgment's cut-off. A thread with three participant contacts therefore shows its judgment once. A meeting context's judgment sits on the meeting's row. A judgment a later run reused keeps the cut-off of its first appearance for its content, and in a view narrowed to the reusing run its header adds "Reused at {run's cut-off}: no new content".
- **The view's ordering.** Newest first, with the shipped row's tie-breaker, in every filter state. Judgments on one row: the production method's first, then the others by method key. Outcomes: as state 2 lists them.
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
| O3 Change answer as a text button | Skip This Cycle | Two outline buttons | "Change answer" as a gray underlined text button, Confirm the only button | Open: the default holds until the user rules |
| O4 Type filter as a segmented control | Interactions Section | A select, like the contact filter | A segmented control with a dark selected segment | Open: the default holds until the user rules |

These reference styles are not offered, because the bench has no element they apply to:

- The "Back to list" layouts C (a header link) and D (a full-bleed bar) of the CON-051 prototype: the bench has no record pager. Layout A shipped.
- The segmented date presets of Interactions Section: the bench has no date filter.
- The uppercase captions of Skip This Cycle: they label artboards, not product UI.

## Behaviors

Each behavior the design asks PR5 to build is one `status: proposed` row in `spec/interactions.yaml`, which PR5 flips to `current` with citing E2E tests:

| ID | Behavior | States |
|---|---|---|
| IXN-041 | One row per interaction across contacts, paginated, reachable from the main navigation | 1, 6 |
| IXN-042 | Contact, type and run filters, combinable, in the address, and no other | 1, 6 |
| IXN-043 | A run's summary and its incomplete threads | 1 |
| IXN-044 | A thread judgment appears once, on the row holding the latest content it read | 1, 2 |
| IXN-045 | The drill-down shows a judgment against the content as of its cut-off | 2 |
| IXN-046 | Each outcome states its answer, confidence and state | 2 |
| IXN-047 | Review by confirming or stating the right answer | 3 |
| IXN-048 | A reviewed outcome shows its review after a reload | 4 |
| IXN-049 | An accepted big-moment flag beside the content judged | 5 |
| IXN-050 | The contact history links out to the view filtered to that contact | 7 |

## Not designed

- Re-reviewing an outcome. A reviewed outcome offers no control, and S4 does not say whether a second review replaces the first.
- The type filter's values, which PR5 settles from its list route.
- Any surface beyond the view and its navigation entry (NG4).
