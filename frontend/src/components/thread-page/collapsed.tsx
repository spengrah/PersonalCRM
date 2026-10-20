'use client'

import { useState, type ReactNode } from 'react'
import { ChevronDown, ChevronUp } from 'lucide-react'

/**
 * A run of messages behind an accordion bar, collapsed until the user opens
 * it. The bar spans its card's width with the label centered; the label is the
 * toggle's name.
 */
export function Collapsed({ label, children }: { label: string; children: ReactNode }) {
  const [open, setOpen] = useState(false)
  const Caret = open ? ChevronUp : ChevronDown
  return (
    <div data-collapsed className="py-1">
      <button
        type="button"
        aria-expanded={open}
        onClick={() => setOpen(!open)}
        className="flex w-full items-center justify-center gap-2 rounded-md bg-gray-100 px-3 py-2 text-sm font-medium text-gray-600 hover:bg-gray-200 hover:text-gray-900"
      >
        {label}
        <Caret aria-hidden="true" className="h-4 w-4" />
      </button>
      {open && <div className="mt-1 space-y-1">{children}</div>}
    </div>
  )
}
