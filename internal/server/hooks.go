package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/giuliano/webterm/internal/control"
	webmcp "github.com/giuliano/webterm/internal/mcp"
	"github.com/giuliano/webterm/internal/store"
)

// maxHookBodyBytes es mucho más grande que maxBodyBytes porque el JSON de un
// hook trae el tool_input entero: un Write de un archivo grande supera
// holgado los 64KB, y cortarlo ahí lo volvería JSON inválido.
const maxHookBodyBytes = 16 << 20

// handleHook recibe los hooks http de Claude Code y mueve el work_status de la
// sesión desde la que corren. La sesión viene en el mismo header que usa el
// MCP, expandido por Claude Code desde WEBTERM_SESSION_ID.
//
// Contesta 204 sin body en todos los casos felices: Claude Code interpreta un
// body JSON como output del hook (decisiones, contexto para el modelo), y este
// endpoint no tiene nada que decirle. Los errores tampoco lo frenan —para
// Claude Code un hook http que falla es siempre no bloqueante— así que el
// status solo sirve para diagnosticar a mano.
func (s *Server) handleHook(w http.ResponseWriter, r *http.Request) {
	// Claude corriendo fuera de WebTerm: la config de hooks es global, pero
	// ahí la variable no existe y Claude Code la reemplaza por vacío. No es un
	// error, es el caso normal de cualquier terminal que no sea de WebTerm.
	id := strings.TrimSpace(r.Header.Get(webmcp.SessionHeader))
	if id == "" {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	var ev control.HookEvent
	if err := json.NewDecoder(io.LimitReader(r.Body, maxHookBodyBytes)).Decode(&ev); err != nil {
		writeErrorMsg(w, http.StatusBadRequest, "body inválido")
		return
	}
	if _, err := s.mgr.ApplyHook(id, ev); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErrorMsg(w, http.StatusNotFound, "la sesión "+id+" no existe")
			return
		}
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
