// CON-045 — the birthdays page groups contacts by proximity.
//   [0] grouped into today / upcoming / already-celebrated
//   [1] gift-planning section appears only near year end
//   [2] upcoming sort soonest-first; celebrated sink to end
//   [3] placeholder-year birthdays display without an age
//   [4] the page follows accelerated time
//
// A single birthdays capture (arrayCap/ariaCap = Infinity — full list + all
// cards). Time-dependent items are graded in the recorded serverTime frame.

import { asArray, asRecord, asString, envelopeData, endpointItems, flattenAria } from '../evidence'
import type { CaptureSet, ItemVerdict, ItemVerdicts } from '../types'
import type { AriaNode, Capture } from '../../../support/types'

type Section = 'today' | 'gift' | 'upcoming' | 'celebrated' | undefined

const MONTH_NAMES = [
  'January',
  'February',
  'March',
  'April',
  'May',
  'June',
  'July',
  'August',
  'September',
  'October',
  'November',
  'December',
]

function sectionOf(node: AriaNode): Section {
  if (node.role !== 'heading' || node.level !== 2 || !node.name) return undefined
  if (node.name.startsWith('Today')) return 'today'
  if (node.name.startsWith('Gift Planning')) return 'gift'
  if (node.name.startsWith('Upcoming Birthdays')) return 'upcoming'
  if (node.name.startsWith('Already Celebrated')) return 'celebrated'
  return undefined
}

function birthdaysCapture(set: CaptureSet): Capture | undefined {
  return set.captures.find(c => c.behaviors.includes('CON-045')) ?? set.captures[0]
}

function fullList(capture: Capture): Array<Record<string, unknown>> {
  const item = endpointItems(capture, 'GET /api/v1/contacts').find(i => i.query.limit === '1000')
  const data = asArray(envelopeData(item?.body))
  return (data ?? [])
    .map(x => asRecord(x))
    .filter((x): x is Record<string, unknown> => x !== undefined)
}

// Which proximity groups (today / upcoming / celebrated) SHOULD render, computed
// from the dated birthdays in the recorded serverTime frame. Returns undefined
// when the full list is missing / the frame is unparseable (no data to compute
// from). An empty set means the list is present but holds no dated birthdays.
export function expectedProximitySections(
  contacts: Array<Record<string, unknown>>,
  currentTime: string
): Set<'today' | 'upcoming' | 'celebrated'> | undefined {
  const now = new Date(currentTime)
  if (Number.isNaN(now.getTime()) || contacts.length === 0) return undefined
  // 1-indexed month to match the birthday string's captured month (07 = July).
  const todayMd = (now.getUTCMonth() + 1) * 100 + now.getUTCDate()
  const out = new Set<'today' | 'upcoming' | 'celebrated'>()
  for (const c of contacts) {
    const m = asString(c.birthday)?.match(/^\d{4}-(\d{2})-(\d{2})/)
    if (!m) continue
    const md = Number(m[1]) * 100 + Number(m[2])
    if (md === todayMd) out.add('today')
    else if (md > todayMd) out.add('upcoming')
    else out.add('celebrated')
  }
  return out
}

export function con045(set: CaptureSet): ItemVerdicts {
  const cap = birthdaysCapture(set)
  const out: ItemVerdicts = {}
  if (!cap) {
    for (const i of [0, 1, 2, 3, 4])
      out[i] = { verdict: 'unsure', reason: 'no birthdays capture — no evidence' }
    return out
  }
  const nodes = flattenAria(cap.aria)
  const sectionsPresent = new Set(
    nodes.map(sectionOf).filter((s): s is Exclude<Section, undefined> => s !== undefined)
  )

  // [0] grouped into today / upcoming / already-celebrated. Data-driven: from
  // the full list + serverTime, compute WHICH of the three groups actually has
  // members, then require EACH such section's heading to be present. A group
  // with members but no heading → fail; a group with no members legitimately
  // renders no heading. Abstain when the full list is unavailable.
  out[0] = ((): ItemVerdict => {
    const expected = expectedProximitySections(fullList(cap), cap.serverTime.currentTime)
    if (expected === undefined) {
      return {
        verdict: 'unsure',
        reason: 'no full birthdays list — cannot compute expected sections',
      }
    }
    const missing = [...expected].filter(s => !sectionsPresent.has(s))
    if (missing.length > 0) {
      return {
        verdict: 'fail',
        citation: 'birthdays section headings',
        reason: `expected proximity section(s) missing: ${missing.join(', ')}`,
      }
    }
    if (expected.size === 0) {
      return { verdict: 'unsure', reason: 'no dated birthdays in the frame — grouping unprovable' }
    }
    return {
      verdict: 'pass',
      citation: `all expected sections present: ${[...expected].join(', ')}`,
    }
  })()

  // [1] gift-planning only near year end (recorded serverTime frame).
  out[1] = ((): ItemVerdict => {
    const month = new Date(cap.serverTime.currentTime).getUTCMonth() // 0=Jan
    const nearYearEnd = month >= 10 // November or December
    const giftShown = sectionsPresent.has('gift')
    if (giftShown && nearYearEnd)
      return { verdict: 'pass', citation: 'gift-planning section shown near year end' }
    if (giftShown && !nearYearEnd) {
      return {
        verdict: 'fail',
        citation: 'Gift Planning section',
        reason: 'gift-planning shown while NOT near year end',
      }
    }
    if (!giftShown && !nearYearEnd)
      return { verdict: 'pass', citation: 'gift-planning correctly hidden away from year end' }
    return {
      verdict: 'unsure',
      reason:
        'near year end but no gift-planning section — may just lack early-next-year birthdays',
    }
  })()

  // [2] upcoming soonest-first; celebrated after upcoming.
  out[2] = ((): ItemVerdict => {
    const upcomingDays: number[] = []
    let section: Section
    let upcomingIdx = -1
    let celebratedIdx = -1
    nodes.forEach((n, idx) => {
      const s = sectionOf(n)
      if (s) {
        section = s
        if (s === 'upcoming') upcomingIdx = idx
        if (s === 'celebrated') celebratedIdx = idx
      }
      if (section === 'upcoming') {
        const m = (n.text ?? '').match(/^(\d+)\s+days?$/)
        if (m) upcomingDays.push(Number(m[1]))
      }
    })
    if (upcomingDays.length === 0)
      return { verdict: 'unsure', reason: 'no upcoming birthday cards — no evidence' }
    const sorted = upcomingDays.every((d, i) => i === 0 || d >= upcomingDays[i - 1])
    const celebratedLast = celebratedIdx === -1 || celebratedIdx > upcomingIdx
    if (sorted && celebratedLast)
      return {
        verdict: 'pass',
        citation: 'upcoming days ascending; celebrated section after upcoming',
      }
    return {
      verdict: 'fail',
      citation: 'upcoming card order',
      reason: sorted
        ? 'celebrated section precedes upcoming'
        : 'upcoming birthdays are not soonest-first',
    }
  })()

  // [3] placeholder-year (1900) birthdays show no age.
  out[3] = ((): ItemVerdict => {
    const placeholders = fullList(cap)
      .filter(c => asString(c.birthday)?.startsWith('1900'))
      .map(c => asString(c.full_name))
      .filter((x): x is string => x !== undefined)
    if (placeholders.length === 0) {
      return {
        verdict: 'unsure',
        reason: 'no placeholder-year birthdays in the frame — no evidence',
      }
    }
    for (const name of placeholders) {
      const headingIdx = nodes.findIndex(
        n => n.role === 'heading' && n.level === 3 && n.name === name
      )
      if (headingIdx === -1) continue // not currently carded (e.g. no birthday-day match)
      // Scan the card subtree (until the next level-3 heading) for an age line.
      for (let i = headingIdx + 1; i < nodes.length; i++) {
        if (nodes[i].role === 'heading' && nodes[i].level === 3) break
        const t = nodes[i].text ?? ''
        if (/^Turn(ing|ed)\s+\d+/.test(t)) {
          return {
            verdict: 'fail',
            citation: `card for ${name}`,
            reason: 'a placeholder-year birthday displays an age',
          }
        }
      }
    }
    return {
      verdict: 'pass',
      citation: 'placeholder-year birthday cards show no "Turning/Turned" age',
    }
  })()

  // [4] the displayed frame follows accelerated time (not wall clock).
  out[4] = ((): ItemVerdict => {
    if (!cap.serverTime.isAccelerated) {
      return {
        verdict: 'unsure',
        reason:
          'serverTime frame is not accelerated — cannot prove the page follows accelerated time',
      }
    }
    const now = new Date(cap.serverTime.currentTime)
    const year = String(now.getUTCFullYear())
    const monthName = MONTH_NAMES[now.getUTCMonth()]
    const dateShown = nodes.some(n => {
      const t = n.text ?? n.name ?? ''
      return t.includes(year) && t.includes(monthName)
    })
    return dateShown
      ? {
          verdict: 'pass',
          citation: `page shows the accelerated frame date (${monthName} ${year})`,
        }
      : {
          verdict: 'unsure',
          reason: 'could not bind the displayed date to the accelerated serverTime frame',
        }
  })()

  return out
}
