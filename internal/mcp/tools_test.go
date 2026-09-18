package mcp

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/giuliano/webterm/internal/resources"
	"github.com/giuliano/webterm/internal/session"
	"github.com/giuliano/webterm/internal/store"
)

// sessionsFalsas implementa Sessions en memoria: las tools se prueban sin DB,
// sin ptys y sin red.
type sessionsFalsas struct {
	existentes map[string]*store.Session
	kv         map[string]map[string]string
	links      []*session.LinkedResource
	errAdd     error
}

func nuevasSesiones() *sessionsFalsas {
	return &sessionsFalsas{
		existentes: map[string]*store.Session{
			"s1": {ID: "s1", Title: "vieja", Description: "ya escrita", Cwd: "/tmp"},
		},
		kv: map[string]map[string]string{},
	}
}

func (f *sessionsFalsas) Get(id string) (*store.Session, error) {
	s, ok := f.existentes[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	return s, nil
}

func (f *sessionsFalsas) SetKV(id, key, value string) error {
	if f.kv[id] == nil {
		f.kv[id] = map[string]string{}
	}
	f.kv[id][key] = value
	return nil
}

func (f *sessionsFalsas) ListKV(id string) (map[string]string, error) {
	if f.kv[id] == nil {
		return map[string]string{}, nil
	}
	return f.kv[id], nil
}

func (f *sessionsFalsas) UpdateMeta(id string, p store.MetaPatch) (*store.Session, error) {
	s := f.existentes[id]
	if p.Title != nil {
		s.Title = *p.Title
	}
	if p.Description != nil {
		s.Description = *p.Description
	}
	return s, nil
}

func (f *sessionsFalsas) AddResource(id, rawURL, system, typ string) (*store.Resource, error) {
	if f.errAdd != nil {
		return nil, f.errAdd
	}
	return &store.Resource{ID: 1, SessionID: id, System: "gh", Type: "pr", Ref: rawURL}, nil
}

func (f *sessionsFalsas) ListResources(context.Context, string) ([]*session.LinkedResource, error) {
	return f.links, nil
}

// reqCon arma un CallToolRequest con el header de sesión que se quiera.
func reqCon(valor string) *sdk.CallToolRequest {
	h := http.Header{}
	if valor != "" {
		h.Set(SessionHeader, valor)
	}
	return &sdk.CallToolRequest{Extra: &sdk.RequestExtra{Header: h}}
}

func soloTexto(t *testing.T, res *sdk.CallToolResult) string {
	t.Helper()
	if res == nil || len(res.Content) == 0 {
		t.Fatal("la tool no devolvió contenido")
	}
	tc, ok := res.Content[0].(*sdk.TextContent)
	if !ok {
		t.Fatalf("se esperaba texto, vino %T", res.Content[0])
	}
	return tc.Text
}

func TestSetYGetContext(t *testing.T) {
	f := nuevasSesiones()
	s := New(f)

	if _, _, err := s.setContext(context.Background(), reqCon("s1"),
		setContextArgs{Key: "ticket", Value: "ABC-2425"}); err != nil {
		t.Fatalf("set_context: %v", err)
	}
	if f.kv["s1"]["ticket"] != "ABC-2425" {
		t.Fatalf("no se guardó: %v", f.kv)
	}

	// Una clave puntual.
	res, _, err := s.getContext(context.Background(), reqCon("s1"), getContextArgs{Key: "ticket"})
	if err != nil {
		t.Fatalf("get_context: %v", err)
	}
	if soloTexto(t, res) != "ABC-2425" {
		t.Fatalf("get_context = %q", soloTexto(t, res))
	}

	// Todo el contexto, en orden estable.
	_ = f.SetKV("s1", "branch", "fix/token")
	res, _, _ = s.getContext(context.Background(), reqCon("s1"), getContextArgs{})
	got := soloTexto(t, res)
	if got != "branch: fix/token\nticket: ABC-2425" {
		t.Fatalf("get_context sin clave = %q", got)
	}
}

func TestGetContextVacioYClaveInexistente(t *testing.T) {
	s := New(nuevasSesiones())

	res, _, _ := s.getContext(context.Background(), reqCon("s1"), getContextArgs{})
	if !strings.Contains(soloTexto(t, res), "todavía no tiene contexto") {
		t.Fatalf("contexto vacío = %q", soloTexto(t, res))
	}
	res, _, _ = s.getContext(context.Background(), reqCon("s1"), getContextArgs{Key: "nope"})
	if !strings.Contains(soloTexto(t, res), "no tiene la clave") {
		t.Fatalf("clave inexistente = %q", soloTexto(t, res))
	}
}

func TestSetContextRechazaClaveVacia(t *testing.T) {
	s := New(nuevasSesiones())
	if _, _, err := s.setContext(context.Background(), reqCon("s1"),
		setContextArgs{Key: "  ", Value: "x"}); err == nil {
		t.Fatal("se esperaba un error")
	}
}

// TestSetTitleNoPisaLaDescripcion: una llamada que solo renombra no puede
// borrar lo que ya estaba escrito en la descripción.
func TestSetTitleNoPisaLaDescripcion(t *testing.T) {
	f := nuevasSesiones()
	s := New(f)

	if _, _, err := s.setTitle(context.Background(), reqCon("s1"),
		setTitleArgs{Title: "arreglar el token"}); err != nil {
		t.Fatalf("set_title: %v", err)
	}
	if f.existentes["s1"].Title != "arreglar el token" {
		t.Fatalf("title = %q", f.existentes["s1"].Title)
	}
	if f.existentes["s1"].Description != "ya escrita" {
		t.Fatalf("se pisó la descripción: %q", f.existentes["s1"].Description)
	}

	// Y si la mandan, sí se escribe.
	_, _, _ = s.setTitle(context.Background(), reqCon("s1"),
		setTitleArgs{Title: "t", Description: "nueva"})
	if f.existentes["s1"].Description != "nueva" {
		t.Fatalf("description = %q", f.existentes["s1"].Description)
	}
}

func TestSetTitleRechazaVacio(t *testing.T) {
	s := New(nuevasSesiones())
	if _, _, err := s.setTitle(context.Background(), reqCon("s1"),
		setTitleArgs{Title: "   "}); err == nil {
		t.Fatal("se esperaba un error")
	}
}

func TestLinkPR(t *testing.T) {
	f := nuevasSesiones()
	s := New(f)

	res, _, err := s.linkPR(context.Background(), reqCon("s1"),
		linkPRArgs{URL: "https://github.com/o/r/pull/1"})
	if err != nil {
		t.Fatalf("link_pr: %v", err)
	}
	if !strings.Contains(soloTexto(t, res), "github.com/o/r/pull/1") {
		t.Fatalf("link_pr = %q", soloTexto(t, res))
	}

	// El error del manager llega tal cual: "ya está linkeado" es más útil que
	// un fallo genérico.
	f.errAdd = store.ErrDuplicate
	if _, _, err := s.linkPR(context.Background(), reqCon("s1"),
		linkPRArgs{URL: "https://github.com/o/r/pull/1"}); !errors.Is(err, store.ErrDuplicate) {
		t.Fatalf("se esperaba ErrDuplicate, vino %v", err)
	}
}

func TestListLinks(t *testing.T) {
	f := nuevasSesiones()
	f.links = []*session.LinkedResource{{
		Resource: &store.Resource{ID: 1, Ref: "https://github.com/o/r/pull/7"},
		Snapshot: &resources.Snapshot{PR: &resources.PRState{
			Repo: "o/r", Number: 7, Title: "un fix", State: "OPEN",
			ReviewDecision: "APPROVED", ChecksTotal: 5, ChecksSuccess: 3,
			ChecksSkipped: 1, ChecksFailing: 1, UnresolvedCount: 2,
		}},
	}}
	s := New(f)

	res, _, err := s.listLinks(context.Background(), reqCon("s1"), listLinksArgs{})
	if err != nil {
		t.Fatalf("list_links: %v", err)
	}
	got := soloTexto(t, res)
	for _, quiero := range []string{"o/r #7", "un fix", "abierto", "APPROVED",
		"4 de 5 ok", "1 fallando", "comments sin resolver: 2"} {
		if !strings.Contains(got, quiero) {
			t.Errorf("falta %q en:\n%s", quiero, got)
		}
	}
}

func TestListLinksVacio(t *testing.T) {
	s := New(nuevasSesiones())
	res, _, _ := s.listLinks(context.Background(), reqCon("s1"), listLinksArgs{})
	if !strings.Contains(soloTexto(t, res), "no tiene nada linkeado") {
		t.Fatalf("= %q", soloTexto(t, res))
	}
}

// TestSinHeaderDeSesion: sin saber sobre qué sesión operar, la tool tiene que
// decir cómo configurarlo.
func TestSinHeaderDeSesion(t *testing.T) {
	s := New(nuevasSesiones())
	_, _, err := s.setContext(context.Background(), reqCon(""), setContextArgs{Key: "k", Value: "v"})
	if err == nil {
		t.Fatal("se esperaba un error")
	}
	if !strings.Contains(err.Error(), SessionHeader) {
		t.Fatalf("el error tendría que nombrar el header: %v", err)
	}
}

// TestLiteralSinExpandir es el caso que reveló la prueba contra Claude Code:
// corriendo fuera de una sesión de WebTerm la variable no existe y llega el
// literal, que parece un id pero no lo es. Decir "la sesión
// ${WEBTERM_SESSION_ID} no existe" mandaría a buscar el problema al lado
// equivocado.
func TestLiteralSinExpandir(t *testing.T) {
	s := New(nuevasSesiones())
	_, _, err := s.setContext(context.Background(), reqCon("${WEBTERM_SESSION_ID}"),
		setContextArgs{Key: "k", Value: "v"})
	if err == nil {
		t.Fatal("se esperaba un error")
	}
	if !strings.Contains(err.Error(), "adentro de una sesión de WebTerm") {
		t.Fatalf("mensaje poco útil: %v", err)
	}
	if strings.Contains(err.Error(), "no existe") {
		t.Fatalf("no tiene que parecer una sesión inexistente: %v", err)
	}
}

func TestSesionInexistente(t *testing.T) {
	s := New(nuevasSesiones())
	_, _, err := s.setContext(context.Background(), reqCon("fantasma"),
		setContextArgs{Key: "k", Value: "v"})
	if err == nil || !strings.Contains(err.Error(), "fantasma") {
		t.Fatalf("el error tendría que incluir el id recibido: %v", err)
	}
}
