'use client'

import { useState } from 'react'
import { useParams, useRouter } from 'next/navigation'
import { Navigation } from '@/components/layout/navigation'
import { ContactForm } from '@/components/contacts/contact-form'
import { Button } from '@/components/ui/button'
import { useContact, useUpdateContact, useDeleteContact, useUpdateLastContacted } from '@/hooks/use-contacts'
import { useRemindersByContact } from '@/hooks/use-reminders'
import { formatDateOnly } from '@/lib/utils'
import { Edit, Trash2, MessageCircle, Mail, Phone, MapPin, Calendar, Bell, Clock } from 'lucide-react'
import type { ContactFormData } from '@/lib/validations/contact'

export default function ContactDetailPage() {
  const params = useParams()
  const router = useRouter()
  const contactId = params.id as string
  
  const [isEditing, setIsEditing] = useState(false)
  
  const { data: contact, isLoading, error } = useContact(contactId)
  const { data: reminders } = useRemindersByContact(contactId)
  const updateContactMutation = useUpdateContact()
  const deleteContactMutation = useDeleteContact()
  const updateLastContactedMutation = useUpdateLastContacted()

  const handleUpdateContact = async (data: ContactFormData) => {
    try {
      await updateContactMutation.mutateAsync({ id: contactId, data })
      setIsEditing(false)
    } catch (error) {
      console.error('Error updating contact:', error)
    }
  }

  const handleDeleteContact = async () => {
    if (confirm('Are you sure you want to delete this contact? This action cannot be undone.')) {
      try {
        await deleteContactMutation.mutateAsync(contactId)
        router.push('/contacts')
      } catch (error) {
        console.error('Error deleting contact:', error)
      }
    }
  }

  const handleMarkAsContacted = async () => {
    try {
      await updateLastContactedMutation.mutateAsync(contactId)
    } catch (error) {
      console.error('Error updating last contacted:', error)
    }
  }

  if (isLoading) {
    return (
      <div className="min-h-screen bg-gray-50">
        <Navigation />
        <div className="max-w-4xl mx-auto py-6 sm:px-6 lg:px-8">
          <div className="animate-pulse space-y-6">
            <div className="h-8 bg-gray-200 rounded w-1/3"></div>
            <div className="bg-white shadow sm:rounded-lg p-6 space-y-4">
              <div className="h-6 bg-gray-200 rounded w-1/2"></div>
              <div className="h-4 bg-gray-200 rounded w-3/4"></div>
              <div className="h-4 bg-gray-200 rounded w-1/2"></div>
            </div>
          </div>
        </div>
      </div>
    )
  }

  if (error || !contact) {
    return (
      <div className="min-h-screen bg-gray-50">
        <Navigation />
        <div className="max-w-4xl mx-auto py-6 sm:px-6 lg:px-8">
          <div className="bg-red-50 border border-red-200 rounded-md p-4">
            <h3 className="text-lg font-medium text-red-800">Contact not found</h3>
            <p className="mt-1 text-sm text-red-700">
              The contact you&apos;re looking for doesn&apos;t exist or you don&apos;t have permission to view it.
            </p>
            <div className="mt-4">
              <Button variant="outline" onClick={() => router.push('/contacts')}>
                Back to Contacts
              </Button>
            </div>
          </div>
        </div>
      </div>
    )
  }

  if (isEditing) {
    return (
      <div className="min-h-screen bg-gray-50">
        <Navigation />
        <div className="max-w-3xl mx-auto py-6 sm:px-6 lg:px-8">
          <div className="md:flex md:items-center md:justify-between mb-6">
            <div className="flex-1 min-w-0">
              <h2 className="text-2xl font-bold leading-7 text-gray-900 sm:text-3xl sm:truncate">
                Edit Contact
              </h2>
              <p className="mt-1 text-sm text-gray-500">
                Update {contact.full_name}&apos;s information
              </p>
            </div>
          </div>

          <div className="bg-white shadow sm:rounded-lg">
            <div className="px-4 py-5 sm:p-6">
              <ContactForm
                contact={contact}
                onSubmit={handleUpdateContact}
                loading={updateContactMutation.isPending}
                submitText="Update Contact"
              />
            </div>
          </div>

          {updateContactMutation.error && (
            <div className="mt-4 bg-red-50 border border-red-200 rounded-md p-4">
              <h3 className="text-sm font-medium text-red-800">Error updating contact</h3>
              <p className="mt-1 text-sm text-red-700">
                {updateContactMutation.error instanceof Error 
                  ? updateContactMutation.error.message 
                  : 'An unexpected error occurred'
                }
              </p>
            </div>
          )}
        </div>
      </div>
    )
  }

  return (
    <div className="min-h-screen bg-gray-50">
      <Navigation />
      
      <div className="max-w-4xl mx-auto py-6 sm:px-6 lg:px-8">
        {/* Header */}
        <div className="md:flex md:items-center md:justify-between mb-6">
          <div className="flex-1 min-w-0">
            <h2 className="text-2xl font-bold leading-7 text-gray-900 sm:text-3xl sm:truncate">
              {contact.full_name}
            </h2>
            <p className="mt-1 text-sm text-gray-500">
              Contact details and information
            </p>
          </div>
          <div className="mt-4 flex space-x-3 md:mt-0 md:ml-4">
            <Button
              variant="outline"
              size="sm"
              onClick={handleMarkAsContacted}
              loading={updateLastContactedMutation.isPending}
            >
              <MessageCircle className="w-4 h-4 mr-2" />
              Mark as Contacted
            </Button>
            <Button
              variant="outline"
              size="sm"
              onClick={() => setIsEditing(true)}
            >
              <Edit className="w-4 h-4 mr-2" />
              Edit
            </Button>
            <Button
              variant="danger"
              size="sm"
              onClick={handleDeleteContact}
              loading={deleteContactMutation.isPending}
            >
              <Trash2 className="w-4 h-4 mr-2" />
              Delete
            </Button>
          </div>
        </div>

        {/* Contact Info */}
        <div className="bg-white shadow overflow-hidden sm:rounded-lg">
          <div className="px-4 py-5 sm:px-6">
            <h3 className="text-lg leading-6 font-medium text-gray-900">Contact Information</h3>
            <p className="mt-1 max-w-2xl text-sm text-gray-500">Personal details and contact information.</p>
          </div>
          <div className="border-t border-gray-200">
            <dl className="divide-y divide-gray-200">
              <div className="py-4 sm:py-5 sm:grid sm:grid-cols-3 sm:gap-4 sm:px-6">
                <dt className="text-sm font-medium text-gray-500">Full name</dt>
                <dd className="mt-1 text-sm text-gray-900 sm:mt-0 sm:col-span-2">{contact.full_name}</dd>
              </div>
              
              {contact.email && (
                <div className="py-4 sm:py-5 sm:grid sm:grid-cols-3 sm:gap-4 sm:px-6">
                  <dt className="text-sm font-medium text-gray-500">Email</dt>
                  <dd className="mt-1 text-sm text-gray-900 sm:mt-0 sm:col-span-2">
                    <div className="flex items-center">
                      <Mail className="w-4 h-4 mr-2 text-gray-400" />
                      <a href={`mailto:${contact.email}`} className="text-blue-600 hover:text-blue-500">
                        {contact.email}
                      </a>
                    </div>
                  </dd>
                </div>
              )}
              
              {contact.phone && (
                <div className="py-4 sm:py-5 sm:grid sm:grid-cols-3 sm:gap-4 sm:px-6">
                  <dt className="text-sm font-medium text-gray-500">Phone</dt>
                  <dd className="mt-1 text-sm text-gray-900 sm:mt-0 sm:col-span-2">
                    <div className="flex items-center">
                      <Phone className="w-4 h-4 mr-2 text-gray-400" />
                      <a href={`tel:${contact.phone}`} className="text-blue-600 hover:text-blue-500">
                        {contact.phone}
                      </a>
                    </div>
                  </dd>
                </div>
              )}
              
              {contact.location && (
                <div className="py-4 sm:py-5 sm:grid sm:grid-cols-3 sm:gap-4 sm:px-6">
                  <dt className="text-sm font-medium text-gray-500">Location</dt>
                  <dd className="mt-1 text-sm text-gray-900 sm:mt-0 sm:col-span-2">
                    <div className="flex items-center">
                      <MapPin className="w-4 h-4 mr-2 text-gray-400" />
                      {contact.location}
                    </div>
                  </dd>
                </div>
              )}
              
              {contact.birthday && (
                <div className="py-4 sm:py-5 sm:grid sm:grid-cols-3 sm:gap-4 sm:px-6">
                  <dt className="text-sm font-medium text-gray-500">Birthday</dt>
                  <dd className="mt-1 text-sm text-gray-900 sm:mt-0 sm:col-span-2">
                    <div className="flex items-center">
                      <Calendar className="w-4 h-4 mr-2 text-gray-400" />
                      {formatDateOnly(contact.birthday)}
                    </div>
                  </dd>
                </div>
              )}
              
              {contact.cadence && (
                <div className="py-4 sm:py-5 sm:grid sm:grid-cols-3 sm:gap-4 sm:px-6">
                  <dt className="text-sm font-medium text-gray-500">Contact cadence</dt>
                  <dd className="mt-1 text-sm text-gray-900 sm:mt-0 sm:col-span-2">
                    <div className="flex items-center">
                      <Calendar className="w-4 h-4 mr-2 text-gray-400" />
                      {contact.cadence}
                    </div>
                  </dd>
                </div>
              )}
              
              <div className="py-4 sm:py-5 sm:grid sm:grid-cols-3 sm:gap-4 sm:px-6">
                <dt className="text-sm font-medium text-gray-500">Last contacted</dt>
                <dd className="mt-1 text-sm text-gray-900 sm:mt-0 sm:col-span-2">
                  {contact.last_contacted 
                    ? new Date(contact.last_contacted).toLocaleDateString()
                    : 'Never'
                  }
                </dd>
              </div>
              
              <div className="py-4 sm:py-5 sm:grid sm:grid-cols-3 sm:gap-4 sm:px-6">
                <dt className="text-sm font-medium text-gray-500">Created</dt>
                <dd className="mt-1 text-sm text-gray-900 sm:mt-0 sm:col-span-2">
                  {new Date(contact.created_at).toLocaleDateString()}
                </dd>
              </div>
              
              {contact.notes && (
                <div className="py-4 sm:py-5 sm:grid sm:grid-cols-3 sm:gap-4 sm:px-6">
                  <dt className="text-sm font-medium text-gray-500">Notes</dt>
                  <dd className="mt-1 text-sm text-gray-900 sm:mt-0 sm:col-span-2 whitespace-pre-wrap">
                    {contact.notes}
                  </dd>
                </div>
              )}
            </dl>
          </div>
        </div>

        {/* Reminders Section */}
        {reminders && reminders.length > 0 && (
          <div className="mt-8 bg-white shadow overflow-hidden sm:rounded-lg">
            <div className="px-4 py-5 sm:px-6 border-b border-gray-200">
              <h3 className="text-lg leading-6 font-medium text-gray-900 flex items-center">
                <Bell className="w-5 h-5 mr-2 text-gray-400" />
                Reminders ({reminders.length})
              </h3>
              <p className="mt-1 max-w-2xl text-sm text-gray-500">
                Active reminders for this contact
              </p>
            </div>
            <div className="divide-y divide-gray-200">
              {reminders.map((reminder) => {
                const isOverdue = new Date(reminder.due_date) < new Date() && !reminder.completed
                
                return (
                  <div key={reminder.id} className="px-4 py-4 sm:px-6">
                    <div className="flex items-center justify-between">
                      <div className="flex items-start space-x-3">
                        <div className={`w-2 h-2 rounded-full mt-2 ${
                          reminder.completed
                            ? 'bg-green-500'
                            : isOverdue
                            ? 'bg-red-500'
                            : 'bg-yellow-500'
                        }`} />
                        <div>
                          <h4 className="text-sm font-medium text-gray-900">
                            {reminder.title}
                          </h4>
                          {reminder.description && (
                            <p className="text-sm text-gray-600 mt-1">
                              {reminder.description}
                            </p>
                          )}
                          <div className="flex items-center space-x-4 mt-2 text-sm text-gray-500">
                            <div className="flex items-center space-x-1">
                              <Clock className="w-4 h-4" />
                              <span>Due {new Date(reminder.due_date).toLocaleDateString()}</span>
                            </div>
                            {isOverdue && (
                              <span className="text-red-600 font-medium">Overdue</span>
                            )}
                            {reminder.completed && (
                              <span className="text-green-600 font-medium">Completed</span>
                            )}
                          </div>
                        </div>
                      </div>
                    </div>
                  </div>
                )
              })}
            </div>
          </div>
        )}
      </div>
    </div>
  )
}
