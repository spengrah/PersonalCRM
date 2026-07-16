import { describe, it, expect } from 'vitest'
import { runTrapSelfTest, selftestExitCode, type TrapResult } from './trap-selftest'
import { TRAPS, type TrapSpec } from './trap-config'
import type { Judge, JudgeInput, PerItemVerdict } from './adapter/types'
import { buildJudgeInput } from './judge-input'
import { buildIntentJudgeInput } from './intent-input'
import { buildPrompt } from './adapter/prompt'
import { allIntents } from './intent-catalog'
import { apiItem, cap, pair, root } from './grader/fixtures'
import type { Capture } from '../support/types'

const CON042_TRAP = TRAPS.find(t => t.targetBehavior === 'CON-042')!
const DSH004_TRAP = TRAPS.find(t => t.targetBehavior === 'DSH-004')!

// A CON-042 capture carrying the delete-confirm dialog the blank_dialog trap
// clears (the judge grades CON-042[0] "warns cannot be undone").
function con042Captures(): Capture[] {
  return [
    cap({
      behaviors: ['CON-042'],
      dialogs: [{ type: 'confirm', message: 'Are you sure? This action cannot be undone.' }],
    }),
  ]
}

// A DSH-004 group in REAL tour order: the `loading` capture (no overdue endpoint)
// FIRST, then the `error` capture, targeted by pair role 'error'. The order
// matters: an index-0-default mutation would land on the loading capture and
// no-op, so the trap MUST select the error capture by role.
//
// The error capture's overdue group leads with a warm 200 hit, THEN the retried
// 500 bracket (retry-1..retry-4). This is the length-varying shape that broke a
// fixed itemIndex:3 — with the leading 200 the group has FIVE entries, so index
// 3 lands on the PENULTIMATE 500 (retry-3), not the final one react-query
// surfaces. The trap's 'last-error' selection corrects to retry-4.
function dsh004Captures(): Capture[] {
  return [
    cap({
      behaviors: ['DSH-004'],
      pair: pair('dsh004', 'loading'),
      aria: root([{ role: 'text', text: 'Loading overdue contacts' }]),
    }),
    cap({
      behaviors: ['DSH-004'],
      pair: pair('dsh004', 'error'),
      // The aria shows the ORIGINAL surfaced reason — JSON-only doctoring never
      // touches it, so after the mutation the shown reason contradicts the
      // doctored API reason.
      aria: root([
        { role: 'heading', name: 'Error loading overdue contacts', level: 2 },
        { role: 'text', text: 'overdue fetch failed' },
      ]),
      apiResponses: {
        'GET /api/v1/contacts/overdue': [
          apiItem({ status: 200, body: { data: { overdue: [] } } }),
          apiItem({ status: 500, body: { error: { message: 'retry-1 failed' } } }),
          apiItem({ status: 500, body: { error: { message: 'retry-2 failed' } } }),
          apiItem({ status: 500, body: { error: { message: 'retry-3 failed' } } }),
          apiItem({ status: 500, body: { error: { message: 'retry-4 failed' } } }),
        ],
      },
    }),
  ]
}

// A mock RAW judge: records every input it receives, returns configured verdicts.
function mockJudge(verdicts: PerItemVerdict[]): { judge: Judge; calls: JudgeInput[] } {
  const calls: JudgeInput[] = []
  const judge: Judge = async input => {
    calls.push(input)
    return verdicts
  }
  return { judge, calls }
}

const v = (
  verdict: PerItemVerdict['verdict'],
  citation = 'cite',
  itemIndex = 0
): PerItemVerdict => ({
  itemIndex,
  verdict,
  citation,
  critique: 'c',
})

describe('runTrapSelfTest — the live detection self-test', () => {
  it('CAUGHT: a grounded fail on doctored evidence → caught, exit 0', async () => {
    const { judge } = mockJudge([v('fail', 'DIALOGS: (empty)')])
    const [r] = await runTrapSelfTest(con042Captures(), [CON042_TRAP], judge)
    expect(r.status).toBe('caught')
    expect(selftestExitCode([r])).toBe(0)
  })

  it('CAUGHT: the DSH-004 trap corrupts the FINAL error response (last-error targeting, robust to a leading 200)', async () => {
    const { judge, calls } = mockJudge([v('fail', 'API overdue error.message', 2)])
    const [r] = await runTrapSelfTest(dsh004Captures(), [DSH004_TRAP], judge)
    // `caught` (not a no-op `error`) proves role:'error' selected the ERROR
    // capture: had the mutation defaulted to capture index 0 (the loading
    // capture, which has no overdue endpoint), it would no-op and the liveness
    // guard would report `error` instead.
    expect(r.status).toBe('caught')
    expect(r.targetItem).toBe(2)

    const prompt = buildPrompt(calls[0])
    const doctored =
      DSH004_TRAP.mutation.op === 'set_json_field' ? String(DSH004_TRAP.mutation.value) : '<n/a>'
    // The doctored reason lands EXACTLY ONCE — on the response the UI renders.
    expect(prompt.split(doctored).length - 1).toBe(1)
    // Targeting proof: the FINAL 500's original reason (retry-4) is overwritten
    // and GONE, while the PENULTIMATE 500 (retry-3, which a fixed itemIndex:3
    // would have hit given the leading 200) SURVIVES. A mis-targeted trap fails
    // these two assertions.
    expect(prompt).not.toContain('retry-4 failed')
    expect(prompt).toContain('retry-3 failed')
    // The aria-shown surfaced reason survives (JSON-only doctoring) — the
    // manufactured contradiction the judge must fail on.
    expect(prompt).toContain('overdue fetch failed')
  })

  it('MISSED: the judge passes doctored evidence → missed, exit 1 (hard signal)', async () => {
    const { judge } = mockJudge([v('pass')])
    const [r] = await runTrapSelfTest(con042Captures(), [CON042_TRAP], judge)
    expect(r.status).toBe('missed')
    expect(selftestExitCode([r])).toBe(1)
  })

  it('MISSED: an unsure verdict is also a miss', async () => {
    const { judge } = mockJudge([v('unsure')])
    const [r] = await runTrapSelfTest(con042Captures(), [CON042_TRAP], judge)
    expect(r.status).toBe('missed')
  })

  it('GROUNDING PARITY: an UNCITED fail is downgraded → missed', async () => {
    const { judge } = mockJudge([v('fail', '')])
    const [r] = await runTrapSelfTest(con042Captures(), [CON042_TRAP], judge)
    expect(r.status).toBe('missed')
  })

  it('GROUNDING PARITY: a CITED fail is caught', async () => {
    const { judge } = mockJudge([v('fail', 'DIALOGS: (empty)')])
    const [r] = await runTrapSelfTest(con042Captures(), [CON042_TRAP], judge)
    expect(r.status).toBe('caught')
  })

  it('LIVENESS NO-OP: a mutation invisible to the judge prompt → error (not caught/missed)', async () => {
    // set_field mutates Capture.fields, which the judge lane never projects — the
    // rendered prompt is unchanged, so this is a silent no-op the guard catches.
    const noopTrap: TrapSpec = {
      id: 'trap-noop',
      targetBehavior: 'CON-042',
      targetItem: 0,
      mutation: { op: 'set_field', field: 'invisible', value: 1 },
      note: '',
    }
    const { judge, calls } = mockJudge([v('fail')])
    const [r] = await runTrapSelfTest(con042Captures(), [noopTrap], judge)
    expect(r.status).toBe('error')
    expect(r.reason).toMatch(/rendered prompt unchanged/)
    expect(calls).toHaveLength(0) // the judge is never consulted on a no-op
    expect(selftestExitCode([r])).toBe(1)
  })

  it('ABSENT TARGET: a trap whose behavior has no captures → error, exit 1 (not a skip)', async () => {
    const { judge } = mockJudge([v('fail')])
    // captures tag CON-042 only; the DSH-004 trap cannot execute.
    const [r] = await runTrapSelfTest(con042Captures(), [DSH004_TRAP], judge)
    expect(r.status).toBe('error')
    expect(r.reason).toMatch(/no captures for behavior DSH-004/)
    expect(selftestExitCode([r])).toBe(1)
  })

  it('ABSENT RESIDUE: a trap targeting a non-judge item → error', async () => {
    const badItem: TrapSpec = { ...CON042_TRAP, id: 'trap-bad-item', targetItem: 1 }
    const { judge } = mockJudge([v('fail', 'x', 1)])
    const [r] = await runTrapSelfTest(con042Captures(), [badItem], judge)
    expect(r.status).toBe('error')
    expect(r.reason).toMatch(/not judge residue/)
  })

  it('PER-TRAP EXCEPTION: a throwing judge is converted to error, never propagated', async () => {
    const judge: Judge = async () => {
      throw new Error('adapter blew up')
    }
    const results = await runTrapSelfTest(con042Captures(), [CON042_TRAP], judge)
    expect(results[0].status).toBe('error')
    expect(results[0].reason).toMatch(/threw: adapter blew up/)
    expect(selftestExitCode(results)).toBe(1)
  })

  it('MUTATION CARRIER: the raw judge receives __trap.mutation on its input', async () => {
    const { judge, calls } = mockJudge([v('fail', 'DIALOGS')])
    await runTrapSelfTest(con042Captures(), [CON042_TRAP], judge)
    expect(calls).toHaveLength(1)
    expect(calls[0].__trap?.mutation).toEqual(CON042_TRAP.mutation)
  })

  it('runs every configured trap, one result each', async () => {
    const { judge } = mockJudge([v('fail', 'DIALOGS'), v('fail', 'API', 2)])
    const results = await runTrapSelfTest([...con042Captures(), ...dsh004Captures()], TRAPS, judge)
    expect(results).toHaveLength(TRAPS.length)
  })
})

describe('LEAK GUARD: normal inputs never carry __trap', () => {
  it('buildJudgeInput sets no __trap', () => {
    const input = buildJudgeInput('CON-042', con042Captures())
    expect(input?.__trap).toBeUndefined()
  })

  it('buildIntentJudgeInput sets no __trap', () => {
    const input = buildIntentJudgeInput(allIntents()[0], con042Captures())
    expect(input.__trap).toBeUndefined()
  })
})

describe('selftestExitCode', () => {
  const r = (status: TrapResult['status']): TrapResult => ({
    id: 't',
    targetBehavior: 'CON-042',
    targetItem: 0,
    status,
    reason: '',
  })

  it('0 iff all caught', () => {
    expect(selftestExitCode([r('caught'), r('caught')])).toBe(0)
    expect(selftestExitCode([])).toBe(0)
  })

  it('1 on any missed or error', () => {
    expect(selftestExitCode([r('caught'), r('missed')])).toBe(1)
    expect(selftestExitCode([r('caught'), r('error')])).toBe(1)
  })
})
