'use client'

import { CheckCircle } from 'lucide-react'

/**
 * Shown on the Interactions tab when conflicts + orphans are both zero.
 * Name candidates may still render below on the People tab.
 */
export function InteractionsEmptyState() {
  return (
    <div className="rounded-lg border border-gray-200 bg-white py-12 text-center">
      <CheckCircle className="mx-auto h-12 w-12 text-green-500" />
      <h3 className="mt-2 text-sm font-medium text-gray-900">Nothing needs attention</h3>
      <p className="mt-1 text-sm text-gray-500">
        Every Anarlog session has been matched to a meeting or logged.
      </p>
    </div>
  )
}
