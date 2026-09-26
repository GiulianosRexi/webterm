package mcp

import (
	"context"
	"fmt"
	"strings"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/giuliano/webterm/internal/store"
)

type listTagsArgs struct{}

type tagSessionArgs struct {
	Add    []string `json:"add,omitempty" jsonschema:"tags a agregar, por ejemplo bugfix, consulta, implementación o brainstorming"`
	Remove []string `json:"remove,omitempty" jsonschema:"tags a sacar"`
	// Mismo criterio que move_session: por defecto la sesión que llama.
	SessionID string `json:"session_id,omitempty" jsonschema:"id de la sesión a taggear; si se omite, la sesión en la que estás corriendo"`
}

// registerTagTools agrega el manejo de tags.
//
// No hay una tool para reemplazar el conjunto entero a propósito: agregar y
// sacar por separado hace imposible que Claude borre sin querer los tags que
// puso el usuario, que es lo que pasaría con un "set" armado sin mirar.
func (s *Server) registerTagTools() {
	sdk.AddTool(s.mcp, &sdk.Tool{
		Name: "list_tags",
		Description: "Lista los tags de WebTerm en uso, con cuántas sesiones lleva cada uno, " +
			"y marca los que tiene la sesión en la que estás corriendo. Los tags dicen " +
			"qué clase de trabajo es una sesión (bugfix, consulta, implementación…). " +
			"Conviene llamarla antes de taggear, para reusar un tag existente en vez de " +
			"inventar una variante.",
	}, s.listTags)

	sdk.AddTool(s.mcp, &sdk.Tool{
		Name: "tag_session",
		Description: "Agrega y/o saca tags de una sesión de WebTerm, por defecto la tuya. " +
			"Los tags se normalizan a minúsculas con guiones. No toca los tags que no " +
			"se nombran.",
	}, s.tagSession)
}

func (s *Server) listTags(_ context.Context, req *sdk.CallToolRequest, _ listTagsArgs) (*sdk.CallToolResult, any, error) {
	tags, err := s.sessions.ListTags()
	if err != nil {
		return nil, nil, err
	}
	if len(tags) == 0 {
		return text("no hay tags todavía"), nil, nil
	}

	mine := map[string]bool{}
	if id, rerr := s.resolve(req); rerr == nil {
		if sess, gerr := s.sessions.Get(id); gerr == nil {
			for _, t := range sess.Tags {
				mine[t] = true
			}
		}
	}

	var b strings.Builder
	for _, t := range tags {
		fmt.Fprintf(&b, "%s: %s", t.Name, pluralize(t.Count, "sesión", "sesiones"))
		if mine[t.Name] {
			b.WriteString("  <- esta sesión lo tiene")
		}
		b.WriteString("\n")
	}
	return text("%s", strings.TrimRight(b.String(), "\n")), nil, nil
}

func (s *Server) tagSession(_ context.Context, req *sdk.CallToolRequest, args tagSessionArgs) (*sdk.CallToolResult, any, error) {
	if len(args.Add) == 0 && len(args.Remove) == 0 {
		return nil, nil, fmt.Errorf("no hay nada que hacer: pasá tags en add o en remove")
	}

	id := strings.TrimSpace(args.SessionID)
	if id == "" {
		var err error
		if id, err = s.resolve(req); err != nil {
			return nil, nil, err
		}
	} else if _, err := s.sessions.Get(id); err != nil {
		return nil, nil, fmt.Errorf("la sesión %s no existe", id)
	}

	// Qué tags existían antes, para avisar cuáles se estrenan: un tag nuevo
	// casi parecido a uno existente es justo el duplicado que hay que evitar, y
	// el aviso le da al modelo la chance de corregirlo.
	before, err := s.sessions.ListTags()
	if err != nil {
		return nil, nil, err
	}
	existed := map[string]bool{}
	for _, t := range before {
		existed[t.Name] = true
	}

	if len(args.Add) > 0 {
		if err := s.sessions.AddSessionTags(id, args.Add); err != nil {
			return nil, nil, err
		}
	}
	if len(args.Remove) > 0 {
		if err := s.sessions.RemoveSessionTags(id, args.Remove); err != nil {
			return nil, nil, err
		}
	}

	sess, err := s.sessions.Get(id)
	if err != nil {
		return nil, nil, err
	}

	var b strings.Builder
	if len(sess.Tags) == 0 {
		fmt.Fprintf(&b, "la sesión %s quedó sin tags", id)
	} else {
		fmt.Fprintf(&b, "la sesión %s quedó con: %s", id, strings.Join(sess.Tags, ", "))
	}

	var fresh []string
	for _, t := range args.Add {
		if clean, nerr := store.NormalizeTag(t); nerr == nil && !existed[clean] {
			fresh = append(fresh, clean)
		}
	}
	if len(fresh) > 0 && len(before) > 0 {
		names := make([]string, 0, len(before))
		for _, t := range before {
			names = append(names, t.Name)
		}
		fmt.Fprintf(&b, "\ntags nuevos: %s (los que ya existían: %s)",
			strings.Join(fresh, ", "), strings.Join(names, ", "))
	}
	return text("%s", b.String()), nil, nil
}
