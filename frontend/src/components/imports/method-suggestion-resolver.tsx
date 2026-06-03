'use client'

import { useState, useEffect, useMemo, useCallback } from 'react'
import { Ban, Check, HelpCircle } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { MethodSelector } from './method-selector'
import { useContact } from '@/hooks/use-contacts'
import { useResolveMethodSuggestions, useDismissMethodSuggestions } from '@/hooks/use-suggestions'
import { detectMethodConflicts } from '@/lib/method-conflict-detection'
import { getSourceDisplay } from '@/lib/source-display'
import type {
  ImportCandidate,
  MethodSuggestion,
  MethodSuggestionMethod,
  MethodComparison,
} from '@/types/import'
import type { ContactMethodType } from '@/types/contact'

interface MethodSuggestionResolverProps {
  suggestion: MethodSuggestion
  onClose: () => void
  onSuccess: (message: string) => void
  onError: (message: string) => void
}

/**
 * Enrich-locked body for a method suggestion: the target contact is fixed
 * (no ContactSelector, no Import), and the pending (type,value) methods feed
 * detectMethodConflicts against the contact's current methods to drive the
 * three-bucket comparison. Confirm enriches; Dismiss is sticky.
 */
export function MethodSuggestionResolver({
  suggestion,
  onClose,
  onSuccess,
  onError,
}: MethodSuggestionResolverProps) {
  const { data: contact } = useContact(suggestion.contact_id)
  const resolveMutation = useResolveMethodSuggestions()
  const dismissMutation = useDismissMethodSuggestions()
  const isLoading = resolveMutation.isPending || dismissMutation.isPending

  const sourceInfo = getSourceDisplay(suggestion.source)
  const SourceIcon = sourceInfo.icon || HelpCircle

  // Per-method selection state, keyed by the pending value (normalized).
  // Pre-select all by default.
  const [selected, setSelected] = useState<Map<string, boolean>>(new Map())

  useEffect(() => {
    const next = new Map<string, boolean>()
    suggestion.methods.forEach(m => next.set(m.value, true))
    setSelected(next)
  }, [suggestion.methods])

  // Adapt the pending methods into an ImportCandidate shape so the existing
  // conflict detector drives the same buckets. Only email/phone are emitted
  // by address books (v1). Unknown types pass through as emails so they
  // still render rather than vanishing.
  const adapter = useMemo<ImportCandidate>(() => {
    const emails: string[] = []
    const phones: string[] = []
    suggestion.methods.forEach(m => {
      if (m.type === 'phone') phones.push(m.value)
      else emails.push(m.value)
    })
    return {
      id: suggestion.external_contact_id,
      source: suggestion.source,
      emails,
      phones,
    }
  }, [suggestion])

  // Map a pending value back to its (type) for the selection submit and for
  // the MethodSelector's selectedType. The submitted (type,value) must match
  // the pending entry exactly (the backend validates by key).
  const typeByValue = useMemo(() => {
    const map = new Map<string, string>()
    suggestion.methods.forEach(m => map.set(m.value, m.type))
    return map
  }, [suggestion.methods])

  const comparisons = useMemo<MethodComparison[]>(() => {
    return detectMethodConflicts(adapter, contact?.methods || [])
  }, [adapter, contact?.methods])

  const toggle = useCallback((value: string) => {
    setSelected(prev => {
      const next = new Map(prev)
      next.set(value, !next.get(value))
      return next
    })
  }, [])

  const selectedMethods = useMemo<MethodSuggestionMethod[]>(() => {
    const out: MethodSuggestionMethod[] = []
    suggestion.methods.forEach(m => {
      if (selected.get(m.value)) out.push({ type: m.type, value: m.value })
    })
    return out
  }, [suggestion.methods, selected])

  const selectedCount = selectedMethods.length

  const handleConfirm = async () => {
    // Confirm requires ≥1 selection — the method body always sends an
    // explicit list (never the empty=all shorthand) so a deselect-all
    // cannot silently confirm everything.
    if (selectedCount === 0) return
    try {
      await resolveMutation.mutateAsync({
        id: suggestion.external_contact_id,
        request: { methods: selectedMethods },
      })
      onSuccess(
        `Added ${selectedCount} method${selectedCount > 1 ? 's' : ''} to ${suggestion.contact_name}`
      )
      onClose()
    } catch (error) {
      onError(error instanceof Error ? error.message : 'Failed to add methods')
    }
  }

  const handleDismissAll = async () => {
    try {
      // Empty methods = dismiss all actionable pending (whole-card).
      await dismissMutation.mutateAsync({
        id: suggestion.external_contact_id,
        request: {},
      })
      onSuccess(`Dismissed suggestions for ${suggestion.contact_name}`)
      onClose()
    } catch (error) {
      onError(error instanceof Error ? error.message : 'Failed to dismiss suggestions')
    }
  }

  // Non-conflict (adding / already-in-CRM) vs conflict buckets. Address
  // books only emit email/phone, so conflicts are rare here, but the
  // detector returns the same shape as the contact-candidate body.
  const addingMethods = comparisons.filter(c => c.conflict_type === 'none')
  const alreadyInCrm = comparisons.filter(c => c.conflict_type === 'identical')

  const renderSelector = (comp: MethodComparison) => {
    const value = comp.external_value
    const type = (typeByValue.get(value) || comp.suggested_crm_type) as ContactMethodType
    const isEmail = type === 'email'
    return (
      <MethodSelector
        key={value}
        value={value}
        selected={comp.conflict_type === 'identical' ? false : Boolean(selected.get(value))}
        selectedType={type}
        state={comp.state}
        onToggle={() => toggle(value)}
        disabled={isLoading || comp.conflict_type === 'identical'}
        isEmail={isEmail}
        lockType
      />
    )
  }

  return (
    <div
      className="fixed inset-0 bg-black/30 backdrop-blur-sm overflow-y-auto h-full w-full z-50"
      onClick={e => {
        if (e.target === e.currentTarget) onClose()
      }}
    >
      <div className="relative top-10 mx-auto p-0 border w-full max-w-xl shadow-lg rounded-lg bg-white overflow-hidden">
        {/* Header — fixed contact, no selector */}
        <div className="px-6 py-4 border-b">
          <div className="flex items-center justify-between gap-2">
            <h3 className="text-lg font-medium text-gray-900 truncate">
              Adding to {suggestion.contact_name}
            </h3>
            <span className="text-xs text-gray-500 flex items-center gap-1 flex-shrink-0">
              <SourceIcon className="w-3.5 h-3.5" />
              {sourceInfo.label}
            </span>
          </div>
          <p className="text-sm text-gray-500 mt-1">
            Confirm the contact methods to add from this source.
          </p>
        </div>

        {/* Methods */}
        <div className="px-6 py-4 max-h-[40vh] overflow-y-auto">
          {comparisons.length === 0 ? (
            <p className="text-sm text-gray-500">No methods to review</p>
          ) : (
            <div className="space-y-4">
              {addingMethods.length > 0 && (
                <div>
                  <h5 className="text-xs font-medium text-gray-500 mb-2 uppercase tracking-wide">
                    Will be added
                  </h5>
                  <div className="space-y-2">{addingMethods.map(renderSelector)}</div>
                </div>
              )}

              {alreadyInCrm.length > 0 && (
                <div>
                  <h5 className="text-xs font-medium text-gray-500 mb-2 uppercase tracking-wide">
                    Already in CRM
                  </h5>
                  <div className="space-y-2">{alreadyInCrm.map(renderSelector)}</div>
                </div>
              )}
            </div>
          )}
        </div>

        {/* Footer */}
        <div className="px-6 py-4 bg-gray-50 border-t flex items-center justify-between">
          <Button
            variant="ghost"
            onClick={handleDismissAll}
            loading={dismissMutation.isPending}
            disabled={isLoading}
            className="text-gray-500"
          >
            <Ban className="w-4 h-4 mr-1" />
            {dismissMutation.isPending ? 'Dismissing...' : 'Dismiss'}
          </Button>

          <div className="flex gap-2">
            <Button variant="outline" onClick={onClose} disabled={isLoading}>
              Cancel
            </Button>
            <Button
              onClick={handleConfirm}
              loading={resolveMutation.isPending}
              disabled={isLoading || selectedCount === 0}
            >
              <Check className="w-4 h-4 mr-1" />
              {resolveMutation.isPending ? 'Adding...' : 'Confirm'}
            </Button>
          </div>
        </div>
      </div>
    </div>
  )
}
