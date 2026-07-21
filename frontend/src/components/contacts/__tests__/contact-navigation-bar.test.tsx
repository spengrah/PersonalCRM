import { describe, it, expect, vi } from 'vitest'
import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'

import { ContactNavigationBar } from '../contact-navigation-bar'

// Common props with prev/next NOT disabled by their boundary flags, so any
// disabling that shows up comes from isEditMode/isLoading — not the boundaries.
const baseProps = {
  onBack: vi.fn(),
  onPrevious: vi.fn(),
  onNext: vi.fn(),
  canGoBack: true,
  canGoForward: true,
  currentIndex: 5,
  totalCount: 21,
}

describe('ContactNavigationBar back control', () => {
  it('keeps Back enabled while the id list loads, even though prev/next are disabled', () => {
    render(<ContactNavigationBar {...baseProps} isLoading={true} isEditMode={false} />)

    // Back is decoupled from isLoading — it is the return escape hatch and
    // falls back to page 1 gracefully if clicked before the index resolves.
    expect(screen.getByRole('button', { name: 'Back to list' })).toBeEnabled()
    // Prev/next ARE disabled while loading (their predicate includes isLoading),
    // proving the enabled-Back assertion above is not vacuous.
    expect(screen.getByRole('button', { name: 'Previous contact' })).toBeDisabled()
    expect(screen.getByRole('button', { name: 'Next contact' })).toBeDisabled()
  })

  it('disables Back in edit mode', () => {
    render(<ContactNavigationBar {...baseProps} isEditMode={true} isLoading={false} />)
    expect(screen.getByRole('button', { name: 'Back to list' })).toBeDisabled()
  })

  it('enables Back when not editing and not loading', () => {
    render(<ContactNavigationBar {...baseProps} isEditMode={false} isLoading={false} />)
    expect(screen.getByRole('button', { name: 'Back to list' })).toBeEnabled()
  })

  it('fires onBack when the enabled control is clicked', async () => {
    const onBack = vi.fn()
    render(<ContactNavigationBar {...baseProps} onBack={onBack} isEditMode={false} />)
    await userEvent.click(screen.getByRole('button', { name: 'Back to list' }))
    expect(onBack).toHaveBeenCalledTimes(1)
  })
})
