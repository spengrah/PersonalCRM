// Tests for the round-manifest provenance merge.
//
// The interesting property is the FAILURE path: the caller treats a merge failure
// as non-fatal, which is only true if a failure cannot damage the manifest the
// round's own provenance assert reads. So the failure is injected at each write
// step and the original file is compared byte-for-byte.

import { afterEach, beforeEach, describe, expect, it } from 'vitest'
import * as fs from 'fs'
import * as os from 'os'
import * as path from 'path'
import { main, mergeManifest, mergedManifest, realIo, type MergeIo } from './manifest-merge'

const PROVENANCE = { status: 'ok', upstreamSha256: 'a'.repeat(64) }

let dir: string
let manifest: string
const ORIGINAL = `${JSON.stringify({ gitSha: 'abc123', runId: '20260722T000000Z', tours: 4 }, null, 2)}\n`

beforeEach(() => {
  dir = fs.mkdtempSync(path.join(os.tmpdir(), 'manifest-merge-'))
  manifest = path.join(dir, 'manifest.json')
  fs.writeFileSync(manifest, ORIGINAL)
})
afterEach(() => fs.rmSync(dir, { recursive: true, force: true }))

const failingIo = (step: keyof MergeIo): MergeIo => ({
  ...realIo,
  [step]: () => {
    throw new Error(`${step} boom`)
  },
})

describe('mergedManifest', () => {
  it('adds priceSync and preserves every pre-existing key', () => {
    const merged = JSON.parse(mergedManifest(ORIGINAL, PROVENANCE)) as Record<string, unknown>
    expect(merged.gitSha).toBe('abc123')
    expect(merged.runId).toBe('20260722T000000Z')
    expect(merged.tours).toBe(4)
    expect(merged.priceSync).toEqual(PROVENANCE)
  })

  it('refuses a manifest that is not a JSON object rather than overwriting it', () => {
    expect(() => mergedManifest('[1,2]', PROVENANCE)).toThrow(/not a JSON object/)
    expect(() => mergedManifest('not json', PROVENANCE)).toThrow()
  })
})

describe('mergeManifest', () => {
  it('writes the merged manifest and leaves no temp file behind', () => {
    mergeManifest(manifest, PROVENANCE)
    const merged = JSON.parse(fs.readFileSync(manifest, 'utf8')) as Record<string, unknown>
    expect(merged.priceSync).toEqual(PROVENANCE)
    expect(merged.gitSha).toBe('abc123')
    expect(fs.readdirSync(dir)).toEqual(['manifest.json'])
  })

  for (const step of ['writeFileSync', 'renameSync'] as const) {
    it(`leaves the original BYTE-identical when ${step} fails, and throws`, () => {
      expect(() => mergeManifest(manifest, PROVENANCE, failingIo(step))).toThrow(`${step} boom`)
      expect(fs.readFileSync(manifest, 'utf8')).toBe(ORIGINAL)
      // The temp file never survives a failure.
      expect(fs.readdirSync(dir)).toEqual(['manifest.json'])
    })
  }

  it('leaves the original untouched when the manifest cannot be parsed', () => {
    fs.writeFileSync(manifest, 'truncated {')
    expect(() => mergeManifest(manifest, PROVENANCE)).toThrow()
    expect(fs.readFileSync(manifest, 'utf8')).toBe('truncated {')
  })
})

describe('main', () => {
  it('exits 0 on success and 1 on any failure, naming the reason', () => {
    const errs: string[] = []
    expect(main([manifest, 'ok', 'b'.repeat(64)], s => errs.push(s))).toBe(0)
    expect(JSON.parse(fs.readFileSync(manifest, 'utf8')).priceSync.upstreamSha256).toBe(
      'b'.repeat(64)
    )

    expect(main([path.join(dir, 'missing.json'), 'ok', ''], s => errs.push(s))).toBe(1)
    expect(errs.join('\n')).toContain('manifest-merge:')
  })

  it('records an EMPTY sha rather than inventing one when the sync reported none', () => {
    expect(main([manifest, 'failed'], () => {})).toBe(0)
    const merged = JSON.parse(fs.readFileSync(manifest, 'utf8')) as {
      priceSync: { status: string; upstreamSha256: string }
    }
    expect(merged.priceSync).toEqual({ status: 'failed', upstreamSha256: '' })
  })

  it('rejects a missing argument with the usage code', () => {
    expect(main([], () => {})).toBe(2)
  })
})
