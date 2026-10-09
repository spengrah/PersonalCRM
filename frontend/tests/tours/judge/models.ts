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
// (e.g. the intent pass passes a stronger model). Codex on a ChatGPT account
// rejects models it no longer serves with a 400 ("not supported when using Codex
// with a ChatGPT account") — gpt-5.4-mini went that way — and newer models need a
// matching Codex CLI. Probe a candidate with `codex exec -m <model>` under the qa
// tenant's login before changing this.
export const DEFAULT_JUDGE_MODEL = 'gpt-6-luna'
export const DEFAULT_JUDGE_EFFORT = 'low'

// Intent judgment is the semantically hard task and the call count is small
// (~one per intent per run), so it runs at a higher effort than the item judge.
// gpt-6-luna is the model too: a 2026-10-09 replay of one round's evidence
// (5 repeats per model) found it as stable as gpt-5.5 and gpt-6-sol, in
// majority agreement with gpt-6-sol, and stricter than gpt-5.5 on real gaps, at
// roughly 1/45 of gpt-5.5's cost. Overridable via QA_INTENT_MODEL /
// QA_INTENT_EFFORT; QA_JUDGE still selects the adapter kind.
export const DEFAULT_INTENT_MODEL = 'gpt-6-luna'
export const DEFAULT_INTENT_EFFORT = 'medium'
