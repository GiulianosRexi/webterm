package store

import (
	"errors"
	"reflect"
	"testing"
)

func TestNormalizeTag(t *testing.T) {
	cases := map[string]string{
		"bugfix":          "bugfix",
		"  Bugfix ":       "bugfix",
		"Bug Fix":         "bug-fix",
		"code   review\t": "code-review",
		"Implementación":  "implementación",
	}
	for in, want := range cases {
		got, err := NormalizeTag(in)
		if err != nil {
			t.Fatalf("NormalizeTag(%q): %v", in, err)
		}
		if got != want {
			t.Fatalf("NormalizeTag(%q) = %q, esperaba %q", in, got, want)
		}
	}
}

func TestNormalizeTagRechazaVacioYLargo(t *testing.T) {
	for _, in := range []string{"", "   ", "una-etiqueta-que-es-mas-bien-una-descripcion"} {
		if _, err := NormalizeTag(in); !errors.Is(err, ErrInvalidTag) {
			t.Fatalf("NormalizeTag(%q) = %v, esperaba ErrInvalidTag", in, err)
		}
	}
}

func TestSetSessionTagsReemplazaYNormaliza(t *testing.T) {
	st := newTestStore(t)
	if err := st.CreateSession(sampleSession("s1")); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	if err := st.SetSessionTags("s1", []string{"Bugfix", "frontend", "bugfix"}); err != nil {
		t.Fatalf("SetSessionTags: %v", err)
	}
	got, err := st.GetSession("s1")
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if want := []string{"bugfix", "frontend"}; !reflect.DeepEqual(got.Tags, want) {
		t.Fatalf("Tags = %v, esperaba %v", got.Tags, want)
	}

	if err := st.SetSessionTags("s1", []string{"consulta"}); err != nil {
		t.Fatalf("SetSessionTags: %v", err)
	}
	got, _ = st.GetSession("s1")
	if want := []string{"consulta"}; !reflect.DeepEqual(got.Tags, want) {
		t.Fatalf("Tags = %v, esperaba %v", got.Tags, want)
	}
}

// Un tag inválido en la lista no puede dejar la sesión a medio taggear.
func TestSetSessionTagsInvalidoNoTocaNada(t *testing.T) {
	st := newTestStore(t)
	if err := st.CreateSession(sampleSession("s1")); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if err := st.SetSessionTags("s1", []string{"bugfix"}); err != nil {
		t.Fatalf("SetSessionTags: %v", err)
	}
	if err := st.SetSessionTags("s1", []string{"consulta", "  "}); !errors.Is(err, ErrInvalidTag) {
		t.Fatalf("SetSessionTags = %v, esperaba ErrInvalidTag", err)
	}
	got, _ := st.GetSession("s1")
	if want := []string{"bugfix"}; !reflect.DeepEqual(got.Tags, want) {
		t.Fatalf("Tags = %v, esperaba %v", got.Tags, want)
	}
}

func TestTagsDeSesionInexistente(t *testing.T) {
	st := newTestStore(t)
	if err := st.SetSessionTags("nope", []string{"x"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("SetSessionTags = %v, esperaba ErrNotFound", err)
	}
	if err := st.AddSessionTags("nope", []string{"x"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("AddSessionTags = %v, esperaba ErrNotFound", err)
	}
	if err := st.RemoveSessionTags("nope", []string{"x"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("RemoveSessionTags = %v, esperaba ErrNotFound", err)
	}
}

func TestAddYRemoveSessionTags(t *testing.T) {
	st := newTestStore(t)
	if err := st.CreateSession(sampleSession("s1")); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if err := st.SetSessionTags("s1", []string{"frontend"}); err != nil {
		t.Fatalf("SetSessionTags: %v", err)
	}

	// Agregar uno que ya estaba no falla ni lo duplica.
	if err := st.AddSessionTags("s1", []string{"Bugfix", "frontend"}); err != nil {
		t.Fatalf("AddSessionTags: %v", err)
	}
	got, _ := st.GetSession("s1")
	if want := []string{"bugfix", "frontend"}; !reflect.DeepEqual(got.Tags, want) {
		t.Fatalf("Tags = %v, esperaba %v", got.Tags, want)
	}

	// Sacar uno que no tenía tampoco falla.
	if err := st.RemoveSessionTags("s1", []string{"FRONTEND", "consulta"}); err != nil {
		t.Fatalf("RemoveSessionTags: %v", err)
	}
	got, _ = st.GetSession("s1")
	if want := []string{"bugfix"}; !reflect.DeepEqual(got.Tags, want) {
		t.Fatalf("Tags = %v, esperaba %v", got.Tags, want)
	}
}

func TestListSessionsTraeTagsYNuncaNil(t *testing.T) {
	st := newTestStore(t)
	for _, id := range []string{"s1", "s2"} {
		if err := st.CreateSession(sampleSession(id)); err != nil {
			t.Fatalf("CreateSession: %v", err)
		}
	}
	if err := st.SetSessionTags("s1", []string{"bugfix"}); err != nil {
		t.Fatalf("SetSessionTags: %v", err)
	}

	list, err := st.ListSessions()
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	byID := map[string][]string{}
	for _, s := range list {
		if s.Tags == nil {
			t.Fatalf("la sesión %s vino con Tags nil", s.ID)
		}
		byID[s.ID] = s.Tags
	}
	if !reflect.DeepEqual(byID["s1"], []string{"bugfix"}) || len(byID["s2"]) != 0 {
		t.Fatalf("tags = %v", byID)
	}
}

func TestListTagsCuentaYSeLimpiaAlBorrarLaSesion(t *testing.T) {
	st := newTestStore(t)
	for _, id := range []string{"s1", "s2"} {
		if err := st.CreateSession(sampleSession(id)); err != nil {
			t.Fatalf("CreateSession: %v", err)
		}
	}
	_ = st.SetSessionTags("s1", []string{"bugfix", "frontend"})
	_ = st.SetSessionTags("s2", []string{"bugfix"})

	got, err := st.ListTags()
	if err != nil {
		t.Fatalf("ListTags: %v", err)
	}
	want := []TagCount{{Name: "bugfix", Count: 2}, {Name: "frontend", Count: 1}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ListTags = %v, esperaba %v", got, want)
	}

	if err := st.DeleteSession("s1"); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}
	got, _ = st.ListTags()
	want = []TagCount{{Name: "bugfix", Count: 1}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ListTags tras borrar = %v, esperaba %v", got, want)
	}
}
