import { useMutation, useQuery } from '@tanstack/react-query'
import { remindersApi } from '@/lib/reminders-api'
import { reminderKeys, invalidateFor } from '@/lib/query-invalidation'
import type { CreateReminderRequest, ReminderListParams } from '@/types/reminder'

// Re-export reminderKeys for backward compatibility
export { reminderKeys }

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
    staleTime: 1000 * 60 * 1, // 1 minute for today's reminders
    refetchInterval: 1000 * 60 * 2, // Refetch every 2 minutes
    refetchOnWindowFocus: true,
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
    refetchInterval: 1000 * 60 * 2, // Refetch every 2 minutes
    refetchOnWindowFocus: true,
  })
}

// Create reminder mutation
export function useCreateReminder() {
  return useMutation({
    mutationFn: (data: CreateReminderRequest) => remindersApi.createReminder(data),
    onSuccess: () => {
      invalidateFor('reminder:created')
    },
  })
}

// Complete reminder mutation
export function useCompleteReminder() {
  return useMutation({
    mutationFn: (id: string) => {
      console.log('🔄 useCompleteReminder: mutationFn called with id:', id)
      return remindersApi.completeReminder(id)
    },
    onSuccess: () => {
      console.log('✅ useCompleteReminder: onSuccess called')
      invalidateFor('reminder:completed')
    },
    onError: error => {
      console.error('❌ useCompleteReminder: onError:', error)
    },
  })
}

// Delete reminder mutation
export function useDeleteReminder() {
  return useMutation({
    mutationFn: (id: string) => remindersApi.deleteReminder(id),
    onSuccess: () => {
      invalidateFor('reminder:deleted')
    },
  })
}
