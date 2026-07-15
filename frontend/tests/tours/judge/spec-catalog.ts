// The judge residue's behaviors, transcribed verbatim from the behavior SSOT
// (spec/contacts.yaml, spec/dashboard.yaml). The full verifier catalog migrated
// to Playwright E2E; only the two behaviors carrying a judge-tagged then-item
// remain (CON-042[0], DSH-004[2]). Used to build judge prompts and the advisory
// report. Each survivor keeps its FULL then array (the judge prompt shows every
// clause for context). The ID set is guarded by spec-catalog.test.ts.

export interface BehaviorSpec {
  id: string
  title: string
  given: string
  when: string
  then: string[]
}

export const SPEC_CATALOG: Record<string, BehaviorSpec> = {
  'CON-042': {
    id: 'CON-042',
    title: 'Deleting a contact requires explicit confirmation',
    given: 'a contact detail page',
    when: 'the user asks to delete the contact',
    then: [
      'a confirmation prompt warns the action cannot be undone',
      'only on confirmation is the contact deleted',
      'on success the user is returned to the contact list',
    ],
  },
  'DSH-004': {
    id: 'DSH-004',
    title: 'The overdue widget distinguishes loading and error states from its content',
    given: 'the overdue widget is loading or its request has failed',
    when: 'the dashboard renders',
    then: [
      'while loading, placeholder content is shown rather than an empty or caught-up state',
      'on request failure, an error state with a failure reason is shown rather than an empty or caught-up state',
      'the shown failure reason faithfully reflects the actual failure',
    ],
  },
}

export function behaviorSpec(id: string): BehaviorSpec | undefined {
  return SPEC_CATALOG[id]
}
