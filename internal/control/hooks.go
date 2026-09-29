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
)

// HookEvent es la parte del JSON que Claude Code le manda a un hook de la que
// depende work_status. El resto (tool_input, prompt, transcript_path…) se
// ignora.
type HookEvent struct {
	Name             string `json:"hook_event_name"`
	ToolName         string `json:"tool_name"`
	NotificationType string `json:"notification_type"`
	// AgentID viene cuando el evento lo dispara un subagente.
	AgentID string `json:"agent_id"`
}

// waitingTools son las tools cuyo único propósito es preguntarle algo al
// usuario: mientras corren, Claude no está trabajando sino esperando respuesta.
var waitingTools = map[string]bool{
	"AskUserQuestion": true,
	"ExitPlanMode":    true,
}

// nextWorkStatus es la máquina de estados: dado el estado actual y un evento,
// a cuál pasa la sesión. Los eventos que no dicen nada al respecto devuelven
// current sin tocar.
func nextWorkStatus(current string, ev HookEvent) string {
	// Los eventos de subagentes se ignoran. Mientras corre uno en primer plano
	// el agente principal ya está en working (está adentro de la tool Agent); y
	// uno en background sigue disparando PreToolUse después del Stop del
	// principal, lo que devolvería a working una sesión idle sin nada que la
	// saque de ahí.
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
				return WorkIdle
			}
		}
		return current
	case "Stop", "SessionStart", "SessionEnd":
		return WorkIdle
	case "StopFailure":
		return WorkError
	}
	return current
}

// ApplyHook mueve el work_status de la sesión según un evento de hook de Claude
// Code. Solo escribe y publica si el estado cambia de verdad: los hooks se
// disparan en cada tool call y la gran mayoría deja la sesión en working.
func (m *Manager) ApplyHook(id string, ev HookEvent) (*store.Session, error) {
	m.hookMu.Lock()
	defer m.hookMu.Unlock()

	rec, err := m.st.GetSession(id)
	if err != nil {
		return nil, err
	}
	next := nextWorkStatus(rec.WorkStatus, ev)
	if next == rec.WorkStatus {
		return rec, nil
	}
	if err := m.st.UpdateMeta(id, store.MetaPatch{WorkStatus: &next}); err != nil {
		return nil, err
	}
	rec.WorkStatus = next
	m.publish(events.SessionUpdated, id)
	return rec, nil
}
