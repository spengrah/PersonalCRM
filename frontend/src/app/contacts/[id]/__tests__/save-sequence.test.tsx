/* eslint-disable @typescript-eslint/no-explicit-any */
import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import type { Contact } from '@/types/contact'

vi.mock('@/hooks/use-contacts', () => ({
  useContact: vi.fn(),
  useContactIDs: vi.fn(),
  usePrefetchContact: vi.fn(),
  useUpdateContact: vi.fn(),
  useApplyMethodOperations: vi.fn(),
  useDeleteContact: vi.fn(),
}))
vi.mock('@/hooks/use-contact-note', () => ({
  useContactNote: vi.fn(),
  useSaveContactNote: vi.fn(),
}))
vi.mock('@/hooks/use-contact-tasks', () => ({ useContactTasks: vi.fn() }))
vi.mock('@/hooks/use-keyboard-navigation', () => ({ useKeyboardNavigation: vi.fn() }))
vi.mock('@/components/layout/navigation', () => ({ Navigation: () => <div /> }))
vi.mock('@/components/contacts/meetings', () => ({ Meetings: () => <div /> }))
vi.mock('@/components/contacts/tasks-section', () => ({ TasksSection: () => <div /> }))
vi.mock('@/components/contacts/merge-contact-modal', () => ({ MergeContactModal: () => <div /> }))
vi.mock('@/components/contacts/log-interaction-modal', () => ({
  LogInteractionModal: () => <div />,
}))
vi.mock('next/navigation', () => ({
  useParams: () => ({ id: CONTACT_ID }),
  useRouter: () => ({ replace: vi.fn(), push: vi.fn() }),
  useSearchParams: () => new URLSearchParams(),
}))

import ContactDetailPage from '../page'
import {
  useContact,
  useContactIDs,
  usePrefetchContact,
  useUpdateContact,
  useApplyMethodOperations,
  useDeleteContact,
} from '@/hooks/use-contacts'
import { useContactNote, useSaveContactNote } from '@/hooks/use-contact-note'
import { useContactTasks } from '@/hooks/use-contact-tasks'
import { useKeyboardNavigation } from '@/hooks/use-keyboard-navigation'

const CONTACT_ID = '11111111-0000-4000-8000-000000000000'
const METHOD_A = 'aaaaaaaa-0000-4000-8000-000000000001'
const METHOD_B = 'bbbbbbbb-0000-4000-8000-000000000002'
const METHOD_C = 'cccccccc-0000-4000-8000-000000000003'

function contactWith(methods: Contact['methods']): Contact {
  return {
    id: CONTACT_ID,
    full_name: 'Test Person',
    methods,
    has_pending_followup: false,
    created_at: '2026-01-01T00:00:00Z',
    updated_at: '2026-01-01T00:00:00Z',
  } as Contact
}

const methodA = { id: METHOD_A, type: 'email' as const, value: 'a@example.test', is_primary: false }
const methodB = { id: METHOD_B, type: 'phone' as const, value: '5555550100', is_primary: false }

let updateContact: ReturnType<typeof vi.fn>
let applyMethods: ReturnType<typeof vi.fn>
let saveNote: ReturnType<typeof vi.fn>

function mutation(mutateAsync: ReturnType<typeof vi.fn>) {
  return { mutateAsync, isPending: false, error: null }
}

function setContact(contact: Contact) {
  ;(useContact as any).mockReturnValue({ data: contact, isLoading: false, error: null })
}

function renderPage() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  return render(
    <QueryClientProvider client={client}>
      <ContactDetailPage />
    </QueryClientProvider>
  )
}

/** Opens edit mode and returns the value input for the first method row. */
async function openEditMode(user: ReturnType<typeof userEvent.setup>) {
  await user.click(screen.getByRole('button', { name: /Edit/ }))
  await screen.findByRole('heading', { name: 'Edit Contact' })
}

function methodValueInputs() {
  return screen.getAllByRole('textbox', { name: 'Contact method value' })
}

async function save(user: ReturnType<typeof userEvent.setup>) {
  await user.click(screen.getByRole('button', { name: 'Update Contact' }))
}

/** The operations of the nth POST /methods call. */
function operationsOfCall(n: number) {
  return applyMethods.mock.calls[n][0].operations
}

beforeEach(() => {
  vi.clearAllMocks()
  updateContact = vi.fn().mockResolvedValue(contactWith([methodA]))
  applyMethods = vi.fn().mockResolvedValue({ methods: [methodA], results: [] })
  saveNote = vi.fn().mockResolvedValue({})

  setContact(contactWith([methodA]))
  ;(useContactIDs as any).mockReturnValue({ data: { ids: [CONTACT_ID] }, isLoading: false })
  ;(usePrefetchContact as any).mockReturnValue(vi.fn())
  ;(useUpdateContact as any).mockReturnValue(mutation(updateContact))
  ;(useApplyMethodOperations as any).mockReturnValue(mutation(applyMethods))
  ;(useDeleteContact as any).mockReturnValue(mutation(vi.fn()))
  ;(useContactNote as any).mockReturnValue({ data: { body: '' } })
  ;(useSaveContactNote as any).mockReturnValue(mutation(saveNote))
  ;(useContactTasks as any).mockReturnValue({ data: [], isLoading: false })
  ;(useKeyboardNavigation as any).mockReturnValue({
    canGoBack: false,
    canGoForward: false,
    goBack: vi.fn(),
    goForward: vi.fn(),
    currentIndex: 0,
    total: 1,
  })
})

describe('the save sequence', () => {
  it('skips the methods request when there are no operations', async () => {
    const user = userEvent.setup()
    renderPage()
    await openEditMode(user)

    await user.clear(screen.getByLabelText(/Full Name/))
    await user.type(screen.getByLabelText(/Full Name/), 'Renamed')
    await save(user)

    await waitFor(() => expect(saveNote).toHaveBeenCalled())
    expect(updateContact).toHaveBeenCalledTimes(1)
    expect(applyMethods).not.toHaveBeenCalled()
  })

  it('sends no methods key on the scalar PUT', async () => {
    const user = userEvent.setup()
    renderPage()
    await openEditMode(user)

    await user.type(methodValueInputs()[0], 'x')
    await save(user)

    await waitFor(() => expect(updateContact).toHaveBeenCalled())
    expect(updateContact.mock.calls[0][0].data).not.toHaveProperty('methods')
  })

  it('stops at the failing step and does not attempt the next one', async () => {
    applyMethods.mockRejectedValue(new Error('methods failed'))
    const user = userEvent.setup()
    renderPage()
    await openEditMode(user)

    await user.type(methodValueInputs()[0], 'x')
    await save(user)

    await waitFor(() => expect(applyMethods).toHaveBeenCalled())
    expect(saveNote).not.toHaveBeenCalled()
  })

  it('reports partial success distinctly from total failure', async () => {
    saveNote.mockRejectedValue(new Error('notes failed'))
    const user = userEvent.setup()
    renderPage()
    await openEditMode(user)

    await user.type(methodValueInputs()[0], 'x')
    await save(user)

    const report = await screen.findByTestId('save-report')
    expect(report).toHaveTextContent(/Saved contact details and contact methods/)
    expect(report).toHaveTextContent(/the note could not be saved/)
    // Still in edit mode, so the user can retry.
    expect(screen.getByRole('heading', { name: 'Edit Contact' })).toBeInTheDocument()
  })

  it('reports that nothing was saved when the first step fails', async () => {
    updateContact.mockRejectedValue(new Error('contact failed'))
    const user = userEvent.setup()
    renderPage()
    await openEditMode(user)

    await user.type(methodValueInputs()[0], 'x')
    await save(user)

    const report = await screen.findByTestId('save-report')
    expect(report).toHaveTextContent(/Nothing was saved/)
  })

  it('resumes an unedited retry from the first unsucceeded step', async () => {
    saveNote.mockRejectedValueOnce(new Error('notes failed'))
    const user = userEvent.setup()
    renderPage()
    await openEditMode(user)

    await user.type(methodValueInputs()[0], 'x')
    await save(user)
    await screen.findByTestId('save-report')

    saveNote.mockResolvedValue({})
    await save(user)

    await waitFor(() => expect(saveNote).toHaveBeenCalledTimes(2))
    // The landed steps stay landed rather than being re-issued.
    expect(updateContact).toHaveBeenCalledTimes(1)
    expect(applyMethods).toHaveBeenCalledTimes(1)
  })

  it('reports cumulative progress when a retry lands a further step', async () => {
    // Attempt one saves scalars and fails at methods; attempt two saves methods
    // and fails at notes. The report must then name BOTH earlier steps — a
    // scenario that fails at the same step twice never advances the state
    // machine and cannot observe this.
    applyMethods.mockRejectedValueOnce(new Error('methods failed'))
    saveNote.mockRejectedValue(new Error('notes failed'))
    const user = userEvent.setup()
    renderPage()
    await openEditMode(user)

    await user.type(methodValueInputs()[0], 'x')
    await save(user)

    let report = await screen.findByTestId('save-report')
    expect(report).toHaveTextContent(
      /Saved contact details, but contact methods could not be saved/
    )

    await save(user)
    report = await screen.findByTestId('save-report')
    await waitFor(() =>
      expect(report).toHaveTextContent(/Saved contact details and contact methods/)
    )
    expect(updateContact).toHaveBeenCalledTimes(1)
    expect(applyMethods).toHaveBeenCalledTimes(2)
  })

  it('starts a fresh attempt chain from step one after a form edit', async () => {
    saveNote.mockRejectedValueOnce(new Error('notes failed'))
    const user = userEvent.setup()
    renderPage()
    await openEditMode(user)

    await user.type(methodValueInputs()[0], 'x')
    await save(user)
    await screen.findByTestId('save-report')

    // Editing after a partial success must not be dropped by resume-from-notes.
    await user.type(screen.getByLabelText(/Full Name/), '!')
    saveNote.mockResolvedValue({})
    await save(user)

    await waitFor(() => expect(saveNote).toHaveBeenCalledTimes(2))
    expect(updateContact).toHaveBeenCalledTimes(2)
    expect(updateContact.mock.calls[1][0].data.full_name).toContain('!')
  })
})

describe('the acknowledged state is never re-derived from server data', () => {
  it('is unaffected when the contact prop changes mid-save', async () => {
    // The scalar PUT refreshes the detail cache, so an unseen method can appear
    // in `contact` while the form is still open. Nothing derived from it may
    // change.
    let releaseUpdate: (value: unknown) => void = () => {}
    updateContact.mockImplementation(
      () =>
        new Promise(resolve => {
          releaseUpdate = resolve
        })
    )

    const user = userEvent.setup()
    renderPage()
    await openEditMode(user)

    await user.type(methodValueInputs()[0], 'x')
    await save(user)
    await waitFor(() => expect(updateContact).toHaveBeenCalled())

    // Another writer's method lands in the cache mid-flight.
    setContact(contactWith([methodA, methodB]))
    releaseUpdate(contactWith([methodA, methodB]))

    await waitFor(() => expect(applyMethods).toHaveBeenCalled())
    const operations = operationsOfCall(0)
    expect(JSON.stringify(operations)).not.toContain(METHOD_B)
    expect(operations).toEqual([
      { op: 'update', method_id: METHOD_A, type: 'email', value: 'a@example.testx' },
    ])
  })

  it('does not re-derive after an edit that follows a partial failure', async () => {
    // The round-3 sequence, and the only one that drives it: an UNEDITED retry
    // resumes at notes and never re-derives operations at all, so it stays
    // green against a dynamic baseline. The edit is what forces a fresh
    // derivation.
    saveNote.mockRejectedValueOnce(new Error('notes failed'))
    applyMethods.mockResolvedValue({
      methods: [{ ...methodA, value: 'a@example.testx' }],
      results: [
        {
          index: 0,
          outcome: 'updated',
          method_id: METHOD_A,
          method: { ...methodA, value: 'a@example.testx' },
        },
      ],
    })

    const user = userEvent.setup()
    renderPage()
    await openEditMode(user)

    await user.type(methodValueInputs()[0], 'x')
    await save(user)
    await screen.findByTestId('save-report')

    // The successful scalar PUT refreshed the detail cache to A + B.
    setContact(contactWith([methodA, methodB]))

    // Any form edit invalidates the session — but must NOT rebuild the
    // acknowledged state from that refreshed cache.
    await user.type(screen.getByLabelText(/Full Name/), '!')
    saveNote.mockResolvedValue({})
    await save(user)

    await waitFor(() => expect(saveNote).toHaveBeenCalledTimes(2))
    const named = applyMethods.mock.calls.map(call => JSON.stringify(call[0].operations)).join('|')
    expect(named).not.toContain(METHOD_B)
  })

  it('never names a method the response carried but no operation addressed', async () => {
    // The OTHER channel by which unseen server state can reach the acknowledged
    // state, and the one that is the original incident wearing a new name:
    // assigning the response's `methods` array instead of applying `results`.
    // The cache-refresh test above cannot see this — its response carries no
    // unseen row, so a response-absorbing implementation passes it.
    saveNote.mockRejectedValueOnce(new Error('notes failed'))
    applyMethods.mockResolvedValueOnce({
      // B is present on the server and in this response, but NO operation
      // addressed it. It must never enter the client's picture.
      methods: [{ ...methodA, value: 'a@example.testx' }, methodB],
      results: [
        {
          index: 0,
          outcome: 'updated',
          method_id: METHOD_A,
          method: { ...methodA, value: 'a@example.testx' },
        },
      ],
    })

    const user = userEvent.setup()
    renderPage()
    await openEditMode(user)

    await user.type(methodValueInputs()[0], 'x')
    await save(user)
    await screen.findByTestId('save-report')

    applyMethods.mockResolvedValue({ methods: [methodA], results: [] })
    saveNote.mockResolvedValue({})
    await user.type(screen.getByLabelText(/Full Name/), '!')
    await save(user)

    await waitFor(() => expect(saveNote).toHaveBeenCalledTimes(2))
    const named = applyMethods.mock.calls.map(call => JSON.stringify(call[0].operations)).join('|')
    expect(named).not.toContain(METHOD_B)
  })

  it('lets a row added in a partially failed save be removed by the next save', async () => {
    // This pins the acknowledged-state ADVANCE, not the form write-back: the
    // deleted row is absent from the submitted set either way, so the removal
    // derives from the acknowledged state alone. The write-back's pin is the
    // edit case below, where identity is at stake.
    saveNote.mockRejectedValueOnce(new Error('notes failed'))
    applyMethods.mockResolvedValueOnce({
      methods: [methodA, { id: METHOD_C, type: 'phone', value: '5555550199', is_primary: false }],
      results: [
        {
          index: 0,
          outcome: 'created',
          method_id: METHOD_C,
          method: { id: METHOD_C, type: 'phone', value: '5555550199', is_primary: false },
        },
      ],
    })

    const user = userEvent.setup()
    renderPage()
    await openEditMode(user)

    await user.click(screen.getByRole('button', { name: 'Add method' }))
    const typeSelects = screen.getAllByRole('combobox', { name: 'Contact method type' })
    await user.selectOptions(typeSelects[1], 'phone')
    await user.type(methodValueInputs()[1], '5555550199')
    await save(user)
    await screen.findByTestId('save-report')

    expect(operationsOfCall(0)).toEqual([{ op: 'add', type: 'phone', value: '5555550199' }])

    // Delete the row that was just created, then save again.
    applyMethods.mockResolvedValue({ methods: [methodA], results: [] })
    saveNote.mockResolvedValue({})
    const removeButtons = screen.getAllByRole('button', { name: 'Remove' })
    await user.click(removeButtons[1])
    await save(user)

    await waitFor(() => expect(applyMethods).toHaveBeenCalledTimes(2))
    expect(operationsOfCall(1)).toEqual([{ op: 'remove', method_id: METHOD_C }])
  })

  it('lets a row added in a partially failed save be edited, keeping its method_id', async () => {
    saveNote.mockRejectedValueOnce(new Error('notes failed'))
    applyMethods.mockResolvedValueOnce({
      methods: [methodA, { id: METHOD_C, type: 'phone', value: '5555550199', is_primary: false }],
      results: [
        {
          index: 0,
          outcome: 'created',
          method_id: METHOD_C,
          method: { id: METHOD_C, type: 'phone', value: '5555550199', is_primary: false },
        },
      ],
    })

    const user = userEvent.setup()
    renderPage()
    await openEditMode(user)

    await user.click(screen.getByRole('button', { name: 'Add method' }))
    await user.selectOptions(
      screen.getAllByRole('combobox', { name: 'Contact method type' })[1],
      'phone'
    )
    await user.type(methodValueInputs()[1], '5555550199')
    await save(user)
    await screen.findByTestId('save-report')

    applyMethods.mockResolvedValue({ methods: [methodA], results: [] })
    saveNote.mockResolvedValue({})
    await user.type(methodValueInputs()[1], '9')
    await save(user)

    await waitFor(() => expect(applyMethods).toHaveBeenCalledTimes(2))
    // An update on the SAME id — not remove + add, which would mint a new id
    // and destroy the row's created_at, the forensic tell that made the
    // original incident diagnosable.
    expect(operationsOfCall(1)).toEqual([
      { op: 'update', method_id: METHOD_C, type: 'phone', value: '55555501999' },
    ])
  })

  it('applies a primary-only result snapshot to the live row, not just the acknowledged state', async () => {
    // set_primary and clear_primary results carry a FULL snapshot, and it is
    // the only place the client learns that row's server-side state. If the
    // acknowledged state applies it while the form does not, the two structures
    // drift: the form keeps showing the stale value, and the next derivation
    // emits an update nobody asked for — overwriting a value the form never
    // displayed, on a row the user never edited.
    //
    // Here another writer changed A's value after edit-start. The user only
    // toggles primary, so the sole operation is set_primary(A), and its
    // snapshot reports the server's new value.
    saveNote.mockRejectedValueOnce(new Error('notes failed'))
    const serverValue = 'moved@example.test'
    applyMethods.mockResolvedValueOnce({
      methods: [{ ...methodA, value: serverValue, is_primary: true }],
      results: [
        {
          index: 0,
          outcome: 'updated',
          method_id: METHOD_A,
          method: { ...methodA, value: serverValue, is_primary: true },
        },
      ],
    })

    const user = userEvent.setup()
    renderPage()
    await openEditMode(user)

    await user.click(screen.getByRole('button', { name: 'Set as primary' }))
    await save(user)
    await screen.findByTestId('save-report')

    expect(operationsOfCall(0)).toEqual([{ op: 'set_primary', method_id: METHOD_A }])
    // The live row learned the server's value.
    await waitFor(() => expect(methodValueInputs()[0]).toHaveValue(serverValue))

    // A form edit invalidates the session, forcing a fresh derivation.
    applyMethods.mockResolvedValue({ methods: [methodA], results: [] })
    saveNote.mockResolvedValue({})
    await user.type(screen.getByLabelText(/Full Name/), '!')
    await save(user)

    await waitFor(() => expect(saveNote).toHaveBeenCalledTimes(2))
    // No spurious update rewriting the server's value back to the stale one.
    const second = applyMethods.mock.calls[1]
    expect(second ? second[0].operations : []).toEqual([])
  })

  it('restores the original value when a change is reverted after a partial failure', async () => {
    saveNote.mockRejectedValueOnce(new Error('notes failed'))
    applyMethods.mockResolvedValueOnce({
      methods: [{ ...methodA, value: 'a@example.testx' }],
      results: [
        {
          index: 0,
          outcome: 'updated',
          method_id: METHOD_A,
          method: { ...methodA, value: 'a@example.testx' },
        },
      ],
    })

    const user = userEvent.setup()
    renderPage()
    await openEditMode(user)

    await user.type(methodValueInputs()[0], 'x')
    await save(user)
    await screen.findByTestId('save-report')

    // Revert. A frozen edit-start baseline emits nothing here and leaves the
    // intermediate value on the server while reporting success.
    saveNote.mockResolvedValue({})
    applyMethods.mockResolvedValue({ methods: [methodA], results: [] })
    await user.clear(methodValueInputs()[0])
    await user.type(methodValueInputs()[0], 'a@example.test')
    await save(user)

    await waitFor(() => expect(applyMethods).toHaveBeenCalledTimes(2))
    expect(operationsOfCall(1)).toEqual([
      { op: 'update', method_id: METHOD_A, type: 'email', value: 'a@example.test' },
    ])
  })
})
