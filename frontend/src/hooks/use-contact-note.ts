import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { notesApi } from '@/lib/notes-api'

// Query keys for notes
export const noteKeys = {
  all: ['notes'] as const,
  contactNote: (contactId: string) => [...noteKeys.all, 'contact', contactId] as const,
}

// Get the notepad note for a contact
export function useContactNote(contactId: string) {
  return useQuery({
    queryKey: noteKeys.contactNote(contactId),
    queryFn: () => notesApi.getContactNote(contactId),
    enabled: !!contactId,
  })
}

// Save the notepad note for a contact
export function useSaveContactNote() {
  const queryClient = useQueryClient()

  return useMutation({
    mutationFn: ({ contactId, body }: { contactId: string; body: string }) =>
      notesApi.saveContactNote(contactId, body),
    onSuccess: (note, { contactId }) => {
      // Update the cache with the new note (or null if deleted)
      queryClient.setQueryData(noteKeys.contactNote(contactId), note)
    },
  })
}
