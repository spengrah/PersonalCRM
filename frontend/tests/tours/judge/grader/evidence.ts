// Pure helpers for reading structured evidence out of a §1 Capture: the aria
// node tree, endpoint-grouped API items, and URL parts. No model, no Playwright.

import type { AriaChild, AriaNode, ApiResponseItem, Capture } from '../../support/types'
import type { CaptureSet } from './types'

// --- aria tree ---

export function isAriaNode(child: AriaChild): child is AriaNode {
  return (child as AriaNode).role !== undefined
}

export function walkAria(node: AriaNode, visit: (n: AriaNode) => void): void {
  visit(node)
  for (const child of node.children ?? []) {
    if (isAriaNode(child)) walkAria(child, visit)
  }
}

export function findAria(node: AriaNode, pred: (n: AriaNode) => boolean): AriaNode | undefined {
  let found: AriaNode | undefined
  walkAria(node, n => {
    if (found === undefined && pred(n)) found = n
  })
  return found
}

export function findAllAria(node: AriaNode, pred: (n: AriaNode) => boolean): AriaNode[] {
  const out: AriaNode[] = []
  walkAria(node, n => {
    if (pred(n)) out.push(n)
  })
  return out
}

// All nodes in document (pre-order) order — used for ordered/section-relative
// checks (e.g. CON-045 upcoming-before-celebrated, per-card age scan).
export function flattenAria(node: AriaNode): AriaNode[] {
  return findAllAria(node, () => true)
}

export function findByRoleName(
  node: AriaNode,
  role: string,
  name: string | RegExp
): AriaNode | undefined {
  return findAria(node, n => n.role === role && n.name !== undefined && matches(n.name, name))
}

// True if any node's accessible name OR visible text contains the substring.
export function ariaTextIncludes(node: AriaNode, needle: string): boolean {
  return (
    findAria(
      node,
      n => (n.name?.includes(needle) ?? false) || (n.text?.includes(needle) ?? false)
    ) !== undefined
  )
}

function matches(value: string, pattern: string | RegExp): boolean {
  return typeof pattern === 'string' ? value === pattern : pattern.test(value)
}

// --- API items ---

export function endpointItems(capture: Capture, key: string): ApiResponseItem[] {
  return capture.apiResponses[key] ?? []
}

// The first item under any endpoint key matching the method + path predicate.
export function findApiItem(
  capture: Capture,
  pred: (item: ApiResponseItem, key: string) => boolean
): ApiResponseItem | undefined {
  for (const [key, items] of Object.entries(capture.apiResponses)) {
    for (const item of items) {
      if (pred(item, key)) return item
    }
  }
  return undefined
}

export function findAllApiItems(
  capture: Capture,
  pred: (item: ApiResponseItem, key: string) => boolean
): ApiResponseItem[] {
  const out: ApiResponseItem[] = []
  for (const [key, items] of Object.entries(capture.apiResponses)) {
    for (const item of items) {
      if (pred(item, key)) out.push(item)
    }
  }
  return out
}

// --- URL parts (normalized url is a path + query) ---

export function urlPathname(url: string): string {
  try {
    return new URL(url, 'http://x').pathname
  } catch {
    return url.split('?')[0]
  }
}

export function urlQuery(url: string): Record<string, string> {
  const out: Record<string, string> = {}
  try {
    const u = new URL(url, 'http://x')
    for (const [k, v] of u.searchParams) out[k] = v
  } catch {
    /* empty */
  }
  return out
}

// --- JSON body accessors (bodies are `unknown` after normalization) ---

export function asRecord(value: unknown): Record<string, unknown> | undefined {
  return typeof value === 'object' && value !== null && !Array.isArray(value)
    ? (value as Record<string, unknown>)
    : undefined
}

export function asArray(value: unknown): unknown[] | undefined {
  return Array.isArray(value) ? value : undefined
}

export function asString(value: unknown): string | undefined {
  return typeof value === 'string' ? value : undefined
}

// The `data` payload of a `{ success, data, meta }` envelope (or the value itself).
export function envelopeData(body: unknown): unknown {
  const rec = asRecord(body)
  return rec && 'data' in rec ? rec.data : body
}

// --- CaptureSet lookups ---

export function byRole(set: CaptureSet, role: string): Capture | undefined {
  return set.captures.find(c => c.pair?.role === role)
}

export function byNoteIncludes(set: CaptureSet, needle: string): Capture | undefined {
  return set.captures.find(c => c.note.includes(needle))
}
