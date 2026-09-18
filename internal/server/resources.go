package server

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/giuliano/webterm/internal/control"
	"github.com/giuliano/webterm/internal/store"
)

type linkResourceReq struct {
	Ref string `json:"ref"`
	// System y Type son opcionales: por defecto se infieren de la URL. Están
	// para no cerrarle la puerta a un formato que el registry no conozca.
	System string `json:"system"`
	Type   string `json:"type"`
}

func (s *Server) handleListResources(w http.ResponseWriter, r *http.Request) {
	list, err := s.mgr.ListResources(r.Context(), r.PathValue("id"))
	if err != nil {
		writeResourceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) handleLinkResource(w http.ResponseWriter, r *http.Request) {
	var req linkResourceReq
	if err := decodeBody(r, &req); err != nil {
		writeErrorMsg(w, http.StatusBadRequest, "body inválido")
		return
	}
	rec, err := s.mgr.AddResource(r.PathValue("id"), req.Ref, req.System, req.Type)
	if err != nil {
		writeResourceError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, rec)
}

func (s *Server) handleUnlinkResource(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("rid"), 10, 64)
	if err != nil {
		writeErrorMsg(w, http.StatusBadRequest, "id de recurso inválido")
		return
	}
	if err := s.mgr.DeleteResource(r.PathValue("id"), id); err != nil {
		writeResourceError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// writeResourceError suma a la traducción general los errores propios del ABM
// de recursos.
func writeResourceError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, control.ErrUnknownResource):
		writeErrorMsg(w, http.StatusBadRequest, "no se reconoce esa URL; por ahora solo PRs de GitHub")
	case errors.Is(err, store.ErrDuplicate):
		writeErrorMsg(w, http.StatusConflict, "ese recurso ya está linkeado a la sesión")
	default:
		writeError(w, err)
	}
}
