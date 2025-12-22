# Feature Development Guide

Complete step-by-step guide for implementing new features in the Personal CRM.

---

## Feature Development Process

Follow this order when implementing new features:

1. Database Schema Changes (if needed)
2. Add SQL Queries
3. Create Repository Layer
4. Add Service Layer (if complex logic)
5. Create HTTP Handlers
6. Register Routes
7. Write Tests
8. Add Frontend Components

---

## 1. Database Schema Changes

**If adding new tables or fields:**

```bash
# Create new migration
cd backend/migrations
touch 00X_feature_name.up.sql
touch 00X_feature_name.down.sql
```

### Migration File Structure

**Up migration:**
```sql
-- 00X_feature_name.up.sql
-- Description of what this migration does

CREATE TABLE new_table (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    field_name TEXT NOT NULL,
    nullable_field TEXT,
    created_at TIMESTAMPTZ DEFAULT NOW(),
    updated_at TIMESTAMPTZ DEFAULT NOW(),
    deleted_at TIMESTAMPTZ  -- For soft deletes
);

CREATE INDEX idx_new_table_field ON new_table(field_name);
CREATE INDEX idx_new_table_deleted ON new_table(deleted_at);
```

**Down migration:**
```sql
-- 00X_feature_name.down.sql
DROP TABLE IF EXISTS new_table;
```

### Schema Design Principles

1. **Use UUIDs for primary keys**
   ```sql
   id UUID PRIMARY KEY DEFAULT uuid_generate_v4()
   ```

2. **Use soft deletes for user data**
   ```sql
   deleted_at TIMESTAMPTZ  -- NULL = not deleted
   ```

3. **Always add timestamps**
   ```sql
   created_at TIMESTAMPTZ DEFAULT NOW()
   updated_at TIMESTAMPTZ DEFAULT NOW()
   ```

4. **Use CHECK constraints for enums**
   ```sql
   cadence TEXT CHECK (cadence IN ('weekly','monthly','quarterly'))
   ```

5. **Add indexes for foreign keys and common queries**
   ```sql
   CREATE INDEX idx_note_contact_id ON note(contact_id);
   CREATE INDEX idx_note_created_at ON note(created_at DESC);
   ```

6. **Use TIMESTAMPTZ (not TIMESTAMP)**
   - Always store in UTC
   - Convert to local timezone in UI

### Migration Best Practices

1. **One logical change per migration**
2. **Test both up and down migrations**
3. **Never modify existing migrations after merge**
4. **Consider data migrations separately**
5. **Add helpful comments**

---

## 2. Add SQL Queries

All queries go in `backend/internal/db/queries/*.sql` and use sqlc for type-safe Go generation.

```sql
-- backend/internal/db/queries/new_table.sql

-- name: GetNewTable :one
SELECT * FROM new_table
WHERE id = $1 AND deleted_at IS NULL
LIMIT 1;

-- name: ListNewTables :many
SELECT * FROM new_table
WHERE deleted_at IS NULL
ORDER BY created_at DESC
LIMIT $1 OFFSET $2;

-- name: CreateNewTable :one
INSERT INTO new_table (
    field1, field2
) VALUES (
    $1, $2
) RETURNING *;

-- name: UpdateNewTable :one
UPDATE new_table
SET
    field1 = $2,
    field2 = $3,
    updated_at = NOW()
WHERE id = $1 AND deleted_at IS NULL
RETURNING *;

-- name: SoftDeleteNewTable :exec
UPDATE new_table
SET deleted_at = NOW()
WHERE id = $1 AND deleted_at IS NULL;
```

**Then regenerate sqlc code:**
```bash
cd backend
sqlc generate
```

---

## 3. Create Repository Layer

The repository layer converts between sqlc-generated DB types and clean domain types.

```go
// backend/internal/repository/new_table.go
package repository

import (
    "context"
    "personal-crm/backend/internal/db"
    "github.com/google/uuid"
    "time"
)

type NewTableRepository struct {
    queries db.Querier
}

func NewNewTableRepository(queries db.Querier) *NewTableRepository {
    return &NewTableRepository{queries: queries}
}

// Domain model (clean types, no pgtype)
type NewTable struct {
    ID        uuid.UUID
    Field1    string
    Field2    *string  // nullable
    CreatedAt time.Time
    UpdatedAt time.Time
}

// Request models
type CreateNewTableRequest struct {
    Field1 string
    Field2 *string
}

type UpdateNewTableRequest struct {
    Field1 string
    Field2 *string
}

// Convert DB types to domain types
func convertDbNewTable(dbItem *db.NewTable) NewTable {
    item := NewTable{
        ID:     uuid.UUID(dbItem.ID.Bytes),
        Field1: dbItem.Field1,
    }
    
    if dbItem.Field2.Valid {
        field2 := dbItem.Field2.String  // Copy value before taking address
        item.Field2 = &field2
    }
    
    if dbItem.CreatedAt.Valid {
        item.CreatedAt = dbItem.CreatedAt.Time
    }
    
    if dbItem.UpdatedAt.Valid {
        item.UpdatedAt = dbItem.UpdatedAt.Time
    }
    
    return item
}

// Repository methods
func (r *NewTableRepository) GetNewTable(ctx context.Context, id uuid.UUID) (*NewTable, error) {
    dbItem, err := r.queries.GetNewTable(ctx, uuidToPgUUID(id))
    if err != nil {
        return nil, err
    }
    
    item := convertDbNewTable(dbItem)
    return &item, nil
}

func (r *NewTableRepository) ListNewTables(ctx context.Context, limit, offset int32) ([]NewTable, error) {
    dbItems, err := r.queries.ListNewTables(ctx, db.ListNewTablesParams{
        Limit:  limit,
        Offset: offset,
    })
    if err != nil {
        return nil, err
    }
    
    items := make([]NewTable, len(dbItems))
    for i, dbItem := range dbItems {
        items[i] = convertDbNewTable(&dbItem)
    }
    
    return items, nil
}

func (r *NewTableRepository) CreateNewTable(ctx context.Context, req CreateNewTableRequest) (*NewTable, error) {
    dbItem, err := r.queries.CreateNewTable(ctx, db.CreateNewTableParams{
        Field1: req.Field1,
        Field2: stringToNullString(req.Field2),
    })
    if err != nil {
        return nil, err
    }
    
    item := convertDbNewTable(dbItem)
    return &item, nil
}

func (r *NewTableRepository) UpdateNewTable(ctx context.Context, id uuid.UUID, req UpdateNewTableRequest) (*NewTable, error) {
    dbItem, err := r.queries.UpdateNewTable(ctx, db.UpdateNewTableParams{
        ID:     uuidToPgUUID(id),
        Field1: req.Field1,
        Field2: stringToNullString(req.Field2),
    })
    if err != nil {
        return nil, err
    }
    
    item := convertDbNewTable(dbItem)
    return &item, nil
}

func (r *NewTableRepository) SoftDeleteNewTable(ctx context.Context, id uuid.UUID) error {
    return r.queries.SoftDeleteNewTable(ctx, uuidToPgUUID(id))
}
```

---

## 4. Add Service Layer (if complex logic)

**When to create a service:**
- Orchestrating multiple repositories
- Complex business logic
- Scheduled jobs
- External API calls

**When NOT to create a service:**
- Simple CRUD operations (just use repository directly from handler)
- Single repository operations
- No business logic beyond validation

```go
// backend/internal/service/new_feature.go
package service

import (
    "context"
    "fmt"
    "personal-crm/backend/internal/repository"
    "github.com/google/uuid"
)

type NewFeatureService struct {
    newTableRepo *repository.NewTableRepository
    contactRepo  *repository.ContactRepository
}

func NewNewFeatureService(
    newTableRepo *repository.NewTableRepository,
    contactRepo *repository.ContactRepository,
) *NewFeatureService {
    return &NewFeatureService{
        newTableRepo: newTableRepo,
        contactRepo:  contactRepo,
    }
}

func (s *NewFeatureService) ProcessFeature(ctx context.Context, id uuid.UUID) error {
    // Complex logic here, possibly calling multiple repositories
    
    // Example: Fetch from multiple sources
    item, err := s.newTableRepo.GetNewTable(ctx, id)
    if err != nil {
        return fmt.Errorf("get item: %w", err)
    }
    
    contact, err := s.contactRepo.GetContact(ctx, item.RelatedContactID)
    if err != nil {
        return fmt.Errorf("get contact: %w", err)
    }
    
    // Do business logic...
    
    return nil
}
```

---

## 5. Create HTTP Handlers

```go
// backend/internal/api/handlers/new_table.go
package handlers

import (
    "net/http"
    "personal-crm/backend/internal/api"
    "personal-crm/backend/internal/repository"
    "github.com/gin-gonic/gin"
    "github.com/google/uuid"
)

type NewTableHandler struct {
    repo *repository.NewTableRepository
}

func NewNewTableHandler(repo *repository.NewTableRepository) *NewTableHandler {
    return &NewTableHandler{repo: repo}
}

// Request/Response models
type CreateNewTableRequest struct {
    Field1 string  `json:"field1" validate:"required"`
    Field2 *string `json:"field2,omitempty"`
}

type UpdateNewTableRequest struct {
    Field1 string  `json:"field1" validate:"required"`
    Field2 *string `json:"field2,omitempty"`
}

type NewTableResponse struct {
    ID        string    `json:"id"`
    Field1    string    `json:"field1"`
    Field2    *string   `json:"field2,omitempty"`
    CreatedAt string    `json:"created_at"`
    UpdatedAt string    `json:"updated_at"`
}

func convertToResponse(item *repository.NewTable) NewTableResponse {
    return NewTableResponse{
        ID:        item.ID.String(),
        Field1:    item.Field1,
        Field2:    item.Field2,
        CreatedAt: item.CreatedAt.Format(time.RFC3339),
        UpdatedAt: item.UpdatedAt.Format(time.RFC3339),
    }
}

// Handler functions
func (h *NewTableHandler) CreateNewTable(c *gin.Context) {
    var req CreateNewTableRequest
    if err := c.ShouldBindJSON(&req); err != nil {
        api.SendValidationError(c, "Invalid request body", err.Error())
        return
    }
    
    item, err := h.repo.CreateNewTable(c.Request.Context(), repository.CreateNewTableRequest{
        Field1: req.Field1,
        Field2: req.Field2,
    })
    if err != nil {
        api.SendInternalError(c, "Failed to create item")
        return
    }
    
    response := convertToResponse(item)
    api.SendSuccess(c, http.StatusCreated, response, nil)
}

func (h *NewTableHandler) GetNewTable(c *gin.Context) {
    idStr := c.Param("id")
    id, err := uuid.Parse(idStr)
    if err != nil {
        api.SendValidationError(c, "Invalid ID", err.Error())
        return
    }
    
    item, err := h.repo.GetNewTable(c.Request.Context(), id)
    if err != nil {
        if err == sql.ErrNoRows {
            api.SendNotFound(c, "Item")
            return
        }
        api.SendInternalError(c, "Failed to fetch item")
        return
    }
    
    response := convertToResponse(item)
    api.SendSuccess(c, http.StatusOK, response, nil)
}

func (h *NewTableHandler) ListNewTables(c *gin.Context) {
    // Parse pagination params
    limit := int32(20)
    offset := int32(0)
    
    items, err := h.repo.ListNewTables(c.Request.Context(), limit, offset)
    if err != nil {
        api.SendInternalError(c, "Failed to list items")
        return
    }
    
    responses := make([]NewTableResponse, len(items))
    for i, item := range items {
        responses[i] = convertToResponse(&item)
    }
    
    api.SendSuccess(c, http.StatusOK, responses, nil)
}

func (h *NewTableHandler) UpdateNewTable(c *gin.Context) {
    idStr := c.Param("id")
    id, err := uuid.Parse(idStr)
    if err != nil {
        api.SendValidationError(c, "Invalid ID", err.Error())
        return
    }
    
    var req UpdateNewTableRequest
    if err := c.ShouldBindJSON(&req); err != nil {
        api.SendValidationError(c, "Invalid request body", err.Error())
        return
    }
    
    item, err := h.repo.UpdateNewTable(c.Request.Context(), id, repository.UpdateNewTableRequest{
        Field1: req.Field1,
        Field2: req.Field2,
    })
    if err != nil {
        api.SendInternalError(c, "Failed to update item")
        return
    }
    
    response := convertToResponse(item)
    api.SendSuccess(c, http.StatusOK, response, nil)
}

func (h *NewTableHandler) DeleteNewTable(c *gin.Context) {
    idStr := c.Param("id")
    id, err := uuid.Parse(idStr)
    if err != nil {
        api.SendValidationError(c, "Invalid ID", err.Error())
        return
    }
    
    if err := h.repo.SoftDeleteNewTable(c.Request.Context(), id); err != nil {
        api.SendInternalError(c, "Failed to delete item")
        return
    }
    
    c.Status(http.StatusNoContent)
}
```

---

## 6. Register Routes

```go
// backend/cmd/crm-api/main.go

// Initialize repository and handler
newTableRepo := repository.NewNewTableRepository(database.Queries)
newTableHandler := handlers.NewNewTableHandler(newTableRepo)

// Add routes
v1 := router.Group("/api/v1")
{
    newTables := v1.Group("/new-tables")
    {
        newTables.POST("", newTableHandler.CreateNewTable)
        newTables.GET("/:id", newTableHandler.GetNewTable)
        newTables.GET("", newTableHandler.ListNewTables)
        newTables.PUT("/:id", newTableHandler.UpdateNewTable)
        newTables.DELETE("/:id", newTableHandler.DeleteNewTable)
    }
}
```

---

## 7. Write Tests

### Unit Tests

```go
// backend/tests/unit/new_table_test.go
package tests

import (
    "testing"
    "github.com/stretchr/testify/assert"
)

func TestNewTableCreation(t *testing.T) {
    // Test business logic in isolation
    // Use mocks for dependencies
}
```

### Integration Tests

```go
// backend/tests/integration/new_table_integration_test.go
package tests

import (
    "context"
    "testing"
    "personal-crm/backend/internal/repository"
    "github.com/stretchr/testify/assert"
    "github.com/stretchr/testify/require"
)

func TestNewTableRepository_Integration(t *testing.T) {
    if testing.Short() {
        t.Skip("Skipping integration test")
    }
    
    ctx := context.Background()
    db := setupTestDB(t)
    defer cleanupTestDB(t, db)
    
    repo := repository.NewNewTableRepository(db.Queries)
    
    // Test CRUD operations
    t.Run("Create", func(t *testing.T) {
        item, err := repo.CreateNewTable(ctx, repository.CreateNewTableRequest{
            Field1: "test",
        })
        require.NoError(t, err)
        assert.NotEmpty(t, item.ID)
        assert.Equal(t, "test", item.Field1)
    })
    
    // More tests...
}
```

---

## 8. Add Frontend Components

### API Client

```typescript
// frontend/src/lib/new-table-api.ts
import { apiClient } from './api-client'

export interface NewTable {
  id: string
  field1: string
  field2?: string
  created_at: string
  updated_at: string
}

export const newTableApi = {
  create: async (data: { field1: string; field2?: string }) => {
    return apiClient.post<NewTable>('/api/v1/new-tables', data)
  },
  
  get: async (id: string) => {
    return apiClient.get<NewTable>(`/api/v1/new-tables/${id}`)
  },
  
  list: async () => {
    return apiClient.get<NewTable[]>('/api/v1/new-tables')
  },
  
  update: async (id: string, data: { field1: string; field2?: string }) => {
    return apiClient.put<NewTable>(`/api/v1/new-tables/${id}`, data)
  },
  
  delete: async (id: string) => {
    return apiClient.delete(`/api/v1/new-tables/${id}`)
  },
}
```

### React Query Hooks

```typescript
// frontend/src/hooks/use-new-table.ts
import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import { newTableApi, NewTable } from '@/lib/new-table-api'

export function useNewTables() {
  return useQuery({
    queryKey: ['new-tables'],
    queryFn: () => newTableApi.list(),
  })
}

export function useNewTable(id: string) {
  return useQuery({
    queryKey: ['new-tables', id],
    queryFn: () => newTableApi.get(id),
    enabled: !!id,
  })
}

export function useCreateNewTable() {
  const queryClient = useQueryClient()
  
  return useMutation({
    mutationFn: newTableApi.create,
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ['new-tables'] })
    },
  })
}

export function useUpdateNewTable() {
  const queryClient = useQueryClient()
  
  return useMutation({
    mutationFn: ({ id, data }: { id: string; data: { field1: string; field2?: string } }) =>
      newTableApi.update(id, data),
    onSuccess: (_, variables) => {
      queryClient.invalidateQueries({ queryKey: ['new-tables'] })
      queryClient.invalidateQueries({ queryKey: ['new-tables', variables.id] })
    },
  })
}

export function useDeleteNewTable() {
  const queryClient = useQueryClient()
  
  return useMutation({
    mutationFn: newTableApi.delete,
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ['new-tables'] })
    },
  })
}
```

### React Component

```typescript
// frontend/src/components/new-table-form.tsx
'use client'

import { useForm } from 'react-hook-form'
import { zodResolver } from '@hookform/resolvers/zod'
import { z } from 'zod'
import { useCreateNewTable } from '@/hooks/use-new-table'

const schema = z.object({
  field1: z.string().min(1, "Field1 is required"),
  field2: z.string().optional(),
})

type FormData = z.infer<typeof schema>

export function NewTableForm() {
  const { register, handleSubmit, formState: { errors }, reset } = useForm<FormData>({
    resolver: zodResolver(schema)
  })
  
  const createMutation = useCreateNewTable()
  
  const onSubmit = (data: FormData) => {
    createMutation.mutate(data, {
      onSuccess: () => {
        reset()
        // Show success message
      },
    })
  }
  
  return (
    <form onSubmit={handleSubmit(onSubmit)} className="space-y-4">
      <div>
        <label htmlFor="field1" className="block text-sm font-medium">
          Field 1 *
        </label>
        <input
          {...register('field1')}
          type="text"
          className="mt-1 block w-full rounded-md border-gray-300 shadow-sm"
        />
        {errors.field1 && (
          <p className="mt-1 text-sm text-red-600">{errors.field1.message}</p>
        )}
      </div>
      
      <div>
        <label htmlFor="field2" className="block text-sm font-medium">
          Field 2 (optional)
        </label>
        <input
          {...register('field2')}
          type="text"
          className="mt-1 block w-full rounded-md border-gray-300 shadow-sm"
        />
      </div>
      
      <button
        type="submit"
        disabled={createMutation.isPending}
        className="bg-blue-600 text-white px-4 py-2 rounded hover:bg-blue-700 disabled:opacity-50"
      >
        {createMutation.isPending ? 'Creating...' : 'Create'}
      </button>
    </form>
  )
}
```

---

## Performance Considerations

### Backend
- Connection pool is tuned for Raspberry Pi (5 max connections)
- Use context timeouts for database queries
- Avoid N+1 queries (use JOINs or batch fetching)
- Cache expensive calculations
- Use indexes on frequently queried fields

### Frontend
- React Query handles caching automatically
- Use `staleTime` to reduce unnecessary refetches
- Lazy load heavy components
- Optimize images (Next.js does this automatically)
- Keep bundle size small (check with `npm run build`)

### Database
- PostgreSQL is tuned for 4-8GB RAM environments
- Use EXPLAIN ANALYZE to check query performance
- Full-text search is better than ILIKE for text search
- pgvector indexes (HNSW) for embeddings (future)

---

## Environment Management

### Environment Files

```
.env                      # Active environment (gitignored)
.env.example              # Template with all variables
.env.example.production   # Real-world timing
.env.example.staging      # Fast cadences (hours)
.env.example.testing      # Ultra-fast (minutes)
```

### Required Environment Variables

```bash
# Database (required)
DATABASE_URL=postgres://user:pass@localhost:5432/dbname?sslmode=disable

# Server
PORT=8080
CRM_ENV=development|staging|production

# Logging
LOG_LEVEL=debug|info|warn|error

# Optional: AI features (future)
ANTHROPIC_API_KEY=your-key-here
ENABLE_VECTOR_SEARCH=false

# Optional: Telegram bot (future)
TELEGRAM_BOT_TOKEN=your-token-here
ENABLE_TELEGRAM_BOT=false
```

---

## AI/LLM Features (Future)

### Current State
- Database has `note_embedding` and `interaction_embedding` tables (unused)
- pgvector extension enabled
- No embedding generation yet

### Planned Architecture
- **Embedding generation:** Run on MacBook M2 Max
- **Storage:** PostgreSQL with pgvector
- **LLM inference:** Ollama on Mac (Llama 3.1 70B)
- **Worker pattern:** Mac polls Pi for pending tasks

### When Implementing AI Features

1. **Create LLM task queue table** (check GitHub Issues for LLM implementation tasks)
2. **Build worker that polls Pi** (runs on Mac)
3. **Generate embeddings asynchronously** (don't block API)
4. **Store results in existing embedding tables**
5. **Keep Pi as source of truth** (Mac is just compute)

---

*For common code patterns, see [`.ai/patterns.md`](./patterns.md)*

*For architecture rationale, see [`.ai/architecture.md`](./architecture.md)*

