'use client'

import { useState, useRef, useEffect } from 'react'
import { Search, User, X } from 'lucide-react'
import { clsx } from 'clsx'
import {
  formatContactMethodValue,
  getContactMethodLabel,
  getPrimaryAndSecondaryMethods,
} from '@/lib/contact-methods'
import type { ContactMethod } from '@/types/contact'

interface Contact {
  id: string
  full_name: string
  methods?: ContactMethod[]
  primary_method?: ContactMethod
}

interface ContactSelectorProps {
  contacts: Contact[]
  value?: string
  onChange: (contactId: string | undefined) => void
  placeholder?: string
  disabled?: boolean
  error?: string
}

export function ContactSelector({
  contacts,
  value,
  onChange,
  placeholder = 'Search contacts...',
  disabled = false,
  error,
}: ContactSelectorProps) {
  const [isOpen, setIsOpen] = useState(false)
  const [searchTerm, setSearchTerm] = useState('')
  const [highlightedIndex, setHighlightedIndex] = useState(-1)
  const containerRef = useRef<HTMLDivElement>(null)
  const inputRef = useRef<HTMLInputElement>(null)

  const selectedContact = contacts.find(contact => contact.id === value)
  const normalizedSearch = searchTerm.toLowerCase()
  const handleSearch = normalizedSearch.startsWith('@')
    ? normalizedSearch.slice(1)
    : normalizedSearch

  const filteredContacts = contacts
    .filter(contact => {
      if (contact.full_name.toLowerCase().includes(normalizedSearch)) {
        return true
      }

      const methodsToSearch = contact.methods?.length
        ? contact.methods
        : contact.primary_method
          ? [contact.primary_method]
          : []

      return methodsToSearch.some(method => method.value.toLowerCase().includes(handleSearch))
    })
    .slice(0, 10) // Limit to 10 results for performance

  useEffect(() => {
    function handleClickOutside(event: MouseEvent) {
      if (containerRef.current && !containerRef.current.contains(event.target as Node)) {
        setIsOpen(false)
        setSearchTerm('')
        setHighlightedIndex(-1)
      }
    }

    document.addEventListener('mousedown', handleClickOutside)
    return () => document.removeEventListener('mousedown', handleClickOutside)
  }, [])

  const handleInputClick = () => {
    if (!disabled) {
      setIsOpen(true)
      inputRef.current?.focus()
    }
  }

  const handleContactSelect = (contact: Contact) => {
    onChange(contact.id)
    setIsOpen(false)
    setSearchTerm('')
    setHighlightedIndex(-1)
  }

  const handleClear = (e: React.MouseEvent) => {
    e.stopPropagation()
    onChange(undefined)
    setSearchTerm('')
  }

  const handleKeyDown = (e: React.KeyboardEvent) => {
    if (!isOpen) {
      if (e.key === 'Enter' || e.key === 'ArrowDown') {
        setIsOpen(true)
        e.preventDefault()
      }
      return
    }

    switch (e.key) {
      case 'ArrowDown':
        e.preventDefault()
        setHighlightedIndex(prev => (prev < filteredContacts.length - 1 ? prev + 1 : prev))
        break
      case 'ArrowUp':
        e.preventDefault()
        setHighlightedIndex(prev => (prev > 0 ? prev - 1 : -1))
        break
      case 'Enter':
        e.preventDefault()
        if (highlightedIndex >= 0 && filteredContacts[highlightedIndex]) {
          handleContactSelect(filteredContacts[highlightedIndex])
        }
        break
      case 'Escape':
        setIsOpen(false)
        setSearchTerm('')
        setHighlightedIndex(-1)
        break
    }
  }

  return (
    <div ref={containerRef} className="relative">
      <div
        className={clsx(
          'relative w-full cursor-pointer rounded-md border bg-white py-2 pl-3 pr-10 text-left shadow-sm',
          'focus-within:border-blue-500 focus-within:ring-1 focus-within:ring-blue-500',
          disabled && 'cursor-not-allowed bg-gray-50',
          error && 'border-red-300 focus-within:border-red-500 focus-within:ring-red-500',
          !error && !disabled && 'border-gray-300'
        )}
        onClick={handleInputClick}
      >
        <div className="flex items-center">
          <Search className="mr-2 h-4 w-4 text-gray-400" />

          {isOpen ? (
            <input
              ref={inputRef}
              type="text"
              value={searchTerm}
              onChange={e => setSearchTerm(e.target.value)}
              onKeyDown={handleKeyDown}
              placeholder={placeholder}
              className="flex-1 border-none bg-transparent text-gray-900 placeholder-gray-500 focus:outline-none"
              disabled={disabled}
            />
          ) : selectedContact ? (
            <div className="flex flex-1 items-center justify-between">
              <div className="flex items-center">
                <User className="mr-2 h-4 w-4 text-gray-400" />
                <span className="text-gray-900">{selectedContact.full_name}</span>
                {(() => {
                  const { primary } = getPrimaryAndSecondaryMethods(
                    selectedContact.methods,
                    selectedContact.primary_method
                  )
                  if (!primary) {
                    return null
                  }
                  const value = formatContactMethodValue(primary.type, primary.value)
                  return <span className="ml-2 text-sm text-gray-500">({value})</span>
                })()}
              </div>
              <button
                type="button"
                onClick={handleClear}
                className="flex-shrink-0 text-gray-400 hover:text-gray-600"
                disabled={disabled}
              >
                <X className="h-4 w-4" />
              </button>
            </div>
          ) : (
            <span className="text-gray-500">{placeholder}</span>
          )}
        </div>
      </div>

      {isOpen && !disabled && (
        <div className="absolute z-10 mt-1 max-h-60 w-full overflow-auto rounded-md bg-white py-1 text-base shadow-lg ring-1 ring-black ring-opacity-5 focus:outline-none sm:text-sm">
          {/* No contact option */}
          <div
            className={clsx(
              'relative cursor-pointer select-none py-2 pl-10 pr-4',
              highlightedIndex === -1 ? 'bg-blue-600 text-white' : 'text-gray-900 hover:bg-gray-100'
            )}
            onClick={() => {
              onChange(undefined)
              setIsOpen(false)
              setSearchTerm('')
              setHighlightedIndex(-1)
            }}
          >
            <span className="block truncate font-normal">No contact (standalone reminder)</span>
          </div>

          {filteredContacts.length === 0 && searchTerm ? (
            <div className="relative cursor-default select-none py-2 pl-10 pr-4 text-gray-700">
              No contacts found for &ldquo;{searchTerm}&rdquo;
            </div>
          ) : (
            filteredContacts.map((contact, index) => (
              <div
                key={contact.id}
                className={clsx(
                  'relative cursor-pointer select-none py-2 pl-10 pr-4',
                  highlightedIndex === index
                    ? 'bg-blue-600 text-white'
                    : 'text-gray-900 hover:bg-gray-100'
                )}
                onClick={() => handleContactSelect(contact)}
              >
                <span className="block truncate font-normal">{contact.full_name}</span>
                {(() => {
                  const { primary } = getPrimaryAndSecondaryMethods(
                    contact.methods,
                    contact.primary_method
                  )
                  if (!primary) {
                    return null
                  }
                  const value = formatContactMethodValue(primary.type, primary.value)
                  const label = getContactMethodLabel(primary.type)

                  return (
                    <span
                      className={clsx(
                        'block truncate text-sm',
                        highlightedIndex === index ? 'text-blue-200' : 'text-gray-500'
                      )}
                    >
                      {value} · {label}
                    </span>
                  )
                })()}
                {value === contact.id && (
                  <span className="absolute inset-y-0 left-0 flex items-center pl-3">
                    <User className="h-4 w-4" />
                  </span>
                )}
              </div>
            ))
          )}
        </div>
      )}

      {error && <p className="mt-1 text-sm text-red-600">{error}</p>}
    </div>
  )
}
