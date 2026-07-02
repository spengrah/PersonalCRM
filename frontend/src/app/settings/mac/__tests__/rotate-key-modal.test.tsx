import { render, screen, fireEvent, waitFor } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'

import { RotateKeyModal } from '../rotate-key-modal'

describe('RotateKeyModal', () => {
  const token = {
    token: 'abc123base64token0000000000000000',
    expires_at: '2026-12-31T23:59:59Z',
  }

  it('renders the templated re-pair command with the token interpolated', () => {
    render(
      <RotateKeyModal
        hostname="mac-1"
        token={token}
        isPending={false}
        isError={false}
        onClose={() => {}}
      />
    )
    const cmd = screen.getByTestId('rotate-key-command')
    expect(cmd.textContent).toBe(`crm-mac install --re-pair --pair ${token.token}`)
  })

  it('shows the hostname in the dialog title', () => {
    render(
      <RotateKeyModal
        hostname="my-mac"
        token={token}
        isPending={false}
        isError={false}
        onClose={() => {}}
      />
    )
    expect(screen.getByRole('dialog', { name: /Rotate pair-key for my-mac/i })).toBeInTheDocument()
    expect(screen.getByRole('heading', { name: /Rotate pair-key for my-mac/i })).toBeInTheDocument()
  })

  it('explains that the current api-key stops working after rotation', () => {
    render(
      <RotateKeyModal
        hostname="mac-1"
        token={token}
        isPending={false}
        isError={false}
        onClose={() => {}}
      />
    )
    expect(screen.getByText(/current api-key stops working/i)).toBeInTheDocument()
    expect(screen.getByText(/can be used\s+only once/i)).toBeInTheDocument()
  })

  it('copies the FULL templated command (not just the token) to clipboard', async () => {
    const writeText = vi.fn().mockResolvedValue(undefined)
    Object.assign(navigator, { clipboard: { writeText } })

    render(
      <RotateKeyModal
        hostname="mac-1"
        token={token}
        isPending={false}
        isError={false}
        onClose={() => {}}
      />
    )
    fireEvent.click(screen.getByRole('button', { name: /Copy/i }))

    await waitFor(() => {
      expect(writeText).toHaveBeenCalledTimes(1)
    })
    expect(writeText).toHaveBeenCalledWith(`crm-mac install --re-pair --pair ${token.token}`)
  })

  it('renders the pending state when the token mint is in flight', () => {
    render(
      <RotateKeyModal
        hostname="mac-1"
        token={null}
        isPending={true}
        isError={false}
        onClose={() => {}}
      />
    )
    expect(screen.getByText(/Generating pairing token/i)).toBeInTheDocument()
    expect(screen.queryByTestId('rotate-key-command')).not.toBeInTheDocument()
  })

  it('renders an error state when the mint failed', () => {
    render(
      <RotateKeyModal
        hostname="mac-1"
        token={null}
        isPending={false}
        isError={true}
        onClose={() => {}}
      />
    )
    expect(screen.getByText(/Failed to mint pairing token/i)).toBeInTheDocument()
  })

  it('invokes onClose when the Close button is clicked', () => {
    const onClose = vi.fn()
    render(
      <RotateKeyModal
        hostname="mac-1"
        token={token}
        isPending={false}
        isError={false}
        onClose={onClose}
      />
    )
    fireEvent.click(screen.getByRole('button', { name: 'Close' }))
    expect(onClose).toHaveBeenCalledTimes(1)
  })
})
