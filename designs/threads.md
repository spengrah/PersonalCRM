# Threads tab: your threads, with a Review switch

The Threads tab is the second tab of `/interactions`. It is both the user's regular view of their threads and the bench on which the user reviews message flags. A Review switch shows or hides everything about judgments. Stage 2 of https://github.com/spengrah/PersonalCRM/blob/9efb37ebd2157a2ac58c4b3c9c30703afce34f40/.agents/spec/2026-10-18-message-flags-and-threads.md (G8–G17) builds it from this note, without opening the canvas.

The canvas is "Judgment Bench" (https://claude.ai/artifact/1d1TcFH9iXtdtRABK6n4iX), page "Threads tab". The user approved this design on 2026-10-19 (decision 20261019T162753Z-305f42dc685c). Every person, message and date on it is synthetic.

| Artboard | Shows |
|---|---|
| T1 | The Threads list, Review on, narrowed to one run |
| T2 | The Threads list, Review off |
| T3A | A thread page, Review on: the approved layout |
| T3 | The same page with each night boxed: drawn for comparison, not built |
| T4 | A thread page, Review off |
| T5 | The T3A thread labeled, then closed |
| T6 | The Interactions tab |

The look follows the shipped bench (`designs/bench.md`): Geist, gray-50 page, white cards with a soft shadow, the shipped row and outline buttons, the blue "Me" sender, and 32 px square confirm and remove buttons.

## Tabs

`/interactions` opens with a heading "Interactions", the Review switch at the heading's right, and two tabs beneath it: **Interactions** and **Threads**. The tab is part of the URL.

The **Interactions** tab (T6) lists interaction rows for correcting frequency data: merge, delete and correct, through each row's "⋯" menu. Its filters are Type (the segmented All, Messages, Email, Events, Calls, Other) and Contact. It shows no judgment, review indicator or run filter. Review controls return to this tab once a judgment proposes changes to interactions, such as merges; until then the Review switch does not change it.

## The Review switch

The switch reads "Review", with a track that is blue-600 when on and gray-300 when off. It sits at the right of the page heading on the list and in the navigation bar of a thread page. It is one setting: changing it on either page changes it on both, and it stays as the user set it across visits.

With Review off, nothing about judgments shows: no run filter, review status, count of judged messages, flag, judgment box or review control.

## The Threads list

One row per thread, in one white card (T1, T2).

**Filters**, in a row above the list: Run (Review on only), Type, Contact, and "Clear filters".

- **Run** lists the historical and scheduled runs of the compared methods (clef-masked, clef-masked-reasoned, jev-masked), each named "{method} · {month}", for example "clef-masked · January 2026". Scorecard reruns and the runs of removed methods never appear. A run fixes its method, so there is no method filter. With Review on and no run chosen, it holds the newest run it lists.
- **Type** is the Interactions tab's segmented Type control, from the same source-to-type mapping: All, Messages, Email, Events. Messages covers every chat app (WhatsApp, Telegram, iMessage, Google Chat), Email covers email threads, and Events covers calendar meetings and anarlog sessions. The Interactions tab's own Type control reads All, Messages, Email, Events, Calls, Other: its Calendar option is renamed Events, and anarlog sessions move there from Other (decision 20261019T205343Z-afc9197af058).
- **Contact** narrows to threads the contact takes part in.

**Order:** by the thread's latest message, newest first, with Review on or off. Turning Review on never reorders the list. With Review on and a run chosen, the list holds the threads that run judged, in that same order.

**Summary line** (Review on only), between the filters and the rows, gray-50: "{n} threads judged in this run · {n} to review · {n} with nothing flagged · {n} reviewed" on the left, "Newest message first, as with Review off" on the right.

**A row**, left to right:

| Column | Holds |
|---|---|
| Venue | the app's chip (WhatsApp, Telegram, iMessage, Google Chat, Email, Calendar, Anarlog), plus a purple "Group" chip for a group thread |
| Thread | the title in semibold where the thread has one, and every participant contact beneath, each a link to the contact |
| Latest | the time of the thread's latest message |
| Count | Review off: the thread's message count ("1,204 messages", "1 meeting note"). Review on: "{n} judged in this run" |
| Status | Review on only: a pill (below) |
| Open | a chevron link to the thread page |

**Status pills:** "{n} to review" (amber-50, amber-300 border, amber-800), "Nothing flagged" (gray-100, gray-300 border, gray-700), "Reviewed" (green-50, green-300 border, green-800). A thread reads "Reviewed" once every night holds one of the user's labels. Otherwise, a thread that shows at least one flag, clearing decision or reply item reads "{n} to review", n counting its shown items without a label plus each night that shows nothing and still awaits Confirm Nones. A thread that shows nothing at all and is not yet confirmed reads "Nothing flagged".

Paging is the shipped "Page {n} of {m} ({total} threads)" with Previous and Next.

## The thread page

A thread page has its own URL, carrying the list's filters so the pager walks the list as filtered.

**Navigation bar** (the contact page's `ContactNavigationBar`): a gray-100 bar with "← Back to list" on the left and, on the right, the Review switch, then "‹ {n} of {total} ›". The left- and right-arrow keys move to the previous and next thread.

**Header**, white, beneath the bar: the thread's title (22 px bold), its venue chip and Group chip, and its participants, each a link. With Review on, the header adds the status pill and " · {method} · {month} run" after the participants, and a Done button (outline) on the right. The bar and the header stay at the top while the page scrolls.

### Review off (T4)

One white card holding the thread's messages in order, each as the bench draws messages: "{sender} · {time}" on one line in 12 px gray, the sender semibold (gray-700, or blue-700 for "Me"), then the text at 15 px. A collapsed "Earlier messages" toggle sits at the top.

### Review on (T3A)

One white card holding the thread as **one timeline**, each message once, in order:

1. A collapsed toggle at the top: "{n} earlier messages, not read by this run" (spec NG6: first to cut).
2. For each night the run judged:
   - **Divider.** A thin gray-200 line across the card with a small centered label: "**{night}** · {n} new messages" (12 px, the night in semibold gray-700). The first judged night covers every message in its window, so its label can span days ("Jan 2–3").
   - **The night's new messages.** Each carries its flag chips and a "+ Flag" control on a row beneath its text (below).
   - **The judgment box** closes the night (below).
3. A collapsed toggle at the bottom: "{n} later messages, after this run's last night".

No other line marks windows. Where a later night's window starts mid-thread is stated by its judgment box's heading.

### Flag chips

A flag shows on its message as a chip: the kind's icon, the kind's name, its probability, its state, and its controls. Kinds:

| Kind | Name | Icon color |
|---|---|---|
| commitment | Commitment | blue-700 |
| big_moment | Big moment | pink-800, the flag icon |
| claim | Claim | teal-700 |

| State | Look | Controls |
|---|---|---|
| To review (a shown flag with no label) | amber-50, dashed amber-700 border, "To review" amber-800 | confirm (green-800 check) and remove (gray-700 ✕) |
| Confirmed | green-50, green-300 border, "Confirmed" green-800 | "Change" link |
| Removed | gray-50, gray-200 border, the name struck through in gray-500, "Removed" gray-600 | "Change" link |
| Added (the user flagged a message the method did not) | as Confirmed, reading "Added", with no probability | "Change" link |

A commitment chip whose commitment a later night judged done also reads "cleared {date}" in green-800.

Every new message has a "+ Flag" outline button after its chips. It opens a menu of the three kinds, offering each kind the message does not already show; choosing one adds that flag's label.

### The judgment box

A box with a blue-50 tint (#f3f8ff), a blue-300 border, 10 px radius and a faint shadow, set apart from the white timeline. Light blue-100 lines separate its sections.

- **Heading:** "Judgment · read {first message's time} to {cut-off}", where the first time is a link that scrolls to that message and highlights it. The status ("{n} to review") sits at the heading's right while anything in the box or the night's chips has no label.
- **Replies:** "I owe a reply" and "Owes me a reply" side by side, as the shipped bench draws them, items and "Add" included. Each list's Add offers every participant the list holds no item for, whatever the participant's label, so adding a participant whose item sits in the other list moves it, marked "Added by you". An empty list reads "None · closest: {name} {n}%" as today.
- **Commitments re-checked** (only when the night shows clearing decisions): one item per clearing decision of a commitment that shows as a chip and that the user has not rejected, "Commitment from {date, time} · judged done" or "· judged not done", with a "Show in thread ↑" link to the commitment's message, its probability, and two controls, "Done" and "Not done", each labeling the decision with its own answer. A labeled item reads "Marked done" or "Marked not done" with Change, which offers the same two controls. The commitment's text is not repeated.
- **Closest unflagged:** one gray line, "Closest unflagged:" then, per kind, "{Kind} {sender}, {time} ↑ {n}%", each linking to its message, or "{Kind} none above 5%". Display only.
- **Footer:** "Confirm Nones" (outline, green-800 check) while the night shows no flag, no clearing and no reply item and has no label. After a close, the footer reads "{n} unflagged new messages labeled none when you moved on" in green-800.

## Labeling and closing

- A **close** is Done or Next (the pager's › or the right-arrow key). Back to list, ‹ and leaving the page are not closes.
- A close labels "no", for all three kinds, every unflagged flag decision without a label among the new messages of each night that holds at least one of the user's labels. A label on a chip, a reply item or a clearing decision the night shows counts. A shown flag without a label stays to review, and a close never labels a flag decision that any compared method's judgment shows as a flag without a label; removed methods' judgments never hold a close back.
- A night that shows nothing gets its label from Confirm Nones; a close alone gives it none.
- A thread is **Reviewed** when every night in it holds at least one of the user's labels.
- No save waits before the user's next click (spec SC9): saves are recorded in order, and a close takes effect after every save made on the page is recorded.

T5 draws a thread step by step. Step 1: the user confirmed the commitment and the claim, removed the big moment and added a claim to "we finally adopted a dog". Step 2, reopened after Next: every night's footer reports the messages labeled none, and the status reads Reviewed.

## Wording

| Where | Text |
|---|---|
| Chip states | To review, Confirmed, Removed, Added |
| Cleared commitment | cleared {date} |
| Clearing section | Commitments re-checked; judged done / judged not done; Done, Not done; Marked done / Marked not done |
| Near misses | Closest unflagged |
| List status | {n} to review, Nothing flagged, Reviewed |
| Night divider | {night} · {n} new messages |
| Judgment heading | Judgment · read {time} to {cut-off} |
