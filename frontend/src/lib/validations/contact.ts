import { z } from 'zod'
import {
  CONTACT_METHOD_TYPE_VALUES,
  isEmailMethod,
  normalizeContactMethodValue,
} from '@/lib/contact-methods'
import type { ContactMethodType } from '@/types/contact'

const contactMethodSchema = z.object({
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

    const seenTypes = new Set<string>()
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

      if (seenTypes.has(rawType)) {
        ctx.addIssue({
          code: z.ZodIssueCode.custom,
          message: 'Each method type can only be used once',
          path: ['methods', index, 'type'],
        })
      } else {
        seenTypes.add(rawType)
      }

      if (method.is_primary) {
        primaryCount += 1
      }

      const normalizedValue = normalizeContactMethodValue(rawType as ContactMethodType, rawValue)
      if (normalizedValue === '') {
        ctx.addIssue({
          code: z.ZodIssueCode.custom,
          message: 'Enter a value',
          path: ['methods', index, 'value'],
        })
        return
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

      if (rawType === 'phone' || rawType === 'signal') {
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

export type ContactFormData = z.infer<typeof contactSchema>

// Transform form data to API format (convert empty strings to undefined)
export function transformContactFormData(data: ContactFormData) {
  const normalizedMethods = (data.methods ?? [])
    .map(method => {
      const type = method.type?.trim() as ContactMethodType | ''
      const rawValue = method.value ?? ''
      if (!type) {
        return null
      }
      const normalizedValue = normalizeContactMethodValue(type, rawValue)
      if (normalizedValue === '') {
        return null
      }
      return {
        type,
        value: normalizedValue,
        is_primary: Boolean(method.is_primary),
      }
    })
    .filter((method): method is NonNullable<typeof method> => method !== null)

  return {
    full_name: data.full_name,
    methods: normalizedMethods,
    location: data.location && data.location.trim() !== '' ? data.location : undefined,
    birthday: data.birthday && data.birthday.trim() !== '' ? data.birthday : undefined,
    notes: data.notes && data.notes.trim() !== '' ? data.notes : undefined,
    cadence: data.cadence && data.cadence.trim() !== '' ? data.cadence : undefined,
  }
}
