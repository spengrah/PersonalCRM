import { describe, it, expect } from 'vitest'
import { contactSchema, transformContactFormData, type ContactFormData } from '../contact'

describe('contactSchema', () => {
  describe('valid data', () => {
    it('validates contact with all fields', () => {
      const validData = {
        full_name: 'John Doe',
        methods: [
          { type: 'email_personal', value: 'john@example.com', is_primary: true },
          { type: 'phone', value: '555-1234', is_primary: false },
        ],
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
        methods: [{ type: 'email_personal', value: 'bob@example.com', is_primary: true }],
        birthday: '1985-06-20',
      }

      const result = contactSchema.safeParse(validData)
      expect(result.success).toBe(true)
    })
  })

  describe('required field validation', () => {
    it('rejects missing full_name', () => {
      const invalidData = {
        methods: [{ type: 'email_personal', value: 'test@example.com', is_primary: true }],
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

  describe('contact method validation', () => {
    it('accepts valid email method', () => {
      const data = {
        full_name: 'Test User',
        methods: [{ type: 'email_personal', value: 'test@example.com', is_primary: true }],
      }

      const result = contactSchema.safeParse(data)
      expect(result.success).toBe(true)
    })

    it('rejects invalid email format', () => {
      const data = {
        full_name: 'Test User',
        methods: [{ type: 'email_personal', value: 'not-an-email', is_primary: true }],
      }

      const result = contactSchema.safeParse(data)
      expect(result.success).toBe(false)
    })

    it('rejects duplicate method types', () => {
      const data = {
        full_name: 'Test User',
        methods: [
          { type: 'email_personal', value: 'test@example.com', is_primary: true },
          { type: 'email_personal', value: 'another@example.com', is_primary: false },
        ],
      }

      const result = contactSchema.safeParse(data)
      expect(result.success).toBe(false)
    })

    it('rejects multiple primary methods', () => {
      const data = {
        full_name: 'Test User',
        methods: [
          { type: 'email_personal', value: 'test@example.com', is_primary: true },
          { type: 'phone', value: '555-1234', is_primary: true },
        ],
      }

      const result = contactSchema.safeParse(data)
      expect(result.success).toBe(false)
    })

    it('rejects value without type', () => {
      const data = {
        full_name: 'Test User',
        methods: [{ type: '', value: 'test@example.com', is_primary: false }],
      }

      const result = contactSchema.safeParse(data)
      expect(result.success).toBe(false)
    })

    it('rejects phone longer than 50 characters', () => {
      const data = {
        full_name: 'Test User',
        methods: [{ type: 'phone', value: '1'.repeat(51), is_primary: false }],
      }

      const result = contactSchema.safeParse(data)
      expect(result.success).toBe(false)
    })

    it('accepts valid whatsapp method', () => {
      const data = {
        full_name: 'Test User',
        methods: [{ type: 'whatsapp', value: '+1-555-123-4567', is_primary: true }],
      }

      const result = contactSchema.safeParse(data)
      expect(result.success).toBe(true)
    })

    it('rejects whatsapp longer than 50 characters', () => {
      const data = {
        full_name: 'Test User',
        methods: [{ type: 'whatsapp', value: '1'.repeat(51), is_primary: false }],
      }

      const result = contactSchema.safeParse(data)
      expect(result.success).toBe(false)
    })

    it('allows empty method rows', () => {
      const data = {
        full_name: 'Test User',
        methods: [{ type: '', value: '', is_primary: false }],
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
  it('drops empty method rows', () => {
    const input: ContactFormData = {
      full_name: 'John Doe',
      methods: [{ type: '', value: '  ', is_primary: false }],
    }

    const result = transformContactFormData(input)
    expect(result.methods).toEqual([])
  })

  it('normalizes handle values', () => {
    const input: ContactFormData = {
      full_name: 'John Doe',
      methods: [{ type: 'twitter', value: ' @handle ', is_primary: true }],
    }

    const result = transformContactFormData(input)
    expect(result.methods).toEqual([{ type: 'twitter', value: 'handle', is_primary: true }])
  })

  it('preserves non-empty strings', () => {
    const input: ContactFormData = {
      full_name: 'John Doe',
      methods: [
        { type: 'email_personal', value: 'john@example.com', is_primary: true },
        { type: 'phone', value: '555-1234', is_primary: false },
      ],
      location: 'New York',
    }

    const result = transformContactFormData(input)
    expect(result.methods).toEqual([
      { type: 'email_personal', value: 'john@example.com', is_primary: true },
      { type: 'phone', value: '555-1234', is_primary: false },
    ])
    expect(result.location).toBe('New York')
  })

  it('always includes required full_name field', () => {
    const input: ContactFormData = {
      full_name: 'John Doe',
      methods: [],
    }

    const result = transformContactFormData(input)
    expect(result.full_name).toBe('John Doe')
  })

  it('converts empty optional fields to undefined', () => {
    const input: ContactFormData = {
      full_name: 'John Doe',
      methods: [],
      location: '',
      birthday: '',
      notes: '   ',
      cadence: '',
    }

    const result = transformContactFormData(input)
    expect(result.full_name).toBe('John Doe')
    expect(result.location).toBeUndefined()
    expect(result.birthday).toBeUndefined()
    expect(result.notes).toBeUndefined()
    expect(result.cadence).toBeUndefined()
  })
})
