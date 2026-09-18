package session

import (
	"bytes"
	"testing"
)

func TestRingGuardaElTail(t *testing.T) {
	r := newRing(250)
	for i := 0; i < 10; i++ {
		r.append(bytes.Repeat([]byte{byte('a' + i)}, 100))
	}
	got := r.snapshot()
	if len(got) > 250 {
		t.Fatalf("el ring guardó %d bytes con un cap de 250", len(got))
	}
	if !bytes.Contains(got, bytes.Repeat([]byte("j"), 100)) {
		t.Fatal("se descartó el chunk más nuevo")
	}
	if bytes.Contains(got, bytes.Repeat([]byte("a"), 100)) {
		t.Fatal("quedó el chunk más viejo")
	}
}

// TestRingCopiaElChunk: el lector del pty reusa su buffer, así que el ring
// tiene que quedarse con una copia o termina mostrando basura.
func TestRingCopiaElChunk(t *testing.T) {
	r := newRing(100)
	buf := []byte("hola")
	r.append(buf)
	copy(buf, "chau")

	if string(r.snapshot()) != "hola" {
		t.Fatalf("el ring se quedó con el buffer del llamador: %q", r.snapshot())
	}
}

// TestRingNuncaQuedaVacio: un chunk más grande que el cap se conserva igual.
func TestRingNuncaQuedaVacio(t *testing.T) {
	r := newRing(10)
	r.append(bytes.Repeat([]byte("x"), 500))
	if len(r.snapshot()) != 500 {
		t.Fatalf("se descartó el único chunk: quedaron %d bytes", len(r.snapshot()))
	}
}

func TestRingPreload(t *testing.T) {
	r := newRing(250)
	r.preload(bytes.Repeat([]byte("h"), 300))
	r.append([]byte("nuevo"))

	got := r.snapshot()
	if !bytes.HasSuffix(got, []byte("nuevo")) {
		t.Fatalf("el append no quedó al final: %q", got[max(0, len(got)-10):])
	}
	if len(got) > 250+len("nuevo") {
		t.Fatalf("el preload ignoró el cap: %d bytes", len(got))
	}
}

func TestHubBroadcastADosSuscriptores(t *testing.T) {
	h := newHub()
	a := h.subscribe(4)
	b := h.subscribe(4)

	h.broadcast([]byte("hola"))

	for name, s := range map[string]*subscriber{"a": a, "b": b} {
		select {
		case got := <-s.out():
			if string(got) != "hola" {
				t.Fatalf("%s recibió %q", name, got)
			}
		default:
			t.Fatalf("%s no recibió nada", name)
		}
	}
}

// TestHubExpulsaAlLento: un cliente que no lee no puede frenar al pty. Se lo
// desconecta a él en vez de bloquear a todos.
func TestHubExpulsaAlLento(t *testing.T) {
	h := newHub()
	lento := h.subscribe(2)
	rapido := h.subscribe(64)

	for i := 0; i < 10; i++ {
		h.broadcast([]byte("x"))
	}

	// El canal del lento quedó cerrado y marcado.
	drenado := 0
	for range lento.out() {
		drenado++
	}
	if !lento.wasDropped() {
		t.Fatal("el suscriptor lento no quedó marcado como expulsado")
	}
	if drenado > 2 {
		t.Fatalf("el lento recibió %d chunks con un buffer de 2", drenado)
	}
	if h.count() != 1 {
		t.Fatalf("quedaron %d suscriptores, se esperaba 1", h.count())
	}

	// El rápido siguió recibiendo todo.
	if len(rapido.out()) != 10 {
		t.Fatalf("el rápido recibió %d chunks, se esperaban 10", len(rapido.out()))
	}
}

func TestHubUnsubscribeEsIdempotente(t *testing.T) {
	h := newHub()
	s := h.subscribe(4)

	h.unsubscribe(s)
	h.unsubscribe(s) // no tiene que panickear por cerrar dos veces el canal

	if h.count() != 0 {
		t.Fatalf("quedaron %d suscriptores", h.count())
	}
	if _, ok := <-s.out(); ok {
		t.Fatal("el canal tenía que quedar cerrado")
	}
}

func TestHubCloseAll(t *testing.T) {
	h := newHub()
	a := h.subscribe(4)
	b := h.subscribe(4)

	h.closeAll()

	if _, ok := <-a.out(); ok {
		t.Fatal("a quedó abierto")
	}
	if _, ok := <-b.out(); ok {
		t.Fatal("b quedó abierto")
	}
	if a.wasDropped() || b.wasDropped() {
		t.Fatal("closeAll no es una expulsión: el cliente tiene que ver un cierre normal")
	}
	if h.count() != 0 {
		t.Fatalf("quedaron %d suscriptores", h.count())
	}
}

// TestHubSubscribeDespuesDeCloseAll: el que llega tarde se lleva un canal ya
// cerrado. Registrarlo sería dejarlo esperando para siempre, porque closeAll
// no vuelve a pasar.
func TestHubSubscribeDespuesDeCloseAll(t *testing.T) {
	h := newHub()
	h.closeAll()

	s := h.subscribe(4)
	if !s.isClosed() {
		t.Fatal("el suscriptor tardío no quedó marcado como cerrado")
	}
	if _, ok := <-s.out(); ok {
		t.Fatal("el canal del suscriptor tardío quedó abierto")
	}
	if s.wasDropped() {
		t.Fatal("llegar tarde no es una expulsión")
	}
	if h.count() != 0 {
		t.Fatalf("el hub cerrado registró %d suscriptores", h.count())
	}
}
