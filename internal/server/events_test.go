package server

import (
	"bufio"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/giuliano/webterm/internal/events"
)

// readFrame junta líneas hasta el renglón en blanco que cierra un frame SSE.
// Devuelve el frame entero para poder afirmar sobre id:, event: y data: juntos.
func readFrame(t *testing.T, br *bufio.Reader) string {
	t.Helper()
	var sb strings.Builder
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("leyendo el frame: %v", err)
		}
		if line == "\n" {
			return sb.String()
		}
		sb.WriteString(line)
	}
}

func TestEventsMandaResyncAlAbrirYLuegoLosEventos(t *testing.T) {
	bus := events.New(8)
	s := &Server{events: bus}
	ts := httptest.NewServer(http.HandlerFunc(s.handleEvents))
	defer ts.Close()

	resp, err := http.Get(ts.URL)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type = %q", ct)
	}

	br := bufio.NewReader(resp.Body)
	if got := readFrame(t, br); !strings.Contains(got, "event: resync") {
		t.Fatalf("el primer frame tendría que ser el resync, fue %q", got)
	}

	bus.Publish(events.ResourceAdded, "s1")

	frame := readFrame(t, br)
	for _, want := range []string{"id: 1", "event: resource.added", `"session_id":"s1"`} {
		if !strings.Contains(frame, want) {
			t.Fatalf("el frame %q no contiene %q", frame, want)
		}
	}
}

// Cortar la conexión tiene que devolver el handler: como unsubscribe está
// diferido, que el handler vuelva ES soltar la suscripción, así que basta con
// observar el return.
//
// (Antes este test publicaba 100 eventos tras cerrar la conexión y afirmaba
// que eso no se colgaba, como prueba indirecta de que la suscripción se
// había soltado. Eso no probaba nada: Publish es no bloqueante para
// cualquier suscriptor, buffer lleno o no —"select { case ch <- ev: default:
// }"—, así que esas 100 llamadas terminan en milisegundos exista o no la
// suscripción. Borrar el defer unsubscribe() del handler no hubiera hecho
// fallar esa versión.)
func TestEventsHandlerVuelveAlIrseElCliente(t *testing.T) {
	bus := events.New(8)
	s := &Server{events: bus}

	// Envolvemos el handler para poder observar cuándo vuelve: eso es lo
	// único que realmente demuestra que soltó la suscripción, porque
	// unsubscribe está en un defer justo después de Subscribe.
	handlerDone := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.handleEvents(w, r)
		close(handlerDone)
	}))
	defer ts.Close()

	resp, err := http.Get(ts.URL)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	br := bufio.NewReader(resp.Body)
	readFrame(t, br) // resync
	resp.Body.Close()

	select {
	case <-handlerDone:
	case <-time.After(2 * time.Second):
		t.Fatal("el handler no volvió después de que el cliente se fue; se quedó colgado sin soltar la suscripción")
	}
}

func TestEventsSinBusDevuelve404(t *testing.T) {
	s := &Server{}
	rec := httptest.NewRecorder()
	s.handleEvents(rec, httptest.NewRequest(http.MethodGet, "/api/events", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, esperaba 404", rec.Code)
	}
}
