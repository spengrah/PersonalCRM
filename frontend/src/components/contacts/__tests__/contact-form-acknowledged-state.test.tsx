/* eslint-disable @typescript-eslint/no-explicit-any */
import { describe, it, expect, vi } from 'vitest'
import { act, createRef } from 'react'
import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { ContactForm, type MethodsReconciler } from '@/components/contacts/contact-form'
import {
  deriveMethodOperations,
  initializeAcknowledgedMethods,
} from '@/lib/contact-method-operations'
import type { Contact } from '@/types/contact'

const METHOD_A = 'aaaaaaaa-0000-4000-8000-000000000001'
const METHOD_B = 'bbbbbbbb-0000-4000-8000-000000000002'
const METHOD_E = 'eeeeeeee-0000-4000-8000-000000000005'

function contactWith(methods: Contact['methods']): Contact {
  return {
    id: '11111111-0000-4000-8000-000000000000',
    full_name: 'Test Person',
    methods,
    has_pending_followup: false,
    created_at: '2026-01-01T00:00:00Z',
    updated_at: '2026-01-01T00:00:00Z',
  } as Contact
}

function methodValueInputs() {
  return screen.getAllByRole('textbox', { name: 'Contact method value' })
}

describe('ContactForm method identity', () => {
  it('carries the server id through mount, edit, and submit', async () => {
    // The collision this pins is quiet: useFieldArray is called without a
    // custom keyName, so react-hook-form OVERWRITES fields[].id with its own
    // generated key. A server id threaded as `id` would arrive here as that
    // generated key, name a method that does not exist, and turn every update
    // and removal into a 400. A derivation-only test cannot see this — the
    // collision happens inside the form.
    const onSubmit = vi.fn()
    const user = userEvent.setup()

    render(
      <ContactForm
        contact={contactWith([
          { id: METHOD_A, type: 'email', value: 'a@example.test', is_primary: false },
        ])}
        onSubmit={onSubmit}
      />
    )

    await user.type(methodValueInputs()[0], 'x')
    await user.click(screen.getByRole('button', { name: 'Save Contact' }))

    await waitFor(() => expect(onSubmit).toHaveBeenCalled())
    expect(onSubmit.mock.calls[0][0].methods).toEqual([
      { method_id: METHOD_A, type: 'email', value: 'a@example.testx', is_primary: false },
    ])
  })

  it("collapses a new row that resolves to an existing row's method_id", async () => {
    // The REAL collision sequence, and the fixture matters more than the
    // assertions here. A stored '5555550100' and a submitted '+5555550100'
    // normalize alike under the database trigger but not under the client's
    // normalizer, so frontend validation accepts both and the endpoint resolves
    // the add to the existing row.
    //
    // Critically, the existing row is UNCHANGED, so it emits no operation and
    // therefore receives no result and no reconciliation. Only the add is
    // reconciled. An earlier version of this test hand-supplied a
    // reconciliation for the unchanged row too — an input the endpoint cannot
    // produce — and so could not see that ownership was never seeded from the
    // live rows. Both rows kept the id, and deleting either one then emitted no
    // removal at all: the save reported success while the deletion silently did
    // not persist.
    const onSubmit = vi.fn()
    const reconcilerRef = createRef<MethodsReconciler | null>() as any
    const user = userEvent.setup()

    render(
      <ContactForm
        contact={contactWith([
          { id: METHOD_E, type: 'email', value: 'e@example.test', is_primary: false },
          { id: METHOD_A, type: 'phone', value: '5555550100', is_primary: false },
        ])}
        onSubmit={onSubmit}
        reconcilerRef={reconcilerRef}
      />
    )

    // Rows sort email-then-phone, so the stored phone row is index 1.
    expect(methodValueInputs()).toHaveLength(2)
    await user.click(screen.getByRole('button', { name: 'Add method' }))
    await user.selectOptions(
      screen.getAllByRole('combobox', { name: 'Contact method type' })[2],
      'phone'
    )
    await user.type(methodValueInputs()[2], '+5555550100')
    await user.click(screen.getByRole('button', { name: 'Save Contact' }))
    await waitFor(() => expect(onSubmit).toHaveBeenCalled())

    // Only the add produced an operation, so only the add gets a result.
    act(() =>
      reconcilerRef.current?.([
        {
          submittedIndex: 2,
          method_id: METHOD_A,
          type: 'phone',
          value: '5555550100',
          is_primary: false,
        },
      ])
    )

    await waitFor(() => expect(methodValueInputs()).toHaveLength(2))
    // The survivor is the EARLIER row carrying that id — the pre-existing one,
    // not the row the result addressed — carrying the resolved snapshot.
    expect(methodValueInputs()[1]).toHaveValue('5555550100')

    // And the collapse is what makes deletion expressible again: removing the
    // one remaining phone row leaves no submitted method carrying METHOD_A, so
    // the derivation can emit remove(METHOD_A).
    onSubmit.mockClear()
    await user.click(screen.getAllByRole('button', { name: 'Remove' })[1])
    await user.click(screen.getByRole('button', { name: 'Save Contact' }))
    await waitFor(() => expect(onSubmit).toHaveBeenCalled())

    const methods = onSubmit.mock.calls[0][0].methods
    expect(methods.map((m: { method_id?: string }) => m.method_id)).not.toContain(METHOD_A)
    expect(
      deriveMethodOperations(
        initializeAcknowledgedMethods([
          { id: METHOD_E, type: 'email', value: 'e@example.test', is_primary: false },
          { id: METHOD_A, type: 'phone', value: '5555550100', is_primary: false },
        ]),
        methods
      )
    ).toEqual([{ op: 'remove', method_id: METHOD_A }])
  })

  it('keeps the collapsed row at the earlier form position', async () => {
    // The survivor rule is "earliest form row", and it needs a configuration
    // where first-vs-last is observable: the colliding pair must not be the
    // last rows in the list. A PRIMARY phone sorts ahead of the email, so the
    // pre-existing alias sits at index 0 and the new one at index 2. Keeping
    // the last row instead would drop index 0 and leave the list reordered.
    const onSubmit = vi.fn()
    const reconcilerRef = createRef<MethodsReconciler | null>() as any
    const user = userEvent.setup()

    render(
      <ContactForm
        contact={contactWith([
          { id: METHOD_A, type: 'phone', value: '5555550100', is_primary: true },
          { id: METHOD_E, type: 'email', value: 'e@example.test', is_primary: false },
        ])}
        onSubmit={onSubmit}
        reconcilerRef={reconcilerRef}
      />
    )

    expect(methodValueInputs()[0]).toHaveValue('5555550100')
    expect(methodValueInputs()[1]).toHaveValue('e@example.test')

    await user.click(screen.getByRole('button', { name: 'Add method' }))
    await user.selectOptions(
      screen.getAllByRole('combobox', { name: 'Contact method type' })[2],
      'phone'
    )
    await user.type(methodValueInputs()[2], '+5555550100')
    await user.click(screen.getByRole('button', { name: 'Save Contact' }))
    await waitFor(() => expect(onSubmit).toHaveBeenCalled())

    act(() =>
      reconcilerRef.current?.([
        {
          submittedIndex: 2,
          method_id: METHOD_A,
          type: 'phone',
          value: '5555550100',
          is_primary: true,
        },
      ])
    )

    await waitFor(() => expect(methodValueInputs()).toHaveLength(2))
    // Order preserved: the survivor is the row that was already at index 0.
    expect(methodValueInputs()[0]).toHaveValue('5555550100')
    expect(methodValueInputs()[1]).toHaveValue('e@example.test')
  })

  it('writes back a confirmed id without counting as a user edit', async () => {
    // A programmatic reconciliation is not a form change. Treating it as one
    // would invalidate the save session it just completed, and could loop.
    const onSubmit = vi.fn()
    const onFormEdit = vi.fn()
    const reconcilerRef = createRef<MethodsReconciler | null>() as any
    const user = userEvent.setup()

    render(
      <ContactForm
        contact={contactWith([])}
        onSubmit={onSubmit}
        reconcilerRef={reconcilerRef}
        onFormEdit={onFormEdit}
      />
    )

    await user.selectOptions(screen.getByRole('combobox', { name: 'Contact method type' }), 'email')
    await user.type(methodValueInputs()[0], 'new@example.test')
    await user.click(screen.getByRole('button', { name: 'Save Contact' }))
    await waitFor(() => expect(onSubmit).toHaveBeenCalled())

    onFormEdit.mockClear()
    act(() =>
      reconcilerRef.current?.([
        {
          submittedIndex: 0,
          method_id: METHOD_B,
          type: 'email',
          value: 'new@example.test',
          is_primary: false,
        },
      ])
    )

    await waitFor(() => expect(methodValueInputs()[0]).toHaveValue('new@example.test'))
    expect(onFormEdit).not.toHaveBeenCalled()

    // The id did land — otherwise "no edit reported" would be trivially true.
    onSubmit.mockClear()
    await user.click(screen.getByRole('button', { name: 'Save Contact' }))
    await waitFor(() => expect(onSubmit).toHaveBeenCalled())
    expect(onSubmit.mock.calls[0][0].methods[0].method_id).toBe(METHOD_B)

    // A real user edit still reports.
    await user.type(methodValueInputs()[0], 'x')
    expect(onFormEdit).toHaveBeenCalled()
  })

  it('does not report the late-arriving notes sync as a user edit', async () => {
    // initialNotes lands when the note query resolves, which can be after edit
    // mode opened and after a save has begun. It is server data, not a user
    // edit; reporting it would invalidate a save session for a keystroke the
    // user never made.
    const onFormEdit = vi.fn()
    const { rerender } = render(
      <ContactForm
        contact={contactWith([])}
        onSubmit={vi.fn()}
        onFormEdit={onFormEdit}
        initialNotes=""
      />
    )
    onFormEdit.mockClear()

    rerender(
      <ContactForm
        contact={contactWith([])}
        onSubmit={vi.fn()}
        onFormEdit={onFormEdit}
        initialNotes="a note that arrived late"
      />
    )

    await waitFor(() =>
      expect(screen.getByLabelText(/Notes/)).toHaveValue('a note that arrived late')
    )
    expect(onFormEdit).not.toHaveBeenCalled()
  })

  it('reports array-shape edits the user makes, which are not input change events', async () => {
    const onFormEdit = vi.fn()
    const user = userEvent.setup()

    render(<ContactForm contact={contactWith([])} onSubmit={vi.fn()} onFormEdit={onFormEdit} />)

    await user.click(screen.getByRole('button', { name: 'Add method' }))
    expect(onFormEdit).toHaveBeenCalled()

    onFormEdit.mockClear()
    await user.click(screen.getAllByRole('button', { name: 'Remove' })[1])
    expect(onFormEdit).toHaveBeenCalled()

    onFormEdit.mockClear()
    await user.click(screen.getByRole('button', { name: /Set as primary|Primary contact method/ }))
    expect(onFormEdit).toHaveBeenCalled()
  })
})
