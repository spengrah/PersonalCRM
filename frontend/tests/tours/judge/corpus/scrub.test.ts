import { describe, it, expect } from 'vitest'
import { scrubCapture } from './scrub'
import { EMAIL_RE, PHONE_RES } from '../pii-patterns'

// The scrubber/pattern assertions live in judge/scrub.test.ts (the scrubber
// relocated to the export path). Here we cover only the curation-specific
// scrubCapture wrapper: it scrubs method values AND drops the screenshot key.

describe('scrubCapture (curation wrapper)', () => {
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

  it('drops the live-run-only screenshot path', () => {
    const scrubbed = scrubCapture({
      tour: 'dashboard',
      screenshot: 'screenshots/dashboard/001.png',
    })
    expect(scrubbed).toEqual({ tour: 'dashboard' })
  })
})
