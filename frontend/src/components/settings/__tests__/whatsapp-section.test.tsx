import { describe, expect, it } from 'vitest'

import { deriveWhatsAppStep, type PairMode, type Step } from '../whatsapp-section'
import type { WhatsAppState, WhatsAppStatus } from '@/lib/whatsapp-api'

// The rendered surface is covered by whatsapp-settings.spec.ts, matching the
// repo convention that settings sections are tested through Playwright. What is
// unit-tested here is the one piece of production LOGIC the section carries:
// the mapping from (query result, local pair mode) onto a render branch.

function statusWith(overrides: Partial<WhatsAppStatus>): WhatsAppStatus {
  return {
    configured: true,
    state: 'not_paired',
    backfill: { pending: 0, processing: 0, failed: 0, dropped_inline_chunks: 0 },
    ingest: { unresolved_lid_peers: 0 },
    ...overrides,
  }
}

function apiError(status: number): Error & { status: number } {
  const err = new Error(`HTTP ${status}`) as Error & { status: number }
  err.status = status
  return err
}

function derive(args: {
  isLoading?: boolean
  error?: unknown
  status?: WhatsAppStatus
  pairMode?: PairMode
}): Step {
  return deriveWhatsAppStep({
    isLoading: args.isLoading ?? false,
    error: args.error,
    status: args.status,
    pairMode: args.pairMode ?? null,
  })
}

describe('deriveWhatsAppStep', () => {
  it('reports loading while the query is in flight', () => {
    expect(derive({ isLoading: true })).toBe('loading')
  })

  it('reports loading when the query has settled with no data yet', () => {
    expect(derive({})).toBe('loading')
  })

  // A 404 is the feature-off signal: the routes are not registered at all.
  it('reads a 404 as not_configured', () => {
    expect(derive({ error: apiError(404), status: statusWith({}) })).toBe('not_configured')
  })

  it('reads any other failure as fetch_error', () => {
    expect(derive({ error: apiError(500) })).toBe('fetch_error')
    expect(derive({ error: new Error('network down') })).toBe('fetch_error')
  })

  // An error outranks stale cached data: a section that kept rendering a
  // connected account while its status endpoint 404s would be lying.
  it('prefers the error branch over cached data', () => {
    expect(derive({ error: apiError(404), status: statusWith({ state: 'connected' }) })).toBe(
      'not_configured'
    )
  })

  const backendStates: Array<[WhatsAppState, Step]> = [
    ['not_ready', 'not_ready'],
    ['not_paired', 'not_paired'],
    ['connecting', 'connecting'],
    ['reconnecting', 'connecting'],
    ['connected', 'connected'],
    ['disconnected', 'disconnected'],
    ['disconnect_failed', 'disconnect_failed'],
    ['error', 'error'],
  ]

  it.each(backendStates)('maps backend state %s onto step %s', (state, expected) => {
    expect(derive({ status: statusWith({ state }) })).toBe(expected)
  })

  it('splits pairing by method', () => {
    expect(
      derive({
        status: statusWith({
          state: 'pairing',
          pairing: { method: 'qr', qr_code: 'CODE', expires_at: '2026-08-04T12:00:00Z' },
        }),
      })
    ).toBe('pairing_qr')
    expect(
      derive({
        status: statusWith({
          state: 'pairing',
          pairing: { method: 'phone', pair_code: 'ABCD1234', expires_at: '2026-08-04T12:00:00Z' },
        }),
      })
    ).toBe('pairing_phone')
  })

  // A pairing whose method has not arrived yet still has to render something;
  // QR is the branch that tolerates a missing code.
  it('falls back to the QR branch when a pairing carries no method', () => {
    expect(derive({ status: statusWith({ state: 'pairing' }) })).toBe('pairing_qr')
  })

  const pairModes: PairMode[] = [null, 'choose', 'phone']

  it.each(pairModes)('keeps not_paired for pairMode %s', pairMode => {
    expect(derive({ status: statusWith({ state: 'not_paired' }), pairMode })).toBe('not_paired')
  })

  // pairMode is read under exactly two backend states. A user who took the
  // relink affordance after a terminal disconnect is in the link flow, and the
  // affordance would otherwise be a button that changes nothing.
  it('moves a disconnected section into the link flow once a pair mode is chosen', () => {
    const disconnected = statusWith({ state: 'disconnected', reason: 'logged_out' })
    expect(derive({ status: disconnected })).toBe('disconnected')
    expect(derive({ status: disconnected, pairMode: 'choose' })).toBe('not_paired')
    expect(derive({ status: disconnected, pairMode: 'phone' })).toBe('not_paired')
  })

  // pairMode must NOT reach any other branch. These are the states a stale
  // pairMode could plausibly outlive if the reset rule were dropped.
  it.each(['connected', 'pairing', 'connecting', 'disconnect_failed'] as WhatsAppState[])(
    'ignores a stale pair mode under backend state %s',
    state => {
      const withMode = derive({ status: statusWith({ state }), pairMode: 'choose' })
      const withoutMode = derive({ status: statusWith({ state }) })
      expect(withMode).toBe(withoutMode)
    }
  )

  // An unrecognised state from a newer backend must not render nothing at all.
  it('falls back to fetch_error for an unknown state', () => {
    expect(derive({ status: statusWith({ state: 'brand_new' as WhatsAppState }) })).toBe(
      'fetch_error'
    )
  })
})
