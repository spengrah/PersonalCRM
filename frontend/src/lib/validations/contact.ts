import * as z from 'zod'
import {
  CONTACT_METHOD_TYPE_VALUES,
  isEmailMethod,
  normalizeContactMethodValue,
  normalizeContactMethodValueForComparison,
} from '@/lib/contact-methods'
import type { ContactMethodType } from '@/types/contact'

const contactMethodSchema = z.object({
  // The server's id for this row, carried through the form so an edit can name
  // the row it is changing. Deliberately `method_id` and never `id`:
  // `useFieldArray` is called without a custom `keyName` and react-hook-form
  // OVERWRITES `fields[].id` with its own generated key, so a server id
  // threaded as `id` is silently replaced — and only becomes visible once it is
  // submitted, naming a method that does not exist. Never rendered.
  method_id: z.string().optional(),
  type: z.string().optional().or(z.literal('')),
  value: z.string().optional().or(z.literal('')),
  is_primary: z.boolean().optional().default(false),
})

export const contactSchema = z
  .object({
    full_name: z
      .string()
      .min(1, 'Full name is required')
      .max(255, 'Full name must be less than 255 characters'),
    methods: z.array(contactMethodSchema).optional(),
    location: z
      .string()
      .max(255, 'Location must be less than 255 characters')
      .optional()
      .or(z.literal('')),
    birthday: z
      .string()
      .refine(date => {
        if (!date) return true // Allow empty birthday
        const parsedDate = new Date(date)
        return !isNaN(parsedDate.getTime()) && parsedDate <= new Date()
      }, 'Please enter a valid date that is not in the future')
      .optional()
      .or(z.literal('')),
    notes: z
      .string()
      .max(2000, 'Notes must be less than 2000 characters')
      .optional()
      .or(z.literal('')),
    cadence: z
      .string()
      .max(50, 'Cadence must be less than 50 characters')
      .optional()
      .or(z.literal('')),
  })
  .superRefine((data, ctx) => {
    const methods = data.methods ?? []
    if (methods.length === 0) {
      return
    }

    const seenNormalized = new Set<string>()
    let primaryCount = 0

    methods.forEach((method, index) => {
      const rawType = method.type?.trim() ?? ''
      const rawValue = method.value?.trim() ?? ''
      if (rawValue === '') {
        return
      }

      if (rawType === '') {
        ctx.addIssue({
          code: z.ZodIssueCode.custom,
          message: 'Select a method type',
          path: ['methods', index, 'type'],
        })
        return
      }

      if (!CONTACT_METHOD_TYPE_VALUES.includes(rawType as ContactMethodType)) {
        ctx.addIssue({
          code: z.ZodIssueCode.custom,
          message: 'Invalid method type',
          path: ['methods', index, 'type'],
        })
        return
      }

      if (method.is_primary) {
        primaryCount += 1
      }

      const normalizedValue = normalizeContactMethodValueForComparison(
        rawType as ContactMethodType,
        rawValue
      )
      if (normalizedValue === '') {
        ctx.addIssue({
          code: z.ZodIssueCode.custom,
          message: 'Enter a value',
          path: ['methods', index, 'value'],
        })
        return
      }

      const normalizedKey = `${rawType}:${normalizedValue}`
      if (seenNormalized.has(normalizedKey)) {
        ctx.addIssue({
          code: z.ZodIssueCode.custom,
          message: 'This method value already exists for that type',
          path: ['methods', index, 'value'],
        })
      } else {
        seenNormalized.add(normalizedKey)
      }

      if (isEmailMethod(rawType as ContactMethodType)) {
        const emailCheck = z.string().email().safeParse(normalizedValue)
        if (!emailCheck.success) {
          ctx.addIssue({
            code: z.ZodIssueCode.custom,
            message: 'Enter a valid email address',
            path: ['methods', index, 'value'],
          })
        }
      }

      if (rawType === 'phone' || rawType === 'signal' || rawType === 'whatsapp') {
        if (normalizedValue.length > 50) {
          ctx.addIssue({
            code: z.ZodIssueCode.custom,
            message: 'Phone numbers must be less than 50 characters',
            path: ['methods', index, 'value'],
          })
        }
      }
    })

    if (primaryCount > 1) {
      ctx.addIssue({
        code: z.ZodIssueCode.custom,
        message: 'Only one contact method can be primary',
        path: ['methods'],
      })
    }
  })

// Raw form values (schema input): is_primary may be absent before zod applies its default
export type ContactFormInput = z.input<typeof contactSchema>
// Validated form values (schema output)
export type ContactFormData = z.output<typeof contactSchema>

// Emits one entry per form row that survives normalization, remembering which
// row each came from. The mapping is the single source of truth for "which form
// row produced this submitted method" — the form needs it to write a confirmed
// server id back into the right row, and recomputing it with a second copy of
// the filter rule is how the two would drift apart.
function emitMethodRows(methods: ContactFormData['methods']) {
  const emitted: Array<{
    method_id?: string
    type: ContactMethodType
    value: string
    is_primary: boolean
  }> = []
  const sourceRowIndexes: number[] = []

  ;(methods ?? []).forEach((method, sourceRowIndex) => {
    const type = method.type?.trim() as ContactMethodType | ''
    if (!type) {
      return
    }
    const normalizedValue = normalizeContactMethodValue(type, method.value ?? '')
    if (normalizedValue === '') {
      return
    }
    emitted.push({
      ...(method.method_id ? { method_id: method.method_id } : {}),
      type,
      value: normalizedValue,
      is_primary: Boolean(method.is_primary),
    })
    sourceRowIndexes.push(sourceRowIndex)
  })

  return { methods: emitted, sourceRowIndexes }
}

/**
 * The form-row index behind each submitted method, parallel to
 * `transformContactFormData(data).methods`.
 */
export function submittedMethodSourceRows(data: ContactFormData): number[] {
  return emitMethodRows(data.methods).sourceRowIndexes
}

// Transform form data to API format (convert empty strings to undefined)
export function transformContactFormData(data: ContactFormData) {
  const normalizedMethods = emitMethodRows(data.methods).methods

  return {
    full_name: data.full_name,
    methods: normalizedMethods,
    location: data.location && data.location.trim() !== '' ? data.location : undefined,
    birthday: data.birthday && data.birthday.trim() !== '' ? data.birthday : undefined,
    cadence: data.cadence && data.cadence.trim() !== '' ? data.cadence : undefined,
    // Notes are included for extraction but saved via separate notes API
    notes: data.notes && data.notes.trim() !== '' ? data.notes : undefined,
  }
}

// Payload the form submits after normalization/filtering — methods are guaranteed
// to have a valid type, non-empty value, and explicit is_primary
export type ContactSubmitData = ReturnType<typeof transformContactFormData>
