import { describe, expect, it, vi } from 'vitest'
import { fireEvent, render, screen } from '@testing-library/react'

import { TimeAccelerationWidget } from '../time-acceleration-widget'

vi.mock('@/hooks/use-accelerated-time', () => ({
  useAcceleratedTime: () => ({
    currentTime: new Date('2026-01-15T10:30:00Z'),
    isAccelerated: true,
    accelerationFactor: 60,
    environment: 'testing',
  }),
  useTimeAcceleration: () => ({
    mutateAsync: vi.fn().mockResolvedValue(undefined),
    isPending: false,
  }),
  ACCELERATION_PRESETS: {
    NORMAL: 1,
    FAST: 60,
    VERY_FAST: 1440,
    ULTRA_FAST: 43200,
  },
  createAccelerationSettings: (factor: number) => ({ factor }),
}))

function expandWidget() {
  render(<TimeAccelerationWidget />)
  // Collapsed state renders a single clock button; clicking it expands the panel
  fireEvent.click(screen.getByRole('button'))
}

describe('TimeAccelerationWidget speed buttons', () => {
  it('renders the active preset with primary (blue) button styling', () => {
    expandWidget()

    // accelerationFactor=60 matches the FAST preset, so "Fast" is the active button
    const fastButton = screen.getByRole('button', { name: 'Fast' })
    expect(fastButton.className).toContain('bg-blue-600')
    expect(fastButton.className).toContain('text-white')
  })

  it('renders inactive presets with outline button styling', () => {
    expandWidget()

    for (const name of ['Normal', 'Very Fast', 'Ultra Fast']) {
      const button = screen.getByRole('button', { name })
      expect(button.className).toContain('border-gray-300')
      expect(button.className).toContain('bg-white')
    }
  })
})
