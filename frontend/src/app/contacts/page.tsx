'use client'

import { useState, useEffect, useRef, useCallback } from 'react'
import { createPortal } from 'react-dom'
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
  Edit,
  GitMerge,
} from 'lucide-react'
import { useContacts, useUpdateLastContacted } from '@/hooks/use-contacts'
import { Button } from '@/components/ui/button'
import { Pagination } from '@/components/ui/pagination'
import { Navigation } from '@/components/layout/navigation'
import { FORM_CONTROL_WITH_ICON, FORM_SELECT_BASE } from '@/lib/form-classes'
import { formatDateOnly, formatCadence } from '@/lib/utils'
import type { Contact, ContactListParams } from '@/types/contact'

type SortField = 'name' | 'location' | 'birthday' | 'last_contacted' | 'contact_by' | 'cadence'

// Default sort configuration
const DEFAULT_SORT_FIELD: SortField = 'cadence'
const DEFAULT_SORT_ORDER: 'asc' | 'desc' = 'desc'

function ContactsTable({
  contacts,
  loading,
  sortBy,
  sortOrder,
  searchTerm,
  cadenceFilter,
  onSort,
}: {
  contacts: Contact[]
  loading: boolean
  sortBy?: SortField
  sortOrder?: 'asc' | 'desc'
  searchTerm?: string
  cadenceFilter?: 'has_cadence' | 'no_cadence'
  onSort: (field: SortField) => void
}) {
  const router = useRouter()
  const [openDropdown, setOpenDropdown] = useState<string | null>(null)
  const [dropdownStyle, setDropdownStyle] = useState<React.CSSProperties>({})
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

  // Build URL with navigation context params
  // Always include sort/order so navigation order matches list order
  const buildContactUrl = (contactId: string, action?: 'edit' | 'merge') => {
    const params = new URLSearchParams()
    params.set('sort', sortBy || DEFAULT_SORT_FIELD)
    params.set('order', sortOrder || DEFAULT_SORT_ORDER)
    if (searchTerm) params.set('search', searchTerm)
    if (cadenceFilter) params.set('cadence_filter', cadenceFilter)
    if (action) params.set('action', action)
    return `/contacts/${contactId}?${params.toString()}`
  }

  const handleRowClick = (contactId: string) => {
    router.push(buildContactUrl(contactId))
  }

  const handleMarkAsContacted = async (e: React.MouseEvent, contactId: string) => {
    e.stopPropagation() // Prevent row click
    try {
      await updateLastContacted.mutateAsync({ id: contactId })
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
    <div className="shadow sm:rounded-lg overflow-hidden">
      <table className="min-w-full divide-y divide-gray-300">
        <thead className="bg-gray-50">
          <tr>
            <th
              className="px-4 py-3 text-left text-xs font-medium text-gray-500 uppercase tracking-wider cursor-pointer hover:bg-gray-100"
              onClick={() => onSort('name')}
            >
              <div className="flex items-center">
                Name
                {getSortIcon('name')}
              </div>
            </th>
            <th
              className="px-4 py-3 text-left text-xs font-medium text-gray-500 uppercase tracking-wider cursor-pointer hover:bg-gray-100"
              onClick={() => onSort('cadence')}
            >
              <div className="flex items-center">
                Cadence
                {getSortIcon('cadence')}
              </div>
            </th>
            <th
              className="px-4 py-3 text-left text-xs font-medium text-gray-500 uppercase tracking-wider cursor-pointer hover:bg-gray-100"
              onClick={() => onSort('location')}
            >
              <div className="flex items-center">
                Location
                {getSortIcon('location')}
              </div>
            </th>
            <th
              className="px-4 py-3 text-left text-xs font-medium text-gray-500 uppercase tracking-wider cursor-pointer hover:bg-gray-100"
              onClick={() => onSort('birthday')}
            >
              <div className="flex items-center">
                Birthday
                {getSortIcon('birthday')}
              </div>
            </th>
            <th
              className="px-4 py-3 text-left text-xs font-medium text-gray-500 uppercase tracking-wider cursor-pointer hover:bg-gray-100"
              onClick={() => onSort('last_contacted')}
            >
              <div className="flex items-center">
                Last Contact
                {getSortIcon('last_contacted')}
              </div>
            </th>
            <th
              className="px-4 py-3 text-left text-xs font-medium text-gray-500 uppercase tracking-wider cursor-pointer hover:bg-gray-100"
              onClick={() => onSort('contact_by')}
            >
              <div className="flex items-center">
                Next Contact
                {getSortIcon('contact_by')}
              </div>
            </th>
            <th className="relative px-4 py-3">
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
              <td className="px-4 py-4 whitespace-nowrap">
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
                      <Link href={buildContactUrl(contact.id)} className="hover:text-blue-600">
                        {contact.full_name}
                      </Link>
                    </div>
                  </div>
                </div>
              </td>
              <td className="px-4 py-4 whitespace-nowrap text-sm text-gray-500">
                {formatCadence(contact.cadence)}
              </td>
              {/* Location column: max-w-[200px] balances table layout with readability.
                  Truncated text shows full value via native title tooltip (desktop only). */}
              <td className="px-4 py-4 whitespace-nowrap">
                {contact.location && (
                  <div
                    className="flex items-center text-sm text-gray-900 max-w-[200px]"
                    title={contact.location}
                  >
                    <MapPin className="w-4 h-4 mr-2 flex-shrink-0 text-gray-400" />
                    <span className="truncate">{contact.location}</span>
                  </div>
                )}
              </td>
              <td className="px-4 py-4 whitespace-nowrap text-sm text-gray-500">
                {contact.birthday
                  ? formatDateOnly(contact.birthday, {
                      year: '2-digit',
                      month: 'numeric',
                      day: 'numeric',
                    })
                  : '-'}
              </td>
              <td className="px-4 py-4 whitespace-nowrap text-sm text-gray-500">
                {contact.last_contacted
                  ? formatDateOnly(contact.last_contacted, {
                      year: '2-digit',
                      month: 'numeric',
                      day: 'numeric',
                    })
                  : 'Never'}
              </td>
              <td className="px-4 py-4 whitespace-nowrap text-sm text-gray-500">
                {contact.cadence && contact.contact_by
                  ? formatDateOnly(contact.contact_by, {
                      year: '2-digit',
                      month: 'numeric',
                      day: 'numeric',
                    })
                  : '-'}
              </td>
              <td className="px-4 py-4 whitespace-nowrap text-right text-sm font-medium">
                <div onClick={handleDropdownClick}>
                  <button
                    ref={el => setButtonRef(contact.id, el)}
                    className="text-gray-400 hover:text-gray-500"
                    aria-label="Contact actions"
                    aria-haspopup="menu"
                    aria-expanded={openDropdown === contact.id}
                    onClick={e => {
                      e.stopPropagation()
                      if (openDropdown === contact.id) {
                        setOpenDropdown(null)
                      } else {
                        const button = buttonRefs.current.get(contact.id)
                        if (button) {
                          const rect = button.getBoundingClientRect()
                          const dropdownHeight = 140
                          const spaceBelow = window.innerHeight - rect.bottom
                          const openAbove = spaceBelow < dropdownHeight
                          setDropdownStyle({
                            position: 'fixed',
                            right: window.innerWidth - rect.right,
                            ...(openAbove
                              ? { bottom: window.innerHeight - rect.top + 4 }
                              : { top: rect.bottom + 4 }),
                          })
                          setOpenDropdown(contact.id)
                        }
                      }
                    }}
                  >
                    <MoreHorizontal className="w-5 h-5" />
                  </button>

                  {openDropdown === contact.id &&
                    createPortal(
                      <div
                        ref={el => {
                          // Auto-focus first menu item when dropdown opens
                          if (el) {
                            const first = el.querySelector<HTMLElement>('[role="menuitem"]')
                            first?.focus()
                          }
                        }}
                        style={dropdownStyle}
                        className="w-48 bg-white rounded-md shadow-lg z-50 ring-1 ring-black ring-opacity-5"
                        role="menu"
                        onKeyDown={e => {
                          const items = (
                            e.currentTarget as HTMLElement
                          ).querySelectorAll<HTMLElement>('[role="menuitem"]')
                          const current = document.activeElement as HTMLElement
                          const idx = Array.from(items).indexOf(current)

                          if (e.key === 'Escape') {
                            setOpenDropdown(null)
                            buttonRefs.current.get(contact.id)?.focus()
                          } else if (e.key === 'ArrowDown') {
                            e.preventDefault()
                            items[(idx + 1) % items.length]?.focus()
                          } else if (e.key === 'ArrowUp') {
                            e.preventDefault()
                            items[(idx - 1 + items.length) % items.length]?.focus()
                          } else if (e.key === 'Home') {
                            e.preventDefault()
                            items[0]?.focus()
                          } else if (e.key === 'End') {
                            e.preventDefault()
                            items[items.length - 1]?.focus()
                          } else if (e.key === 'Tab') {
                            setOpenDropdown(null)
                            buttonRefs.current.get(contact.id)?.focus()
                          }
                        }}
                      >
                        <div className="py-1">
                          <button
                            role="menuitem"
                            tabIndex={-1}
                            onClick={e => handleMarkAsContacted(e, contact.id)}
                            className="flex items-center w-full px-4 py-2 text-sm text-gray-700 hover:bg-gray-100 focus:bg-gray-100 focus:outline-none"
                            disabled={updateLastContacted.isPending}
                          >
                            <CheckCircle className="w-4 h-4 mr-2" />
                            {updateLastContacted.isPending ? 'Marking...' : 'Mark as Contacted'}
                          </button>
                          <button
                            role="menuitem"
                            tabIndex={-1}
                            onClick={e => {
                              e.stopPropagation()
                              router.push(buildContactUrl(contact.id, 'edit'))
                            }}
                            className="flex items-center w-full px-4 py-2 text-sm text-gray-700 hover:bg-gray-100 focus:bg-gray-100 focus:outline-none"
                          >
                            <Edit className="w-4 h-4 mr-2" />
                            Edit
                          </button>
                          <button
                            role="menuitem"
                            tabIndex={-1}
                            onClick={e => {
                              e.stopPropagation()
                              router.push(buildContactUrl(contact.id, 'merge'))
                            }}
                            className="flex items-center w-full px-4 py-2 text-sm text-gray-700 hover:bg-gray-100 focus:bg-gray-100 focus:outline-none"
                          >
                            <GitMerge className="w-4 h-4 mr-2" />
                            Merge
                          </button>
                        </div>
                      </div>,
                      document.body
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
    sort: DEFAULT_SORT_FIELD,
    order: DEFAULT_SORT_ORDER,
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
      // Default sort: cadence/last_contacted → desc (most frequent/recent first)
      // contact_by/birthday/name/location → asc (soonest due/alphabetical first)
      return {
        ...prev,
        sort: field,
        order: field === 'cadence' || field === 'last_contacted' ? 'desc' : 'asc',
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
            <h2 className="text-2xl font-bold leading-normal text-gray-900 sm:text-3xl sm:truncate">
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

        {/* Search and Filter */}
        <div className="mb-6 flex gap-3 items-start">
          <form onSubmit={handleSearch} className="max-w-md flex-1">
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
          <select
            value={params.cadence_filter || ''}
            onChange={e =>
              setParams(prev => ({
                ...prev,
                cadence_filter:
                  (e.target.value as ContactListParams['cadence_filter']) || undefined,
                page: 1,
              }))
            }
            className={FORM_SELECT_BASE + ' w-40'}
            aria-label="Filter by cadence"
          >
            <option value="">All contacts</option>
            <option value="has_cadence">Has cadence</option>
            <option value="no_cadence">No cadence</option>
          </select>
        </div>

        {/* Top Pagination */}
        {data && data.pages > 1 && (
          <div className="mb-6">
            <Pagination
              page={data.page}
              pages={data.pages}
              total={data.total}
              onPageChange={p => setParams(prev => ({ ...prev, page: p }))}
              noun="contacts"
            />
          </div>
        )}

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
        <div className="bg-white shadow sm:rounded-lg">
          <ContactsTable
            contacts={data?.contacts || []}
            loading={isLoading}
            sortBy={params.sort}
            sortOrder={params.order}
            searchTerm={searchTerm || undefined}
            cadenceFilter={params.cadence_filter}
            onSort={handleSort}
          />
        </div>

        {/* Bottom Pagination */}
        {data && data.pages > 1 && (
          <div className="mt-6">
            <Pagination
              page={data.page}
              pages={data.pages}
              total={data.total}
              onPageChange={p => setParams(prev => ({ ...prev, page: p }))}
              noun="contacts"
            />
          </div>
        )}
      </div>
    </div>
  )
}
