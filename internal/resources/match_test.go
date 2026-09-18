package resources

import "testing"

// TestMatchPRNormaliza: se acepta cualquier forma en que se copie la URL de un
// PR desde el browser, y se guarda siempre la canónica.
func TestMatchPRNormaliza(t *testing.T) {
	gh := NewGitHub()
	canonica := "https://github.com/cli/cli/pull/14456"

	entradas := []string{
		"https://github.com/cli/cli/pull/14456",
		"http://github.com/cli/cli/pull/14456",
		"github.com/cli/cli/pull/14456",
		"https://github.com/cli/cli/pull/14456/files",
		"https://github.com/cli/cli/pull/14456/commits/abc123",
		"https://github.com/cli/cli/pull/14456#discussion_r123456",
		"https://github.com/cli/cli/pull/14456?w=1",
		"  https://github.com/cli/cli/pull/14456  ",
		"https://www.github.com/cli/cli/pull/14456",
	}
	for _, in := range entradas {
		ref, ok := gh.Match(in)
		if !ok {
			t.Errorf("no reconoció %q", in)
			continue
		}
		if ref.URL != canonica {
			t.Errorf("%q se normalizó a %q, se esperaba %q", in, ref.URL, canonica)
		}
		if ref.System != "gh" || ref.Type != "pr" {
			t.Errorf("%q dio system=%q type=%q", in, ref.System, ref.Type)
		}
	}
}

// TestMatchRechaza: mejor rechazar claro que adivinar.
func TestMatchRechaza(t *testing.T) {
	gh := NewGitHub()
	entradas := []string{
		"",
		"no es una url",
		"https://github.com/cli/cli",
		"https://github.com/cli/cli/issues/14456",
		"https://github.com/cli/cli/pull/",
		"https://github.com/cli/cli/pull/abc",
		"https://gitlab.com/cli/cli/pull/1",
		"https://github.com.evil.example/cli/cli/pull/1",
		"https://github.com/cli/cli/pull/-1",
	}
	for _, in := range entradas {
		if ref, ok := gh.Match(in); ok {
			t.Errorf("aceptó %q como %+v", in, ref)
		}
	}
}

func TestParseRefDevuelveOwnerRepoNumero(t *testing.T) {
	gh := NewGitHub()
	ref, ok := gh.Match("https://github.com/cli/cli/pull/14456")
	if !ok {
		t.Fatal("no matcheó")
	}
	owner, repo, number, err := parsePRRef(ref)
	if err != nil {
		t.Fatalf("parsePRRef: %v", err)
	}
	if owner != "cli" || repo != "cli" || number != 14456 {
		t.Fatalf("parsePRRef = %s/%s#%d", owner, repo, number)
	}
}

func TestRegistryResolve(t *testing.T) {
	reg := NewRegistry(NewGitHub())

	ref, p, ok := reg.Resolve("https://github.com/cli/cli/pull/14456")
	if !ok {
		t.Fatal("el registry no resolvió un PR de GitHub")
	}
	if p == nil {
		t.Fatal("resolvió sin provider")
	}
	if ref.System != "gh" || ref.Type != "pr" {
		t.Fatalf("ref = %+v", ref)
	}

	if _, _, ok := reg.Resolve("https://ejemplo.invalido/algo"); ok {
		t.Fatal("el registry aceptó una URL que ningún provider reconoce")
	}
}
