# Masking fixes

Make the masker hide names without destroying ordinary words, and hide more of the names it misses today, so the decision-model methods read legible text and fewer names reach the vendors. The January review (2026-10-21) showed the methods reading text such as "I’NAME_2 meet you guys" while real names still went out. This spec also lets a historical run cover a range of days, for re-judging January's first week. Quoted email history and reaction messages are handled in a separate change (see Relation to existing & planned work); the flag definitions and question wording are another (decision 20261021T163917Z-77e06cd925bb).

## Context & problem

The January review read the masked text of the 122 messages jev-masked flagged and the user labeled (2026-10-21), and spikes on prod data measured the masker the same day (read-only).

**Masking over-masks ordinary words.** The masker's dictionary holds every word of two or more letters of every name of every contact, address-book names included, matched case-insensitively. Ten contacts carry the word "will", and "me", "it", "is", "or", "as", "he" and "ll" are name words too, so each is replaced everywhere. The tokenizer splits on apostrophes, so "I'll" becomes "I" plus the masked "ll". A capitalized word mid-sentence is masked as an unknown name, so "Happy New Year" becomes "Happy NAME_1 Year" and meeting notes become mostly placeholders. A prototype that kept apostrophes inside words, matched a common English word to a name only when capitalized, and exempted common words from the unknown-name rule cut the placeholders in those 122 messages from 575 to 242, and the text read normally. Keeping apostrophes inside words broke possessives: "Emily's" no longer matched the name, so a possessive suffix must stay separable.

**Masking under-masks names it has never seen.** Names that appear in no contact record pass through: a contact's partner or child named in passing, the user's own first name at the start of a sentence, a name in an email signature or quoted sender line, and a name written in lower case. Some of these appear elsewhere as a display name in a sender or recipient field.

**A historical run covers a whole month.** `crm-admin --judge-month` judges every cut-off of a month. Re-judging January to measure these fixes and the new flag definitions needs only its first week.

## Terms

- **Placeholder:** a masked token (NAME_n, EMAIL_n, PHONE_n, URL_n, HANDLE_n).
- **Known name:** a name the CRM holds: a contact's name, the user's own name, or a display name seen in a sender or recipient field of a stored message.
- **Review set:** the 122 messages jev-masked flagged in its January scorecard rerun that the user labeled (selected by the read-only query in the 2026-10-21 session).

## What matters

1. No known name reaches a vendor.
2. The methods read each message's own words, legibly.

## Intent & appetite (delegation charter)

**What it is for:** the first-week January re-judge that will measure the new flag definitions. Its precision is meaningless while the input is garbled and leaky.

**What failure costs:** a name sent to a vendor is a privacy breach that cannot be undone. Masking that eats ordinary words costs precision on every question.

**Appetite:** one PR, or two if the date-range run stands apart.

**Rabbit holes (do not enter):**

1. A local name-recognition model. Measure what the dictionary fixes leave, and stop.
2. Quoted email history and reactions: the separate change owns them.
3. Flag definitions and question wording.

**Call-me-when triggers:**

- The changes leave more known names unmasked than today's masker on the review set.
- A common-word list that keeps ordinary words also unmasks capitalized names that are words ("Will", "Mark", "Rose").

**User-reserved decisions:** what counts as a name to hide (G2 is the user's ruling, 2026-10-21); the first production method; flag definitions and wording.

**Tradeoff rules for a delegate:** when hiding a name and keeping a word compete, hide the name.

**Unattended running:** the arc may run unattended, deciding questions by citing this spec and logging each decision with the passage it cites.

## Goals

- **G1 Masking keeps ordinary words.** Contractions stay whole, a possessive suffix stays separable from the name before it, a common English word matches a name only when capitalized, and common words are never masked as unknown names. The common-word list is a generated or vendored artifact with a stated source.
- **G2 Masking knows more names.** The dictionary adds the user's own names (G5) as ME and every display name seen in a sender or recipient field of any stored message (user ruling, 2026-10-21).
- **G3 New method versions.** Each method gets a new version when the masker changes, so a rerun judges again instead of reusing judgments made on the old masking.
- **G4 A day range.** `crm-admin --judge-month YYYY-MM` is replaced by `--judge-start-day YYYY-MM-DD --judge-end-day YYYY-MM-DD` (user ruling, 2026-10-21): a historical run judges the nightly cut-offs that close each day of the range, inclusive, with `--method` and `--measure` as today. `--judge-month` is removed, not kept beside it; the one caller outside the repository, a script on the Pi, is updated by hand.
- **G5 The user's names in configuration.** The user has no contact record, so the user's own names come from an environment setting, with no UI (user ruling, 2026-10-21).

## Non-Goals

- **NG1** No change to quoted email handling or reactions.
- **NG2** No change to the questions, their wording or the flag definitions.
- **NG3** No name-recognition model.

## Relation to existing & planned work

- A separate change (another session, 2026-10-21) removes quoted email history and marks reaction messages. Measured masking results improve once it lands, since quoted sender lines and signatures carry many unseen names; neither change depends on the other.
- The commitment definition (decision 20261021T163917Z-77e06cd925bb) ships separately as new wording with its own method version. With these fixes and the quoted-history change, it makes the first-week January re-judge.
- The masker's guarantees are in `backend/internal/threadctx/mask.go`; HC3 of `.agents/spec/2026-10-18-message-flags-and-threads.md` (no content at rest off the Pi; content-free Langfuse) still holds.

## Hard constraints

- **HC1** No masking change lets a name through that today's masker hides.
- **HC2** No content at rest off the Pi.

## Success criteria

- **SC1** Masking the review set yields far fewer placeholders than today's masker, no more surviving known names, and "I'll", "it", "is", "will" and "Happy New Year" intact where they are not names.
- **SC2** A capitalized name that is also a common word ("Will") is masked mid-sentence, and a possessive of a known name ("Emily's") is masked.
- **SC3** The user's own first name is masked as ME, and a name seen only as an email display name is masked.
- **SC4** A rerun after the change judges again rather than reusing a judgment made on the old masking.
- **SC5** `--judge-start-day 2026-01-01 --judge-end-day 2026-01-07` judges the cut-offs that close those seven days and no others, and `--judge-month` is refused as an unknown flag.

## Assumptions & deferred questions

**Deferred to the arc:**

- **D1 The common-word list's source and size.** Choose a list that keeps names that are also words masked when capitalized; measure it on the review set.

## Engines

```json engines
{ "authorFamily": "claude", "arcPlanner": "claude-opus-5-5:high", "arcReview": "gpt-6.1-sol:high" }
```

Quota is in surplus on both subscriptions, so planners, contract reviews, approval gates and single-implementer builds use no cheaper engine unless it is the best fit, and prefer Opus 5.5 over Fable.
