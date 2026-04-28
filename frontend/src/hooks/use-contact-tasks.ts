import { useMutation, useQuery } from '@tanstack/react-query'
import { contactTasksApi } from '@/lib/contact-tasks-api'
import { contactTaskKeys, invalidateFor } from '@/lib/query-invalidation'
import type { ContactTaskListParams, CreateManualTaskRequest } from '@/types/contact-task'

// List tasks for a contact
export function useContactTasks(
  contactId: string,
  params: ContactTaskListParams = {},
  options: { enabled?: boolean } = {}
) {
  const { enabled = true } = options
  return useQuery({
    queryKey: contactTaskKeys.list(contactId, params),
    queryFn: () => contactTasksApi.listTasks(contactId, params),
    enabled: !!contactId && enabled,
    staleTime: 1000 * 60 * 2, // 2 minutes
  })
}

// Create manual (user-picker) task mutation. Kind defaults to "reach_out"
// in callers that don't yet expose the picker UI; the AddTaskModal kind
// picker (PR-B Commit 3) replaces that hardcoded default.
export function useCreateManualTask() {
  return useMutation({
    mutationFn: ({ contactId, data }: { contactId: string; data: CreateManualTaskRequest }) =>
      contactTasksApi.createTask(contactId, data),
    onSuccess: () => {
      invalidateFor('task:created')
    },
  })
}

// Delete task link mutation
export function useDeleteTaskLink() {
  return useMutation({
    mutationFn: ({ contactId, taskId }: { contactId: string; taskId: string }) =>
      contactTasksApi.deleteTaskLink(contactId, taskId),
    onSuccess: () => {
      invalidateFor('task:deleted')
    },
  })
}
