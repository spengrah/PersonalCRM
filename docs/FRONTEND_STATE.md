# Frontend State Management

This document describes the frontend's React Query architecture, focusing on query invalidation and cross-domain data consistency.

---

## Overview

The frontend uses [TanStack Query](https://tanstack.com/query) (React Query) for server state management. To ensure data consistency across related domains (contacts, reminders, time entries), we use a **centralized invalidation registry** that defines which queries should be refreshed when mutations occur.

---

## Architecture

### Query Keys

All query keys are defined in `frontend/src/lib/query-keys.ts`:

```typescript
// Contacts
contactKeys.all          // ['contacts']
contactKeys.lists()      // ['contacts', 'list']
contactKeys.list(params) // ['contacts', 'list', {...}]
contactKeys.detail(id)   // ['contacts', 'detail', id]
contactKeys.overdue()    // ['contacts', 'overdue']

// Reminders
reminderKeys.all           // ['reminders']
reminderKeys.lists()       // ['reminders', 'list']
reminderKeys.list(params)  // ['reminders', 'list', {...}]
reminderKeys.stats()       // ['reminders', 'stats']
reminderKeys.byContact(id) // ['reminders', 'contact', id]

// Time Entries
timeEntryKeys.all          // ['time-entries']
timeEntryKeys.lists()      // ['time-entries', 'list']
timeEntryKeys.detail(id)   // ['time-entries', 'detail', id]
timeEntryKeys.running()    // ['time-entries', 'running']
timeEntryKeys.stats()      // ['time-entries', 'stats']

// System
systemKeys.all    // ['system']
systemKeys.time() // ['system', 'time']
```

### Domain Events

Mutations emit domain events that trigger query invalidations. Events are defined in `frontend/src/lib/query-invalidation.ts`:

| Event | Description | Triggered By |
|-------|-------------|--------------|
| `contact:created` | New contact added | `useCreateContact` |
| `contact:updated` | Contact details changed | `useUpdateContact` |
| `contact:deleted` | Contact removed | `useDeleteContact` |
| `contact:touched` | Contact marked as contacted | `useUpdateLastContacted` |
| `reminder:created` | New reminder added | `useCreateReminder` |
| `reminder:completed` | Reminder marked done | `useCompleteReminder` |
| `reminder:deleted` | Reminder removed | `useDeleteReminder` |
| `time-entry:created` | Time entry started/added | `useCreateTimeEntry` |
| `time-entry:updated` | Time entry modified | `useUpdateTimeEntry` |
| `time-entry:deleted` | Time entry removed | `useDeleteTimeEntry` |

### Invalidation Rules

The invalidation registry maps events to affected query keys:

```typescript
const invalidationRules = {
  // Contact events
  'contact:created': [contactKeys.lists()],
  'contact:updated': [contactKeys.lists()],
  'contact:deleted': [contactKeys.lists(), reminderKeys.all],
  'contact:touched': [contactKeys.lists(), contactKeys.overdue(), reminderKeys.all],

  // Reminder events
  'reminder:created': [reminderKeys.all],
  'reminder:completed': [reminderKeys.all],
  'reminder:deleted': [reminderKeys.all],

  // Time entry events
  'time-entry:created': [timeEntryKeys.lists(), timeEntryKeys.running(), timeEntryKeys.stats()],
  'time-entry:updated': [timeEntryKeys.lists(), timeEntryKeys.running(), timeEntryKeys.stats()],
  'time-entry:deleted': [timeEntryKeys.lists(), timeEntryKeys.running(), timeEntryKeys.stats()],
}
```

### Cross-Domain Effects

Some operations have effects that span multiple domains:

| Action | Frontend Effect | Backend Effect |
|--------|-----------------|----------------|
| Mark contact as contacted | Invalidates contacts + reminders | Auto-completes pending reminders for that contact |
| Delete contact | Invalidates contacts + reminders | Soft-deletes all reminders for that contact |

This is why `contact:touched` and `contact:deleted` invalidate `reminderKeys.all` - the backend modifies reminder state as a side effect.

---

## Usage

### In Mutation Handlers

```typescript
import { invalidateFor } from '@/lib/query-invalidation'
import { contactKeys } from '@/lib/query-keys'

export function useUpdateLastContacted() {
  const queryClient = useQueryClient()

  return useMutation({
    mutationFn: (id: string) => contactsApi.updateLastContacted(id),
    onSuccess: updatedContact => {
      // Optimistic update for the specific contact
      queryClient.setQueryData(contactKeys.detail(updatedContact.id), updatedContact)

      // Invalidate all affected queries (contacts + reminders)
      invalidateFor('contact:touched')
    },
  })
}
```

### Adding a New Mutation

1. **Identify the domain event** - What action is being performed?
2. **Check existing events** - Does an event already cover this case?
3. **Add to invalidation rules** if needed:

```typescript
// In query-invalidation.ts
const invalidationRules = {
  // ... existing rules
  'contact:archived': [contactKeys.lists(), contactKeys.overdue()],
}
```

4. **Use in mutation**:

```typescript
onSuccess: () => {
  invalidateFor('contact:archived')
}
```

---

## Query Configuration

Default settings in `frontend/src/lib/query-client.ts`:

| Setting | Value | Purpose |
|---------|-------|---------|
| `staleTime` | 5 minutes | How long data is considered fresh |
| `gcTime` | 10 minutes | How long unused data stays in cache |
| `retry` | 3 attempts | Retries on failure (except 4xx errors) |

Individual queries can override these:

```typescript
useQuery({
  queryKey: reminderKeys.list(params),
  queryFn: () => remindersApi.getReminders(params),
  staleTime: 1000 * 60 * 1, // 1 minute (more frequent updates)
})
```

---

## Best Practices

1. **Use `invalidateFor(event)`** instead of direct `queryClient.invalidateQueries()` calls
2. **Keep query keys in `query-keys.ts`** - Single source of truth
3. **Document cross-domain effects** when adding new backend behavior
4. **Use `setQueryData` for optimistic updates** before `invalidateFor`
5. **Add `refetchOnWindowFocus: true`** for dashboard-critical queries

---

## Debugging

### React Query DevTools

Enable in development by clicking the flower icon in the bottom-right corner. Shows:
- All cached queries and their state
- When queries were last fetched
- What data is cached

### Common Issues

**Query not updating after mutation:**
- Check that `invalidateFor` is called with the correct event
- Verify the event includes the affected query key in `invalidationRules`

**Too many refetches:**
- Check `staleTime` settings
- Ensure you're not invalidating too broadly

**Stale data across pages:**
- Check for missing cross-domain invalidations
- Add the missing query key to the relevant event in `invalidationRules`
