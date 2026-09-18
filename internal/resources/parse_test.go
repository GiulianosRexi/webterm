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
	// SKIPPED y SUCCESS pasan; el que está corriendo queda pendiente.
	if pr.ChecksPassed != 2 || pr.ChecksPending != 0 {
		t.Fatalf("passed=%d pending=%d", pr.ChecksPassed, pr.ChecksPending)
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
	if pr.ChecksState != "" || pr.ChecksTotal != 0 || pr.ChecksFailing != 0 {
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

// TestParsePRChecksSalteadosCuentanComoPasados es el caso que hace la
// diferencia entre una card útil y una que miente: un PR sano con muchos
// checks condicionales tiene la mayoría en SKIPPED. Contar solo los SUCCESS
// lo mostraría como "7/20" y parecería roto.
func TestParsePRChecksSalteadosCuentanComoPasados(t *testing.T) {
	body := []byte(`{"data":{"repository":{"nameWithOwner":"cli/cli","pullRequest":{
	  "number":1,"title":"t","url":"u","state":"OPEN","isDraft":false,
	  "mergeable":"MERGEABLE","reviewDecision":"","author":{"login":"a"},
	  "reviewThreads":{"totalCount":0,"nodes":[]},
	  "commits":{"nodes":[{"commit":{"statusCheckRollup":{"state":"SUCCESS",
	    "contexts":{"totalCount":4,"nodes":[
	      {"__typename":"CheckRun","conclusion":"SKIPPED","status":"COMPLETED"},
	      {"__typename":"CheckRun","conclusion":"SKIPPED","status":"COMPLETED"},
	      {"__typename":"CheckRun","conclusion":"SUCCESS","status":"COMPLETED"},
	      {"__typename":"CheckRun","conclusion":"","status":"IN_PROGRESS"}
	    ]}}}}]}
	}}}}`)

	pr, err := parsePRResponse(body)
	if err != nil {
		t.Fatalf("parsePRResponse: %v", err)
	}
	if pr.ChecksTotal != 4 {
		t.Fatalf("total = %d", pr.ChecksTotal)
	}
	if pr.ChecksPassed != 3 {
		t.Fatalf("passed = %d, se esperaban 3 (2 salteados + 1 ok)", pr.ChecksPassed)
	}
	if pr.ChecksPending != 1 {
		t.Fatalf("pending = %d, se esperaba 1", pr.ChecksPending)
	}
	if pr.ChecksFailing != 0 {
		t.Fatalf("failing = %d", pr.ChecksFailing)
	}
}
