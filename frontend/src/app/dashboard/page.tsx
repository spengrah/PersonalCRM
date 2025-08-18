'use client'

import { useState } from 'react'
import Link from 'next/link'
import { CheckCircle, Clock, AlertCircle, User, Calendar, Bell, Plus } from 'lucide-react'
import { Navigation } from '@/components/layout/navigation'
import { Button } from '@/components/ui/button'
import { useTodayReminders, useReminderStats, useCompleteReminder, useDeleteReminder } from '@/hooks/use-reminders'
import type { DueReminder } from '@/types/reminder'
import { clsx } from 'clsx'

function ReminderCard({ reminder }: { reminder: DueReminder }) {
  const completeReminderMutation = useCompleteReminder()
  const deleteReminderMutation = useDeleteReminder()

  const handleComplete = async () => {
    try {
      await completeReminderMutation.mutateAsync(reminder.id)
    } catch (error) {
      console.error('Error completing reminder:', error)
    }
  }

  const handleDelete = async () => {
    if (confirm('Are you sure you want to delete this reminder?')) {
      try {
        await deleteReminderMutation.mutateAsync(reminder.id)
      } catch (error) {
        console.error('Error deleting reminder:', error)
      }
    }
  }

  const isOverdue = new Date(reminder.due_date) < new Date()

  return (
    <div className={clsx(
      'bg-white rounded-lg shadow-sm border p-4 hover:shadow-md transition-shadow',
      isOverdue && 'border-red-200 bg-red-50'
    )}>
      <div className="flex items-start justify-between">
        <div className="flex-1">
          <div className="flex items-center space-x-2 mb-2">
            <div className={clsx(
              'w-2 h-2 rounded-full',
              isOverdue ? 'bg-red-500' : 'bg-yellow-500'
            )} />
            <h3 className="text-sm font-medium text-gray-900">
              {reminder.title}
            </h3>
          </div>
          
          <div className="flex items-center space-x-4 text-sm text-gray-500 mb-2">
            <div className="flex items-center space-x-1">
              <User className="w-4 h-4" />
              <Link 
                href={`/contacts/${reminder.contact_id}`}
                className="hover:text-blue-600 underline"
              >
                {reminder.contact_name}
              </Link>
            </div>
            <div className="flex items-center space-x-1">
              <Calendar className="w-4 h-4" />
              <span>
                {new Date(reminder.due_date).toLocaleDateString()}
              </span>
            </div>
          </div>

          {reminder.description && (
            <p className="text-sm text-gray-600 mb-3">
              {reminder.description}
            </p>
          )}
        </div>

        <div className="flex items-center space-x-2 ml-4">
          <Button
            size="sm"
            variant="outline"
            onClick={handleComplete}
            loading={completeReminderMutation.isPending}
            disabled={reminder.completed}
          >
            <CheckCircle className="w-4 h-4 mr-1" />
            {reminder.completed ? 'Done' : 'Complete'}
          </Button>
          
          <Button
            size="sm"
            variant="ghost"
            onClick={handleDelete}
            loading={deleteReminderMutation.isPending}
            disabled={completeReminderMutation.isPending}
          >
            ×
          </Button>
        </div>
      </div>
      
      {isOverdue && (
        <div className="mt-2 flex items-center text-red-600 text-sm">
          <AlertCircle className="w-4 h-4 mr-1" />
          Overdue
        </div>
      )}
    </div>
  )
}

function StatsCard({ 
  title, 
  value, 
  icon: Icon, 
  color = 'blue',
  href
}: { 
  title: string
  value: number
  icon: React.ElementType
  color?: 'blue' | 'green' | 'red' | 'yellow'
  href?: string
}) {
  const colorClasses = {
    blue: 'bg-blue-50 text-blue-600 border-blue-200',
    green: 'bg-green-50 text-green-600 border-green-200',
    red: 'bg-red-50 text-red-600 border-red-200',
    yellow: 'bg-yellow-50 text-yellow-600 border-yellow-200',
  }

  const content = (
    <div className={clsx(
      "bg-white rounded-lg shadow-sm border p-4",
      href && "hover:shadow-md transition-shadow cursor-pointer"
    )}>
      <div className="flex items-center justify-between">
        <div>
          <p className="text-sm font-medium text-gray-600">{title}</p>
          <p className="text-2xl font-bold text-gray-900">{value}</p>
        </div>
        <div className={clsx(
          'rounded-full p-3 border',
          colorClasses[color]
        )}>
          <Icon className="w-6 h-6" />
        </div>
      </div>
    </div>
  )

  if (href) {
    return <Link href={href}>{content}</Link>
  }

  return content
}

export default function DashboardPage() {
  const { data: todayReminders, isLoading: loadingReminders, error: remindersError } = useTodayReminders()
  const { data: stats, isLoading: loadingStats } = useReminderStats()
  
  const [filter, setFilter] = useState<'all' | 'overdue'>('all')

  const filteredReminders = todayReminders?.filter(reminder => {
    if (filter === 'overdue') {
      return new Date(reminder.due_date) < new Date()
    }
    return true
  }) || []

  const overdueCount = todayReminders?.filter(r => new Date(r.due_date) < new Date()).length || 0

  return (
    <div className="min-h-screen bg-gray-50">
      <Navigation />
      
      <div className="max-w-7xl mx-auto py-6 sm:px-6 lg:px-8">
        {/* Header */}
        <div className="md:flex md:items-center md:justify-between mb-6">
          <div className="flex-1 min-w-0">
            <h2 className="text-2xl font-bold leading-7 text-gray-900 sm:text-3xl sm:truncate">
              Dashboard
            </h2>
            <p className="mt-1 text-sm text-gray-500">
              Your personal CRM overview and reminders
            </p>
          </div>
          <div className="mt-4 flex space-x-3 md:mt-0 md:ml-4">
            <Link href="/contacts/new">
              <Button variant="outline">
                <Plus className="w-4 h-4 mr-2" />
                Add Contact
              </Button>
            </Link>
            <Link href="/contacts">
              <Button>
                View All Contacts
              </Button>
            </Link>
          </div>
        </div>

        {/* Stats Cards */}
        {!loadingStats && stats && (
          <div className="grid grid-cols-1 md:grid-cols-3 gap-6 mb-8">
            <StatsCard
              title="Total Reminders"
              value={stats.total_reminders}
              icon={Bell}
              color="blue"
              href="/reminders"
            />
            <StatsCard
              title="Due Today"
              value={stats.due_today}
              icon={Clock}
              color="yellow"
              href="/reminders"
            />
            <StatsCard
              title="Overdue"
              value={stats.overdue}
              icon={AlertCircle}
              color="red"
              href="/reminders"
            />
          </div>
        )}

        {/* Filters */}
        <div className="mb-6">
          <div className="flex items-center space-x-4">
            <h3 className="text-lg font-medium text-gray-900">Today&apos;s Reminders</h3>
            <div className="flex space-x-2">
              <button
                onClick={() => setFilter('all')}
                className={clsx(
                  'px-3 py-1 text-sm font-medium rounded-md',
                  filter === 'all'
                    ? 'bg-blue-100 text-blue-700'
                    : 'text-gray-500 hover:text-gray-700'
                )}
              >
                All ({todayReminders?.length || 0})
              </button>
              <button
                onClick={() => setFilter('overdue')}
                className={clsx(
                  'px-3 py-1 text-sm font-medium rounded-md',
                  filter === 'overdue'
                    ? 'bg-red-100 text-red-700'
                    : 'text-gray-500 hover:text-gray-700'
                )}
              >
                Overdue ({overdueCount})
              </button>
            </div>
          </div>
        </div>

        {/* Reminders List */}
        <div className="space-y-4">
          {loadingReminders && (
            <div className="space-y-4">
              {[...Array(3)].map((_, i) => (
                <div key={i} className="bg-white rounded-lg shadow-sm border p-4 animate-pulse">
                  <div className="h-4 bg-gray-200 rounded w-3/4 mb-2"></div>
                  <div className="h-3 bg-gray-200 rounded w-1/2 mb-2"></div>
                  <div className="h-3 bg-gray-200 rounded w-2/3"></div>
                </div>
              ))}
            </div>
          )}

          {remindersError && (
            <div className="bg-red-50 border border-red-200 rounded-md p-4">
              <div className="flex">
                <div className="flex-shrink-0">
                  <AlertCircle className="h-5 w-5 text-red-400" />
                </div>
                <div className="ml-3">
                  <h3 className="text-sm font-medium text-red-800">
                    Error loading reminders
                  </h3>
                  <p className="mt-1 text-sm text-red-700">
                    {remindersError instanceof Error ? remindersError.message : 'An unexpected error occurred'}
                  </p>
                </div>
              </div>
            </div>
          )}

          {!loadingReminders && !remindersError && filteredReminders.length === 0 && (
            <div className="text-center py-12 bg-white rounded-lg shadow-sm border">
              <CheckCircle className="mx-auto h-12 w-12 text-green-500 mb-4" />
              <h3 className="text-lg font-medium text-gray-900 mb-2">
                {filter === 'overdue' ? 'No overdue reminders' : 'All caught up!'}
              </h3>
              <p className="text-gray-500 mb-6">
                {filter === 'overdue' 
                  ? 'You have no overdue reminders right now.'
                  : 'You don&apos;t have any reminders due today. Great job staying on top of your contacts!'
                }
              </p>
              <Link href="/contacts">
                <Button variant="outline">
                  <User className="w-4 h-4 mr-2" />
                  View Contacts
                </Button>
              </Link>
            </div>
          )}

          {!loadingReminders && !remindersError && filteredReminders.length > 0 && (
            <>
              {filteredReminders.map((reminder) => (
                <ReminderCard key={reminder.id} reminder={reminder} />
              ))}
            </>
          )}
        </div>
      </div>
    </div>
  )
}

