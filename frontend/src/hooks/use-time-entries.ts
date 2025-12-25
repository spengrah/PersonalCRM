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
import type {
  CreateTimeEntryRequest,
  UpdateTimeEntryRequest,
  ListTimeEntriesParams,
} from '@/types/time-entry'

// Query keys
export const timeEntryKeys = {
  all: ['time-entries'] as const,
  lists: () => [...timeEntryKeys.all, 'list'] as const,
  list: (params?: ListTimeEntriesParams) => [...timeEntryKeys.lists(), params] as const,
  details: () => [...timeEntryKeys.all, 'detail'] as const,
  detail: (id: string) => [...timeEntryKeys.details(), id] as const,
  running: () => [...timeEntryKeys.all, 'running'] as const,
  stats: () => [...timeEntryKeys.all, 'stats'] as const,
}

// Get time entries list
export function useTimeEntries(params?: ListTimeEntriesParams) {
  return useQuery({
    queryKey: timeEntryKeys.list(params),
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
  const queryClient = useQueryClient()

  return useMutation({
    mutationFn: (data: CreateTimeEntryRequest) => createTimeEntry(data),
    onSuccess: () => {
      // Invalidate and refetch time entries list
      queryClient.invalidateQueries({ queryKey: timeEntryKeys.lists() })
      queryClient.invalidateQueries({ queryKey: timeEntryKeys.running() })
      queryClient.invalidateQueries({ queryKey: timeEntryKeys.stats() })
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
      // Update the time entry in cache
      queryClient.setQueryData(timeEntryKeys.detail(updatedEntry.id), updatedEntry)
      // Invalidate lists to refresh
      queryClient.invalidateQueries({ queryKey: timeEntryKeys.lists() })
      queryClient.invalidateQueries({ queryKey: timeEntryKeys.running() })
      queryClient.invalidateQueries({ queryKey: timeEntryKeys.stats() })
    },
  })
}

// Delete time entry mutation
export function useDeleteTimeEntry() {
  const queryClient = useQueryClient()

  return useMutation({
    mutationFn: (id: string) => deleteTimeEntry(id),
    onSuccess: () => {
      // Invalidate and refetch time entries list
      queryClient.invalidateQueries({ queryKey: timeEntryKeys.lists() })
      queryClient.invalidateQueries({ queryKey: timeEntryKeys.stats() })
    },
  })
}
