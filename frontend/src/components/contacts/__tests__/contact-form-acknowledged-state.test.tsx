/* eslint-disable @typescript-eslint/no-explicit-any */
import { describe, it, expect, vi } from 'vitest'
import { act, createRef } from 'react'
import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { ContactForm, type MethodsReconciler } from '@/components/contacts/contact-form'
import type { MethodReconciliation } from '@/lib/contact-method-operations'
import type { Contact } from '@/types/contact'

const METHOD_A = 'aaaaaaaa-0000-4000-8000-000000000001'
const METHOD_B = 'bbbbbbbb-0000-4000-8000-000000000002'

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

  it('collapses two live rows that reconcile to one method_id', async () => {
    // A stored value and a submitted respelling can normalize alike under the
    // database trigger but not under the client's normalizer, so both rows are
    // valid client-side and both resolve to one server row. Stamping the id
    // onto both leaves the form showing one domain row twice; the next save
    // then derives two operations against one id, which the endpoint rejects as
    // a conflict the user can neither understand nor act on.
    const onSubmit = vi.fn()
    const reconcilerRef = createRef<MethodsReconciler | null>() as any
    const user = userEvent.setup()

    render(
      <ContactForm
        contact={contactWith([
          { id: METHOD_A, type: 'phone', value: '5555550100', is_primary: false },
        ])}
        onSubmit={onSubmit}
        reconcilerRef={reconcilerRef}
      />
    )

    await user.click(screen.getByRole('button', { name: 'Add method' }))
    await user.selectOptions(
      screen.getAllByRole('combobox', { name: 'Contact method type' })[1],
      'phone'
    )
    await user.type(methodValueInputs()[1], '+5555550100')
    await user.click(screen.getByRole('button', { name: 'Save Contact' }))
    await waitFor(() => expect(onSubmit).toHaveBeenCalled())

    expect(methodValueInputs()).toHaveLength(2)

    // Both submitted rows resolved to METHOD_A, whose stored value is the one
    // that was already there.
    const reconciliations: MethodReconciliation[] = [
      {
        submittedIndex: 0,
        method_id: METHOD_A,
        type: 'phone',
        value: '5555550100',
        is_primary: true,
      },
      {
        submittedIndex: 1,
        method_id: METHOD_A,
        type: 'phone',
        value: '5555550100',
        is_primary: true,
      },
    ]
    act(() => reconcilerRef.current?.(reconciliations))

    await waitFor(() => expect(methodValueInputs()).toHaveLength(1))
    // The survivor is the earlier row, carrying the RESOLVED snapshot rather
    // than either submitted value.
    expect(methodValueInputs()[0]).toHaveValue('5555550100')

    // And exactly one operation targets that id on the next save.
    onSubmit.mockClear()
    await user.click(screen.getByRole('button', { name: 'Save Contact' }))
    await waitFor(() => expect(onSubmit).toHaveBeenCalled())
    const methods = onSubmit.mock.calls[0][0].methods
    expect(methods).toHaveLength(1)
    expect(methods[0]).toMatchObject({ method_id: METHOD_A, is_primary: true })
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
