// Prompt protocol + verdict schema (design D4). Evidence is presented in STABLE
// labeled blocks in a FIXED order (position-bias mitigation): SPEC, URL, ARIA,
// API, SERVER_TIME, DIALOGS. The judge grades each residual then-item
// separately → pass|fail|unsure + a required citation. Output is
// schema-constrained; categorical only (no scores/Likert). Zero-shot (the
// shot-insertion seam is `fewShotBlock`, currently empty).

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

// The intent-pass preamble: the judge grades ONE experience goal over several
// CAPTURE[n] sections instead of then-items over one merged evidence bundle.
const INTENT_PREAMBLE = [
  'You are a read-only UX experience JUDGE. You are criticism, not agency: reason',
  'ONLY over the labeled evidence blocks below. Do NOT use any tool, run any',
  'command, browse, or fetch — a run that calls a tool is DISCARDED.',
  '',
  'Under INTENT is one experience GOAL the surface exists to achieve. Under',
  'CAPTURE[0..n] are independent captured states of that surface (accessibility',
  'tree + recorded API responses). Judge whether the captured surface, taken as',
  'a whole, ACHIEVES the goal — not whether individual elements exist, but',
  'whether a user in these states would actually get the experience the goal',
  'names. Return ONE categorical verdict for item [0]:',
  '  - pass   : the evidence shows the goal is achieved in the captured states',
  '  - fail   : the evidence shows the goal is violated or undermined',
  '  - unsure : the evidence is insufficient to judge the goal (abstention)',
  'GROUNDING RULE: a `fail` MUST cite the capture index AND the exact aria node',
  'label or JSON path that undermines the goal (e.g. "CAPTURE[2]: <cite>") in',
  '`citation`. An uncited fail is treated as unsure. Categorical only — no scores.',
].join('\n')

// Visual framing switches on whether screenshots are attached: aria-only runs
// must not fail goals on unobservable visual qualities; image-carrying runs may.
const INTENT_ARIA_ONLY_CAUTION = [
  'The aria tree carries no visual styling — do not fail a goal for purely visual',
  'qualities (size, color, spacing) you cannot observe; abstain instead.',
].join('\n')

const INTENT_IMAGES_NOTE = [
  'Screenshots of the captured states are attached in CAPTURE[n] order — visual',
  'qualities (layout, hierarchy, spacing, color, readability) MAY ground your',
  'verdict; cite the capture index for visual observations too.',
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

// The shot-insertion seam (deferred — the prompt is zero-shot today; few-shot
// examples would come from human critiques confirmed in the Langfuse annotation
// queue).
export function fewShotBlock(): string {
  return ''
}

export function buildPrompt(input: JudgeInput): string {
  if (input.intent) return buildIntentPrompt(input)

  const spec = [
    `BEHAVIOR ${input.behaviorId}: ${input.behaviorTitle}`,
    `GIVEN: ${input.given}`,
    `WHEN: ${input.when}`,
    'THEN:',
    ...input.then.map((t, i) => `  [${i}] ${t}`),
  ].join('\n')

  const items = input.items.map(i => `  [${i.itemIndex}] ${i.thenText}`).join('\n')

  // Per-capture sections (when present) keep distinct captured states — e.g.
  // an in-flight redirect vs the settled page — distinguishable; the merged
  // bundle is the fallback for section-less inputs (older corpus paths).
  const sections =
    input.captureSections && input.captureSections.length > 0
      ? input.captureSections.map((s, n) =>
          block(`CAPTURE[${n}] — ${s.note}`, renderEvidence(s.evidence))
        )
      : [renderEvidence(input.evidence)]

  const sectionNotes: string[] = []
  if (input.captureSections && input.captureSections.length > 0) {
    sectionNotes.push(
      'Evidence is presented as one CAPTURE[n] section per captured state, in tour order — cite the capture index alongside the node/path.'
    )
  }
  if ((input.images?.length ?? 0) > 0) {
    sectionNotes.push(
      'Screenshots of the captured states are attached in CAPTURE[n] order — visual qualities MAY ground your verdict.'
    )
  }

  const parts = [
    SYSTEM_PREAMBLE,
    ...sectionNotes,
    fewShotBlock(),
    block('SPEC', spec),
    ...sections,
    block('ITEMS', items),
    'Return ONLY JSON matching the required schema: { "verdicts": [ { "item_index", "verdict", "citation", "critique" } ] }.',
  ].filter(p => p.trim() !== '')

  return parts.join('\n\n')
}

// The intent-pass prompt: INTENT block + one CAPTURE[n] section per bound
// capture (same labeled-block protocol inside each section), fixed order.
function buildIntentPrompt(input: JudgeInput): string {
  const intent = input.intent
  if (!intent) throw new Error('buildIntentPrompt requires input.intent')
  const head = [
    `INTENT ${input.behaviorId}: ${input.behaviorTitle}`,
    `STATUS: ${intent.status} (current = achieved today, judge as regression; proposed = aspirational, judge as progress)`,
    `STATEMENT: ${intent.statement}`,
  ].join('\n')

  const sections = (input.captureSections ?? []).map((s, n) =>
    block(`CAPTURE[${n}] — ${s.note}`, renderEvidence(s.evidence))
  )

  const parts = [
    INTENT_PREAMBLE,
    (input.images?.length ?? 0) > 0 ? INTENT_IMAGES_NOTE : INTENT_ARIA_ONLY_CAUTION,
    fewShotBlock(),
    block('INTENT', head),
    ...sections,
    block('ITEMS', `  [0] ${intent.statement}`),
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
