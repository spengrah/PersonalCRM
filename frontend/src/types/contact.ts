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

