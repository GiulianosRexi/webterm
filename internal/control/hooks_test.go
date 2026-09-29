package control

import (
	"testing"

	"github.com/giuliano/webterm/internal/events"
)

func TestNextWorkStatus(t *testing.T) {
	cases := []struct {
		name    string
		current string
		ev      HookEvent
		want    string
	}{
		{"prompt", WorkIdle, HookEvent{Name: "UserPromptSubmit"}, WorkWorking},
		{"prompt clears error", WorkError, HookEvent{Name: "UserPromptSubmit"}, WorkWorking},
		{"tool", WorkIdle, HookEvent{Name: "PreToolUse", ToolName: "Bash"}, WorkWorking},
		{"question", WorkWorking, HookEvent{Name: "PreToolUse", ToolName: "AskUserQuestion"}, WorkWaitingInput},
		{"plan", WorkWorking, HookEvent{Name: "PreToolUse", ToolName: "ExitPlanMode"}, WorkWaitingInput},
		{"answered", WorkWaitingInput, HookEvent{Name: "PostToolUse", ToolName: "AskUserQuestion"}, WorkWorking},
		{"permission", WorkWorking, HookEvent{Name: "PermissionRequest", ToolName: "Bash"}, WorkWaitingInput},
		{"permission notification", WorkWorking, HookEvent{Name: "Notification", NotificationType: "permission_prompt"}, WorkWaitingInput},
		{"elicitation", WorkWorking, HookEvent{Name: "Notification", NotificationType: "elicitation_dialog"}, WorkWaitingInput},
		{"idle after interrupt", WorkWorking, HookEvent{Name: "Notification", NotificationType: "idle_prompt"}, WorkIdle},
		{"idle keeps question", WorkWaitingInput, HookEvent{Name: "Notification", NotificationType: "idle_prompt"}, WorkWaitingInput},
		{"idle keeps error", WorkError, HookEvent{Name: "Notification", NotificationType: "idle_prompt"}, WorkError},
		{"other notification", WorkWorking, HookEvent{Name: "Notification", NotificationType: "auth_success"}, WorkWorking},
		{"stop", WorkWorking, HookEvent{Name: "Stop"}, WorkIdle},
		{"api error", WorkWorking, HookEvent{Name: "StopFailure"}, WorkError},
		{"session start", WorkError, HookEvent{Name: "SessionStart"}, WorkIdle},
		{"session end", WorkWorking, HookEvent{Name: "SessionEnd"}, WorkIdle},
		{"subagent ignored", WorkIdle, HookEvent{Name: "PreToolUse", ToolName: "Bash", AgentID: "a1"}, WorkIdle},
		{"unknown event", WorkWorking, HookEvent{Name: "CwdChanged"}, WorkWorking},
	}
	for _, c := range cases {
		if got := nextWorkStatus(c.current, c.ev); got != c.want {
			t.Errorf("%s: nextWorkStatus(%q, %+v) = %q, want %q", c.name, c.current, c.ev, got, c.want)
		}
	}
}

func TestApplyHookPublishesOnlyOnChange(t *testing.T) {
	m, bus := newTestManagerWithBus(t)
	rec, err := m.Create(CreateOpts{Title: "t", Cwd: t.TempDir(), Cols: 80, Rows: 24})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	ch, stop := bus.Subscribe()
	defer stop()

	got, err := m.ApplyHook(rec.ID, HookEvent{Name: "UserPromptSubmit"})
	if err != nil {
		t.Fatalf("ApplyHook: %v", err)
	}
	if got.WorkStatus != WorkWorking {
		t.Fatalf("WorkStatus = %q", got.WorkStatus)
	}
	waitFor(t, ch, events.SessionUpdated)

	// Seguir en working no es un cambio: no tiene que salir ningún evento.
	if _, err := m.ApplyHook(rec.ID, HookEvent{Name: "PreToolUse", ToolName: "Bash"}); err != nil {
		t.Fatalf("ApplyHook: %v", err)
	}
	select {
	case ev := <-ch:
		t.Fatalf("evento inesperado: %+v", ev)
	default:
	}

	stored, err := m.Get(rec.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if stored.WorkStatus != WorkWorking {
		t.Fatalf("persistido = %q", stored.WorkStatus)
	}
}
