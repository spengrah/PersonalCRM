// OpenAI-compatible-HTTP judge (the policy hedge — Venice or a metered key).
// Interface stub behind the same `Judge` seam: it builds the identical prompt +
// output schema and POSTs to a chat/completions endpoint. Config is env-only and
// it throws if unconfigured — it is never the merge-gate default (design D2).

import { buildGradedEvidence, buildScenario } from '../label-trace'
import { buildPrompt, OUTPUT_SCHEMA, parseVerdicts } from './prompt'
import { appendSpan, buildGenAiSpan } from './span'
import type { Judge, JudgeInput, PerItemVerdict } from './types'

export interface HttpJudgeOptions {
  url?: string
  model?: string
  apiKey?: string
  tracePath?: string
  fetchImpl?: typeof fetch
}

function allUnsure(input: JudgeInput, critique: string): PerItemVerdict[] {
  return input.items.map(i => ({
    itemIndex: i.itemIndex,
    verdict: 'unsure' as const,
    citation: '',
    critique,
  }))
}

export function makeHttpJudge(opts: HttpJudgeOptions = {}): Judge {
  const url = opts.url ?? process.env.QA_JUDGE_HTTP_URL
  const model = opts.model ?? process.env.QA_JUDGE_HTTP_MODEL ?? 'gpt-4o-mini'
  const apiKey = opts.apiKey ?? process.env.QA_JUDGE_HTTP_KEY ?? ''
  const fetchImpl = opts.fetchImpl ?? fetch
  const tracePath = opts.tracePath ?? process.env.QA_JUDGE_TRACE

  return async (input: JudgeInput): Promise<PerItemVerdict[]> => {
    if (!url) {
      throw new Error(
        'QA_JUDGE_HTTP_URL is not set — the HTTP judge is an interface stub (see judge/DEFERRED.md)'
      )
    }
    // This adapter posts text only — it cannot attach image files, so the
    // prompt must keep the aria-only visual framing even when the caller
    // resolved screenshots (else the model is told images exist that it
    // cannot see, licensing false visual grounding).
    const prompt = buildPrompt({ ...input, images: undefined })
    const start = Date.now()
    let content: string | undefined
    let error: string | undefined
    let usage: { prompt_tokens?: number; completion_tokens?: number } | undefined
    try {
      const resp = await fetchImpl(url, {
        method: 'POST',
        headers: { 'content-type': 'application/json', authorization: `Bearer ${apiKey}` },
        body: JSON.stringify({
          model,
          messages: [{ role: 'user', content: prompt }],
          response_format: {
            type: 'json_schema',
            json_schema: { name: 'verdicts', schema: OUTPUT_SCHEMA },
          },
        }),
      })
      if (!resp.ok) throw new Error(`HTTP judge returned ${resp.status}`)
      const body = (await resp.json()) as {
        choices?: Array<{ message?: { content?: string } }>
        usage?: { prompt_tokens?: number; completion_tokens?: number }
      }
      content = body.choices?.[0]?.message?.content
      usage = body.usage
    } catch (err) {
      error = err instanceof Error ? err.message : String(err)
    }
    const end = Date.now()

    // Normalize the verdicts ONCE, BEFORE the span append (error/no-content →
    // allUnsure, else parse + omitted-item fill) — so the span's
    // `response`/`item_verdicts` are the SAME array returned, not a
    // pre-normalization parse. One source of truth for span + return value.
    let verdicts: PerItemVerdict[]
    if (error || content === undefined) {
      verdicts = allUnsure(input, `judge error: ${error ?? 'no content'}`)
    } else {
      const parsed = parseVerdicts(content)
      const byIndex = new Map(parsed.map(v => [v.itemIndex, v]))
      verdicts = input.items.map(
        i =>
          byIndex.get(i.itemIndex) ?? {
            itemIndex: i.itemIndex,
            verdict: 'unsure',
            citation: '',
            critique: 'no verdict returned',
          }
      )
    }

    if (tracePath) {
      appendSpan(
        tracePath,
        buildGenAiSpan({
          impl: 'http',
          behaviorId: input.behaviorId,
          model,
          startMs: start,
          endMs: end,
          inputTokens: usage?.prompt_tokens,
          outputTokens: usage?.completion_tokens,
          error,
          // This adapter now emits content too (was metrics-only). It is
          // text-only — it cannot attach image files — so it passes NO
          // `screenshots` and its `gradedEvidence` entries carry no screenshot
          // (buildGradedEvidence over an empty `images`), the correct
          // adapter-specific semantics.
          prompt,
          response: JSON.stringify(verdicts),
          scenario: buildScenario(input),
          gradedEvidence: buildGradedEvidence(input, []),
          itemVerdicts: verdicts,
          mutation: input.__trap?.mutation,
        })
      )
    }

    return verdicts
  }
}
