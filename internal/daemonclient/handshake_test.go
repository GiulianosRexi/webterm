package daemonclient

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// Reproduce el I1 del review de ronda 1: un peer que sube el WebSocket y
// manda un frame de texto que no es "ready" (acá, {"type":"hola"}) y después
// se queda mudo. Antes del fix, readHandshake no tenía deadline de lectura y
// Attach quedaba bloqueado para siempre —el orquestador no puede cancelarlo
// porque ptyapi.Client.Attach no toma contexto—, así que la única defensa
// posible está de este lado, en handshakeTimeout.
//
// Se arma un httptest.Server en vez de un daemon real porque acá el peer
// tiene que comportarse mal a propósito, y el daemon de verdad nunca hace
// esto (no es un caso que pueda reproducirse contra él).
func TestAttachCortaSiElPeerNoMandaReady(t *testing.T) {
	upgrader := websocket.Upgrader{}
	mudo := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.WriteJSON(map[string]string{"type": "hola"})
		<-mudo // se queda con la conexión abierta y sin mandar nada más
	}))
	defer func() {
		close(mudo)
		srv.Close()
	}()

	// El dialer ignora la red y la dirección del pedido y siempre disca el
	// httptest.Server: no importa qué URL arma Attach, el peer del otro lado
	// es el handler de arriba.
	cl := &Client{dialer: &websocket.Dialer{
		NetDialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "tcp", srv.Listener.Addr().String())
		},
	}}

	start := time.Now()
	_, err := cl.Attach("x")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("attach contra un peer que nunca manda ready tendría que fallar")
	}
	// Margen generoso sobre handshakeTimeout: lo que importa es que haya un
	// límite, no ajustarlo al milisegundo.
	if elapsed > handshakeTimeout+5*time.Second {
		t.Fatalf("attach tardó %s; handshakeTimeout (%s) tendría que haber cortado antes", elapsed, handshakeTimeout)
	}
}
