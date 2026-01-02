export type ContactMethodType =
  | 'email_personal'
  | 'email_work'
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
  notes?: string
  cadence?: string
  last_contacted?: string
  created_at: string
  updated_at: string
  deleted_at?: string
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
  notes?: string
  cadence?: string
}

export interface UpdateContactRequest {
  full_name?: string
  methods?: ContactMethod[]
  location?: string
  birthday?: string
  notes?: string
  cadence?: string
}

export interface ContactListParams {
  page?: number
  limit?: number
  search?: string
  sort?: 'name' | 'location' | 'birthday' | 'last_contacted'
  order?: 'asc' | 'desc'
}
