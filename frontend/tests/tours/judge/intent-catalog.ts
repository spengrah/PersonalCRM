// The intent-behavior catalog: the `type: intent` rows of the behavior SSOT
// (spec/{dashboard,contacts,cadence-followup}.yaml), transcribed verbatim like
// SPEC_CATALOG, plus `servedBy` — the corpus-wide INVERSION of the YAML's
// `serves:` edges. The judge binds an intent's evidence to captures tagged with
// the intent's own ID or any behavior in servedBy (see intent-input.ts).
//
// Kept in sync with the YAML by a unit test that parses the spec files and
// asserts ids/titles/statements/status AND the inverted edges match
// (intent-catalog.test.ts) — catalog drift fails offline tests, not a live run.

export type IntentStatus = 'current' | 'proposed'

export interface IntentSpec {
  id: string
  title: string
  statement: string
  status: IntentStatus
  /** Behavior IDs whose `serves:` lists name this intent (inverted edges). */
  servedBy: string[]
}

export const INTENT_CATALOG: Record<string, IntentSpec> = {
  'DSH-010': {
    id: 'DSH-010',
    title: 'The dashboard tells the user who to reach out to, at a glance',
    statement:
      "a user opening the dashboard can decide who to contact next and how to reach them without opening any contact's page — the overdue surface is scannable rather than a wall of undifferentiated entries, and more urgent relationships stand out from less urgent ones",
    status: 'current',
    servedBy: ['CAD-026', 'CAD-027', 'CAD-028'],
  },
  'DSH-011': {
    id: 'DSH-011',
    title: 'The dashboard never dead-ends',
    statement:
      'whatever state the dashboard is in — populated, all caught up, still loading, or failed — the user always has a clear next action available and is never stranded on a blank or ambiguous screen',
    status: 'current',
    servedBy: ['CAD-026', 'DSH-003', 'DSH-004'],
  },
  'DSH-012': {
    id: 'DSH-012',
    title: 'The dashboard reflects reality without manual refreshes',
    statement:
      'any action in the app that changes who is overdue is reflected on an open dashboard without the user reloading the page — the overdue list can be trusted as live',
    status: 'proposed',
    servedBy: ['CAD-028', 'DSH-005', 'DSH-009'],
  },
  'CON-050': {
    id: 'CON-050',
    title: 'Destructive contact actions are deliberate',
    statement:
      'contact data is never lost through a single accidental interaction — destructive flows demand an explicit, informed confirmation, and what will happen is visible before it happens',
    status: 'current',
    servedBy: ['CON-042', 'CON-043'],
  },
  'CON-051': {
    id: 'CON-051',
    title: "Browsing contacts never loses the user's place",
    statement:
      'moving between the contact list and contact detail feels like traversing one consistent, ordered list — sort, search, and filter context carries across navigation in both directions, with keyboard and mouse as equals',
    status: 'current',
    servedBy: ['CON-038', 'CON-040', 'CON-041'],
  },
  'CON-052': {
    id: 'CON-052',
    title: 'The birthdays page is a planning surface, not a data dump',
    statement:
      'the birthdays page answers whose birthday needs attention now and soon — imminent birthdays stand apart from distant ones, celebrated ones recede, and the user never has to compute dates themselves',
    status: 'current',
    servedBy: ['CON-045'],
  },
  'CAD-036': {
    id: 'CAD-036',
    title: 'A contact page answers where the relationship stands',
    statement:
      "a contact's detail page answers where things stand with this person — when we last talked in each direction, whether a reply is pending, and what work is queued — without the user digging through raw history",
    status: 'current',
    servedBy: ['CAD-029', 'CAD-030', 'CAD-031'],
  },
  'CAD-037': {
    id: 'CAD-037',
    title: 'CRM task actions are safe for remote task state',
    statement:
      "managing a linked task from the CRM never silently mutates or destroys the user's task in the remote app — remote state changes only where the user expects them to happen",
    status: 'current',
    servedBy: ['CAD-033'],
  },
}

export function intentSpec(id: string): IntentSpec | undefined {
  return INTENT_CATALOG[id]
}

export function allIntents(): IntentSpec[] {
  return Object.keys(INTENT_CATALOG)
    .sort()
    .map(id => INTENT_CATALOG[id])
}
