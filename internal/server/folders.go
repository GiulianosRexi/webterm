package server

import (
	"errors"
	"net/http"

	"github.com/giuliano/webterm/internal/store"
)

func (s *Server) handleListFolders(w http.ResponseWriter, r *http.Request) {
	list, err := s.mgr.ListFolders()
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) handleCreateFolder(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if err := decodeBody(r, &req); err != nil {
		writeErrorMsg(w, http.StatusBadRequest, "body inválido")
		return
	}
	f, err := s.mgr.CreateFolder(req.Name)
	if err != nil {
		writeFolderError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, f)
}

func (s *Server) handleRenameFolder(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if err := decodeBody(r, &req); err != nil {
		writeErrorMsg(w, http.StatusBadRequest, "body inválido")
		return
	}
	if err := s.mgr.RenameFolder(r.PathValue("id"), req.Name); err != nil {
		writeFolderError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleDeleteFolder(w http.ResponseWriter, r *http.Request) {
	if err := s.mgr.DeleteFolder(r.PathValue("id")); err != nil {
		writeFolderError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleSetSessionFolder mueve una sesión.
//
// Es un endpoint propio y no un campo más del PATCH de la sesión porque en
// JSON "no tocar el folder" y "sacarla del folder" son lo mismo al
// deserializar: ausente y null llegan los dos como nil. Acá el campo siempre
// viene, así que null significa sin ambigüedad "sacala del folder".
func (s *Server) handleSetSessionFolder(w http.ResponseWriter, r *http.Request) {
	var req struct {
		FolderID *string `json:"folder_id"`
	}
	if err := decodeBody(r, &req); err != nil {
		writeErrorMsg(w, http.StatusBadRequest, "body inválido")
		return
	}
	if err := s.mgr.SetSessionFolder(r.PathValue("id"), req.FolderID); err != nil {
		writeFolderError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// writeFolderError suma a la traducción general los errores del ABM de
// folders.
func writeFolderError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrNombreVacio):
		writeErrorMsg(w, http.StatusBadRequest, "el folder necesita un nombre")
	case errors.Is(err, store.ErrDuplicate):
		writeErrorMsg(w, http.StatusConflict, "ya existe un folder con ese nombre")
	default:
		writeError(w, err)
	}
}
