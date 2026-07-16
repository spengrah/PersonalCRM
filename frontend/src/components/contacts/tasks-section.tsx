'use client'

import { useState } from 'react'
import { useDeleteTaskLink } from '@/hooks/use-contact-tasks'
import { Button } from '@/components/ui/button'
import { Plus, Circle, CheckCircle2, Clock, ExternalLink, X, ListTodo } from 'lucide-react'
import { AddTaskModal } from './add-task-modal'
import { formatDateOnly } from '@/lib/utils'
import type { ContactTask } from '@/types/contact-task'

/**
 * Cleans markdown links from task content for display in CRM UI.
 * - Action tasks: "[Name](url): task" → "task"
 * - Cadence tasks: "Reach out to [Name](url)" → "Reach out to Name"
 */
function cleanTaskContent(content: string | undefined): string {
  if (!content) return 'Untitled task'

  // First, strip leading markdown link prefix with colon
  let cleaned = content.replace(/^\[([^\]]+)\]\([^)]+\):\s*/, '')

  // Then replace any remaining markdown links with just their text
  cleaned = cleaned.replace(/\[([^\]]+)\]\([^)]+\)/g, '$1')

  return cleaned.trim() || 'Untitled task'
}

interface TasksSectionProps {
  contactId: string
  contactName: string
  activeTasks: ContactTask[]
  completedTasks: ContactTask[]
  loadingActive: boolean
  loadingCompleted: boolean
}

export function TasksSection({
  contactId,
  contactName,
  activeTasks,
  completedTasks,
  loadingActive,
  loadingCompleted,
}: TasksSectionProps) {
  const [showAddModal, setShowAddModal] = useState(false)
  const [showCompleted, setShowCompleted] = useState(false)

  const isLoading = loadingActive || (showCompleted && loadingCompleted)
  // Show empty state only when both lists are truly empty
  const hasNoTasks = activeTasks.length === 0 && completedTasks.length === 0

  return (
    <section aria-label="Tasks" className="bg-white shadow overflow-hidden sm:rounded-lg">
      <div className="px-4 py-5 sm:px-6 border-b border-gray-200 flex items-center justify-between">
        <div className="flex items-center gap-2">
          <ListTodo className="w-5 h-5 text-gray-400" />
          <h3 className="text-lg leading-6 font-medium text-gray-900">Tasks</h3>
        </div>
        <Button variant="outline" size="sm" onClick={() => setShowAddModal(true)}>
          <Plus className="w-4 h-4 mr-1" />
          Add
        </Button>
      </div>

      <div className="divide-y divide-gray-200">
        {isLoading ? (
          <div className="px-4 py-4 sm:px-6">
            <div className="animate-pulse space-y-3">
              <div className="h-5 bg-gray-200 rounded w-3/4"></div>
              <div className="h-5 bg-gray-200 rounded w-1/2"></div>
            </div>
          </div>
        ) : hasNoTasks ? (
          <div className="px-4 py-8 sm:px-6 text-center">
            <p className="text-sm text-gray-500">No tasks yet</p>
            <p className="text-xs text-gray-400 mt-1">
              Add a task to track follow-ups for this contact
            </p>
          </div>
        ) : (
          <>
            {/* Active tasks */}
            {activeTasks.map(task => (
              <TaskRow key={task.id} task={task} contactId={contactId} completed={false} />
            ))}

            {/* Completed tasks toggle */}
            {completedTasks.length > 0 && (
              <div className="px-4 py-2 sm:px-6 bg-gray-50">
                <button
                  onClick={() => setShowCompleted(!showCompleted)}
                  className="text-sm text-gray-500 hover:text-gray-700"
                >
                  {showCompleted ? 'Hide' : 'Show'} completed ({completedTasks.length})
                </button>
              </div>
            )}

            {/* Completed tasks */}
            {showCompleted &&
              completedTasks.map(task => (
                <TaskRow key={task.id} task={task} contactId={contactId} completed={true} />
              ))}
          </>
        )}
      </div>

      {/* Add Task Modal */}
      {showAddModal && (
        <AddTaskModal
          contactId={contactId}
          contactName={contactName}
          onClose={() => setShowAddModal(false)}
        />
      )}
    </section>
  )
}

interface TaskRowProps {
  task: {
    id: string
    content?: string
    due_date?: string
    external_task_id: string
    kind?: string
    lifecycle?: string
  }
  contactId: string
  completed: boolean
}

// getBadgeLabel derives the display label from the (kind, lifecycle) pair.
// Follow-up rows are kind=reach_out + lifecycle=followup_loop; cadence rows
// are kind=reach_out + lifecycle=cadence_due; everything else uses the kind
// directly.
function getBadgeLabel(kind: string | undefined, lifecycle: string | undefined): string {
  if (kind === 'reach_out' && lifecycle === 'cadence_due') return 'Cadence'
  if (kind === 'reach_out' && lifecycle === 'followup_loop') return 'Follow-up'
  if (kind === 'reach_out') return 'Reach out'
  if (kind === 'send') return 'Send'
  if (kind === 'reminder') return 'Reminder'
  if (kind === 'meet') return 'Meet'
  if (kind === 'action') return 'Action'
  return kind ?? ''
}

function TaskRow({ task, contactId, completed }: TaskRowProps) {
  const todoistUrl = `todoist://task?id=${task.external_task_id}`
  const deleteTaskLink = useDeleteTaskLink()

  const handleUnlink = () => {
    if (confirm('Remove this task from CRM? (The task will remain in Todoist)')) {
      deleteTaskLink.mutate({ contactId, taskId: task.id })
    }
  }

  const badge = getBadgeLabel(task.kind, task.lifecycle)

  return (
    <div className="px-4 py-3 sm:px-6 flex items-center gap-3 group">
      {/* Task icon — follow-up tasks use Clock, others use Circle/CheckCircle */}
      {completed ? (
        <CheckCircle2 className="w-5 h-5 text-gray-400 flex-shrink-0" />
      ) : task.lifecycle === 'followup_loop' ? (
        <Clock
          role="img"
          aria-label="Awaiting reply"
          className="w-5 h-5 text-amber-400 flex-shrink-0"
        />
      ) : (
        <Circle className="w-5 h-5 text-gray-300 flex-shrink-0" />
      )}

      {/* Task content */}
      <div className="flex-1 min-w-0">
        <p
          className={`text-sm truncate ${completed ? 'text-gray-400 line-through' : 'text-gray-900'}`}
        >
          {cleanTaskContent(task.content)}
        </p>
      </div>

      {/* Kind/lifecycle badge */}
      {badge ? (
        <span className="flex-shrink-0 inline-flex items-center px-2 py-0.5 rounded text-xs font-medium bg-gray-100 text-gray-700">
          {badge}
        </span>
      ) : null}

      {/* Due date */}
      <div className="flex-shrink-0">
        {task.due_date ? (
          <span className="text-sm text-gray-500">
            {formatDateOnly(task.due_date, { month: 'short', day: 'numeric' })}
          </span>
        ) : (
          <span className="text-sm text-gray-400">—</span>
        )}
      </div>

      {/* Link to Todoist (visible on hover) */}
      <a
        href={todoistUrl}
        target="_blank"
        rel="noopener noreferrer"
        className="flex-shrink-0 opacity-0 group-hover:opacity-100 transition-opacity text-gray-400 hover:text-gray-600"
        title="Open in Todoist"
      >
        <ExternalLink className="w-4 h-4" />
      </a>

      {/* Unlink from CRM (visible on hover) */}
      <button
        onClick={handleUnlink}
        disabled={deleteTaskLink.isPending}
        className="flex-shrink-0 opacity-0 group-hover:opacity-100 transition-opacity text-gray-400 hover:text-red-500 disabled:opacity-50"
        title="Remove from CRM (keeps in Todoist)"
      >
        <X className="w-4 h-4" />
      </button>
    </div>
  )
}
