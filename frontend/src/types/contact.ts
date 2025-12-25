export interface Contact {
  id: string
  full_name: string
  email?: string
  phone?: string
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
  email?: string
  phone?: string
  location?: string
  birthday?: string
  notes?: string
  cadence?: string
}

export interface UpdateContactRequest {
  full_name?: string
  email?: string
  phone?: string
  location?: string
  birthday?: string
  notes?: string
  cadence?: string
}

export interface ContactListParams {
  page?: number
  limit?: number
  search?: string
  sort?: 'name' | 'email' | 'created_at' | 'last_contacted'
  order?: 'asc' | 'desc'
}
