'use client'

import { useState, useRef, useEffect, useCallback } from 'react'
import { useParams, useRouter, useSearchParams } from 'next/navigation'
import { Navigation } from '@/components/layout/navigation'
import { ContactForm } from '@/components/contacts/contact-form'
import { Button } from '@/components/ui/button'
import {
  useContact,
  useContactIDs,
  usePrefetchContact,
  useUpdateContact,
  useDeleteContact,
  useUpdateLastContacted,
} from '@/hooks/use-contacts'
import { useContactNote, useSaveContactNote } from '@/hooks/use-contact-note'
import { useContactTasks } from '@/hooks/use-contact-tasks'
import { useKeyboardNavigation } from '@/hooks/use-keyboard-navigation'
import { ContactNavigationBar } from '@/components/contacts/contact-navigation-bar'
import { formatDateOnly, formatRelativeTime } from '@/lib/utils'
import {
  Edit,
  Trash2,
  MessageCircle,
  MapPin,
  Calendar,
  Clock,
  ChevronDown,
  Pencil,
  Check,
  X,
  GitMerge,
} from 'lucide-react'
import { ContactMethodIcon } from '@/components/contacts/contact-method-icon'
import { Meetings } from '@/components/contacts/meetings'
import {
  formatContactMethodValue,
  getContactMethodHref,
  getContactMethodLabel,
  sortContactMethods,
} from '@/lib/contact-methods'
import type { ContactFormData } from '@/lib/validations/contact'
import { MergeContactModal } from '@/components/contacts/merge-contact-modal'
import { TasksSection } from '@/components/contacts/tasks-section'

export default function ContactDetailPage() {
  const params = useParams()
  const router = useRouter()
  const searchParams = useSearchParams()
  const contactId = params.id as string

  const action = searchParams.get('action')
  const [isEditing, setIsEditing] = useState(action === 'edit')
  const [notesExpanded, setNotesExpanded] = useState(false)
  const [notesOverflowing, setNotesOverflowing] = useState(false)
  const [isEditingLastContacted, setIsEditingLastContacted] = useState(false)
  const [lastContactedDate, setLastContactedDate] = useState('')
  const [isMergeModalOpen, setIsMergeModalOpen] = useState(action === 'merge')

  // Clear the action param from URL after consuming it (prevents re-triggering on refresh)
  useEffect(() => {
    if (action) {
      const params = new URLSearchParams(searchParams.toString())
      params.delete('action')
      const queryString = params.toString()
      router.replace(`/contacts/${contactId}${queryString ? `?${queryString}` : ''}`)
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []) // Run only on mount

  const [mergeMessage, setMergeMessage] = useState<{
    type: 'success' | 'error'
    text: string
  } | null>(null)
  const notesRef = useRef<HTMLDivElement>(null)

  // Extract list context from URL params (or use defaults)
  const listContext = {
    sort: searchParams.get('sort') || 'cadence',
    order: (searchParams.get('order') as 'asc' | 'desc') || 'desc',
    search: searchParams.get('search') || undefined,
    cadence_filter:
      (searchParams.get('cadence_filter') as 'has_cadence' | 'no_cadence') || undefined,
    followup_filter:
      (searchParams.get('followup_filter') as 'has_followup' | 'no_followup') || undefined,
  }

  const { data: contact, isLoading, error } = useContact(contactId)
  const { data: contactNote } = useContactNote(contactId)
  const { data: navigationData, isLoading: isLoadingIDs } = useContactIDs(listContext)
  const updateContactMutation = useUpdateContact()
  const saveContactNoteMutation = useSaveContactNote()

  // Fetch tasks at page level to start loading in parallel with contact.
  // Post-046, the lifecycle filter is the natural axis: lifecycle=manual
  // covers all user-pickable tasks AND legacy action rows (which are
  // backfilled to (action, manual)); lifecycle=followup_loop covers
  // follow-up tasks created by FollowUpManager.
  const { data: activeManualTasks = [], isLoading: loadingActiveTasks } = useContactTasks(
    contactId,
    {
      state: 'managed',
      lifecycle: 'manual',
    }
  )
  const { data: completedManualTasks = [], isLoading: loadingCompletedTasks } = useContactTasks(
    contactId,
    { state: 'completed', lifecycle: 'manual' }
  )
  const { data: followUpTasks = [] } = useContactTasks(contactId, {
    state: 'managed',
    lifecycle: 'followup_loop',
  })

  // Merge active manual tasks and follow-up tasks for display.
  const activeTasks = [...followUpTasks, ...activeManualTasks]
  const completedTasks = completedManualTasks

  // Build URL preserving list context params
  const buildNavigationUrl = useCallback(
    (newId?: string) => {
      const params = new URLSearchParams()
      if (listContext.sort) params.set('sort', listContext.sort)
      if (listContext.order) params.set('order', listContext.order)
      if (listContext.search) params.set('search', listContext.search)
      if (listContext.cadence_filter) params.set('cadence_filter', listContext.cadence_filter)
      if (listContext.followup_filter) params.set('followup_filter', listContext.followup_filter)
      const queryString = params.toString()
      const path = newId ? `/contacts/${newId}` : '/contacts'
      return `${path}${queryString ? `?${queryString}` : ''}`
    },
    [
      listContext.sort,
      listContext.order,
      listContext.search,
      listContext.cadence_filter,
      listContext.followup_filter,
    ]
  )

  // Keyboard navigation
  const navigationIds = navigationData?.ids || []
  const { canGoBack, canGoForward, goBack, goForward, currentIndex, total } = useKeyboardNavigation(
    {
      ids: navigationIds,
      currentId: contactId,
      onNavigate: id => router.push(buildNavigationUrl(id)),
      enabled: !isEditing && !isLoading && !isLoadingIDs,
    }
  )

  // Prefetch adjacent contacts for smooth navigation
  const prefetchContact = usePrefetchContact()
  useEffect(() => {
    if (navigationIds.length === 0 || currentIndex < 0) return

    // Prefetch previous contact
    if (currentIndex > 0) {
      prefetchContact(navigationIds[currentIndex - 1])
    }
    // Prefetch next contact
    if (currentIndex < navigationIds.length - 1) {
      prefetchContact(navigationIds[currentIndex + 1])
    }
  }, [navigationIds, currentIndex, prefetchContact])

  // Handle Enter key (toggle edit mode) and Escape key (discard/return to list)
  useEffect(() => {
    function handleKeyDown(event: KeyboardEvent) {
      // Don't handle if typing in an input
      if (event.target instanceof HTMLInputElement || event.target instanceof HTMLTextAreaElement) {
        return
      }

      if (event.key === 'Enter' && !isEditing) {
        // Don't intercept Enter on buttons/links - let native activation work
        const target = event.target as HTMLElement
        if (target.tagName === 'BUTTON' || target.tagName === 'A' || target.closest('button, a')) {
          return
        }
        event.preventDefault()
        setIsEditing(true)
      }

      if (event.key === 'Escape') {
        event.preventDefault()
        if (isEditing) {
          // Discard changes and exit edit mode
          setIsEditing(false)
        } else {
          // Return to contacts list (preserving context)
          router.push(buildNavigationUrl())
        }
      }
    }

    window.addEventListener('keydown', handleKeyDown)
    return () => window.removeEventListener('keydown', handleKeyDown)
  }, [isEditing, router, buildNavigationUrl])

  // Detect if notes content overflows the 4-line clamp
  useEffect(() => {
    if (notesRef.current && !notesExpanded) {
      const isOverflowing = notesRef.current.scrollHeight > notesRef.current.clientHeight
      setNotesOverflowing(isOverflowing)
    }
  }, [contactNote?.body, notesExpanded])
  const deleteContactMutation = useDeleteContact()
  const updateLastContactedMutation = useUpdateLastContacted()

  const handleUpdateContact = async (data: ContactFormData) => {
    try {
      // Extract notes from form data and save separately
      const { notes, ...contactData } = data
      await Promise.all([
        updateContactMutation.mutateAsync({ id: contactId, data: contactData }),
        saveContactNoteMutation.mutateAsync({ contactId, body: notes || '' }),
      ])
      setIsEditing(false)
    } catch {
      // Error handled by TanStack Query error state
    }
  }

  const handleDeleteContact = async () => {
    if (confirm('Are you sure you want to delete this contact? This action cannot be undone.')) {
      try {
        await deleteContactMutation.mutateAsync(contactId)
        router.push('/contacts')
      } catch {
        // Error handled by TanStack Query error state
      }
    }
  }

  const handleMarkAsContacted = async () => {
    try {
      await updateLastContactedMutation.mutateAsync({ id: contactId })
    } catch {
      // Error handled by TanStack Query error state
    }
  }

  const handleEditLastContacted = () => {
    // Initialize with current last contacted date or empty
    if (contact?.last_contacted) {
      const date = new Date(contact.last_contacted)
      setLastContactedDate(date.toISOString().split('T')[0])
    } else {
      setLastContactedDate('')
    }
    setIsEditingLastContacted(true)
  }

  const handleSaveLastContacted = async () => {
    if (!lastContactedDate) return

    // Validate date is not in the future
    const selectedDate = new Date(lastContactedDate)
    const today = new Date()
    today.setHours(23, 59, 59, 999) // End of today
    if (selectedDate > today) {
      alert('Last contacted date cannot be in the future')
      return
    }

    try {
      await updateLastContactedMutation.mutateAsync({ id: contactId, date: lastContactedDate })
      setIsEditingLastContacted(false)
    } catch {
      // Error handled by TanStack Query error state
    }
  }

  const handleCancelEditLastContacted = () => {
    setIsEditingLastContacted(false)
    setLastContactedDate('')
  }

  if (isLoading) {
    return (
      <div className="min-h-screen bg-gray-50">
        <Navigation />
        <div className="max-w-4xl mx-auto py-6 sm:px-6 lg:px-8">
          <div className="animate-pulse space-y-6">
            <div className="h-8 bg-gray-200 rounded w-1/3"></div>
            <div className="bg-white shadow sm:rounded-lg p-6 space-y-4">
              <div className="h-6 bg-gray-200 rounded w-1/2"></div>
              <div className="h-4 bg-gray-200 rounded w-3/4"></div>
              <div className="h-4 bg-gray-200 rounded w-1/2"></div>
            </div>
          </div>
        </div>
      </div>
    )
  }

  if (error || !contact) {
    return (
      <div className="min-h-screen bg-gray-50">
        <Navigation />
        <div className="max-w-4xl mx-auto py-6 sm:px-6 lg:px-8">
          <div className="bg-red-50 border border-red-200 rounded-md p-4">
            <h3 className="text-lg font-medium text-red-800">Contact not found</h3>
            <p className="mt-1 text-sm text-red-700">
              The contact you&apos;re looking for doesn&apos;t exist or you don&apos;t have
              permission to view it.
            </p>
            <div className="mt-4">
              <Button variant="outline" onClick={() => router.push('/contacts')}>
                Back to Contacts
              </Button>
            </div>
          </div>
        </div>
      </div>
    )
  }

  if (isEditing) {
    return (
      <div className="min-h-screen bg-gray-50">
        <Navigation />

        {/* Navigation Bar (disabled in edit mode) */}
        <ContactNavigationBar
          onPrevious={goBack}
          onNext={goForward}
          canGoBack={canGoBack}
          canGoForward={canGoForward}
          currentIndex={currentIndex}
          totalCount={total}
          isEditMode={true}
          isLoading={isLoadingIDs}
        />

        <div className="max-w-3xl mx-auto py-6 sm:px-6 lg:px-8">
          <div className="md:flex md:items-center md:justify-between mb-6">
            <div className="flex-1 min-w-0">
              <h2 className="text-2xl font-bold leading-normal text-gray-900 sm:text-3xl sm:truncate">
                Edit Contact
              </h2>
              <p className="mt-1 text-sm text-gray-500">
                Update {contact.full_name}&apos;s information
              </p>
            </div>
            <div className="mt-4 md:mt-0 md:ml-4">
              <Button variant="outline" size="sm" onClick={() => setIsEditing(false)}>
                Cancel
              </Button>
            </div>
          </div>

          <div className="bg-white shadow sm:rounded-lg">
            <div className="px-4 py-5 sm:p-6">
              <ContactForm
                contact={contact}
                initialNotes={contactNote?.body || ''}
                onSubmit={handleUpdateContact}
                loading={updateContactMutation.isPending || saveContactNoteMutation.isPending}
                submitText="Update Contact"
              />
            </div>
          </div>

          {updateContactMutation.error && (
            <div className="mt-4 bg-red-50 border border-red-200 rounded-md p-4">
              <h3 className="text-sm font-medium text-red-800">Error updating contact</h3>
              <p className="mt-1 text-sm text-red-700">
                {updateContactMutation.error instanceof Error
                  ? updateContactMutation.error.message
                  : 'An unexpected error occurred'}
              </p>
            </div>
          )}
        </div>
      </div>
    )
  }

  const sortedMethods = contact.methods?.length
    ? sortContactMethods(contact.methods)
    : contact.primary_method
      ? [contact.primary_method]
      : []

  return (
    <div className="min-h-screen bg-gray-50">
      <Navigation />

      {/* Navigation Bar */}
      <ContactNavigationBar
        onPrevious={goBack}
        onNext={goForward}
        canGoBack={canGoBack}
        canGoForward={canGoForward}
        currentIndex={currentIndex}
        totalCount={total}
        isEditMode={isEditing}
        isLoading={isLoadingIDs}
      />

      <div className="max-w-4xl mx-auto py-6 sm:px-6 lg:px-8">
        {/* Header */}
        <div className="md:flex md:items-center md:justify-between mb-6">
          <div className="flex-1 min-w-0">
            <h2 className="text-2xl font-bold leading-normal text-gray-900 sm:text-3xl sm:truncate">
              {contact.full_name}
            </h2>
          </div>
          <div className="mt-4 flex space-x-3 md:mt-0 md:ml-4">
            <Button
              variant="outline"
              size="sm"
              onClick={handleMarkAsContacted}
              loading={updateLastContactedMutation.isPending}
            >
              <MessageCircle className="w-4 h-4 mr-2" />
              Mark as Contacted
            </Button>
            <Button variant="outline" size="sm" onClick={() => setIsEditing(true)}>
              <Edit className="w-4 h-4 mr-2" />
              Edit
            </Button>
            <Button variant="outline" size="sm" onClick={() => setIsMergeModalOpen(true)}>
              <GitMerge className="w-4 h-4 mr-2" />
              Merge
            </Button>
            <Button
              variant="danger"
              size="sm"
              onClick={handleDeleteContact}
              loading={deleteContactMutation.isPending}
            >
              <Trash2 className="w-4 h-4 mr-2" />
              Delete
            </Button>
          </div>
        </div>

        {/* Contact Info */}
        <div className="bg-white shadow overflow-hidden sm:rounded-lg">
          <div className="px-4 py-5 sm:px-6">
            <h3 className="text-lg leading-6 font-medium text-gray-900">Contact Information</h3>
          </div>
          <div className="border-t border-gray-200">
            <dl className="divide-y divide-gray-200">
              <div className="py-4 sm:py-5 sm:grid sm:grid-cols-3 sm:gap-4 sm:px-6">
                <dt className="text-sm font-medium text-gray-500">Full name</dt>
                <dd className="mt-1 text-sm text-gray-900 sm:mt-0 sm:col-span-2">
                  {contact.full_name}
                </dd>
              </div>

              <div className="py-4 sm:py-5 sm:grid sm:grid-cols-3 sm:gap-4 sm:px-6">
                <dt className="text-sm font-medium text-gray-500">Contact methods</dt>
                <dd className="mt-1 text-sm text-gray-900 sm:mt-0 sm:col-span-2">
                  {sortedMethods.length === 0 ? (
                    <span className="text-sm text-gray-500">-</span>
                  ) : (
                    <div className="space-y-2">
                      {sortedMethods.map(method => {
                        const value = formatContactMethodValue(method.type, method.value)
                        const href = getContactMethodHref(method.type, method.value)
                        const label = getContactMethodLabel(method.type)
                        const key = method.id ?? `${method.type}-${method.value}`

                        return (
                          <div key={key} className="flex items-center text-sm text-gray-900">
                            <ContactMethodIcon
                              type={method.type}
                              className="w-4 h-4 mr-2 text-gray-400"
                            />
                            {href ? (
                              <a href={href} className="text-blue-600 hover:text-blue-500">
                                {value}
                              </a>
                            ) : (
                              <span>{value}</span>
                            )}
                            <span className="ml-2 text-xs text-gray-500">{label}</span>
                            {method.is_primary && (
                              <span className="ml-2 text-xs font-medium text-blue-700">
                                Primary
                              </span>
                            )}
                          </div>
                        )
                      })}
                    </div>
                  )}
                </dd>
              </div>

              {contact.location && (
                <div className="py-4 sm:py-5 sm:grid sm:grid-cols-3 sm:gap-4 sm:px-6">
                  <dt className="text-sm font-medium text-gray-500">Location</dt>
                  <dd className="mt-1 text-sm text-gray-900 sm:mt-0 sm:col-span-2">
                    <div className="flex items-center">
                      <MapPin className="w-4 h-4 mr-2 text-gray-400" />
                      {contact.location}
                    </div>
                  </dd>
                </div>
              )}

              {contact.birthday && (
                <div className="py-4 sm:py-5 sm:grid sm:grid-cols-3 sm:gap-4 sm:px-6">
                  <dt className="text-sm font-medium text-gray-500">Birthday</dt>
                  <dd className="mt-1 text-sm text-gray-900 sm:mt-0 sm:col-span-2">
                    <div className="flex items-center">
                      <Calendar className="w-4 h-4 mr-2 text-gray-400" />
                      {formatDateOnly(contact.birthday)}
                    </div>
                  </dd>
                </div>
              )}

              {contact.cadence && (
                <div className="py-4 sm:py-5 sm:grid sm:grid-cols-3 sm:gap-4 sm:px-6">
                  <dt className="text-sm font-medium text-gray-500">Contact cadence</dt>
                  <dd className="mt-1 text-sm text-gray-900 sm:mt-0 sm:col-span-2">
                    <div className="flex items-center" data-testid="contact-cadence">
                      <Calendar className="w-4 h-4 mr-2 text-gray-400" />
                      {contact.cadence}
                    </div>
                  </dd>
                </div>
              )}

              <div className="py-4 sm:py-5 sm:grid sm:grid-cols-3 sm:gap-4 sm:px-6">
                <dt className="text-sm font-medium text-gray-500">Last contacted</dt>
                <dd className="mt-1 text-sm text-gray-900 sm:mt-0 sm:col-span-2">
                  {isEditingLastContacted ? (
                    <div className="flex items-center gap-2">
                      <input
                        type="date"
                        value={lastContactedDate}
                        onChange={e => setLastContactedDate(e.target.value)}
                        max={new Date().toISOString().split('T')[0]}
                        className="block rounded-md border-gray-300 shadow-sm focus:border-blue-500 focus:ring-blue-500 sm:text-sm"
                        data-testid="last-contacted-date-input"
                      />
                      <button
                        onClick={handleSaveLastContacted}
                        disabled={!lastContactedDate || updateLastContactedMutation.isPending}
                        className="inline-flex items-center p-1.5 text-green-600 hover:text-green-700 hover:bg-green-50 rounded disabled:opacity-50 disabled:cursor-not-allowed"
                        title="Save"
                        data-testid="save-last-contacted-btn"
                      >
                        <Check className="w-4 h-4" />
                      </button>
                      <button
                        onClick={handleCancelEditLastContacted}
                        disabled={updateLastContactedMutation.isPending}
                        className="inline-flex items-center p-1.5 text-gray-500 hover:text-gray-700 hover:bg-gray-100 rounded disabled:opacity-50"
                        title="Cancel"
                        data-testid="cancel-last-contacted-btn"
                      >
                        <X className="w-4 h-4" />
                      </button>
                    </div>
                  ) : (
                    <div className="flex items-center group">
                      <Clock className="w-4 h-4 mr-2 text-gray-400" />
                      <span>
                        {contact.last_contacted
                          ? formatDateOnly(contact.last_contacted, {
                              year: 'numeric',
                              month: 'numeric',
                              day: 'numeric',
                            })
                          : 'Never'}
                      </span>
                      <button
                        onClick={handleEditLastContacted}
                        className="ml-2 p-1 text-gray-400 hover:text-gray-600 opacity-0 group-hover:opacity-100 transition-opacity"
                        title="Edit last contacted date"
                        data-testid="edit-last-contacted-btn"
                      >
                        <Pencil className="w-3.5 h-3.5" />
                      </button>
                    </div>
                  )}
                </dd>
              </div>

              {(contact.last_outreach_at ||
                contact.last_response_at ||
                contact.has_pending_followup) && (
                <div className="py-4 sm:py-5 sm:grid sm:grid-cols-3 sm:gap-4 sm:px-6">
                  <dt className="text-sm font-medium text-gray-500">Direction signals</dt>
                  <dd className="mt-1 text-sm text-gray-900 sm:mt-0 sm:col-span-2 space-y-1">
                    {contact.last_outreach_at && (
                      <div className="flex items-center gap-1.5">
                        <span className="text-gray-400" title="Last outreach">
                          &#8599;
                        </span>
                        <span>Last outreach: {formatRelativeTime(contact.last_outreach_at)}</span>
                      </div>
                    )}
                    {contact.last_response_at && (
                      <div className="flex items-center gap-1.5">
                        <span className="text-gray-400" title="Last response">
                          &#8601;
                        </span>
                        <span>Last response: {formatRelativeTime(contact.last_response_at)}</span>
                      </div>
                    )}
                    {contact.has_pending_followup && (
                      <div className="flex items-center gap-1.5 text-amber-600">
                        <span title="Awaiting reply">&#9888;</span>
                        <span>Awaiting reply</span>
                      </div>
                    )}
                  </dd>
                </div>
              )}

              <div className="py-4 sm:py-5 sm:grid sm:grid-cols-3 sm:gap-4 sm:px-6">
                <dt className="text-sm font-medium text-gray-500">Created</dt>
                <dd className="mt-1 text-sm text-gray-900 sm:mt-0 sm:col-span-2">
                  {new Date(contact.created_at).toLocaleDateString()}
                </dd>
              </div>

              {contactNote?.body && (
                <div className="py-4 sm:py-5 sm:grid sm:grid-cols-3 sm:gap-4 sm:px-6">
                  <dt className="text-sm font-medium text-gray-500">Notes</dt>
                  <dd className="mt-1 text-sm text-gray-900 sm:mt-0 sm:col-span-2">
                    <div
                      ref={notesRef}
                      className={`whitespace-pre-wrap ${!notesExpanded ? 'line-clamp-4' : ''}`}
                    >
                      {contactNote.body}
                    </div>
                    {(notesOverflowing || notesExpanded) && (
                      <button
                        onClick={() => setNotesExpanded(!notesExpanded)}
                        className="mt-2 inline-flex items-center text-sm text-blue-600 hover:text-blue-800 font-medium"
                      >
                        {notesExpanded ? 'Show less' : 'Show more'}
                        <ChevronDown
                          className={`ml-1 w-4 h-4 transition-transform duration-200 ${notesExpanded ? 'rotate-180' : ''}`}
                        />
                      </button>
                    )}
                  </dd>
                </div>
              )}
            </dl>
          </div>
        </div>

        {/* Tasks Section */}
        <div className="mt-8">
          <TasksSection
            contactId={contactId}
            contactName={contact.full_name}
            activeTasks={activeTasks}
            completedTasks={completedTasks}
            loadingActive={loadingActiveTasks}
            loadingCompleted={loadingCompletedTasks}
          />
        </div>

        {/* Meetings Section */}
        <div className="mt-8">
          <Meetings contactId={contactId} />
        </div>

        {/* Merge success/error message */}
        {mergeMessage && (
          <div
            className={`fixed bottom-4 right-4 px-4 py-3 rounded-lg shadow-lg ${
              mergeMessage.type === 'success'
                ? 'bg-green-50 text-green-800 border border-green-200'
                : 'bg-red-50 text-red-800 border border-red-200'
            }`}
          >
            <div className="flex items-center gap-2">
              <span>{mergeMessage.text}</span>
              <button
                onClick={() => setMergeMessage(null)}
                className="text-current opacity-60 hover:opacity-100"
              >
                <X className="w-4 h-4" />
              </button>
            </div>
          </div>
        )}
      </div>

      {/* Merge Contact Modal */}
      {isMergeModalOpen && contact && (
        <MergeContactModal
          targetContact={contact}
          onClose={() => setIsMergeModalOpen(false)}
          onSuccess={message => {
            setMergeMessage({ type: 'success', text: message })
            setTimeout(() => setMergeMessage(null), 5000)
          }}
          onError={message => {
            setMergeMessage({ type: 'error', text: message })
            setTimeout(() => setMergeMessage(null), 5000)
          }}
        />
      )}
    </div>
  )
}
