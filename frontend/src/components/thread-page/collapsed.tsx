'use client'

import { useState, type ReactNode } from 'react'
import { ChevronDown, ChevronRight } from 'lucide-react'

/**
 * A run of messages behind a toggle, collapsed until the user opens it. The
 * label is the toggle's name; the note, if any, sits beside it.
 */
export function Collapsed({
  label,
  note,
  children,
}: {
  label: string
  note?: string
  children: ReactNode
}) {
  const [open, setOpen] = useState(false)
  const Caret = open ? ChevronDown : ChevronRight
  return (
    <div data-collapsed>
      <div className="flex items-center gap-2 px-3 py-2 text-sm">
        <button
          type="button"
          aria-expanded={open}
          onClick={() => setOpen(!open)}
          className="inline-flex items-center gap-1 font-medium text-gray-600 hover:text-gray-900"
        >
          <Caret aria-hidden="true" className="h-4 w-4" />
          {label}
        </button>
        {note && <span className="text-xs text-gray-500">{note}</span>}
      </div>
      {open && <div className="space-y-1">{children}</div>}
    </div>
  )
}
