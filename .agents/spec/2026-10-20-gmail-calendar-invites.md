# Exclude calendar messages from Gmail ingestion

## Context & problem

Gmail ingestion stores every fetched message that a known contact sent or received, as long as the message is outside Gmail's bulk-mail categories. Calendar invitations, updates, cancellations and accept/decline replies pass that gate: they come from or go to real contacts, and they land in the Primary category. Each one becomes an email interaction, moves the contact's cadence and last-contacted date, and feeds Gmail participant and correspondence discovery. Calendar sync already records the meeting itself as an interaction, so the invite email counts the same meeting a second time and makes a scheduling notice look like correspondence.

## What matters

1. A calendar message never counts as correspondence: no stored email, no interaction, no cadence effect.
2. A calendar message never proposes or strengthens a contact candidate.
3. Real correspondence is untouched, including a reply in an invite's thread that carries no calendar data.

## Intent & appetite (delegation charter)

The artifact is a filter in the Gmail sync provider of a single-user CRM. It runs unattended on every Gmail sync tick and on rematch scans. A miss costs little: one extra email interaction on a contact's timeline. A false positive costs more, because it silently drops real correspondence, so the detection rule stays narrow and exact.

Appetite: one small PR. The filter, the two behavior-spec entries and a focused test are worth building. A configurable filter, a settings toggle, a list of excluded senders, subject heuristics and any UI are not.

Rabbit holes:
- Parsing the iCalendar body (METHOD, organizer, attendees) to distinguish kinds of calendar message. Every kind is dropped, so the presence of calendar data decides alone.
- Detecting invites by subject prefix ("Invitation:", "Accepted:") or by sender (calendar-notification addresses). Both are locale- and client-dependent; the MIME content type is not.
- Excluding calendar messages in the Gmail search query to save the message fetch. Gmail search cannot see an inline calendar part with no filename, so the query alone misses Outlook and Apple invites. A query term on top of the content check is a second rule for one fact.
- Building committed cleanup machinery (an admin command, a migration or a backfill job) for rows already stored. The cleanup is a one-time throwaway script (see Relation to existing & planned work).

Call-me-when triggers:
- The detection rule would drop a message that is not a calendar message, or the implementation needs a second signal beyond calendar content to decide.
- Excluding calendar messages from discovery would change a discovery behavior other than the two this spec names (create candidates and correspondence link evidence).
- The change appears to need a schema migration or a new stored field.

User-reserved decisions:
- Whether to keep any kind of calendar message (the user ruled on 2026-10-20: drop them all, accept/decline replies with a typed note included).
- Whether calendar messages still feed discovery (the user ruled on 2026-10-20: they feed neither storage nor discovery).
- Running the cleanup against production data.

Tradeoff rule: when narrowness and coverage collide, choose narrowness. A calendar message that slips through is cheaper than real mail that disappears.

## Goals / Non-Goals

Goals:
- A Gmail message that carries calendar data is neither stored nor counted as an interaction, on every scan path that stores Gmail messages (ING-046).
- The same message feeds no Gmail discovery (IMP-050).
- A message with no calendar data behaves exactly as before (ING-046.non-calendar-reply-stored).

Non-Goals:
- No change to calendar sync, which keeps recording meetings from Google Calendar.
- No change to the Gmail search query or category filter.
- No user-facing setting, UI or configuration for the filter.
- No committed cleanup of rows stored before the change.

## Relation to existing & planned work

- Builds on Gmail integration (`.agents/spec/2026-06-01-gmail-integration-design.md`), whose category exclusion this narrows further, and on Gmail participant discovery (`.agents/spec/2026-08-11-gmail-participant-discovery.md`, IMP-042 to IMP-049), which gains one more exclusion.
- Must not break IMP-042 to IMP-049 or any existing Gmail storage behavior for messages without calendar data.
- One-time cleanup, outside this spec's PR: after the change is deployed, the coordinator writes a throwaway script, kept out of the repository, that removes the calendar emails already stored and the interactions derived from them. The user reviews the script and approves its production run. Its design follows from how derived state (interactions, cadence, last-contacted) recomputes, which is investigated then.

## Hard constraints

- A message is a calendar message when any MIME part, at any depth, inline or attachment, has the content type `text/calendar`. No other signal decides.
- The rule applies identically on every path that fetches and processes Gmail messages: scheduled sync and identifier rematch scans.
- Spec maintenance rule: the PR flips ING-046 and IMP-050 from `proposed` to `current` and passes `make spec-lint` and `make spec-coverage`.

## Success criteria

- After a sync over a mailbox holding an invitation, an update, a cancellation and an accept reply from a known contact, the contact has no email row and no interaction from any of them, and no candidate gains evidence from them.
- A plain reply in the invitation's thread, with no calendar part, is stored and recorded as before.

## Desired behavior sketch

The user's timeline for a contact shows the meeting once, from calendar sync, and no "Invitation: …" or "Accepted: …" email beside it. The contact's last-contacted date moves only for real correspondence and real meetings.

## Assumptions & deferred questions

- Assumed: Google Calendar, Outlook and Apple Calendar all send a `text/calendar` part with every invitation, update, cancellation and reply. Messages from a client that sends only an `.ics` attachment typed otherwise slip through, which the tradeoff rule accepts.
- Assumed: a forwarded message that carries an invite's calendar part is a calendar message and is dropped.

## Engines

```json engines
{
  "authorFamily": "claude",
  "arcPlanner": "claude-opus-5-5:high",
  "arcReview": "gpt-6.1-sol:high"
}
```

The user ruled on 2026-10-20 that the PR is built by one direct implementer on Opus and reviewed on gpt-6.1-sol. The coordinator writes the arc itself, on its own engine.
