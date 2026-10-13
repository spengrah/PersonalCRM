package api

import (
	"strings"
	"testing"

	"personal-crm/backend/internal/api/handlers"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
)

// The interactions surface writes only through the manual entry and the
// correction requests; no request deletes an interaction.
// spec: IXN-029.removal-is-only-delete
func TestInteractionRoutes_RegisteredSet(t *testing.T) {
	router := gin.New()
	v1 := router.Group("/api/v1")
	handlers.RegisterContactRoutes(v1, handlers.ContactRouteDeps{
		Contact:       &handlers.ContactHandler{},
		Interaction:   &handlers.InteractionHandler{},
		Correction:    &handlers.InteractionCorrectionHandler{},
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
		"POST /api/v1/interactions/merge",
		"POST /api/v1/interactions/merge/preview",
		"POST /api/v1/interactions/:id/removals",
	}, actual)
}

func containsInteractionPath(path string) bool {
	return strings.Contains(path, "/interactions")
}
