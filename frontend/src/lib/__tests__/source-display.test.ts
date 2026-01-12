import { describe, it, expect } from 'vitest'
import { getSourceDisplay } from '../source-display'
import { Users, Calendar, HelpCircle } from 'lucide-react'

describe('getSourceDisplay', () => {
  it('returns friendly name and icon for gcontacts', () => {
    const result = getSourceDisplay('gcontacts')
    expect(result.label).toBe('Google Contacts')
    expect(result.icon).toBe(Users)
  })

  it('returns friendly name and icon for gcal_attendee', () => {
    const result = getSourceDisplay('gcal_attendee')
    expect(result.label).toBe('Google Calendar')
    expect(result.icon).toBe(Calendar)
  })

  it('returns raw source name for unknown sources', () => {
    const result = getSourceDisplay('unknown_source')
    expect(result.label).toBe('unknown_source')
    expect(result.icon).toBe(HelpCircle)
  })

  it('returns raw source name for empty string', () => {
    const result = getSourceDisplay('')
    expect(result.label).toBe('')
    expect(result.icon).toBe(HelpCircle)
  })
})
