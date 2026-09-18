package server

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"

	"github.com/giuliano/webterm/internal/control"
	"github.com/giuliano/webterm/internal/ptyapi"
	"github.com/giuliano/webterm/internal/store"
)

// maxBodyBytes acota lo que leemos de un request: nadie manda un título de
// más de unos KB, y sin el límite un cliente podría inflarnos la memoria.
const maxBodyBytes = 64 << 10

// Enums que la API valida. M7 construye el dashboard sobre estos campos, así
// que conviene que la base no acumule valores inventados.
var (
	workStatuses   = map[string]bool{"idle": true, "working": true, "waiting_input": true, "error": true}
	kanbanStatuses = map[string]bool{"todo": true, "in_progress": true, "done": true}
)

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("escribiendo respuesta: %v", err)
	}
}

func writeErrorMsg(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// writeError traduce los errores del manager al status que corresponde.
func writeError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeErrorMsg(w, http.StatusNotFound, "no existe")
	case errors.Is(err, control.ErrAlreadyRunning):
		writeErrorMsg(w, http.StatusConflict, "la sesión ya está corriendo")
	case errors.Is(err, ptyapi.ErrNotLive):
		writeErrorMsg(w, http.StatusConflict, "la sesión no está corriendo")
	case errors.Is(err, ptyapi.ErrAlreadyLive):
		// Es el mismo 409 que ErrAlreadyRunning pero llega por otro camino: el
		// chequeo de LiveIDs de control.Restart no es atómico, así que dos
		// restarts concurrentes lo pasan los dos y al segundo lo frena recién
		// el spawnMu del daemon, con este error. Sin esta rama salía 500, o sea
		// "se rompió algo" en vez de "llegaste segundo".
		writeErrorMsg(w, http.StatusConflict, "la sesión ya está corriendo")
	case errors.Is(err, ptyapi.ErrClosed):
		// 503 y no 500: el dueño de los ptys se está apagando, no que algo
		// esté roto. Reintentar contra el daemon que vuelve es razonable, a
		// diferencia de los otros tres casos de este switch.
		writeErrorMsg(w, http.StatusServiceUnavailable, "el daemon de sesiones se está apagando")
	default:
		writeErrorMsg(w, http.StatusInternalServerError, err.Error())
	}
}

// decodeBody lee un JSON opcional: un body vacío deja el struct con sus ceros.
func decodeBody(r *http.Request, v any) error {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
	if err != nil {
		return err
	}
	if len(body) == 0 {
		return nil
	}
	return json.Unmarshal(body, v)
}

type sizeReq struct {
	Cols int `json:"cols"`
	Rows int `json:"rows"`
}

type createSessionReq struct {
	Title       string `json:"title"`
	Description string `json:"description"`
	Cwd         string `json:"cwd"`
	Cols        int    `json:"cols"`
	Rows        int    `json:"rows"`
}

func (s *Server) handleListSessions(w http.ResponseWriter, _ *http.Request) {
	list, err := s.mgr.List()
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) handleCreateSession(w http.ResponseWriter, r *http.Request) {
	var req createSessionReq
	if err := decodeBody(r, &req); err != nil {
		writeErrorMsg(w, http.StatusBadRequest, "body inválido")
		return
	}
	rec, err := s.mgr.Create(control.CreateOpts{
		Title: req.Title, Description: req.Description, Cwd: req.Cwd,
		Cols: req.Cols, Rows: req.Rows,
	})
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, rec)
}

func (s *Server) handleGetSession(w http.ResponseWriter, r *http.Request) {
	rec, err := s.mgr.Get(r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rec)
}

func (s *Server) handlePatchSession(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Title        *string `json:"title"`
		Description  *string `json:"description"`
		WorkStatus   *string `json:"work_status"`
		KanbanStatus *string `json:"kanban_status"`
	}
	if err := decodeBody(r, &req); err != nil {
		writeErrorMsg(w, http.StatusBadRequest, "body inválido")
		return
	}
	if req.WorkStatus != nil && !workStatuses[*req.WorkStatus] {
		writeErrorMsg(w, http.StatusBadRequest, "work_status inválido")
		return
	}
	if req.KanbanStatus != nil && !kanbanStatuses[*req.KanbanStatus] {
		writeErrorMsg(w, http.StatusBadRequest, "kanban_status inválido")
		return
	}
	rec, err := s.mgr.UpdateMeta(r.PathValue("id"), store.MetaPatch{
		Title: req.Title, Description: req.Description,
		WorkStatus: req.WorkStatus, KanbanStatus: req.KanbanStatus,
	})
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rec)
}

// handleKillSession mata el proceso y conserva el historial.
func (s *Server) handleKillSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.mgr.Kill(id); err != nil {
		writeError(w, err)
		return
	}
	rec, err := s.mgr.Get(id)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rec)
}

func (s *Server) handleRestartSession(w http.ResponseWriter, r *http.Request) {
	var req sizeReq
	if err := decodeBody(r, &req); err != nil {
		writeErrorMsg(w, http.StatusBadRequest, "body inválido")
		return
	}
	rec, err := s.mgr.Restart(r.PathValue("id"), req.Cols, req.Rows)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rec)
}

// handleDeleteSession borra la fila con su KV y su historial. Es el acto
// destructivo, separado del kill a propósito.
func (s *Server) handleDeleteSession(w http.ResponseWriter, r *http.Request) {
	if err := s.mgr.Delete(r.PathValue("id")); err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleListKV(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.mgr.Get(id); err != nil {
		writeError(w, err)
		return
	}
	kv, err := s.mgr.ListKV(id)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, kv)
}

// handleSetKV toma el valor crudo del body: es más cómodo para el CLI de M5
// que envolverlo en un JSON.
func (s *Server) handleSetKV(w http.ResponseWriter, r *http.Request) {
	value, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
	if err != nil {
		writeErrorMsg(w, http.StatusBadRequest, "no se pudo leer el valor")
		return
	}
	if err := s.mgr.SetKV(r.PathValue("id"), r.PathValue("key"), string(value)); err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleDeleteKV(w http.ResponseWriter, r *http.Request) {
	if err := s.mgr.DeleteKV(r.PathValue("id"), r.PathValue("key")); err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
