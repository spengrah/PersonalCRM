'use client'

import { useForm } from 'react-hook-form'
import { zodResolver } from '@hookform/resolvers/zod'
import { z } from 'zod'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Textarea } from '@/components/ui/textarea'
import { useContacts } from '@/hooks/use-contacts'
import type { CreateReminderRequest } from '@/types/reminder'

const reminderSchema = z.object({
  contact_id: z.string().optional().or(z.literal('')),
  title: z.string()
    .min(1, 'Title is required')
    .max(255, 'Title must be less than 255 characters'),
  description: z.string()
    .max(1000, 'Description must be less than 1000 characters')
    .optional()
    .or(z.literal('')),
  due_date: z.string()
    .min(1, 'Due date is required')
    .refine((date) => {
      const parsedDate = new Date(date)
      return !isNaN(parsedDate.getTime())
    }, 'Please enter a valid date'),
})

type ReminderFormData = z.infer<typeof reminderSchema>

interface ReminderFormProps {
  onSubmit: (data: CreateReminderRequest) => void | Promise<void>
  onCancel: () => void
  loading?: boolean
}

export function ReminderForm({ onSubmit, onCancel, loading }: ReminderFormProps) {
  const { data: contactsData } = useContacts({ limit: 1000 }) // Get all contacts for dropdown
  
  const {
    register,
    handleSubmit,
    formState: { errors, isSubmitting },
    reset,
  } = useForm<ReminderFormData>({
    resolver: zodResolver(reminderSchema),
    defaultValues: {
      contact_id: '',
      title: '',
      description: '',
      due_date: new Date(Date.now() + 24 * 60 * 60 * 1000).toISOString().split('T')[0], // Tomorrow
    },
  })

  const handleFormSubmit = async (data: ReminderFormData) => {
    try {
      // Transform form data to API format
      const reminderData: CreateReminderRequest = {
        title: data.title,
        description: data.description || undefined,
        due_date: new Date(data.due_date).toISOString(),
        ...(data.contact_id && { contact_id: data.contact_id }),
      }
      
      await onSubmit(reminderData)
      reset()
    } catch (error) {
      console.error('Form submission error:', error)
    }
  }

  const isLoading = loading || isSubmitting

  return (
    <form onSubmit={handleSubmit(handleFormSubmit)} className="space-y-6">
      <div className="grid grid-cols-1 gap-6">
        {/* Title */}
        <Input
          {...register('title')}
          label="Title"
          placeholder="Enter reminder title"
          error={errors.title?.message}
          required
          disabled={isLoading}
        />

        {/* Contact Selection (Optional) */}
        <div className="space-y-1">
          <label htmlFor="contact_id" className="block text-sm font-medium text-gray-700">
            Contact (Optional)
          </label>
          <select
            {...register('contact_id')}
            id="contact_id"
            className="block w-full rounded-md border-gray-300 shadow-sm focus:border-blue-500 focus:ring-blue-500 sm:text-sm text-gray-900 disabled:bg-gray-50 disabled:text-gray-500"
            disabled={isLoading}
          >
            <option value="">No contact (standalone reminder)</option>
            {contactsData?.contacts?.map((contact) => (
              <option key={contact.id} value={contact.id}>
                {contact.full_name}
              </option>
            ))}
          </select>
          {errors.contact_id && (
            <p className="text-sm text-red-600">{errors.contact_id.message}</p>
          )}
          <p className="text-sm text-gray-500">
            Link this reminder to a specific contact, or leave blank for a standalone reminder
          </p>
        </div>

        {/* Due Date */}
        <Input
          {...register('due_date')}
          label="Due Date"
          type="date"
          error={errors.due_date?.message}
          required
          disabled={isLoading}
        />

        {/* Description */}
        <Textarea
          {...register('description')}
          label="Description (Optional)"
          placeholder="Add any additional details..."
          rows={3}
          error={errors.description?.message}
          disabled={isLoading}
          helpText="Optional description or notes about this reminder"
        />
      </div>

      {/* Submit Buttons */}
      <div className="flex justify-end space-x-3">
        <Button
          type="button"
          variant="outline"
          onClick={onCancel}
          disabled={isLoading}
        >
          Cancel
        </Button>
        <Button
          type="submit"
          loading={isLoading}
          disabled={isLoading}
        >
          Create Reminder
        </Button>
      </div>
    </form>
  )
}
