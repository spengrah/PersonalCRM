'use client'

import { useEffect, useRef, type MutableRefObject } from 'react'
import { useForm, useFieldArray } from 'react-hook-form'
import { zodResolver } from '@hookform/resolvers/zod'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Select } from '@/components/ui/select'
import { Textarea } from '@/components/ui/textarea'
import { FORM_CONTROL_BASE } from '@/lib/form-classes'
import {
  contactSchema,
  submittedMethodSourceRows,
  transformContactFormData,
  type ContactFormData,
  type ContactFormInput,
  type ContactSubmitData,
} from '@/lib/validations/contact'
import type { MethodReconciliation } from '@/lib/contact-method-operations'
import {
  CONTACT_METHOD_OPTIONS,
  formatContactMethodValue,
  sortContactMethods,
} from '@/lib/contact-methods'
import type { Contact } from '@/types/contact'
import { clsx } from 'clsx'

/**
 * Writes confirmed server results back into the live form rows. Programmatic —
 * it must never count as a user edit, which would invalidate the save session
 * it just completed and could loop.
 */
export type MethodsReconciler = (reconciliations: MethodReconciliation[]) => void

interface ContactFormProps {
  contact?: Contact
  initialNotes?: string
  onSubmit: (data: ContactSubmitData) => void | Promise<void>
  loading?: boolean
  submitText?: string
  /** Populated by the form with a reconciler the page calls after a successful methods step. */
  reconcilerRef?: MutableRefObject<MethodsReconciler | null>
  /** Fired on a user edit, so the page can invalidate an in-progress save session. */
  onFormEdit?: () => void
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
  initialNotes = '',
  onSubmit,
  loading,
  submitText = 'Save Contact',
  reconcilerRef,
  onFormEdit,
}: ContactFormProps) {
  const defaultMethods = contact?.methods?.length
    ? sortContactMethods(contact.methods).map(method => ({
        method_id: method.id,
        type: method.type,
        value: formatContactMethodValue(method.type, method.value),
        is_primary: method.is_primary,
      }))
    : [{ method_id: undefined, type: '', value: '', is_primary: false }]

  const {
    register,
    handleSubmit,
    formState: { errors, isSubmitting },
    reset,
    control,
    setValue,
    getValues,
    watch,
  } = useForm<ContactFormInput, unknown, ContactFormData>({
    resolver: zodResolver(contactSchema),
    defaultValues: contact
      ? {
          full_name: contact.full_name,
          methods: defaultMethods,
          location: contact.location || '',
          birthday: contact.birthday ? contact.birthday.split('T')[0] : '', // Format date for input
          notes: initialNotes,
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

  // Programmatic writes must not read as user edits. An explicit flag is the
  // only reliable discriminator: react-hook-form reports setValue with the same
  // `change` event type as a real input event (verified against 7.62), so the
  // subscription metadata cannot tell the two apart.
  const programmaticWriteRef = useRef(false)
  const withoutReportingEdit = (write: () => void) => {
    programmaticWriteRef.current = true
    try {
      write()
    } finally {
      programmaticWriteRef.current = false
    }
  }

  // Sync notes field when initialNotes changes (e.g., when async query resolves)
  // This prevents stale empty notes from being saved if edit mode is opened before query resolves
  useEffect(() => {
    programmaticWriteRef.current = true
    try {
      setValue('notes', initialNotes, { shouldDirty: false })
    } finally {
      programmaticWriteRef.current = false
    }
  }, [initialNotes, setValue])

  const onFormEditRef = useRef(onFormEdit)
  useEffect(() => {
    onFormEditRef.current = onFormEdit
  })

  // Report user edits so the page can invalidate an in-progress save session.
  // Array-shape edits — adding, removing, or re-designating a row — also report
  // explicitly at their call sites, since a field-array mutation is not
  // guaranteed to surface here. A duplicate report is harmless: invalidation is
  // idempotent.
  useEffect(() => {
    const subscription = watch(() => {
      if (programmaticWriteRef.current) return
      onFormEditRef.current?.()
    })
    return () => subscription.unsubscribe()
  }, [watch])

  // The form-row key behind each method of the most recent submission, so a
  // result can be written back into the row that produced it. Keys rather than
  // indexes: react-hook-form's generated key survives rows being added or
  // removed while the request is in flight.
  const submittedRowKeysRef = useRef<Array<string | null>>([])

  useEffect(() => {
    if (!reconcilerRef) return
    reconcilerRef.current = (reconciliations: MethodReconciliation[]) =>
      withoutReportingEdit(() => reconcileRows(reconciliations))

    function reconcileRows(reconciliations: MethodReconciliation[]) {
      const rowKeys = submittedRowKeysRef.current

      // What each result assigned, keyed by the form row that produced it.
      const assignedIdByRowKey = new Map<string, string>()
      const snapshotById = new Map<string, MethodReconciliation>()
      for (const entry of reconciliations) {
        const rowKey = rowKeys[entry.submittedIndex]
        if (!rowKey) continue
        assignedIdByRowKey.set(rowKey, entry.method_id)
        snapshotById.set(entry.method_id, entry)
      }

      // Ownership is seeded from EVERY live row's id, not only from the rows a
      // result addressed. In the real collision sequence the pre-existing row
      // is unchanged, so it emits no operation and receives no result — but it
      // already owns the id the new row just resolved to. Considering only
      // reconciled rows leaves both rows carrying that id, and then deleting
      // either one emits no `remove` (the other alias still submits the id),
      // so the save reports success while the deletion silently does not
      // persist. That is the original defect's own signature.
      //
      // Read through getValues rather than `fields`: useFieldArray refreshes
      // `fields` only on array operations, so an id written by an earlier
      // reconciliation's setValue would not be visible there.
      const currentMethods = getValues('methods') ?? []
      const rowsById = new Map<string, number[]>()
      fields.forEach((field, index) => {
        const methodID = assignedIdByRowKey.get(field.id) ?? currentMethods[index]?.method_id
        if (!methodID) return
        const rows = rowsById.get(methodID) ?? []
        rows.push(index)
        rowsById.set(methodID, rows)
      })

      const options = { shouldDirty: false, shouldTouch: false } as const
      const duplicateIndexes: number[] = []
      let promotedIndex: number | null = null

      for (const [methodID, indexes] of rowsById) {
        // Deterministic survivor: the EARLIEST form row carrying this id,
        // whichever row a result happened to address. Ordering by form position
        // rather than by payload position is what makes the outcome
        // independent of the order operations were submitted in.
        const [survivor, ...duplicates] = indexes
        duplicateIndexes.push(...duplicates)

        // A row whose id no result addressed is left alone — there is nothing
        // authoritative to write into it.
        const snapshot = snapshotById.get(methodID)
        if (!snapshot) continue

        setValue(`methods.${survivor}.method_id`, methodID, options)
        setValue(`methods.${survivor}.type`, snapshot.type, options)
        setValue(
          `methods.${survivor}.value`,
          formatContactMethodValue(snapshot.type, snapshot.value),
          options
        )
        setValue(`methods.${survivor}.is_primary`, snapshot.is_primary, options)
        if (snapshot.is_primary) promotedIndex = survivor
      }

      // A snapshot reporting is_primary demotes every other live row.
      //
      // The promotion can come from the SERVER rather than from the user's
      // designation: an add can resolve to a row another writer made primary
      // after edit-start, so the row this reconciliation just starred is one
      // the form never showed as primary, while the row the form DOES show
      // starred was never addressed. Leaving both set contradicts the server,
      // and — because the form rejects more than one primary — it also blocks
      // the next save outright, including an unedited retry after the methods
      // step already landed.
      // Enumerated over the getValues snapshot rather than `fields` for the
      // same reason the ownership map is: `fields` refreshes only on array
      // operations, so a flag an earlier reconciliation wrote through setValue
      // is not visible there. The write is unconditional rather than guarded on
      // the current flag, so a stale read cannot leave a row starred.
      if (promotedIndex !== null) {
        currentMethods.forEach((_method, index) => {
          if (index === promotedIndex || duplicateIndexes.includes(index)) return
          setValue(`methods.${index}.is_primary`, false, options)
        })
      }

      // Descending, so each index is still valid when its turn comes.
      for (const index of duplicateIndexes.sort((a, b) => b - a)) {
        remove(index)
      }
    }
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
    onFormEditRef.current?.()
  }

  const handleAddMethod = () => {
    append({ method_id: undefined, type: '', value: '', is_primary: false })
    onFormEditRef.current?.()
  }

  const handleRemoveMethod = (index: number) => {
    remove(index)
    onFormEditRef.current?.()
  }

  const handleFormSubmit = async (data: ContactFormData) => {
    try {
      const transformedData = transformContactFormData(data)
      submittedRowKeysRef.current = submittedMethodSourceRows(data).map(
        rowIndex => fields[rowIndex]?.id ?? null
      )
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

        <div className="space-y-4">
          <div>
            <label className="block text-sm font-medium text-gray-700 mb-1">Contact methods</label>
            <p className="text-sm text-gray-500">Add one or more ways to reach this contact.</p>
          </div>

          {methodsError && <p className="text-sm text-red-600">{methodsError}</p>}

          <div className="divide-y divide-gray-200">
            {fields.map((field, index) => {
              const selectedType = watchedMethods?.[index]?.type
              const option = CONTACT_METHOD_OPTIONS.find(opt => opt.value === selectedType)
              const isPrimary = Boolean(watchedMethods?.[index]?.is_primary)

              return (
                <div key={field.id} className="group py-3 first:pt-0">
                  <div className="flex items-center gap-3">
                    {/* Type selector - styled as rounded rectangle tag */}
                    <div className="relative">
                      <Select
                        id={`methods.${index}.type`}
                        aria-label="Contact method type"
                        {...register(`methods.${index}.type`)}
                        className={clsx(
                          'font-medium',
                          isPrimary ? 'bg-blue-100 text-blue-700' : 'bg-gray-100 text-gray-700'
                        )}
                        caretClassName={isPrimary ? 'text-blue-500' : 'text-gray-500'}
                        disabled={isLoading}
                      >
                        <option value="">Select</option>
                        {CONTACT_METHOD_OPTIONS.map(opt => (
                          <option key={opt.value} value={opt.value}>
                            {opt.label}
                          </option>
                        ))}
                      </Select>
                    </div>

                    {/* Value input */}
                    <input
                      id={`methods.${index}.value`}
                      aria-label="Contact method value"
                      {...register(`methods.${index}.value`)}
                      type={option?.inputType ?? 'text'}
                      placeholder={option?.placeholder ?? 'Enter value'}
                      className={clsx(FORM_CONTROL_BASE, 'flex-1')}
                      disabled={isLoading}
                    />

                    {/* Primary toggle - star icon */}
                    <button
                      type="button"
                      onClick={() => handlePrimaryToggle(index)}
                      disabled={isLoading}
                      className={`p-1.5 transition-colors ${
                        isPrimary ? 'text-yellow-500' : 'text-gray-300 hover:text-yellow-500'
                      }`}
                      title={isPrimary ? 'Primary contact method' : 'Set as primary'}
                    >
                      <svg className="w-4 h-4" fill="currentColor" viewBox="0 0 20 20">
                        <path d="M9.049 2.927c.3-.921 1.603-.921 1.902 0l1.07 3.292a1 1 0 00.95.69h3.462c.969 0 1.371 1.24.588 1.81l-2.8 2.034a1 1 0 00-.364 1.118l1.07 3.292c.3.921-.755 1.688-1.54 1.118l-2.8-2.034a1 1 0 00-1.175 0l-2.8 2.034c-.784.57-1.838-.197-1.539-1.118l1.07-3.292a1 1 0 00-.364-1.118L2.98 8.72c-.783-.57-.38-1.81.588-1.81h3.461a1 1 0 00.951-.69l1.07-3.292z" />
                      </svg>
                    </button>

                    {/* Remove button - X icon, visible on hover */}
                    <button
                      type="button"
                      onClick={() => handleRemoveMethod(index)}
                      disabled={isLoading || fields.length === 1}
                      className="p-1.5 text-gray-300 opacity-100 sm:opacity-0 sm:group-hover:opacity-100 hover:text-red-500 transition-all disabled:opacity-0"
                      title="Remove"
                    >
                      <svg
                        className="w-4 h-4"
                        fill="none"
                        stroke="currentColor"
                        viewBox="0 0 24 24"
                      >
                        <path
                          strokeLinecap="round"
                          strokeLinejoin="round"
                          strokeWidth={2}
                          d="M6 18L18 6M6 6l12 12"
                        />
                      </svg>
                    </button>
                  </div>

                  {/* Error messages */}
                  {(errors.methods?.[index]?.type || errors.methods?.[index]?.value) && (
                    <div className="mt-1 text-sm text-red-600">
                      {errors.methods?.[index]?.type?.message ||
                        errors.methods?.[index]?.value?.message}
                    </div>
                  )}
                </div>
              )
            })}
          </div>

          {/* Add method - text link at bottom */}
          <button
            type="button"
            onClick={handleAddMethod}
            disabled={isLoading}
            className="text-sm text-blue-600 hover:text-blue-700 flex items-center gap-1 disabled:opacity-50"
          >
            <svg className="w-4 h-4" fill="none" stroke="currentColor" viewBox="0 0 24 24">
              <path
                strokeLinecap="round"
                strokeLinejoin="round"
                strokeWidth={2}
                d="M12 4v16m8-8H4"
              />
            </svg>
            Add method
          </button>
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
            helpText="Optional - for birthday tracking"
          />
        </div>

        {/* Cadence */}
        <Select
          {...register('cadence')}
          id="cadence"
          label="Contact Cadence"
          error={errors.cadence?.message}
          helpText="How often you want to be reminded to reach out"
          disabled={isLoading}
        >
          {cadenceOptions.map(option => (
            <option key={option.value} value={option.value}>
              {option.label}
            </option>
          ))}
        </Select>

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
