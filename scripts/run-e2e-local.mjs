import { readFileSync } from 'node:fs'
import { execSync, spawnSync } from 'node:child_process'
import path from 'node:path'

const repoRoot = execSync('git rev-parse --show-toplevel', {
  encoding: 'utf8',
  stdio: ['ignore', 'pipe', 'inherit'],
}).trim()
const mappingPath = path.join(repoRoot, 'frontend/tests/e2e/test-map.json')

const refExists = ref => {
  try {
    execSync(`git rev-parse --verify --quiet ${ref}`, {
      cwd: repoRoot,
      stdio: ['ignore', 'ignore', 'ignore'],
    })
    return true
  } catch {
    return false
  }
}
// Resolve the diff base the SAME way scripts/hooks/pre-push does (upstream branch,
// falling back to origin/develop) so the tag selection AND the unmatched-file
// warning below both operate over the real push range — not origin/main, which on
// the develop-default branch model can include already-merged-to-develop work.
// E2E_BASE_REF still overrides for CI/explicit use. The final origin/main last-resort
// keeps baseRef a string even on a bare clone lacking origin/develop.
const resolveBaseRef = () => {
  if (process.env.E2E_BASE_REF) return process.env.E2E_BASE_REF
  try {
    const upstream = execSync('git rev-parse --abbrev-ref --symbolic-full-name @{u}', {
      encoding: 'utf8',
      cwd: repoRoot,
      stdio: ['ignore', 'pipe', 'ignore'],
    }).trim()
    if (upstream) return upstream
  } catch {
    // no tracked upstream — fall through to the develop/main chain
  }
  return ['origin/develop', 'origin/main'].find(refExists) || 'origin/main'
}
const baseRef = resolveBaseRef()

const readChangedFiles = command => {
  let output
  try {
    output = execSync(command, {
      encoding: 'utf8',
      stdio: ['ignore', 'pipe', 'ignore'],
      cwd: repoRoot,
    }).trim()
  } catch {
    // A `git diff` against a base ref that doesn't exist (e.g. a bare clone with
    // neither origin/develop nor origin/main, or an invalid E2E_BASE_REF) throws.
    // Degrade to an empty changed set rather than crashing the whole run — but warn,
    // since silently dropping to @smoke would under-cover real committed changes.
    console.error(`WARNING: test-e2e-diff — "${command}" failed; diff-selection is treating it as no changes. Selection may under-cover this push (set E2E_BASE_REF to a valid ref).`)
    return []
  }

  return output.length ? output.split('\n').map(line => line.trim()).filter(Boolean) : []
}

const changedFiles = new Set([
  ...readChangedFiles(`git diff --name-only ${baseRef}...HEAD`),
  ...readChangedFiles('git diff --name-only'),
  ...readChangedFiles('git diff --name-only --cached'),
])

const mapping = JSON.parse(readFileSync(mappingPath, 'utf8'))

const tags = new Set(['@smoke'])
const matchedFiles = new Set()

for (const file of changedFiles) {
  for (const rule of mapping) {
    const regex = new RegExp(rule.pattern)
    if (regex.test(file)) {
      matchedFiles.add(file)
      for (const tag of rule.tags || []) {
        tags.add(tag)
      }
    }
  }
}

// Warn-first (non-fatal): a changed frontend/src or backend/internal file that
// matched NO pattern contributed no E2E tags, so test-e2e-diff may under-cover it.
// Goes to stderr so it never pollutes the stdout grep-pattern contract (the
// E2E_PRINT_ONLY consumer reads stdout) — emitted unconditionally, including under
// E2E_PRINT_ONLY, so an author gets the signal without spawning Playwright.
const unmatched = [...changedFiles].filter(
  f => !matchedFiles.has(f) && (f.startsWith('frontend/src/') || f.startsWith('backend/internal/')),
)
if (unmatched.length) {
  console.error('WARNING: test-e2e-diff — changed file(s) matched no test-map.json pattern, contributing no E2E tags:')
  for (const file of unmatched) {
    console.error(`  - ${file}`)
  }
  console.error('These files may be under-covered by the diff-selected run. Add a test-map.json entry to map them to an @area, or accept this (CI runs the full suite regardless).')
}

const escapeRegex = value => value.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')
const grepPattern = Array.from(tags)
  .map(tag => escapeRegex(tag))
  .join('|')

if (process.env.E2E_PRINT_ONLY === '1') {
  console.log(grepPattern)
  process.exit(0)
}

const env = {
  ...process.env,
  PLAYWRIGHT_GREP: grepPattern,
}

const result = spawnSync('make', ['test-e2e-local'], {
  stdio: 'inherit',
  env,
  cwd: repoRoot,
})

process.exit(result.status ?? 1)
