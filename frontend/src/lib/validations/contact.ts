import { z } from 'zod'

export const contactSchema = z.object({
  full_name: z.string()
    .min(1, 'Full name is required')
    .max(255, 'Full name must be less than 255 characters'),
  email: z.string()
    .email('Please enter a valid email address')
    .optional()
    .or(z.literal('')),
  phone: z.string()
    .max(50, 'Phone number must be less than 50 characters')
    .optional()
    .or(z.literal('')),
  location: z.string()
    .max(255, 'Location must be less than 255 characters')
    .optional()
    .or(z.literal('')),
  notes: z.string()
    .max(2000, 'Notes must be less than 2000 characters')
    .optional()
    .or(z.literal('')),
  cadence: z.string()
    .max(50, 'Cadence must be less than 50 characters')
    .optional()
    .or(z.literal('')),
})

export type ContactFormData = z.infer<typeof contactSchema>

// Transform form data to API format (convert empty strings to undefined)
export function transformContactFormData(data: ContactFormData) {
  return {
    full_name: data.full_name,
    email: data.email || undefined,
    phone: data.phone || undefined,
    location: data.location || undefined,
    notes: data.notes || undefined,
    cadence: data.cadence || undefined,
  }
}

