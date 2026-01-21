import { apiClient } from './api-client'
import type { Note, SaveNoteRequest } from '@/types/note'

export const notesApi = {
  // Get the notepad note for a contact
  // Returns null if no note exists (204 response)
  getContactNote: async (contactId: string): Promise<Note | null> => {
    const response = await apiClient.getWithStatus<Note>(`/api/v1/contacts/${contactId}/notes`)
    if (response.status === 204) {
      return null
    }
    return response.data
  },

  // Save the notepad note for a contact
  // If body is empty/whitespace, deletes the note and returns null
  // Otherwise, creates or updates the note and returns the note
  saveContactNote: async (contactId: string, body: string): Promise<Note | null> => {
    const response = await apiClient.putWithStatus<Note>(`/api/v1/contacts/${contactId}/notes`, {
      body,
    } as SaveNoteRequest)
    if (response.status === 204) {
      return null
    }
    return response.data
  },
}
