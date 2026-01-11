'use client'

import { Check, Mail, Phone } from 'lucide-react'
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
  /** The method value to display (email or phone) */
  value: string
  /** Whether this method is selected for import/link */
  selected: boolean
  /** The assigned CRM type (email_personal, email_work, phone, etc.) */
  selectedType: ContactMethodType
  /** Visual state for styling */
  state: MethodState
  /** Callback when checkbox is toggled */
  onToggle: () => void
  /** Callback when type is changed */
  onTypeChange: (type: ContactMethodType) => void
  /** Types that are already used (to disable in dropdown) */
  usedTypes: Set<string>
  /** Whether the selector is disabled */
  disabled?: boolean
  /** Whether this is an email (vs phone) */
  isEmail: boolean
}

export function MethodSelector({
  value,
  selected,
  selectedType,
  state,
  onToggle,
  onTypeChange,
  usedTypes,
  disabled = false,
  isEmail,
}: MethodSelectorProps) {
  // Filter options to only show relevant types
  const relevantOptions = CONTACT_METHOD_OPTIONS.filter(opt => {
    if (isEmail) {
      return opt.value === 'email_personal' || opt.value === 'email_work'
    }
    return opt.value === 'phone'
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
        {isEmail ? <Mail className="w-4 h-4" /> : <Phone className="w-4 h-4" />}
      </div>

      {/* Value */}
      <div className="flex-1 min-w-0">
        <span className="text-sm text-gray-900 truncate block">{value}</span>
      </div>

      {/* Type selector */}
      {isEmail && selected ? (
        <Select
          value={selectedType}
          onChange={e => onTypeChange(e.target.value as ContactMethodType)}
          disabled={disabled}
          className="w-32 text-sm"
          aria-label="Email type"
        >
          {relevantOptions.map(opt => (
            <option
              key={opt.value}
              value={opt.value}
              disabled={usedTypes.has(opt.value) && opt.value !== selectedType}
            >
              {opt.label}
            </option>
          ))}
        </Select>
      ) : (
        <span className="text-xs text-gray-500 w-32">
          {relevantOptions.find(opt => opt.value === selectedType)?.label || selectedType}
        </span>
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
