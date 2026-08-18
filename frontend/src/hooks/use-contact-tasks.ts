import { useMutation, useQuery } from '@tanstack/react-query'
import { contactTasksApi } from '@/lib/contact-tasks-api'
import { contactTaskKeys, invalidateFor } from '@/lib/query-invalidation'
import type { ContactTaskListParams, CreateManualTaskRequest } from '@/types/contact-task'
import { staleTime } from '@/lib/query-client'

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
    staleTime: staleTime(1000 * 60 * 2), // 2 minutes
  })
}

// Create manual (user-picker) task mutation. Callers must supply
// `kind` (one of reach_out / send / reminder); see AddTaskModal for
// the UI surface that picks one.
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
