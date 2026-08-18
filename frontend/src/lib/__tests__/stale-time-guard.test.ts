import { describe, expect, it } from 'vitest'
import { readdirSync, readFileSync } from 'node:fs'
import { join, relative } from 'node:path'

// A raw numeric staleTime silently overrides the query-client default, which is
// where the Playwright zero-stale escape hatch lives — the query would keep its
// cache under E2E and never issue the request a tour/spec is waiting on. Every
// pin must therefore go through the staleTime() helper in lib/query-client.
//
// This scans app source (not tests, which may configure their own QueryClient).

// Vitest runs with cwd at the frontend package root.
const SRC_ROOT = join(process.cwd(), 'src')

function sourceFiles(dir: string): string[] {
  const out: string[] = []
  for (const entry of readdirSync(dir, { withFileTypes: true })) {
    const path = join(dir, entry.name)
    if (entry.isDirectory()) {
      if (entry.name === '__tests__' || entry.name === 'node_modules') continue
      out.push(...sourceFiles(path))
    } else if (/\.(ts|tsx)$/.test(entry.name) && !/\.test\.(ts|tsx)$/.test(entry.name)) {
      out.push(path)
    }
  }
  return out
}

describe('staleTime guard', () => {
  it('every staleTime pin goes through the staleTime() helper', () => {
    const violations: string[] = []
    for (const file of sourceFiles(SRC_ROOT)) {
      const lines = readFileSync(file, 'utf8').split('\n')
      lines.forEach((rawLine, i) => {
        const line = rawLine.replace(/\/\/.*$/, '')
        const match = line.match(/staleTime\s*:(?!\s*staleTime\()/)
        if (match) {
          violations.push(`${relative(SRC_ROOT, file)}:${i + 1}: ${rawLine.trim()}`)
        }
      })
    }
    expect(
      violations,
      `raw staleTime pin(s) found — wrap the value in the staleTime() helper from lib/query-client so the Playwright zero-stale escape hatch still applies:\n${violations.join('\n')}`
    ).toEqual([])
  })
})
