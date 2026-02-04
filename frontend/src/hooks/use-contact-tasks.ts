import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { contactTasksApi } from '@/lib/contact-tasks-api'
import { contactTaskKeys, invalidateFor } from '@/lib/query-invalidation'
import type { ContactTaskListParams, CreateActionTaskRequest } from '@/types/contact-task'

// List tasks for a contact
export function useContactTasks(contactId: string, params: ContactTaskListParams = {}) {
  return useQuery({
    queryKey: contactTaskKeys.list(contactId, params),
    queryFn: () => contactTasksApi.listTasks(contactId, params),
    enabled: !!contactId,
    staleTime: 1000 * 60 * 2, // 2 minutes
  })
}

// Create action task mutation
export function useCreateActionTask() {
  const queryClient = useQueryClient()

  return useMutation({
    mutationFn: ({ contactId, data }: { contactId: string; data: CreateActionTaskRequest }) =>
      contactTasksApi.createTask(contactId, data),
    onSuccess: () => {
      invalidateFor('task:created')
    },
  })
}

// Delete task link mutation
export function useDeleteTaskLink() {
  const queryClient = useQueryClient()

  return useMutation({
    mutationFn: ({ contactId, taskId }: { contactId: string; taskId: string }) =>
      contactTasksApi.deleteTaskLink(contactId, taskId),
    onSuccess: () => {
      invalidateFor('task:deleted')
    },
  })
}
