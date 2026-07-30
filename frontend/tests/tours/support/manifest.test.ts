import { describe, it, expect } from 'vitest'
import { buildManifest, redactHost } from './manifest'

describe('buildManifest seed-profile provenance', () => {
  it('defaults an unset seedProfile to "unknown" (NOT "standard")', () => {
    const m = buildManifest({ runId: 'r1' })
    expect(m.seedProfile).toBe('unknown')
  })

  it('honors an operator-declared seedProfile override', () => {
    const m = buildManifest({ runId: 'r1', seedProfile: 'minimal-scoped' })
    expect(m.seedProfile).toBe('minimal-scoped')
  })

  it('stamps both version fields and the runId', () => {
    const m = buildManifest({ runId: 'r-xyz' })
    expect(m.captureFormatVersion).toBe(1)
    expect(m.captureGeneratorVersion).toBe(1)
    expect(m.runId).toBe('r-xyz')
    expect(m.gitSha).toBe('unknown')
    expect(m.stagingImageDigest).toBe('unknown')
  })
})

describe('redactHost', () => {
  it('replaces the host with <staging>, keeping scheme + path', () => {
    expect(redactHost('https://secret.example.com/')).toBe('https://<staging>/')
    expect(redactHost('http://localhost:3000/contacts')).toBe('http://<staging>/contacts')
  })

  it('falls back to an opaque value for an unparseable URL', () => {
    expect(redactHost('not a url')).toBe('<staging>')
  })
})
