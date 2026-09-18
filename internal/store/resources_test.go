package store

import (
	"errors"
	"testing"
)

func sampleResource(sessionID, ref string) *Resource {
	return &Resource{SessionID: sessionID, System: "gh", Type: "pr", Ref: ref}
}

func TestAddYListResources(t *testing.T) {
	st := newTestStore(t)
	_ = st.CreateSession(sampleSession("s1"))

	r := sampleResource("s1", "https://github.com/o/r/pull/1")
	if err := st.AddResource(r); err != nil {
		t.Fatalf("AddResource: %v", err)
	}
	if r.ID == 0 || r.CreatedAt == 0 {
		t.Fatalf("AddResource tiene que completar id y timestamp: %+v", r)
	}

	list, err := st.ListResources("s1")
	if err != nil {
		t.Fatalf("ListResources: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("vinieron %d recursos", len(list))
	}
	if list[0].System != "gh" || list[0].Type != "pr" || list[0].Ref != r.Ref {
		t.Fatalf("se guardó mal: %+v", list[0])
	}
}

func TestListResourcesVacio(t *testing.T) {
	st := newTestStore(t)
	_ = st.CreateSession(sampleSession("s1"))

	list, err := st.ListResources("s1")
	if err != nil {
		t.Fatalf("ListResources: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("una sesión sin recursos tiene que dar lista vacía, dio %d", len(list))
	}
}

// TestAddResourceDuplicado: linkear dos veces el mismo PR a la misma sesión es
// un error del usuario, no una fila repetida en la UI.
func TestAddResourceDuplicado(t *testing.T) {
	st := newTestStore(t)
	_ = st.CreateSession(sampleSession("s1"))
	ref := "https://github.com/o/r/pull/1"

	if err := st.AddResource(sampleResource("s1", ref)); err != nil {
		t.Fatalf("AddResource 1: %v", err)
	}
	if err := st.AddResource(sampleResource("s1", ref)); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("se esperaba ErrDuplicate, vino %v", err)
	}
}

// TestMismoRefEnDosSesiones: el mismo PR sí puede estar en varias sesiones.
func TestMismoRefEnDosSesiones(t *testing.T) {
	st := newTestStore(t)
	_ = st.CreateSession(sampleSession("s1"))
	_ = st.CreateSession(sampleSession("s2"))
	ref := "https://github.com/o/r/pull/1"

	if err := st.AddResource(sampleResource("s1", ref)); err != nil {
		t.Fatalf("AddResource s1: %v", err)
	}
	if err := st.AddResource(sampleResource("s2", ref)); err != nil {
		t.Fatalf("el mismo ref en otra sesión tiene que andar: %v", err)
	}
}

func TestDeleteResource(t *testing.T) {
	st := newTestStore(t)
	_ = st.CreateSession(sampleSession("s1"))
	r := sampleResource("s1", "https://github.com/o/r/pull/1")
	_ = st.AddResource(r)

	if err := st.DeleteResource("s1", r.ID); err != nil {
		t.Fatalf("DeleteResource: %v", err)
	}
	if err := st.DeleteResource("s1", r.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("borrar dos veces tiene que dar ErrNotFound, vino %v", err)
	}
}

// TestDeleteResourceDeOtraSesion: el id es global, así que el borrado tiene
// que filtrar por sesión o una sesión podría borrar recursos de otra.
func TestDeleteResourceDeOtraSesion(t *testing.T) {
	st := newTestStore(t)
	_ = st.CreateSession(sampleSession("s1"))
	_ = st.CreateSession(sampleSession("s2"))
	r := sampleResource("s1", "https://github.com/o/r/pull/1")
	_ = st.AddResource(r)

	if err := st.DeleteResource("s2", r.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("se esperaba ErrNotFound, vino %v", err)
	}
	list, _ := st.ListResources("s1")
	if len(list) != 1 {
		t.Fatal("el recurso se borró desde otra sesión")
	}
}

// TestSessionsForRef es la búsqueda inversa que motiva que esto sea una tabla
// y no un blob JSON: dado un PR, qué sesiones se prenden.
func TestSessionsForRef(t *testing.T) {
	st := newTestStore(t)
	_ = st.CreateSession(sampleSession("s1"))
	_ = st.CreateSession(sampleSession("s2"))
	_ = st.CreateSession(sampleSession("s3"))
	ref := "https://github.com/o/r/pull/1"
	_ = st.AddResource(sampleResource("s1", ref))
	_ = st.AddResource(sampleResource("s3", ref))
	_ = st.AddResource(sampleResource("s2", "https://github.com/o/r/pull/2"))

	ids, err := st.SessionsForRef(ref)
	if err != nil {
		t.Fatalf("SessionsForRef: %v", err)
	}
	if len(ids) != 2 || ids[0] != "s1" || ids[1] != "s3" {
		t.Fatalf("SessionsForRef = %v", ids)
	}
}

func TestResourcesCascadaAlBorrarSesion(t *testing.T) {
	st := newTestStore(t)
	_ = st.CreateSession(sampleSession("s1"))
	_ = st.AddResource(sampleResource("s1", "https://github.com/o/r/pull/1"))

	if err := st.DeleteSession("s1"); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}
	var n int
	_ = st.DB().QueryRow(`SELECT COUNT(*) FROM session_resources`).Scan(&n)
	if n != 0 {
		t.Fatalf("quedaron %d recursos huérfanos", n)
	}
}
