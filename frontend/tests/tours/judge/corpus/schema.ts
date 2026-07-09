// Corpus case schema + validator (design D6). A case is a git-diffable YAML:
// captures (fixture refs) + expected per-then-item verdicts + a clean|doctored
// source. A doctored case references a base clean case + a single-point mutation
// the doctoring tool applies deterministically at eval time.
//
// Pure zod over an already-parsed object — the vitest tests build objects
// directly (no YAML parse), so this module has no bun/node runtime dependency.
// The YAML file loading lives in load.ts (bun runtime).

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
