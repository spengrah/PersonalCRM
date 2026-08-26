package api

import (
	"strings"
	"testing"

	"personal-crm/backend/internal/api/handlers"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
)

// spec: IXN-010
func TestInteractionRoutes_ReadOnly(t *testing.T) {
	router := gin.New()
	v1 := router.Group("/api/v1")
	handlers.RegisterContactRoutes(v1, handlers.ContactRouteDeps{
		Contact:       &handlers.ContactHandler{},
		Interaction:   &handlers.InteractionHandler{},
		Note:          &handlers.NoteHandler{},
		ContactMethod: &handlers.ContactMethodHandler{},
	})

	actual := make([]string, 0)
	for _, route := range router.Routes() {
		if containsInteractionPath(route.Path) {
			actual = append(actual, route.Method+" "+route.Path)
		}
	}
	assert.ElementsMatch(t, []string{
		"GET /api/v1/contacts/:id/interactions",
		"POST /api/v1/contacts/:id/interactions",
		"GET /api/v1/interactions/:id/content",
		"DELETE /api/v1/interactions/:id",
	}, actual)
}

func containsInteractionPath(path string) bool {
	return strings.Contains(path, "/interactions")
}
