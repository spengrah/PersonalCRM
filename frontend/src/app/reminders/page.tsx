'use client'

import { useState } from 'react'
import Link from 'next/link'
import { Plus, Search, CheckCircle, AlertCircle, User, Calendar, MoreHorizontal } from 'lucide-react'
import { Navigation } from '@/components/layout/navigation'
import { Button } from '@/components/ui/button'
import { useReminders, useCompleteReminder, useDeleteReminder } from '@/hooks/use-reminders'
import type { DueReminder, ReminderListParams } from '@/types/reminder'
import { clsx } from 'clsx'

function RemindersTable({ reminders, loading }: { reminders: DueReminder[]; loading: boolean }) {
  const completeReminderMutation = useCompleteReminder()
  const deleteReminderMutation = useDeleteReminder()

  const handleComplete = async (reminderId: string) => {
    try {
      await completeReminderMutation.mutateAsync(reminderId)
    } catch (error) {
      console.error('Error completing reminder:', error)
    }
  }

  const handleDelete = async (reminderId: string) => {
    if (confirm('Are you sure you want to delete this reminder?')) {
      try {
        await deleteReminderMutation.mutateAsync(reminderId)
      } catch (error) {
        console.error('Error deleting reminder:', error)
      }
    }
  }

  if (loading) {
    return (
      <div className="animate-pulse space-y-4">
        {[...Array(5)].map((_, i) => (
          <div key={i} className="h-16 bg-gray-200 rounded"></div>
        ))}
      </div>
    )
  }

  if (reminders.length === 0) {
    return (
      <div className="text-center py-12">
        <div className="mx-auto h-12 w-12 text-gray-400">
          <CheckCircle className="w-12 h-12" />
        </div>
        <h3 className="mt-2 text-sm font-medium text-gray-900">No reminders</h3>
        <p className="mt-1 text-sm text-gray-500">You&apos;re all caught up! No reminders to show.</p>
      </div>
    )
  }

  return (
    <div className="overflow-hidden shadow ring-1 ring-black ring-opacity-5 md:rounded-lg">
      <table className="min-w-full divide-y divide-gray-300">
        <thead className="bg-gray-50">
          <tr>
            <th className="px-6 py-3 text-left text-xs font-medium text-gray-500 uppercase tracking-wider">
              Reminder
            </th>
            <th className="px-6 py-3 text-left text-xs font-medium text-gray-500 uppercase tracking-wider">
              Contact
            </th>
            <th className="px-6 py-3 text-left text-xs font-medium text-gray-500 uppercase tracking-wider">
              Due Date
            </th>
            <th className="px-6 py-3 text-left text-xs font-medium text-gray-500 uppercase tracking-wider">
              Status
            </th>
            <th className="relative px-6 py-3">
              <span className="sr-only">Actions</span>
            </th>
          </tr>
        </thead>
        <tbody className="bg-white divide-y divide-gray-200">
          {reminders.map((reminder) => {
            const isOverdue = new Date(reminder.due_date) < new Date() && !reminder.completed
            
            return (
              <tr key={reminder.id} className="hover:bg-gray-50">
                <td className="px-6 py-4">
                  <div className="flex items-start space-x-3">
                    <div className={clsx(
                      'w-2 h-2 rounded-full mt-2 flex-shrink-0',
                      reminder.completed
                        ? 'bg-green-500'
                        : isOverdue
                        ? 'bg-red-500'
                        : 'bg-yellow-500'
                    )} />
                    <div>
                      <div className="text-sm font-medium text-gray-900">
                        {reminder.title}
                      </div>
                      {reminder.description && (
                        <div className="text-sm text-gray-500 mt-1">
                          {reminder.description}
                        </div>
                      )}
                    </div>
                  </div>
                </td>
                <td className="px-6 py-4 whitespace-nowrap">
                  <div className="flex items-center">
                    <User className="w-4 h-4 mr-2 text-gray-400" />
                    <Link
                      href={`/contacts/${reminder.contact_id}`}
                      className="text-sm text-blue-600 hover:text-blue-500"
                    >
                      {reminder.contact_name}
                    </Link>
                  </div>
                  {reminder.contact_email && (
                    <div className="text-sm text-gray-500">
                      {reminder.contact_email}
                    </div>
                  )}
                </td>
                <td className="px-6 py-4 whitespace-nowrap">
                  <div className="flex items-center text-sm text-gray-900">
                    <Calendar className="w-4 h-4 mr-2 text-gray-400" />
                    {new Date(reminder.due_date).toLocaleDateString()}
                  </div>
                  {isOverdue && (
                    <div className="flex items-center text-red-600 text-sm mt-1">
                      <AlertCircle className="w-4 h-4 mr-1" />
                      Overdue
                    </div>
                  )}
                </td>
                <td className="px-6 py-4 whitespace-nowrap">
                  <span className={clsx(
                    'inline-flex px-2 py-1 text-xs font-semibold rounded-full',
                    reminder.completed
                      ? 'bg-green-100 text-green-800'
                      : isOverdue
                      ? 'bg-red-100 text-red-800'
                      : 'bg-yellow-100 text-yellow-800'
                  )}>
                    {reminder.completed ? 'Completed' : isOverdue ? 'Overdue' : 'Due'}
                  </span>
                </td>
                <td className="px-6 py-4 whitespace-nowrap text-right text-sm font-medium">
                  <div className="flex items-center space-x-2">
                    {!reminder.completed && (
                      <Button
                        size="sm"
                        variant="outline"
                        onClick={() => handleComplete(reminder.id)}
                        loading={completeReminderMutation.isPending}
                      >
                        <CheckCircle className="w-4 h-4 mr-1" />
                        Complete
                      </Button>
                    )}
                    <button
                      onClick={() => handleDelete(reminder.id)}
                      disabled={deleteReminderMutation.isPending}
                      className="text-gray-400 hover:text-gray-500"
                    >
                      <MoreHorizontal className="w-5 h-5" />
                    </button>
                  </div>
                </td>
              </tr>
            )
          })}
        </tbody>
      </table>
    </div>
  )
}

export default function RemindersPage() {
  const [searchTerm, setSearchTerm] = useState('')
  const [filter, setFilter] = useState<'all' | 'due' | 'completed'>('all')
  const params: ReminderListParams = {
    page: 1,
    limit: 50,
  }

  const { data: reminders, isLoading, error } = useReminders({
    ...params,
    ...(filter === 'due' && { due_today: true }),
  })

  const filteredReminders = reminders?.filter(reminder => {
    const matchesSearch = !searchTerm || 
      reminder.title.toLowerCase().includes(searchTerm.toLowerCase()) ||
      reminder.contact_name.toLowerCase().includes(searchTerm.toLowerCase())
    
    const matchesFilter = filter === 'all' || 
      (filter === 'completed' && reminder.completed) ||
      (filter === 'due' && !reminder.completed)
    
    return matchesSearch && matchesFilter
  }) || []

  const handleSearch = (e: React.FormEvent) => {
    e.preventDefault()
    // Search is handled by the filteredReminders logic above
  }

  return (
    <div className="min-h-screen bg-gray-50">
      <Navigation />
      
      <div className="max-w-7xl mx-auto py-6 sm:px-6 lg:px-8">
        {/* Header */}
        <div className="md:flex md:items-center md:justify-between mb-6">
          <div className="flex-1 min-w-0">
            <h2 className="text-2xl font-bold leading-7 text-gray-900 sm:text-3xl sm:truncate">
              Reminders
            </h2>
            <p className="mt-1 text-sm text-gray-500">
              {reminders?.length ? `${reminders.length} reminders` : 'Loading reminders...'}
            </p>
          </div>
          <div className="mt-4 flex md:mt-0 md:ml-4">
            <Link href="/contacts">
              <Button>
                <Plus className="w-4 h-4 mr-2" />
                Add Contact
              </Button>
            </Link>
          </div>
        </div>

        {/* Filters and Search */}
        <div className="mb-6 space-y-4">
          {/* Search */}
          <form onSubmit={handleSearch} className="max-w-md">
            <div className="relative">
              <div className="absolute inset-y-0 left-0 pl-3 flex items-center pointer-events-none">
                <Search className="h-5 w-5 text-gray-400" />
              </div>
              <input
                type="text"
                placeholder="Search reminders..."
                value={searchTerm}
                onChange={(e) => setSearchTerm(e.target.value)}
                className="block w-full pl-10 pr-3 py-2 border border-gray-300 rounded-md leading-5 bg-white placeholder-gray-500 focus:outline-none focus:placeholder-gray-400 focus:ring-1 focus:ring-blue-500 focus:border-blue-500"
              />
            </div>
          </form>

          {/* Filter Tabs */}
          <div className="flex space-x-4">
            <button
              onClick={() => setFilter('all')}
              className={clsx(
                'px-3 py-2 text-sm font-medium rounded-md',
                filter === 'all'
                  ? 'bg-blue-100 text-blue-700'
                  : 'text-gray-500 hover:text-gray-700'
              )}
            >
              All
            </button>
            <button
              onClick={() => setFilter('due')}
              className={clsx(
                'px-3 py-2 text-sm font-medium rounded-md',
                filter === 'due'
                  ? 'bg-yellow-100 text-yellow-700'
                  : 'text-gray-500 hover:text-gray-700'
              )}
            >
              Due
            </button>
            <button
              onClick={() => setFilter('completed')}
              className={clsx(
                'px-3 py-2 text-sm font-medium rounded-md',
                filter === 'completed'
                  ? 'bg-green-100 text-green-700'
                  : 'text-gray-500 hover:text-gray-700'
              )}
            >
              Completed
            </button>
          </div>
        </div>

        {/* Error state */}
        {error && (
          <div className="bg-red-50 border border-red-200 rounded-md p-4 mb-6">
            <div className="flex">
              <div className="flex-shrink-0">
                <AlertCircle className="h-5 w-5 text-red-400" />
              </div>
              <div className="ml-3">
                <h3 className="text-sm font-medium text-red-800">
                  Error loading reminders
                </h3>
                <p className="mt-1 text-sm text-red-700">
                  {error instanceof Error ? error.message : 'An unexpected error occurred'}
                </p>
              </div>
            </div>
          </div>
        )}

        {/* Reminders Table */}
        <div className="bg-white shadow overflow-hidden sm:rounded-md">
          <RemindersTable reminders={filteredReminders} loading={isLoading} />
        </div>
      </div>
    </div>
  )
}
