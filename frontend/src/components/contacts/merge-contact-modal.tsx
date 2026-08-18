'use client'

import { useState, useEffect, useRef, useMemo } from 'react'
import { X, Check, GitMerge, Archive, Shield } from 'lucide-react'
import { clsx } from 'clsx'
import { Button } from '@/components/ui/button'
import { ContactSelector } from '@/components/ui/contact-selector'
import { useContacts } from '@/hooks/use-contacts'
import { useMergePreview, useMergeContacts, type MergeFieldSelections } from '@/hooks/use-merge'
import type { Contact } from '@/types/contact'
import { formatCadence as formatCadenceBase } from '@/lib/utils'

interface MergeContactModalProps {
  /** The target contact (will receive merged data) */
  targetContact: Contact
  /** Callback when modal is closed */
  onClose: () => void
  /** Callback when merge completes successfully */
  onSuccess: (message: string) => void
  /** Callback when merge fails */
  onError: (message: string) => void
}

export function MergeContactModal({
  targetContact,
  onClose,
  onSuccess,
  onError,
}: MergeContactModalProps) {
  const [sourceContactId, setSourceContactId] = useState<string | undefined>()
  const [editedName, setEditedName] = useState(targetContact.full_name)
  const [isEditingName, setIsEditingName] = useState(false)
  const [fieldSelections, setFieldSelections] = useState<MergeFieldSelections>({
    cadence: 'target',
    location: 'target',
    birthday: 'target',
  })
  const nameInputRef = useRef<HTMLInputElement>(null)

  // Fetch contacts for source selector (exclude target contact)
  const { data: contactsData } = useContacts({ limit: 500 })
  const availableContacts = useMemo(() => {
    return (contactsData?.contacts || []).filter(c => c.id !== targetContact.id)
  }, [contactsData?.contacts, targetContact.id])

  // Fetch merge preview when source is selected
  const {
    data: preview,
    isLoading: isLoadingPreview,
    error: previewError,
  } = useMergePreview(targetContact.id, sourceContactId || '')

  // Merge mutation
  const mergeMutation = useMergeContacts()
  const isLoading = mergeMutation.isPending

  // Focus name input when entering edit mode
  useEffect(() => {
    if (isEditingName && nameInputRef.current) {
      nameInputRef.current.focus()
      nameInputRef.current.select()
    }
  }, [isEditingName])

  // Keyboard shortcuts
  useEffect(() => {
    const handleKeyDown = (e: KeyboardEvent) => {
      if (e.target instanceof HTMLInputElement || e.target instanceof HTMLTextAreaElement) {
        return
      }
      if (e.key === 'Escape') {
        onClose()
      }
    }

    window.addEventListener('keydown', handleKeyDown)
    return () => window.removeEventListener('keydown', handleKeyDown)
  }, [onClose])

  // Check for field conflicts
  const conflicts = useMemo(() => {
    if (!preview) return { cadence: false, location: false, birthday: false }

    const source = preview.source_contact
    const target = preview.target_contact

    return {
      cadence: !!source.cadence && source.cadence !== target.cadence,
      location: !!source.location && source.location !== target.location,
      birthday: !!source.birthday && source.birthday !== target.birthday,
    }
  }, [preview])

  const hasAnyConflicts = conflicts.cadence || conflicts.location || conflicts.birthday

  // Handle name editing
  const handleStartEditingName = () => setIsEditingName(true)

  const handleConfirmNameEdit = () => {
    if (editedName.trim()) {
      setIsEditingName(false)
    }
  }

  const handleNameKeyDown = (e: React.KeyboardEvent<HTMLInputElement>) => {
    if (e.key === 'Enter') {
      e.preventDefault()
      handleConfirmNameEdit()
    } else if (e.key === 'Escape') {
      e.preventDefault()
      setEditedName(targetContact.full_name)
      setIsEditingName(false)
    }
  }

  const handleQuickFillName = (name: string) => {
    setEditedName(name)
  }

  // Handle field selection toggle
  const handleFieldToggle = (field: keyof MergeFieldSelections) => {
    setFieldSelections(prev => ({
      ...prev,
      [field]: prev[field] === 'target' ? 'source' : 'target',
    }))
  }

  // Handle merge
  const handleMerge = async () => {
    if (!sourceContactId) return

    try {
      await mergeMutation.mutateAsync({
        targetId: targetContact.id,
        request: {
          source_contact_id: sourceContactId,
          field_selections: fieldSelections,
          new_name: editedName !== targetContact.full_name ? editedName : undefined,
        },
      })
      onSuccess(
        `Contacts merged successfully! ${preview?.source_contact.full_name} has been archived.`
      )
      onClose()
    } catch (error) {
      onError(error instanceof Error ? error.message : 'Failed to merge contacts')
    }
  }

  // Format date for display
  const formatDate = (dateString: string | undefined | null) => {
    if (!dateString) return 'Not set'
    return new Date(dateString).toLocaleDateString('en-US', {
      month: 'short',
      day: 'numeric',
      year: 'numeric',
    })
  }

  // Format cadence for display (uses shared formatter, but shows 'Not set' for merge context)
  const formatCadence = (cadence: string | undefined | null) => {
    if (!cadence) return 'Not set'
    return formatCadenceBase(cadence)
  }

  return (
    <div
      className="fixed inset-0 bg-black/30 backdrop-blur-sm overflow-y-auto h-full w-full z-50"
      onClick={e => {
        if (e.target === e.currentTarget) onClose()
      }}
    >
      <div
        role="dialog"
        aria-modal="true"
        aria-label="Merge Contacts"
        className="relative top-10 mx-auto p-0 border w-full max-w-xl shadow-lg rounded-lg bg-white mb-10"
      >
        {/* Header */}
        <div className="flex items-center justify-between px-6 py-4 bg-gray-50 border-b rounded-t-lg">
          <div className="flex items-center gap-2">
            <GitMerge className="w-5 h-5 text-blue-600" />
            <h2 className="text-lg font-semibold text-gray-900">Merge Contacts</h2>
          </div>
          <button
            onClick={onClose}
            className="p-1.5 rounded text-gray-400 hover:text-gray-600 hover:bg-gray-200 transition-colors"
          >
            <X className="w-5 h-5" />
          </button>
        </div>

        {/* Target Contact (Keeping) */}
        <div className="px-6 py-4 border-b">
          <div className="flex items-center justify-between">
            <div className="flex items-center gap-3 flex-1 min-w-0">
              <div className="w-10 h-10 rounded-full bg-blue-100 flex items-center justify-center flex-shrink-0">
                <span className="text-sm font-medium text-blue-700">
                  {editedName.charAt(0).toUpperCase()}
                </span>
              </div>
              <div className="flex-1 min-w-0">
                {isEditingName ? (
                  <div className="flex items-center gap-2">
                    <input
                      ref={nameInputRef}
                      type="text"
                      aria-label="Merged contact name"
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
                  <div className="group">
                    <h3
                      className="text-base font-medium text-gray-900 cursor-pointer hover:bg-blue-50 hover:text-blue-700 px-2 py-1 -mx-2 -my-1 rounded transition-colors inline-flex items-center gap-2 truncate"
                      onClick={handleStartEditingName}
                    >
                      <span className="truncate">{editedName}</span>
                      <svg
                        className="w-4 h-4 text-gray-400 opacity-100 sm:opacity-0 sm:group-hover:opacity-100 transition-opacity flex-shrink-0"
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
                {preview && preview.source_contact.full_name !== editedName && !isEditingName && (
                  <p className="text-sm text-amber-600 mt-0.5">
                    Source: &quot;{preview.source_contact.full_name}&quot; —
                    <button
                      type="button"
                      className="text-blue-600 hover:underline ml-1"
                      onClick={() => handleQuickFillName(preview.source_contact.full_name)}
                    >
                      use this
                    </button>
                  </p>
                )}
              </div>
            </div>
            <span className="px-2.5 py-1 text-xs font-medium rounded-full bg-blue-100 text-blue-700 flex items-center gap-1">
              <Shield className="w-3 h-3" />
              Keeping
            </span>
          </div>
        </div>

        {/* Source Contact Selector (Archiving) */}
        <div className="px-6 py-4 border-b bg-gray-50">
          <div className="flex items-center justify-between mb-2">
            <label className="block text-sm font-medium text-gray-700">Merge from</label>
            <span className="px-2.5 py-1 text-xs font-medium rounded-full bg-amber-100 text-amber-700 flex items-center gap-1">
              <Archive className="w-3 h-3" />
              Archiving
            </span>
          </div>
          <ContactSelector
            contacts={availableContacts}
            value={sourceContactId}
            onChange={setSourceContactId}
            placeholder="Search for a contact to merge..."
            disabled={isLoading}
            showNoContactOption={false}
          />
        </div>

        {/* Loading state */}
        {sourceContactId && isLoadingPreview && (
          <div className="px-6 py-8 flex items-center justify-center">
            <div className="animate-spin rounded-full h-8 w-8 border-b-2 border-blue-600"></div>
          </div>
        )}

        {/* Error state */}
        {previewError && (
          <div className="px-6 py-4 bg-red-50 border-b">
            <p className="text-sm text-red-600">Failed to load merge preview. Please try again.</p>
          </div>
        )}

        {/* Merge Preview */}
        {preview && !isLoadingPreview && (
          <>
            {/* Field Conflicts */}
            {hasAnyConflicts && (
              <div className="px-6 py-4 border-b">
                <h4 className="text-sm font-medium text-gray-700 mb-3">Resolve Conflicts</h4>
                <div className="space-y-3">
                  {conflicts.cadence && (
                    <FieldToggle
                      label="Cadence"
                      targetValue={formatCadence(preview.target_contact.cadence)}
                      sourceValue={formatCadence(preview.source_contact.cadence)}
                      selected={fieldSelections.cadence || 'target'}
                      onToggle={() => handleFieldToggle('cadence')}
                    />
                  )}
                  {conflicts.location && (
                    <FieldToggle
                      label="Location"
                      targetValue={preview.target_contact.location || 'Not set'}
                      sourceValue={preview.source_contact.location || 'Not set'}
                      selected={fieldSelections.location || 'target'}
                      onToggle={() => handleFieldToggle('location')}
                    />
                  )}
                  {conflicts.birthday && (
                    <FieldToggle
                      label="Birthday"
                      targetValue={formatDate(preview.target_contact.birthday)}
                      sourceValue={formatDate(preview.source_contact.birthday)}
                      selected={fieldSelections.birthday || 'target'}
                      onToggle={() => handleFieldToggle('birthday')}
                    />
                  )}
                </div>
              </div>
            )}

            {/* Will Be Merged Summary */}
            <div className="px-6 py-4 border-b">
              <h4 className="text-sm font-medium text-gray-700 mb-3">Will Be Merged</h4>
              <div className="space-y-2 text-sm">
                <MergeSummaryRow
                  label="Contact methods"
                  value={
                    preview.methods_to_transfer > 0
                      ? `${preview.methods_to_transfer} unique added${preview.duplicate_methods > 0 ? ` (${preview.duplicate_methods} duplicate skipped)` : ''}`
                      : preview.duplicate_methods > 0
                        ? `${preview.duplicate_methods} duplicate skipped`
                        : 'None'
                  }
                  count={preview.methods_to_transfer}
                />
                <MergeSummaryRow
                  label="Notes"
                  value={preview.notes_to_transfer > 0 ? 'Combined' : 'None'}
                  count={preview.notes_to_transfer}
                />
                <MergeSummaryRow
                  label="Interactions"
                  value={
                    preview.interactions_to_transfer > 0
                      ? `${preview.interactions_to_transfer} added`
                      : 'None'
                  }
                  count={preview.interactions_to_transfer}
                />
                <MergeSummaryRow
                  label="Calendar events"
                  value={
                    preview.calendar_events_to_update > 0
                      ? `${preview.calendar_events_to_update} updated`
                      : 'None'
                  }
                  count={preview.calendar_events_to_update}
                />
              </div>
            </div>
          </>
        )}

        {/* Footer */}
        <div className="px-6 py-4 bg-gray-50 border-t flex items-center justify-end gap-3 rounded-b-lg">
          <Button variant="outline" onClick={onClose} disabled={isLoading}>
            Cancel
          </Button>
          <Button
            onClick={handleMerge}
            loading={isLoading}
            disabled={isLoading || !sourceContactId || isLoadingPreview}
          >
            <GitMerge className="w-4 h-4 mr-1.5" />
            {isLoading ? 'Merging...' : 'Merge Contacts'}
          </Button>
        </div>
      </div>
    </div>
  )
}

// Field toggle component for conflict resolution
function FieldToggle({
  label,
  targetValue,
  sourceValue,
  selected,
  onToggle,
}: {
  label: string
  targetValue: string
  sourceValue: string
  selected: 'source' | 'target'
  onToggle: () => void
}) {
  return (
    <div className="flex flex-wrap items-center justify-between gap-y-1">
      <span className="text-sm text-gray-600 w-24">{label}</span>
      <div className="flex rounded-lg border border-gray-200 overflow-hidden">
        <button
          type="button"
          onClick={selected !== 'target' ? onToggle : undefined}
          aria-pressed={selected === 'target'}
          className={clsx(
            'px-3 py-1.5 text-xs font-medium transition-colors truncate max-w-[140px]',
            selected === 'target'
              ? 'bg-blue-600 text-white'
              : 'bg-white text-gray-700 hover:bg-gray-50'
          )}
          title={targetValue}
        >
          {targetValue}
        </button>
        <button
          type="button"
          onClick={selected !== 'source' ? onToggle : undefined}
          aria-pressed={selected === 'source'}
          className={clsx(
            'px-3 py-1.5 text-xs font-medium transition-colors border-l border-gray-200 truncate max-w-[140px]',
            selected === 'source'
              ? 'bg-blue-600 text-white'
              : 'bg-white text-gray-700 hover:bg-gray-50'
          )}
          title={sourceValue}
        >
          {sourceValue}
        </button>
      </div>
    </div>
  )
}

// Summary row component
function MergeSummaryRow({ label, value, count }: { label: string; value: string; count: number }) {
  return (
    <div className="flex items-center justify-between py-1">
      <span className="text-gray-600">{label}</span>
      <span className={clsx('text-gray-900', count === 0 && 'text-gray-400')}>{value}</span>
    </div>
  )
}
