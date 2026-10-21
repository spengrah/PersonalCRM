# Message reactions carry their facts

## Context & problem

An iMessage reaction ("tapback") reaches the CRM as an ordinary text message. The Mac daemon reads `Liked "…"`, `Loved "…"` or `Reacted 👍 to "…"` from chat.db as the message body and sends it as a text message, so nothing stored tells a reaction apart from something the person wrote. The daemon never reads `associated_message_type`, the chat.db column that says a row is a reaction, which kind it is, and whether it adds or removes one.

The decision model, which another session is reworking, reads every stored message as conversation. It cannot skip a reaction for the questions where a reaction is noise, and it cannot use one where a reaction means something: when someone reacts to the user's message, they often no longer owe a reply. That work needs the facts stored; this spec supplies them.

The other message sources store no reactions. WhatsApp drops them at classification (WHA-027); Telegram and Google Chat never read them. The user rules WHA-027's reasoning ("reactions are not turns") not quite right, since reactions inform ball-in-court, but bringing in reactions from those sources is outside this arc's appetite.

A separate correction rides along. Core rule 2 ("Never write raw SQL in Go … helper scripts") was read on 2026-10-21 as covering a one-time cleanup script, which turned a requested throwaway into a permanent `crm-admin` subcommand. The user rules the rule does not reach scripts outside the repo.

## What matters

1. Every iMessage reaction the daemon reads is stored with its facts on the reaction's own message.
2. Those facts are source-neutral, so a later arc can fill them from another source without reshaping them.
3. The decision model can read the facts wherever it reads messages.
4. Reactions already stored gain the same facts.
5. Core rule 2 covers only code committed to the repository.

## Intent & appetite (delegation charter)

**What this is for.** A data foundation for the decision-model work in another session: that work decides how judgments treat reactions, and this arc only records what a reaction is. Nothing the user sees changes. The backfill runs once, by the user, from the Mac.

**Appetite.** Small: one arc that adds the facts at capture, stores them, exposes them to the decision model's read, backfills stored rows once, and narrows one rule's wording. It is not a reaction feature.

**Rabbit holes.**
- Changing reply, outreach, interaction or cadence logic because reactions are now visible. A tapback keeps creating and extending interactions and keeps its existing reply link exactly as today.
- Teaching the decision model anything about reactions, including filtering them in its prompts or scorecard.
- Capturing reactions from WhatsApp, Telegram or Google Chat, or revisiting WHA-027.
- Hardening against reaction kinds Apple has not shipped: an unrecognized reaction type arriving unmarked, as it does today, is acceptable until someone notices.
- Chasing every stored tapback: one the backfill cannot match to its chat.db record stays unmarked.
- Any UI.

**Call me when.**
- Recording a reaction's facts appears to require changing how a reaction counts as a message, a reply or an interaction.
- The facts cannot be stored source-neutrally without a shape another source could not fill.
- The backfill would need to rewrite anything on a stored row other than its reaction facts.
- chat.db on the deployed macOS stores reactions in a way the kinds below cannot describe.

**User-reserved decisions.**
- Running the backfill against prod, and promoting to prod.
- Any change to what a reaction does downstream (replies, outreach, interactions, judgments).
- Adding a reaction kind beyond those named here.

When priorities collide, a fact recorded correctly for the reactions this arc names beats coverage of rarer forms.

## Goals / Non-Goals

**Goals**
- G1. The daemon recognizes each iMessage reaction it reads and sends its facts with the message.
- G2. Ingest stores the facts on the reaction's own message, in source-neutral form.
- G3. The decision model's message read carries the facts beside each message.
- G4. A one-time operator backfill gives already-stored iMessage reactions the same facts.
- G5. Core rule 2 is reworded to cover only code committed to the repository and to say nothing about scripts outside it.

**Non-Goals**
- No change to how a reaction affects replies, outreach, interactions, cadence or the reply link it carries today.
- No change to the decision model's questions, prompts, scorecard or bench.
- No reaction capture from WhatsApp, Telegram or Google Chat.
- No UI change.
- No rule about where ad hoc scripts live or how they reach the database; whether a script may run is the user's permission setting, not a repository rule.

## Relation to existing & planned work

- Decision-model work in another session (`.agents/spec/2026-10-14-decision-model-extraction.md`, `.agents/spec/2026-10-18-message-flags-and-threads.md` and later): this arc's consumer. It defined the facts below; this arc must not change any judgment.
- `.agents/spec/2026-09-15-awaiting-reply-derivation-and-cadence-skip.md`: ball-in-court; the target key is what lets later work say "they reacted to my message".
- `.agents/spec/mac-daemon.md`: defers reactions to a v2 "mutation scan". This arc reads reactions in the existing message reader instead, as rows the reader already sends.
- MAC-058 and its re-scan script (`mac-daemon/Scripts/rescan-messages`): the operator re-scan this arc's backfill extends. It already reads `associated_message_type` but classifies only 2000–2005 and 3000–3005.
- WHA-027: unchanged in this arc; its "not turns" reasoning is noted as questioned for a later arc.
- `.agents/rules/core.md` rule 2: reworded (G5).

## Hard constraints

- A reaction's facts are its **kind**, its **target message key**, and whether it is a **removal**. A message is a reaction exactly when it has a kind; no separate is-reaction flag.
- Kinds: love, like, dislike, laugh, emphasize, question, emoji, sticker. The emoji kind also records the emoji.
- The target message key is chat.db's `associated_message_guid` with its part prefix (`p:<n>/`) or `bp:` prefix removed, so it equals the target message's stored key.
- A removal is a 3000-range type; it carries the same kind and target as the reaction it removes.
- The facts' names and values are source-neutral: nothing in them names iMessage, and another source could fill them as they are.
- A reaction row keeps everything it stores today: body, type, reply link, interaction effect.
- A daemon that predates this arc keeps working against the new backend: a message sent without facts is stored as today.

## Architectural direction

- **constraint** (user ruling, from the decision-model session): the facts live on the reaction message itself, not in a separate table keyed to it.
- **constraint**: the decision model reads them through the read it already uses for message content, beside each message, so it needs no second query.

## Success criteria

- SC1. After a daemon on this arc's build sends a love, like, dislike, laugh, emphasize, question, emoji or sticker reaction, the stored message carries that kind, the emoji for the emoji kind, the target's key without prefix, and removal false; the removal of each carries removal true.
- SC2. A message that is not a reaction stores no kind, no target and no removal.
- SC3. A reaction's interaction, reply and cadence effects are the same as before this arc, for an inbound and an outbound reaction, one-to-one and in a group.
- SC4. The decision model's message read returns each message's reaction facts.
- SC5. After the operator backfill, a stored iMessage reaction whose chat.db record the Mac still holds carries the same facts SC1 names; rerunning it changes nothing; nothing else on any row changes.
- SC6. Core rule 2's text no longer reaches scripts outside the repository.

**Negative behavior**
- N1. An unrecognized reaction type is stored as today, with no kind.
- N2. A message sent by an older daemon, without facts, is stored as today.
- N3. No judgment, flag, clearing, ball-in-court answer or scorecard number changes because of this arc.

## Desired behavior sketch

None user-visible. A reaction looks and behaves exactly as before; its stored message now says what kind of reaction it is, which message it targets, and whether it takes a reaction back.

## Assumptions & deferred questions

- Assumed: the deployed macOS stores the emoji of an emoji reaction (type 2006) in chat.db where the reader can read it; planning verifies the column against the deployed schema.
- Sticker reactions arrive as type 1000 with a target message (a sticker placed on a message, the older form, with no removal type) or as types 2007 and 3007; a counts-only read of one Mac's chat.db on 2026-10-21 found only the type-1000 form (ruling R1 in the arc).
- Left to planning: which message tables and view carry the facts, the migration, the daemon payload field names, and how the backfill lists and writes rows (the re-scan's Pi exchange or a new one).
- No question is deferred to the arc: every open point above is settled by reading the code or chat.db.

## Engines

```json engines
{ "authorFamily": "claude", "arcPlanner": "claude-opus-5-5:high", "arcReview": "gpt-6.1-sol:high" }
```

The arc planner chooses every other engine from the catalog. Quota is in surplus on both subscriptions, so planners, reviews and approval gates use no cheaper engine unless it is genuinely the best fit, and prefer Opus 5.5 over Fable.
