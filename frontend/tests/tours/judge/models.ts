// The judge's model + reasoning-effort defaults, in ONE place.
//
// These previously lived next to the transports that consumed them — the ux
// pass's pair in `adapter/codex-exec.ts` (a transport the harness no longer
// runs) and the intent pass's pair in `intent-runner.ts`. The price sync needs
// a single answer to "which models will this run actually send", and reading
// half of that out of a retired adapter is fragile and misleading. Nothing
// re-exports these from the old locations: the point is to remove the
// ambiguity, not relocate it.

// The spec mandates a CHEAP judge ("cheap model judges, stronger model authors
// issues"). Pin a mini-tier model + low reasoning effort as the DEFAULT so the
// judge never silently inherits the operator's codex config (a global
// gpt-5.5 / xhigh default is both costly AND miscalibrating here — over-reasoning
// invents false fails). Overridable via QA_JUDGE_MODEL / QA_JUDGE_EFFORT or opts
// (e.g. the intent pass passes a stronger model). gpt-5.4-mini is the cheapest tier
// the pinned Codex CLI supports on a ChatGPT account; gpt-5.6-luna is cheaper/
// newer but needs a Codex CLI upgrade (revalidate exact models at build time).
export const DEFAULT_JUDGE_MODEL = 'gpt-5.4-mini'
export const DEFAULT_JUDGE_EFFORT = 'low'

// Intent judgment is the semantically hard task and the call count is small
// (~one per intent per run), so it defaults to a stronger model + effort than
// the cheap item-residue judge. Overridable via QA_INTENT_MODEL /
// QA_INTENT_EFFORT; QA_JUDGE still selects the adapter kind.
export const DEFAULT_INTENT_MODEL = 'gpt-5.5'
export const DEFAULT_INTENT_EFFORT = 'medium'

/**
 * The distinct model strings a round will actually send, deduped + sorted.
 *
 * The two passes read DIFFERENT env vars — resolving only QA_JUDGE_MODEL would
 * leave an experiment's intent model unpriced, which is the more expensive of
 * the two. Sorted + deduped so a caller (the price sync) has a stable, minimal
 * target list.
 *
 * NOT included by design: the http adapter's own default (`gpt-4o-mini`). That
 * adapter resolves its model itself from QA_JUDGE_HTTP_MODEL and is a non-codex
 * reference stub no round runs — including it here would price a model nothing
 * sends. This exclusion is a decision, not an oversight.
 */
export function activeModels(env: Record<string, string | undefined> = process.env): string[] {
  // `??`, NOT `||`, and deliberately so: this must resolve EXACTLY as the
  // transports do (they all use `??`). With `||`, an empty override
  // (QA_JUDGE_MODEL=) would resolve to the default here while the transport
  // resolves it to '' — and threadOptionsFor's `if (model)` then sets NO model
  // and lets codex pick. We would faithfully price a model nothing sends.
  // Under `??` an empty override yields the target '', which matches zero
  // upstream entries and is reported LOUDLY by the sync's absence path — the
  // honest outcome for a misconfigured harness.
  const models = [
    env.QA_JUDGE_MODEL ?? DEFAULT_JUDGE_MODEL,
    env.QA_INTENT_MODEL ?? DEFAULT_INTENT_MODEL,
  ]
  return [...new Set(models)].sort()
}
