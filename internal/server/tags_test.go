package server

import (
	"encoding/json"
	"net/http"
	"reflect"
	"testing"

	"github.com/giuliano/webterm/internal/store"
)

func TestSetSessionTagsYListTags(t *testing.T) {
	srv, _ := newTestServer(t)

	_, body := do(t, srv, http.MethodPost, "/api/sessions", `{"cols":80,"rows":24}`)
	sess := decodeSession(t, body)
	if sess.Tags == nil {
		t.Fatalf("una sesión recién creada tiene que traer tags: [] (%s)", body)
	}

	code, body := do(t, srv, http.MethodPut, "/api/sessions/"+sess.ID+"/tags",
		`{"tags":["Bug fix","frontend"]}`)
	if code != http.StatusOK {
		t.Fatalf("PUT tags = %d (%s)", code, body)
	}
	got := decodeSession(t, body)
	if want := []string{"bug-fix", "frontend"}; !reflect.DeepEqual(got.Tags, want) {
		t.Fatalf("Tags = %v, esperaba %v", got.Tags, want)
	}

	code, body = do(t, srv, http.MethodGet, "/api/tags", "")
	if code != http.StatusOK {
		t.Fatalf("GET /api/tags = %d (%s)", code, body)
	}
	var tags []store.TagCount
	if err := json.Unmarshal(body, &tags); err != nil {
		t.Fatalf("decodificando tags: %v", err)
	}
	if len(tags) != 2 || tags[0].Name != "bug-fix" || tags[0].Count != 1 {
		t.Fatalf("tags = %s", body)
	}
}

func TestSetSessionTagsErrores(t *testing.T) {
	srv, _ := newTestServer(t)

	_, body := do(t, srv, http.MethodPost, "/api/sessions", `{"cols":80,"rows":24}`)
	sess := decodeSession(t, body)

	if code, body := do(t, srv, http.MethodPut, "/api/sessions/"+sess.ID+"/tags",
		`{"tags":["  "]}`); code != http.StatusBadRequest {
		t.Fatalf("tag vacío = %d (%s), esperaba 400", code, body)
	}
	if code, body := do(t, srv, http.MethodPut, "/api/sessions/nope/tags",
		`{"tags":["x"]}`); code != http.StatusNotFound {
		t.Fatalf("sesión inexistente = %d (%s), esperaba 404", code, body)
	}
}
