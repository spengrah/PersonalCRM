'use client'

import { useRouter } from 'next/navigation'
import { Navigation } from '@/components/layout/navigation'
import { ContactForm } from '@/components/contacts/contact-form'
import { useCreateContact } from '@/hooks/use-contacts'
import type { ContactFormData } from '@/lib/validations/contact'

export default function NewContactPage() {
  const router = useRouter()
  const createContactMutation = useCreateContact()

  const handleSubmit = async (data: ContactFormData) => {
    try {
      const newContact = await createContactMutation.mutateAsync(data)
      router.push(`/contacts/${newContact.id}`)
    } catch (error) {
      console.error('Error creating contact:', error)
      // Handle error - you might want to show a toast notification here
    }
  }

  return (
    <div className="min-h-screen bg-gray-50">
      <Navigation />

      <div className="max-w-3xl mx-auto py-6 sm:px-6 lg:px-8">
        <div className="md:flex md:items-center md:justify-between mb-6">
          <div className="flex-1 min-w-0">
            <h2 className="text-2xl font-bold leading-7 text-gray-900 sm:text-3xl sm:truncate">
              Add New Contact
            </h2>
            <p className="mt-1 text-sm text-gray-500">Create a new contact in your personal CRM</p>
          </div>
        </div>

        <div className="bg-white shadow sm:rounded-lg">
          <div className="px-4 py-5 sm:p-6">
            <ContactForm
              onSubmit={handleSubmit}
              loading={createContactMutation.isPending}
              submitText="Create Contact"
            />
          </div>
        </div>

        {/* Error Display */}
        {createContactMutation.error && (
          <div className="mt-4 bg-red-50 border border-red-200 rounded-md p-4">
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
                <h3 className="text-sm font-medium text-red-800">Error creating contact</h3>
                <p className="mt-1 text-sm text-red-700">
                  {createContactMutation.error instanceof Error
                    ? createContactMutation.error.message
                    : 'An unexpected error occurred'}
                </p>
              </div>
            </div>
          </div>
        )}
      </div>
    </div>
  )
}
