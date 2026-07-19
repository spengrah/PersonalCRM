import { describe, it, expect } from 'vitest'
import { submittedMethodSourceRows, transformContactFormData } from '@/lib/validations/contact'
import type { ContactFormData } from '@/lib/validations/contact'

// ContactForm and transformContactFormData are SHARED with contact creation:
// frontend/src/app/contacts/new/page.tsx passes the transformed `methods`
// straight into CreateContactRequest. "Stop emitting methods for the PUT" is an
// edit-path change only — removing methods from the shared transform would
// silently break creation, and no backend DTO test can see that.
//
// This test passes against the pre-change tree by design. Its discrimination
// gate is mutating transformContactFormData to drop methods, not reverting the
// feature.
function formData(overrides: Partial<ContactFormData> = {}): ContactFormData {
  return {
    full_name: 'New Person',
    methods: [{ type: 'email', value: 'new@example.test', is_primary: true }],
    location: '',
    birthday: '',
    notes: '',
    cadence: '',
    ...overrides,
  } as ContactFormData
}

describe('transformContactFormData on the create path', () => {
  it('still emits methods', () => {
    expect(transformContactFormData(formData()).methods).toEqual([
      { type: 'email', value: 'new@example.test', is_primary: true },
    ])
  })

  it('omits method_id entirely for rows that have none', () => {
    // A create payload must not carry the key at all — CreateContactRequest has
    // no such field, and an always-present null would be a wire contract nobody
    // asked for.
    const [method] = transformContactFormData(formData()).methods
    expect('method_id' in method).toBe(false)
  })

  it('normalizes handle values and drops blank rows', () => {
    const data = formData({
      methods: [
        { type: 'telegram', value: '  @handle ', is_primary: false },
        { type: '', value: '', is_primary: false },
        { type: 'email', value: '   ', is_primary: false },
      ],
    } as Partial<ContactFormData>)

    expect(transformContactFormData(data).methods).toEqual([
      { type: 'telegram', value: 'handle', is_primary: false },
    ])
  })

  it('reports the source row behind each emitted method', () => {
    // The mapping the form uses to write a confirmed server id back into the
    // right row. Blank rows are filtered out, so emitted index != row index.
    const data = formData({
      methods: [
        { type: '', value: '', is_primary: false },
        { type: 'email', value: 'a@example.test', is_primary: false },
        { type: 'phone', value: '', is_primary: false },
        { type: 'phone', value: '5555550100', is_primary: false },
      ],
    } as Partial<ContactFormData>)

    expect(submittedMethodSourceRows(data)).toEqual([1, 3])
    expect(transformContactFormData(data).methods).toHaveLength(2)
  })

  it('carries method_id through on the edit path', () => {
    const data = formData({
      methods: [
        {
          method_id: 'aaaaaaaa-0000-4000-8000-000000000001',
          type: 'email',
          value: 'a@example.test',
          is_primary: false,
        },
      ],
    } as Partial<ContactFormData>)

    expect(transformContactFormData(data).methods[0]).toEqual({
      method_id: 'aaaaaaaa-0000-4000-8000-000000000001',
      type: 'email',
      value: 'a@example.test',
      is_primary: false,
    })
  })
})
