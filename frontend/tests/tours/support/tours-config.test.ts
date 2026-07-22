import { describe, it, expect } from 'vitest'
import toursConfig from '../../../playwright.tours.config'

// The clock-anchored birthday fixtures (CON-052) only land in the birthdays page's
// highlight window if the tour browser resolves "today" in the SAME zone the reseed
// computes the fixtures in (UTC). This guard fails loudly if that pin is dropped —
// the pin is load-bearing, so it must be provably removable-detectable.
describe('playwright.tours.config UTC pin', () => {
  it('pins the browser calendar zone to UTC', () => {
    expect(toursConfig.use?.timezoneId).toBe('UTC')
  })

  it('pins a fixed locale', () => {
    expect(toursConfig.use?.locale).toBe('en-US')
  })
})
