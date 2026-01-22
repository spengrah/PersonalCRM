import { readFileSync } from 'node:fs'
import { execSync, spawnSync } from 'node:child_process'
import path from 'node:path'

const repoRoot = execSync('git rev-parse --show-toplevel', {
  encoding: 'utf8',
  stdio: ['ignore', 'pipe', 'inherit'],
}).trim()
const mappingPath = path.join(repoRoot, 'frontend/tests/e2e/test-map.json')
const baseRef = process.env.E2E_BASE_REF || 'origin/main'

const diffOutput = execSync(`git diff --name-only ${baseRef}...HEAD`, {
  encoding: 'utf8',
  stdio: ['ignore', 'pipe', 'inherit'],
  cwd: repoRoot,
}).trim()

const changedFiles = diffOutput.length
  ? diffOutput.split('\n').map(line => line.trim()).filter(Boolean)
  : []

const mapping = JSON.parse(readFileSync(mappingPath, 'utf8'))

const tags = new Set(['@smoke'])

for (const file of changedFiles) {
  for (const rule of mapping) {
    const regex = new RegExp(rule.pattern)
    if (regex.test(file)) {
      for (const tag of rule.tags || []) {
        tags.add(tag)
      }
    }
  }
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
