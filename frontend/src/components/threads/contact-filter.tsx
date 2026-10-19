'use client'

import { ContactSelector } from '@/components/ui/contact-selector'
import { useContact, useContacts } from '@/hooks/use-contacts'

interface ContactFilterProps {
  contactId: string | undefined
  onChange: (contactId: string | undefined) => void
}

/** The contact filter both tabs share: a searchable choice among all contacts. */
export function ContactFilter({ contactId, onChange }: ContactFilterProps) {
  const contacts = useContacts({ limit: 1000, sort: 'name', order: 'asc' })
  // A contact filter set from a link may name a contact beyond the loaded
  // options; it stays selectable under its own name.
  const selected = useContact(contactId ?? '')
  const options = contacts.data?.contacts ?? []
  const missing =
    contactId && selected.data && !options.some(contact => contact.id === contactId)
      ? [selected.data]
      : []

  return (
    <div className="flex min-w-[16rem] flex-col gap-1 text-sm">
      <span id="contact-filter-label" className="font-medium text-gray-700">
        Contact
      </span>
      <div role="group" aria-labelledby="contact-filter-label">
        <ContactSelector
          contacts={[...missing, ...options]}
          value={contactId}
          onChange={onChange}
          noContactLabel="All contacts"
        />
      </div>
    </div>
  )
}
