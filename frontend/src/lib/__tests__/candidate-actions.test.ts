import { describe, it, expect } from 'vitest'
import { allowedActionsForSource, sourceAllowsImport } from '../candidate-actions'

describe('candidate-actions (link-only policy mirror)', () => {
  it('allows import/link/ignore for ordinary sources', () => {
    for (const source of ['gcontacts', 'icloud_contacts', 'gcal_attendee', 'telegram']) {
      expect(allowedActionsForSource(source)).toEqual(['import', 'link', 'ignore'])
      expect(sourceAllowsImport(source)).toBe(true)
    }
  })

  it('omits import for link-only sources (gmail_correspondence)', () => {
    expect(allowedActionsForSource('gmail_correspondence')).toEqual(['link', 'ignore'])
    expect(sourceAllowsImport('gmail_correspondence')).toBe(false)
  })
})
