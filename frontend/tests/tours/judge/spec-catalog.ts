// The 20 current first-cut `ux` behaviors, transcribed verbatim from the
// behavior SSOT: spec/contacts.yaml (7), spec/dashboard.yaml (6), and
// spec/cadence-followup.yaml (7). Used to build judge prompts and the advisory
// report. The then-item order/count MUST match the classification map, and the
// ID set MUST equal the current-ux first-cut set — both guarded by unit tests.

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

  // --- dashboard (spec/dashboard.yaml) ---
  'DSH-001': {
    id: 'DSH-001',
    title: "The dashboard is the application's default landing surface",
    given: 'the application root is opened',
    when: 'the entry route resolves',
    then: [
      'the user is taken to the dashboard as the default destination',
      'the redirect does not present as a broken or blank surface while it resolves',
    ],
  },
  'DSH-002': {
    id: 'DSH-002',
    title: 'Every primary surface is reachable from a persistent global navigation',
    given: 'any primary application surface, including the dashboard',
    when: 'the global navigation renders',
    then: [
      'at wide (>= sm) viewports, links to the dashboard, contacts, birthdays, imports, and settings sections are present',
      'the link matching the current section is visually marked active',
      'the navigation bar stays visible while the page scrolls (sticky)',
    ],
  },
  'DSH-003': {
    id: 'DSH-003',
    title: 'The dashboard always offers a path to add or browse contacts',
    given: 'the dashboard is open',
    when: 'the surface renders in any state',
    then: [
      'an add-contact action is always available from the header',
      'the caught-up state additionally offers a path to add a contact and to view the full contact list',
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
  'DSH-005': {
    id: 'DSH-005',
    title: 'The overdue widget reflects overdue-membership changes made in other flows',
    given: 'an open dashboard whose overdue list has been fetched',
    when: 'an action elsewhere changes who is overdue',
    then: [
      'the overdue list refreshes to reflect the change without a manual page reload',
      "this covers logging an interaction, merging contacts, and resolving a meeting note that touches a contact's cadence",
      'purely cosmetic contact edits that do not change overdue membership do not disturb the list',
      'returning focus to the window re-checks freshness, but refetches only once the list has gone stale (a 5-minute staleTime); refocusing sooner does not refetch',
    ],
  },
  'DSH-007': {
    id: 'DSH-007',
    title: "Search is the contact list's search; the dashboard exposes no separate global search",
    given: 'a user wanting to find a contact by text',
    when: 'they look for a search affordance',
    then: [
      "contact text search is provided through the contact list's search input",
      'no dedicated dashboard-level or app-global search surface exists (no top-bar search box or command palette)',
    ],
  },

  // --- cadence-followup surfaces (spec/cadence-followup.yaml) ---
  'CAD-026': {
    id: 'CAD-026',
    title: 'The dashboard is an action-required list of overdue contacts',
    given: 'contacts past their next-contact date',
    when: 'the dashboard renders',
    then: [
      'overdue contacts appear as cards with the count in the header',
      'each card shows urgency (tiers at up to 2, up to 7, and more days overdue), cadence, how long since last contact, reachable contact methods, and the suggested action',
      'with nothing overdue, an all-caught-up state is shown instead',
    ],
  },
  'CAD-027': {
    id: 'CAD-027',
    title: 'The overdue list offers urgency, name, and recency orderings',
    given: 'an overdue list with several contacts',
    when: 'the user switches the sort',
    then: [
      'urgency (default) orders most-overdue first',
      'name orders alphabetically',
      'last-contacted orders oldest contact first, with never-contacted at the end',
    ],
  },
  'CAD-028': {
    id: 'CAD-028',
    title: 'Marking contacted from the dashboard clears the contact immediately',
    given: 'an overdue contact card',
    when: 'the user marks the contact as contacted',
    then: [
      "a mutual interaction is logged, timestamped by the server's accelerated clock",
      'the contact leaves the overdue list without a page reload, and the count updates',
      'the change is consistent across dashboard, contact list, and contact detail',
    ],
  },
  'CAD-029': {
    id: 'CAD-029',
    title: 'Recent activity summarizes the direction timestamps and pending reply',
    given: 'a contact detail page',
    when: 'the recent-activity summary renders',
    then: [
      'the last outreach time is shown when one exists',
      'the last response time is shown when one exists',
      'an awaiting-reply indicator is shown while a follow-up is pending',
      'with none of these, an explicit no-recent-activity state is shown',
    ],
  },
  'CAD-030': {
    id: 'CAD-030',
    title: 'The tasks section shows live work first and history on demand',
    given: 'a contact with follow-up, manual, and completed tasks',
    when: 'the tasks section renders',
    then: [
      'managed follow-up tasks appear first with a distinct pending indicator, then live manual tasks',
      'each task carries a badge derived from its kind and lifecycle',
      'completed tasks are collapsed behind a toggle with a count',
      'with no tasks, an empty state invites adding one',
    ],
  },
  'CAD-031': {
    id: 'CAD-031',
    title: 'Users can add manual tasks of three kinds',
    given: 'the add-task flow on a contact',
    when: 'the user creates a task',
    then: [
      'the kind is chosen from reach-out, send, and reminder',
      'task text is required; notes are optional',
      "the created task appears in the contact's live tasks",
    ],
  },
  'CAD-033': {
    id: 'CAD-033',
    title: 'Unlinking is the only in-CRM mutation of a linked task',
    given: 'a task linked to a contact',
    when: 'the user manages it from the CRM',
    then: [
      'the CRM offers unlink (with confirmation), which keeps the task alive in the remote app',
      'completing and dismissing happen in the remote task app, not the CRM',
    ],
  },
}

export function behaviorSpec(id: string): BehaviorSpec | undefined {
  return SPEC_CATALOG[id]
}
