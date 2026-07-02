// Contact API types are generated from the backend Go wire structs
// (backend/internal/api/handlers/contact_dto.go via `make api-types`) —
// see src/types/generated/contact.ts. This module re-exports them under
// the names the app uses, plus the few frontend-only types.
import type { CadenceFilter, FollowupFilter, SortField, SortOrder } from '@/lib/contact-list-params'
import type {
  ContactMethodRequest,
  ContactResponse,
  OverdueContactResponse,
} from './generated/contact'

export type {
  ContactMethodType,
  CreateContactRequest,
  UpdateContactRequest,
} from './generated/contact'

export type Contact = ContactResponse
export type OverdueContact = OverdueContactResponse

// UI-side method shape: response methods (id required) and form-built
// methods (no id yet) both flow through this type.
export type ContactMethod = ContactMethodRequest & { id?: string }

export interface ContactListParams {
  page?: number
  limit?: number
  search?: string
  sort?: SortField
  order?: SortOrder
  cadence_filter?: CadenceFilter
  followup_filter?: FollowupFilter
}
