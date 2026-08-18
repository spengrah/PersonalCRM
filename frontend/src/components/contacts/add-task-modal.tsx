'use client'

import { useState, useEffect, useRef } from 'react'
import { useCreateManualTask } from '@/hooks/use-contact-tasks'
import { Button } from '@/components/ui/button'
import { X, ChevronDown } from 'lucide-react'

interface AddTaskModalProps {
  contactId: string
  contactName: string
  onClose: () => void
}

type TaskKind = 'reach_out' | 'send' | 'reminder'

export function AddTaskModal({ contactId, contactName, onClose }: AddTaskModalProps) {
  const [kind, setKind] = useState<TaskKind>('reach_out')
  const [text, setText] = useState('')
  const [notes, setNotes] = useState('')
  const [showNotes, setShowNotes] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const inputRef = useRef<HTMLInputElement>(null)

  const createTask = useCreateManualTask()

  // Focus input on mount
  useEffect(() => {
    inputRef.current?.focus()
  }, [])

  // Handle escape key
  useEffect(() => {
    function handleKeyDown(e: KeyboardEvent) {
      if (e.key === 'Escape') {
        onClose()
      }
    }
    window.addEventListener('keydown', handleKeyDown)
    return () => window.removeEventListener('keydown', handleKeyDown)
  }, [onClose])

  const handleSubmit = async (e: React.FormEvent) => {
    e.preventDefault()
    setError(null)

    if (!text.trim()) {
      setError('Task text is required')
      return
    }

    try {
      await createTask.mutateAsync({
        contactId,
        data: {
          kind,
          text: text.trim(),
          notes: notes.trim() || undefined,
        },
      })
      onClose()
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Failed to create task')
    }
  }

  const kindOptions: { value: TaskKind; label: string }[] = [
    { value: 'reach_out', label: 'Reach out' },
    { value: 'send', label: 'Send' },
    { value: 'reminder', label: 'Reminder' },
  ]

  return (
    <div
      className="fixed inset-0 bg-black/30 backdrop-blur-sm z-50 flex items-start justify-center overflow-y-auto pt-4 sm:pt-20 px-4"
      onClick={e => {
        if (e.target === e.currentTarget) onClose()
      }}
    >
      <div className="bg-white rounded-lg shadow-xl max-w-lg w-full mb-4">
        {/* Header */}
        <div className="px-4 py-3 border-b border-gray-200 flex items-center justify-between">
          <h2 className="text-lg font-medium text-gray-900">Add Task for {contactName}</h2>
          <button
            onClick={onClose}
            className="text-gray-400 hover:text-gray-600 p-1 rounded-full hover:bg-gray-100"
          >
            <X className="w-5 h-5" />
          </button>
        </div>

        {/* Form */}
        <form onSubmit={handleSubmit}>
          <div className="px-4 py-4 space-y-4">
            {/* Kind picker */}
            <div>
              <label className="block text-sm font-medium text-gray-700 mb-1">Type</label>
              <div className="inline-flex rounded-md shadow-sm" role="group" aria-label="Task type">
                {kindOptions.map((opt, idx) => {
                  const isSelected = kind === opt.value
                  const base =
                    'inline-flex items-center px-3 py-2 text-sm font-medium border border-gray-300 focus:outline-none focus:ring-2 focus:ring-blue-500 focus:z-10'
                  const position =
                    idx === 0
                      ? 'rounded-l-md'
                      : idx === kindOptions.length - 1
                        ? 'rounded-r-md -ml-px'
                        : '-ml-px'
                  const tone = isSelected
                    ? 'bg-blue-600 text-white border-blue-600'
                    : 'bg-white text-gray-700 hover:bg-gray-50'
                  return (
                    <button
                      key={opt.value}
                      type="button"
                      aria-pressed={isSelected}
                      onClick={() => setKind(opt.value)}
                      className={`${base} ${position} ${tone}`}
                    >
                      {opt.label}
                    </button>
                  )
                })}
              </div>
            </div>

            {/* Task text input */}
            <div>
              <input
                ref={inputRef}
                type="text"
                value={text}
                onChange={e => setText(e.target.value)}
                placeholder="Follow up about surgery next tuesday p2"
                className="w-full px-3 py-2 border border-gray-300 rounded-md shadow-sm focus:ring-blue-500 focus:border-blue-500 text-sm text-gray-900 placeholder-gray-400"
              />
              <p className="mt-1 text-xs text-gray-500">Supports: dates, #project, @label, p1-p4</p>
            </div>

            {/* Notes toggle */}
            <div>
              <button
                type="button"
                onClick={() => setShowNotes(!showNotes)}
                className="flex items-center text-sm text-gray-600 hover:text-gray-900"
              >
                <ChevronDown
                  className={`w-4 h-4 mr-1 transition-transform ${showNotes ? 'rotate-180' : ''}`}
                />
                Add notes
              </button>

              {showNotes && (
                <textarea
                  value={notes}
                  onChange={e => setNotes(e.target.value)}
                  placeholder="Additional context for this task..."
                  rows={3}
                  className="mt-2 w-full px-3 py-2 border border-gray-300 rounded-md shadow-sm focus:ring-blue-500 focus:border-blue-500 text-sm text-gray-900 placeholder-gray-400"
                />
              )}
            </div>

            {/* Error message */}
            {error && (
              <div className="text-sm text-red-600 bg-red-50 px-3 py-2 rounded-md">{error}</div>
            )}
          </div>

          {/* Footer */}
          <div className="px-4 py-3 border-t border-gray-200 flex justify-end gap-3">
            <Button type="button" variant="outline" onClick={onClose}>
              Cancel
            </Button>
            <Button type="submit" loading={createTask.isPending} disabled={!text.trim()}>
              Add Task
            </Button>
          </div>
        </form>
      </div>
    </div>
  )
}
