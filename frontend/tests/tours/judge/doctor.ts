// The deterministic doctoring tool (design D6): a single-point mutation of a
// clean capture manufactures a known fail on exactly one then-item, so doctored
// cases are self-labeled BY CONSTRUCTION and the eval runs end-to-end with zero
// human input. Pure `applyMutation` (a library the eval imports) + a thin
// `bun run` CLI. Byte-stable across runs (JSON clone + deterministic ops).

import type { AriaChild, AriaNode, Capture } from '../support/types'
import type { Mutation } from './corpus/schema'

function clone<T>(v: T): T {
  return JSON.parse(JSON.stringify(v)) as T
}

export function selectIndex(captures: Capture[], m: Mutation): number {
  if (m.captureIndex !== undefined) return m.captureIndex
  if (m.role !== undefined) {
    const i = captures.findIndex(c => c.pair?.role === m.role)
    if (i >= 0) return i
  }
  return 0
}

function isAriaNode(c: AriaChild): c is AriaNode {
  return (c as AriaNode).role !== undefined
}

function setAriaDisabled(node: AriaNode, role: string, name: string, value: boolean): boolean {
  if (node.role === role && node.name === name) {
    node.disabled = value
    return true
  }
  for (const child of node.children ?? []) {
    if (isAriaNode(child) && setAriaDisabled(child, role, name, value)) return true
  }
  return false
}

// Remove the first descendant node matching role + (name OR text) by splicing it
// from its parent's children. Named nodes (buttons/links/headings) match on name;
// text leaves ({ role: 'text', text }) match on text.
function removeAriaNode(node: AriaNode, role: string, nameOrText: string): boolean {
  const children = node.children
  if (!children) return false
  for (let i = 0; i < children.length; i++) {
    const c = children[i]
    if (isAriaNode(c) && c.role === role && (c.name === nameOrText || c.text === nameOrText)) {
      children.splice(i, 1)
      return true
    }
  }
  for (const c of children) {
    if (isAriaNode(c) && removeAriaNode(c, role, nameOrText)) return true
  }
  return false
}

// Set body[path[0]][path[1]]... = value on an already-parsed JSON body.
function setJsonPath(body: unknown, path: string[], value: unknown): void {
  let cur: unknown = body
  for (let i = 0; i < path.length - 1; i++) {
    if (cur === null || typeof cur !== 'object' || Array.isArray(cur)) return
    cur = (cur as Record<string, unknown>)[path[i]]
  }
  if (cur !== null && typeof cur === 'object' && !Array.isArray(cur)) {
    ;(cur as Record<string, unknown>)[path[path.length - 1]] = value
  }
}

// Apply a single-point mutation to a deep clone of the base captures. The base
// is never mutated; the returned array carries exactly one changed datum.
export function applyMutation(baseCaptures: Capture[], mutation: Mutation): Capture[] {
  const captures = clone(baseCaptures)
  const idx = selectIndex(captures, mutation)
  const cap = captures[idx]
  if (!cap) throw new Error(`doctor: no capture at index ${idx} for mutation ${mutation.op}`)

  switch (mutation.op) {
    case 'inject_query': {
      // Preserve the raw path (a `new URL` would percent-encode the <id:N>
      // placeholders); only touch the query string.
      const [pathPart, queryPart = ''] = cap.url.split('?')
      const params = new URLSearchParams(queryPart)
      params.set(mutation.param, mutation.value)
      const qs = params.toString()
      cap.url = qs ? `${pathPart}?${qs}` : pathPart
      break
    }
    case 'delete_endpoint': {
      delete cap.apiResponses[mutation.endpoint]
      break
    }
    case 'set_aria_disabled': {
      setAriaDisabled(cap.aria, mutation.node_role, mutation.node_name, mutation.value)
      break
    }
    case 'reorder_ids': {
      const items = cap.apiResponses['GET /api/v1/contacts'] ?? []
      const item = items.find(i => i.query.ids_only === 'true')
      const body = item?.body as { data?: { ids?: unknown[] } } | undefined
      const ids = body?.data?.ids
      if (Array.isArray(ids) && ids.length >= 2) {
        if (mutation.mode === 'reverse') ids.reverse()
        else [ids[0], ids[1]] = [ids[1], ids[0]]
      }
      break
    }
    case 'blank_dialog': {
      // Remove the irreversibility warning wherever it appears in the bracket
      // (the confirm dialog is recorded on every capture whose action fired it),
      // so the judge sees no warning at all — one semantic single-point change.
      for (const c of captures) for (const d of c.dialogs) d.message = ''
      break
    }
    case 'remove_aria_subtree': {
      removeAriaNode(cap.aria, mutation.node_role, mutation.node_name)
      break
    }
    case 'set_field': {
      cap.fields = { ...(cap.fields ?? {}), [mutation.field]: mutation.value }
      break
    }
    case 'set_json_field': {
      const item = (cap.apiResponses[mutation.endpoint] ?? [])[0]
      if (item) setJsonPath(item.body, mutation.path, mutation.value)
      break
    }
  }
  return captures
}

// Resolve the captures a case grades: for a doctored case, apply its
// single-point mutation to the base fixtures; a clean case returns them as-is.
// The SINGLE place the doctor mutation is applied — the eval AND the labeling
// CLI both go through here, so a doctored case never drafts/grades the clean
// evidence.
export function resolveCaseCaptures(
  c: { source: 'clean' | 'doctored'; doctor?: { mutation: Mutation } },
  baseCaptures: Capture[]
): Capture[] {
  if (c.source === 'doctored' && c.doctor) return applyMutation(baseCaptures, c.doctor.mutation)
  return baseCaptures
}

// CLI: bun run tests/tours/judge/doctor.ts <baseCaptureFile> <mutationJson> [outFile]
// Applies the mutation to a single base capture and writes the doctored JSON.
async function main(): Promise<void> {
  const fs = await import('fs')
  const [baseFile, mutationJson, outFile] = process.argv.slice(2)
  if (!baseFile || !mutationJson) {
    console.error('usage: doctor.ts <baseCaptureFile> <mutationJson> [outFile]')
    process.exit(2)
  }
  const base = JSON.parse(fs.readFileSync(baseFile, 'utf8')) as Capture
  const { parseMutation } = await import('./corpus/schema')
  const mutation = parseMutation(JSON.parse(mutationJson))
  const [doctored] = applyMutation([base], mutation)
  const out = `${JSON.stringify(doctored, null, 2)}\n`
  if (outFile) fs.writeFileSync(outFile, out, 'utf8')
  else process.stdout.write(out)
}

if (typeof import.meta !== 'undefined' && (import.meta as ImportMeta).main) {
  void main()
}
