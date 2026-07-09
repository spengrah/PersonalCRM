// The 7 current contacts `ux` behaviors, transcribed verbatim from
// spec/contacts.yaml (the behavior SSOT). Used to build judge prompts and the
// advisory report. The then-item order/count MUST match the classification map
// (guarded by a unit test).

export interface BehaviorSpec {
  id: string
  title: string
  given: string
  when: string
  then: string[]
}

export const SPEC_CATALOG: Record<string, BehaviorSpec> = {
  'CON-038': {
    id: 'CON-038',
    title: 'List and detail navigation share one default ordering',
    given: 'no explicit sort has been chosen',
    when: 'the contact list renders and a contact is opened from it',
    then: [
      'the list defaults to cadence order, most frequent first',
      "the detail page's prev/next navigation uses the same default",
    ],
  },
  'CON-040': {
    id: 'CON-040',
    title: 'Keyboard navigation drives the contact detail page',
    given: 'a contact detail page opened from a list',
    when: 'keyboard input is used outside form fields',
    then: [
      'left/right arrows move to the previous/next contact, disabled at the boundaries',
      'arrows are inert while editing or while focus is in an input',
      'Enter opens edit mode',
      'Escape discards edit mode, or returns to the list (context preserved) when not editing',
    ],
  },
  'CON-041': {
    id: 'CON-041',
    title: 'Action parameters trigger once and are consumed',
    given: 'a detail URL carrying a one-time action parameter (edit or merge)',
    when: 'the page mounts',
    then: [
      'the action runs once (edit mode opens, or the merge modal opens)',
      'the parameter is stripped from the URL so refresh or back-navigation does not re-trigger it',
    ],
  },
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
  'CON-043': {
    id: 'CON-043',
    title: 'The merge flow keeps the current contact and archives the chosen source',
    given: 'a merge started from a contact (detail button or list action)',
    when: 'the merge modal is used',
    then: [
      'the current contact is marked as kept; the user picks a source to archive from a searchable selector that excludes the target',
      'selecting a source loads a preview of what will transfer',
      "conflicting fields (cadence, location, birthday) offer a source/target toggle defaulting to keep the target's value",
      "the merged name is editable, with a quick-fill to adopt the source's name",
      'the merge cannot be submitted before a source is selected, while the preview is loading, or while a merge is in flight',
      'the outcome is reported and auto-dismissed',
    ],
  },
  'CON-044': {
    id: 'CON-044',
    title: 'Mark-as-contacted logs a mutual interaction from the list',
    given: 'a contact row in the list',
    when: 'the user marks it as contacted via the row actions',
    then: ['a mutual-direction interaction is logged, timestamped by the server'],
  },
  'CON-045': {
    id: 'CON-045',
    title: 'The birthdays page groups contacts by proximity',
    given: 'contacts with birthdays, some with a placeholder year',
    when: 'the birthdays page renders',
    then: [
      'contacts are grouped into today, upcoming, and already-celebrated-this-year sections',
      'a gift-planning section for early next year appears only near year end',
      'upcoming birthdays sort soonest-first; celebrated ones sink to the end',
      'placeholder-year birthdays display without an age',
      "the page follows the app's accelerated time",
    ],
  },
}

export function behaviorSpec(id: string): BehaviorSpec | undefined {
  return SPEC_CATALOG[id]
}
