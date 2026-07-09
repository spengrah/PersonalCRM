// Deterministic, conservative normalization for the tours capture format.
// Pure functions only — NO Playwright import — so this module is unit-testable
// without a browser (see normalize.test.ts) and importable by the grader.
//
// Design: preserve-by-default, deny-list only. A blunt "strip all
// timestamps/ids" would destroy the very evidence time- and identity-dependent
// verifiers must read, so we MAP UUIDs (never drop), sentinel only transport /
// audit-only noise, and preserve semantic dates + query strings.

import type { AriaChild, AriaNode } from './types'

export const DEFAULT_ARRAY_CAP = 50
export const DEFAULT_ARIA_CAP = 20

const UUID_GLOBAL = /[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}/gi
const UUID_SEGMENT = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i

// Audit-only wall-clock columns no toured behavior references → inert <ts>
// sentinel (key structure preserved). Behavior-relevant dates (contact_by,
// last_contacted, birthday, occurred_at, deleted_at, …) are NOT here and so
// survive verbatim.
const TIMESTAMP_DENY = new Set(['created_at', 'updated_at'])
// Transport noise → inert <redacted> sentinel.
const REDACT_DENY = new Set(['etag', 'request_id', 'requestid', 'trace_id', 'traceid'])
// Secret-bearing key names → inert <redacted> sentinel. Kept broad so a future
// tour over a settings/oauth surface cannot leak a credential value.
const TOKEN_KEY =
  /token|csrf|secret|session_id|password|passphrase|api[_-]?key|apikey|authorization|bearer|private[_-]?key|access[_-]?key/i

// ---------------------------------------------------------------------------
// Run-scoped UUID → ordinal mapper
// ---------------------------------------------------------------------------

export interface UuidMapper {
  map(uuid: string): string
}

// A first-seen map so the same UUID → the same <id:N> across every capture in a
// run (before/after pairs correlate; PII-safe; structure-preserving).
export function createUuidMapper(): UuidMapper {
  const seen = new Map<string, string>()
  let n = 0
  return {
    map(uuid: string): string {
      const key = uuid.toLowerCase()
      let placeholder = seen.get(key)
      if (!placeholder) {
        placeholder = `<id:${++n}>`
        seen.set(key, placeholder)
      }
      return placeholder
    },
  }
}

export function mapUuids(value: string, mapper: UuidMapper): string {
  return value.replace(UUID_GLOBAL, m => mapper.map(m))
}

// Redact the host of any absolute http(s) URL embedded in a string value (e.g. a
// profile_photo DTO field or an error-body URL), keeping the path + query. This
// is defense-in-depth alongside the non-production target guard, so captures
// never carry a real hostname even inside body/aria text.
export function scrubHosts(value: string): string {
  return value.replace(/https?:\/\/[^\s"'<>)\]}]+/gi, m => {
    try {
      const u = new URL(m)
      return `${u.protocol}//<host>${u.pathname}${u.search}`
    } catch {
      return m
    }
  })
}

// Host-redact then UUID-map a free-text value (body strings, aria names/text).
function scrubString(value: string, mapper: UuidMapper): string {
  return mapUuids(scrubHosts(value), mapper)
}

// ---------------------------------------------------------------------------
// URL normalization: host-stripped, query preserved, UUIDs mapped
// ---------------------------------------------------------------------------

function stripHost(url: string): string {
  try {
    const u = new URL(url)
    return u.pathname + u.search
  } catch {
    return url
  }
}

export function normalizeUrl(url: string, mapper: UuidMapper): string {
  return mapUuids(stripHost(url), mapper)
}

export function parseQuery(url: string, mapper: UuidMapper): Record<string, string> {
  const out: Record<string, string> = {}
  try {
    // A dummy base lets both absolute URLs (base ignored) and relative paths
    // (e.g. a harness probe path) parse, so probe items preserve query too.
    const u = new URL(url, 'http://localhost')
    const keys = [...u.searchParams.keys()].sort()
    for (const k of keys) {
      out[k] = mapUuids(u.searchParams.get(k) ?? '', mapper)
    }
  } catch {
    // Non-URL input yields an empty query.
  }
  return out
}

// The grouping key: "<METHOD> <path with UUID segments → :id>".
export function endpointKey(method: string, url: string): string {
  let path: string
  try {
    path = new URL(url).pathname
  } catch {
    path = url.split('?')[0]
  }
  const templated = path
    .split('/')
    .map(seg => (UUID_SEGMENT.test(seg) ? ':id' : seg))
    .join('/')
  return `${method} ${templated}`
}

// ---------------------------------------------------------------------------
// JSON body normalization: sentinel deny-list keys, sort keys, cap arrays
// ---------------------------------------------------------------------------

export function normalizeJson(value: unknown, mapper: UuidMapper, arrayCap: number): unknown {
  if (value === null || value === undefined) return value
  if (typeof value === 'string') return scrubString(value, mapper)
  if (typeof value === 'number' || typeof value === 'boolean') return value

  if (Array.isArray(value)) {
    let arr = value
    let truncated = 0
    if (Number.isFinite(arrayCap) && arr.length > arrayCap) {
      truncated = arr.length - arrayCap
      arr = arr.slice(0, arrayCap)
    }
    const out: unknown[] = arr.map(v => normalizeJson(v, mapper, arrayCap))
    if (truncated > 0) out.push({ __truncated__: truncated })
    return out
  }

  if (typeof value === 'object') {
    const src = value as Record<string, unknown>
    const out: Record<string, unknown> = {}
    for (const key of Object.keys(src).sort()) {
      if (TIMESTAMP_DENY.has(key)) {
        out[key] = '<ts>'
      } else if (REDACT_DENY.has(key) || TOKEN_KEY.test(key)) {
        out[key] = '<redacted>'
      } else {
        out[key] = normalizeJson(src[key], mapper, arrayCap)
      }
    }
    return out
  }

  return value
}

// ---------------------------------------------------------------------------
// Aria snapshot YAML → node tree — the key testable seam
// ---------------------------------------------------------------------------

// Unescape a YAML double-quoted scalar (Playwright's yamlEscapeValueIfNeeded /
// JSON.stringify emit these; the escaper is a JSON superset with \xNN).
function unescapeDoubleQuoted(raw: string): string {
  const inner = raw.slice(1, -1)
  return inner.replace(/\\(x[0-9a-fA-F]{2}|u[0-9a-fA-F]{4}|.)/g, (_m, esc: string) => {
    if (esc[0] === 'x' || esc[0] === 'u') return String.fromCharCode(parseInt(esc.slice(1), 16))
    switch (esc) {
      case 'n':
        return '\n'
      case 'r':
        return '\r'
      case 't':
        return '\t'
      case 'b':
        return '\b'
      case 'f':
        return '\f'
      case '"':
        return '"'
      case '\\':
        return '\\'
      case '/':
        return '/'
      default:
        return esc
    }
  })
}

// A YAML scalar is either a double-quoted string or a bare value.
function parseScalar(raw: string): string {
  const trimmed = raw.trim()
  if (trimmed.length >= 2 && trimmed.startsWith('"') && trimmed.endsWith('"')) {
    return unescapeDoubleQuoted(trimmed)
  }
  return trimmed
}

// Read a JSON-style double-quoted string starting at s[i] (s[i] === '"').
function readJsonString(s: string, i: number): { value: string; end: number } {
  let j = i + 1
  while (j < s.length) {
    if (s[j] === '\\') {
      j += 2
      continue
    }
    if (s[j] === '"') break
    j++
  }
  const raw = s.slice(i, j + 1)
  return { value: unescapeDoubleQuoted(raw), end: j + 1 }
}

// Read a "[token]" starting at s[i] (s[i] === '[').
function readBracket(s: string, i: number): { token: string; end: number } {
  let j = i + 1
  while (j < s.length && s[j] !== ']') j++
  return { token: s.slice(i + 1, j), end: j + 1 }
}

// Read a YAML single-quoted string starting at s[0] === "'" (Playwright wraps a
// whole createKey in single quotes when it needs escaping; '' → ').
function readSingleQuoted(s: string): { value: string; rest: string } {
  let j = 1
  let out = ''
  while (j < s.length) {
    if (s[j] === "'") {
      if (s[j + 1] === "'") {
        out += "'"
        j += 2
        continue
      }
      j++ // consume closing quote
      break
    }
    out += s[j]
    j++
  }
  return { value: out, rest: s.slice(j) }
}

function applyToken(node: AriaNode, token: string): void {
  if (token === 'disabled') node.disabled = true
  else if (token === 'checked') node.checked = true
  else if (token === 'checked=mixed') node.checked = 'mixed'
  else if (token === 'expanded') node.expanded = true
  else if (token === 'active') node.active = true
  else if (token === 'pressed') node.pressed = true
  else if (token === 'pressed=mixed') node.pressed = 'mixed'
  else if (token === 'selected') node.selected = true
  else if (token.startsWith('level=')) node.level = Number(token.slice('level='.length))
  // ref= and cursor=pointer are non-deterministic / not part of the contract — dropped.
}

// Parse a createKey string: role ["name"] [tokens...] (no trailing marker).
function parseKey(s: string): AriaNode {
  const str = s.trim()
  let i = 0
  while (i < str.length && str[i] !== ' ' && str[i] !== '"' && str[i] !== '[') i++
  const node: AriaNode = { role: str.slice(0, i) }
  while (i < str.length) {
    const c = str[i]
    if (c === ' ') {
      i++
    } else if (c === '"') {
      const r = readJsonString(str, i)
      node.name = r.value
      i = r.end
    } else if (c === '[') {
      const r = readBracket(str, i)
      applyToken(node, r.token)
      i = r.end
    } else {
      i++ // unexpected char — skip defensively
    }
  }
  return node
}

// Split "key: value" / "key:" respecting quoted names (a ':' inside a name must
// not be read as the marker).
function splitKeyAndMarker(s: string): { key: string; marker: string } {
  let inStr = false
  for (let i = 0; i < s.length; i++) {
    const c = s[i]
    if (inStr) {
      if (c === '\\') {
        i++
        continue
      }
      if (c === '"') inStr = false
      continue
    }
    if (c === '"') inStr = true
    else if (c === ':') return { key: s.slice(0, i), marker: s.slice(i) }
  }
  return { key: s, marker: '' }
}

// The marker is '' | ':' (has children) | ': <value>' (single inline text child).
function markerToInlineText(marker: string): string | undefined {
  if (marker === '' || marker === ':') return undefined
  if (marker.startsWith(': ')) return parseScalar(marker.slice(2))
  return parseScalar(marker.slice(1).replace(/^\s+/, ''))
}

function parseNodeLine(content: string): AriaNode {
  const rest = content.slice(2) // drop the leading "- "
  if (rest.startsWith('text:')) {
    return { role: 'text', text: parseScalar(rest.slice('text:'.length).replace(/^\s+/, '')) }
  }
  let keyStr: string
  let marker: string
  if (rest.startsWith("'")) {
    const r = readSingleQuoted(rest)
    keyStr = r.value
    marker = r.rest
  } else {
    const split = splitKeyAndMarker(rest)
    keyStr = split.key
    marker = split.marker
  }
  const node = parseKey(keyStr)
  const inline = markerToInlineText(marker)
  if (inline !== undefined) node.children = [{ role: 'text', text: inline }]
  return node
}

function countIndent(line: string): number {
  let n = 0
  while (n < line.length && line[n] === ' ') n++
  return n
}

// Parse Playwright's ariaSnapshot() YAML into a single root node whose children
// are the snapshot's top-level nodes. Indentation drives nesting.
export function parseAriaSnapshot(yaml: string): AriaNode {
  const root: AriaNode = { role: 'root', children: [] }
  const stack: Array<{ indent: number; node: AriaNode }> = [{ indent: -1, node: root }]
  const lines = yaml.split('\n')
  for (const rawLine of lines) {
    if (rawLine.trim() === '') continue
    const indent = countIndent(rawLine)
    const content = rawLine.slice(indent)
    if (!content.startsWith('- ')) continue // defensive: skip malformed lines
    const node = parseNodeLine(content)
    while (stack.length > 1 && stack[stack.length - 1].indent >= indent) stack.pop()
    const parent = stack[stack.length - 1].node
    ;(parent.children ??= []).push(node)
    stack.push({ indent, node })
  }
  return root
}

// Normalize a parsed aria tree: UUID-map names + text, cap repeated siblings.
export function normalizeAriaTree(node: AriaNode, mapper: UuidMapper, ariaCap: number): AriaNode {
  const out: AriaNode = { role: node.role }
  if (node.name !== undefined) out.name = scrubString(node.name, mapper)
  if (node.text !== undefined) out.text = scrubString(node.text, mapper)
  if (node.disabled !== undefined) out.disabled = node.disabled
  if (node.checked !== undefined) out.checked = node.checked
  if (node.expanded !== undefined) out.expanded = node.expanded
  if (node.level !== undefined) out.level = node.level
  if (node.pressed !== undefined) out.pressed = node.pressed
  if (node.selected !== undefined) out.selected = node.selected
  if (node.active !== undefined) out.active = node.active
  if (node.children && node.children.length > 0) {
    let kids = node.children as AriaNode[]
    let truncated = 0
    if (Number.isFinite(ariaCap) && kids.length > ariaCap) {
      truncated = kids.length - ariaCap
      kids = kids.slice(0, ariaCap)
    }
    const children: AriaChild[] = kids.map(k => normalizeAriaTree(k, mapper, ariaCap))
    if (truncated > 0) children.push({ __ariaTruncated__: truncated })
    out.children = children
  }
  return out
}
