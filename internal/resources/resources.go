// Package resources traduce URLs de sistemas externos a referencias tipadas, y
// esas referencias al estado actual del recurso.
//
// No conoce ni la base ni las sesiones a propósito: dada una URL devuelve un
// Ref, y dado un Ref devuelve un Snapshot. Eso lo hace testeable sin DB y sin
// red, y deja que el único lugar que sabe de sesiones sea el manager.
package resources

import (
	"context"
	"errors"
)

// ErrUnknownResource lo devuelve el registry cuando ningún provider reconoce
// la URL.
var ErrUnknownResource = errors.New("no se reconoce la URL")

// Ref identifica un recurso externo.
type Ref struct {
	System string `json:"system"` // gh
	Type   string `json:"type"`   // pr
	URL    string `json:"url"`    // URL canónica, ya normalizada
}

// PRState es el estado de un pull request de GitHub.
type PRState struct {
	Number         int    `json:"number"`
	Title          string `json:"title"`
	Author         string `json:"author"`
	State          string `json:"state"` // OPEN | CLOSED | MERGED
	IsDraft        bool   `json:"is_draft"`
	Mergeable      string `json:"mergeable"`       // MERGEABLE | CONFLICTING | UNKNOWN
	ReviewDecision string `json:"review_decision"` // APPROVED | CHANGES_REQUESTED | REVIEW_REQUIRED | ""

	UnresolvedCount  int  `json:"unresolved_count"`
	ThreadsTruncated bool `json:"threads_truncated"`

	// ChecksState queda vacío cuando el PR no tiene checks o cuando ya
	// expiraron, que es lo que devuelve la API en PRs viejos.
	ChecksState   string `json:"checks_state"` // SUCCESS | FAILURE | PENDING | ""
	ChecksTotal   int    `json:"checks_total"`
	ChecksFailing int    `json:"checks_failing"`
}

// Snapshot es el estado de un recurso en un momento dado. Error y PR son
// excluyentes: si hubo error, no hay estado que mostrar.
type Snapshot struct {
	FetchedAt int64    `json:"fetched_at"`
	Error     string   `json:"error,omitempty"`
	PR        *PRState `json:"pr,omitempty"`
}

// Provider traduce URLs a refs y refs a estado.
type Provider interface {
	// Match reconoce la URL y la normaliza. El segundo valor dice si este
	// provider se hace cargo.
	Match(rawURL string) (Ref, bool)
	// Fetch consulta el estado actual del recurso.
	Fetch(ctx context.Context, ref Ref) (*Snapshot, error)
}

// Registry es el punto de extensión: sumar Linear o Slack es sumar un provider
// acá, sin tocar el contrato de la API ni el modelo de datos.
type Registry struct {
	providers []Provider
}

func NewRegistry(ps ...Provider) *Registry {
	return &Registry{providers: ps}
}

// Resolve encuentra el provider que reconoce la URL.
func (r *Registry) Resolve(rawURL string) (Ref, Provider, bool) {
	for _, p := range r.providers {
		if ref, ok := p.Match(rawURL); ok {
			return ref, p, true
		}
	}
	return Ref{}, nil, false
}

// ProviderFor devuelve el provider que maneja un ref ya guardado.
func (r *Registry) ProviderFor(ref Ref) (Provider, bool) {
	for _, p := range r.providers {
		if got, ok := p.Match(ref.URL); ok && got.System == ref.System {
			return p, true
		}
	}
	return nil, false
}
