// Corpus case schema + validator (design D6). A case is a git-diffable JSON file:
// captures (fixture refs) + expected per-then-item verdicts + a clean|doctored
// source. A doctored case references a base clean case + a single-point mutation
// the doctoring tool applies deterministically when a doctored case is resolved.
//
// Pure zod over an already-parsed object — the vitest tests build objects
// directly (no parse needed), so this module has no runtime file dependency.
// The JSON file loading lives in load.ts (portable JSON.parse).

import { z } from 'zod'
import { MutationSchema } from '../mutation'

export const VerdictSchema = z.enum(['pass', 'fail', 'unsure'])
export const GraderSchema = z.enum(['verifier', 'judge'])

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

// An intent-pass case. Its expectation is a self-labeled HYPOTHESIS that no
// longer runs anywhere: the corpus eval that once scored it under --judge is
// retired, and label.ts drafts the case but never reads expected_hypothesis. It
// is retained for the deferred human-labeling path (the ground-truth step).
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
