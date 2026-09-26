// Package mcp expone las sesiones de WebTerm por Model Context Protocol, para
// que el agente que corre dentro de una sesión pueda escribirle contexto,
// nombrarla y linkearle recursos sin salir de la terminal.
//
// Es un adaptador de protocolo: toda la lógica vive en el session manager, y
// este paquete solo traduce llamadas de tools a métodos suyos.
package mcp

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/giuliano/webterm/internal/control"
	"github.com/giuliano/webterm/internal/store"
)

// SessionHeader viaja en cada request con el id de la sesión sobre la que
// operar. Claude Code lo llena expandiendo ${WEBTERM_SESSION_ID} —la variable
// que el pty ya recibe— y la expansión ocurre en cada request, así que una sola
// configuración sirve para todas las sesiones.
const SessionHeader = "X-Webterm-Session"

// Sessions es lo que el servidor MCP necesita del manager. Se declara acá, del
// lado del consumidor, para no depender del struct concreto y poder testear
// las tools sin levantar ptys.
type Sessions interface {
	Get(id string) (*store.Session, error)
	SetKV(id, key, value string) error
	ListKV(id string) (map[string]string, error)
	UpdateMeta(id string, p store.MetaPatch) (*store.Session, error)
	AddResource(id, rawURL, system, typ string) (*store.Resource, error)
	ListResources(ctx context.Context, id string) ([]*control.LinkedResource, error)
	List() ([]*store.Session, error)
	ListFolders() ([]*store.Folder, error)
	CreateFolder(name string) (*store.Folder, error)
	SetSessionFolder(sessionID string, folderID *string) error
	ListTags() ([]store.TagCount, error)
	AddSessionTags(sessionID string, tags []string) error
	RemoveSessionTags(sessionID string, tags []string) error
}

// Server es el servidor MCP de WebTerm.
type Server struct {
	sessions Sessions
	mcp      *sdk.Server
}

// New construye el servidor con las tools registradas.
func New(sessions Sessions) *Server {
	s := &Server{
		sessions: sessions,
		mcp: sdk.NewServer(&sdk.Implementation{
			Name:    "webterm",
			Version: "1",
		}, nil),
	}
	s.registerTools()
	return s
}

// Handler devuelve el http.Handler para montar en /mcp.
//
// Va en modo Stateless: las tools son llamadas sueltas contra una sesión que
// identifica un header, así que no hay estado que arrastrar entre requests. De
// paso evita que convivan dos cosas distintas llamadas "sesión" —la de MCP y la
// de WebTerm— dentro del mismo servidor.
func (s *Server) Handler() http.Handler {
	return sdk.NewStreamableHTTPHandler(
		func(*http.Request) *sdk.Server { return s.mcp },
		&sdk.StreamableHTTPOptions{Stateless: true},
	)
}

// errNoSession lo devuelven las tools cuando no pueden saber sobre qué sesión
// operar.
var errNoSession = errors.New("sin sesión")

// sessionID saca el id de sesión de los headers del request HTTP original.
//
// Contempla el caso de Claude corriendo fuera de WebTerm: ahí la variable no
// existe y Claude Code manda el literal sin expandir, que parece un id pero no
// lo es. Sin este chequeo el error sería "la sesión ${WEBTERM_SESSION_ID} no
// existe", que manda a buscar el problema al lado equivocado.
func sessionID(req *sdk.CallToolRequest) (string, error) {
	if req.Extra == nil || req.Extra.Header == nil {
		return "", fmt.Errorf("%w: falta el header %s", errNoSession, SessionHeader)
	}
	id := strings.TrimSpace(req.Extra.Header.Get(SessionHeader))
	switch {
	case id == "":
		return "", fmt.Errorf("%w: falta el header %s; agregalo con "+
			`-H 'X-Webterm-Session: ${WEBTERM_SESSION_ID}'`, errNoSession, SessionHeader)
	case strings.Contains(id, "${"):
		return "", fmt.Errorf("%w: la variable WEBTERM_SESSION_ID no está definida, "+
			"así que Claude no está corriendo adentro de una sesión de WebTerm", errNoSession)
	}
	return id, nil
}

// resolve valida el header y que la sesión exista.
func (s *Server) resolve(req *sdk.CallToolRequest) (string, error) {
	id, err := sessionID(req)
	if err != nil {
		return "", err
	}
	if _, err := s.sessions.Get(id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return "", fmt.Errorf("la sesión %s no existe", id)
		}
		return "", err
	}
	return id, nil
}

// text arma el resultado de una tool que devuelve texto plano.
func text(format string, args ...any) *sdk.CallToolResult {
	return &sdk.CallToolResult{
		Content: []sdk.Content{&sdk.TextContent{Text: fmt.Sprintf(format, args...)}},
	}
}
