package mcp

import (
	"context"
	"fmt"
	"strings"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/giuliano/webterm/internal/store"
)

// statusGuide es lo que el modelo lee para elegir el estado: los nombres solos
// no alcanzan para distinguir in_review de needs_testing.
const statusGuide = "todo (Not started): todavía no se arrancó. " +
	"in_progress (WIP): se está trabajando. " +
	"blocked (Blocked): no puede avanzar hasta que algo externo se destrabe. " +
	"in_review (In Review): hay un PR abierto esperando review o merge. " +
	"needs_testing (Needs Testing): mergeado o deployado, falta probarlo. " +
	"done (Done): terminado."

type getStatusArgs struct {
	SessionID string `json:"session_id,omitempty" jsonschema:"id de la sesión a consultar; si se omite, la sesión en la que estás corriendo"`
}

type setStatusArgs struct {
	Status    string `json:"status" jsonschema:"uno de: todo, in_progress, blocked, in_review, needs_testing, done"`
	SessionID string `json:"session_id,omitempty" jsonschema:"id de la sesión a actualizar; si se omite, la sesión en la que estás corriendo"`
}

// registerStatusTools agrega la lectura y escritura del estado de trabajo.
func (s *Server) registerStatusTools() {
	sdk.AddTool(s.mcp, &sdk.Tool{
		Name: "get_status",
		Description: "Lee el estado de trabajo de una sesión de WebTerm, por defecto la tuya. " +
			"Estados posibles: " + statusGuide,
	}, s.getStatus)

	sdk.AddTool(s.mcp, &sdk.Tool{
		Name: "set_status",
		Description: "Cambia el estado de trabajo de una sesión de WebTerm, por defecto la tuya. " +
			"Es lo que el usuario ve para saber en qué quedó cada sesión, así que conviene " +
			"moverlo cuando el trabajo cambia de etapa. Estados: " + statusGuide,
	}, s.setStatus)
}

// target resuelve sobre qué sesión opera una tool que acepta session_id: la
// nombrada si viene, la que llama si no.
func (s *Server) target(req *sdk.CallToolRequest, explicit string) (string, error) {
	id := strings.TrimSpace(explicit)
	if id == "" {
		return s.resolve(req)
	}
	if _, err := s.sessions.Get(id); err != nil {
		return "", fmt.Errorf("la sesión %s no existe", id)
	}
	return id, nil
}

func (s *Server) getStatus(_ context.Context, req *sdk.CallToolRequest, args getStatusArgs) (*sdk.CallToolResult, any, error) {
	id, err := s.target(req, args.SessionID)
	if err != nil {
		return nil, nil, err
	}
	sess, err := s.sessions.Get(id)
	if err != nil {
		return nil, nil, err
	}
	return text("la sesión %s está en %s", id, sess.KanbanStatus), nil, nil
}

func (s *Server) setStatus(_ context.Context, req *sdk.CallToolRequest, args setStatusArgs) (*sdk.CallToolResult, any, error) {
	status := strings.TrimSpace(args.Status)
	if !store.ValidKanbanStatus(status) {
		return nil, nil, fmt.Errorf("estado %q inválido; los válidos son: %s",
			args.Status, strings.Join(store.KanbanStatuses, ", "))
	}
	id, err := s.target(req, args.SessionID)
	if err != nil {
		return nil, nil, err
	}
	sess, err := s.sessions.UpdateMeta(id, store.MetaPatch{KanbanStatus: &status})
	if err != nil {
		return nil, nil, err
	}
	return text("la sesión %s quedó en %s", id, sess.KanbanStatus), nil, nil
}
