// Corpus case schema + validator (design D6). A case is a git-diffable JSON file:
// captures (fixture refs) + expected per-then-item verdicts + a clean|doctored
// source. A doctored case references a base clean case + a single-point mutation
// the doctoring tool applies deterministically at eval time.
//
// Pure zod over an already-parsed object — the vitest tests build objects
// directly (no parse needed), so this module has no runtime file dependency.
// The JSON file loading lives in load.ts (portable JSON.parse).

import { z } from 'zod'

export const VerdictSchema = z.enum(['pass', 'fail', 'unsure'])
export const GraderSchema = z.enum(['verifier', 'judge'])

// A single-point mutation. Each targets ONE capture in the base case (by pair
// role, else by index) and changes exactly one datum.
const target = {
  role: z.string().optional(),
  captureIndex: z.number().int().nonnegative().optional(),
}

export const MutationSchema = z.discriminatedUnion('op', [
  // Re-inject a query param into a capture's url (e.g. ?action=edit) — CON-041[1].
  z.object({ op: z.literal('inject_query'), ...target, param: z.string(), value: z.string() }),
  // Delete an endpoint group from a capture's apiResponses — CON-044[0].
  z.object({ op: z.literal('delete_endpoint'), ...target, endpoint: z.string() }),
  // Flip an aria node's disabled state — CON-040[0].
  z.object({
    op: z.literal('set_aria_disabled'),
    ...target,
    node_role: z.string(),
    node_name: z.string(),
    value: z.boolean(),
  }),
  // Reorder an ids_only response's data.ids — CON-038[1].
  z.object({
    op: z.literal('reorder_ids'),
    ...target,
    mode: z.enum(['reverse', 'swap-first-two']).default('swap-first-two'),
  }),
  // Blank the native dialog message — CON-042[0] (judge-item; --judge layer only).
  z.object({ op: z.literal('blank_dialog'), ...target }),
  // Remove the first aria node matching role + (name OR text). The single-point
  // fail for an aria-rendered item: drop the "Add Contact" CTA (DSH-003[0]), a
  // nav link (DSH-002[0]), the "Action Required"/overdue card heading (CAD-026),
  // the "Search contacts..." input (DSH-007[0]), the Task-type button (CAD-031[0]),
  // or the "No tasks yet" empty-state text (CAD-030[3]).
  z.object({
    op: z.literal('remove_aria_subtree'),
    ...target,
    node_role: z.string(),
    node_name: z.string(),
  }),
  // Overwrite a value in a capture's fields — the single-point fail for the
  // aria-invisible D2a fields evidence: set overdueLoadingSkeletons: 0 (DSH-004[0]),
  // a wrong overdueCards tierClass/order (CAD-026[1] / CAD-027), navPosition: 'static'
  // (DSH-002[2]).
  z.object({ op: z.literal('set_field'), ...target, field: z.string(), value: z.unknown() }),
  // Overwrite a JSON path in an apiResponses body item (first item under the
  // endpoint key) — the single-point fail for a body-driven item, e.g. clearing
  // a detail contact's last_outreach_at while the aria still shows it (CAD-029).
  z.object({
    op: z.literal('set_json_field'),
    ...target,
    endpoint: z.string(),
    path: z.array(z.string()).min(1),
    value: z.unknown(),
    // Which item in the endpoint group to mutate (default 0). Needed when the
    // meaningful body is not the first entry (e.g. mutating the FINAL 500 of a
    // retried failure bracket to manufacture a stale-reason faithfulness fail).
    itemIndex: z.number().int().nonnegative().optional(),
  }),
])

export type Mutation = z.infer<typeof MutationSchema>

export function parseMutation(obj: unknown): Mutation {
  return MutationSchema.parse(obj)
}

export const ExpectedItemSchema = z.object({
  then_index: z.number().int().nonnegative(),
  grader: GraderSchema,
  verdict: VerdictSchema,
  critique: z.string().optional(),
})

export const CaseSchema = z.object({
  id: z.string().min(1),
  behavior_id: z.string().min(1),
  // Fixture refs relative to corpus/captures/ (a clean case's real captures, or
  // the base captures a doctored case mutates).
  captures: z.array(z.string()).min(1),
  expected: z.array(ExpectedItemSchema).min(1),
  source: z.enum(['clean', 'doctored']),
  doctor: z
    .object({
      base_case: z.string().min(1),
      mutation: MutationSchema,
    })
    .optional(),
  metadata: z.object({ capture_generator_version: z.number().int() }).optional(),
})

export type Case = z.infer<typeof CaseSchema>
export type ExpectedItem = z.infer<typeof ExpectedItemSchema>

// A doctored case MUST carry a doctor spec; a clean case MUST NOT.
export function parseCase(obj: unknown): Case {
  const c = CaseSchema.parse(obj)
  if (c.source === 'doctored' && !c.doctor) {
    throw new Error(`case ${c.id}: source=doctored requires a doctor spec`)
  }
  if (c.source === 'clean' && c.doctor) {
    throw new Error(`case ${c.id}: source=clean must not carry a doctor spec`)
  }
  return c
}

// An intent-pass case. Its expectation is a self-labeled HYPOTHESIS — intent
// verdicts are judge-only, so these run under --judge only and NEVER gate the
// merge (ground truth comes from the deferred human-labeling path).
export const IntentCaseSchema = z.object({
  id: z.string().min(1),
  intent_id: z.string().min(1),
  captures: z.array(z.string()).min(1),
  source: z.enum(['clean', 'doctored']),
  mutation: MutationSchema.optional(),
  expected_hypothesis: VerdictSchema,
  notes: z.string().optional(),
})

export type IntentCase = z.infer<typeof IntentCaseSchema>

export function parseIntentCase(obj: unknown): IntentCase {
  const c = IntentCaseSchema.parse(obj)
  if (c.source === 'doctored' && !c.mutation) {
    throw new Error(`intent case ${c.id}: source=doctored requires a mutation`)
  }
  if (c.source === 'clean' && c.mutation) {
    throw new Error(`intent case ${c.id}: source=clean must not carry a mutation`)
  }
  return c
}
