package mcp

import (
	"context"
	"fmt"
	"sort"
	"strings"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/giuliano/webterm/internal/store"
)

// Los argumentos de cada tool. El SDK deriva el JSON Schema de estos structs,
// así que los tags jsonschema son lo que ve el modelo al elegir la tool.

type setContextArgs struct {
	Key   string `json:"key" jsonschema:"clave del contexto, por ejemplo ticket o branch"`
	Value string `json:"value" jsonschema:"valor a guardar"`
}

type getContextArgs struct {
	Key string `json:"key,omitempty" jsonschema:"clave a leer; si se omite se devuelve todo el contexto"`
}

type setTitleArgs struct {
	Title       string `json:"title" jsonschema:"título corto que describa en qué trabaja la sesión"`
	Description string `json:"description,omitempty" jsonschema:"detalle opcional más largo"`
}

type linkPRArgs struct {
	URL string `json:"url" jsonschema:"URL del pull request de GitHub"`
}

type listLinksArgs struct{}

// registerTools declara las cinco tools. Ninguna borra nada: deslinkear y
// borrar sesiones son decisiones humanas y la UI ya las tiene.
func (s *Server) registerTools() {
	sdk.AddTool(s.mcp, &sdk.Tool{
		Name: "set_context",
		Description: "Guarda una clave en el contexto persistido de la sesión de WebTerm " +
			"en la que estás corriendo. Sirve para dejar anotado qué estás haciendo " +
			"(ticket, branch, decisiones) y que sobreviva a que te reinicien.",
	}, s.setContext)

	sdk.AddTool(s.mcp, &sdk.Tool{
		Name: "get_context",
		Description: "Lee el contexto persistido de la sesión de WebTerm en la que estás " +
			"corriendo. Sin argumentos devuelve todas las claves.",
	}, s.getContext)

	sdk.AddTool(s.mcp, &sdk.Tool{
		Name: "set_title",
		Description: "Nombra la sesión de WebTerm en la que estás corriendo. El título es lo " +
			"que el usuario ve en la lista de sesiones, así que conviene que diga de qué " +
			"trabajo se trata.",
	}, s.setTitle)

	sdk.AddTool(s.mcp, &sdk.Tool{
		Name: "link_pr",
		Description: "Linkea un pull request de GitHub a la sesión de WebTerm en la que estás " +
			"corriendo, para que el usuario vea su estado —review, checks y comments sin " +
			"resolver— al lado de la terminal.",
	}, s.linkPR)

	sdk.AddTool(s.mcp, &sdk.Tool{
		Name: "list_links",
		Description: "Lista los recursos linkeados a la sesión de WebTerm en la que estás " +
			"corriendo, con su estado actual. Útil para saber cómo vienen los checks de tu " +
			"propio PR sin salir de la terminal.",
	}, s.listLinks)
}

func (s *Server) setContext(_ context.Context, req *sdk.CallToolRequest, args setContextArgs) (*sdk.CallToolResult, any, error) {
	id, err := s.resolve(req)
	if err != nil {
		return nil, nil, err
	}
	key := strings.TrimSpace(args.Key)
	if key == "" {
		return nil, nil, fmt.Errorf("la clave no puede estar vacía")
	}
	if err := s.sessions.SetKV(id, key, args.Value); err != nil {
		return nil, nil, err
	}
	return text("guardado %s en la sesión %s", key, id), nil, nil
}

func (s *Server) getContext(_ context.Context, req *sdk.CallToolRequest, args getContextArgs) (*sdk.CallToolResult, any, error) {
	id, err := s.resolve(req)
	if err != nil {
		return nil, nil, err
	}
	kv, err := s.sessions.ListKV(id)
	if err != nil {
		return nil, nil, err
	}

	if key := strings.TrimSpace(args.Key); key != "" {
		v, ok := kv[key]
		if !ok {
			return text("la sesión %s no tiene la clave %s", id, key), nil, nil
		}
		return text("%s", v), nil, nil
	}

	if len(kv) == 0 {
		return text("la sesión %s todavía no tiene contexto guardado", id), nil, nil
	}
	// Orden estable: si no, dos llamadas seguidas devuelven lo mismo en
	// distinto orden y parece que cambió algo.
	claves := make([]string, 0, len(kv))
	for k := range kv {
		claves = append(claves, k)
	}
	sort.Strings(claves)

	var sb strings.Builder
	for _, k := range claves {
		fmt.Fprintf(&sb, "%s: %s\n", k, kv[k])
	}
	return text("%s", strings.TrimRight(sb.String(), "\n")), nil, nil
}

func (s *Server) setTitle(_ context.Context, req *sdk.CallToolRequest, args setTitleArgs) (*sdk.CallToolResult, any, error) {
	id, err := s.resolve(req)
	if err != nil {
		return nil, nil, err
	}
	title := strings.TrimSpace(args.Title)
	if title == "" {
		return nil, nil, fmt.Errorf("el título no puede estar vacío")
	}

	patch := store.MetaPatch{Title: &title}
	// La descripción solo se toca si la mandaron: así una llamada que solo
	// quiere renombrar no borra lo que ya había escrito.
	if d := strings.TrimSpace(args.Description); d != "" {
		patch.Description = &d
	}
	if _, err := s.sessions.UpdateMeta(id, patch); err != nil {
		return nil, nil, err
	}
	return text("la sesión %s ahora se llama %q", id, title), nil, nil
}

func (s *Server) linkPR(_ context.Context, req *sdk.CallToolRequest, args linkPRArgs) (*sdk.CallToolResult, any, error) {
	id, err := s.resolve(req)
	if err != nil {
		return nil, nil, err
	}
	// system y type los infiere el backend de la URL, igual que en el endpoint
	// REST: sumar otro sistema no cambia esta firma.
	r, err := s.sessions.AddResource(id, args.URL, "", "")
	if err != nil {
		return nil, nil, err
	}
	return text("linkeado %s a la sesión %s", r.Ref, id), nil, nil
}

func (s *Server) listLinks(ctx context.Context, req *sdk.CallToolRequest, _ listLinksArgs) (*sdk.CallToolResult, any, error) {
	id, err := s.resolve(req)
	if err != nil {
		return nil, nil, err
	}
	links, err := s.sessions.ListResources(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	if len(links) == 0 {
		return text("la sesión %s no tiene nada linkeado", id), nil, nil
	}

	var sb strings.Builder
	for _, l := range links {
		fmt.Fprintf(&sb, "- %s", l.Ref)
		switch {
		case l.Snapshot == nil:
		case l.Snapshot.Error != "":
			fmt.Fprintf(&sb, " (no se pudo consultar: %s)", l.Snapshot.Error)
		case l.Snapshot.PR != nil:
			pr := l.Snapshot.PR
			fmt.Fprintf(&sb, "\n  %s #%d %s", pr.Repo, pr.Number, pr.Title)
			fmt.Fprintf(&sb, "\n  estado: %s", estadoPR(pr.State, pr.IsDraft))
			if pr.ReviewDecision != "" {
				fmt.Fprintf(&sb, " · review: %s", pr.ReviewDecision)
			}
			if pr.ChecksTotal > 0 {
				fmt.Fprintf(&sb, "\n  checks: %d de %d ok", pr.ChecksSuccess+pr.ChecksSkipped, pr.ChecksTotal)
				if pr.ChecksFailing > 0 {
					fmt.Fprintf(&sb, ", %d fallando", pr.ChecksFailing)
				}
				if pr.ChecksPending > 0 {
					fmt.Fprintf(&sb, ", %d corriendo", pr.ChecksPending)
				}
			}
			if pr.UnresolvedCount > 0 {
				fmt.Fprintf(&sb, "\n  comments sin resolver: %d", pr.UnresolvedCount)
			}
		}
		sb.WriteString("\n")
	}
	return text("%s", strings.TrimRight(sb.String(), "\n")), nil, nil
}

func estadoPR(state string, draft bool) string {
	if draft && state == "OPEN" {
		return "borrador"
	}
	switch state {
	case "OPEN":
		return "abierto"
	case "MERGED":
		return "mergeado"
	case "CLOSED":
		return "cerrado"
	}
	return strings.ToLower(state)
}
