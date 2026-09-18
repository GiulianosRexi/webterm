package control

import (
	"errors"
	"log"

	"github.com/giuliano/webterm/internal/ptyapi"
	"github.com/giuliano/webterm/internal/store"
)

// Attachment es la conexión de un cliente a una sesión.
//
// Session viene de la base y no del daemon: el daemon no conoce títulos ni
// kanban. Es el único dato del handshake que el orquestador no reenvía sino
// que compone.
type Attachment struct {
	Session *store.Session
	// History es el replay que hay que mandar antes del stream vivo.
	History []byte
	// Live dice si hay proceso corriendo. Si es false, la conexión es de solo
	// lectura y Output() devuelve nil.
	Live bool

	m     *Manager
	inner ptyapi.Attachment
}

func (a *Attachment) Output() <-chan []byte {
	if a.inner == nil {
		return nil
	}
	return a.inner.Output()
}

func (a *Attachment) Write(p []byte) error {
	if a.inner == nil {
		return ptyapi.ErrNotLive
	}
	return a.inner.Write(p)
}

func (a *Attachment) Resize(rows, cols uint16) error {
	if a.inner == nil {
		return ptyapi.ErrNotLive
	}
	return a.inner.Resize(rows, cols)
}

func (a *Attachment) Detach() {
	if a.inner != nil {
		a.inner.Detach()
	}
}

// Dropped dice si al cliente lo expulsaron por lento, según lo que haya podido
// contar el transporte. Es best-effort y no alcanza para decidir nada: ver End.
func (a *Attachment) Dropped() bool {
	return a.inner != nil && a.inner.Dropped()
}

// StreamEnd es por qué se cortó el stream vivo de un cliente.
//
// Existe porque la señal de "a este cliente lo expulsaron" NO puede depender
// del transporte. El frame `dropped` que manda el daemon es best-effort: si el
// cliente dejó de leer, la escritura del daemon se traba, gorilla envenena la
// conexión y no sale ningún frame más —justo en el caso que querríamos
// reportar—. Está documentado en internal/daemon/attach.go y en
// internal/daemonclient/attach.go.
//
// La señal autoritativa es la fila, que sobrevive a cualquier corte del
// socket:
//
//	stream cortado + fila exited  => la sesión terminó
//	stream cortado + fila running => al cliente lo expulsaron, o se cayó el
//	                                 transporte, y la sesión sigue viva
//
// Vale la pena consultarlo recién después de que Output() se haya cerrado.
type StreamEnd struct {
	// Session es la fila releída. Es nil si la sesión se borró mientras tanto.
	Session *store.Session
	// Exited dice que la sesión terminó de verdad, no que se cortó el stream.
	Exited bool
	// Dropped es lo que alcanzó a decir el transporte. Solo sirve para
	// enriquecer el mensaje al usuario; no para decidir.
	Dropped bool
}

// End explica por qué se terminó el stream, leyendo la fila.
func (a *Attachment) End() StreamEnd {
	end := StreamEnd{Dropped: a.Dropped()}
	rec, err := a.m.st.GetSession(a.Session.ID)
	if err != nil {
		// La fila no está: la sesión se borró mientras el cliente miraba. No
		// es "terminó", es "ya no existe", y quien nos llama tiene el id.
		return end
	}
	end.Session = rec
	end.Exited = rec.PtyStatus == store.StatusExited
	return end
}

// Attach conecta un cliente. Una sesión muerta se attachea igual, en modo
// lectura: así ver su historial no necesita una vista aparte.
func (m *Manager) Attach(id string) (*Attachment, error) {
	rec, err := m.st.GetSession(id)
	if err != nil {
		return nil, err
	}
	if rec.PtyStatus == store.StatusExited {
		return m.readOnly(rec)
	}

	inner, err := m.pty.Attach(id)
	if errors.Is(err, ptyapi.ErrNotLive) {
		// La fila estaba desactualizada: el pty murió y el reap del daemon
		// todavía no la había marcado. No es un error, es el mismo camino de
		// solo lectura al que hubiéramos ido con la fila al día.
		if fresh, ferr := m.st.GetSession(id); ferr == nil {
			rec = fresh
		}
		return m.readOnly(rec)
	}
	if err != nil {
		return nil, err
	}

	if terr := m.st.TouchActive(id); terr != nil {
		log.Printf("[%s] no se pudo actualizar last_active_at: %v", id, terr)
	}
	return &Attachment{Session: rec, History: inner.History(), Live: true, m: m, inner: inner}, nil
}

// readOnly arma la conexión de solo lectura sobre el historial de la base. No
// consulta al daemon: una sesión muerta no le compete.
func (m *Manager) readOnly(rec *store.Session) (*Attachment, error) {
	hist, err := m.st.ReadOutput(rec.ID)
	if err != nil {
		return nil, err
	}
	return &Attachment{Session: rec, History: ptyapi.SanitizeReplay(hist), m: m}, nil
}
