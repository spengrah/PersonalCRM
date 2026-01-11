'use client'

import { useState, useEffect, useMemo } from 'react'
import { ChevronLeft, ChevronRight, UserPlus, Link2, Ban } from 'lucide-react'
import { clsx } from 'clsx'
import { Button } from '@/components/ui/button'
import { ContactSelector } from '@/components/ui/contact-selector'
import { MethodSelector } from './method-selector'
import { ConflictResolver } from './conflict-resolver'
import { useContacts, useContact } from '@/hooks/use-contacts'
import { useImportAsContact, useLinkCandidate, useIgnoreCandidate } from '@/hooks/use-imports'
import { inferEmailType } from '@/lib/email-type-inference'
import {
  detectMethodConflicts,
  getCandidateDisplayName,
  areNamesSimilar,
} from '@/lib/method-conflict-detection'
import type { ImportCandidate, SelectedMethod, MethodComparison } from '@/types/import'
import type { ContactMethodType } from '@/types/contact'

// Trusted domains for photo URLs
const TRUSTED_PHOTO_DOMAINS = ['googleusercontent.com', 'google.com', 'gstatic.com']

function isPhotoUrlTrusted(url: string): boolean {
  try {
    const hostname = new URL(url).hostname
    return TRUSTED_PHOTO_DOMAINS.some(domain => hostname.endsWith(domain))
  } catch {
    return false
  }
}

interface ImportLinkModalProps {
  /** List of candidates to process */
  candidates: ImportCandidate[]
  /** Initial index in the candidates array */
  initialIndex: number
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

export function ImportLinkModal({
  candidates,
  initialIndex,
  onClose,
  onSuccess,
  onError,
}: ImportLinkModalProps) {
  const [currentIndex, setCurrentIndex] = useState(initialIndex)
  const [mode, setMode] = useState<ModalMode>('import')
  const [selectedContactId, setSelectedContactId] = useState<string | undefined>()
  const [methodSelections, setMethodSelections] = useState<Map<string, MethodSelection>>(new Map())
  const [conflictResolutions, setConflictResolutions] = useState<
    Map<string, 'use_crm' | 'use_external'>
  >(new Map())

  const candidate = candidates[currentIndex]
  const displayName = getCandidateDisplayName(candidate)

  // Fetch contacts for link mode selector
  const { data: contactsData } = useContacts({ limit: 500 })

  // Fetch the selected CRM contact's full details (including methods)
  const { data: selectedContact } = useContact(selectedContactId || '')

  // Mutations
  const importMutation = useImportAsContact()
  const linkMutation = useLinkCandidate()
  const ignoreMutation = useIgnoreCandidate()

  const isLoading = importMutation.isPending || linkMutation.isPending || ignoreMutation.isPending

  // Initialize method selections when candidate changes
  useEffect(() => {
    const selections = new Map<string, MethodSelection>()

    // Add emails with inferred types
    candidate.emails.forEach(email => {
      const inferredType = inferEmailType(email)
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

    setMethodSelections(selections)
    setConflictResolutions(new Map())

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

  // Check for name mismatch in link mode
  const hasNameMismatch = useMemo(() => {
    if (mode !== 'link' || !selectedContact) return false
    return !areNamesSimilar(displayName, selectedContact.full_name)
  }, [mode, selectedContact, displayName])

  // Get used types (for disabling in dropdowns)
  const usedTypes = useMemo(() => {
    const used = new Set<string>()
    methodSelections.forEach(sel => {
      if (sel.selected) {
        used.add(sel.type)
      }
    })
    return used
  }, [methodSelections])

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
        // Handle auto-swap for emails
        if (existing.isEmail) {
          // Find any other email with this type and swap
          next.forEach((sel, key) => {
            if (key !== value && sel.isEmail && sel.selected && sel.type === type) {
              const otherType: ContactMethodType = existing.type
              next.set(key, { ...sel, type: otherType })
            }
          })
        }
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

  // Build selected methods for API
  const buildSelectedMethods = (): SelectedMethod[] => {
    const methods: SelectedMethod[] = []
    methodSelections.forEach(sel => {
      if (sel.selected) {
        methods.push({
          original_value: sel.value,
          type: sel.type,
        })
      }
    })
    return methods
  }

  // Handle Import action
  const handleImport = async () => {
    const selectedMethods = buildSelectedMethods()

    try {
      await importMutation.mutateAsync({
        id: candidate.id,
        request: selectedMethods.length > 0 ? { selected_methods: selectedMethods } : undefined,
      })
      onSuccess(`${displayName} imported successfully!`)

      // Move to next candidate or close
      if (currentIndex < candidates.length - 1) {
        setCurrentIndex(currentIndex + 1)
      } else {
        onClose()
      }
    } catch (error) {
      onError(error instanceof Error ? error.message : 'Failed to import contact')
    }
  }

  // Handle Link action
  const handleLink = async () => {
    if (!selectedContactId) return

    const selectedMethods = buildSelectedMethods()
    const resolutions: Record<string, 'use_crm' | 'use_external'> = {}
    conflictResolutions.forEach((value, key) => {
      resolutions[key] = value
    })

    try {
      await linkMutation.mutateAsync({
        id: candidate.id,
        request: {
          crm_contact_id: selectedContactId,
          selected_methods: selectedMethods.length > 0 ? selectedMethods : undefined,
          conflict_resolutions: Object.keys(resolutions).length > 0 ? resolutions : undefined,
        },
      })
      onSuccess('Contact linked successfully!')

      // Move to next candidate or close
      if (currentIndex < candidates.length - 1) {
        setCurrentIndex(currentIndex + 1)
      } else {
        onClose()
      }
    } catch (error) {
      onError(error instanceof Error ? error.message : 'Failed to link contact')
    }
  }

  // Handle Ignore action
  const handleIgnore = async () => {
    try {
      await ignoreMutation.mutateAsync(candidate.id)
      onSuccess(`${displayName} ignored`)

      // Move to next candidate or close
      if (currentIndex < candidates.length - 1) {
        setCurrentIndex(currentIndex + 1)
      } else {
        onClose()
      }
    } catch (error) {
      onError(error instanceof Error ? error.message : 'Failed to ignore contact')
    }
  }

  // Navigation
  const canGoBack = currentIndex > 0
  const canGoForward = currentIndex < candidates.length - 1

  const goBack = () => {
    if (canGoBack) setCurrentIndex(currentIndex - 1)
  }

  const goForward = () => {
    if (canGoForward) setCurrentIndex(currentIndex + 1)
  }

  // Separate methods by conflict status for link mode
  const nonConflictMethods = methodComparisons.filter(
    c => c.conflict_type === 'none' || c.conflict_type === 'identical'
  )
  const conflictMethods = methodComparisons.filter(
    c => c.conflict_type === 'value_conflict' || c.conflict_type === 'type_conflict'
  )

  return (
    <div
      className="fixed inset-0 bg-gray-600 bg-opacity-50 overflow-y-auto h-full w-full z-50"
      onClick={e => {
        if (e.target === e.currentTarget) onClose()
      }}
    >
      <div className="relative top-10 mx-auto p-0 border w-full max-w-xl shadow-lg rounded-lg bg-white overflow-hidden">
        {/* Navigation header */}
        <div className="flex items-center justify-between px-4 py-3 bg-gray-50 border-b">
          <button
            onClick={goBack}
            disabled={!canGoBack || isLoading}
            className={clsx(
              'p-1.5 rounded transition-colors',
              canGoBack && !isLoading
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
            disabled={!canGoForward || isLoading}
            className={clsx(
              'p-1.5 rounded transition-colors',
              canGoForward && !isLoading
                ? 'text-gray-600 hover:bg-gray-200'
                : 'text-gray-300 cursor-not-allowed'
            )}
            aria-label="Next candidate"
          >
            <ChevronRight className="w-5 h-5" />
          </button>
        </div>

        {/* Candidate info */}
        <div className="px-6 py-4 border-b">
          <div className="flex items-center gap-4">
            {candidate.photo_url && isPhotoUrlTrusted(candidate.photo_url) ? (
              <img
                src={candidate.photo_url}
                alt={displayName}
                className="w-12 h-12 rounded-full object-cover"
              />
            ) : (
              <div className="w-12 h-12 rounded-full bg-gray-200 flex items-center justify-center">
                <span className="text-lg font-medium text-gray-600">
                  {displayName.charAt(0).toUpperCase()}
                </span>
              </div>
            )}
            <div>
              <h3 className="text-lg font-medium text-gray-900">{displayName}</h3>
              <p className="text-sm text-gray-500">{candidate.source}</p>
            </div>
          </div>
        </div>

        {/* Mode toggle */}
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
            {hasNameMismatch && selectedContact && (
              <div className="mt-2 p-2 rounded bg-amber-50 border border-amber-200 text-sm text-amber-800">
                Name mismatch: &quot;{displayName}&quot; vs &quot;{selectedContact.full_name}&quot;
              </div>
            )}
          </div>
        )}

        {/* Contact methods section */}
        <div className="px-6 py-4 max-h-[40vh] overflow-y-auto">
          <h4 className="text-sm font-medium text-gray-700 mb-3">Contact Methods</h4>

          {candidate.emails.length === 0 && candidate.phones.length === 0 ? (
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
                  usedTypes={usedTypes}
                  disabled={isLoading}
                  isEmail={sel.isEmail}
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
                          usedTypes={usedTypes}
                          disabled={isLoading || comp.conflict_type === 'identical'}
                          isEmail={sel.isEmail}
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
            Ignore
          </Button>

          <div className="flex gap-2">
            <Button variant="outline" onClick={onClose} disabled={isLoading}>
              Cancel
            </Button>
            {mode === 'import' ? (
              <Button
                onClick={handleImport}
                loading={importMutation.isPending}
                disabled={isLoading}
              >
                <UserPlus className="w-4 h-4 mr-1" />
                Import
              </Button>
            ) : (
              <Button
                onClick={handleLink}
                loading={linkMutation.isPending}
                disabled={isLoading || !selectedContactId}
              >
                <Link2 className="w-4 h-4 mr-1" />
                Link
              </Button>
            )}
          </div>
        </div>
      </div>
    </div>
  )
}
