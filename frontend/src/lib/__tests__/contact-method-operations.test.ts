import { describe, it, expect } from 'vitest'
import {
  advanceAcknowledgedMethods,
  deriveMethodOperations,
  deriveMethodOperationsWithOrigins,
  initializeAcknowledgedMethods,
  reconciliationsFromResults,
  type AcknowledgedMethod,
  type SubmittedMethod,
} from '@/lib/contact-method-operations'
import type {
  ContactMethodOperation,
  ContactMethodOperationResult,
} from '@/types/generated/contact'

const A = 'aaaaaaaa-0000-4000-8000-000000000001'
const B = 'bbbbbbbb-0000-4000-8000-000000000002'
const C = 'cccccccc-0000-4000-8000-000000000003'
const D = 'dddddddd-0000-4000-8000-000000000004'

function ack(
  method_id: string,
  type: AcknowledgedMethod['type'],
  value: string,
  is_primary = false
): AcknowledgedMethod {
  return { method_id, type, value, is_primary }
}

function row(
  type: SubmittedMethod['type'],
  value: string,
  extra: Partial<SubmittedMethod> = {}
): SubmittedMethod {
  return { type, value, is_primary: false, ...extra }
}

function result(
  index: number,
  outcome: string,
  method_id: string,
  method?: { id: string; type: string; value: string; is_primary: boolean }
): ContactMethodOperationResult {
  return {
    index,
    outcome,
    method_id,
    ...(method ? { method: method as ContactMethodOperationResult['method'] } : {}),
  }
}

describe('deriveMethodOperations', () => {
  it('emits add for a new row', () => {
    expect(deriveMethodOperations([], [row('email', 'new@example.test')])).toEqual([
      { op: 'add', type: 'email', value: 'new@example.test' },
    ])
  })

  it('emits remove for a deleted row', () => {
    expect(deriveMethodOperations([ack(A, 'email', 'a@example.test')], [])).toEqual([
      { op: 'remove', method_id: A },
    ])
  })

  it('emits update for a changed value', () => {
    expect(
      deriveMethodOperations(
        [ack(A, 'email', 'old@example.test')],
        [row('email', 'new@example.test', { method_id: A })]
      )
    ).toEqual([{ op: 'update', method_id: A, type: 'email', value: 'new@example.test' }])
  })

  it('emits update when only the type changed', () => {
    expect(
      deriveMethodOperations(
        [ack(A, 'telegram', 'handle')],
        [row('discord', 'handle', { method_id: A })]
      )
    ).toEqual([{ op: 'update', method_id: A, type: 'discord', value: 'handle' }])
  })

  it('emits no operation for an untouched row', () => {
    expect(
      deriveMethodOperations(
        [ack(A, 'email', 'a@example.test')],
        [row('email', 'a@example.test', { method_id: A })]
      )
    ).toEqual([])
  })

  it('emits an empty result when nothing changed', () => {
    const acknowledged = [ack(A, 'email', 'a@example.test', true), ack(B, 'phone', '5555550100')]
    const submitted = [
      row('email', 'a@example.test', { method_id: A, is_primary: true }),
      row('phone', '5555550100', { method_id: B }),
    ]
    expect(deriveMethodOperations(acknowledged, submitted)).toEqual([])
  })

  it('emits set_primary only when the designation changed', () => {
    const acknowledged = [ack(A, 'email', 'a@example.test', true), ack(B, 'phone', '5555550100')]

    // Designation unchanged.
    expect(
      deriveMethodOperations(acknowledged, [
        row('email', 'a@example.test', { method_id: A, is_primary: true }),
        row('phone', '5555550100', { method_id: B }),
      ])
    ).toEqual([])

    // Moved to B.
    expect(
      deriveMethodOperations(acknowledged, [
        row('email', 'a@example.test', { method_id: A }),
        row('phone', '5555550100', { method_id: B, is_primary: true }),
      ])
    ).toEqual([{ op: 'set_primary', method_id: B }])
  })

  it('emits clear_primary when the sole primary is toggled off', () => {
    expect(
      deriveMethodOperations(
        [ack(A, 'email', 'a@example.test', true)],
        [row('email', 'a@example.test', { method_id: A })]
      )
    ).toEqual([{ op: 'clear_primary', method_id: A }])
  })

  it('suppresses clear_primary when the same row is being removed', () => {
    // remove(A) already leaves no primary, so a clear alongside it is
    // redundant AND rejected by the endpoint as self-conflicting.
    expect(deriveMethodOperations([ack(A, 'email', 'a@example.test', true)], [])).toEqual([
      { op: 'remove', method_id: A },
    ])
  })

  it('carries is_primary on an add, since a new row has no id to name', () => {
    expect(
      deriveMethodOperations(
        [ack(A, 'email', 'a@example.test', true)],
        [
          row('email', 'a@example.test', { method_id: A }),
          row('phone', '5555550100', { is_primary: true }),
        ]
      )
    ).toEqual([{ op: 'add', type: 'phone', value: '5555550100', is_primary: true }])
  })

  it('never names a method absent from the acknowledged state', () => {
    // The structural guarantee. An unseen server method B is simply not in the
    // acknowledged state, so no operation can mention it whatever the form does.
    const acknowledged = [ack(A, 'email', 'a@example.test')]
    const operations = deriveMethodOperations(acknowledged, [
      row('email', 'edited@example.test', { method_id: A }),
      row('phone', '5555550100'),
    ])
    expect(JSON.stringify(operations)).not.toContain(B)
    expect(operations).toEqual([
      { op: 'update', method_id: A, type: 'email', value: 'edited@example.test' },
      { op: 'add', type: 'phone', value: '5555550100' },
    ])
  })

  it('treats a submitted id the acknowledged state does not know as a new row', () => {
    expect(deriveMethodOperations([], [row('email', 'x@example.test', { method_id: C })])).toEqual([
      { op: 'add', type: 'email', value: 'x@example.test' },
    ])
  })

  // --- comparison representation --------------------------------------------

  it('emits no operation when a stored @-prefixed handle round-trips unchanged', () => {
    // Stored '@foo' is displayed '@foo' and submitted 'foo'. Normalizing the
    // acknowledged side at initialization absorbs the difference; without it
    // this manufactures a spurious update on every save.
    const acknowledged = initializeAcknowledgedMethods([
      { id: A, type: 'telegram', value: '@foo', is_primary: false },
    ])
    expect(acknowledged[0].value).toBe('foo')
    expect(
      deriveMethodOperations(acknowledged, [row('telegram', 'foo', { method_id: A })])
    ).toEqual([])
  })

  it('emits update when the value differs only by letter case', () => {
    // The intent question, not the collision question. An equivalence
    // normalizer would call these identical and silently discard the edit.
    expect(
      deriveMethodOperations(
        [ack(A, 'email', 'Case@Example.test')],
        [row('email', 'case@example.test', { method_id: A })]
      )
    ).toEqual([{ op: 'update', method_id: A, type: 'email', value: 'case@example.test' }])
  })

  it('emits update when a phone is respelled equivalently', () => {
    expect(
      deriveMethodOperations(
        [ack(A, 'phone', '5555550100')],
        [row('phone', '(555) 555-0100', { method_id: A })]
      )
    ).toEqual([{ op: 'update', method_id: A, type: 'phone', value: '(555) 555-0100' }])
  })
})

describe('deriveMethodOperationsWithOrigins', () => {
  it('reports the submitted row behind each add and update, and -1 otherwise', () => {
    const { operations, submittedIndexes } = deriveMethodOperationsWithOrigins(
      [ack(A, 'email', 'a@example.test', true), ack(B, 'phone', '5555550100')],
      [
        row('email', 'edited@example.test', { method_id: A }),
        row('discord', 'handle', { is_primary: true }),
      ]
    )
    expect(operations.map(op => op.op)).toEqual(['remove', 'update', 'add'])
    expect(submittedIndexes).toEqual([-1, 0, 1])
  })
})

describe('advanceAcknowledgedMethods', () => {
  it('advances from results for this client’s own operations', () => {
    const operations: ContactMethodOperation[] = [{ op: 'add', type: 'phone', value: '5555550100' }]
    const next = advanceAcknowledgedMethods([ack(A, 'email', 'a@example.test')], operations, [
      result(0, 'created', B, { id: B, type: 'phone', value: '5555550100', is_primary: false }),
    ])
    expect(next).toEqual([ack(A, 'email', 'a@example.test'), ack(B, 'phone', '5555550100')])
  })

  it('does not absorb response methods no operation resolved to', () => {
    // The response's `methods` array carries an unseen row. Only `results` may
    // teach the acknowledged state anything, so the unseen row must never
    // appear — and therefore can never be named for removal later.
    const operations: ContactMethodOperation[] = [
      { op: 'update', method_id: A, type: 'email', value: 'edited@example.test' },
    ]
    const next = advanceAcknowledgedMethods([ack(A, 'email', 'a@example.test')], operations, [
      result(0, 'updated', A, {
        id: A,
        type: 'email',
        value: 'edited@example.test',
        is_primary: false,
      }),
    ])
    expect(next.map(m => m.method_id)).toEqual([A])

    // And the derivation that follows names nothing about the unseen row.
    const operationsAfter = deriveMethodOperations(next, [
      row('email', 'edited@example.test', { method_id: A }),
    ])
    expect(operationsAfter).toEqual([])
  })

  it('retires an id on a confirmed removal', () => {
    const operations: ContactMethodOperation[] = [{ op: 'remove', method_id: B }]
    const next = advanceAcknowledgedMethods(
      [ack(A, 'email', 'a@example.test'), ack(B, 'phone', '5555550100')],
      operations,
      [result(0, 'removed', B)]
    )
    expect(next.map(m => m.method_id)).toEqual([A])
  })

  it('demotes the previous primary when a snapshot promotes another row', () => {
    const operations: ContactMethodOperation[] = [{ op: 'set_primary', method_id: B }]
    const next = advanceAcknowledgedMethods(
      [ack(A, 'email', 'a@example.test', true), ack(B, 'phone', '5555550100')],
      operations,
      [result(0, 'updated', B, { id: B, type: 'phone', value: '5555550100', is_primary: true })]
    )
    expect(next).toEqual([
      ack(A, 'email', 'a@example.test', false),
      ack(B, 'phone', '5555550100', true),
    ])
  })

  // --- same-id collapse, stated as a property -------------------------------

  describe('same-id collapse', () => {
    // Two form rows resolve to one server row: a stored '1234567890' and a
    // submitted '+1234567890' normalize alike under the database trigger but
    // NOT under the client's own normalizer, so frontend validation sees two
    // distinct values and the endpoint resolves the add to the existing row.
    const snapshot = { id: A, type: 'phone', value: '5555550100', is_primary: true }

    const operations: ContactMethodOperation[] = [
      { op: 'update', method_id: A, type: 'phone', value: '5555550100' },
      { op: 'add', type: 'phone', value: '+5555550100' },
    ]
    const results = [result(0, 'updated', A, snapshot), result(1, 'matched_existing', A, snapshot)]

    it('collapses an update and an add resolving to one method_id', () => {
      // Named for the fixture it actually uses. The two-adds case is below.
      const next = advanceAcknowledgedMethods([ack(A, 'phone', 'old')], operations, results)
      expect(next).toHaveLength(1)
      expect(next[0].method_id).toBe(A)
    })

    it('collapses two coalesced adds resolving to one method_id', () => {
      // The case the endpoint produces when two submitted values normalize
      // alike under the trigger: both adds resolve to one row. An
      // implementation handling only update+add would miss this entirely.
      const addOps: ContactMethodOperation[] = [
        { op: 'add', type: 'phone', value: '5555550100' },
        { op: 'add', type: 'phone', value: '+5555550100' },
      ]
      const addResults = [
        result(0, 'created', A, snapshot),
        result(1, 'matched_existing', A, snapshot),
      ]
      const next = advanceAcknowledgedMethods([], addOps, addResults)
      expect(next).toHaveLength(1)
      expect(next[0]).toEqual(ack(A, 'phone', '5555550100', true))
    })

    it('takes the surviving row’s value and primary flag from the resolved snapshot', () => {
      const next = advanceAcknowledgedMethods([ack(A, 'phone', 'old')], operations, results)
      // Neither submitted value: '5555550100' is what the server stored.
      expect(next[0]).toEqual(ack(A, 'phone', '5555550100', true))
    })

    it('upserts in place rather than moving the row to the end', () => {
      // The collapsing row must NOT be last, or position is unobservable:
      // with [C, A] an implementation that removes and re-appends also yields
      // [C, A], and the assertion cannot distinguish it from upsert-in-place.
      const acknowledged = [
        ack(C, 'email', 'c@example.test'),
        ack(A, 'phone', 'old'),
        ack(D, 'telegram', 'handle'),
      ]
      const next = advanceAcknowledgedMethods(acknowledged, operations, results)
      expect(next.map(m => m.method_id)).toEqual([C, A, D])
    })

    it('is insensitive to result order, which the identical-snapshot contract makes safe', () => {
      // Narrowed deliberately. This permutes the RESULTS array only, and both
      // results for one id carry the same snapshot by the backend contract
      // (they are read from a single post-apply state), so last-wins,
      // first-wins, and index-ordered implementations are all equivalent here.
      // Claiming "independent of submission order" would overstate it — that
      // property is carried by the position test above plus the server's own
      // order-independence, not by this assertion.
      expect(results[0].method).toEqual(results[1].method)

      const acknowledged = [ack(A, 'phone', 'old')]
      const forward = advanceAcknowledgedMethods(acknowledged, operations, results)
      const reversed = advanceAcknowledgedMethods(acknowledged, operations, [...results].reverse())
      expect(forward).toEqual(reversed)
    })

    it('never lets a method_id appear twice, for either payload order', () => {
      // Genuinely permutes the OPERATIONS and reindexes the results to match,
      // rather than reversing the results against a fixed payload. An
      // implementation that collapses update-then-add but not add-then-update
      // passes the weaker version.
      const acknowledged = [ack(A, 'phone', 'old')]

      const updateThenAdd: ContactMethodOperation[] = [
        { op: 'update', method_id: A, type: 'phone', value: '5555550100' },
        { op: 'add', type: 'phone', value: '+5555550100' },
      ]
      const addThenUpdate: ContactMethodOperation[] = [
        { op: 'add', type: 'phone', value: '+5555550100' },
        { op: 'update', method_id: A, type: 'phone', value: '5555550100' },
      ]
      const payloads: Array<[ContactMethodOperation[], ContactMethodOperationResult[]]> = [
        [
          updateThenAdd,
          [result(0, 'updated', A, snapshot), result(1, 'matched_existing', A, snapshot)],
        ],
        [
          addThenUpdate,
          [result(0, 'matched_existing', A, snapshot), result(1, 'updated', A, snapshot)],
        ],
      ]

      for (const [ops, res] of payloads) {
        const next = advanceAcknowledgedMethods(acknowledged, ops, res)
        const ids = next.map(m => m.method_id)
        expect(new Set(ids).size).toBe(ids.length)
        expect(next).toHaveLength(1)
      }
    })

    it('coalesces duplicate removes of one id without leaving a stray row', () => {
      const removeOps: ContactMethodOperation[] = [
        { op: 'remove', method_id: A },
        { op: 'remove', method_id: A },
      ]
      const next = advanceAcknowledgedMethods(
        [ack(A, 'phone', '5555550100'), ack(B, 'email', 'b@example.test')],
        removeOps,
        [result(0, 'removed', A), result(1, 'no_op', A)]
      )
      expect(next.map(m => m.method_id)).toEqual([B])
    })
  })

  // --- the revert pins ------------------------------------------------------

  it('emits an update back to the original value after a reverted change', () => {
    // Acknowledged A = x. The user saves A -> y; the methods step succeeds and
    // notes fail. The user reverts to x. A frozen edit-start baseline would see
    // x == x, emit nothing, report success, and leave y on the server.
    const acknowledged = [ack(A, 'email', 'x@example.test')]
    const operations: ContactMethodOperation[] = [
      { op: 'update', method_id: A, type: 'email', value: 'y@example.test' },
    ]
    const advanced = advanceAcknowledgedMethods(acknowledged, operations, [
      result(0, 'updated', A, { id: A, type: 'email', value: 'y@example.test', is_primary: false }),
    ])

    expect(
      deriveMethodOperations(advanced, [row('email', 'x@example.test', { method_id: A })])
    ).toEqual([{ op: 'update', method_id: A, type: 'email', value: 'x@example.test' }])
  })

  it('emits the primary designation back after a reverted designation', () => {
    const acknowledged = [ack(A, 'email', 'a@example.test', true), ack(B, 'phone', '5555550100')]
    const operations: ContactMethodOperation[] = [{ op: 'set_primary', method_id: B }]
    const advanced = advanceAcknowledgedMethods(acknowledged, operations, [
      result(0, 'updated', B, { id: B, type: 'phone', value: '5555550100', is_primary: true }),
    ])

    expect(
      deriveMethodOperations(advanced, [
        row('email', 'a@example.test', { method_id: A, is_primary: true }),
        row('phone', '5555550100', { method_id: B }),
      ])
    ).toEqual([{ op: 'set_primary', method_id: A }])
  })
})

describe('reconciliationsFromResults', () => {
  it('resolves every snapshot-bearing result back to its submitted row', () => {
    // A primary designation's origin is the row it addresses, NOT -1. This test
    // previously encoded the opposite and so positively endorsed the defect it
    // was meant to guard: deleting primary-result reconciliation left it green.
    const operations: ContactMethodOperation[] = [
      { op: 'remove', method_id: C },
      { op: 'add', type: 'phone', value: '5555550100' },
      { op: 'set_primary', method_id: A },
    ]
    const submittedIndexes = [-1, 1, 0]
    const results = [
      result(0, 'removed', C),
      result(1, 'created', B, { id: B, type: 'phone', value: '5555550100', is_primary: false }),
      result(2, 'updated', A, { id: A, type: 'email', value: 'a@example.test', is_primary: true }),
    ]

    expect(reconciliationsFromResults(operations, results, submittedIndexes)).toEqual([
      { submittedIndex: 1, method_id: B, type: 'phone', value: '5555550100', is_primary: false },
      // The primary designation's snapshot reaches the live row too. Dropping
      // it is what let the form and the acknowledged state hold different
      // values for one row.
      { submittedIndex: 0, method_id: A, type: 'email', value: 'a@example.test', is_primary: true },
    ])
  })

  it('carries a clear_primary snapshot to its row as well', () => {
    const operations: ContactMethodOperation[] = [{ op: 'clear_primary', method_id: A }]
    const results = [
      result(0, 'updated', A, { id: A, type: 'email', value: 'a@example.test', is_primary: false }),
    ]
    expect(reconciliationsFromResults(operations, results, [0])).toEqual([
      {
        submittedIndex: 0,
        method_id: A,
        type: 'email',
        value: 'a@example.test',
        is_primary: false,
      },
    ])
  })

  it('drops only removals, whose row is absent from the submitted set', () => {
    const operations: ContactMethodOperation[] = [{ op: 'remove', method_id: C }]
    expect(reconciliationsFromResults(operations, [result(0, 'removed', C)], [-1])).toEqual([])
  })
})

// The class-level invariant behind three consecutive review findings. Each was
// the same defect: a path that updates ONE of the two structures that must
// agree. Server data reaches them at exactly two points — edit-start (both
// seeded together from contact.methods) and result snapshots — and a snapshot
// reaches the live rows only when its operation reports a submitted index. So
// the whole class reduces to one checkable rule.
describe('the submitted-index contract', () => {
  it('reports -1 only for remove, across every derivation shape', () => {
    const scenarios: Array<{
      name: string
      acknowledged: AcknowledgedMethod[]
      submitted: SubmittedMethod[]
    }> = [
      { name: 'pure add', acknowledged: [], submitted: [row('email', 'n@example.test')] },
      {
        name: 'pure update',
        acknowledged: [ack(A, 'email', 'old@example.test')],
        submitted: [row('email', 'new@example.test', { method_id: A })],
      },
      { name: 'pure remove', acknowledged: [ack(A, 'email', 'a@example.test')], submitted: [] },
      {
        name: 'set_primary onto an existing row',
        acknowledged: [ack(A, 'email', 'a@example.test', true), ack(B, 'phone', '5555550100')],
        submitted: [
          row('email', 'a@example.test', { method_id: A }),
          row('phone', '5555550100', { method_id: B, is_primary: true }),
        ],
      },
      {
        name: 'clear_primary on the sole primary',
        acknowledged: [ack(A, 'email', 'a@example.test', true)],
        submitted: [row('email', 'a@example.test', { method_id: A })],
      },
      {
        name: 'primary designation on a brand-new row',
        acknowledged: [ack(A, 'email', 'a@example.test', true)],
        submitted: [
          row('email', 'a@example.test', { method_id: A }),
          row('phone', '5555550100', { is_primary: true }),
        ],
      },
      {
        name: 'remove alongside an add and an update',
        acknowledged: [ack(A, 'email', 'a@example.test'), ack(B, 'phone', '5555550100')],
        submitted: [
          row('email', 'edited@example.test', { method_id: A }),
          row('telegram', 'handle'),
        ],
      },
      {
        name: 'submitted row carrying an id the acknowledged state does not know',
        acknowledged: [],
        submitted: [row('email', 'x@example.test', { method_id: C })],
      },
    ]

    for (const { name, acknowledged, submitted } of scenarios) {
      const { operations, submittedIndexes } = deriveMethodOperationsWithOrigins(
        acknowledged,
        submitted
      )
      expect(operations.length, `${name}: produced no operations to check`).toBeGreaterThan(0)
      operations.forEach((operation, index) => {
        if (operation.op === 'remove') {
          expect(submittedIndexes[index], `${name}: remove must be -1`).toBe(-1)
        } else {
          expect(
            submittedIndexes[index],
            `${name}: ${operation.op} must name the live row it addresses, or its snapshot never reaches the form`
          ).toBeGreaterThanOrEqual(0)
        }
      })
    }
  })
})

describe('initializeAcknowledgedMethods', () => {
  it('drops methods with no server id', () => {
    expect(
      initializeAcknowledgedMethods([
        { id: A, type: 'email', value: 'a@example.test', is_primary: true },
        { type: 'phone', value: '5555550100', is_primary: false },
      ])
    ).toEqual([ack(A, 'email', 'a@example.test', true)])
  })
})
