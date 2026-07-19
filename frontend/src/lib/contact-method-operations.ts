/**
 * Contact-method operation derivation, and the two client structures it runs
 * between.
 *
 * THE INVARIANT THAT MATTERS HERE. The client keeps two pictures of the same
 * confirmed truth — the acknowledged state (this module) and the live
 * react-hook-form rows (`contact-form.tsx`). Three consecutive review rounds
 * each found the same defect: a path that updated one without the other. So the
 * places server data can enter them are enumerated, and the list is short:
 *
 *   1. EDIT-START — `contact.methods` seeds `initializeAcknowledgedMethods` and
 *      the form's `defaultValues` at the same moment from the same source.
 *      Both, together, by construction.
 *   2. RESULT SNAPSHOTS — `advanceAcknowledgedMethods` applies every
 *      snapshot-bearing result; `reconciliationsFromResults` forwards the same
 *      ones to the live rows. They agree only because every non-`remove`
 *      operation reports the submitted row it addresses. A `remove` reports -1
 *      and carries no snapshot, so both structures drop that row.
 *
 * Everything else server-originated — `setQueryData` on the detail cache after
 * either mutation, `usePrefetchContact`, `RematchJobWatcher`'s invalidation,
 * any ordinary refetch — reaches NEITHER structure, deliberately. Those feed
 * display only. That is what makes the acknowledged state safe to hold across a
 * cache refresh, and it is the whole defense against the original incident.
 *
 * The checkable form of rule 2 has two halves, and the first alone is NOT
 * sufficient — stating it as if it were is how this doc originally overstated
 * the guarantee:
 *
 *   a. `submittedIndexes[i] < 0` if and only if `operations[i].op === 'remove'`.
 *   b. every other index identifies the row the operation actually addresses —
 *      an `add` points at the submitted row carrying its own type and value; an
 *      `update`, `set_primary`, or `clear_primary` points at the row carrying
 *      its `method_id`.
 *
 * (a) alone would be satisfied by mapping every non-removal to index 0, which
 * reconciles one row's snapshot into a different row — the same
 * acknowledged-vs-live divergence, reached by a new route. Both halves are
 * pinned by "the submitted-index contract" test, which is the class-level
 * guard: any operation type added later that forgets to thread its row index,
 * or threads the wrong one, fails without anyone having to rediscover this
 * reasoning.
 */
import { normalizeContactMethodValue } from '@/lib/contact-methods'
import type { ContactMethod, ContactMethodType } from '@/types/contact'
import type {
  ContactMethodOperation,
  ContactMethodOperationResult,
} from '@/types/generated/contact'

/**
 * A method this client asserted and had confirmed on the server.
 *
 * The acknowledged state is the left-hand side of every derivation. It is
 * initialized once at edit-start and thereafter advances ONLY by applying this
 * client's own confirmed operations — never by absorbing a server response.
 * Both rules are load-bearing in opposite directions: re-reading server data
 * lets a method the form never showed enter a diff (and be destroyed), while
 * freezing it for the whole edit session silently discards a value the user
 * reverts after a partially successful save.
 *
 * `value` is held in the SUBMISSION representation (see
 * `normalizeContactMethodValue`) so the diff can be a plain equality against
 * what the form submits. It is deliberately NOT the comparison/uniqueness
 * representation: that answers "would these collide?", and the diff asks "did
 * the user change this?". Answering the second question with the first makes
 * case-only and respelling edits vanish.
 */
export interface AcknowledgedMethod {
  method_id: string
  type: ContactMethodType
  value: string
  is_primary: boolean
}

/** A method row as the form submits it. A row with no `method_id` is new. */
export interface SubmittedMethod {
  method_id?: string
  type: ContactMethodType
  value: string
  is_primary: boolean
}

export interface DerivedMethodOperations {
  operations: ContactMethodOperation[]
  /**
   * `submittedIndexes[i]` is the index into `submitted` whose live row the
   * operation addresses, or -1 when no live row corresponds — which is only
   * `remove`, whose row is by definition absent from the submitted set.
   *
   * Primary designations DO carry their row's index: their results include a
   * full snapshot, and that snapshot has to reach the live row or the form and
   * the acknowledged state drift apart.
   *
   * This is what lets a successful result be written back into the live form
   * row that produced it, without re-deriving server identity from the
   * response.
   */
  submittedIndexes: number[]
}

/** A confirmed result, resolved back to the submitted row that produced it. */
export interface MethodReconciliation {
  submittedIndex: number
  method_id: string
  type: ContactMethodType
  value: string
  is_primary: boolean
}

/**
 * Builds the acknowledged state at edit-start from the contact's methods.
 *
 * Values are normalized into the submission representation here, which is what
 * makes a stored `@foo` (displayed `@foo`, submitted `foo`) produce no
 * operation on an untouched round trip.
 */
export function initializeAcknowledgedMethods(
  methods: ContactMethod[] | undefined
): AcknowledgedMethod[] {
  return (methods ?? [])
    .filter((method): method is ContactMethod & { id: string } => Boolean(method.id))
    .map(method => ({
      method_id: method.id,
      type: method.type,
      value: normalizeContactMethodValue(method.type, method.value),
      is_primary: Boolean(method.is_primary),
    }))
}

/**
 * Derives the operations that carry the acknowledged state to what the form is
 * asking for.
 *
 * A method absent from the acknowledged state is never named by any operation,
 * which is the structural reason a save cannot remove a method the form did not
 * show.
 */
export function deriveMethodOperations(
  acknowledged: AcknowledgedMethod[],
  submitted: SubmittedMethod[]
): ContactMethodOperation[] {
  return deriveMethodOperationsWithOrigins(acknowledged, submitted).operations
}

export function deriveMethodOperationsWithOrigins(
  acknowledged: AcknowledgedMethod[],
  submitted: SubmittedMethod[]
): DerivedMethodOperations {
  const acknowledgedById = new Map(acknowledged.map(method => [method.method_id, method]))

  // An id the acknowledged state does not know is not a row this client can
  // safely name: naming it would assert an identity we never confirmed. Treat
  // the row as new instead — the server resolves it to whichever row satisfies
  // the value, and tells us which one.
  const knownId = (row: SubmittedMethod) =>
    row.method_id && acknowledgedById.has(row.method_id) ? row.method_id : undefined

  const submittedIds = new Set<string>()
  for (const row of submitted) {
    const id = knownId(row)
    if (id) submittedIds.add(id)
  }

  const operations: ContactMethodOperation[] = []
  const submittedIndexes: number[] = []
  const push = (operation: ContactMethodOperation, submittedIndex: number) => {
    operations.push(operation)
    submittedIndexes.push(submittedIndex)
  }

  const removedIds = new Set<string>()
  for (const method of acknowledged) {
    if (submittedIds.has(method.method_id)) continue
    removedIds.add(method.method_id)
    push({ op: 'remove', method_id: method.method_id }, -1)
  }

  submitted.forEach((row, index) => {
    const id = knownId(row)
    if (!id) {
      const add: ContactMethodOperation = { op: 'add', type: row.type, value: row.value }
      // A row that does not exist yet has no id for set_primary to name, so a
      // new row's designation necessarily travels on its own add.
      if (row.is_primary) add.is_primary = true
      push(add, index)
      return
    }
    const acknowledgedRow = acknowledgedById.get(id)
    if (!acknowledgedRow) return
    if (acknowledgedRow.type !== row.type || acknowledgedRow.value !== row.value) {
      push({ op: 'update', method_id: id, type: row.type, value: row.value }, index)
    }
  })

  const acknowledgedPrimary = acknowledged.find(method => method.is_primary)
  const submittedPrimaryIndex = submitted.findIndex(row => row.is_primary)
  const submittedPrimary = submittedPrimaryIndex >= 0 ? submitted[submittedPrimaryIndex] : undefined

  // A primary designation is reported with the live row it addresses, NOT -1.
  // Its result carries a full snapshot (the row survives), and that snapshot is
  // the only place the client learns the row's server-side state. Dropping it
  // from reconciliation while the acknowledged state applies it splits the two
  // structures apart: the form keeps showing a stale value while the
  // acknowledged state holds the server's, and the next derivation then emits
  // an update nobody asked for, overwriting a value the form never displayed.
  if (submittedPrimary) {
    const id = knownId(submittedPrimary)
    if (id && acknowledgedPrimary?.method_id !== id) {
      push({ op: 'set_primary', method_id: id }, submittedPrimaryIndex)
    }
  } else if (acknowledgedPrimary && !removedIds.has(acknowledgedPrimary.method_id)) {
    // Suppressed when the same row is being removed: removal already leaves no
    // primary, so the clear is redundant AND rejected as self-conflicting.
    // The row is still submitted (it was not removed), so its live index is
    // known and the snapshot can reach it.
    const clearedIndex = submitted.findIndex(row => knownId(row) === acknowledgedPrimary.method_id)
    push({ op: 'clear_primary', method_id: acknowledgedPrimary.method_id }, clearedIndex)
  }

  return { operations, submittedIndexes }
}

/**
 * Advances the acknowledged state by applying this client's own confirmed
 * operations.
 *
 * The response's `methods` array is deliberately never read here. The test is
 * "did MY operation address this row?", not "did the server send it?" — taking
 * the list wholesale is how state the client never saw enters its picture,
 * which is the original defect. A per-result snapshot cannot do that: a result
 * only ever arrives for an operation this client submitted.
 *
 * Upserting by `method_id` is what satisfies the same-id collapse property: two
 * rows whose results resolve to one id become one row, at the earlier position,
 * carrying the resolved snapshot rather than either submitted value.
 */
export function advanceAcknowledgedMethods(
  acknowledged: AcknowledgedMethod[],
  operations: ContactMethodOperation[],
  results: ContactMethodOperationResult[]
): AcknowledgedMethod[] {
  let next = acknowledged.map(method => ({ ...method }))
  let promotedId: string | null = null

  for (const result of results) {
    const operation = operations[result.index]
    if (!operation) continue

    if (operation.op === 'remove') {
      next = next.filter(method => method.method_id !== result.method_id)
      continue
    }

    // No snapshot means the operation left no surviving row to learn about.
    if (!result.method) continue

    const snapshot: AcknowledgedMethod = {
      method_id: result.method.id,
      type: result.method.type,
      value: normalizeContactMethodValue(result.method.type, result.method.value),
      is_primary: result.method.is_primary,
    }
    if (snapshot.is_primary) promotedId = snapshot.method_id

    const at = next.findIndex(method => method.method_id === snapshot.method_id)
    if (at >= 0) next[at] = snapshot
    else next.push(snapshot)
  }

  // A promotion demotes every other row. Rows no operation addressed keep their
  // edit-start flag otherwise, and would leave the state with two primaries —
  // which makes the next derivation's primary comparison ambiguous.
  if (promotedId) {
    next = next.map(method => ({ ...method, is_primary: method.method_id === promotedId }))
  }

  return next
}

/**
 * Resolves confirmed results back to the submitted rows that produced them, so
 * the live form rows can learn the ids and stored values the server assigned.
 *
 * Without this the acknowledged state and the form disagree: a row added in a
 * save whose later step failed would still be id-less in the form, and the next
 * save would derive `remove` + `add` — changing the row's identity, which is
 * the forensic evidence this whole workstream exists to protect.
 */
export function reconciliationsFromResults(
  operations: ContactMethodOperation[],
  results: ContactMethodOperationResult[],
  submittedIndexes: number[]
): MethodReconciliation[] {
  const out: MethodReconciliation[] = []
  for (const result of results) {
    const operation = operations[result.index]
    if (!operation || operation.op === 'remove' || !result.method) continue
    const submittedIndex = submittedIndexes[result.index] ?? -1
    if (submittedIndex < 0) continue
    out.push({
      submittedIndex,
      method_id: result.method.id,
      type: result.method.type,
      value: result.method.value,
      is_primary: result.method.is_primary,
    })
  }
  return out
}
