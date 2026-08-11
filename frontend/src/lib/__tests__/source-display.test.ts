import { describe, it, expect } from 'vitest'
import { getSourceDisplay } from '../source-display'
import { Users, Calendar, Send, Cloud, MessageCircle, Mail, HelpCircle } from 'lucide-react'

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

  it('returns friendly name and icon for icloud_contacts', () => {
    const result = getSourceDisplay('icloud_contacts')
    expect(result.label).toBe('iCloud Contacts')
    expect(result.icon).toBe(Cloud)
  })

  it('returns friendly name and icon for telegram', () => {
    const result = getSourceDisplay('telegram')
    expect(result.label).toBe('Telegram')
    expect(result.icon).toBe(Send)
  })

  // WhatsApp import candidates carry this badge in the imports queue; without
  // the map entry the badge would fall through to the raw source string.
  it('returns friendly name and icon for whatsapp', () => {
    const result = getSourceDisplay('whatsapp')
    expect(result.label).toBe('WhatsApp')
    expect(result.icon).toBe(MessageCircle)
  })

  it('returns a Gmail participant label and the mail icon for gmail_participant', () => {
    const result = getSourceDisplay('gmail_participant')
    expect(result.label).toBe('Gmail participant')
    expect(result.icon).toBe(Mail)
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
