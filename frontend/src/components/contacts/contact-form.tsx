'use client'

import { useForm } from 'react-hook-form'
import { zodResolver } from '@hookform/resolvers/zod'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Textarea } from '@/components/ui/textarea'
import {
  contactSchema,
  transformContactFormData,
  type ContactFormData,
} from '@/lib/validations/contact'
import type { Contact } from '@/types/contact'

interface ContactFormProps {
  contact?: Contact
  onSubmit: (data: ContactFormData) => void | Promise<void>
  loading?: boolean
  submitText?: string
}

const cadenceOptions = [
  { value: '', label: 'No cadence' },
  { value: 'weekly', label: 'Weekly' },
  { value: 'biweekly', label: 'Bi-weekly' },
  { value: 'monthly', label: 'Monthly' },
  { value: 'quarterly', label: 'Quarterly' },
  { value: 'biannual', label: 'Bi-annual' },
  { value: 'annual', label: 'Annual' },
]

export function ContactForm({
  contact,
  onSubmit,
  loading,
  submitText = 'Save Contact',
}: ContactFormProps) {
  const {
    register,
    handleSubmit,
    formState: { errors, isSubmitting },
    reset,
  } = useForm<ContactFormData>({
    resolver: zodResolver(contactSchema),
    defaultValues: contact
      ? {
          full_name: contact.full_name,
          email: contact.email || '',
          phone: contact.phone || '',
          location: contact.location || '',
          birthday: contact.birthday ? contact.birthday.split('T')[0] : '', // Format date for input
          notes: contact.notes || '',
          cadence: contact.cadence || '',
        }
      : {
          full_name: '',
          email: '',
          phone: '',
          location: '',
          birthday: '',
          notes: '',
          cadence: '',
        },
  })

  const handleFormSubmit = async (data: ContactFormData) => {
    try {
      const transformedData = transformContactFormData(data)
      await onSubmit(transformedData)
      if (!contact) {
        // Reset form after creating new contact
        reset()
      }
    } catch (error) {
      console.error('Form submission error:', error)
    }
  }

  const isLoading = loading || isSubmitting

  return (
    <form onSubmit={handleSubmit(handleFormSubmit)} className="space-y-6">
      <div className="grid grid-cols-1 gap-6">
        {/* Full Name */}
        <Input
          {...register('full_name')}
          label="Full Name"
          placeholder="Enter full name"
          error={errors.full_name?.message}
          required
          disabled={isLoading}
        />

        {/* Email and Phone */}
        <div className="grid grid-cols-1 md:grid-cols-2 gap-6">
          <Input
            {...register('email')}
            label="Email"
            type="email"
            placeholder="Enter email address"
            error={errors.email?.message}
            disabled={isLoading}
          />
          <Input
            {...register('phone')}
            label="Phone"
            type="tel"
            placeholder="Enter phone number"
            error={errors.phone?.message}
            disabled={isLoading}
          />
        </div>

        {/* Location and Birthday */}
        <div className="grid grid-cols-1 md:grid-cols-2 gap-6">
          <Input
            {...register('location')}
            label="Location"
            placeholder="Enter location (city, country)"
            error={errors.location?.message}
            disabled={isLoading}
          />
          <Input
            {...register('birthday')}
            label="Birthday"
            type="date"
            error={errors.birthday?.message}
            disabled={isLoading}
            helpText="Optional - for birthday reminders"
          />
        </div>

        {/* Cadence */}
        <div className="space-y-1">
          <label htmlFor="cadence" className="block text-sm font-medium text-gray-700">
            Contact Cadence
          </label>
          <select
            {...register('cadence')}
            id="cadence"
            className="block w-full rounded-md border-gray-300 shadow-sm focus:border-blue-500 focus:ring-blue-500 sm:text-sm text-gray-900 disabled:bg-gray-50 disabled:text-gray-500"
            disabled={isLoading}
          >
            {cadenceOptions.map(option => (
              <option key={option.value} value={option.value}>
                {option.label}
              </option>
            ))}
          </select>
          {errors.cadence && <p className="text-sm text-red-600">{errors.cadence.message}</p>}
          <p className="text-sm text-gray-500">How often you want to be reminded to reach out</p>
        </div>

        {/* Notes */}
        <Textarea
          {...register('notes')}
          label="Notes"
          placeholder="Add any notes about this contact..."
          rows={4}
          error={errors.notes?.message}
          disabled={isLoading}
          helpText="Any additional information you want to remember about this contact"
        />
      </div>

      {/* Submit Button */}
      <div className="flex justify-end space-x-3">
        <Button
          type="button"
          variant="outline"
          onClick={() => window.history.back()}
          disabled={isLoading}
        >
          Cancel
        </Button>
        <Button type="submit" loading={isLoading} disabled={isLoading}>
          {submitText}
        </Button>
      </div>
    </form>
  )
}
