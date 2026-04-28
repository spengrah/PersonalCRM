export type ContactMethodType =
  | 'email'
  | 'phone'
  | 'telegram'
  | 'signal'
  | 'discord'
  | 'twitter'
  | 'gchat'
  | 'whatsapp'

export interface ContactMethod {
  id?: string
  type: ContactMethodType
  value: string
  is_primary: boolean
}

export interface Contact {
  id: string
  full_name: string
  methods?: ContactMethod[]
  primary_method?: ContactMethod
  location?: string
  birthday?: string
  cadence?: string
  last_contacted?: string
  contact_by?: string
  last_interaction_at?: string
  last_outreach_at?: string
  last_response_at?: string
  has_pending_followup?: boolean
  created_at: string
  updated_at: string
  deleted_at?: string
  /**
   * Set when create/update kicked off a rematch job to retroactively link
   * historical calendar events / telegram messages. Null when no methods
   * matched a registered handler. Polled by RematchJobsProvider.
   */
  rematch_job_id?: string | null
}

export interface OverdueContact extends Contact {
  days_overdue: number
  next_due_date: string
  suggested_action: string
}

export interface CreateContactRequest {
  full_name: string
  methods?: ContactMethod[]
  location?: string
  birthday?: string
  cadence?: string
}

export interface UpdateContactRequest {
  full_name?: string
  methods?: ContactMethod[]
  location?: string
  birthday?: string
  cadence?: string
}

export interface ContactListParams {
  page?: number
  limit?: number
  search?: string
  sort?:
    | 'name'
    | 'location'
    | 'birthday'
    | 'last_contacted'
    | 'last_response_at'
    | 'contact_by'
    | 'cadence'
  order?: 'asc' | 'desc'
  cadence_filter?: 'has_cadence' | 'no_cadence'
  followup_filter?: 'has_followup' | 'no_followup'
}
