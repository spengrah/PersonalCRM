// CON-038 — list and detail navigation share one default ordering.
//   [0] list defaults to cadence order, most frequent first (verifier† caveat)
//   [1] detail prev/next uses the same default ordering (ids_only == list order)

import { asArray, asRecord, asString, byRole, endpointItems, envelopeData } from '../evidence'
import type { CaptureSet, ItemVerdicts } from '../types'
import type { ApiResponseItem, Capture } from '../../../support/types'

// Frequency rank: most frequent first (weekly) → least (annual) → no-cadence last.
const CADENCE_RANK: Record<string, number> = {
  weekly: 0,
  biweekly: 1,
  monthly: 2,
  quarterly: 3,
  biannual: 4,
  annual: 5,
}
const NO_CADENCE_RANK = 6

function cadenceRank(cadence: unknown): number {
  const c = asString(cadence)
  return c && c in CADENCE_RANK ? CADENCE_RANK[c] : NO_CADENCE_RANK
}

// The visible list request (sort=cadence, NOT ids_only) → its ordered contacts.
function listContacts(capture: Capture): Array<Record<string, unknown>> | undefined {
  const item = endpointItems(capture, 'GET /api/v1/contacts').find(i => i.query.ids_only !== 'true')
  if (!item) return undefined
  const data = asArray(envelopeData(item.body))
  return data?.map(x => asRecord(x)).filter((x): x is Record<string, unknown> => x !== undefined)
}

// The ids_only navigation request → its ordered id list.
function idsOnlyIds(capture: Capture): string[] | undefined {
  const item = endpointItems(capture, 'GET /api/v1/contacts').find(
    (i: ApiResponseItem) => i.query.ids_only === 'true'
  )
  if (!item) return undefined
  const data = asRecord(envelopeData(item.body))
  const ids = asArray(data?.ids)
  return ids?.map(x => asString(x)).filter((x): x is string => x !== undefined)
}

export function con038(set: CaptureSet): ItemVerdicts {
  const list = byRole(set, 'list')
  const detail = byRole(set, 'detail')
  const out: ItemVerdicts = {}

  // [0] cadence-ordered list — capture-coverage caveat: the tour forces an
  // explicit sort, so the IMPLICIT no-sort default is not captured → abstain.
  const contacts = list ? listContacts(list) : undefined
  if (!contacts || contacts.length === 0) {
    out[0] = { verdict: 'unsure', reason: 'no list capture / empty contacts body — no evidence' }
  } else {
    let ordered = true
    for (let i = 1; i < contacts.length; i++) {
      if (cadenceRank(contacts[i].cadence) < cadenceRank(contacts[i - 1].cadence)) ordered = false
    }
    out[0] = {
      verdict: 'unsure',
      citation: 'GET /api/v1/contacts (sort=cadence&order=desc) body cadence order',
      reason:
        `capture-coverage caveat: cadence-ordering ${ordered ? 'holds' : 'is VIOLATED'} in the ` +
        'explicit-sort context, but the implicit no-sort default is not captured (tour follows ' +
        'sort=cadence&order=desc) — a bare-/contacts capture is a tour follow-up.',
    }
  }

  // [1] detail ids_only order == list order (the visible list ids are a prefix
  // of the full ids_only navigation order).
  const listIds = contacts?.map(c => asString(c.id)).filter((x): x is string => x !== undefined)
  const navIds = detail ? idsOnlyIds(detail) : undefined
  if (!listIds || listIds.length === 0 || !navIds || navIds.length === 0) {
    out[1] = {
      verdict: 'unsure',
      reason: 'missing list ids or ids_only navigation order — no evidence',
    }
  } else {
    const prefix = navIds.slice(0, listIds.length)
    const agree = prefix.length === listIds.length && prefix.every((id, i) => id === listIds[i])
    out[1] = agree
      ? { verdict: 'pass', citation: 'detail ids_only GET /api/v1/contacts data.ids == list order' }
      : {
          verdict: 'fail',
          citation: 'detail ids_only data.ids',
          reason: 'the detail navigation ids_only order does not match the list order',
        }
  }

  return out
}
