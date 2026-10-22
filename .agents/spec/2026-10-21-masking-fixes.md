# Masking fixes

Make the masker hide names without destroying ordinary words, and hide more of the names it misses today, so the decision-model methods read legible text and fewer names reach the vendors. The January review (2026-10-21) showed the methods reading text such as "I’NAME_2 meet you guys" while real names still went out. A bakeoff on the same messages (2026-10-21) then measured name-recognition models against the dictionary, and the user chose to add one (amendment of 2026-10-21, below). This spec also lets a historical run cover a range of days, for re-judging January's first week. Quoted email history and reaction messages are handled in a separate change (see Relation to existing & planned work); the flag definitions and question wording are another (decision 20261021T163917Z-77e06cd925bb).

## Context & problem

The January review read the masked text of the 122 messages jev-masked flagged and the user labeled (2026-10-21), and spikes on prod data measured the masker the same day (read-only).

**Masking over-masks ordinary words.** The masker's dictionary holds every word of two or more letters of every name of every contact, address-book names included, matched case-insensitively. Ten contacts carry the word "will", and "me", "it", "is", "or", "as", "he" and "ll" are name words too, so each is replaced everywhere. The tokenizer splits on apostrophes, so "I'll" becomes "I" plus the masked "ll". A capitalized word mid-sentence is masked as an unknown name, so "Happy New Year" becomes "Happy NAME_1 Year" and meeting notes become mostly placeholders. A prototype that kept apostrophes inside words, matched a common English word to a name only when capitalized, and exempted common words from the unknown-name rule cut the placeholders in those 122 messages from 575 to 242, and the text read normally. Keeping apostrophes inside words broke possessives: "Emily's" no longer matched the name, so a possessive suffix must stay separable.

**Masking under-masks names it has never seen.** Names that appear in no contact record pass through: a contact's partner or child named in passing, the user's own first name at the start of a sentence, a name in an email signature or quoted sender line, and a name written in lower case. Some of these appear elsewhere as a display name in a sender or recipient field.

**A name-recognition model finds the names the dictionary misses.** A bakeoff on the Pi (2026-10-21; results and the user's labels kept outside the repository) layered three models on the dictionary and scored them against the user's labels of 80 person names, 27 other proper nouns and 343 ordinary words in the review set's 113 distinct messages. With GLiNER (`urchade/gliner_multi_pii-v1`, label `person`) at a score of 0.5 or more, the dictionary plus the model masked all 80 names, against 73 for the dictionary plus the capitalized-unknown-word rule, and masked 5 other proper nouns against 20. Adding the capitalized-unknown-word rule to the model masked no further name. spaCy and the OpenAI Privacy Filter each left names in three or four messages. GLiNER peaked at 2.8 GB and took about 0.55 s per message at its median on the Pi with two threads; the Pi has about 5.4 GB available with the CRM running.

**A historical run covers a whole month.** `crm-admin --judge-month` judges every cut-off of a month. Re-judging January to measure these fixes and the new flag definitions needs only its first week.

## Terms

- **Placeholder:** a masked token (NAME_n, EMAIL_n, PHONE_n, URL_n, HANDLE_n).
- **Known name:** a name the CRM holds: a contact's name, the user's own name, or a display name seen in a sender or recipient field of a stored message.
- **Name spans:** the character ranges of a message's text the model reports as a person, at or above the threshold.
- **Review set:** the 122 messages jev-masked flagged in its January scorecard rerun that the user labeled (selected by the read-only query in the 2026-10-21 session).

## What matters

1. No known name reaches a vendor.
2. The methods read each message's own words, legibly.

## Intent & appetite (delegation charter)

**What it is for:** the first-week January re-judge that will measure the new flag definitions. Its precision is meaningless while the input is garbled and leaky.

**What failure costs:** a name sent to a vendor is a privacy breach that cannot be undone. Masking that eats ordinary words costs precision on every question.

**Appetite:** one arc of two or three PRs: the dictionary and date-range changes, and the model with its span cache, with the model's container standing apart if its deployment needs it.

**Rabbit holes (do not enter):**

1. Running the model inside Go (an ONNX port of GLiNER's tokenizer and span decoding), another model, or a model service that stays loaded between uses.
2. Quoted email history and reactions: the separate change owns them.
3. Flag definitions and question wording.

**Call-me-when triggers:**

- The changes leave more known names unmasked than today's masker on the review set.
- A common-word list that keeps ordinary words also unmasks capitalized names that are words ("Will", "Mark", "Rose").
- The model container needs more memory than its cap on the review set, or running it disturbs the CRM's other containers.
- On the review set, the dictionary plus the model leaves a labeled name unmasked.

**User-reserved decisions:** what counts as a name to hide (G2 is the user's ruling, 2026-10-21); the model and its threshold (G6 is the user's ruling, 2026-10-21); the first production method; flag definitions and wording.

**Tradeoff rules for a delegate:** when hiding a name and keeping a word compete, hide the name.

**Unattended running:** the arc may run unattended, deciding questions by citing this spec and logging each decision with the passage it cites.

## Goals

- **G1 Masking keeps ordinary words.** Contractions stay whole, a possessive suffix stays separable from the name before it, and a common English word matches a known name only when capitalized. The masker has no rule that masks a word for being capitalized and unknown: the model finds unknown names (G6). The common-word list is a generated or vendored artifact with a stated source.
- **G2 Masking knows more names.** The dictionary adds the user's own names (G5) as ME and every display name seen in a sender or recipient field of any stored message (user ruling, 2026-10-21).
- **G3 New method versions.** Each method gets a new version when the masker changes, so a rerun judges again instead of reusing judgments made on the old masking.
- **G4 A day range.** `crm-admin --judge-month YYYY-MM` is replaced by `--judge-start-day YYYY-MM-DD --judge-end-day YYYY-MM-DD` (user ruling, 2026-10-21): a historical run judges the nightly cut-offs that close each day of the range, inclusive, with `--method` and `--measure` as today. `--judge-month` is removed, not kept beside it; the one caller outside the repository, a script on the Pi, is updated by hand.
- **G5 The user's names in configuration.** The user has no contact record, so the user's own names come from the environment setting `OWN_NAMES`, with no UI (user ruling, 2026-10-21). The setting is general, not decision-specific: other features may read the user's names.
- **G6 A model finds the names the dictionary misses.** GLiNER `urchade/gliner_multi_pii-v1`, label `person`, at a threshold of 0.5 (user ruling, 2026-10-21: both 0.5 and 0.6 masked every labeled name in the bakeoff, 0.6 was chosen on that same sample, and the tradeoff rule favors hiding the name). The threshold is one setting. The masker masks the union of the dictionary's matches and the message's name spans, and every masked range takes its placeholder from the one per-request placeholder map. A name span follows G1 as a dictionary match does: a span whose every word is a common English word written in lower case is not masked (user ruling, 2026-10-22, decision 20261022T021159Z-25b0429017f3: on the review set the model masked lower-case common words that refer to people, and the rule cut masked ordinary words from 150 to 118 with every labeled name still masked).
- **G7 One path to name spans, cached.** Before masking, the masker ensures every entry has name spans, asking the model for those that lack them, and the spans are stored per message, keyed by the message, its text and the model version (model, label and threshold). Masking is the only caller, since only masking reads spans: a historical or nightly run fills the spans its contexts lack, with no separate backfill and no ingestion step (one path is the user's ruling, 2026-10-21). Go is the only writer of the stored spans.
- **G8 No spans, no send.** If any entry of a request lacks name spans after the masker asked for them, the request is not sent to a vendor, and the run reports the failure.
- **G9 The model runs on demand in its own container.** The model runs in a Python container on the Pi that takes text and returns name spans and nothing else: it reads and writes no database. It starts when the backend first asks it, runs under a memory cap, and exits when idle, so its memory is held only while a run needs it (user ruling, 2026-10-21). Its weights are in its image, and it makes no network call at run time.

## Non-Goals

- **NG1** No change to quoted email handling or reactions.
- **NG2** No change to the questions, their wording or the flag definitions.
- **NG3** No second model, no model inside the Go binary, and no model service that stays loaded between uses.

## Relation to existing & planned work

- A separate change (2026-10-21, landed with #964 and #968) removes quoted email history at ingestion and marks reaction messages, both backfilled. It left the review set's 113 message texts unchanged (checked 2026-10-21), so the bakeoff labels still apply.
- The commitment definition (decision 20261021T163917Z-77e06cd925bb) ships separately as new wording with its own method version. With these fixes and the quoted-history change, it makes the first-week January re-judge.
- The masker's guarantees are in `backend/internal/threadctx/mask.go`; HC3 of `.agents/spec/2026-10-18-message-flags-and-threads.md` (no content at rest off the Pi; content-free Langfuse) still holds.

## Hard constraints

- **HC1** No masking change lets a name through that today's masker hides, except a common English word written in lower case that is also a name ("talked to will"), which G1 lets through (user ruling, 2026-10-21: common names are among the least identifying).
- **HC2** No content at rest off the Pi.
- **HC3** No message text reaches a vendor unmasked by the model: a request with an entry the model has not covered is not sent (G8).

## Success criteria

- **SC1** Masking the review set yields far fewer placeholders than today's masker, no more surviving known names, and "I'll", "it", "is", "will" and "Happy New Year" intact where they are not names.
- **SC2** A capitalized name that is also a common word ("Will") is masked mid-sentence, and a possessive of a known name ("Emily's") is masked.
- **SC3** The user's own first name is masked as ME, and a name seen only as an email display name is masked.
- **SC4** A rerun after the change judges again rather than reusing a judgment made on the old masking.
- **SC5** `--judge-start-day 2026-01-01 --judge-end-day 2026-01-07` judges the cut-offs that close those seven days and no others, and `--judge-month` is refused as an unknown flag.
- **SC6** On the review set, the dictionary plus the model masks every name the user labeled in the bakeoff, and masks fewer other proper nouns than the capitalized-unknown-word rule did.
- **SC7** A request whose entries the model cannot cover (the model container down or refusing) is not sent, and a rerun after the container recovers sends it.
- **SC8** Masking the review set keeps the model container's peak memory under its cap, and the container exits after a run goes idle.

## Assumptions & deferred questions

**Deferred to the arc:**

- **D1 The common-word list's source and size.** Choose a list that keeps names that are also words masked when capitalized; measure it on the review set.
- **D2 How the container starts on demand and how the backend reaches it.** Choose the mechanism that keeps the Python container a function of text and lets the backend learn it is down; measure its start time and memory on the Pi.

## Engines

```json engines
{ "authorFamily": "claude", "arcPlanner": "claude-opus-5-5:high", "arcReview": "gpt-6.1-sol:high" }
```

Quota is in surplus on both subscriptions, so planners, contract reviews, approval gates and single-implementer builds use no cheaper engine unless it is the best fit, and prefer Opus 5.5 over Fable.
