import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { remindersApi } from '@/lib/reminders-api'
import type { CreateReminderRequest, ReminderListParams } from '@/types/reminder'

// Query keys
export const reminderKeys = {
  all: ['reminders'] as const,
  lists: () => [...reminderKeys.all, 'list'] as const,
  list: (params: ReminderListParams) => [...reminderKeys.lists(), params] as const,
  stats: () => [...reminderKeys.all, 'stats'] as const,
  byContact: (contactId: string) => [...reminderKeys.all, 'contact', contactId] as const,
}

// Get reminders list
export function useReminders(params: ReminderListParams = {}) {
  return useQuery({
    queryKey: reminderKeys.list(params),
    queryFn: () => remindersApi.getReminders(params),
    staleTime: 1000 * 60 * 1, // 1 minute for reminders (they change more frequently)
  })
}

// Get today's reminders
export function useTodayReminders() {
  return useQuery({
    queryKey: reminderKeys.list({ due_today: true }),
    queryFn: () => remindersApi.getReminders({ due_today: true }),
    staleTime: 1000 * 30, // 30 seconds for today's reminders
    refetchInterval: 1000 * 60, // Refetch every minute
  })
}

// Get reminders for a specific contact
export function useRemindersByContact(contactId: string) {
  return useQuery({
    queryKey: reminderKeys.byContact(contactId),
    queryFn: () => remindersApi.getRemindersByContact(contactId),
    enabled: !!contactId,
  })
}

// Get reminder statistics
export function useReminderStats() {
  return useQuery({
    queryKey: reminderKeys.stats(),
    queryFn: () => remindersApi.getStats(),
    staleTime: 1000 * 60 * 2, // 2 minutes
    refetchInterval: 1000 * 60 * 5, // Refetch every 5 minutes
  })
}

// Create reminder mutation
export function useCreateReminder() {
  const queryClient = useQueryClient()

  return useMutation({
    mutationFn: (data: CreateReminderRequest) => remindersApi.createReminder(data),
    onSuccess: () => {
      // Invalidate and refetch reminders
      queryClient.invalidateQueries({ queryKey: reminderKeys.all })
    },
  })
}

// Complete reminder mutation
export function useCompleteReminder() {
  const queryClient = useQueryClient()

  return useMutation({
    mutationFn: (id: string) => remindersApi.completeReminder(id),
    onSuccess: () => {
      // Invalidate and refetch reminders
      queryClient.invalidateQueries({ queryKey: reminderKeys.all })
    },
  })
}

// Delete reminder mutation
export function useDeleteReminder() {
  const queryClient = useQueryClient()

  return useMutation({
    mutationFn: (id: string) => remindersApi.deleteReminder(id),
    onSuccess: () => {
      // Invalidate and refetch reminders
      queryClient.invalidateQueries({ queryKey: reminderKeys.all })
    },
  })
}
