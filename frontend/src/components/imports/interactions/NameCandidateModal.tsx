'use client'

import { useEffect, useMemo, useState } from 'react'
import { ChevronLeft, ChevronRight, UserPlus, Link2, Ban, MessageCircle } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { Select } from '@/components/ui/select'
import { ContactSelector } from '@/components/ui/contact-selector'
import { useContacts } from '@/hooks/use-contacts'
import { useResolveNameCandidate } from '@/hooks/use-interactions-queue'
import type { NameCandidateGroup } from '@/types/import'

// Cadence options mirror the import-link modal's kept controls.
const cadenceOptions = [
  { value: '', label: 'No cadence' },
  { value: 'weekly', label: 'Weekly' },
  { value: 'biweekly', label: 'Bi-weekly' },
  { value: 'monthly', label: 'Monthly' },
  { value: 'quarterly', label: 'Quarterly' },
  { value: 'biannual', label: 'Bi-annual' },
  { value: 'annual', label: 'Annual' },
]

type NameCandidateMode = 'import' | 'link'

interface NameCandidateModalProps {
  /** The name-candidate queue the pager iterates. */
  groups: NameCandidateGroup[]
  /** Index in `groups` to open at. */
  initialIndex: number
  onClose: () => void
  onSuccess: (message: string) => void
  onError: (message: string) => void
}

/**
 * The name-candidate "Create contact…" modal — a name-only sibling of the
 * import/link modal. It keeps the same shell (header pager, editable
 * name, Import/Link toggle, ContactSelector, cadence Select, footer) but
 * has NO contact-methods apparatus (Anarlog only captured a name) and
 * resolves the WHOLE token group via the token-group endpoint rather than
 * a per-row import/link/ignore mutation.
 */
export function NameCandidateModal({
  groups,
  initialIndex,
  onClose,
  onSuccess,
  onError,
}: NameCandidateModalProps) {
  // The modal owns a local copy of the queue so resolved tokens can be
  // removed from the pager without waiting on a refetch.
  const [queue, setQueue] = useState<NameCandidateGroup[]>(groups)
  const [index, setIndex] = useState(initialIndex)
  const [mode, setMode] = useState<NameCandidateMode>('import')
  const [name, setName] = useState('')
  const [cadence, setCadence] = useState('')
  const [contactId, setContactId] = useState<string | undefined>()

  const resolveMutation = useResolveNameCandidate()
  const { data: contactsData } = useContacts({ limit: 500 })
  const contacts = useMemo(() => contactsData?.contacts ?? [], [contactsData])

  const group = queue[index]

  // Reset per-group form state when the visible group changes.
  useEffect(() => {
    if (!group) return
    setName(group.token_display)
    setMode('import')
    setCadence('')
    setContactId(undefined)
  }, [index, group])

  if (!group) return null

  const canBack = index > 0
  const canForward = index < queue.length - 1
  const go = (delta: number) => {
    const next = index + delta
    if (next >= 0 && next < queue.length) setIndex(next)
  }

  // Remove the just-resolved group from the queue + advance the pager.
  const advanceAfterResolve = () => {
    const next = queue.filter(g => g.normalized_token !== group.normalized_token)
    if (next.length === 0) {
      onClose()
      return
    }
    setQueue(next)
    setIndex(i => Math.min(i, next.length - 1))
  }

  const resolve = async (action: 'import' | 'link' | 'ignore') => {
    // Only import needs a name (it names the new contact). Link attaches
    // evidence to the chosen existing contact and must NOT rename it.
    if (action === 'import' && !name.trim()) {
      onError('Contact name cannot be empty.')
      return
    }
    if (action === 'link' && !contactId) {
      onError('Select a contact to link to.')
      return
    }
    try {
      await resolveMutation.mutateAsync({
        normalized_token: group.normalized_token,
        action,
        // Send the name only for import; sending it on link would overwrite
        // the linked contact's existing name with the token text.
        name: action === 'import' ? name.trim() || undefined : undefined,
        cadence: action === 'ignore' ? undefined : cadence || undefined,
        crm_contact_id: action === 'link' ? contactId : undefined,
      })
      const linkedName = contacts.find(c => c.id === contactId)?.full_name ?? group.token_display
      onSuccess(
        action === 'ignore'
          ? `${group.token_display} ignored`
          : action === 'import'
            ? `${name.trim()} created`
            : `${linkedName} linked`
      )
      advanceAfterResolve()
    } catch (error) {
      onError(error instanceof Error ? error.message : 'Failed to resolve token group')
    }
  }

  const busy = resolveMutation.isPending
  const titles = group.session_titles ?? []

  return (
    <div
      className="fixed inset-0 z-50 flex items-start justify-center overflow-auto bg-gray-900/35 p-12 backdrop-blur-sm"
      onClick={e => {
        if (e.target === e.currentTarget) onClose()
      }}
      role="dialog"
      aria-modal="true"
      aria-label="Create contact from name candidate"
    >
      <div className="w-full max-w-xl overflow-hidden rounded-lg border border-gray-200 bg-white shadow-lg">
        {/* Header pager */}
        <div className="flex items-center justify-between border-b border-gray-200 bg-gray-50 px-4 py-2.5">
          <button
            type="button"
            onClick={() => go(-1)}
            disabled={!canBack || busy}
            aria-label="Previous"
            className="rounded p-1 text-gray-500 hover:bg-gray-200 disabled:opacity-40 disabled:hover:bg-transparent"
          >
            <ChevronLeft className="h-5 w-5" />
          </button>
          <span className="text-[0.8125rem] text-gray-600">
            {index + 1} of {queue.length}
          </span>
          <button
            type="button"
            onClick={() => go(1)}
            disabled={!canForward || busy}
            aria-label="Next"
            className="rounded p-1 text-gray-500 hover:bg-gray-200 disabled:opacity-40 disabled:hover:bg-transparent"
          >
            <ChevronRight className="h-5 w-5" />
          </button>
        </div>

        {/* Identity + name */}
        <div className="border-b border-gray-200 px-6 py-4">
          <div className="flex items-center gap-3.5">
            <span className="flex h-12 w-12 flex-shrink-0 items-center justify-center rounded-full border border-dashed border-gray-300 bg-gray-50 text-lg font-semibold text-gray-400">
              {(name.charAt(0) || '?').toUpperCase()}
            </span>
            <div className="flex-1">
              <label
                htmlFor="name-candidate-name"
                className="block text-xs font-medium uppercase tracking-wide text-gray-500"
              >
                Name
              </label>
              <input
                id="name-candidate-name"
                type="text"
                value={name}
                onChange={e => setName(e.target.value)}
                className="mt-1 w-full rounded-md border border-gray-300 px-3 py-2 text-sm text-gray-900 placeholder-gray-400 focus:border-blue-500 focus:outline-none focus:ring-1 focus:ring-blue-500"
                placeholder="Contact name"
              />
            </div>
          </div>

          {/* Import / Link toggle */}
          <div className="mt-4 inline-flex rounded-md border border-gray-300 p-0.5">
            <button
              type="button"
              onClick={() => setMode('import')}
              className={`rounded px-3 py-1.5 text-sm font-medium ${
                mode === 'import' ? 'bg-blue-600 text-white' : 'text-gray-600 hover:bg-gray-100'
              }`}
            >
              Create new
            </button>
            <button
              type="button"
              onClick={() => setMode('link')}
              className={`rounded px-3 py-1.5 text-sm font-medium ${
                mode === 'link' ? 'bg-blue-600 text-white' : 'text-gray-600 hover:bg-gray-100'
              }`}
            >
              Link to existing
            </button>
          </div>
        </div>

        {/* Link target selector (link mode) */}
        {mode === 'link' && (
          <div className="border-b border-gray-200 px-6 py-4">
            <label className="mb-1 block text-xs font-medium uppercase tracking-wide text-gray-500">
              Link to contact
            </label>
            <ContactSelector
              contacts={contacts}
              value={contactId}
              onChange={setContactId}
              showNoContactOption={false}
              placeholder="Search contacts..."
            />
          </div>
        )}

        {/* No-methods note + evidence */}
        <div className="border-b border-gray-200 px-6 py-4">
          <div className="rounded-md border border-blue-200 bg-blue-50 px-3 py-2.5 text-sm text-blue-800">
            {mode === 'import'
              ? 'No contact methods — Anarlog only captured a name. You can add an email or phone after creating.'
              : 'Attaches this session evidence to the contact. There are no methods to merge.'}
          </div>
          {titles.length > 0 && (
            <div className="mt-3">
              <div className="text-xs font-medium uppercase tracking-wide text-gray-500">
                Evidence · session titles
              </div>
              <ul className="mt-1.5 flex flex-col gap-1">
                {titles.map((title, i) => (
                  <li key={i} className="flex items-center gap-1.5 text-[0.8125rem] text-gray-600">
                    <MessageCircle className="h-3 w-3 text-gray-400" />
                    {title}
                  </li>
                ))}
              </ul>
            </div>
          )}
        </div>

        {/* Cadence */}
        <div className="border-b border-gray-200 px-6 py-4">
          <Select
            label="Cadence"
            value={cadence}
            onChange={e => setCadence(e.target.value)}
            disabled={busy}
          >
            {cadenceOptions.map(opt => (
              <option key={opt.value} value={opt.value}>
                {opt.label}
              </option>
            ))}
          </Select>
        </div>

        {/* Footer */}
        <div className="flex items-center justify-between gap-2 px-6 py-4">
          <Button
            variant="ghost"
            size="sm"
            disabled={busy}
            onClick={() => resolve('ignore')}
            className="text-gray-500"
          >
            <Ban className="mr-1 h-4 w-4" />
            Not a person
          </Button>
          <div className="flex items-center gap-2">
            <Button variant="outline" size="sm" disabled={busy} onClick={onClose}>
              Cancel
            </Button>
            {mode === 'import' ? (
              <Button size="sm" loading={busy} onClick={() => resolve('import')}>
                <UserPlus className="mr-1 h-4 w-4" />
                Create contact
              </Button>
            ) : (
              <Button
                size="sm"
                loading={busy}
                disabled={!contactId}
                onClick={() => resolve('link')}
              >
                <Link2 className="mr-1 h-4 w-4" />
                Link contact
              </Button>
            )}
          </div>
        </div>
      </div>
    </div>
  )
}
