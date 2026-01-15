# Feature Development Guide

Complete step-by-step guide for implementing new features in the Personal CRM.

---

## Development Workflow

### Starting Development

```bash
# First time setup
make setup                  # Install dependencies and git hooks
make start-local            # Start all services in production mode
```

### Development Cycle

```bash
# Make code changes, then:
make reload                 # ⚠️ ALWAYS use this after code changes

# NOT just `make build` - that doesn't restart services!
```

### Why `make reload` is Critical

When running in production mode (`make start-local`), the frontend and backend run as detached processes. Unlike `make dev` which has hot-reload:

- **Frontend**: Next.js embeds `NEXT_PUBLIC_*` env vars at build time
- **Backend**: Go binaries must be rebuilt to pick up changes
- **Old processes**: Keep running with stale code if not killed

`make reload` does:
1. Rebuilds both frontend and backend
2. Kills existing processes on ports 3001 and 8080
3. Starts new processes with the fresh build
4. Verifies startup success and BUILD_ID

### Quick Reference

| Scenario | Command |
|----------|---------|
| Start fresh | `make start-local` |
| After code changes | `make reload` |
| Full restart (inc. DB) | `make restart` |
| Check status | `make status` |
| Stop everything | `make stop` |

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
make sqlc
```

---

## 3. Create Repository Layer

The repository layer converts between sqlc-generated DB types and clean domain types.

See `.ai/patterns/backend.md` for the full repository conversion pattern with examples.

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

Create handler file at `backend/internal/api/handlers/new_table.go`:

```go
type NewTableHandler struct {
    repo *repository.NewTableRepository
}

// Request/Response structs with json tags and validate:"required"
type CreateNewTableRequest struct { ... }
type NewTableResponse struct { ... }

// CRUD handlers following this pattern:
func (h *NewTableHandler) CreateNewTable(c *gin.Context) {
    // 1. Parse: c.ShouldBindJSON(&req)
    // 2. Call repo: h.repo.CreateNewTable(c.Request.Context(), ...)
    // 3. Handle errors: errors.Is(err, db.ErrNotFound) → api.SendNotFound()
    // 4. Respond: api.SendSuccess(c, http.StatusCreated, response, nil)
}
```

**HTTP status codes:**
- Create: `http.StatusCreated` (201)
- Get/List/Update: `http.StatusOK` (200)
- Delete: `http.StatusNoContent` (204)

See [`.ai/patterns/backend.md`](../patterns/backend.md) for full handler and error response patterns.

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

Add tests for new features. See [`.ai/rules/testing.md`](../rules/testing.md) for:
- Test pyramid (unit vs integration vs E2E)
- Code templates and patterns
- E2E parallelism with TestAPI
- Running test commands

**Quick reference:**
```bash
make test-unit         # Backend unit tests
make test-integration  # Backend DB tests
make test-e2e          # Playwright E2E tests
```

---

## 8. Add Frontend Components

### UI Design Preview (Before Implementation)

**When creating new UI elements**, generate a standalone HTML preview file to explore design options before writing React code. This allows rapid iteration on visual design without build cycles.

**When to use this approach:**
- New form layouts or complex input patterns
- Dashboard widgets or data displays
- Any UI where multiple design approaches are valid
- When the user needs to approve visual direction

**How to create a preview:**
1. Create a standalone HTML file in `/temp` (e.g., `temp/contact-form-preview.html`) - this directory is gitignored
2. Use Tailwind CSS via CDN for styling
3. Match the app's existing visual style (colors, spacing, borders)
4. Show multiple design options side-by-side with labels
5. Include interactive elements (dropdowns, buttons) so UX can be evaluated
6. Add a recommendation section explaining trade-offs

**Example structure:**
```html
<!DOCTYPE html>
<html>
<head>
  <script src="https://cdn.tailwindcss.com"></script>
</head>
<body class="bg-gray-100 p-8">
  <h1>Component Name - Design Options</h1>

  <!-- Option 1 -->
  <section class="bg-white rounded-lg shadow p-6 mb-8">
    <span class="bg-blue-100 text-blue-700 text-xs px-2 py-1 rounded">Option 1</span>
    <h2>Option Name</h2>
    <p class="text-gray-500">Description of this approach</p>
    <!-- Interactive mockup -->
  </section>

  <!-- Option 2, 3, etc. -->

  <!-- Recommendation -->
  <div class="bg-blue-50 rounded-lg p-4">
    <h3>Recommendation</h3>
    <p>Explain which option works best and why</p>
  </div>
</body>
</html>
```

**After approval:**
- Implement the chosen design in React
- Preview files in `/temp` are already gitignored, so no cleanup needed

---

### Frontend Files to Create

**1. API Client** (`frontend/src/lib/new-table-api.ts`):
```typescript
export const newTableApi = {
  create: (data) => apiClient.post<NewTable>('/api/v1/new-tables', data),
  get: (id) => apiClient.get<NewTable>(`/api/v1/new-tables/${id}`),
  list: () => apiClient.get<NewTable[]>('/api/v1/new-tables'),
  update: (id, data) => apiClient.put<NewTable>(`/api/v1/new-tables/${id}`, data),
  delete: (id) => apiClient.delete(`/api/v1/new-tables/${id}`),
}
```

**2. React Query Hooks** (`frontend/src/hooks/use-new-table.ts`):
- `useNewTables()` / `useNewTable(id)` - query hooks
- `useCreateNewTable()` / `useUpdateNewTable()` / `useDeleteNewTable()` - mutation hooks
- Use `invalidateFor('entity:created')` for cache invalidation

**3. React Component** (`frontend/src/components/new-table-form.tsx`):
- Zod schema + React Hook Form with `zodResolver`
- Use mutation hooks for submit

See [`.ai/patterns/frontend.md`](../patterns/frontend.md) for full form, mutation, and invalidation patterns.

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
- Keep bundle size small (check with `bun run build`)

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

*For common code patterns, see [`.ai/patterns/`](../patterns/)*

*For architecture rationale, see [`.ai/guides/architecture.md`](./architecture.md)*

