'use client'

import { Check, Mail, Phone, Send } from 'lucide-react'
import { clsx } from 'clsx'
import { Select } from '@/components/ui/select'
import { CONTACT_METHOD_OPTIONS } from '@/lib/contact-methods'
import {
  getMethodStateClasses,
  getMethodStateBadgeText,
  getMethodStateBadgeClasses,
} from '@/lib/method-conflict-detection'
import type { MethodState } from '@/types/import'
import type { ContactMethodType } from '@/types/contact'

interface MethodSelectorProps {
  /** The method value to display (email, phone, or handle) */
  value: string
  /** Whether this method is selected for import/link */
  selected: boolean
  /** The assigned CRM type (email, phone, telegram, etc.) */
  selectedType: ContactMethodType
  /** Visual state for styling */
  state: MethodState
  /** Callback when checkbox is toggled */
  onToggle: () => void
  /** Callback when type is changed (only used for emails, which support
   * subtypes). Optional — omit it (with lockType) in the method-suggestion
   * body where the (type,value) is fixed and the type must not be changed. */
  onTypeChange?: (type: ContactMethodType) => void
  /** Whether the selector is disabled */
  disabled?: boolean
  /** Whether this is an email (vs phone/handle). When false, no type dropdown is shown. */
  isEmail: boolean
  /** When true, render a static type label even for emails (no dropdown).
   * Used by the method-suggestion body, where the submitted (type,value)
   * must match the pending entry exactly and the type cannot be reassigned. */
  lockType?: boolean
  /** Whether this method is marked as primary */
  isPrimary?: boolean
  /** Callback when primary star is clicked */
  onPrimaryToggle?: () => void
}

export function MethodSelector({
  value,
  selected,
  selectedType,
  state,
  onToggle,
  onTypeChange,
  disabled = false,
  isEmail,
  lockType = false,
  isPrimary = false,
  onPrimaryToggle,
}: MethodSelectorProps) {
  // Filter options to only show relevant types. Email has multiple subtypes;
  // phone and handle-based types (telegram, signal, etc.) just render a label.
  const relevantOptions = CONTACT_METHOD_OPTIONS.filter(opt => {
    if (isEmail) return opt.value === 'email'
    return opt.value === selectedType
  })

  const stateClasses = getMethodStateClasses(state)
  const badgeText = getMethodStateBadgeText(state)
  const badgeClasses = getMethodStateBadgeClasses(state)

  return (
    <div
      className={clsx(
        'flex items-center gap-3 p-3 rounded-lg border transition-colors',
        stateClasses,
        !selected && 'opacity-60'
      )}
    >
      {/* Checkbox */}
      <button
        type="button"
        onClick={onToggle}
        disabled={disabled}
        aria-pressed={selected}
        className={clsx(
          'flex-shrink-0 w-5 h-5 rounded border-2 flex items-center justify-center transition-colors',
          selected
            ? 'bg-blue-600 border-blue-600 text-white'
            : 'border-gray-300 hover:border-blue-400',
          disabled && 'opacity-50 cursor-not-allowed'
        )}
        aria-label={selected ? 'Deselect method' : 'Select method'}
      >
        {selected && <Check className="w-3 h-3" />}
      </button>

      {/* Icon */}
      <div className="flex-shrink-0 text-gray-400">
        {isEmail ? (
          <Mail className="w-4 h-4" />
        ) : selectedType === 'telegram' ? (
          <Send className="w-4 h-4" />
        ) : (
          <Phone className="w-4 h-4" />
        )}
      </div>

      {/* Value */}
      <div className="flex-1 min-w-0">
        <span className="text-sm text-gray-900 truncate block">{value}</span>
      </div>

      {/* Type selector — emails get a subtype dropdown UNLESS locked. When
          lockType is set (method-suggestion body), the type is fixed and
          rendered as a static label so the submitted (type,value) matches
          the pending entry. */}
      {isEmail && selected && !lockType ? (
        <Select
          value={selectedType}
          onChange={e => onTypeChange?.(e.target.value as ContactMethodType)}
          disabled={disabled}
          className="w-32 text-sm"
          aria-label="Email type"
        >
          {relevantOptions.map(opt => (
            <option key={opt.value} value={opt.value}>
              {opt.label}
            </option>
          ))}
        </Select>
      ) : (
        <span className="text-xs text-gray-500 w-32">
          {relevantOptions.find(opt => opt.value === selectedType)?.label || selectedType}
        </span>
      )}

      {/* Primary star - only shown for selected methods */}
      {selected && onPrimaryToggle ? (
        <button
          type="button"
          onClick={onPrimaryToggle}
          disabled={disabled}
          aria-pressed={isPrimary}
          aria-label="Set as primary"
          className={clsx(
            'p-1.5 transition-colors flex-shrink-0',
            isPrimary ? 'text-yellow-500' : 'text-gray-300 hover:text-yellow-500',
            disabled && 'opacity-50 cursor-not-allowed'
          )}
          title={isPrimary ? 'Primary contact method' : 'Set as primary'}
        >
          <svg className="w-4 h-4" fill="currentColor" viewBox="0 0 20 20">
            <path d="M9.049 2.927c.3-.921 1.603-.921 1.902 0l1.07 3.292a1 1 0 00.95.69h3.462c.969 0 1.371 1.24.588 1.81l-2.8 2.034a1 1 0 00-.364 1.118l1.07 3.292c.3.921-.755 1.688-1.54 1.118l-2.8-2.034a1 1 0 00-1.175 0l-2.8 2.034c-.784.57-1.838-.197-1.539-1.118l1.07-3.292a1 1 0 00-.364-1.118L2.98 8.72c-.783-.57-.38-1.81.588-1.81h3.461a1 1 0 00.951-.69l1.07-3.292z" />
          </svg>
        </button>
      ) : (
        // Placeholder to maintain layout when star is hidden
        <span className="w-7 flex-shrink-0" />
      )}

      {/* State badge */}
      {badgeText && (
        <span className={clsx('text-xs px-2 py-0.5 rounded-full flex-shrink-0', badgeClasses)}>
          {badgeText}
        </span>
      )}
    </div>
  )
}
