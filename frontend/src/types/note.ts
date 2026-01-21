export interface Note {
  id: string
  contact_id: string
  body: string
  category?: string
  created_at: string
  updated_at: string
}

export interface SaveNoteRequest {
  body: string
}
