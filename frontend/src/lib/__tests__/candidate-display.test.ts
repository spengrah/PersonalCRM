import { describe, it, expect } from 'vitest'
import { getCandidateDisplayName } from '../candidate-display'
import type { ImportCandidate } from '@/types/import'

function candidate(overrides: Partial<ImportCandidate> = {}): ImportCandidate {
  return {
    id: '1',
    source: 'test',
    emails: [],
    phones: [],
    ...overrides,
  }
}

describe('getCandidateDisplayName', () => {
  it('uses display_name when present', () => {
    expect(
      getCandidateDisplayName(
        candidate({ display_name: 'John Doe', first_name: 'Johnny', last_name: 'Doer' })
      )
    ).toBe('John Doe')
  })

  it('joins first + last when no display_name', () => {
    expect(getCandidateDisplayName(candidate({ first_name: 'John', last_name: 'Doe' }))).toBe(
      'John Doe'
    )
  })

  it('returns first name alone', () => {
    expect(getCandidateDisplayName(candidate({ first_name: 'John' }))).toBe('John')
  })

  it('returns last name alone', () => {
    expect(getCandidateDisplayName(candidate({ last_name: 'Doe' }))).toBe('Doe')
  })

  it('returns Unknown when nothing is set', () => {
    expect(getCandidateDisplayName(candidate())).toBe('Unknown')
  })

  it('falls back to telegram @username when no names are set', () => {
    expect(
      getCandidateDisplayName(
        candidate({ source: 'telegram', metadata: { username: '@daledobeck' } })
      )
    ).toBe('@daledobeck')
  })

  it('ignores metadata.username for non-telegram sources', () => {
    expect(
      getCandidateDisplayName(
        candidate({ source: 'gcontacts', metadata: { username: '@daledobeck' } })
      )
    ).toBe('Unknown')
  })

  it('returns Unknown when telegram metadata.username is empty', () => {
    expect(
      getCandidateDisplayName(candidate({ source: 'telegram', metadata: { username: '' } }))
    ).toBe('Unknown')
  })

  it('prefers stored name fields over telegram fallback', () => {
    expect(
      getCandidateDisplayName(
        candidate({
          source: 'telegram',
          first_name: 'Dale',
          last_name: 'Dobeck',
          metadata: { username: '@daledobeck' },
        })
      )
    ).toBe('Dale Dobeck')
  })
})
