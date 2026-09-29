package store

import (
	"fmt"
	"time"
)

// AgentStaleAfter es cuánto se le cree a un subagente que arrancó y nunca
// avisó que terminó. La documentación de Claude Code no dice si SubagentStop se
// dispara cuando un subagente se cancela o muere a mitad de camino; sin este
// corte, uno así dejaría la sesión esperándolo para siempre.
const AgentStaleAfter = 4 * time.Hour

// agentCutoff es el started_at mínimo de un subagente que todavía cuenta.
func agentCutoff() int64 {
	return time.Now().Add(-AgentStaleAfter).UnixMilli()
}

// AddAgent registra un subagente corriendo en la sesión. Repetirlo con el
// mismo id no duplica nada. De paso barre los vencidos de esa sesión, que es
// lo que evita que la tabla acumule los que nunca terminaron.
func (s *Store) AddAgent(sessionID, agentID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.db.Exec(`DELETE FROM session_agents WHERE session_id = ? AND started_at < ?`,
		sessionID, agentCutoff()); err != nil {
		return fmt.Errorf("barriendo subagentes de %s: %w", sessionID, err)
	}
	_, err := s.db.Exec(`
		INSERT INTO session_agents (session_id, agent_id, started_at) VALUES (?, ?, ?)
		ON CONFLICT (session_id, agent_id) DO NOTHING`,
		sessionID, agentID, time.Now().UnixMilli())
	if err != nil {
		return fmt.Errorf("registrando el subagente %s de %s: %w", agentID, sessionID, err)
	}
	return nil
}

// RemoveAgent saca un subagente que terminó. Si no estaba (arrancó antes de
// instalar los hooks, o ya se había vencido) no es un error.
func (s *Store) RemoveAgent(sessionID, agentID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`DELETE FROM session_agents WHERE session_id = ? AND agent_id = ?`,
		sessionID, agentID)
	if err != nil {
		return fmt.Errorf("sacando el subagente %s de %s: %w", agentID, sessionID, err)
	}
	return nil
}

// ClearAgents olvida todos los subagentes de la sesión: el Claude que los
// lanzó terminó o se reinició, y los suyos murieron con él.
func (s *Store) ClearAgents(sessionID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.db.Exec(`DELETE FROM session_agents WHERE session_id = ?`, sessionID); err != nil {
		return fmt.Errorf("limpiando subagentes de %s: %w", sessionID, err)
	}
	return nil
}

// runningAgents cuenta los subagentes vigentes de una sesión.
func (s *Store) runningAgents(sessionID string) (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM session_agents WHERE session_id = ? AND started_at >= ?`,
		sessionID, agentCutoff()).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("contando subagentes de %s: %w", sessionID, err)
	}
	return n, nil
}

// allRunningAgents cuenta los subagentes vigentes de todas las sesiones de una
// sola query, igual que allSessionTags.
func (s *Store) allRunningAgents() (map[string]int, error) {
	rows, err := s.db.Query(`
		SELECT session_id, COUNT(*) FROM session_agents WHERE started_at >= ? GROUP BY session_id`,
		agentCutoff())
	if err != nil {
		return nil, fmt.Errorf("contando subagentes: %w", err)
	}
	defer rows.Close()

	out := map[string]int{}
	for rows.Next() {
		var id string
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			return nil, err
		}
		out[id] = n
	}
	return out, rows.Err()
}
