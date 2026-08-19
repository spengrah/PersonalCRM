'use client'

import { useState, useRef, useEffect, useCallback, useMemo } from 'react'
import { useParams, useRouter, useSearchParams } from 'next/navigation'
import { Navigation } from '@/components/layout/navigation'
import { ContactForm, type MethodsReconciler } from '@/components/contacts/contact-form'
import { Button } from '@/components/ui/button'
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
import { ContactNavigationBar } from '@/components/contacts/contact-navigation-bar'
import { formatBirthday, formatRelativeTime } from '@/lib/utils'
import {
  Edit,
  Trash2,
  MessageCircle,
  MapPin,
  Calendar,
  ChevronDown,
  GitMerge,
  X,
} from 'lucide-react'
import { ContactMethodIcon } from '@/components/contacts/contact-method-icon'
import { Meetings } from '@/components/contacts/meetings'
import {
  formatContactMethodValue,
  getContactMethodHref,
  getContactMethodLabel,
  sortContactMethods,
} from '@/lib/contact-methods'
import type { ContactSubmitData } from '@/lib/validations/contact'
import type { UpdateContactRequest } from '@/types/contact'
import {
  advanceAcknowledgedMethods,
  deriveMethodOperationsWithOrigins,
  initializeAcknowledgedMethods,
  reconciliationsFromResults,
  type AcknowledgedMethod,
} from '@/lib/contact-method-operations'
import type { ContactMethodOperation } from '@/types/generated/contact'
import {
  buildContactDetailUrl,
  buildContactListUrl,
  pageFromIndex,
  parseListContext,
} from '@/lib/contact-list-params'
import { MergeContactModal } from '@/components/contacts/merge-contact-modal'
import { TasksSection } from '@/components/contacts/tasks-section'
import { LogInteractionModal } from '@/components/contacts/log-interaction-modal'
import { isAnyDetailModalOpen, isContactMainRendered } from './modal-gate'

type SaveStep = 'contact' | 'methods' | 'notes'

const SAVE_STEP_LABELS: Record<SaveStep, string> = {
  contact: 'contact details',
  methods: 'contact methods',
  notes: 'the note',
}

/**
 * One attempt chain over an immutable submitted payload.
 *
 * A retry resumes from the first step that has not yet succeeded, so a step
 * that landed stays landed and the report can never say "nothing was saved"
 * when something was. Resume applies only to an UNEDITED retry: any form edit
 * discards the session, and the next save is a fresh chain from step one with a
 * fresh payload — otherwise resuming would silently drop the new edit.
 *
 * The acknowledged method state is deliberately NOT part of this. Rebuilding it
 * on session invalidation is the original defect: a successful scalar PUT
 * refreshes the detail cache, so a rebuilt picture would include a method the
 * form never showed, and the next payload would name it for removal.
 */
interface SaveSession {
  payload: ContactSubmitData
  operations: ContactMethodOperation[]
  submittedIndexes: number[]
  completed: SaveStep[]
}

interface SaveReport {
  saved: SaveStep[]
  failed: SaveStep
}

function describeSaveReport({ saved, failed }: SaveReport): string {
  const failedLabel = SAVE_STEP_LABELS[failed]
  if (saved.length === 0) {
    return `Nothing was saved — ${failedLabel} could not be saved. Try again.`
  }
  const savedLabels = saved.map(step => SAVE_STEP_LABELS[step])
  const savedText =
    savedLabels.length === 1
      ? savedLabels[0]
      : `${savedLabels.slice(0, -1).join(', ')} and ${savedLabels[savedLabels.length - 1]}`
  return `Saved ${savedText}, but ${failedLabel} could not be saved. Try again to save the rest.`
}

export default function ContactDetailPage() {
  const params = useParams()
  const router = useRouter()
  const searchParams = useSearchParams()
  const contactId = params.id as string

  const action = searchParams.get('action')
  const [isEditing, setIsEditing] = useState(action === 'edit')
  const [notesExpanded, setNotesExpanded] = useState(false)
  const [notesOverflowing, setNotesOverflowing] = useState(false)
  const [isMergeModalOpen, setIsMergeModalOpen] = useState(action === 'merge')
  const [isLogModalOpen, setIsLogModalOpen] = useState(false)
  const [isAddTaskModalOpen, setIsAddTaskModalOpen] = useState(false)

  // Before this was lifted here, AddTaskModal's open state was local to
  // TasksSection, so React destroyed it whenever edit mode unmounted that
  // subtree — closing the modal for free. Lifting it to page-level state
  // removed that: without this reset, a modal left open when edit mode is
  // entered (e.g. via the same unmounted-modal focus leak isMainRendered
  // guards against) would reopen unprompted when edit mode ends.
  useEffect(() => {
    if (isEditing) {
      setIsAddTaskModalOpen(false)
    }
  }, [isEditing])

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
  // A callback ref, not useRef: the notes element mounts LATER than the note
  // data arrives whenever the note query resolves before the contact query,
  // and a plain ref gives the overflow effect below no dependency that changes
  // when it finally appears. See that effect.
  const [notesEl, setNotesEl] = useState<HTMLDivElement | null>(null)

  // Extract list context from URL params (or use defaults)
  const listContext = useMemo(() => parseListContext(searchParams), [searchParams])

  const { data: contact, isLoading, error } = useContact(contactId)
  const { data: contactNote } = useContactNote(contactId)
  const { data: navigationData, isLoading: isLoadingIDs } = useContactIDs(listContext)
  const updateContactMutation = useUpdateContact()
  const applyMethodOperationsMutation = useApplyMethodOperations()
  const saveContactNoteMutation = useSaveContactNote()

  // The client's picture of the server's methods, and the only left-hand side
  // any operation is derived against.
  const acknowledgedMethodsRef = useRef<AcknowledgedMethod[] | null>(null)
  const saveSessionRef = useRef<SaveSession | null>(null)
  const methodsReconcilerRef = useRef<MethodsReconciler | null>(null)
  const [saveReport, setSaveReport] = useState<SaveReport | null>(null)

  useEffect(() => {
    if (!isEditing) {
      acknowledgedMethodsRef.current = null
      saveSessionRef.current = null
      // Functional form so an already-null report is a true bail-out. A plain
      // setSaveReport(null) can still cost a render pass before React compares,
      // and this effect runs on mount and on every contact change on the
      // read-only detail view — where a stray render perturbs the notes
      // overflow measurement (a ResizeObserver reading scrollHeight).
      setSaveReport(current => (current === null ? current : null))
      return
    }
    if (!contact) return
    // Captured ONCE, at edit-start, and never re-read from server data
    // afterwards — not on session invalidation, not on retry, not when the
    // detail cache refreshes underneath the open form. This guard is what makes
    // that true; deleting it re-derives the picture from whatever the cache
    // holds now, which is how a method the form never showed gets named for
    // removal.
    if (acknowledgedMethodsRef.current) return
    acknowledgedMethodsRef.current = initializeAcknowledgedMethods(contact.methods)
  }, [isEditing, contact])

  // A user edit invalidates the payload and the step progress. It does NOT
  // touch the acknowledged state, which carries forward including whatever this
  // client's confirmed operations have already taught it.
  const handleFormEdit = useCallback(() => {
    saveSessionRef.current = null
    setSaveReport(null)
  }, [])

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

  // Build a detail URL for an adjacent contact, preserving list context params.
  const buildNavigationUrl = useCallback(
    (newId: string) => buildContactDetailUrl(listContext, newId),
    [listContext]
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

  // Return to the list at the user's place: the same context prev/next carries,
  // plus the page holding the contact currently in view (computed from its
  // global index). Shared by the "Back to list" button and the Escape key so
  // mouse and keyboard restore identically. Falls back to page 1 when the id
  // list has not resolved yet (currentIndex < 0 → pageFromIndex undefined).
  const buildBackToListUrl = useCallback(
    () => buildContactListUrl(listContext, pageFromIndex(currentIndex)),
    [listContext, currentIndex]
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

  // Handle Enter key (toggle edit mode) and Escape key (discard/return to list).
  // Gated on whether a modal is actually MOUNTED, not just requested: every
  // modal on this page (merge, log-interaction, add-task) only renders once
  // the main body below renders, so the gate must match the SAME predicate
  // the early returns above use — not just `Boolean(contact)`. TanStack Query
  // keeps the previous `contact` around while a background refetch is
  // erroring, so `contact` can be truthy even while `error` is set and the
  // page has already taken its not-found return with nothing mounted,
  // swallowing Escape there too. isMainRendered() is exported and unit-tested
  // directly rather than exercised via a live refetch race in E2E.
  const isMainRendered = isContactMainRendered({ isLoading, error, contact, isEditing })
  const isModalOpen = isAnyDetailModalOpen({
    isMainRendered,
    isMergeModalOpen,
    isLogModalOpen,
    isAddTaskModalOpen,
  })
  useEffect(() => {
    function handleKeyDown(event: KeyboardEvent) {
      // An open modal owns the keyboard: it registers its own window-level
      // Escape listener, so acting here too would dismiss the modal AND
      // navigate/toggle edit on the same keypress.
      if (isModalOpen) {
        return
      }

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
          // Return to contacts list (preserving context + page)
          router.push(buildBackToListUrl())
        }
      }
    }

    window.addEventListener('keydown', handleKeyDown)
    return () => window.removeEventListener('keydown', handleKeyDown)
  }, [isEditing, isModalOpen, router, buildBackToListUrl])

  // Detect if notes content overflows the 4-line clamp. ResizeObserver
  // re-measures whenever the container's box changes — the initial
  // mount measurement can read scrollHeight=0 before fonts/styles
  // settle, leaving notesOverflowing false until the deps change.
  //
  // The observed ELEMENT is a dependency, which is load-bearing rather than
  // tidy. The note and the contact are two independent queries, and the Notes
  // row is inside the block this component renders only once the CONTACT has
  // loaded. When the note resolves first, an effect keyed on the note body alone
  // runs while there is no element to measure, bails out, and never re-runs —
  // because the body it is keyed on does not change again. The expand control
  // then stays hidden over content that plainly overflows. Keying on the node
  // makes its mount the trigger.
  useEffect(() => {
    if (!notesEl || notesExpanded) return

    const measure = () => {
      setNotesOverflowing(notesEl.scrollHeight > notesEl.clientHeight)
    }
    measure()

    const observer = new ResizeObserver(measure)
    observer.observe(notesEl)
    return () => observer.disconnect()
  }, [notesEl, contactNote?.body, notesExpanded])
  const deleteContactMutation = useDeleteContact()

  // Three sequential requests, never Promise.all: on failure at step n, stop
  // and do not attempt n+1. Every step tolerates replay — operations fold to a
  // no-op, the notes PUT is a full-document replace, and the scalar PUT
  // converges — so resuming an unedited retry is safe. That is safe replay with
  // defined side effects, not strict idempotency: a replayed scalar PUT still
  // advances timestamps and re-runs cadence work.
  const handleUpdateContact = async (data: ContactSubmitData) => {
    const acknowledged = acknowledgedMethodsRef.current ?? []

    let session = saveSessionRef.current
    if (!session) {
      const derived = deriveMethodOperationsWithOrigins(acknowledged, data.methods)
      session = {
        payload: data,
        operations: derived.operations,
        submittedIndexes: derived.submittedIndexes,
        completed: [],
      }
      saveSessionRef.current = session
    }

    // Built field by field rather than by spreading the payload, so the absence
    // of `methods` is visible at the call site: PUT /contacts/:id no longer
    // accepts them, and sending a desired set is the defect this arc removes.
    const notes = session.payload.notes
    const contactData: UpdateContactRequest = {
      full_name: session.payload.full_name,
      location: session.payload.location,
      birthday: session.payload.birthday,
      cadence: session.payload.cadence,
    }

    let step: SaveStep = 'contact'
    try {
      if (!session.completed.includes('contact')) {
        await updateContactMutation.mutateAsync({ id: contactId, data: contactData })
        session.completed = [...session.completed, 'contact']
      }

      step = 'methods'
      if (!session.completed.includes('methods')) {
        // An empty derivation skips the request entirely.
        if (session.operations.length > 0) {
          const response = await applyMethodOperationsMutation.mutateAsync({
            id: contactId,
            operations: session.operations,
          })
          // Advance by applying THIS CLIENT'S OWN confirmed operations. The
          // response's `methods` array is never read here: taking it wholesale
          // would let rows no operation addressed enter the picture, which is
          // the original incident with extra steps.
          acknowledgedMethodsRef.current = advanceAcknowledgedMethods(
            acknowledged,
            session.operations,
            response.results
          )
          // The live form rows need the same ids, or a row created here would
          // still be id-less and the next save would derive remove + add,
          // silently changing the row's identity.
          methodsReconcilerRef.current?.(
            reconciliationsFromResults(
              session.operations,
              response.results,
              session.submittedIndexes
            )
          )
        }
        session.completed = [...session.completed, 'methods']
      }

      step = 'notes'
      if (!session.completed.includes('notes')) {
        await saveContactNoteMutation.mutateAsync({ contactId, body: notes || '' })
        session.completed = [...session.completed, 'notes']
      }

      saveSessionRef.current = null
      setSaveReport(null)
      setIsEditing(false)
    } catch {
      setSaveReport({ saved: session.completed, failed: step })
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

  const handleOpenLogModal = () => {
    setIsLogModalOpen(true)
  }

  if (isLoading) {
    return (
      <div className="min-h-screen bg-gray-50">
        <Navigation />
        <div className="max-w-4xl mx-auto py-6 px-4 sm:px-6 lg:px-8">
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
        <div className="max-w-4xl mx-auto py-6 px-4 sm:px-6 lg:px-8">
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
          onBack={() => router.push(buildBackToListUrl())}
          onPrevious={goBack}
          onNext={goForward}
          canGoBack={canGoBack}
          canGoForward={canGoForward}
          currentIndex={currentIndex}
          totalCount={total}
          isEditMode={true}
          isLoading={isLoadingIDs}
        />

        <div className="max-w-3xl mx-auto py-6 px-4 sm:px-6 lg:px-8">
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
                loading={
                  updateContactMutation.isPending ||
                  applyMethodOperationsMutation.isPending ||
                  saveContactNoteMutation.isPending
                }
                submitText="Update Contact"
                reconcilerRef={methodsReconcilerRef}
                onFormEdit={handleFormEdit}
              />
            </div>
          </div>

          {/* A partial save is a different situation from a total failure, and
              the user's next action differs — so say which parts landed rather
              than reporting a generic failure. */}
          {saveReport && (
            <div
              className="mt-4 bg-amber-50 border border-amber-200 rounded-md p-4"
              data-testid="save-report"
            >
              <h3 className="text-sm font-medium text-amber-800">
                {saveReport.saved.length > 0 ? 'Partially saved' : 'Not saved'}
              </h3>
              <p className="mt-1 text-sm text-amber-700">{describeSaveReport(saveReport)}</p>
            </div>
          )}

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
        onBack={() => router.push(buildBackToListUrl())}
        onPrevious={goBack}
        onNext={goForward}
        canGoBack={canGoBack}
        canGoForward={canGoForward}
        currentIndex={currentIndex}
        totalCount={total}
        isEditMode={isEditing}
        isLoading={isLoadingIDs}
      />

      <div className="max-w-4xl mx-auto py-6 px-4 sm:px-6 lg:px-8">
        {/* Header */}
        <div className="md:flex md:items-center md:justify-between mb-6">
          <div className="flex-1 min-w-0">
            <h2 className="text-2xl font-bold leading-normal text-gray-900 sm:text-3xl sm:truncate">
              {contact.full_name}
            </h2>
          </div>
          <div className="mt-4 flex flex-wrap gap-3 md:mt-0 md:ml-4">
            <Button variant="outline" size="sm" onClick={handleOpenLogModal}>
              <MessageCircle className="w-4 h-4 mr-2" />
              Log Interaction
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
                      {formatBirthday(contact.birthday)}
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
                <dt className="text-sm font-medium text-gray-500">Recent activity</dt>
                <dd className="mt-1 text-sm text-gray-900 sm:mt-0 sm:col-span-2 space-y-1">
                  {contact.last_outreach_at ? (
                    <div className="flex items-center gap-1.5">
                      <span className="text-gray-400" title="Last outreach">
                        &#8599;
                      </span>
                      <span>Last outreach: {formatRelativeTime(contact.last_outreach_at)}</span>
                    </div>
                  ) : null}
                  {contact.last_response_at ? (
                    <div className="flex items-center gap-1.5">
                      <span className="text-gray-400" title="Last response">
                        &#8601;
                      </span>
                      <span>Last response: {formatRelativeTime(contact.last_response_at)}</span>
                      {contact.has_pending_followup && (
                        <span className="ml-2 text-amber-600" title="Awaiting reply">
                          &#9888; Awaiting reply
                        </span>
                      )}
                    </div>
                  ) : contact.has_pending_followup ? (
                    <div className="flex items-center gap-1.5 text-amber-600">
                      <span title="Awaiting reply">&#9888;</span>
                      <span>Awaiting reply</span>
                    </div>
                  ) : null}
                  {!contact.last_outreach_at &&
                    !contact.last_response_at &&
                    !contact.has_pending_followup && (
                      <span className="text-gray-500">No recent activity</span>
                    )}
                </dd>
              </div>

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
                      ref={setNotesEl}
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
            isAddModalOpen={isAddTaskModalOpen}
            onAddModalOpenChange={setIsAddTaskModalOpen}
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

      {/* Log Interaction Modal */}
      {isLogModalOpen && contact && (
        <LogInteractionModal
          contactId={contactId}
          contactName={contact.full_name}
          onClose={() => setIsLogModalOpen(false)}
        />
      )}
    </div>
  )
}
