package resources

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// fakeRunner devuelve siempre lo mismo y registra con qué lo llamaron.
func fakeRunner(out []byte, err error, seen *[]string) runner {
	return func(_ context.Context, args ...string) ([]byte, error) {
		if seen != nil {
			*seen = append(*seen, args...)
		}
		return out, err
	}
}

func testRef(t *testing.T) Ref {
	t.Helper()
	ref, ok := NewGitHub().Match("https://github.com/cli/cli/pull/14456")
	if !ok {
		t.Fatal("no matcheó la URL de prueba")
	}
	return ref
}

func TestFetchArmaLaQuery(t *testing.T) {
	var seen []string
	g := newGitHubWith(fakeRunner(fixture(t, "pr_open.json"), nil, &seen))

	snap, err := g.Fetch(context.Background(), testRef(t))
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if snap.PR == nil || snap.PR.Number != 14456 {
		t.Fatalf("snapshot = %+v", snap)
	}
	if snap.FetchedAt == 0 {
		t.Fatal("el snapshot tiene que traer fetched_at")
	}
	if snap.Error != "" {
		t.Fatalf("no tendría que haber error: %q", snap.Error)
	}

	args := strings.Join(seen, " ")
	// Owner, repo y número tienen que viajar como variables de GraphQL y no
	// interpolados en la query: así una URL rara no puede inyectar nada.
	for _, want := range []string{"api", "graphql", "owner=cli", "name=cli", "number=14456"} {
		if !strings.Contains(args, want) {
			t.Errorf("falta %q en los argumentos: %v", want, seen)
		}
	}
}

// TestFetchErrorDeGitHub: el mensaje del sistema externo va al snapshot, no se
// pierde. gh escribe el JSON de error en stdout, así que hay que parsearlo
// aunque el comando haya devuelto un exit code distinto de cero.
func TestFetchErrorDeGitHub(t *testing.T) {
	g := newGitHubWith(fakeRunner(
		fixture(t, "pr_no_encontrado.json"),
		&exec.ExitError{},
		nil,
	))

	snap, err := g.Fetch(context.Background(), testRef(t))
	if err != nil {
		t.Fatalf("un error remoto va en el snapshot, no como error de Fetch: %v", err)
	}
	if snap.PR != nil {
		t.Fatal("con error no tendría que haber estado")
	}
	if !strings.Contains(snap.Error, "99999999") {
		t.Fatalf("se perdió el mensaje de GitHub: %q", snap.Error)
	}
}

// TestFetchSinGh: sin el CLI instalado, linkear sigue andando y el estado
// explica qué hacer.
func TestFetchSinGh(t *testing.T) {
	g := newGitHubWith(fakeRunner(nil, exec.ErrNotFound, nil))

	snap, err := g.Fetch(context.Background(), testRef(t))
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if !strings.Contains(snap.Error, "gh") {
		t.Fatalf("el error tendría que mencionar gh: %q", snap.Error)
	}
}

// TestFetchSinAutenticar: gh avisa por stderr; el mensaje tiene que llegar al
// usuario en vez de un "error desconocido".
func TestFetchSinAutenticar(t *testing.T) {
	g := newGitHubWith(fakeRunner(nil,
		errors.New("gh: To get started with GitHub CLI, please run: gh auth login"), nil))

	snap, err := g.Fetch(context.Background(), testRef(t))
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if !strings.Contains(snap.Error, "gh auth login") {
		t.Fatalf("no se propagó la instrucción: %q", snap.Error)
	}
}

func TestFetchRespetaElContexto(t *testing.T) {
	lento := func(ctx context.Context, _ ...string) ([]byte, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	g := newGitHubWith(lento)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	snap, err := g.Fetch(ctx, testRef(t))
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if snap.Error == "" {
		t.Fatal("el timeout tendría que quedar registrado en el snapshot")
	}
}

// TestFetchIntegracion es el único test que toca la red. Se saltea si no hay
// gh o no está autenticado, para que la suite corra en cualquier máquina.
func TestFetchIntegracion(t *testing.T) {
	if testing.Short() {
		t.Skip("modo -short")
	}
	if _, err := exec.LookPath("gh"); err != nil {
		t.Skip("gh no está instalado")
	}
	if err := exec.Command("gh", "auth", "status").Run(); err != nil {
		t.Skip("gh no está autenticado")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	g := NewGitHub()
	ref, _ := g.Match("https://github.com/cli/cli/pull/9000")
	snap, err := g.Fetch(ctx, ref)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if snap.Error != "" {
		t.Fatalf("error consultando un PR público real: %s", snap.Error)
	}
	if snap.PR == nil || snap.PR.Number != 9000 {
		t.Fatalf("snapshot = %+v", snap)
	}
	if snap.PR.State != "MERGED" {
		t.Fatalf("el PR 9000 de cli/cli está mergeado, vino %q", snap.PR.State)
	}
	// Ese PR tiene dos threads, uno sin resolver: es el dato que no existe en
	// la API REST y por el que esto es GraphQL.
	if snap.PR.UnresolvedCount != 1 {
		t.Fatalf("unresolved = %d, se esperaba 1", snap.PR.UnresolvedCount)
	}
}
