package control

import (
	"github.com/giuliano/webterm/internal/events"
	"github.com/giuliano/webterm/internal/store"
)

// Estados de work_status: qué está haciendo Claude ahora mismo adentro de la
// sesión. Los mueven los hooks de Claude Code, nunca la mano.
const (
	WorkIdle         = "idle"
	WorkWorking      = "working"
	WorkWaitingInput = "waiting_input"
	WorkError        = "error"
	// WorkSubagents es "Claude terminó su turno pero quedan subagentes
	// corriendo": no pide nada, pero tampoco está idle.
	WorkSubagents = "subagents"
)

// HookEvent es la parte del JSON que Claude Code le manda a un hook de la que
// depende work_status. El resto (tool_input, prompt, transcript_path…) se
// ignora.
type HookEvent struct {
	Name             string `json:"hook_event_name"`
	ToolName         string `json:"tool_name"`
	NotificationType string `json:"notification_type"`
	// Source es de dónde viene un SessionStart: startup, resume, clear,
	// compact o fork.
	Source string `json:"source"`
	// AgentID viene cuando el evento lo dispara un subagente.
	AgentID string `json:"agent_id"`
}

// waitingTools son las tools cuyo único propósito es preguntarle algo al
// usuario: mientras corren, Claude no está trabajando sino esperando respuesta.
var waitingTools = map[string]bool{
	"AskUserQuestion": true,
	"ExitPlanMode":    true,
}

// nextWorkStatus es la máquina de estados: dado el estado actual, un evento y
// cuántos subagentes quedan corriendo después de aplicarlo, a cuál pasa la
// sesión. Los eventos que no dicen nada al respecto devuelven current sin
// tocar.
func nextWorkStatus(current string, ev HookEvent, agents int) string {
	// rest es a dónde va la sesión cuando el agente principal deja de
	// trabajar: idle, o esperando a los subagentes que lanzó.
	rest := WorkIdle
	if agents > 0 {
		rest = WorkSubagents
	}

	switch ev.Name {
	case "SubagentStart":
		// Mientras el principal trabaja, working ya lo dice todo.
		return current
	case "SubagentStop":
		if current == WorkSubagents {
			return rest
		}
		return current
	}
	// El resto de los eventos de subagentes se ignoran. Mientras corre uno en
	// primer plano el agente principal ya está en working (está adentro de la
	// tool Agent); y uno en background sigue disparando PreToolUse después del
	// Stop del principal, lo que devolvería a working una sesión que no está
	// esperando nada de Claude.
	if ev.AgentID != "" {
		return current
	}
	switch ev.Name {
	case "UserPromptSubmit", "PostToolUse", "PostToolUseFailure":
		return WorkWorking
	case "PreToolUse":
		if waitingTools[ev.ToolName] {
			return WorkWaitingInput
		}
		return WorkWorking
	case "PermissionRequest":
		return WorkWaitingInput
	case "Notification":
		switch ev.NotificationType {
		case "permission_prompt", "elicitation_dialog", "elicitation_url_dialog":
			return WorkWaitingInput
		case "idle_prompt":
			// Claude espera un prompt nuevo. Viniendo de working quiere decir
			// que el turno terminó sin Stop (una interrupción, por ejemplo);
			// desde cualquier otro estado borraría algo que vale la pena
			// conservar, como una pregunta abierta o un error.
			if current == WorkWorking {
				return rest
			}
		}
		return current
	case "Stop":
		return rest
	case "SessionStart", "SessionEnd":
		// Una compactación dispara SessionStart en medio del turno: Claude
		// sigue en lo que estaba, y sus subagentes también.
		if isCompaction(ev) {
			return current
		}
		// Llegan con los subagentes ya olvidados (ver trackAgents): un Claude
		// nuevo o que se fue no tiene subagentes propios corriendo.
		return WorkIdle
	case "StopFailure":
		return WorkError
	}
	return current
}

// ApplyHook mueve el work_status de la sesión según un evento de hook de Claude
// Code, llevando de paso la cuenta de subagentes corriendo. Solo escribe y
// publica si algo cambia de verdad: los hooks se disparan en cada tool call y
// la gran mayoría deja la sesión en working.
func (m *Manager) ApplyHook(id string, ev HookEvent) (*store.Session, error) {
	m.hookMu.Lock()
	defer m.hookMu.Unlock()

	before, err := m.st.GetSession(id)
	if err != nil {
		return nil, err
	}
	if err := m.trackAgents(id, ev); err != nil {
		return nil, err
	}
	rec, err := m.st.GetSession(id)
	if err != nil {
		return nil, err
	}

	next := nextWorkStatus(rec.WorkStatus, ev, rec.RunningAgents)
	if next != rec.WorkStatus {
		if err := m.st.UpdateMeta(id, store.MetaPatch{WorkStatus: &next}); err != nil {
			return nil, err
		}
		rec.WorkStatus = next
	}
	if rec.WorkStatus != before.WorkStatus || rec.RunningAgents != before.RunningAgents {
		m.publish(events.SessionUpdated, id)
	}
	return rec, nil
}

// trackAgents actualiza el conjunto de subagentes corriendo según el evento.
func (m *Manager) trackAgents(id string, ev HookEvent) error {
	switch ev.Name {
	case "SubagentStart":
		if ev.AgentID != "" {
			return m.st.AddAgent(id, ev.AgentID)
		}
	case "SubagentStop":
		if ev.AgentID != "" {
			return m.st.RemoveAgent(id, ev.AgentID)
		}
	case "SessionStart", "SessionEnd":
		if !isCompaction(ev) {
			return m.st.ClearAgents(id)
		}
	}
	return nil
}

func isCompaction(ev HookEvent) bool {
	return ev.Name == "SessionStart" && ev.Source == "compact"
}
