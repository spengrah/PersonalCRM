'use client'

import { useState } from 'react'
import Link from 'next/link'
import { CheckCircle, Clock, AlertCircle, User, Calendar, Plus } from 'lucide-react'
import { Navigation } from '@/components/layout/navigation'
import { Button } from '@/components/ui/button'
import { ContactMethodIcon } from '@/components/contacts/contact-method-icon'
import { useOverdueContacts, useUpdateLastContacted } from '@/hooks/use-contacts'
import { useAcceleratedTime } from '@/hooks/use-accelerated-time'
import {
  formatContactMethodValue,
  getContactMethodHref,
  getContactMethodLabel,
  getPrimaryAndSecondaryMethods,
} from '@/lib/contact-methods'
import type { ContactMethod, OverdueContact } from '@/types/contact'
import { clsx } from 'clsx'

function OverdueContactCard({ contact }: { contact: OverdueContact }) {
  const updateLastContactedMutation = useUpdateLastContacted()
  const { currentTime } = useAcceleratedTime()
  const { primary, secondary } = getPrimaryAndSecondaryMethods(
    contact.methods,
    contact.primary_method
  )
  const methods = [primary, secondary].filter((method): method is ContactMethod => Boolean(method))

  const handleMarkContacted = async () => {
    try {
      await updateLastContactedMutation.mutateAsync(contact.id)
    } catch (error) {
      console.error('Error marking as contacted:', error)
    }
  }

  const getUrgencyColor = (daysOverdue: number) => {
    if (daysOverdue <= 2) return 'border-yellow-200 bg-yellow-50'
    if (daysOverdue <= 7) return 'border-orange-200 bg-orange-50'
    return 'border-red-200 bg-red-50'
  }

  const getUrgencyIndicator = (daysOverdue: number) => {
    if (daysOverdue <= 2) return 'bg-yellow-500'
    if (daysOverdue <= 7) return 'bg-orange-500'
    return 'bg-red-500'
  }

  const formatLastContacted = (lastContacted?: string) => {
    if (!lastContacted) return 'Never contacted'
    const date = new Date(lastContacted)
    const now = currentTime // Use accelerated time instead of new Date()
    const diffTime = Math.abs(now.getTime() - date.getTime())
    const diffDays = Math.ceil(diffTime / (1000 * 60 * 60 * 24))

    if (diffDays === 1) return 'Yesterday'
    if (diffDays <= 7) return `${diffDays} days ago`
    if (diffDays <= 30) return `${Math.floor(diffDays / 7)} weeks ago`
    return `${Math.floor(diffDays / 30)} months ago`
  }

  return (
    <div
      className={clsx(
        'bg-white rounded-lg shadow-sm border p-6 hover:shadow-md transition-shadow',
        getUrgencyColor(contact.days_overdue)
      )}
    >
      <div className="flex items-start justify-between">
        <div className="flex-1">
          <div className="flex items-center space-x-3 mb-3">
            <div
              className={clsx('w-3 h-3 rounded-full', getUrgencyIndicator(contact.days_overdue))}
            />
            <h3 className="text-lg font-semibold text-gray-900">{contact.full_name}</h3>
            <span className="text-sm font-medium text-gray-500">({contact.cadence} cadence)</span>
          </div>

          <div className="grid grid-cols-1 md:grid-cols-2 gap-3 mb-4">
            <div className="flex items-center space-x-2 text-sm text-gray-600">
              <Clock className="w-4 h-4" />
              <span>
                <strong>{contact.days_overdue} days overdue</strong> - Last contacted{' '}
                {formatLastContacted(contact.last_contacted)}
              </span>
            </div>

            {methods.map((method, index) => {
              const value = formatContactMethodValue(method.type, method.value)
              const href = getContactMethodHref(method.type, method.value)
              const label = getContactMethodLabel(method.type)
              const key = method.id ?? `${method.type}-${method.value}`
              const valueClassName = index === 0 ? 'font-medium text-gray-700' : 'text-gray-600'

              return (
                <div key={key} className={`flex items-center space-x-2 text-sm ${valueClassName}`}>
                  <ContactMethodIcon type={method.type} />
                  {href ? (
                    <a href={href} className="hover:text-blue-600 underline">
                      {value}
                    </a>
                  ) : (
                    <span>{value}</span>
                  )}
                  <span className="text-xs text-gray-500">{label}</span>
                </div>
              )
            })}

            <div className="flex items-center space-x-2 text-sm text-gray-600">
              <User className="w-4 h-4" />
              <Link href={`/contacts/${contact.id}`} className="hover:text-blue-600 underline">
                View details
              </Link>
            </div>
          </div>

          <div className="bg-blue-50 border border-blue-200 rounded-md p-3 mb-4">
            <p className="text-sm font-medium text-blue-800">💡 {contact.suggested_action}</p>
          </div>
        </div>

        <div className="flex items-center space-x-3 ml-6">
          <Button
            size="sm"
            onClick={handleMarkContacted}
            loading={updateLastContactedMutation.isPending}
            className="whitespace-nowrap"
          >
            <CheckCircle className="w-4 h-4 mr-2" />
            Mark as Contacted
          </Button>
        </div>
      </div>
    </div>
  )
}

function EmptyState() {
  return (
    <div className="text-center py-16 bg-white rounded-lg shadow-sm border">
      <CheckCircle className="mx-auto h-16 w-16 text-green-500 mb-6" />
      <h3 className="text-xl font-semibold text-gray-900 mb-3">All caught up! 🎉</h3>
      <p className="text-gray-600 mb-8 max-w-md mx-auto">
        You don&apos;t have any overdue contacts right now. You&apos;re doing a great job staying
        connected with your network!
      </p>
      <div className="flex items-center justify-center space-x-4">
        <Link href="/contacts">
          <Button variant="outline">
            <User className="w-4 h-4 mr-2" />
            View All Contacts
          </Button>
        </Link>
        <Link href="/contacts/new">
          <Button>
            <Plus className="w-4 h-4 mr-2" />
            Add New Contact
          </Button>
        </Link>
      </div>
    </div>
  )
}

export default function DashboardPage() {
  const { data: overdueContacts, isLoading, error } = useOverdueContacts()

  const [sortBy, setSortBy] = useState<'urgency' | 'name' | 'lastContacted'>('urgency')

  const sortedContacts =
    overdueContacts?.slice().sort((a, b) => {
      switch (sortBy) {
        case 'urgency':
          return b.days_overdue - a.days_overdue
        case 'name':
          return a.full_name.localeCompare(b.full_name)
        case 'lastContacted':
          if (!a.last_contacted && !b.last_contacted) return 0
          if (!a.last_contacted) return 1
          if (!b.last_contacted) return -1
          return new Date(a.last_contacted).getTime() - new Date(b.last_contacted).getTime()
        default:
          return 0
      }
    }) || []

  return (
    <div className="min-h-screen bg-gray-50">
      <Navigation />

      <div className="max-w-7xl mx-auto py-6 sm:px-6 lg:px-8">
        {/* Header */}
        <div className="md:flex md:items-center md:justify-between mb-8">
          <div className="flex-1 min-w-0">
            <h2 className="text-3xl font-bold leading-7 text-gray-900 sm:text-4xl">
              Action Required
            </h2>
            <p className="mt-2 text-lg text-gray-600">
              {overdueContacts?.length === 0
                ? "You're all caught up! No contacts need attention right now."
                : `${overdueContacts?.length || 0} contacts need your attention`}
            </p>
          </div>
          <div className="mt-6 flex space-x-3 md:mt-0 md:ml-4">
            <Link href="/reminders">
              <Button variant="outline">
                <Calendar className="w-4 h-4 mr-2" />
                View Reminders
              </Button>
            </Link>
            <Link href="/contacts/new">
              <Button>
                <Plus className="w-4 h-4 mr-2" />
                Add Contact
              </Button>
            </Link>
          </div>
        </div>

        {/* Sort Controls */}
        {overdueContacts && overdueContacts.length > 0 && (
          <div className="mb-6 flex items-center space-x-4">
            <span className="text-sm font-medium text-gray-700">Sort by:</span>
            <div className="flex space-x-2">
              <button
                onClick={() => setSortBy('urgency')}
                className={clsx(
                  'px-3 py-1 text-sm font-medium rounded-md',
                  sortBy === 'urgency'
                    ? 'bg-blue-100 text-blue-700'
                    : 'text-gray-500 hover:text-gray-700'
                )}
              >
                Most Urgent
              </button>
              <button
                onClick={() => setSortBy('name')}
                className={clsx(
                  'px-3 py-1 text-sm font-medium rounded-md',
                  sortBy === 'name'
                    ? 'bg-blue-100 text-blue-700'
                    : 'text-gray-500 hover:text-gray-700'
                )}
              >
                Name
              </button>
              <button
                onClick={() => setSortBy('lastContacted')}
                className={clsx(
                  'px-3 py-1 text-sm font-medium rounded-md',
                  sortBy === 'lastContacted'
                    ? 'bg-blue-100 text-blue-700'
                    : 'text-gray-500 hover:text-gray-700'
                )}
              >
                Last Contacted
              </button>
            </div>
          </div>
        )}

        {/* Contacts List */}
        <div className="space-y-6">
          {isLoading && (
            <div className="space-y-6">
              {[...Array(3)].map((_, i) => (
                <div key={i} className="bg-white rounded-lg shadow-sm border p-6 animate-pulse">
                  <div className="h-6 bg-gray-200 rounded w-1/3 mb-3"></div>
                  <div className="h-4 bg-gray-200 rounded w-2/3 mb-2"></div>
                  <div className="h-4 bg-gray-200 rounded w-1/2 mb-4"></div>
                  <div className="h-8 bg-gray-200 rounded w-24"></div>
                </div>
              ))}
            </div>
          )}

          {error && (
            <div className="bg-red-50 border border-red-200 rounded-md p-4">
              <div className="flex">
                <div className="flex-shrink-0">
                  <AlertCircle className="h-5 w-5 text-red-400" />
                </div>
                <div className="ml-3">
                  <h3 className="text-sm font-medium text-red-800">
                    Error loading overdue contacts
                  </h3>
                  <p className="mt-1 text-sm text-red-700">
                    {error instanceof Error ? error.message : 'An unexpected error occurred'}
                  </p>
                </div>
              </div>
            </div>
          )}

          {!isLoading && !error && overdueContacts?.length === 0 && <EmptyState />}

          {!isLoading && !error && sortedContacts.length > 0 && (
            <>
              {sortedContacts.map(contact => (
                <OverdueContactCard key={contact.id} contact={contact} />
              ))}
            </>
          )}
        </div>
      </div>
    </div>
  )
}
