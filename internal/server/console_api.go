package server

import (
	"net/http"
)

func (s *Server) handleConsoleSnapshot(w http.ResponseWriter, _ *http.Request) {
	snapshot, err := s.console.Read()
	if err != nil {
		writeJSONStatus(w, http.StatusInternalServerError, map[string]any{
			"ok":    false,
			"error": "load console snapshot failed",
		})
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, snapshot)
}
