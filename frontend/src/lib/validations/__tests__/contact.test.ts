import { describe, it, expect } from 'vitest'
import { contactSchema, transformContactFormData, type ContactFormData } from '../contact'

describe('contactSchema', () => {
  describe('valid data', () => {
    it('validates contact with all fields', () => {
      const validData = {
        full_name: 'John Doe',
        email: 'john@example.com',
        phone: '555-1234',
        location: 'New York, NY',
        birthday: '1990-01-15',
        notes: 'Some notes here',
        cadence: 'weekly',
      }

      const result = contactSchema.safeParse(validData)
      expect(result.success).toBe(true)
    })

    it('validates contact with only required field (full_name)', () => {
      const validData = {
        full_name: 'Jane Smith',
      }

      const result = contactSchema.safeParse(validData)
      expect(result.success).toBe(true)
    })

    it('validates contact with some optional fields', () => {
      const validData = {
        full_name: 'Bob Johnson',
        email: 'bob@example.com',
        birthday: '1985-06-20',
      }

      const result = contactSchema.safeParse(validData)
      expect(result.success).toBe(true)
    })
  })

  describe('required field validation', () => {
    it('rejects missing full_name', () => {
      const invalidData = {
        email: 'test@example.com',
      }

      const result = contactSchema.safeParse(invalidData)
      expect(result.success).toBe(false)
    })

    it('rejects empty full_name', () => {
      const invalidData = {
        full_name: '',
      }

      const result = contactSchema.safeParse(invalidData)
      expect(result.success).toBe(false)
    })
  })

  describe('email validation', () => {
    it('accepts valid email', () => {
      const data = {
        full_name: 'Test User',
        email: 'test@example.com',
      }

      const result = contactSchema.safeParse(data)
      expect(result.success).toBe(true)
    })

    it('rejects invalid email format', () => {
      const data = {
        full_name: 'Test User',
        email: 'not-an-email',
      }

      const result = contactSchema.safeParse(data)
      expect(result.success).toBe(false)
    })

    it('accepts empty string for email (optional)', () => {
      const data = {
        full_name: 'Test User',
        email: '',
      }

      const result = contactSchema.safeParse(data)
      expect(result.success).toBe(true)
    })

    it('accepts undefined email', () => {
      const data = {
        full_name: 'Test User',
        // email is undefined
      }

      const result = contactSchema.safeParse(data)
      expect(result.success).toBe(true)
    })
  })

  describe('length constraint validation', () => {
    it('rejects full_name longer than 255 characters', () => {
      const data = {
        full_name: 'a'.repeat(256),
      }

      const result = contactSchema.safeParse(data)
      expect(result.success).toBe(false)
    })

    it('rejects phone longer than 50 characters', () => {
      const data = {
        full_name: 'Test User',
        phone: '1'.repeat(51),
      }

      const result = contactSchema.safeParse(data)
      expect(result.success).toBe(false)
    })

    it('rejects notes longer than 2000 characters', () => {
      const data = {
        full_name: 'Test User',
        notes: 'a'.repeat(2001),
      }

      const result = contactSchema.safeParse(data)
      expect(result.success).toBe(false)
    })

    it('rejects location longer than 255 characters', () => {
      const data = {
        full_name: 'Test User',
        location: 'a'.repeat(256),
      }

      const result = contactSchema.safeParse(data)
      expect(result.success).toBe(false)
    })
  })

  describe('birthday validation', () => {
    it('accepts valid past date', () => {
      const data = {
        full_name: 'Test User',
        birthday: '1990-01-15',
      }

      const result = contactSchema.safeParse(data)
      expect(result.success).toBe(true)
    })

    it("accepts today's date", () => {
      const today = new Date().toISOString().split('T')[0]
      const data = {
        full_name: 'Test User',
        birthday: today,
      }

      const result = contactSchema.safeParse(data)
      expect(result.success).toBe(true)
    })

    it('rejects future date', () => {
      const tomorrow = new Date()
      tomorrow.setDate(tomorrow.getDate() + 1)

      const data = {
        full_name: 'Test User',
        birthday: tomorrow.toISOString().split('T')[0],
      }

      const result = contactSchema.safeParse(data)
      expect(result.success).toBe(false)
    })

    it('rejects invalid date format', () => {
      const data = {
        full_name: 'Test User',
        birthday: 'not-a-date',
      }

      const result = contactSchema.safeParse(data)
      expect(result.success).toBe(false)
    })

    it('accepts empty string for birthday (optional)', () => {
      const data = {
        full_name: 'Test User',
        birthday: '',
      }

      const result = contactSchema.safeParse(data)
      expect(result.success).toBe(true)
    })
  })
})

describe('transformContactFormData', () => {
  it('converts empty email to undefined', () => {
    const input: ContactFormData = {
      full_name: 'John Doe',
      email: '',
    }

    const result = transformContactFormData(input)
    expect(result.email).toBeUndefined()
    expect(result.full_name).toBe('John Doe')
  })

  it('converts whitespace-only phone to undefined', () => {
    const input: ContactFormData = {
      full_name: 'John Doe',
      phone: '   ',
    }

    const result = transformContactFormData(input)
    expect(result.phone).toBeUndefined()
  })

  it('preserves non-empty strings', () => {
    const input: ContactFormData = {
      full_name: 'John Doe',
      email: 'john@example.com',
      phone: '555-1234',
      location: 'New York',
    }

    const result = transformContactFormData(input)
    expect(result.email).toBe('john@example.com')
    expect(result.phone).toBe('555-1234')
    expect(result.location).toBe('New York')
  })

  it('always includes required full_name field', () => {
    const input: ContactFormData = {
      full_name: 'John Doe',
    }

    const result = transformContactFormData(input)
    expect(result.full_name).toBe('John Doe')
  })

  it('converts all empty optional fields to undefined', () => {
    const input: ContactFormData = {
      full_name: 'John Doe',
      email: '',
      phone: '  ',
      location: '',
      birthday: '',
      notes: '   ',
      cadence: '',
    }

    const result = transformContactFormData(input)
    expect(result.full_name).toBe('John Doe')
    expect(result.email).toBeUndefined()
    expect(result.phone).toBeUndefined()
    expect(result.location).toBeUndefined()
    expect(result.birthday).toBeUndefined()
    expect(result.notes).toBeUndefined()
    expect(result.cadence).toBeUndefined()
  })
})
