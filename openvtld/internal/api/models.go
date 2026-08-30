package api

// The emulation model catalog behind the create-library wizard — static
// facts about the installed mhVTL (inventory/models.go), so read-only.

import (
	"net/http"

	"github.com/openvtl/openvtld/internal/inventory"
)

func (s *Server) listModels(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, 200, map[string]any{
		"libraries": inventory.LibraryModels(),
		"drives":    inventory.DriveModels(),
	})
}
