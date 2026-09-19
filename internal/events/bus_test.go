package events

import (
	"testing"
	"time"
)

// recv saca un evento del canal o falla: un test que se cuelga esperando un
// evento que no llega no dice nada útil.
func recv(t *testing.T, ch <-chan Event) Event {
	t.Helper()
	select {
	case ev, ok := <-ch:
		if !ok {
			t.Fatal("el canal estaba cerrado")
		}
		return ev
	case <-time.After(2 * time.Second):
		t.Fatal("no llegó ningún evento")
		return Event{}
	}
}

func TestPublicaATodosLosSuscriptores(t *testing.T) {
	b := New(4)
	a, closeA := b.Subscribe()
	defer closeA()
	c, closeC := b.Subscribe()
	defer closeC()

	b.Publish(ResourceAdded, "s1")

	for _, ch := range []<-chan Event{a, c} {
		ev := recv(t, ch)
		if ev.Kind != ResourceAdded || ev.SessionID != "s1" {
			t.Fatalf("evento inesperado: %+v", ev)
		}
		if ev.Seq != 1 {
			t.Fatalf("Seq = %d, esperaba 1", ev.Seq)
		}
	}
}

func TestSeqEsMonotonicoYCompartido(t *testing.T) {
	b := New(4)
	ch, stop := b.Subscribe()
	defer stop()

	b.Publish(SessionCreated, "s1")
	b.Publish(SessionUpdated, "s1")

	if got := recv(t, ch).Seq; got != 1 {
		t.Fatalf("primer Seq = %d, esperaba 1", got)
	}
	if got := recv(t, ch).Seq; got != 2 {
		t.Fatalf("segundo Seq = %d, esperaba 2", got)
	}
}

func TestUnsubscribeCierraYDejaDeRecibir(t *testing.T) {
	b := New(4)
	ch, stop := b.Subscribe()
	stop()

	if _, ok := <-ch; ok {
		t.Fatal("el canal tendría que estar cerrado")
	}
	// Publicar después de que se fue el único suscriptor no puede entrar en
	// pánico por escribir en un canal cerrado.
	b.Publish(ResourceAdded, "s1")
}

func TestUnsubscribeEsIdempotente(t *testing.T) {
	b := New(4)
	_, stop := b.Subscribe()
	stop()
	stop() // un doble defer no puede cerrar dos veces el mismo canal
}

// Un cliente que no lee no puede frenar al que publica: se le saltean eventos
// y el hueco queda visible en Seq, que es como el cliente sabe que tiene que
// resincronizarse.
func TestSuscriptorLentoNoBloqueaYDejaHueco(t *testing.T) {
	b := New(1)
	ch, stop := b.Subscribe()
	defer stop()

	done := make(chan struct{})
	go func() {
		b.Publish(ResourceAdded, "s1") // entra al buffer
		b.Publish(ResourceAdded, "s2") // se descarta
		b.Publish(ResourceAdded, "s3") // se descarta
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Publish se bloqueó con el suscriptor lleno")
	}

	if got := recv(t, ch).Seq; got != 1 {
		t.Fatalf("Seq = %d, esperaba 1", got)
	}

	b.Publish(ResourceAdded, "s4")
	if got := recv(t, ch).Seq; got != 4 {
		t.Fatalf("Seq = %d, esperaba 4: el hueco tiene que verse", got)
	}
}
