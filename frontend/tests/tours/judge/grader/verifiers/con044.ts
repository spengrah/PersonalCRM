// CON-044 — mark-as-contacted logs a mutual interaction from the list.
//   [0] a mutual-direction interaction is logged, server-timestamped

import { asRecord, asString, byRole, envelopeData, findApiItem } from '../evidence'
import type { CaptureSet, ItemVerdict, ItemVerdicts } from '../types'

export function con044(set: CaptureSet): ItemVerdicts {
  const after = byRole(set, 'after')
  const out: ItemVerdicts = {}

  out[0] = ((): ItemVerdict => {
    // The `after` bracket is captured right after mark-as-contacted, so an
    // ABSENT POST in a PRESENT after-bracket is a real fail (the action did not
    // log an interaction) — distinct from the whole bracket being uncaptured.
    if (!after) return { verdict: 'unsure', reason: 'no after-mark capture — no evidence' }
    const post = findApiItem(
      after,
      (i, k) => k === 'POST /api/v1/contacts/:id/interactions' && i.method === 'POST'
    )
    if (!post) {
      return {
        verdict: 'fail',
        citation: 'after-mark bracket',
        reason: 'mark-as-contacted did not log an interaction (no POST in the after bracket)',
      }
    }
    const data = asRecord(envelopeData(post.body))
    const direction = asString(data?.direction)
    const occurredAt = asString(data?.occurred_at)
    const ok =
      post.status === 201 && direction === 'mutual' && occurredAt !== undefined && occurredAt !== ''
    return ok
      ? {
          verdict: 'pass',
          citation: 'POST .../interactions body direction=mutual + server occurred_at',
        }
      : {
          verdict: 'fail',
          citation: 'POST .../interactions body',
          reason: `expected a mutual, server-timestamped interaction (got direction=${direction ?? 'none'}, occurred_at=${occurredAt ?? 'none'}, status=${post.status})`,
        }
  })()

  return out
}
