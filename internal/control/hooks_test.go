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
		agents  int
		want    string
	}{
		{"prompt", WorkIdle, HookEvent{Name: "UserPromptSubmit"}, 0, WorkWorking},
		{"prompt clears error", WorkError, HookEvent{Name: "UserPromptSubmit"}, 0, WorkWorking},
		{"tool", WorkIdle, HookEvent{Name: "PreToolUse", ToolName: "Bash"}, 0, WorkWorking},
		{"question", WorkWorking, HookEvent{Name: "PreToolUse", ToolName: "AskUserQuestion"}, 0, WorkWaitingInput},
		{"plan", WorkWorking, HookEvent{Name: "PreToolUse", ToolName: "ExitPlanMode"}, 0, WorkWaitingInput},
		{"answered", WorkWaitingInput, HookEvent{Name: "PostToolUse", ToolName: "AskUserQuestion"}, 0, WorkWorking},
		{"permission", WorkWorking, HookEvent{Name: "PermissionRequest", ToolName: "Bash"}, 0, WorkWaitingInput},
		{"permission notification", WorkWorking, HookEvent{Name: "Notification", NotificationType: "permission_prompt"}, 0, WorkWaitingInput},
		{"elicitation", WorkWorking, HookEvent{Name: "Notification", NotificationType: "elicitation_dialog"}, 0, WorkWaitingInput},
		{"idle after interrupt", WorkWorking, HookEvent{Name: "Notification", NotificationType: "idle_prompt"}, 0, WorkIdle},
		{"idle keeps question", WorkWaitingInput, HookEvent{Name: "Notification", NotificationType: "idle_prompt"}, 0, WorkWaitingInput},
		{"idle keeps error", WorkError, HookEvent{Name: "Notification", NotificationType: "idle_prompt"}, 0, WorkError},
		{"other notification", WorkWorking, HookEvent{Name: "Notification", NotificationType: "auth_success"}, 0, WorkWorking},
		{"stop", WorkWorking, HookEvent{Name: "Stop"}, 0, WorkIdle},
		{"api error", WorkWorking, HookEvent{Name: "StopFailure"}, 0, WorkError},
		{"session start", WorkError, HookEvent{Name: "SessionStart"}, 0, WorkIdle},
		{"session end", WorkWorking, HookEvent{Name: "SessionEnd"}, 0, WorkIdle},
		{"subagent ignored", WorkIdle, HookEvent{Name: "PreToolUse", ToolName: "Bash", AgentID: "a1"}, 0, WorkIdle},
		{"unknown event", WorkWorking, HookEvent{Name: "CwdChanged"}, 0, WorkWorking},
		{"compaction keeps working", WorkWorking, HookEvent{Name: "SessionStart", Source: "compact"}, 0, WorkWorking},
		{"stop with agents", WorkWorking, HookEvent{Name: "Stop"}, 2, WorkSubagents},
		{"idle prompt with agents", WorkWorking, HookEvent{Name: "Notification", NotificationType: "idle_prompt"}, 1, WorkSubagents},
		{"agent start while working", WorkWorking, HookEvent{Name: "SubagentStart", AgentID: "a1"}, 1, WorkWorking},
		{"agent stop, others left", WorkSubagents, HookEvent{Name: "SubagentStop", AgentID: "a1"}, 1, WorkSubagents},
		{"last agent stop", WorkSubagents, HookEvent{Name: "SubagentStop", AgentID: "a1"}, 0, WorkIdle},
		{"agent stop while working", WorkWorking, HookEvent{Name: "SubagentStop", AgentID: "a1"}, 0, WorkWorking},
		{"agent stop keeps question", WorkWaitingInput, HookEvent{Name: "SubagentStop", AgentID: "a1"}, 0, WorkWaitingInput},
		{"main wakes up", WorkSubagents, HookEvent{Name: "PreToolUse", ToolName: "Read"}, 1, WorkWorking},
		{"agent tool ignored", WorkSubagents, HookEvent{Name: "PreToolUse", ToolName: "Bash", AgentID: "a1"}, 1, WorkSubagents},
	}
	for _, c := range cases {
		if got := nextWorkStatus(c.current, c.ev, c.agents); got != c.want {
			t.Errorf("%s: nextWorkStatus(%q, %+v, %d) = %q, want %q", c.name, c.current, c.ev, c.agents, got, c.want)
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

func TestApplyHookWaitsForBackgroundAgents(t *testing.T) {
	m, _ := newTestManagerWithBus(t)
	rec, err := m.Create(CreateOpts{Title: "t", Cwd: t.TempDir(), Cols: 80, Rows: 24})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	steps := []struct {
		ev     HookEvent
		want   string
		agents int
	}{
		{HookEvent{Name: "UserPromptSubmit"}, WorkWorking, 0},
		{HookEvent{Name: "SubagentStart", AgentID: "a1"}, WorkWorking, 1},
		{HookEvent{Name: "SubagentStart", AgentID: "a2"}, WorkWorking, 2},
		{HookEvent{Name: "SubagentStart", AgentID: "a2"}, WorkWorking, 2},
		{HookEvent{Name: "Stop"}, WorkSubagents, 2},
		{HookEvent{Name: "SubagentStop", AgentID: "a1"}, WorkSubagents, 1},
		{HookEvent{Name: "SubagentStop", AgentID: "a2"}, WorkIdle, 0},
		{HookEvent{Name: "SubagentStop", AgentID: "nunca-arranco"}, WorkIdle, 0},
		{HookEvent{Name: "SubagentStart", AgentID: "a3"}, WorkIdle, 1},
		{HookEvent{Name: "SessionEnd"}, WorkIdle, 0},
	}
	for i, st := range steps {
		got, err := m.ApplyHook(rec.ID, st.ev)
		if err != nil {
			t.Fatalf("paso %d: %v", i, err)
		}
		if got.WorkStatus != st.want || got.RunningAgents != st.agents {
			t.Fatalf("paso %d (%s): = %q/%d, want %q/%d", i, st.ev.Name, got.WorkStatus, got.RunningAgents, st.want, st.agents)
		}
	}
}
