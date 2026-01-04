import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import {
  createTimeEntry,
  getTimeEntry,
  listTimeEntries,
  getRunningTimeEntry,
  updateTimeEntry,
  deleteTimeEntry,
  getTimeEntryStats,
} from '@/lib/time-entries-api'
import { timeEntryKeys, invalidateFor } from '@/lib/query-invalidation'
import type {
  CreateTimeEntryRequest,
  UpdateTimeEntryRequest,
  ListTimeEntriesParams,
} from '@/types/time-entry'

// Re-export timeEntryKeys for backward compatibility
export { timeEntryKeys }

// Get time entries list
export function useTimeEntries(params?: ListTimeEntriesParams) {
  return useQuery({
    queryKey: timeEntryKeys.list(params || {}),
    queryFn: () => listTimeEntries(params),
    staleTime: 1000 * 60 * 1, // 1 minute
  })
}

// Get single time entry
export function useTimeEntry(id: string) {
  return useQuery({
    queryKey: timeEntryKeys.detail(id),
    queryFn: () => getTimeEntry(id),
    enabled: !!id,
  })
}

// Get running time entry
export function useRunningTimeEntry() {
  return useQuery({
    queryKey: timeEntryKeys.running(),
    queryFn: getRunningTimeEntry,
    refetchInterval: false, // Don't auto-refetch - timer updates are handled client-side
    staleTime: Infinity, // Don't refetch automatically
  })
}

// Get time entry statistics
export function useTimeEntryStats() {
  return useQuery({
    queryKey: timeEntryKeys.stats(),
    queryFn: getTimeEntryStats,
    staleTime: 1000 * 60 * 5, // 5 minutes
  })
}

// Create time entry mutation
export function useCreateTimeEntry() {
  return useMutation({
    mutationFn: (data: CreateTimeEntryRequest) => createTimeEntry(data),
    onSuccess: () => {
      invalidateFor('time-entry:created')
    },
  })
}

// Update time entry mutation
export function useUpdateTimeEntry() {
  const queryClient = useQueryClient()

  return useMutation({
    mutationFn: ({ id, data }: { id: string; data: UpdateTimeEntryRequest }) =>
      updateTimeEntry(id, data),
    onSuccess: updatedEntry => {
      queryClient.setQueryData(timeEntryKeys.detail(updatedEntry.id), updatedEntry)
      invalidateFor('time-entry:updated')
    },
  })
}

// Delete time entry mutation
export function useDeleteTimeEntry() {
  return useMutation({
    mutationFn: (id: string) => deleteTimeEntry(id),
    onSuccess: () => {
      invalidateFor('time-entry:deleted')
    },
  })
}
