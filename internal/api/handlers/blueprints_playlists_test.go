package handlers

import (
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jmal1/selfservice-api/internal/api/routes"
)

// Test that the blueprint VM playlists route is registered.
func TestBlueprintVMPlaylistsRouteRegistered(t *testing.T) {
	r := routes.Setup(nil, nil, nil, []string{"https://example.test"})

	found := false
	err := chi.Walk(r, func(method, route string, _, ...interface{}) error {
		route = strings.TrimSuffix(route, "/*")
		if method == "GET" && route == "/api/v1/admin/blueprints/{blueprintID}/vm-playlists" {
			found = true
		}
		return nil
	})
	if err != nil {
		t.Fatalf("chi.Walk: %v", err)
	}
	if !found {
		t.Errorf("route GET /api/v1/admin/blueprints/{blueprintID}/vm-playlists not registered")
	}
}

