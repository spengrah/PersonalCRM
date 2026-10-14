'use client'

import { useEffect, useId, type ReactNode } from 'react'
import { X } from 'lucide-react'

interface CorrectionDialogProps {
  title: string
  onClose: () => void
  children: ReactNode
  footer: ReactNode
}

/** The modal frame both correction flows share: a titled dialog closed by Escape, the backdrop or the close button. */
export function CorrectionDialog({ title, onClose, children, footer }: CorrectionDialogProps) {
  const titleId = useId()

  useEffect(() => {
    function handleKeyDown(e: KeyboardEvent) {
      if (e.key === 'Escape') onClose()
    }
    window.addEventListener('keydown', handleKeyDown)
    return () => window.removeEventListener('keydown', handleKeyDown)
  }, [onClose])

  return (
    <div
      className="fixed inset-0 z-50 flex items-start justify-center overflow-y-auto bg-black/30 px-4 pt-4 backdrop-blur-sm sm:pt-20"
      onClick={e => {
        if (e.target === e.currentTarget) onClose()
      }}
      role="dialog"
      aria-modal="true"
      aria-labelledby={titleId}
    >
      <div className="mb-4 w-full max-w-2xl rounded-lg bg-white shadow-xl">
        <div className="flex items-center justify-between border-b border-gray-200 px-4 py-3">
          <h2 id={titleId} className="text-lg font-medium text-gray-900">
            {title}
          </h2>
          <button
            type="button"
            onClick={onClose}
            className="rounded-full p-1 text-gray-400 hover:bg-gray-100 hover:text-gray-600"
            aria-label="Close"
          >
            <X className="h-5 w-5" />
          </button>
        </div>
        <div className="space-y-4 px-4 py-4">{children}</div>
        <div className="flex justify-end gap-3 border-t border-gray-200 px-4 py-3">{footer}</div>
      </div>
    </div>
  )
}
