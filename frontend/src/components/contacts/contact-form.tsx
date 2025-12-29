'use client'

import { useForm, useFieldArray } from 'react-hook-form'
import { zodResolver } from '@hookform/resolvers/zod'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Textarea } from '@/components/ui/textarea'
import {
  contactSchema,
  transformContactFormData,
  type ContactFormData,
} from '@/lib/validations/contact'
import {
  CONTACT_METHOD_OPTIONS,
  formatContactMethodValue,
  sortContactMethods,
} from '@/lib/contact-methods'
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
  const defaultMethods = contact?.methods?.length
    ? sortContactMethods(contact.methods).map(method => ({
        type: method.type,
        value: formatContactMethodValue(method.type, method.value),
        is_primary: method.is_primary,
      }))
    : [{ type: '', value: '', is_primary: false }]

  const {
    register,
    handleSubmit,
    formState: { errors, isSubmitting },
    reset,
    control,
    setValue,
    watch,
  } = useForm<ContactFormData>({
    resolver: zodResolver(contactSchema),
    defaultValues: contact
      ? {
          full_name: contact.full_name,
          methods: defaultMethods,
          location: contact.location || '',
          birthday: contact.birthday ? contact.birthday.split('T')[0] : '', // Format date for input
          notes: contact.notes || '',
          cadence: contact.cadence || '',
        }
      : {
          full_name: '',
          methods: defaultMethods,
          location: '',
          birthday: '',
          notes: '',
          cadence: '',
        },
  })

  const { fields, append, remove } = useFieldArray({
    control,
    name: 'methods',
  })

  const watchedMethods = watch('methods')

  const handlePrimaryToggle = (index: number) => {
    const currentValue = watchedMethods?.[index]?.is_primary
    const nextValue = !currentValue

    fields.forEach((_, fieldIndex) => {
      setValue(`methods.${fieldIndex}.is_primary`, fieldIndex === index ? nextValue : false, {
        shouldDirty: true,
        shouldValidate: true,
      })
    })
  }

  const handleAddMethod = () => {
    append({ type: '', value: '', is_primary: false })
  }

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
  const methodsError =
    (errors.methods as { message?: string; root?: { message?: string } } | undefined)?.message ??
    (errors.methods as { message?: string; root?: { message?: string } } | undefined)?.root?.message

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

        <div className="space-y-3">
          <div className="flex items-center justify-between">
            <div>
              <label className="block text-sm font-medium text-gray-700">Contact methods</label>
              <p className="text-sm text-gray-500">Add one or more ways to reach this contact.</p>
            </div>
            <Button type="button" variant="outline" size="sm" onClick={handleAddMethod}>
              Add method
            </Button>
          </div>

          {methodsError && <p className="text-sm text-red-600">{methodsError}</p>}

          <div className="space-y-3">
            {fields.map((field, index) => {
              const selectedType = watchedMethods?.[index]?.type
              const usedTypes = new Set(
                watchedMethods
                  ?.map((method, methodIndex) =>
                    methodIndex === index
                      ? undefined
                      : method?.value?.trim()
                        ? method?.type?.trim()
                        : undefined
                  )
                  .filter(type => type)
              )
              const option = CONTACT_METHOD_OPTIONS.find(opt => opt.value === selectedType)

              return (
                <div
                  key={field.id}
                  className={`rounded-md border px-4 py-3 ${
                    watchedMethods?.[index]?.is_primary
                      ? 'border-blue-200 bg-blue-50'
                      : 'border-gray-200 bg-white'
                  }`}
                >
                  <div className="grid grid-cols-1 gap-4 md:grid-cols-[180px_1fr_110px_auto]">
                    <div>
                      <label
                        htmlFor={`methods.${index}.type`}
                        className="block text-sm font-medium text-gray-700"
                      >
                        Type
                      </label>
                      <select
                        id={`methods.${index}.type`}
                        {...register(`methods.${index}.type`)}
                        className="mt-1 block w-full rounded-md border-gray-300 text-gray-900 shadow-sm focus:border-blue-500 focus:ring-blue-500 sm:text-sm"
                        disabled={isLoading}
                      >
                        <option value="">Select type</option>
                        {CONTACT_METHOD_OPTIONS.map(option => (
                          <option
                            key={option.value}
                            value={option.value}
                            disabled={usedTypes.has(option.value)}
                          >
                            {option.label}
                          </option>
                        ))}
                      </select>
                      {errors.methods?.[index]?.type && (
                        <p className="mt-1 text-sm text-red-600">
                          {errors.methods?.[index]?.type?.message}
                        </p>
                      )}
                    </div>

                    <div>
                      <label
                        htmlFor={`methods.${index}.value`}
                        className="block text-sm font-medium text-gray-700"
                      >
                        Value
                      </label>
                      <input
                        id={`methods.${index}.value`}
                        {...register(`methods.${index}.value`)}
                        type={option?.inputType ?? 'text'}
                        placeholder={option?.placeholder ?? 'Enter value'}
                        className="mt-1 block w-full rounded-md border-gray-300 text-gray-900 shadow-sm focus:border-blue-500 focus:ring-blue-500 sm:text-sm"
                        disabled={isLoading}
                      />
                      {errors.methods?.[index]?.value && (
                        <p className="mt-1 text-sm text-red-600">
                          {errors.methods?.[index]?.value?.message}
                        </p>
                      )}
                    </div>

                    <div className="flex items-center">
                      <label className="mt-6 inline-flex items-center space-x-2 text-sm text-gray-700">
                        <input
                          type="checkbox"
                          className="h-4 w-4 rounded border-gray-300 text-blue-600 focus:ring-blue-500"
                          checked={Boolean(watchedMethods?.[index]?.is_primary)}
                          onChange={() => handlePrimaryToggle(index)}
                          disabled={isLoading}
                        />
                        <span>Primary</span>
                      </label>
                    </div>

                    <div className="flex items-center md:justify-end">
                      <Button
                        type="button"
                        variant="outline"
                        size="sm"
                        onClick={() => remove(index)}
                        disabled={isLoading || fields.length === 1}
                      >
                        Remove
                      </Button>
                    </div>
                  </div>
                </div>
              )
            })}
          </div>
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
