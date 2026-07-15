// Broad PII + synthetic-provenance audit over ALL committed corpus artifacts
// (captures / cases / labels / reports) — the mechanical P0 gate (test-plan 7).
// NO human eyeball is relied upon. Any hit blocks the commit.
//
// Checks: (a) zero raw UUIDs + zero real-host absolute URLs; (b) zero emails +
// zero phone patterns (both scrubbed by scrub.ts); (c) no token/secret values;
// (d) the mechanical synthetic-name gate — every full_name/new_name is prefixed
// synth-prodshaped-; (e) the secondary provenance hint — PROVENANCE.json
// declares seedProfile prod-shaped.
//
// The pure `auditText`/`auditNames`/`auditCorpus` are vitest-covered; the file
// walker (`runAudit`) is invoked by the vitest real-corpus test AND the CLI.

import * as fs from 'fs'
import * as path from 'path'
import {
  EMAIL_RE,
  PHONE_RES,
  RAW_UUID_RE,
  REAL_HOST_URL_RE,
  SECRET_RES,
  SYNTH_NAME_PREFIX,
} from './patterns'

export interface Violation {
  kind: string
  source: string
  match: string
}

function scan(text: string, re: RegExp, kind: string, source: string, out: Violation[]): void {
  for (const m of text.matchAll(
    new RegExp(re.source, re.flags.includes('g') ? re.flags : re.flags + 'g')
  )) {
    out.push({ kind, source, match: m[0].slice(0, 80) })
  }
}

// (a)–(c): pattern bans over the raw text of any committed artifact.
export function auditText(text: string, source: string): Violation[] {
  const out: Violation[] = []
  scan(text, RAW_UUID_RE, 'raw-uuid', source, out)
  scan(text, REAL_HOST_URL_RE, 'real-host-url', source, out)
  scan(text, EMAIL_RE, 'email', source, out)
  for (const re of PHONE_RES) scan(text, re, 'phone', source, out)
  for (const re of SECRET_RES) scan(text, re, 'secret', source, out)
  return out
}

// Collect every contact-name value (full_name + the merge new_name) in a parsed
// JSON artifact.
export function collectContactNames(value: unknown): string[] {
  const out: string[] = []
  const walk = (v: unknown): void => {
    if (Array.isArray(v)) {
      for (const x of v) walk(x)
    } else if (v !== null && typeof v === 'object') {
      for (const [k, val] of Object.entries(v as Record<string, unknown>)) {
        if ((k === 'full_name' || k === 'new_name') && typeof val === 'string') out.push(val)
        walk(val)
      }
    }
  }
  walk(value)
  return out
}

// (d) the binding synthetic-name gate: every contact name is prefixed. A real
// target's un-prefixed names fail here regardless of any manifest label.
export function auditNames(names: string[], source: string): Violation[] {
  return names
    .filter(n => !n.startsWith(SYNTH_NAME_PREFIX))
    .map(n => ({ kind: 'unprefixed-name', source, match: n.slice(0, 80) }))
}

// The UI word vocabulary — words that legitimately appear in the contacts
// surface's aria labels. A TitleCase bigram is exempt (treated as a UI label,
// NOT a contact name) ONLY when BOTH words are in this set; a bigram with even
// one out-of-vocabulary token is flagged. (This set grows as new surfaces are
// toured; a new UI label trips a loud failure.)
const UI_VOCAB = new Set(
  [
    'all',
    'contact',
    'contacts',
    'already',
    'celebrated',
    'archiving',
    'search',
    'birthday',
    'birthdays',
    'tracker',
    'information',
    'edit',
    'email',
    'primary',
    'full',
    'name',
    'google',
    'chat',
    'interaction',
    'interactions',
    'none',
    'keeping',
    'merge',
    'location',
    'log',
    'cadence',
    'new',
    'next',
    'previous',
    'notes',
    'note',
    'open',
    'tanstack',
    'resolve',
    'conflicts',
    'this',
    'year',
    'total',
    'upcoming',
    'with',
    'personal',
    'crm',
    'add',
    'task',
    'gift',
    'planning',
    'mark',
    'contacted',
    'delete',
    'cancel',
    'save',
    'dashboard',
    'imports',
    'settings',
    'methods',
    'actions',
    'pending',
    'followup',
    'will',
    'merged',
    'from',
    'kept',
    'source',
    'target',
    'has',
    'phone',
    'telegram',
    'discord',
    'twitter',
    'signal',
    'whatsapp',
    'gchat',
    'today',
    'tracking',
    'no',
    'be',
  ].map(w => w.toLowerCase())
)

// Exact multi-word UI label phrases (lowercased). A TitleCase bigram is exempt
// when it is a substring of one of these — preferred over unioning each label's
// constituent words into UI_VOCAB, which would erode the heuristic (a real name
// whose two tokens both coincide with unrelated label words would slip). The
// dashboard + cadence-followup surfaces add their multi-word labels here.
const UI_PHRASES = [
  'action required',
  'most urgent',
  'last contacted',
  'view all contacts',
  'add new contact',
  'add contact',
  'add task',
  'contact information',
  'birthday tracker',
  'notes combined',
]

// A TitleCase name bigram, optionally carrying the synthetic prefix. Tokens are
// >= 2 chars (cap + >= 1 lower) so SHORT names like "Al Smith" / "Jo Kim" are
// covered, not only >= 3-letter tokens.
const NAME_BIGRAM_RE = /(synth-prodshaped-)?([A-Z][a-z]+(?:-[A-Z][a-z]+)? [A-Z][a-z]+)/g

// Collect the name/text on every aria node (any object carrying a string role).
export function collectAriaNodeStrings(value: unknown): string[] {
  const out: string[] = []
  const walk = (v: unknown): void => {
    if (Array.isArray(v)) {
      for (const x of v) walk(x)
    } else if (v !== null && typeof v === 'object') {
      const rec = v as Record<string, unknown>
      if (typeof rec.role === 'string') {
        if (typeof rec.name === 'string') out.push(rec.name)
        if (typeof rec.text === 'string') out.push(rec.text)
      }
      for (const val of Object.values(rec)) walk(val)
    }
  }
  walk(value)
  return out
}

// (d, aria) BEST-EFFORT SECONDARY heuristic over VISIBLE aria copy. It flags an
// un-prefixed contact-name-shaped bigram (a TitleCase pair NOT both-UI-vocab) in
// an aria name/text node. It is NOT a completeness guarantee: "does arbitrary
// free text look like a real name" is fundamentally incompletable (a name whose
// tokens all coincide with UI vocab, or an unusual single-token shape, can slip).
//
// The BINDING PII guarantees are: (a) synthetic-seed provenance — captures come
// from a `--reset-and-seed --profile prod-shaped` world; and (b) the airtight
// full_name/new_name prefix gate (auditNames) over response bodies, which is
// where contact names authoritatively live and which the UI renders into aria.
// This aria scan is defense-in-depth on top of those — it closes the obvious
// bypass (a real name visible in aria) without claiming to catch every name.
export function auditAriaNames(ariaStrings: string[], source: string): Violation[] {
  const out: Violation[] = []
  for (const s of ariaStrings) {
    for (const m of s.matchAll(NAME_BIGRAM_RE)) {
      if (m[1]) continue // synth-prefixed → provably synthetic
      const bigram = m[2].toLowerCase()
      // Exempt a bigram when it is part of a known multi-word UI PHRASE (e.g.
      // "action required", "view all contacts") — preferred over unioning every
      // constituent word into UI_VOCAB, so a real name whose two tokens happen to
      // coincide with unrelated label words is still flagged.
      if (UI_PHRASES.some(p => p.includes(bigram))) continue
      // Fallback: both tokens are single-domain UI vocabulary (a label like
      // "Contact Information"). A bigram with even one non-vocab token — "Mark
      // Smith", "Al Smith" — is flagged. On the synthetic corpus this can only
      // produce harmless false-positives on UI phrases (extend UI_PHRASES); it
      // never misses a real-name bigram of this shape.
      const [w1, w2] = bigram.split(' ')
      if (!(UI_VOCAB.has(w1) && UI_VOCAB.has(w2))) {
        out.push({ kind: 'unprefixed-aria-name', source, match: m[2].slice(0, 80) })
      }
    }
  }
  return out
}

export interface CorpusFile {
  path: string
  content: string
  json?: unknown
}

export interface AuditOptions {
  provenance?: { seedProfile?: string }
}

export function auditCorpus(files: CorpusFile[], opts: AuditOptions = {}): Violation[] {
  const out: Violation[] = []
  for (const f of files) {
    out.push(...auditText(f.content, f.path))
    if (f.json !== undefined) {
      out.push(...auditNames(collectContactNames(f.json), f.path))
      out.push(...auditAriaNames(collectAriaNodeStrings(f.json), f.path))
    }
  }
  // (e) secondary provenance hint.
  if (opts.provenance && opts.provenance.seedProfile !== 'prod-shaped') {
    out.push({
      kind: 'provenance',
      source: 'PROVENANCE.json',
      match: `seedProfile='${opts.provenance.seedProfile ?? ''}' (expected prod-shaped)`,
    })
  }
  return out
}

// --- File walker (used by the vitest real-corpus test + the CLI) ---

const AUDITED_EXT = new Set(['.json', '.yaml', '.yml', '.md'])
const JSON_EXT = new Set(['.json'])

function walkFiles(dir: string): string[] {
  if (!fs.existsSync(dir)) return []
  const out: string[] = []
  for (const entry of fs.readdirSync(dir, { withFileTypes: true })) {
    const full = path.join(dir, entry.name)
    if (entry.isDirectory()) out.push(...walkFiles(full))
    else if (AUDITED_EXT.has(path.extname(entry.name))) out.push(full)
  }
  return out
}

export function runAudit(corpusRoot: string): Violation[] {
  const provenancePath = path.join(corpusRoot, 'captures', 'PROVENANCE.json')
  let provenance: { seedProfile?: string } | undefined
  if (fs.existsSync(provenancePath)) {
    try {
      provenance = JSON.parse(fs.readFileSync(provenancePath, 'utf8')) as { seedProfile?: string }
    } catch {
      provenance = { seedProfile: 'unparseable' }
    }
  }
  // Every committed artifact is text-audited — including PROVENANCE.json (a
  // stray host/UUID/email/phone/token/name in its note must be caught). It is
  // ALSO parsed above for the seedProfile provenance hint.
  const files: CorpusFile[] = walkFiles(corpusRoot).map(f => {
    const content = fs.readFileSync(f, 'utf8')
    let json: unknown
    if (JSON_EXT.has(path.extname(f))) {
      try {
        json = JSON.parse(content)
      } catch {
        json = undefined
      }
    }
    return { path: f, content, json }
  })
  return auditCorpus(files, { provenance })
}

// CLI entry: `bun run tests/tours/judge/corpus/pii-audit.ts [corpusRoot]`.
if (typeof import.meta !== 'undefined' && (import.meta as ImportMeta).main) {
  const root = process.argv[2] ?? path.join(import.meta.dirname ?? __dirname, '..', 'corpus')
  const violations = runAudit(root)
  if (violations.length === 0) {
    console.log(`pii-audit: clean over ${root}`)
  } else {
    console.error(`pii-audit: ${violations.length} violation(s):`)
    for (const v of violations) console.error(`  [${v.kind}] ${v.source}: ${v.match}`)
    process.exit(1)
  }
}
