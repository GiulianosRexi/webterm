package resources

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("leyendo fixture %s: %v", name, err)
	}
	return b
}

func TestParsePROpenConChecks(t *testing.T) {
	pr, err := parsePRResponse(fixture(t, "pr_open.json"))
	if err != nil {
		t.Fatalf("parsePRResponse: %v", err)
	}
	if pr.Number != 14456 || pr.Author != "babakks" {
		t.Fatalf("cabecera mal: %+v", pr)
	}
	if pr.Repo != "cli/cli" {
		t.Fatalf("repo = %q", pr.Repo)
	}
	if pr.State != "OPEN" || pr.IsDraft || pr.Mergeable != "MERGEABLE" {
		t.Fatalf("estado mal: %+v", pr)
	}
	if pr.ReviewDecision != "REVIEW_REQUIRED" {
		t.Fatalf("review_decision = %q", pr.ReviewDecision)
	}
	if pr.ChecksState != "FAILURE" || pr.ChecksTotal != 4 {
		t.Fatalf("checks = %s %d", pr.ChecksState, pr.ChecksTotal)
	}
	// FAILURE del CheckRun y ERROR del StatusContext cuentan; SKIPPED no.
	if pr.ChecksFailing != 2 {
		t.Fatalf("checks_failing = %d, se esperaban 2", pr.ChecksFailing)
	}
	if pr.ChecksSuccess != 1 || pr.ChecksSkipped != 1 {
		t.Fatalf("success=%d skipped=%d", pr.ChecksSuccess, pr.ChecksSkipped)
	}
	if pr.UnresolvedCount != 0 {
		t.Fatalf("unresolved = %d", pr.UnresolvedCount)
	}
}

// TestParsePRSinChecks: statusCheckRollup null es un caso real —PRs viejos con
// los checks expirados— y no puede romper el parseo.
func TestParsePRSinChecks(t *testing.T) {
	pr, err := parsePRResponse(fixture(t, "pr_merged_sin_checks.json"))
	if err != nil {
		t.Fatalf("parsePRResponse: %v", err)
	}
	if pr.State != "MERGED" || pr.ReviewDecision != "APPROVED" {
		t.Fatalf("estado mal: %+v", pr)
	}
	if pr.ChecksState != "" || pr.ChecksTotal != 0 || pr.ChecksSuccess != 0 {
		t.Fatalf("sin checks tendría que quedar todo en cero: %+v", pr)
	}
	// De dos threads, uno sin resolver.
	if pr.UnresolvedCount != 1 {
		t.Fatalf("unresolved = %d, se esperaba 1", pr.UnresolvedCount)
	}
	if pr.ThreadsTruncated {
		t.Fatal("2 threads no se truncan")
	}
}

// TestParsePRErrorDeGraphQL: gh escribe el JSON con la clave errors en stdout
// aunque falle, así que el parseo tiene que mirar ahí y no solo el exit code.
func TestParsePRErrorDeGraphQL(t *testing.T) {
	_, err := parsePRResponse(fixture(t, "pr_no_encontrado.json"))
	if err == nil {
		t.Fatal("se esperaba un error")
	}
	if !errors.Is(err, ErrRemote) {
		t.Fatalf("se esperaba ErrRemote, vino %v", err)
	}
	if !strings.Contains(err.Error(), "99999999") {
		t.Fatalf("el mensaje de GitHub tiene que sobrevivir: %v", err)
	}
}

func TestParsePRBasura(t *testing.T) {
	if _, err := parsePRResponse([]byte("esto no es json")); err == nil {
		t.Fatal("se esperaba un error")
	}
}

// TestParsePRThreadsTruncados: con más de 100 threads la query trunca, y la
// card tiene que poder decir "100+" en vez de mentir con un número bajo.
func TestParsePRThreadsTruncados(t *testing.T) {
	var sb strings.Builder
	sb.WriteString(`{"data":{"repository":{"pullRequest":{"number":1,"title":"t","url":"u",`)
	sb.WriteString(`"state":"OPEN","isDraft":false,"mergeable":"MERGEABLE","reviewDecision":"",`)
	sb.WriteString(`"author":{"login":"a"},"reviewThreads":{"totalCount":150,"nodes":[`)
	for i := 0; i < 100; i++ {
		if i > 0 {
			sb.WriteString(",")
		}
		sb.WriteString(`{"isResolved":false}`)
	}
	sb.WriteString(`]},"commits":{"nodes":[{"commit":{"statusCheckRollup":null}}]}}}}}`)

	pr, err := parsePRResponse([]byte(sb.String()))
	if err != nil {
		t.Fatalf("parsePRResponse: %v", err)
	}
	if pr.UnresolvedCount != 100 || !pr.ThreadsTruncated {
		t.Fatalf("unresolved=%d truncated=%v", pr.UnresolvedCount, pr.ThreadsTruncated)
	}
}

// TestParsePRDesgloseDeChecks reproduce el resumen que muestra GitHub
// ("1 skipped, 1 expected, 28 successful checks"): colapsar las categorías en
// un solo número pierde justo lo que uno mira, porque no es lo mismo que
// falten checks por correr que que estén salteados.
func TestParsePRDesgloseDeChecks(t *testing.T) {
	body := []byte(`{"data":{"repository":{"nameWithOwner":"o/r","pullRequest":{
	  "number":1,"title":"t","url":"u","state":"OPEN","isDraft":false,
	  "mergeable":"MERGEABLE","reviewDecision":"","author":{"login":"a"},
	  "reviewThreads":{"totalCount":0,"nodes":[]},
	  "commits":{"nodes":[{"commit":{"statusCheckRollup":{"state":"PENDING",
	    "contexts":{"totalCount":8,"nodes":[
	      {"__typename":"CheckRun","conclusion":"SUCCESS","status":"COMPLETED"},
	      {"__typename":"CheckRun","conclusion":"SUCCESS","status":"COMPLETED"},
	      {"__typename":"CheckRun","conclusion":"SKIPPED","status":"COMPLETED"},
	      {"__typename":"CheckRun","conclusion":"FAILURE","status":"COMPLETED"},
	      {"__typename":"CheckRun","conclusion":"CANCELLED","status":"COMPLETED"},
	      {"__typename":"CheckRun","conclusion":"","status":"IN_PROGRESS"},
	      {"__typename":"StatusContext","state":"EXPECTED"},
	      {"__typename":"StatusContext","state":"ERROR"}
	    ]}}}}]}
	}}}}`)

	pr, err := parsePRResponse(body)
	if err != nil {
		t.Fatalf("parsePRResponse: %v", err)
	}
	casos := map[string]struct{ got, want int }{
		"success":   {pr.ChecksSuccess, 2},
		"skipped":   {pr.ChecksSkipped, 1},
		"failing":   {pr.ChecksFailing, 2}, // FAILURE del run + ERROR del status
		"cancelled": {pr.ChecksCancelled, 1},
		"pending":   {pr.ChecksPending, 1},
		"expected":  {pr.ChecksExpected, 1},
	}
	for nombre, c := range casos {
		if c.got != c.want {
			t.Errorf("%s = %d, se esperaba %d", nombre, c.got, c.want)
		}
	}
}

// TestParsePRLasCategoriasSumanElTotal: si las categorías no cierran contra
// ChecksTotal, la card muestra un desglose que no coincide con el número que
// muestra GitHub. Por eso existe ChecksOther.
func TestParsePRLasCategoriasSumanElTotal(t *testing.T) {
	for _, f := range []string{"pr_open.json"} {
		pr, err := parsePRResponse(fixture(t, f))
		if err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		suma := pr.ChecksSuccess + pr.ChecksFailing + pr.ChecksPending +
			pr.ChecksExpected + pr.ChecksSkipped + pr.ChecksCancelled +
			pr.ChecksNeutral + pr.ChecksOther
		if suma != pr.ChecksTotal {
			t.Errorf("%s: las categorías suman %d y el total es %d", f, suma, pr.ChecksTotal)
		}
	}
}

// TestCheckCategoryClasifica cubre la tabla completa, incluido lo que no
// sabemos clasificar.
func TestCheckCategoryClasifica(t *testing.T) {
	casos := []struct {
		typeName, conclusion, status, state string
		want                                string
	}{
		{"CheckRun", "SUCCESS", "COMPLETED", "", "success"},
		{"CheckRun", "SKIPPED", "COMPLETED", "", "skipped"},
		{"CheckRun", "FAILURE", "COMPLETED", "", "failing"},
		{"CheckRun", "TIMED_OUT", "COMPLETED", "", "failing"},
		{"CheckRun", "ACTION_REQUIRED", "COMPLETED", "", "failing"},
		{"CheckRun", "CANCELLED", "COMPLETED", "", "cancelled"},
		{"CheckRun", "NEUTRAL", "COMPLETED", "", "neutral"},
		{"CheckRun", "STALE", "COMPLETED", "", "neutral"},
		{"CheckRun", "", "IN_PROGRESS", "", "pending"},
		{"CheckRun", "", "QUEUED", "", "pending"},
		// Un run que todavía corre no se clasifica por su conclusion vieja.
		{"CheckRun", "SUCCESS", "IN_PROGRESS", "", "pending"},
		{"CheckRun", "COSA_NUEVA", "COMPLETED", "", "other"},
		{"StatusContext", "", "", "SUCCESS", "success"},
		{"StatusContext", "", "", "FAILURE", "failing"},
		{"StatusContext", "", "", "ERROR", "failing"},
		{"StatusContext", "", "", "PENDING", "pending"},
		{"StatusContext", "", "", "EXPECTED", "expected"},
		{"StatusContext", "", "", "COSA_NUEVA", "other"},
	}
	for _, c := range casos {
		got := checkCategory(c.typeName, c.conclusion, c.status, c.state)
		if got != c.want {
			t.Errorf("%s/%s/%s/%s = %q, se esperaba %q",
				c.typeName, c.conclusion, c.status, c.state, got, c.want)
		}
	}
}
