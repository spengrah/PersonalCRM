import { describe, it, expect } from 'vitest'
import * as path from 'path'
import { auditCorpus, auditNames, auditText, collectContactNames, runAudit } from './pii-audit'

describe('auditText — pattern bans', () => {
  it('flags a raw UUID', () => {
    expect(auditText('id 11111111-1111-4111-8111-111111111111', 'f').map(v => v.kind)).toContain(
      'raw-uuid'
    )
  })

  it('flags a real-host absolute URL but ALLOWS <host>/<staging> placeholders', () => {
    expect(auditText('http://secret.example.com/x', 'f').map(v => v.kind)).toContain(
      'real-host-url'
    )
    expect(auditText('https://<host>/photos/x.jpg', 'f')).toEqual([])
    expect(auditText('http://<staging>/contacts', 'f')).toEqual([])
  })

  it('flags emails and phones', () => {
    expect(auditText('a@b.com', 'f').map(v => v.kind)).toContain('email')
    expect(auditText('+1-479-555-0100', 'f').map(v => v.kind)).toContain('phone')
  })

  it('does NOT flag preserved ISO dates/timestamps as phones', () => {
    const kinds = auditText('1990-05-05 and 2026-07-12T16:14:34.223608Z', 'f').map(v => v.kind)
    expect(kinds).not.toContain('phone')
  })

  it('flags secret literals', () => {
    expect(auditText('Bearer abcdef1234567890', 'f').map(v => v.kind)).toContain('secret')
    expect(auditText('eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9', 'f').map(v => v.kind)).toContain(
      'secret'
    )
  })
})

describe('synthetic-name gate', () => {
  it('collects full_name + new_name and flags un-prefixed names', () => {
    const json = {
      data: [{ full_name: 'synth-prodshaped-Brux Testwell' }, { full_name: 'Real Person' }],
      requestBody: { new_name: 'synth-prodshaped-Brux (merged)' },
    }
    expect(collectContactNames(json)).toEqual([
      'synth-prodshaped-Brux Testwell',
      'Real Person',
      'synth-prodshaped-Brux (merged)',
    ])
    const violations = auditNames(collectContactNames(json), 'cap.json')
    expect(violations).toHaveLength(1)
    expect(violations[0]).toMatchObject({ kind: 'unprefixed-name', match: 'Real Person' })
  })
})

describe('auditCorpus', () => {
  it('is clean over a synthetic + scrubbed artifact set', () => {
    const files = [
      {
        path: 'captures/c.json',
        content: JSON.stringify({
          full_name: 'synth-prodshaped-Brux',
          url: 'https://<host>/x',
          method: '<phone:1>',
        }),
        json: { full_name: 'synth-prodshaped-Brux' },
      },
    ]
    expect(auditCorpus(files, { provenance: { seedProfile: 'prod-shaped' } })).toEqual([])
  })

  it('flags a wrong-provenance manifest', () => {
    expect(auditCorpus([], { provenance: { seedProfile: 'unknown' } }).map(v => v.kind)).toContain(
      'provenance'
    )
  })

  it('flags a real name reaching the fixtures (wrong target)', () => {
    const files = [{ path: 'captures/c.json', content: '{}', json: { full_name: 'Jane Real' } }]
    expect(auditCorpus(files).map(v => v.kind)).toContain('unprefixed-name')
  })
})

// The REAL merge-gate check: audit the actually-committed corpus tree. Empty
// until the corpus is seeded; must stay clean once it is.
describe('runAudit over the committed corpus', () => {
  it('finds zero violations in the committed corpus', () => {
    const corpusRoot = path.join(__dirname)
    const violations = runAudit(corpusRoot)
    if (violations.length > 0) {
      console.error('PII violations:', violations.slice(0, 20))
    }
    expect(violations).toEqual([])
  })
})
