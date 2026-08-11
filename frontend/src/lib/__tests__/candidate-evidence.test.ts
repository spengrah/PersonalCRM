import { describe, it, expect } from 'vitest'
import { candidateEvidenceLabel } from '../candidate-evidence'
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

describe('candidateEvidenceLabel', () => {
  it('renders the co-occurring contact and message count for gmail_correspondence', () => {
    expect(
      candidateEvidenceLabel(
        candidate({
          source: 'gmail_correspondence',
          metadata: {
            co_occurring_contact: { id: 'c1', name: 'Alex Rivera' },
            message_count: 4,
          },
        })
      )
    ).toBe('Seen with Alex Rivera · 4 messages')
  })

  it('renders count, recency and counterpart for telegram metadata', () => {
    // Handle-only telegram candidates have no name fields, so
    // getCandidateDisplayName falls back to the username itself — the exact
    // case where the card's existing username chip is suppressed as
    // redundant with the heading, and the evidence line picks up the
    // counterpart instead (no duplicate rendering with the chip).
    expect(
      candidateEvidenceLabel(
        candidate({
          source: 'telegram',
          metadata: {
            username: '@dalepeer5',
            message_count: 12,
            last_message_at: '2026-08-01T10:00:00Z',
          },
        })
      )
    ).toBe('@dalepeer5 · 12 messages · Last: Aug 1, 2026')
  })

  it('omits the counterpart segment for telegram when a chip already shows it', () => {
    // A named candidate (display_name != username) is exactly when the
    // existing username chip in page.tsx renders — the evidence line must
    // not repeat it.
    expect(
      candidateEvidenceLabel(
        candidate({
          source: 'telegram',
          display_name: 'Dale Peer',
          metadata: {
            username: '@dalepeer5',
            message_count: 12,
          },
        })
      )
    ).toBe('12 messages')
  })

  it('renders whatsapp push_name and counts', () => {
    expect(
      candidateEvidenceLabel(
        candidate({
          source: 'whatsapp',
          metadata: {
            push_name: 'Sam Okafor',
            message_count: 74,
            last_message_at: '2026-02-14T09:30:00Z',
          },
        })
      )
    ).toBe('Sam Okafor · 74 messages · Last: Feb 14, 2026')
  })

  it('renders trusted_sender counterpart and self variant', () => {
    expect(
      candidateEvidenceLabel(
        candidate({
          source: 'gmail_participant',
          metadata: {
            trusted_sender: { address: 'pat@example.com', name: 'Pat Nguyen' },
            message_count: 1,
          },
        })
      )
    ).toBe('From Pat Nguyen · 1 message')

    expect(
      candidateEvidenceLabel(
        candidate({
          source: 'gmail_participant',
          metadata: {
            trusted_sender: { address: 'me@own-domain.example', self: true },
            message_count: 2,
          },
        })
      )
    ).toBe('Sent by you · 2 messages')

    expect(
      candidateEvidenceLabel(
        candidate({
          source: 'gmail_participant',
          metadata: {
            trusted_sender: { address: 'unknown@example.com' },
            message_count: 3,
          },
        })
      )
    ).toBe('From unknown@example.com · 3 messages')
  })

  it('renders nothing for an unparseable last_message_at', () => {
    expect(
      candidateEvidenceLabel(
        candidate({
          source: 'gmail_correspondence',
          metadata: {
            co_occurring_contact: { id: 'c1', name: 'Alex Rivera' },
            last_message_at: 'not-a-date',
          },
        })
      )
    ).toBe('Seen with Alex Rivera')
  })

  it('returns null when metadata carries no evidence keys', () => {
    expect(candidateEvidenceLabel(candidate({ source: 'gcontacts', metadata: {} }))).toBeNull()
  })

  it('returns null when the candidate carries no metadata at all', () => {
    expect(candidateEvidenceLabel(candidate({ source: 'gcontacts' }))).toBeNull()
  })

  it('returns null for gcal_attendee meeting metadata (no evidence keys)', () => {
    expect(
      candidateEvidenceLabel(
        candidate({
          source: 'gcal_attendee',
          metadata: { meeting_title: 'Weekly sync', meeting_date: '2026-08-01' },
        })
      )
    ).toBeNull()
  })
})
