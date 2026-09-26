package server

import (
	"errors"
	"net/http"

	"github.com/giuliano/webterm/internal/store"
)

func (s *Server) handleListTags(w http.ResponseWriter, _ *http.Request) {
	list, err := s.mgr.ListTags()
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

// handleSetSessionTags reemplaza el conjunto de tags de la sesión y devuelve la
// sesión como quedó, con los tags ya normalizados: así la UI muestra "bug-fix"
// apenas se escribe "Bug fix", sin esperar al refresco.
//
// Es un PUT del conjunto entero y no add/remove porque la UI siempre tiene la
// lista completa a mano; el MCP, que no la tiene, usa las variantes
// incrementales del manager.
func (s *Server) handleSetSessionTags(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Tags []string `json:"tags"`
	}
	if err := decodeBody(r, &req); err != nil {
		writeErrorMsg(w, http.StatusBadRequest, "body inválido")
		return
	}
	id := r.PathValue("id")
	if err := s.mgr.SetSessionTags(id, req.Tags); err != nil {
		writeTagError(w, err)
		return
	}
	rec, err := s.mgr.Get(id)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rec)
}

func writeTagError(w http.ResponseWriter, err error) {
	if errors.Is(err, store.ErrInvalidTag) {
		writeErrorMsg(w, http.StatusBadRequest, err.Error())
		return
	}
	writeError(w, err)
}
