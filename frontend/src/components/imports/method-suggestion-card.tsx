'use client'

import { HelpCircle } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { getSourceDisplay } from '@/lib/source-display'
import type { MethodSuggestion } from '@/types/import'

interface MethodSuggestionCardProps {
  suggestion: MethodSuggestion
  onReview: () => void
  onDismiss: () => void
  dismissLoading: boolean
}

/**
 * A method-kind queue card: "‹contact name› — N new methods", listing the
 * discovered values + originating source, with Review (opens the
 * enrich-locked body) and Dismiss (whole-card sticky dismiss).
 */
export function MethodSuggestionCard({
  suggestion,
  onReview,
  onDismiss,
  dismissLoading,
}: MethodSuggestionCardProps) {
  const sourceInfo = getSourceDisplay(suggestion.source)
  const SourceIcon = sourceInfo.icon || HelpCircle
  const count = suggestion.methods.length

  return (
    <div className="p-4 bg-white border border-gray-200 rounded-lg hover:shadow-sm transition-shadow">
      <div className="flex items-start justify-between gap-4">
        <div className="flex-1 min-w-0">
          <div className="flex items-center flex-wrap gap-2">
            <h3 className="text-base font-medium text-gray-900">
              {suggestion.contact_name} — {count} new method{count > 1 ? 's' : ''}
            </h3>
            <span className="text-xs text-gray-500 flex items-center gap-1">
              <SourceIcon className="w-3.5 h-3.5" />
              {sourceInfo.label}
            </span>
          </div>
          <div className="mt-2 flex flex-wrap gap-1.5">
            {suggestion.methods.map(m => (
              <span
                key={`${m.type}:${m.value}`}
                className="inline-flex items-center px-2 py-0.5 rounded bg-gray-100 text-sm text-gray-700"
              >
                {m.value}
              </span>
            ))}
          </div>
        </div>

        <div className="flex items-center space-x-2 ml-4 flex-shrink-0">
          <Button size="sm" onClick={onReview} disabled={dismissLoading}>
            Review
          </Button>
          <Button
            size="sm"
            variant="ghost"
            onClick={onDismiss}
            loading={dismissLoading}
            className="text-gray-500 hover:text-gray-700"
          >
            Dismiss
          </Button>
        </div>
      </div>
    </div>
  )
}
