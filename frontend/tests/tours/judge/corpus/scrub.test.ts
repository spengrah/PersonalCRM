import { describe, it, expect } from 'vitest'
import { createScrubber, scrubCapture, scrubValue } from './scrub'
import { EMAIL_RE, PHONE_RES } from './patterns'

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

  it('maps synthetic + international phones to <phone:N>', () => {
    const s = createScrubber()
    expect(s.scrub('call +1-479-555-0100 or (479) 555-0101')).toBe('call <phone:1> or <phone:2>')
  })

  it('preserves ISO dates and timestamps (NOT phone-shaped)', () => {
    const s = createScrubber()
    const kept = 'birthday 1990-05-05 occurred_at 2026-07-12T16:14:34.223608Z'
    expect(s.scrub(kept)).toBe(kept)
  })

  it('deep-scrubs a capture object without mutating the input', () => {
    const capture = {
      aria: { role: 'text', text: 'Keeping synth-x (brux@synthetic.example)' },
      apiResponses: {
        'GET /x': [{ body: { methods: [{ type: 'phone', value: '+1-479-555-0100' }] } }],
      },
    }
    const scrubbed = scrubCapture(capture)
    expect(JSON.stringify(scrubbed)).not.toMatch(EMAIL_RE)
    for (const re of PHONE_RES) expect(JSON.stringify(scrubbed)).not.toMatch(re)
    // input untouched
    expect(capture.aria.text).toContain('@synthetic.example')
  })

  it('scrubValue leaves non-strings alone', () => {
    const s = createScrubber()
    expect(scrubValue({ n: 5, b: true, x: null }, s)).toEqual({ n: 5, b: true, x: null })
  })

  it('scrubCapture drops the live-run-only screenshot path', () => {
    const scrubbed = scrubCapture({
      tour: 'dashboard',
      screenshot: 'screenshots/dashboard/001.png',
    })
    expect(scrubbed).toEqual({ tour: 'dashboard' })
  })
})
