# Frontend Patterns

Reusable React/TypeScript patterns for consistency across the frontend codebase.

## React Query Hooks Inventory

| Hook | Domain | File |
|------|--------|------|
| `useContacts`, `useContact`, `useCreateContact`, etc. | Contacts | `use-contacts.ts` |
| `useContactNote`, `useCreateNote`, etc. | Notes | `use-contact-note.ts` |
| `useCalendarEvents`, `useCreateCalendarEvent` | Calendar | `use-calendar.ts` |
| `useImportCandidates`, `useImportContact` | Imports | `use-imports.ts` |
| `useInteractionsQueue`, `useResolveLink`, `useAnarlogTitleCandidates`, `useResolveNameCandidate` | Imports — Interactions tab + Anarlog name candidates | `use-interactions-queue.ts` |
| `useMergeContacts` | Contact merge | `use-merge.ts` |
| `useSyncStates`, `useTriggerSync` | Sync status | `use-sync-states.ts` |
| `useGoogleAccounts`, `useConnectGoogle` | Google OAuth | `use-google-accounts.ts` |
| `useTodoistAccounts`, `useConnectTodoist` | Todoist OAuth | `use-todoist-accounts.ts` |
| `useTodoistSettings`, `useUpdateTodoistSettings` | Todoist config | `use-todoist-settings.ts` |
| `useAcceleratedTime` | Time acceleration | `use-accelerated-time.ts` |
| `useKeyboardNavigation` | Keyboard nav | `use-keyboard-navigation.ts` |
| `useContactTasks`, `useCreateActionTask`, `useDeleteTaskLink` | Contact tasks | `use-contact-tasks.ts` |

All hooks are in `frontend/src/hooks/`.

---

## Parallel Data Loading Pattern

Fetch related data at the same component level to avoid waterfall loading:

```typescript
// ❌ WRONG - Child component fetches create waterfall
function ContactPage({ id }: Props) {
  const { data: contact } = useContact(id)  // Fetch 1
  return <TasksSection contactId={id} />    // Mounts after contact loads
}

function TasksSection({ contactId }: Props) {
  const { data: tasks } = useContactTasks(contactId)  // Fetch 2 (waits for mount)
  // ...
}

// ✅ CORRECT - Parent fetches all data in parallel
function ContactPage({ id }: Props) {
  const { data: contact } = useContact(id)           // Fetch 1
  const { data: tasks } = useContactTasks(id)        // Fetch 2 (parallel!)

  return <TasksSection tasks={tasks} />              // Pass as props
}
```

---

## DOM Measurement Pattern (ResizeObserver)

Use ResizeObserver for layout measurements that depend on dynamic content or run under parallel E2E load:

```typescript
// ❌ WRONG - Mount-time measurement can race with layout settling
useEffect(() => {
  if (ref.current) {
    const hasOverflow = ref.current.scrollHeight > ref.current.clientHeight
    setShowExpand(hasOverflow)
  }
}, [contactNote?.body, isExpanded]) // Won't re-run if deps don't change

// ✅ CORRECT - ResizeObserver fires whenever element size changes
useEffect(() => {
  const element = ref.current
  if (!element) return

  const observer = new ResizeObserver(() => {
    const hasOverflow = element.scrollHeight > element.clientHeight
    setShowExpand(hasOverflow)
  })

  observer.observe(element)
  return () => observer.disconnect()
}, []) // No deps - observer handles all size changes
```

**When to use:**
- Overflow detection (show more buttons, truncation affordances)
- Dynamic height calculations
- Conditional rendering based on element dimensions
- E2E tests with parallel workers (fonts/styles may settle late)

**Why it matters:**
Initial measurements in mount-time useEffect can read dimensions before fonts/styles fully settle. If dependencies don't change after mount, the measurement never re-runs, leaving stale decisions. ResizeObserver ensures late-settling layout triggers remeasurement.

---

## Loading Pattern

```typescript
function MyComponent() {
  const { data, isLoading, error } = useContacts()

  if (isLoading) {
    return (
      <div className="flex items-center justify-center p-8">
        <LoadingSpinner />
      </div>
    )
  }

  if (error) {
    return (
      <div className="bg-red-50 border border-red-200 rounded-md p-4">
        <p className="text-red-800">
          {error.message || 'Failed to load data'}
        </p>
      </div>
    )
  }

  if (!data || data.length === 0) {
    return (
      <div className="text-center text-gray-500 p-8">
        No items found
      </div>
    )
  }

  return (
    <div>
      {/* Render data */}
    </div>
  )
}
```

## Form Pattern (with Zod + React Hook Form)

```typescript
import { useForm } from 'react-hook-form'
import { zodResolver } from '@hookform/resolvers/zod'
import { z } from 'zod'

const schema = z.object({
  full_name: z.string().min(1, "Name is required"),
  email: z.string().email().optional().or(z.literal('')),
  phone: z.string().optional(),
})

type FormData = z.infer<typeof schema>

export function ContactForm({ initialData, onSuccess }: Props) {
  const {
    register,
    handleSubmit,
    formState: { errors },
    reset
  } = useForm<FormData>({
    resolver: zodResolver(schema),
    defaultValues: initialData,
  })

  const createMutation = useCreateContact()

  const onSubmit = (data: FormData) => {
    createMutation.mutate(data, {
      onSuccess: (result) => {
        reset()
        onSuccess?.(result)
      },
      onError: (error) => {
        // Handle error (show toast, etc.)
      },
    })
  }

  return (
    <form onSubmit={handleSubmit(onSubmit)} className="space-y-4">
      <div>
        <label htmlFor="full_name" className="block text-sm font-medium text-gray-700">
          Full Name *
        </label>
        <input
          {...register('full_name')}
          type="text"
          className="mt-1 block w-full rounded-md border-gray-300 shadow-sm focus:border-blue-500 focus:ring-blue-500"
        />
        {errors.full_name && (
          <p className="mt-1 text-sm text-red-600">{errors.full_name.message}</p>
        )}
      </div>

      <button
        type="submit"
        disabled={createMutation.isPending}
        className="w-full bg-blue-600 text-white px-4 py-2 rounded-md hover:bg-blue-700 disabled:opacity-50"
      >
        {createMutation.isPending ? 'Saving...' : 'Save Contact'}
      </button>
    </form>
  )
}
```

## React Query Mutation Pattern

```typescript
export function useUpdateContact() {
  const queryClient = useQueryClient()

  return useMutation({
    mutationFn: ({ id, data }: { id: string; data: UpdateContactData }) =>
      contactApi.update(id, data),
    onMutate: async (variables) => {
      // Optimistic update (optional)
      await queryClient.cancelQueries({ queryKey: ['contacts', variables.id] })

      const previousContact = queryClient.getQueryData(['contacts', variables.id])

      queryClient.setQueryData(['contacts', variables.id], (old: any) => ({
        ...old,
        ...variables.data,
      }))

      return { previousContact }
    },
    onError: (err, variables, context) => {
      // Rollback optimistic update
      if (context?.previousContact) {
        queryClient.setQueryData(['contacts', variables.id], context.previousContact)
      }
    },
    onSuccess: (data, variables) => {
      // Invalidate queries
      queryClient.invalidateQueries({ queryKey: ['contacts'] })
      queryClient.invalidateQueries({ queryKey: ['contacts', variables.id] })
    },
  })
}
```

## Centralized Query Invalidation Pattern

Use the centralized invalidation registry for all mutations. This ensures cross-domain effects are handled correctly.

**Using domain events (preferred):**
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

      // Invalidate all affected queries via domain event
      invalidateFor('contact:touched')
    },
  })
}
```

**Available domain events:**

| Event | Use When | Invalidates |
|-------|----------|-------------|
| `contact:created` | New contact added | Contact lists |
| `contact:updated` | Contact details changed | Contact lists |
| `contact:deleted` | Contact removed | Contacts + Reminders |
| `contact:touched` | Marked as contacted | Contacts + Reminders |
| `reminder:created` | New reminder | All reminders |
| `reminder:completed` | Reminder done | All reminders |
| `reminder:deleted` | Reminder removed | All reminders |

## API Module Pattern

When creating new API modules, **always use the shared `apiClient`**:

```typescript
// ✅ CORRECT - Use shared apiClient
import { apiClient } from './api-client'

export const myFeatureApi = {
  async list(): Promise<MyType[]> {
    return apiClient.get<MyType[]>('/api/v1/my-feature')
  },

  async create(data: CreateRequest): Promise<MyType> {
    return apiClient.post<MyType>('/api/v1/my-feature', data)
  },
}

// ❌ WRONG - Independent URL construction
const API_BASE = process.env.NEXT_PUBLIC_API_URL || 'http://localhost:8080/api/v1'
// This breaks when env var is set without /api/v1 suffix
```

---

## API Client Pattern

```typescript
class APIClient {
  private baseURL: string

  constructor(baseURL: string) {
    this.baseURL = baseURL
  }

  private async request<T>(
    endpoint: string,
    options: RequestInit = {}
  ): Promise<T> {
    const config: RequestInit = {
      ...options,
      headers: {
        'Content-Type': 'application/json',
        ...options.headers,
      },
    }

    const response = await fetch(`${this.baseURL}${endpoint}`, config)

    if (!response.ok) {
      const errorData = await response.json().catch(() => ({}))
      throw new Error(errorData.error || `HTTP ${response.status}`)
    }

    // Handle 204 No Content
    if (response.status === 204) {
      return undefined as T
    }

    const data = await response.json()
    return data.data || data
  }

  async get<T>(endpoint: string): Promise<T> {
    return this.request<T>(endpoint, { method: 'GET' })
  }

  async post<T>(endpoint: string, data: any): Promise<T> {
    return this.request<T>(endpoint, {
      method: 'POST',
      body: JSON.stringify(data),
    })
  }

  async put<T>(endpoint: string, data: any): Promise<T> {
    return this.request<T>(endpoint, {
      method: 'PUT',
      body: JSON.stringify(data),
    })
  }

  async delete<T = void>(endpoint: string): Promise<T> {
    return this.request<T>(endpoint, { method: 'DELETE' })
  }
}

export const apiClient = new APIClient(
  process.env.NEXT_PUBLIC_API_URL || ''
)
```

## Conditional Rendering Pattern

```typescript
// Null/undefined checks
{contact.email && (
  <a href={`mailto:${contact.email}`} className="text-blue-600">
    {contact.email}
  </a>
)}

// Optional chaining
<p>{contact.location || 'No location set'}</p>

// Multiple conditions
{contact.birthday && (
  <div className="flex items-center gap-2">
    <CalendarIcon className="w-4 h-4" />
    <span>{formatDate(contact.birthday)}</span>
  </div>
)}

// Conditional classes (with clsx)
import clsx from 'clsx'

<button
  className={clsx(
    'px-4 py-2 rounded-md',
    isActive ? 'bg-blue-600 text-white' : 'bg-gray-200 text-gray-700',
    isDisabled && 'opacity-50 cursor-not-allowed'
  )}
>
  Click me
</button>
```

## DOM Measurement Pattern with ResizeObserver

For features that measure DOM dimensions for conditional rendering (show more buttons, overflow detection, truncation), use ResizeObserver to handle layout settling under load:

```typescript
// ✅ CORRECT - ResizeObserver remeasures when layout settles
useEffect(() => {
  const element = elementRef.current
  if (!element) return

  const observer = new ResizeObserver(() => {
    const hasOverflow = element.scrollHeight > element.clientHeight
    setShowExpandButton(hasOverflow)
  })

  observer.observe(element)
  return () => observer.disconnect()
}, [contactNote?.body, isExpanded]) // Re-observe when content changes

// ❌ WRONG - Mount-time measurement misses late-settling layout
useEffect(() => {
  const element = elementRef.current
  if (!element) return

  const hasOverflow = element.scrollHeight > element.clientHeight
  setShowExpandButton(hasOverflow)
}, [contactNote?.body, isExpanded])
// Problem: Fonts/styles may not be fully applied yet, causing incorrect measurement
```

**Why ResizeObserver is needed:**
- Under parallel E2E load or slow networks, initial measurements can read dimensions before fonts/styles fully settle
- If deps don't change, the measurement never re-runs
- ResizeObserver fires whenever element size changes, catching late layout updates

---

## Date Formatting Pattern

```typescript
export function formatDate(dateString: string): string {
  const date = new Date(dateString)
  return new Intl.DateTimeFormat('en-US', {
    year: 'numeric',
    month: 'long',
    day: 'numeric',
  }).format(date)
}

export function formatRelativeTime(dateString: string): string {
  const date = new Date(dateString)
  const now = new Date()
  const diffMs = now.getTime() - date.getTime()
  const diffDays = Math.floor(diffMs / (1000 * 60 * 60 * 24))

  if (diffDays === 0) return 'Today'
  if (diffDays === 1) return 'Yesterday'
  if (diffDays < 7) return `${diffDays} days ago`
  if (diffDays < 30) return `${Math.floor(diffDays / 7)} weeks ago`
  if (diffDays < 365) return `${Math.floor(diffDays / 30)} months ago`
  return `${Math.floor(diffDays / 365)} years ago`
}

// Usage
<span>{formatDate(contact.created_at)}</span>
<span className="text-gray-500">{formatRelativeTime(contact.last_contacted)}</span>
```
