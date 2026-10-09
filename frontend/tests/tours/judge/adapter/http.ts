// OpenAI-compatible-HTTP judge (the policy hedge — Venice or a metered key).
// Interface stub behind the same `Judge` seam: it builds the identical prompt +
// output schema and POSTs to a chat/completions endpoint. Config is env-only and
// it throws if unconfigured — it is never the merge-gate default (design D2).

import { buildPrompt, OUTPUT_SCHEMA, parseVerdicts } from './prompt'
import { judgeFailureVerdicts } from './types'
import type { Judge, JudgeInput, PerItemVerdict } from './types'

export interface HttpJudgeOptions {
  url?: string
  model?: string
  apiKey?: string
  fetchImpl?: typeof fetch
}

export function makeHttpJudge(opts: HttpJudgeOptions = {}): Judge {
  const url = opts.url ?? process.env.QA_JUDGE_HTTP_URL
  const model = opts.model ?? process.env.QA_JUDGE_HTTP_MODEL ?? 'gpt-4o-mini'
  const apiKey = opts.apiKey ?? process.env.QA_JUDGE_HTTP_KEY ?? ''
  const fetchImpl = opts.fetchImpl ?? fetch

  return async (input: JudgeInput): Promise<PerItemVerdict[]> => {
    if (!url) {
      throw new Error(
        'QA_JUDGE_HTTP_URL is not set — the HTTP judge is an interface stub (see .ai/spec/2026-07-19-codex-sdk-judge-transport.md)'
      )
    }
    // This adapter posts text only — it cannot attach image files, so the
    // prompt must keep the aria-only visual framing even when the caller
    // resolved screenshots (else the model is told images exist that it
    // cannot see, licensing false visual grounding).
    const prompt = buildPrompt({ ...input, images: undefined })
    let content: string | undefined
    let error: string | undefined
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
      }
      content = body.choices?.[0]?.message?.content
    } catch (err) {
      error = err instanceof Error ? err.message : String(err)
    }

    // Errors, tool use and omitted verdicts abstain instead of fabricating a fail.
    let verdicts: PerItemVerdict[]
    if (error || content === undefined) {
      verdicts = judgeFailureVerdicts(input, `judge error: ${error ?? 'no content'}`)
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
            judgeError: true,
          }
      )
    }

    return verdicts
  }
}
