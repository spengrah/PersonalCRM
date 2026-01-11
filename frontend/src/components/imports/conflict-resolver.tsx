'use client'

import { Mail, Phone, ArrowRight } from 'lucide-react'
import { clsx } from 'clsx'
import type { MethodComparison } from '@/types/import'

interface ConflictResolverProps {
  /** The method comparison showing the conflict */
  comparison: MethodComparison
  /** Current resolution choice */
  resolution: 'use_crm' | 'use_external'
  /** Callback when resolution changes */
  onResolve: (resolution: 'use_crm' | 'use_external') => void
  /** Whether the resolver is disabled */
  disabled?: boolean
}

export function ConflictResolver({
  comparison,
  resolution,
  onResolve,
  disabled = false,
}: ConflictResolverProps) {
  const isEmail = comparison.external_type === 'email'
  const Icon = isEmail ? Mail : Phone

  // Determine conflict description
  let conflictDescription = ''
  if (comparison.conflict_type === 'value_conflict') {
    conflictDescription = `Different ${isEmail ? 'email' : 'phone'} exists for this type`
  } else if (comparison.conflict_type === 'type_conflict') {
    conflictDescription = 'Same value exists with different type'
  }

  return (
    <div className="rounded-lg border border-red-200 bg-red-50 p-4">
      {/* Conflict header */}
      <div className="flex items-center gap-2 mb-3">
        <Icon className="w-4 h-4 text-red-600" />
        <span className="text-sm font-medium text-red-800">Conflict</span>
        <span className="text-xs text-red-600">{conflictDescription}</span>
      </div>

      {/* Side-by-side comparison */}
      <div className="flex items-center gap-3">
        {/* CRM value */}
        <label
          className={clsx(
            'flex-1 p-3 rounded-lg border cursor-pointer transition-all',
            resolution === 'use_crm'
              ? 'border-blue-500 bg-blue-50 ring-2 ring-blue-200'
              : 'border-gray-200 bg-white hover:border-gray-300',
            disabled && 'opacity-50 cursor-not-allowed'
          )}
        >
          <input
            type="radio"
            name={`conflict-${comparison.external_value}`}
            value="use_crm"
            checked={resolution === 'use_crm'}
            onChange={() => onResolve('use_crm')}
            disabled={disabled}
            className="sr-only"
          />
          <div className="text-xs text-gray-500 mb-1">Keep CRM value</div>
          <div className="text-sm font-medium text-gray-900 truncate">
            {comparison.crm_method?.value || 'No value'}
          </div>
          <div className="text-xs text-gray-500 mt-1">
            Type: {comparison.crm_method?.type || 'N/A'}
          </div>
        </label>

        {/* Arrow */}
        <ArrowRight className="w-4 h-4 text-gray-400 flex-shrink-0" />

        {/* External value */}
        <label
          className={clsx(
            'flex-1 p-3 rounded-lg border cursor-pointer transition-all',
            resolution === 'use_external'
              ? 'border-blue-500 bg-blue-50 ring-2 ring-blue-200'
              : 'border-gray-200 bg-white hover:border-gray-300',
            disabled && 'opacity-50 cursor-not-allowed'
          )}
        >
          <input
            type="radio"
            name={`conflict-${comparison.external_value}`}
            value="use_external"
            checked={resolution === 'use_external'}
            onChange={() => onResolve('use_external')}
            disabled={disabled}
            className="sr-only"
          />
          <div className="text-xs text-gray-500 mb-1">Use external value</div>
          <div className="text-sm font-medium text-gray-900 truncate">
            {comparison.external_value}
          </div>
          <div className="text-xs text-gray-500 mt-1">Type: {comparison.suggested_crm_type}</div>
        </label>
      </div>

      {/* Selection indicator */}
      <div className="mt-3 text-xs text-gray-600">
        {resolution === 'use_crm' ? (
          <span>Keeping existing CRM value (no changes)</span>
        ) : (
          <span className="text-blue-600">Will replace CRM value with external value</span>
        )}
      </div>
    </div>
  )
}
