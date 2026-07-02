import { describe, expectTypeOf, it } from 'vitest'

import type {
  Contact,
  ContactMethod,
  CreateContactRequest,
  OverdueContact,
  UpdateContactRequest,
} from '../contact'

// Type-level contract tests: these assertions are enforced by `tsc --noEmit`
// (the lint gate), locking the frontend types to the backend wire format.
// Each assertion below documents drift that existed in the hand-written
// mirrors before types were generated from the Go structs.
describe('contact wire contract', () => {
  it('carries every field the backend serializes', () => {
    // how_met and profile_photo are emitted by ContactResponse but were
    // missing from the hand-written Contact type.
    expectTypeOf<Contact>().toHaveProperty('how_met')
    expectTypeOf<Contact>().toHaveProperty('profile_photo')
  })

  it('marks always-emitted fields as required', () => {
    // has_pending_followup is a non-pointer bool without omitempty — the
    // backend always sends it.
    expectTypeOf<Contact['has_pending_followup']>().toEqualTypeOf<boolean>()
  })

  it('does not invent fields the backend never sends', () => {
    // deleted_at existed on the hand-written type but ContactResponse has no
    // such field (soft-deleted contacts are filtered server-side).
    expectTypeOf<'deleted_at' extends keyof Contact ? true : false>().toEqualTypeOf<false>()
  })

  it('keeps request payloads aligned with backend validation', () => {
    // full_name is validate:"required" on both requests; the hand-written
    // UpdateContactRequest wrongly marked it optional.
    expectTypeOf<UpdateContactRequest['full_name']>().toEqualTypeOf<string>()
    expectTypeOf<CreateContactRequest>().toHaveProperty('how_met')
    expectTypeOf<CreateContactRequest>().toHaveProperty('profile_photo')
    expectTypeOf<UpdateContactRequest>().toHaveProperty('how_met')
    expectTypeOf<UpdateContactRequest>().toHaveProperty('profile_photo')
  })

  it('keeps the overdue payload extending the contact payload', () => {
    expectTypeOf<OverdueContact>().toMatchTypeOf<Contact>()
    expectTypeOf<OverdueContact['days_overdue']>().toEqualTypeOf<number>()
  })

  it('keeps response methods assignable to the UI method type', () => {
    expectTypeOf<NonNullable<Contact['methods']>[number]>().toMatchTypeOf<ContactMethod>()
  })
})
