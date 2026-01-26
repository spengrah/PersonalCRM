'use client'

import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import { todoistApi, TodoistSettingsUpdateRequest } from '@/lib/todoist-api'

/**
 * Query key for Todoist settings
 */
export const todoistSettingsQueryKey = ['todoist-settings'] as const
export const todoistProjectsQueryKey = ['todoist-projects'] as const
export const todoistLabelsQueryKey = ['todoist-labels'] as const

/**
 * Hook to fetch Todoist integration settings
 */
export function useTodoistSettings(enabled: boolean = true) {
  return useQuery({
    queryKey: todoistSettingsQueryKey,
    queryFn: () => todoistApi.getSettings(),
    enabled,
  })
}

/**
 * Hook to update Todoist integration settings
 */
export function useUpdateTodoistSettings() {
  const queryClient = useQueryClient()

  return useMutation({
    mutationFn: (settings: TodoistSettingsUpdateRequest) => todoistApi.updateSettings(settings),
    onSuccess: () => {
      // Invalidate settings to refetch
      queryClient.invalidateQueries({ queryKey: todoistSettingsQueryKey })
    },
  })
}

/**
 * Hook to fetch Todoist projects
 */
export function useTodoistProjects(enabled: boolean = true) {
  return useQuery({
    queryKey: todoistProjectsQueryKey,
    queryFn: () => todoistApi.listProjects(),
    enabled,
  })
}

/**
 * Hook to fetch Todoist labels
 */
export function useTodoistLabels(enabled: boolean = true) {
  return useQuery({
    queryKey: todoistLabelsQueryKey,
    queryFn: () => todoistApi.listLabels(),
    enabled,
  })
}
