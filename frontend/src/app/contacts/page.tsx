'use client'

import { useState, useEffect, useRef, useCallback } from 'react'
import Link from 'next/link'
import { useRouter } from 'next/navigation'
import {
  Plus,
  Search,
  MoreHorizontal,
  MapPin,
  CheckCircle,
  ArrowUpDown,
  ArrowUp,
  ArrowDown,
} from 'lucide-react'
import { useContacts, useUpdateLastContacted } from '@/hooks/use-contacts'
import { ContactMethodIcon } from '@/components/contacts/contact-method-icon'
import { Button } from '@/components/ui/button'
import { Navigation } from '@/components/layout/navigation'
import {
  formatContactMethodValue,
  getContactMethodHref,
  getContactMethodLabel,
  getPrimaryAndSecondaryMethods,
} from '@/lib/contact-methods'
import { FORM_CONTROL_WITH_ICON } from '@/lib/form-classes'
import { formatDateOnly } from '@/lib/utils'
import type { Contact, ContactListParams, ContactMethod } from '@/types/contact'

type SortField = 'name' | 'location' | 'birthday' | 'last_contacted'

function ContactsTable({
  contacts,
  loading,
  sortBy,
  sortOrder,
  onSort,
}: {
  contacts: Contact[]
  loading: boolean
  sortBy?: SortField
  sortOrder?: 'asc' | 'desc'
  onSort: (field: SortField) => void
}) {
  const router = useRouter()
  const [openDropdown, setOpenDropdown] = useState<string | null>(null)
  const [dropdownPosition, setDropdownPosition] = useState<'below' | 'above'>('below')
  const buttonRefs = useRef<Map<string, HTMLButtonElement>>(new Map())
  const updateLastContacted = useUpdateLastContacted()

  const setButtonRef = useCallback((id: string, el: HTMLButtonElement | null) => {
    if (el) {
      buttonRefs.current.set(id, el)
    } else {
      buttonRefs.current.delete(id)
    }
  }, [])

  const getSortIcon = (field: SortField) => {
    if (sortBy !== field) {
      return <ArrowUpDown className="w-4 h-4 ml-1 text-gray-400" />
    }
    return sortOrder === 'asc' ? (
      <ArrowUp className="w-4 h-4 ml-1 text-blue-600" />
    ) : (
      <ArrowDown className="w-4 h-4 ml-1 text-blue-600" />
    )
  }

  const handleRowClick = (contactId: string) => {
    router.push(`/contacts/${contactId}`)
  }

  const handleMarkAsContacted = async (e: React.MouseEvent, contactId: string) => {
    e.stopPropagation() // Prevent row click
    try {
      await updateLastContacted.mutateAsync(contactId)
      setOpenDropdown(null)
    } catch (error) {
      console.error('Failed to mark as contacted:', error)
    }
  }

  const handleDropdownClick = (e: React.MouseEvent) => {
    e.stopPropagation() // Prevent row click
  }

  // Close dropdown when clicking outside
  useEffect(() => {
    const handleClickOutside = () => {
      setOpenDropdown(null)
    }

    if (openDropdown) {
      document.addEventListener('click', handleClickOutside)
      return () => document.removeEventListener('click', handleClickOutside)
    }
  }, [openDropdown])

  if (loading) {
    return (
      <div className="animate-pulse space-y-4">
        {[...Array(5)].map((_, i) => (
          <div key={i} className="h-16 bg-gray-200 rounded"></div>
        ))}
      </div>
    )
  }

  if (contacts.length === 0) {
    return (
      <div className="text-center py-12">
        <div className="mx-auto h-12 w-12 text-gray-400">
          <svg fill="none" viewBox="0 0 24 24" stroke="currentColor">
            <path
              strokeLinecap="round"
              strokeLinejoin="round"
              strokeWidth={2}
              d="M17 20h5v-2a3 3 0 00-5.356-1.857M17 20H7m10 0v-2c0-.656-.126-1.283-.356-1.857M7 20H2v-2a3 3 0 015.356-1.857M7 20v-2c0-.656.126-1.283.356-1.857m0 0a5.002 5.002 0 019.288 0M15 7a3 3 0 11-6 0 3 3 0 016 0zm6 3a2 2 0 11-4 0 2 2 0 014 0zM7 10a2 2 0 11-4 0 2 2 0 014 0z"
            />
          </svg>
        </div>
        <h3 className="mt-2 text-sm font-medium text-gray-900">No contacts</h3>
        <p className="mt-1 text-sm text-gray-500">Get started by creating a new contact.</p>
        <div className="mt-6">
          <Link href="/contacts/new">
            <Button>
              <Plus className="w-4 h-4 mr-2" />
              New Contact
            </Button>
          </Link>
        </div>
      </div>
    )
  }

  return (
    <div className="shadow ring-1 ring-black ring-opacity-5 md:rounded-lg">
      <table className="min-w-full divide-y divide-gray-300">
        <thead className="bg-gray-50">
          <tr>
            <th
              className="px-6 py-3 text-left text-xs font-medium text-gray-500 uppercase tracking-wider cursor-pointer hover:bg-gray-100"
              onClick={() => onSort('name')}
            >
              <div className="flex items-center">
                Name
                {getSortIcon('name')}
              </div>
            </th>
            <th className="px-6 py-3 text-left text-xs font-medium text-gray-500 uppercase tracking-wider">
              Contact Info
            </th>
            <th
              className="px-6 py-3 text-left text-xs font-medium text-gray-500 uppercase tracking-wider cursor-pointer hover:bg-gray-100"
              onClick={() => onSort('location')}
            >
              <div className="flex items-center">
                Location
                {getSortIcon('location')}
              </div>
            </th>
            <th
              className="px-6 py-3 text-left text-xs font-medium text-gray-500 uppercase tracking-wider cursor-pointer hover:bg-gray-100"
              onClick={() => onSort('birthday')}
            >
              <div className="flex items-center">
                Birthday
                {getSortIcon('birthday')}
              </div>
            </th>
            <th
              className="px-6 py-3 text-left text-xs font-medium text-gray-500 uppercase tracking-wider cursor-pointer hover:bg-gray-100"
              onClick={() => onSort('last_contacted')}
            >
              <div className="flex items-center">
                Last Contacted
                {getSortIcon('last_contacted')}
              </div>
            </th>
            <th className="relative px-6 py-3">
              <span className="sr-only">Actions</span>
            </th>
          </tr>
        </thead>
        <tbody className="bg-white divide-y divide-gray-200">
          {contacts.map(contact => (
            <tr
              key={contact.id}
              className="hover:bg-gray-50 cursor-pointer"
              onClick={() => handleRowClick(contact.id)}
            >
              <td className="px-6 py-4 whitespace-nowrap">
                <div className="flex items-center">
                  <div className="flex-shrink-0 h-10 w-10">
                    <div className="h-10 w-10 rounded-full bg-gray-300 flex items-center justify-center">
                      <span className="text-sm font-medium text-gray-700">
                        {contact.full_name.charAt(0).toUpperCase()}
                      </span>
                    </div>
                  </div>
                  <div className="ml-4">
                    <div className="text-sm font-medium text-gray-900">
                      <Link href={`/contacts/${contact.id}`} className="hover:text-blue-600">
                        {contact.full_name}
                      </Link>
                    </div>
                    {contact.cadence && (
                      <div className="text-sm text-gray-700">Cadence: {contact.cadence}</div>
                    )}
                  </div>
                </div>
              </td>
              <td className="px-6 py-4 whitespace-nowrap">
                {(() => {
                  const { primary, secondary } = getPrimaryAndSecondaryMethods(
                    contact.methods,
                    contact.primary_method
                  )
                  const methods = [primary, secondary].filter(Boolean) as ContactMethod[]

                  if (methods.length === 0) {
                    return <span className="text-sm text-gray-500">-</span>
                  }

                  return (
                    <div className="space-y-1">
                      {methods.map((method, index) => {
                        const value = formatContactMethodValue(method.type, method.value)
                        const href = getContactMethodHref(method.type, method.value)
                        const label = getContactMethodLabel(method.type)
                        const key = method.id ?? `${method.type}-${method.value}`
                        const valueClassName =
                          index === 0 ? 'font-medium text-gray-900' : 'text-gray-700'

                        return (
                          <div key={key} className={`flex items-center text-sm ${valueClassName}`}>
                            <ContactMethodIcon
                              type={method.type}
                              className="w-4 h-4 mr-2 text-gray-400"
                            />
                            {href ? (
                              <a href={href} className="hover:text-blue-600">
                                {value}
                              </a>
                            ) : (
                              <span>{value}</span>
                            )}
                            <span className="ml-2 text-xs text-gray-500">{label}</span>
                          </div>
                        )
                      })}
                    </div>
                  )
                })()}
              </td>
              <td className="px-6 py-4 whitespace-nowrap">
                {contact.location && (
                  <div className="flex items-center text-sm text-gray-900">
                    <MapPin className="w-4 h-4 mr-2 text-gray-400" />
                    {contact.location}
                  </div>
                )}
              </td>
              <td className="px-6 py-4 whitespace-nowrap text-sm text-gray-500">
                {contact.birthday ? formatDateOnly(contact.birthday) : '-'}
              </td>
              <td className="px-6 py-4 whitespace-nowrap text-sm text-gray-500">
                {contact.last_contacted
                  ? new Date(contact.last_contacted).toLocaleDateString()
                  : 'Never'}
              </td>
              <td className="px-6 py-4 whitespace-nowrap text-right text-sm font-medium">
                <div className="relative" onClick={handleDropdownClick}>
                  <button
                    ref={el => setButtonRef(contact.id, el)}
                    className="text-gray-400 hover:text-gray-500"
                    onClick={e => {
                      e.stopPropagation()
                      if (openDropdown === contact.id) {
                        setOpenDropdown(null)
                      } else {
                        // Calculate if dropdown should open above or below
                        const button = buttonRefs.current.get(contact.id)
                        if (button) {
                          const rect = button.getBoundingClientRect()
                          const spaceBelow = window.innerHeight - rect.bottom
                          const dropdownHeight = 50 // approximate height of dropdown
                          setDropdownPosition(spaceBelow < dropdownHeight ? 'above' : 'below')
                        }
                        setOpenDropdown(contact.id)
                      }
                    }}
                  >
                    <MoreHorizontal className="w-5 h-5" />
                  </button>

                  {openDropdown === contact.id && (
                    <div
                      className={`absolute right-0 w-48 bg-white rounded-md shadow-lg z-10 ring-1 ring-black ring-opacity-5 ${
                        dropdownPosition === 'above' ? 'bottom-full mb-2' : 'top-full mt-2'
                      }`}
                    >
                      <div className="py-1">
                        <button
                          onClick={e => handleMarkAsContacted(e, contact.id)}
                          className="flex items-center w-full px-4 py-2 text-sm text-gray-700 hover:bg-gray-100"
                          disabled={updateLastContacted.isPending}
                        >
                          <CheckCircle className="w-4 h-4 mr-2" />
                          {updateLastContacted.isPending ? 'Marking...' : 'Mark as Contacted'}
                        </button>
                      </div>
                    </div>
                  )}
                </div>
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  )
}

export default function ContactsPage() {
  const [searchTerm, setSearchTerm] = useState('')
  const [params, setParams] = useState<ContactListParams>({
    page: 1,
    limit: 20,
  })

  const { data, isLoading, error } = useContacts({
    ...params,
    ...(searchTerm && { search: searchTerm }),
  })

  const handleSearch = (e: React.FormEvent) => {
    e.preventDefault()
    setParams(prev => ({ ...prev, page: 1 }))
  }

  const handleSort = (field: SortField) => {
    setParams(prev => {
      // If clicking the same field, toggle order
      if (prev.sort === field) {
        return {
          ...prev,
          order: prev.order === 'asc' ? 'desc' : 'asc',
          page: 1, // Reset to first page when sorting
        }
      }
      // If clicking a new field, default to ascending
      return {
        ...prev,
        sort: field,
        order: 'asc',
        page: 1,
      }
    })
  }

  return (
    <div className="min-h-screen bg-gray-50">
      <Navigation />

      <div className="max-w-7xl mx-auto py-6 sm:px-6 lg:px-8">
        {/* Header */}
        <div className="md:flex md:items-center md:justify-between mb-6">
          <div className="flex-1 min-w-0">
            <h2 className="text-2xl font-bold leading-7 text-gray-900 sm:text-3xl sm:truncate">
              Contacts
            </h2>
            <p className="mt-1 text-sm text-gray-500">
              {data?.total ? `${data.total} contacts` : 'Loading contacts...'}
            </p>
          </div>
          <div className="mt-4 flex md:mt-0 md:ml-4">
            <Link href="/contacts/new">
              <Button>
                <Plus className="w-4 h-4 mr-2" />
                New Contact
              </Button>
            </Link>
          </div>
        </div>

        {/* Search */}
        <div className="mb-6">
          <form onSubmit={handleSearch} className="max-w-md">
            <div className="relative">
              <div className="absolute inset-y-0 left-0 pl-3 flex items-center pointer-events-none">
                <Search className="h-5 w-5 text-gray-400" />
              </div>
              <input
                type="text"
                placeholder="Search contacts..."
                value={searchTerm}
                onChange={e => setSearchTerm(e.target.value)}
                className={FORM_CONTROL_WITH_ICON}
              />
            </div>
          </form>
        </div>

        {/* Error state */}
        {error && (
          <div className="bg-red-50 border border-red-200 rounded-md p-4 mb-6">
            <div className="flex">
              <div className="flex-shrink-0">
                <svg className="h-5 w-5 text-red-400" viewBox="0 0 20 20" fill="currentColor">
                  <path
                    fillRule="evenodd"
                    d="M10 18a8 8 0 100-16 8 8 0 000 16zM8.707 7.293a1 1 0 00-1.414 1.414L8.586 10l-1.293 1.293a1 1 0 101.414 1.414L10 11.414l1.293 1.293a1 1 0 001.414-1.414L11.414 10l1.293-1.293a1 1 0 00-1.414-1.414L10 8.586 8.707 7.293z"
                    clipRule="evenodd"
                  />
                </svg>
              </div>
              <div className="ml-3">
                <h3 className="text-sm font-medium text-red-800">Error loading contacts</h3>
                <p className="mt-1 text-sm text-red-700">
                  {error instanceof Error ? error.message : 'An unexpected error occurred'}
                </p>
              </div>
            </div>
          </div>
        )}

        {/* Contacts Table */}
        <div className="bg-white shadow sm:rounded-md">
          <ContactsTable
            contacts={data?.contacts || []}
            loading={isLoading}
            sortBy={params.sort}
            sortOrder={params.order}
            onSort={handleSort}
          />
        </div>

        {/* Pagination */}
        {data && data.pages > 1 && (
          <div className="mt-6 flex items-center justify-between">
            <div className="text-sm text-gray-700">
              Showing page {data.page} of {data.pages} ({data.total} total contacts)
            </div>
            <div className="flex space-x-2">
              <Button
                variant="outline"
                size="sm"
                disabled={data.page <= 1}
                onClick={() => setParams(prev => ({ ...prev, page: prev.page! - 1 }))}
              >
                Previous
              </Button>
              <Button
                variant="outline"
                size="sm"
                disabled={data.page >= data.pages}
                onClick={() => setParams(prev => ({ ...prev, page: prev.page! + 1 }))}
              >
                Next
              </Button>
            </div>
          </div>
        )}
      </div>
    </div>
  )
}
