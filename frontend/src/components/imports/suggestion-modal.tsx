'use client'

import { useState, useEffect, useMemo, useCallback, useRef } from 'react'
import { ChevronLeft, ChevronRight, UserPlus, Link2, Ban, HelpCircle, Check } from 'lucide-react'
import { clsx } from 'clsx'
import { Button } from '@/components/ui/button'
import { Select } from '@/components/ui/select'
import { ContactSelector } from '@/components/ui/contact-selector'
import { MethodSelector } from './method-selector'
import { ConflictResolver } from './conflict-resolver'
import { useContacts, useContact } from '@/hooks/use-contacts'
import {
  useImportAsContact,
  useLinkCandidate,
  useIgnoreCandidate,
  useImportCandidates,
} from '@/hooks/use-imports'
import { detectMethodConflicts, areNamesSimilar } from '@/lib/method-conflict-detection'
import { getCandidateDisplayName } from '@/lib/candidate-display'
import { getSourceDisplay } from '@/lib/source-display'
import { sourceAllowsImport } from '@/lib/candidate-actions'
import type {
  ImportCandidate,
  MethodSuggestion,
  SelectedMethod,
  MethodComparison,
} from '@/types/import'
import type { ContactMethodType } from '@/types/contact'
import { MethodSuggestionResolver } from './method-suggestion-resolver'

// Trusted domains for photo URLs
const TRUSTED_PHOTO_DOMAINS = ['googleusercontent.com', 'google.com', 'gstatic.com']

// Cadence options for the dropdown
const cadenceOptions = [
  { value: '', label: 'No cadence' },
  { value: 'weekly', label: 'Weekly' },
  { value: 'biweekly', label: 'Bi-weekly' },
  { value: 'monthly', label: 'Monthly' },
  { value: 'quarterly', label: 'Quarterly' },
  { value: 'biannual', label: 'Bi-annual' },
  { value: 'annual', label: 'Annual' },
]

function isPhotoUrlTrusted(url: string): boolean {
  try {
    const hostname = new URL(url).hostname
    return TRUSTED_PHOTO_DOMAINS.some(domain => hostname.endsWith(domain))
  } catch {
    return false
  }
}

/** The item the SuggestionModal shell is resolving: a contact candidate
 * (import/link/ignore, with candidate-array navigation) or a method
 * suggestion (enrich-locked confirm/dismiss). */
export type SuggestionModalItem =
  | {
      kind: 'contact'
      candidates: ImportCandidate[]
      initialIndex: number
      initialMode?: 'import' | 'link'
    }
  | { kind: 'method'; suggestion: MethodSuggestion }

interface SuggestionModalProps {
  item: SuggestionModalItem
  onClose: () => void
  onSuccess: (message: string) => void
  onError: (message: string) => void
}

/** Thin shell: dispatches to the contact-candidate body or the
 * method-suggestion body based on the item kind. */
export function SuggestionModal({ item, onClose, onSuccess, onError }: SuggestionModalProps) {
  if (item.kind === 'method') {
    return (
      <MethodSuggestionResolver
        suggestion={item.suggestion}
        onClose={onClose}
        onSuccess={onSuccess}
        onError={onError}
      />
    )
  }
  return (
    <ContactCandidateResolver
      candidates={item.candidates}
      initialIndex={item.initialIndex}
      initialMode={item.initialMode}
      onClose={onClose}
      onSuccess={onSuccess}
      onError={onError}
    />
  )
}

interface ContactCandidateResolverProps {
  /** List of candidates to process */
  candidates: ImportCandidate[]
  /** Initial index in the candidates array */
  initialIndex: number
  /** Initial mode - 'import' or 'link' */
  initialMode?: 'import' | 'link'
  /** Callback when modal is closed */
  onClose: () => void
  /** Callback when an action completes successfully */
  onSuccess: (message: string) => void
  /** Callback when an action fails */
  onError: (message: string) => void
}

type ModalMode = 'import' | 'link'

interface MethodSelection {
  value: string
  selected: boolean
  type: ContactMethodType
  isEmail: boolean
}

function ContactCandidateResolver({
  candidates: initialCandidates,
  initialIndex,
  initialMode = 'import',
  onClose,
  onSuccess,
  onError,
}: ContactCandidateResolverProps) {
  const [currentIndex, setCurrentIndex] = useState(initialIndex)
  const [mode, setMode] = useState<ModalMode>(initialMode)
  const [selectedContactId, setSelectedContactId] = useState<string | undefined>()
  const [methodSelections, setMethodSelections] = useState<Map<string, MethodSelection>>(new Map())
  const [conflictResolutions, setConflictResolutions] = useState<
    Map<string, 'use_crm' | 'use_external'>
  >(new Map())
  const [selectedCadence, setSelectedCadence] = useState<string>('')
  const [isTransitioning, setIsTransitioning] = useState(false)

  // Name editing state (GH-155)
  const [editedName, setEditedName] = useState<string>('')
  const [isEditingName, setIsEditingName] = useState(false)
  const nameInputRef = useRef<HTMLInputElement>(null)

  // Primary method state (GH-159)
  const [primaryMethodValue, setPrimaryMethodValue] = useState<string | null>(null)

  // Fetch all candidates for the modal (not limited by page pagination)
  // Note: We need to pass page: 1 explicitly to ensure consistent query params
  const { data: allCandidatesData, isSuccess } = useImportCandidates({ page: 1, limit: 1000 })
  // Use fetched data once query succeeds, otherwise fall back to initialCandidates
  const candidates =
    isSuccess && allCandidatesData?.candidates?.length
      ? allCandidatesData.candidates
      : initialCandidates

  const candidate = candidates[currentIndex]
  const displayName = candidate ? getCandidateDisplayName(candidate) : ''
  const sourceInfo = candidate ? getSourceDisplay(candidate.source) : null
  // Link-only sources (e.g. gmail_correspondence) cannot create a new
  // contact. Derived from the source via the shared policy mirror — NOT a
  // response field, since the 1000-candidate refetch above doesn't carry
  // allowed_actions. The server is the authority/enforcer.
  const linkOnly = candidate ? !sourceAllowsImport(candidate.source) : false

  // Force link mode for a link-only source (Import is absent).
  useEffect(() => {
    if (linkOnly && mode !== 'link') {
      setMode('link')
    }
  }, [linkOnly, mode])

  // Fetch contacts for link mode selector
  const { data: contactsData } = useContacts({ limit: 500 })

  // Fetch the selected CRM contact's full details (including methods)
  const { data: selectedContact } = useContact(selectedContactId || '')

  // Mutations
  const importMutation = useImportAsContact()
  const linkMutation = useLinkCandidate()
  const ignoreMutation = useIgnoreCandidate()

  const isLoading = importMutation.isPending || linkMutation.isPending || ignoreMutation.isPending

  // Helper to handle successful actions - avoids code duplication
  const handleActionSuccess = useCallback(
    (message: string) => {
      onSuccess(message)
      // Close modal if this was the last candidate
      if (candidates.length <= 1) {
        onClose()
      }
    },
    [candidates.length, onSuccess, onClose]
  )

  // Helper to handle action errors - avoids code duplication
  const handleActionError = useCallback(
    (error: unknown, fallbackMessage: string) => {
      onError(error instanceof Error ? error.message : fallbackMessage)
    },
    [onError]
  )

  // Clamp currentIndex when candidates array shrinks (after import/link/ignore)
  useEffect(() => {
    if (currentIndex >= candidates.length) {
      setCurrentIndex(Math.max(0, candidates.length - 1))
    }
  }, [candidates.length, currentIndex])

  // Initialize method selections when candidate changes
  useEffect(() => {
    // Guard against undefined candidate (can happen during cache invalidation race conditions)
    if (!candidate) return

    const selections = new Map<string, MethodSelection>()

    // Add emails with inferred types
    candidate.emails.forEach(email => {
      const inferredType: ContactMethodType = 'email'
      selections.set(email, {
        value: email,
        selected: true, // Pre-select all by default
        type: inferredType,
        isEmail: true,
      })
    })

    // Add phones
    candidate.phones.forEach(phone => {
      selections.set(phone, {
        value: phone,
        selected: true,
        type: 'phone',
        isEmail: false,
      })
    })

    // Add Telegram @username from metadata as a telegram method.
    // Mirrors the backend's buildMethodsAuto behavior in handlers/import.go so
    // the UI reflects the method that will actually be created on import.
    // Display the handle with a leading '@' for consistency with the card chip.
    if (candidate.source === 'telegram' && candidate.metadata?.username) {
      const rawHandle = candidate.metadata.username
      const displayValue = rawHandle.startsWith('@') ? rawHandle : `@${rawHandle}`
      selections.set(displayValue, {
        value: displayValue,
        selected: true,
        type: 'telegram',
        isEmail: false,
      })
    }

    setMethodSelections(selections)
    setConflictResolutions(new Map())
    setSelectedCadence('')

    // Auto-select suggested match if available
    if (candidate.suggested_match) {
      setSelectedContactId(candidate.suggested_match.contact_id)
    } else {
      setSelectedContactId(undefined)
    }
  }, [currentIndex, candidate])

  // Detect conflicts when in link mode and CRM contact is selected
  const methodComparisons = useMemo<MethodComparison[]>(() => {
    if (mode !== 'link' || !selectedContact) {
      return []
    }
    // If selectedContact exists but has no methods, pass an empty array
    return detectMethodConflicts(candidate, selectedContact.methods || [])
  }, [mode, selectedContact, candidate])

  // Initialize conflict resolutions when comparisons change
  useEffect(() => {
    if (mode === 'link' && methodComparisons.length > 0) {
      const resolutions = new Map<string, 'use_crm' | 'use_external'>()
      methodComparisons.forEach(comp => {
        if (comp.conflict_type === 'value_conflict') {
          // Pre-select CRM value for safety
          resolutions.set(comp.external_value, 'use_crm')
        }
      })
      setConflictResolutions(resolutions)
    }
  }, [mode, methodComparisons])

  // Pre-select the contact's existing cadence in link mode
  useEffect(() => {
    if (mode === 'link' && selectedContact) {
      setSelectedCadence(selectedContact.cadence || '')
    }
  }, [mode, selectedContact])

  // Initialize editedName when candidate/mode/contact changes (GH-155)
  useEffect(() => {
    if (mode === 'import') {
      // Import mode: use external name
      setEditedName(displayName)
    } else if (mode === 'link' && selectedContact) {
      // Link mode: use CRM contact name
      setEditedName(selectedContact.full_name)
    } else if (mode === 'link') {
      // Link mode but no contact selected yet: use external name as placeholder
      setEditedName(displayName)
    }
    setIsEditingName(false)
  }, [mode, selectedContact, displayName, currentIndex])

  // Initialize primary method from CRM contact in link mode (GH-159)
  useEffect(() => {
    if (mode === 'link' && selectedContact?.methods) {
      const primaryMethod = selectedContact.methods.find(m => m.is_primary)
      if (primaryMethod) {
        setPrimaryMethodValue(primaryMethod.value)
      } else {
        setPrimaryMethodValue(null)
      }
    } else if (mode === 'import') {
      // Reset primary in import mode
      setPrimaryMethodValue(null)
    }
  }, [mode, selectedContact, currentIndex])

  // Focus name input when entering edit mode
  useEffect(() => {
    if (isEditingName && nameInputRef.current) {
      nameInputRef.current.focus()
      nameInputRef.current.select()
    }
  }, [isEditingName])

  // Check for name mismatch in link mode
  const hasNameMismatch = useMemo(() => {
    if (mode !== 'link' || !selectedContact) return false
    return !areNamesSimilar(displayName, selectedContact.full_name)
  }, [mode, selectedContact, displayName])

  // Handle method toggle
  const handleMethodToggle = (value: string) => {
    setMethodSelections(prev => {
      const next = new Map(prev)
      const existing = next.get(value)
      if (existing) {
        next.set(value, { ...existing, selected: !existing.selected })
      }
      return next
    })
  }

  // Handle type change
  const handleTypeChange = (value: string, type: ContactMethodType) => {
    setMethodSelections(prev => {
      const next = new Map(prev)
      const existing = next.get(value)
      if (existing) {
        next.set(value, { ...existing, type })
      }
      return next
    })
  }

  // Handle conflict resolution
  const handleConflictResolve = (value: string, resolution: 'use_crm' | 'use_external') => {
    setConflictResolutions(prev => {
      const next = new Map(prev)
      next.set(value, resolution)
      return next
    })
  }

  // Handle primary method toggle (GH-159)
  const handlePrimaryToggle = (value: string) => {
    // Toggle: if already primary, clear it; otherwise set it as primary
    if (primaryMethodValue === value) {
      setPrimaryMethodValue(null)
    } else {
      setPrimaryMethodValue(value)
    }
  }

  // Handle name editing (GH-155)
  const handleStartEditingName = () => {
    setIsEditingName(true)
  }

  const handleConfirmNameEdit = () => {
    // Validate non-empty
    if (editedName.trim()) {
      setIsEditingName(false)
    }
  }

  const handleCancelNameEdit = () => {
    // Revert to original name
    if (mode === 'import') {
      setEditedName(displayName)
    } else if (selectedContact) {
      setEditedName(selectedContact.full_name)
    }
    setIsEditingName(false)
  }

  const handleQuickFillName = (name: string) => {
    setEditedName(name)
    // If in view mode, entering edit mode isn't needed since we're quick-filling
    // But if we're already editing, just update the value
  }

  const handleNameKeyDown = (e: React.KeyboardEvent<HTMLInputElement>) => {
    if (e.key === 'Enter') {
      e.preventDefault()
      handleConfirmNameEdit()
    } else if (e.key === 'Escape') {
      e.preventDefault()
      handleCancelNameEdit()
    }
  }

  // Build selected methods for API
  const buildSelectedMethods = (): SelectedMethod[] => {
    const methods: SelectedMethod[] = []
    methodSelections.forEach(sel => {
      if (sel.selected) {
        methods.push({
          original_value: sel.value,
          type: sel.type,
          is_primary: sel.value === primaryMethodValue,
        })
      }
    })
    return methods
  }

  // Handle Import action
  const handleImport = async () => {
    if (!candidate) return
    // Validate name
    if (!editedName.trim()) {
      onError('Contact name cannot be empty. Please enter a valid name.')
      return
    }
    const selectedMethods = buildSelectedMethods()
    const cadence = selectedCadence || undefined
    // Only include name if it differs from external source.
    // For candidates with no source name fields (e.g. Telegram peers where the
    // displayed heading is a metadata.username fallback), always send the name
    // — the backend cannot derive one from display_name/first_name/last_name.
    const candidateHasSourceName = Boolean(
      candidate.display_name || candidate.first_name || candidate.last_name
    )
    const nameToSend = candidateHasSourceName
      ? editedName.trim() !== displayName
        ? editedName.trim()
        : undefined
      : editedName.trim()

    try {
      await importMutation.mutateAsync({
        id: candidate.id,
        request:
          selectedMethods.length > 0 || cadence || nameToSend
            ? {
                selected_methods: selectedMethods.length > 0 ? selectedMethods : undefined,
                cadence,
                name: nameToSend,
              }
            : undefined,
      })
      handleActionSuccess(`${editedName.trim()} imported successfully!`)
    } catch (error) {
      handleActionError(error, 'Failed to import contact')
    }
  }

  // Handle Link action
  const handleLink = async () => {
    if (!selectedContactId || !candidate) return
    // Validate name
    if (!editedName.trim()) {
      onError('Contact name cannot be empty. Please enter a valid name.')
      return
    }

    const selectedMethods = buildSelectedMethods()
    const resolutions: Record<string, 'use_crm' | 'use_external'> = {}
    conflictResolutions.forEach((value, key) => {
      resolutions[key] = value
    })
    const cadence = selectedCadence || undefined
    // Only include name if it differs from CRM contact's current name
    const nameToSend =
      selectedContact && editedName.trim() !== selectedContact.full_name
        ? editedName.trim()
        : undefined

    // §4 residual: send methods_curated ONLY when the candidate actually
    // offered methods to curate (the modal rendered the method-selection
    // UI). A deselect-all then link sends methods_curated:true with an
    // empty selected_methods, so the backend classifies the link as
    // `imported` (not `matched`). A zero-method candidate offered no
    // curation choice → omit the flag → stays `matched`.
    const offeredMethodCuration = methodSelections.size > 0

    try {
      await linkMutation.mutateAsync({
        id: candidate.id,
        request: {
          crm_contact_id: selectedContactId,
          selected_methods: offeredMethodCuration ? selectedMethods : undefined,
          methods_curated: offeredMethodCuration || undefined,
          conflict_resolutions: Object.keys(resolutions).length > 0 ? resolutions : undefined,
          cadence,
          name: nameToSend,
        },
      })
      handleActionSuccess('Contact linked successfully!')
    } catch (error) {
      handleActionError(error, 'Failed to link contact')
    }
  }

  // Handle Ignore action
  const handleIgnore = async () => {
    if (!candidate) return
    try {
      await ignoreMutation.mutateAsync(candidate.id)
      handleActionSuccess(`${displayName} ignored`)
    } catch (error) {
      handleActionError(error, 'Failed to ignore contact')
    }
  }

  // Navigation with transitions
  const canGoBack = currentIndex > 0
  const canGoForward = currentIndex < candidates.length - 1

  const navigateTo = useCallback((newIndex: number) => {
    setIsTransitioning(true)
    setTimeout(() => {
      setCurrentIndex(newIndex)
      setIsTransitioning(false)
    }, 150)
  }, [])

  const goBack = useCallback(() => {
    if (canGoBack && !isLoading && !isTransitioning) navigateTo(currentIndex - 1)
  }, [canGoBack, isLoading, isTransitioning, currentIndex, navigateTo])

  const goForward = useCallback(() => {
    if (canGoForward && !isLoading && !isTransitioning) navigateTo(currentIndex + 1)
  }, [canGoForward, isLoading, isTransitioning, currentIndex, navigateTo])

  // Keyboard navigation
  useEffect(() => {
    const handleKeyDown = (e: KeyboardEvent) => {
      // Don't handle if typing in an input
      if (e.target instanceof HTMLInputElement || e.target instanceof HTMLTextAreaElement) {
        return
      }

      switch (e.key) {
        case 'Escape':
          onClose()
          break
        case 'ArrowLeft':
          e.preventDefault()
          goBack() // goBack already checks canGoBack, isLoading, isTransitioning
          break
        case 'ArrowRight':
          e.preventDefault()
          goForward() // goForward already checks canGoForward, isLoading, isTransitioning
          break
      }
    }

    window.addEventListener('keydown', handleKeyDown)
    return () => window.removeEventListener('keydown', handleKeyDown)
  }, [onClose, goBack, goForward])

  // Separate methods by conflict status for link mode
  const nonConflictMethods = methodComparisons.filter(
    c => c.conflict_type === 'none' || c.conflict_type === 'identical'
  )
  const conflictMethods = methodComparisons.filter(
    c => c.conflict_type === 'value_conflict' || c.conflict_type === 'type_conflict'
  )

  // Guard against no candidate (shouldn't happen but safety check)
  if (!candidate) {
    return null
  }

  const SourceIcon = sourceInfo?.icon || HelpCircle

  return (
    <div
      className="fixed inset-0 bg-black/30 backdrop-blur-sm overflow-y-auto h-full w-full z-50"
      onClick={e => {
        if (e.target === e.currentTarget) onClose()
      }}
    >
      <div className="relative top-10 mx-auto p-0 border w-full max-w-xl shadow-lg rounded-lg bg-white overflow-hidden">
        {/* Navigation header */}
        <div className="flex items-center justify-between px-4 py-3 bg-gray-50 border-b">
          <button
            onClick={goBack}
            disabled={!canGoBack || isLoading || isTransitioning}
            className={clsx(
              'p-1.5 rounded transition-colors',
              canGoBack && !isLoading && !isTransitioning
                ? 'text-gray-600 hover:bg-gray-200'
                : 'text-gray-300 cursor-not-allowed'
            )}
            aria-label="Previous candidate"
          >
            <ChevronLeft className="w-5 h-5" />
          </button>

          <span className="text-sm text-gray-600">
            {currentIndex + 1} of {candidates.length}
          </span>

          <button
            onClick={goForward}
            disabled={!canGoForward || isLoading || isTransitioning}
            className={clsx(
              'p-1.5 rounded transition-colors',
              canGoForward && !isLoading && !isTransitioning
                ? 'text-gray-600 hover:bg-gray-200'
                : 'text-gray-300 cursor-not-allowed'
            )}
            aria-label="Next candidate"
          >
            <ChevronRight className="w-5 h-5" />
          </button>
        </div>

        {/* Candidate info with transition */}
        <div
          className={clsx(
            'px-6 py-4 border-b transition-opacity duration-150',
            isTransitioning ? 'opacity-0' : 'opacity-100'
          )}
        >
          <div className="flex items-center gap-4">
            {candidate.photo_url && isPhotoUrlTrusted(candidate.photo_url) ? (
              <img
                src={candidate.photo_url}
                alt={editedName || displayName}
                className="w-12 h-12 rounded-full object-cover flex-shrink-0"
              />
            ) : (
              <div className="w-12 h-12 rounded-full bg-gray-200 flex items-center justify-center flex-shrink-0">
                <span className="text-lg font-medium text-gray-600">
                  {(editedName || displayName).charAt(0).toUpperCase()}
                </span>
              </div>
            )}
            <div className="flex-1 min-w-0">
              {/* Row 1: Name + Source */}
              <div className="flex items-center justify-between gap-2 h-8">
                {isEditingName ? (
                  /* Edit mode: input + checkmark */
                  <div className="flex items-center gap-2 flex-1 min-w-0">
                    <input
                      ref={nameInputRef}
                      type="text"
                      value={editedName}
                      onChange={e => setEditedName(e.target.value)}
                      onKeyDown={handleNameKeyDown}
                      onBlur={handleConfirmNameEdit}
                      className="text-base font-medium text-gray-900 border border-blue-500 rounded px-2 py-0.5 flex-1 min-w-0 focus:outline-none focus:ring-2 focus:ring-blue-500"
                      disabled={isLoading}
                    />
                    <button
                      type="button"
                      onClick={handleConfirmNameEdit}
                      className="p-1 text-green-600 hover:bg-green-50 rounded flex-shrink-0"
                      disabled={isLoading}
                    >
                      <Check className="w-4 h-4" />
                    </button>
                  </div>
                ) : (
                  /* View mode: clickable name with pencil on hover */
                  <div className="group">
                    <h3
                      className="text-lg font-medium text-gray-900 cursor-pointer hover:bg-blue-50 hover:text-blue-700 px-2 py-1 -mx-2 -my-1 rounded transition-colors inline-flex items-center gap-2 truncate"
                      onClick={handleStartEditingName}
                    >
                      <span className="truncate">{editedName || displayName}</span>
                      <svg
                        className="w-4 h-4 text-gray-400 opacity-0 group-hover:opacity-100 transition-opacity flex-shrink-0"
                        fill="none"
                        stroke="currentColor"
                        viewBox="0 0 24 24"
                      >
                        <path
                          strokeLinecap="round"
                          strokeLinejoin="round"
                          strokeWidth={2}
                          d="M15.232 5.232l3.536 3.536m-2.036-5.036a2.5 2.5 0 113.536 3.536L6.5 21.036H3v-3.572L16.732 3.732z"
                        />
                      </svg>
                    </h3>
                  </div>
                )}
                {/* Source attribution - always visible, right-aligned */}
                {!isEditingName && (
                  <span className="text-xs text-gray-500 flex items-center gap-1 flex-shrink-0">
                    <SourceIcon className="w-3.5 h-3.5" />
                    {sourceInfo?.label || candidate.source}
                  </span>
                )}
              </div>

              {/* Row 2: Context line (only in edit mode or when name mismatch) */}
              {isEditingName ? (
                /* Edit mode: source + original name as quick-fill */
                <p className="text-sm text-gray-500 mt-1 h-5 flex items-center gap-1">
                  <SourceIcon className="w-3 h-3" />
                  {sourceInfo?.label?.replace(' Contacts', '') || candidate.source}:
                  <button
                    type="button"
                    className="text-blue-600 hover:underline"
                    onClick={() => handleQuickFillName(displayName)}
                  >
                    &quot;{displayName}&quot;
                  </button>
                </p>
              ) : hasNameMismatch && selectedContact && !isEditingName ? (
                /* Link mode with mismatch: show external name hint */
                <p className="text-sm text-amber-600 mt-1 h-5">
                  External: &quot;{displayName}&quot; —
                  <button
                    type="button"
                    className="text-blue-600 hover:underline ml-1"
                    onClick={() => handleQuickFillName(displayName)}
                  >
                    use this
                  </button>
                </p>
              ) : null}
            </div>
          </div>
        </div>

        {/* Mode toggle — hidden for link-only sources (Import is absent;
            the modal is locked to link mode). */}
        {!linkOnly && (
          <div className="px-6 py-3 border-b">
            <div className="flex rounded-lg border border-gray-200 p-1 bg-gray-50">
              <button
                onClick={() => setMode('import')}
                disabled={isLoading}
                className={clsx(
                  'flex-1 px-4 py-2 text-sm font-medium rounded-md transition-colors flex items-center justify-center gap-2',
                  mode === 'import'
                    ? 'bg-white text-gray-900 shadow-sm'
                    : 'text-gray-600 hover:text-gray-900'
                )}
              >
                <UserPlus className="w-4 h-4" />
                Import as New
              </button>
              <button
                onClick={() => setMode('link')}
                disabled={isLoading}
                className={clsx(
                  'flex-1 px-4 py-2 text-sm font-medium rounded-md transition-colors flex items-center justify-center gap-2',
                  mode === 'link'
                    ? 'bg-white text-gray-900 shadow-sm'
                    : 'text-gray-600 hover:text-gray-900'
                )}
              >
                <Link2 className="w-4 h-4" />
                Link to Existing
              </button>
            </div>
          </div>
        )}

        {/* Link mode: Contact selector */}
        {mode === 'link' && (
          <div className="px-6 py-4 border-b bg-gray-50">
            <label className="block text-sm font-medium text-gray-700 mb-2">Link to</label>
            <ContactSelector
              contacts={contactsData?.contacts || []}
              value={selectedContactId}
              onChange={setSelectedContactId}
              placeholder="Search for a contact..."
              disabled={isLoading}
              showNoContactOption={false}
            />
          </div>
        )}

        {/* Contact methods section with transition */}
        <div
          className={clsx(
            'px-6 py-4 max-h-[40vh] overflow-y-auto transition-opacity duration-150',
            isTransitioning ? 'opacity-0' : 'opacity-100'
          )}
        >
          <h4 className="text-sm font-medium text-gray-700 mb-3">Contact Methods</h4>

          {methodSelections.size === 0 ? (
            <p className="text-sm text-gray-500">No contact methods available</p>
          ) : mode === 'import' ? (
            // Import mode: Simple method selection
            <div className="space-y-2">
              {Array.from(methodSelections.values()).map(sel => (
                <MethodSelector
                  key={sel.value}
                  value={sel.value}
                  selected={sel.selected}
                  selectedType={sel.type}
                  state="adding"
                  onToggle={() => handleMethodToggle(sel.value)}
                  onTypeChange={type => handleTypeChange(sel.value, type)}
                  disabled={isLoading}
                  isEmail={sel.isEmail}
                  isPrimary={sel.value === primaryMethodValue}
                  onPrimaryToggle={() => handlePrimaryToggle(sel.value)}
                />
              ))}
            </div>
          ) : (
            // Link mode: Show conflicts and non-conflicts separately
            <div className="space-y-4">
              {/* Non-conflicting methods */}
              {nonConflictMethods.length > 0 && (
                <div>
                  <h5 className="text-xs font-medium text-gray-500 mb-2 uppercase tracking-wide">
                    {nonConflictMethods.some(m => m.conflict_type === 'none')
                      ? 'Will be added'
                      : 'Already in CRM'}
                  </h5>
                  <div className="space-y-2">
                    {nonConflictMethods.map(comp => {
                      const sel = methodSelections.get(comp.external_value)
                      if (!sel) return null
                      return (
                        <MethodSelector
                          key={comp.external_value}
                          value={comp.external_value}
                          selected={sel.selected}
                          selectedType={sel.type}
                          state={comp.state}
                          onToggle={() => handleMethodToggle(comp.external_value)}
                          onTypeChange={type => handleTypeChange(comp.external_value, type)}
                          disabled={isLoading || comp.conflict_type === 'identical'}
                          isEmail={sel.isEmail}
                          isPrimary={comp.external_value === primaryMethodValue}
                          onPrimaryToggle={() => handlePrimaryToggle(comp.external_value)}
                        />
                      )
                    })}
                  </div>
                </div>
              )}

              {/* Conflicting methods */}
              {conflictMethods.length > 0 && (
                <div>
                  <h5 className="text-xs font-medium text-red-600 mb-2 uppercase tracking-wide">
                    Conflicts to resolve
                  </h5>
                  <div className="space-y-3">
                    {conflictMethods.map(comp => (
                      <ConflictResolver
                        key={comp.external_value}
                        comparison={comp}
                        resolution={conflictResolutions.get(comp.external_value) || 'use_crm'}
                        onResolve={res => handleConflictResolve(comp.external_value, res)}
                        disabled={isLoading}
                      />
                    ))}
                  </div>
                </div>
              )}

              {selectedContactId && !selectedContact && (
                <p className="text-sm text-gray-500">Loading contact methods...</p>
              )}

              {selectedContactId && selectedContact && methodComparisons.length === 0 && (
                <p className="text-sm text-gray-500">
                  All contact methods will be added as new (no conflicts)
                </p>
              )}

              {!selectedContactId && (
                <p className="text-sm text-gray-500">Select a contact to see method comparison</p>
              )}
            </div>
          )}
        </div>

        {/* Cadence selector */}
        <div className="px-6 py-4 border-t">
          <Select
            label="Contact Cadence"
            helpText="How often you want to be reminded to reach out"
            value={selectedCadence}
            onChange={e => setSelectedCadence(e.target.value)}
            disabled={isLoading}
          >
            {cadenceOptions.map(option => (
              <option key={option.value} value={option.value}>
                {option.label}
              </option>
            ))}
          </Select>
        </div>

        {/* Footer actions */}
        <div className="px-6 py-4 bg-gray-50 border-t flex items-center justify-between">
          <Button
            variant="ghost"
            onClick={handleIgnore}
            loading={ignoreMutation.isPending}
            disabled={isLoading}
            className="text-gray-500"
          >
            <Ban className="w-4 h-4 mr-1" />
            {ignoreMutation.isPending ? 'Ignoring...' : 'Ignore'}
          </Button>

          <div className="flex gap-2">
            <Button variant="outline" onClick={onClose} disabled={isLoading}>
              Cancel
            </Button>
            {mode === 'import' && !linkOnly ? (
              <Button
                onClick={handleImport}
                loading={importMutation.isPending}
                disabled={isLoading}
              >
                <UserPlus className="w-4 h-4 mr-1" />
                {importMutation.isPending ? 'Importing...' : 'Import as New Contact'}
              </Button>
            ) : (
              <Button
                onClick={handleLink}
                loading={linkMutation.isPending}
                disabled={isLoading || !selectedContactId}
              >
                <Link2 className="w-4 h-4 mr-1" />
                {linkMutation.isPending ? 'Linking...' : 'Link Contact'}
              </Button>
            )}
          </div>
        </div>
      </div>
    </div>
  )
}
