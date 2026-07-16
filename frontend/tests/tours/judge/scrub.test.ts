import { describe, it, expect } from 'vitest'
import { createScrubber, scrubValue } from './scrub'
import { EMAIL_RE, PHONE_RES } from './pii-patterns'

describe('scrub email/phone → placeholders', () => {
  it('maps emails to stable <email:N> placeholders (first-seen)', () => {
    const s = createScrubber()
    expect(s.scrub('reach synth-prodshaped-brux.dummond-52@synthetic.example now')).toBe(
      'reach <email:1> now'
    )
    expect(s.scrub('a@b.com and synth-prodshaped-brux.dummond-52@synthetic.example')).toBe(
      '<email:2> and <email:1>'
    )
  })

  it('maps synthetic + international + bare 3-3-4 phones to <phone:N>', () => {
    const s = createScrubber()
    expect(s.scrub('call +1-479-555-0100 or (479) 555-0101 or 479-555-0102')).toBe(
      'call <phone:1> or <phone:2> or <phone:3>'
    )
  })

  it('preserves ISO dates and timestamps (NOT phone-shaped)', () => {
    const s = createScrubber()
    const kept = 'birthday 1990-05-05 occurred_at 2026-07-12T16:14:34.223608Z'
    expect(s.scrub(kept)).toBe(kept)
  })

  it('deep-scrubs a nested value without mutating the input', () => {
    const value = {
      aria: { role: 'text', text: 'Keeping synth-x (brux@synthetic.example)' },
      apiResponses: {
        'GET /x': [{ body: { methods: [{ type: 'phone', value: '+1-479-555-0100' }] } }],
      },
    }
    const scrubbed = scrubValue(value, createScrubber())
    expect(JSON.stringify(scrubbed)).not.toMatch(EMAIL_RE)
    for (const re of PHONE_RES) expect(JSON.stringify(scrubbed)).not.toMatch(re)
    // input untouched
    expect(value.aria.text).toContain('@synthetic.example')
  })

  it('scrubValue leaves non-strings alone', () => {
    expect(scrubValue({ n: 5, b: true, x: null }, createScrubber())).toEqual({
      n: 5,
      b: true,
      x: null,
    })
  })

  it('double-scrub is a no-op — placeholders do not re-match the REs (safe anywhere)', () => {
    const s = createScrubber()
    const raw = 'reach brux@synthetic.example or call +1-479-555-0100'
    const once = s.scrub(raw)
    expect(s.scrub(once)).toBe(once)
    // and the placeholders survived unchanged
    expect(once).toBe('reach <email:1> or call <phone:1>')
  })
})
