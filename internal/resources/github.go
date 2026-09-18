package resources

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	neturl "net/url"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// prPath captura owner, repo y número de un path de PR. Lo que venga después
// del número (/files, /commits/..., el fragmento) se descarta al normalizar.
//
// El owner y el repo se restringen al alfabeto que GitHub permite, para que
// una URL rara no se cuele como si fuera un repo.
var prPath = regexp.MustCompile(`^/([A-Za-z0-9_.-]+)/([A-Za-z0-9_.-]+)/pull/([0-9]+)(?:/|$)`)

// ghTimeout acota lo que puede tardar el subproceso.
const ghTimeout = 10 * time.Second

// runner ejecuta el gh CLI. Se inyecta para que los tests no dependan de la red.
type runner func(ctx context.Context, args ...string) ([]byte, error)

// GitHub es el provider de pull requests.
type GitHub struct {
	run runner
}

// NewGitHub construye el provider con el gh real.
func NewGitHub() *GitHub { return &GitHub{run: ghRunner} }

// newGitHubWith inyecta un runner. Es para los tests: así el paquete entero se
// prueba sin red.
func newGitHubWith(run runner) *GitHub { return &GitHub{run: run} }

// Match reconoce una URL de PR y devuelve su forma canónica.
//
// Acepta lo que uno copia del browser: con o sin esquema, con www, con sufijos
// como /files o un fragmento de comment. Rechaza todo lo demás en vez de
// intentar adivinar.
func (g *GitHub) Match(rawURL string) (Ref, bool) {
	s := strings.TrimSpace(rawURL)
	if s == "" {
		return Ref{}, false
	}
	// Sin esquema, url.Parse mete todo en Path; se lo agregamos.
	if !strings.Contains(s, "://") {
		s = "https://" + s
	}
	u, err := neturl.Parse(s)
	if err != nil {
		return Ref{}, false
	}
	host := strings.ToLower(u.Hostname())
	if host != "github.com" && host != "www.github.com" {
		return Ref{}, false
	}
	m := prPath.FindStringSubmatch(u.Path)
	if m == nil {
		return Ref{}, false
	}
	return Ref{
		System: "gh",
		Type:   "pr",
		URL:    fmt.Sprintf("https://github.com/%s/%s/pull/%s", m[1], m[2], m[3]),
	}, true
}

// parsePRRef vuelve de la URL canónica a sus partes, para armar la query.
func parsePRRef(ref Ref) (owner, repo string, number int, err error) {
	u, perr := neturl.Parse(ref.URL)
	if perr != nil {
		return "", "", 0, fmt.Errorf("ref inválido %q: %w", ref.URL, perr)
	}
	m := prPath.FindStringSubmatch(u.Path)
	if m == nil {
		return "", "", 0, fmt.Errorf("ref inválido %q", ref.URL)
	}
	n, perr := strconv.Atoi(m[3])
	if perr != nil {
		return "", "", 0, fmt.Errorf("número de PR inválido en %q", ref.URL)
	}
	return m[1], m[2], n, nil
}

// prQuery trae en una sola llamada todo lo que muestra la card.
//
// Es GraphQL y no REST por una razón concreta: la cuenta de comments sin
// resolver no existe en REST. Los review threads con isResolved solo están
// acá; por REST habría que paginar comments e inferirlo, y saldría mal.
const prQuery = `
query($owner:String!, $name:String!, $number:Int!) {
  repository(owner:$owner, name:$name) {
    nameWithOwner
    pullRequest(number:$number) {
      number title url state isDraft mergeable reviewDecision
      author { login }
      reviewThreads(first:100) { totalCount nodes { isResolved } }
      commits(last:1) { nodes { commit { statusCheckRollup {
        state
        contexts(first:100) { totalCount nodes {
          __typename
          ... on CheckRun { name conclusion status }
          ... on StatusContext { context state }
        } }
      } } } }
    }
  }
}`

// ghRunner ejecuta el gh CLI.
//
// Se shellea a gh en vez de manejar un token propio: ya está autenticado en la
// máquina y maneja el keyring y el refresh. Los argumentos van como argv
// separado, así que no hay inyección posible desde la URL.
func ghRunner(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "gh", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	if err != nil && stdout.Len() == 0 {
		// Sin nada en stdout, el único diagnóstico es lo que gh dijo por
		// stderr: que no está autenticado, que no encuentra el repo, etc.
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return nil, errors.New(msg)
		}
		return nil, err
	}
	// Con salida en stdout devolvemos el body aunque el exit code sea != 0:
	// ante un error de GraphQL, gh escribe ahí el JSON con la clave `errors`,
	// que tiene mejor mensaje que el de stderr.
	return stdout.Bytes(), nil
}

// Fetch consulta el estado actual del PR.
//
// Los problemas del sistema externo —PR inexistente, sin permisos, gh sin
// autenticar— vuelven dentro del Snapshot y no como error de Fetch: son estado
// que la card tiene que mostrar, no fallas de esta capa.
func (g *GitHub) Fetch(ctx context.Context, ref Ref) (*Snapshot, error) {
	owner, repo, number, err := parsePRRef(ref)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(ctx, ghTimeout)
	defer cancel()

	out, runErr := g.run(ctx,
		"api", "graphql",
		"-F", "owner="+owner,
		"-F", "name="+repo,
		"-F", "number="+strconv.Itoa(number),
		"-f", "query="+prQuery,
	)

	snap := &Snapshot{FetchedAt: time.Now().UnixMilli()}

	if len(out) == 0 {
		snap.Error = describeRunError(runErr)
		return snap, nil
	}
	pr, perr := parsePRResponse(out)
	if perr != nil {
		snap.Error = perr.Error()
		return snap, nil
	}
	snap.PR = pr
	return snap, nil
}

// describeRunError traduce la falla del subproceso a algo accionable.
func describeRunError(err error) string {
	switch {
	case err == nil:
		return "GitHub no devolvió respuesta"
	case errors.Is(err, exec.ErrNotFound):
		return "el gh CLI no está instalado: instalalo para ver el estado del PR"
	case errors.Is(err, context.DeadlineExceeded):
		return "GitHub no respondió a tiempo"
	case errors.Is(err, context.Canceled):
		return "consulta cancelada"
	default:
		// gh ya explica bien lo suyo (por ejemplo "please run: gh auth login").
		return err.Error()
	}
}
