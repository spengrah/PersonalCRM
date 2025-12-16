# Common Code Patterns

Reusable patterns for consistency across the codebase.

---

## Backend Patterns

### Error Response Pattern

Use the standardized API response helpers in all handlers:

```go
// Success
api.SendSuccess(c, http.StatusOK, data, nil)

// Success with metadata (pagination)
api.SendSuccess(c, http.StatusOK, data, &api.Meta{
    Pagination: &api.PaginationMeta{
        Page:  1,
        Limit: 20,
        Total: 100,
    },
})

// Validation error
api.SendValidationError(c, "Invalid input", err.Error())

// Not found
api.SendNotFound(c, "Contact")

// Internal error
api.SendInternalError(c, "Failed to process request")

// Conflict (duplicate)
api.SendConflict(c, "Email already exists")
```

### Repository Conversion Pattern

Convert between sqlc-generated DB types and clean domain types:

```go
// Domain model (no pgtype, clean types)
type Contact struct {
    ID            uuid.UUID
    FullName      string
    Email         *string  // nullable
    LastContacted *time.Time
    CreatedAt     time.Time
    UpdatedAt     time.Time
}

// Convert DB type to domain type
func convertDbContact(dbContact *db.Contact) Contact {
    contact := Contact{
        ID:       uuid.UUID(dbContact.ID.Bytes),
        FullName: dbContact.FullName,
    }
    
    // Handle nullable string - copy value before taking address
    if dbContact.Email.Valid {
        emailStr := dbContact.Email.String
        contact.Email = &emailStr
    }
    
    // Handle nullable time - copy value before taking address
    if dbContact.LastContacted.Valid {
        lastContactedTime := dbContact.LastContacted.Time
        contact.LastContacted = &lastContactedTime
    }
    
    // Handle timestamps
    if dbContact.CreatedAt.Valid {
        contact.CreatedAt = dbContact.CreatedAt.Time
    }
    
    if dbContact.UpdatedAt.Valid {
        contact.UpdatedAt = dbContact.UpdatedAt.Time
    }
    
    return contact
}

// Helper: string pointer to pgtype.Text
func stringToNullString(s *string) pgtype.Text {
    if s == nil {
        return pgtype.Text{Valid: false}
    }
    return pgtype.Text{String: *s, Valid: true}
}

// Helper: uuid.UUID to pgtype.UUID
func uuidToPgUUID(id uuid.UUID) pgtype.UUID {
    return pgtype.UUID{
        Bytes: [16]byte(id),
        Valid: true,
    }
}

// Helper: time pointer to pgtype.Timestamptz
func timeToNullTime(t *time.Time) pgtype.Timestamptz {
    if t == nil {
        return pgtype.Timestamptz{Valid: false}
    }
    return pgtype.Timestamptz{Time: *t, Valid: true}
}
```

### Handler Validation Pattern

```go
func (h *ContactHandler) CreateContact(c *gin.Context) {
    // 1. Parse request body
    var req CreateContactRequest
    if err := c.ShouldBindJSON(&req); err != nil {
        api.SendValidationError(c, "Invalid request body", err.Error())
        return
    }
    
    // 2. Additional validation (if needed)
    if req.Email != nil && !isValidEmail(*req.Email) {
        api.SendValidationError(c, "Invalid email format", "")
        return
    }
    
    // 3. Call repository/service
    contact, err := h.repo.CreateContact(c.Request.Context(), repository.CreateContactRequest{
        FullName: req.FullName,
        Email:    req.Email,
    })
    if err != nil {
        api.SendInternalError(c, "Failed to create contact")
        return
    }
    
    // 4. Convert to response model
    response := convertToContactResponse(contact)
    
    // 5. Send response
    api.SendSuccess(c, http.StatusCreated, response, nil)
}
```

### Error Wrapping Pattern

```go
// Always wrap errors with context
if err != nil {
    return fmt.Errorf("create contact: %w", err)
}

// For multiple operations
contact, err := r.contactRepo.GetContact(ctx, id)
if err != nil {
    return nil, fmt.Errorf("get contact: %w", err)
}

reminder, err := r.reminderRepo.CreateReminder(ctx, req)
if err != nil {
    return nil, fmt.Errorf("create reminder: %w", err)
}
```

### Context Timeout Pattern

```go
// For database operations with timeout
ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
defer cancel()

contact, err := repo.GetContact(ctx, id)
if err != nil {
    // Handle error
}
```

### Time Handling Pattern

**Always use accelerated time for testing:**

```go
// ❌ WRONG
now := time.Now()

// ✅ CORRECT
import "personal-crm/backend/internal/accelerated"

now := accelerated.GetCurrentTime()
```

---

## Frontend Patterns

### Loading Pattern

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

### Form Pattern (with Zod + React Hook Form)

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
      
      <div>
        <label htmlFor="email" className="block text-sm font-medium text-gray-700">
          Email
        </label>
        <input
          {...register('email')}
          type="email"
          className="mt-1 block w-full rounded-md border-gray-300 shadow-sm focus:border-blue-500 focus:ring-blue-500"
        />
        {errors.email && (
          <p className="mt-1 text-sm text-red-600">{errors.email.message}</p>
        )}
      </div>
      
      <button
        type="submit"
        disabled={createMutation.isPending}
        className="w-full bg-blue-600 text-white px-4 py-2 rounded-md hover:bg-blue-700 disabled:opacity-50 disabled:cursor-not-allowed"
      >
        {createMutation.isPending ? 'Saving...' : 'Save Contact'}
      </button>
    </form>
  )
}
```

### React Query Mutation Pattern

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

### API Client Pattern

```typescript
// frontend/src/lib/api-client.ts
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
  process.env.NEXT_PUBLIC_API_URL || 'http://localhost:8080'
)
```

### Conditional Rendering Pattern

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

### Date Formatting Pattern

```typescript
// frontend/src/lib/date-utils.ts
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

---

## Testing Patterns

### Unit Test Pattern

```go
func TestCadenceCalculation(t *testing.T) {
    tests := []struct {
        name        string
        cadence     reminder.CadenceType
        lastContact time.Time
        checkTime   time.Time
        wantOverdue bool
    }{
        {
            name:        "weekly not overdue",
            cadence:     reminder.CadenceWeekly,
            lastContact: time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
            checkTime:   time.Date(2024, 1, 6, 0, 0, 0, 0, time.UTC),
            wantOverdue: false,
        },
        {
            name:        "weekly overdue",
            cadence:     reminder.CadenceWeekly,
            lastContact: time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
            checkTime:   time.Date(2024, 1, 10, 0, 0, 0, 0, time.UTC),
            wantOverdue: true,
        },
    }
    
    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            got := reminder.IsOverdue(tt.cadence, &tt.lastContact, tt.checkTime)
            assert.Equal(t, tt.wantOverdue, got)
        })
    }
}
```

### Integration Test Pattern

```go
func TestContactRepository_Integration(t *testing.T) {
    if testing.Short() {
        t.Skip("Skipping integration test")
    }
    
    ctx := context.Background()
    db := setupTestDB(t)
    defer cleanupTestDB(t, db)
    
    repo := repository.NewContactRepository(db.Queries)
    
    t.Run("Create and Get", func(t *testing.T) {
        // Create
        created, err := repo.CreateContact(ctx, repository.CreateContactRequest{
            FullName: "Test User",
            Email:    stringPtr("test@example.com"),
        })
        require.NoError(t, err)
        assert.NotEmpty(t, created.ID)
        
        // Get
        fetched, err := repo.GetContact(ctx, created.ID)
        require.NoError(t, err)
        assert.Equal(t, created.FullName, fetched.FullName)
    })
    
    t.Run("Soft Delete", func(t *testing.T) {
        // Create and delete
        created, _ := repo.CreateContact(ctx, repository.CreateContactRequest{
            FullName: "To Delete",
        })
        
        err := repo.SoftDeleteContact(ctx, created.ID)
        require.NoError(t, err)
        
        // Should not be found
        _, err = repo.GetContact(ctx, created.ID)
        assert.Error(t, err)
    })
}
```

---

## SQL Patterns

### Basic CRUD Queries

```sql
-- Get one (with soft delete check)
-- name: GetContact :one
SELECT * FROM contact
WHERE id = $1 AND deleted_at IS NULL
LIMIT 1;

-- List with pagination
-- name: ListContacts :many
SELECT * FROM contact
WHERE deleted_at IS NULL
ORDER BY created_at DESC
LIMIT $1 OFFSET $2;

-- Create
-- name: CreateContact :one
INSERT INTO contact (
    full_name, email, phone
) VALUES (
    $1, $2, $3
) RETURNING *;

-- Update
-- name: UpdateContact :one
UPDATE contact
SET
    full_name = $2,
    email = $3,
    updated_at = NOW()
WHERE id = $1 AND deleted_at IS NULL
RETURNING *;

-- Soft delete
-- name: SoftDeleteContact :exec
UPDATE contact
SET deleted_at = NOW()
WHERE id = $1 AND deleted_at IS NULL;
```

### Search Query Pattern

```sql
-- name: SearchContacts :many
SELECT * FROM contact
WHERE 
    deleted_at IS NULL
    AND (
        full_name ILIKE '%' || $1 || '%'
        OR email ILIKE '%' || $1 || '%'
    )
ORDER BY created_at DESC
LIMIT $2 OFFSET $3;
```

### Aggregate Query Pattern

```sql
-- name: CountContactsByCadence :many
SELECT 
    cadence,
    COUNT(*) as count
FROM contact
WHERE deleted_at IS NULL
GROUP BY cadence
ORDER BY count DESC;
```

---

*For full feature development process, see [`.ai/development.md`](./development.md)*

*For architecture rationale, see [`.ai/architecture.md`](./architecture.md)*

