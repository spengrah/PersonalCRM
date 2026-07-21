'use client'

import { ArrowLeft, ChevronLeft, ChevronRight } from 'lucide-react'
import { clsx } from 'clsx'

interface ContactNavigationBarProps {
  /** Return to the contact list at the user's place */
  onBack: () => void
  /** Navigate to previous contact */
  onPrevious: () => void
  /** Navigate to next contact */
  onNext: () => void
  /** Whether navigation backward is possible */
  canGoBack: boolean
  /** Whether navigation forward is possible */
  canGoForward: boolean
  /** Current position in the list (0-indexed) */
  currentIndex: number
  /** Total number of contacts in the list */
  totalCount: number
  /** Whether contact is being edited (disables navigation) */
  isEditMode?: boolean
  /** Whether navigation is loading */
  isLoading?: boolean
}

/**
 * Compact navigation bar for contact details page
 *
 * Shows: [Back to list] ... [‹ N of M ›]
 *
 * The bar background is full-bleed; its contents are constrained to the same
 * max-w-4xl column as the detail body so "Back to list" aligns with the
 * contact name (left) and the pager aligns with the action buttons (right).
 *
 * Visual states:
 * - Enabled: text-gray-500, hover:text-gray-700
 * - Disabled (boundary): text-gray-400, cursor-not-allowed
 * - Disabled (edit mode): text-gray-300, cursor-not-allowed
 */
export function ContactNavigationBar({
  onBack,
  onPrevious,
  onNext,
  canGoBack,
  canGoForward,
  currentIndex,
  totalCount,
  isEditMode = false,
  isLoading = false,
}: ContactNavigationBarProps) {
  const isPrevDisabled = !canGoBack || isEditMode || isLoading
  const isNextDisabled = !canGoForward || isEditMode || isLoading
  // Back stays available whenever the user isn't mid-edit — it's the return
  // escape hatch and works regardless of the id list's load state (it falls
  // back to page 1 gracefully if clicked before the index resolves).
  const isBackDisabled = isEditMode

  // Determine button styles based on state
  const getButtonStyles = (isDisabled: boolean) => {
    if (isEditMode) {
      return 'text-gray-300 cursor-not-allowed'
    }
    if (isDisabled) {
      return 'text-gray-400 cursor-not-allowed'
    }
    return 'text-gray-500 hover:text-gray-700 hover:bg-white/50'
  }

  return (
    <div className="py-1.5 bg-gray-100 border-b border-gray-200">
      <div className="max-w-4xl mx-auto sm:px-6 lg:px-8 flex items-center justify-between">
        <button
          onClick={onBack}
          disabled={isBackDisabled}
          title={isEditMode ? 'Save or discard changes to navigate' : 'Back to list'}
          className={clsx(
            'inline-flex items-center gap-1.5 leading-none p-1 rounded transition-colors text-sm',
            'focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-blue-500',
            getButtonStyles(isBackDisabled)
          )}
        >
          <ArrowLeft className="w-4 h-4 block" aria-hidden="true" />
          <span className="block">Back to list</span>
        </button>

        <div className="flex items-center gap-2">
          <button
            onClick={onPrevious}
            disabled={isPrevDisabled}
            title={
              isEditMode
                ? 'Save or discard changes to navigate'
                : canGoBack
                  ? 'Previous contact (←)'
                  : 'At first contact'
            }
            aria-label="Previous contact"
            className={clsx('p-1 rounded transition-colors', getButtonStyles(isPrevDisabled))}
          >
            <ChevronLeft className="w-4 h-4" />
          </button>

          <span className="text-xs text-gray-600">
            {totalCount > 0 ? (
              <>
                <span className="font-medium">{currentIndex + 1}</span>
                {' of '}
                <span className="font-medium">{totalCount}</span>
              </>
            ) : (
              <span className="text-gray-400">No contacts</span>
            )}
          </span>

          <button
            onClick={onNext}
            disabled={isNextDisabled}
            title={
              isEditMode
                ? 'Save or discard changes to navigate'
                : canGoForward
                  ? 'Next contact (→)'
                  : 'At last contact'
            }
            aria-label="Next contact"
            className={clsx('p-1 rounded transition-colors', getButtonStyles(isNextDisabled))}
          >
            <ChevronRight className="w-4 h-4" />
          </button>
        </div>
      </div>
    </div>
  )
}
