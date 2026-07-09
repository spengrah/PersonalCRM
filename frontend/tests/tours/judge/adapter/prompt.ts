// Prompt protocol + verdict schema (design D4). Evidence is presented in STABLE
// labeled blocks in a FIXED order (position-bias mitigation): SPEC, URL, ARIA,
// API, SERVER_TIME, DIALOGS. The judge grades each residual then-item
// separately → pass|fail|unsure + a required citation. Output is
// schema-constrained; categorical only (no scores/Likert). Zero-shot (the
// shot-insertion seam is `fewShotBlock`, empty in PR2).

import type { AriaChild, AriaNode } from '../../support/types'
import type { EvidenceBlocks, JudgeInput, PerItemVerdict } from './types'

// The JSON schema handed to `codex exec --output-schema` (and an
// OpenAI-compatible json_schema response_format).
export const OUTPUT_SCHEMA = {
  type: 'object',
  additionalProperties: false,
  required: ['verdicts'],
  properties: {
    verdicts: {
      type: 'array',
      items: {
        type: 'object',
        additionalProperties: false,
        required: ['item_index', 'verdict', 'citation', 'critique'],
        properties: {
          item_index: { type: 'integer' },
          verdict: { type: 'string', enum: ['pass', 'fail', 'unsure'] },
          citation: { type: 'string' },
          critique: { type: 'string' },
        },
      },
    },
  },
} as const

const SYSTEM_PREAMBLE = [
  'You are a read-only UX behavior JUDGE. You are criticism, not agency: reason',
  'ONLY over the labeled evidence blocks below. Do NOT use any tool, run any',
  'command, browse, or fetch — a run that calls a tool is DISCARDED.',
  '',
  'For EACH then-item listed under ITEMS, return a categorical verdict:',
  '  - pass   : the evidence clearly shows the behavior holds',
  '  - fail   : the evidence clearly shows the behavior is violated',
  '  - unsure : the evidence is absent or ambiguous (abstention)',
  'GROUNDING RULE: a `fail` MUST cite the exact aria node label or JSON path in',
  '`citation`. An uncited fail is treated as unsure. Categorical only — no scores.',
].join('\n')

// Render the normalized aria tree as a stable indented outline.
export function renderAria(node: AriaNode, indent = 0): string {
  const pad = '  '.repeat(indent)
  const lines: string[] = []
  if (node.role !== 'root') {
    const state: string[] = []
    if (node.disabled) state.push('disabled')
    if (node.checked !== undefined) state.push(`checked=${node.checked}`)
    if (node.pressed !== undefined) state.push(`pressed=${node.pressed}`)
    if (node.selected) state.push('selected')
    if (node.active) state.push('active')
    if (node.expanded) state.push('expanded')
    if (node.level !== undefined) state.push(`level=${node.level}`)
    const label =
      node.name !== undefined ? ` "${node.name}"` : node.text !== undefined ? `: ${node.text}` : ''
    const tokens = state.length > 0 ? ` [${state.join(', ')}]` : ''
    lines.push(`${pad}- ${node.role}${label}${tokens}`)
  }
  const childIndent = node.role === 'root' ? indent : indent + 1
  for (const child of node.children ?? []) {
    if (isAriaNode(child)) lines.push(renderAria(child, childIndent))
    else lines.push(`${'  '.repeat(childIndent)}- … (${child.__ariaTruncated__} more)`)
  }
  return lines.join('\n')
}

function isAriaNode(child: AriaChild): child is AriaNode {
  return (child as AriaNode).role !== undefined
}

function block(label: string, body: string): string {
  return `=== ${label} ===\n${body}`
}

function renderEvidence(ev: EvidenceBlocks): string {
  const blocks: string[] = []
  if (ev.url !== undefined) blocks.push(block('URL', ev.url))
  if (ev.aria !== undefined) blocks.push(block('ARIA', renderAria(ev.aria)))
  if (ev.api !== undefined) blocks.push(block('API', JSON.stringify(ev.api, null, 2)))
  if (ev.serverTime !== undefined)
    blocks.push(block('SERVER_TIME', JSON.stringify(ev.serverTime, null, 2)))
  if (ev.dialogs !== undefined && ev.dialogs.length > 0) {
    blocks.push(block('DIALOGS', JSON.stringify(ev.dialogs, null, 2)))
  }
  return blocks.join('\n\n')
}

// The shot-insertion seam (deferred — PR2 ships a zero-shot prompt; few-shot
// examples come from promoted human critiques the labeling CLI produces).
export function fewShotBlock(): string {
  return ''
}

export function buildPrompt(input: JudgeInput): string {
  const spec = [
    `BEHAVIOR ${input.behaviorId}: ${input.behaviorTitle}`,
    `GIVEN: ${input.given}`,
    `WHEN: ${input.when}`,
    'THEN:',
    ...input.then.map((t, i) => `  [${i}] ${t}`),
  ].join('\n')

  const items = input.items.map(i => `  [${i.itemIndex}] ${i.thenText}`).join('\n')

  const parts = [
    SYSTEM_PREAMBLE,
    fewShotBlock(),
    block('SPEC', spec),
    renderEvidence(input.evidence),
    block('ITEMS', items),
    'Return ONLY JSON matching the required schema: { "verdicts": [ { "item_index", "verdict", "citation", "critique" } ] }.',
  ].filter(p => p.trim() !== '')

  return parts.join('\n\n')
}

// The stable labeled-block order the prompt emits (for tests / documentation).
export const BLOCK_ORDER = [
  'SPEC',
  'URL',
  'ARIA',
  'API',
  'SERVER_TIME',
  'DIALOGS',
  'ITEMS',
] as const

// Parse a schema-constrained model message into PerItemVerdict[]. Tolerant:
// unknown verdicts coerce to `unsure`; missing citation/critique default to ''.
export function parseVerdicts(raw: string): PerItemVerdict[] {
  let parsed: unknown
  try {
    parsed = JSON.parse(raw)
  } catch {
    // The message may wrap the JSON in prose/fences — extract the first object.
    const match = raw.match(/\{[\s\S]*\}/)
    if (!match) return []
    try {
      parsed = JSON.parse(match[0])
    } catch {
      return []
    }
  }
  const verdicts = (parsed as { verdicts?: unknown }).verdicts
  if (!Array.isArray(verdicts)) return []
  const out: PerItemVerdict[] = []
  for (const v of verdicts) {
    if (typeof v !== 'object' || v === null) continue
    const rec = v as Record<string, unknown>
    const idx = Number(rec.item_index)
    if (!Number.isInteger(idx)) continue
    const verdict = rec.verdict === 'pass' || rec.verdict === 'fail' ? rec.verdict : 'unsure'
    out.push({
      itemIndex: idx,
      verdict,
      citation: typeof rec.citation === 'string' ? rec.citation : '',
      critique: typeof rec.critique === 'string' ? rec.critique : '',
    })
  }
  return out
}
