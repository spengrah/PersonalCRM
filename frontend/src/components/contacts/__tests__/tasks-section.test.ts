import { describe, it, expect } from 'vitest'

// Extract the function for testing - mirrors the implementation in tasks-section.tsx
function cleanTaskContent(content: string | undefined): string {
  if (!content) return 'Untitled task'

  // First, strip leading markdown link prefix with colon
  let cleaned = content.replace(/^\[([^\]]+)\]\([^)]+\):\s*/, '')

  // Then replace any remaining markdown links with just their text
  cleaned = cleaned.replace(/\[([^\]]+)\]\([^)]+\)/g, '$1')

  return cleaned.trim() || 'Untitled task'
}

describe('cleanTaskContent', () => {
  it('returns "Untitled task" for undefined content', () => {
    expect(cleanTaskContent(undefined)).toBe('Untitled task')
  })

  it('returns "Untitled task" for empty string', () => {
    expect(cleanTaskContent('')).toBe('Untitled task')
  })

  it('strips leading markdown link prefix with colon (action tasks)', () => {
    const content = '[John Doe](https://example.com/contacts/123): Follow up on proposal'
    expect(cleanTaskContent(content)).toBe('Follow up on proposal')
  })

  it('replaces inline markdown links with text (cadence tasks)', () => {
    const content = 'Reach out to [John Doe](https://example.com/contacts/123)'
    expect(cleanTaskContent(content)).toBe('Reach out to John Doe')
  })

  it('handles both prefix and inline links', () => {
    const content = '[Jane](https://a.com): Talk to [Bob](https://b.com) about project'
    expect(cleanTaskContent(content)).toBe('Talk to Bob about project')
  })

  it('returns plain text unchanged', () => {
    expect(cleanTaskContent('Simple task')).toBe('Simple task')
  })

  it('handles multiple inline links', () => {
    const content = 'Meet [Alice](url1) and [Bob](url2)'
    expect(cleanTaskContent(content)).toBe('Meet Alice and Bob')
  })

  it('handles whitespace-only result as untitled', () => {
    const content = '[Name](url):   '
    expect(cleanTaskContent(content)).toBe('Untitled task')
  })
})
