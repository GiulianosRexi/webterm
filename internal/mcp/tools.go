package mcp

import (
	"context"
	"errors"
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

	s.registerFolderTools()
	s.registerTagTools()
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

type listFoldersArgs struct{}

type createFolderArgs struct {
	Name string `json:"name" jsonschema:"nombre del folder; uno por proyecto"`
}

type moveSessionArgs struct {
	Folder string `json:"folder" jsonschema:"nombre o id del folder destino; vacío saca la sesión de su folder"`
	// Por defecto se mueve la sesión que llama, que es el caso normal. El id
	// explícito está para cuando el usuario pide reorganizar varias de una.
	SessionID string `json:"session_id,omitempty" jsonschema:"id de la sesión a mover; si se omite, la sesión en la que estás corriendo"`
}

// registerFolderTools agrega el manejo de folders.
//
// Borrar folders no está: es la única operación destructiva del conjunto y la
// UI la tiene, con la confirmación a la vista.
func (s *Server) registerFolderTools() {
	sdk.AddTool(s.mcp, &sdk.Tool{
		Name: "list_folders",
		Description: "Lista los folders de WebTerm con cuántas sesiones tiene cada uno, y " +
			"marca en cuál está la sesión en la que estás corriendo. Conviene llamarla " +
			"antes de crear o mover: así se reusa el folder que ya existe en vez de " +
			"crear un duplicado con otro nombre.",
	}, s.listFolders)

	sdk.AddTool(s.mcp, &sdk.Tool{
		Name: "create_folder",
		Description: "Crea un folder en WebTerm. Un folder es un proyecto y agrupa sus " +
			"sesiones. Falla si ya existe uno con ese nombre, sin distinguir mayúsculas.",
	}, s.createFolder)

	sdk.AddTool(s.mcp, &sdk.Tool{
		Name: "move_session",
		Description: "Mueve una sesión de WebTerm a un folder, o la saca de su folder si " +
			"no se indica ninguno. El folder tiene que existir: no lo crea al vuelo, " +
			"para que un nombre mal escrito falle en vez de generar un duplicado.",
	}, s.moveSession)
}

func (s *Server) listFolders(_ context.Context, req *sdk.CallToolRequest, _ listFoldersArgs) (*sdk.CallToolResult, any, error) {
	folders, err := s.sessions.ListFolders()
	if err != nil {
		return nil, nil, err
	}
	if len(folders) == 0 {
		return text("no hay folders todavía"), nil, nil
	}

	sessions, err := s.sessions.List()
	if err != nil {
		return nil, nil, err
	}
	counts := map[string]int{}
	for _, sess := range sessions {
		if sess.FolderID != nil {
			counts[*sess.FolderID]++
		}
	}

	// En qué folder está la sesión que llama. Sin esto la tool no puede
	// contestar "¿dónde estoy?", que es lo primero que hay que saber para
	// decidir si mover algo —y el motivo por el que esta tool existe es
	// justamente que el modelo vea el estado antes de actuar.
	callerFolder := ""
	if id, rerr := s.resolve(req); rerr == nil {
		for _, sess := range sessions {
			if sess.ID == id && sess.FolderID != nil {
				callerFolder = *sess.FolderID
			}
		}
	}

	var b strings.Builder
	for _, f := range folders {
		fmt.Fprintf(&b, "%s (id %s): %s", f.Name, f.ID, pluralize(counts[f.ID], "sesión", "sesiones"))
		if f.ID == callerFolder {
			b.WriteString("  <- esta sesión está acá")
		}
		b.WriteString("\n")
	}
	if callerFolder == "" {
		b.WriteString("esta sesión no está en ningún folder\n")
	}
	return text("%s", strings.TrimRight(b.String(), "\n")), nil, nil
}

// pluralize arma "1 sesión" / "2 sesiones". Existe porque el texto lo lee un
// modelo y después se lo repite al usuario: "1 sesiones" se propaga.
func pluralize(n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s", one)
	}
	return fmt.Sprintf("%d %s", n, many)
}

func (s *Server) createFolder(_ context.Context, _ *sdk.CallToolRequest, args createFolderArgs) (*sdk.CallToolResult, any, error) {
	f, err := s.sessions.CreateFolder(args.Name)
	if errors.Is(err, store.ErrDuplicate) {
		// El error dice cuál es el que ya existe: así la salida natural es
		// usar ese, en vez de reintentar con una variante del nombre.
		if existente, lerr := s.folderPorNombre(args.Name); lerr == nil && existente != nil {
			return nil, nil, fmt.Errorf("ya existe el folder %q (id %s); usá ese", existente.Name, existente.ID)
		}
		return nil, nil, fmt.Errorf("ya existe un folder llamado %q", args.Name)
	}
	if err != nil {
		return nil, nil, err
	}
	return text("folder %q creado (id %s)", f.Name, f.ID), nil, nil
}

func (s *Server) moveSession(_ context.Context, req *sdk.CallToolRequest, args moveSessionArgs) (*sdk.CallToolResult, any, error) {
	id := strings.TrimSpace(args.SessionID)
	if id == "" {
		var err error
		if id, err = s.resolve(req); err != nil {
			return nil, nil, err
		}
	} else if _, err := s.sessions.Get(id); err != nil {
		return nil, nil, fmt.Errorf("la sesión %s no existe", id)
	}

	destino := strings.TrimSpace(args.Folder)
	if destino == "" {
		if err := s.sessions.SetSessionFolder(id, nil); err != nil {
			return nil, nil, err
		}
		return text("la sesión %s quedó sin folder", id), nil, nil
	}

	f, err := s.folderPorNombre(destino)
	if err != nil {
		return nil, nil, err
	}
	if f == nil {
		disponibles, lerr := s.sessions.ListFolders()
		if lerr != nil || len(disponibles) == 0 {
			return nil, nil, fmt.Errorf("no existe el folder %q y no hay ninguno creado", destino)
		}
		nombres := make([]string, 0, len(disponibles))
		for _, d := range disponibles {
			nombres = append(nombres, d.Name)
		}
		return nil, nil, fmt.Errorf("no existe el folder %q; los que hay son: %s",
			destino, strings.Join(nombres, ", "))
	}

	if err := s.sessions.SetSessionFolder(id, &f.ID); err != nil {
		return nil, nil, err
	}
	return text("la sesión %s quedó en %q", id, f.Name), nil, nil
}

// folderPorNombre busca por id exacto o por nombre sin distinguir mayúsculas.
// Devuelve nil sin error cuando no hay ninguno, para que quien llama decida
// qué decir.
func (s *Server) folderPorNombre(nombreOID string) (*store.Folder, error) {
	folders, err := s.sessions.ListFolders()
	if err != nil {
		return nil, err
	}
	for _, f := range folders {
		if f.ID == nombreOID || strings.EqualFold(f.Name, nombreOID) {
			return f, nil
		}
	}
	return nil, nil
}
