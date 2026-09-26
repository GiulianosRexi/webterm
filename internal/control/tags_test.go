package control

import (
	"testing"

	"github.com/giuliano/webterm/internal/events"
)

func TestTagsPublicanSessionUpdated(t *testing.T) {
	m, bus := newTestManagerWithBus(t)
	ch, stop := bus.Subscribe()
	defer stop()

	rec, err := m.Create(CreateOpts{Title: "t", Cwd: t.TempDir(), Cols: 80, Rows: 24})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	steps := []func() error{
		func() error { return m.SetSessionTags(rec.ID, []string{"bugfix"}) },
		func() error { return m.AddSessionTags(rec.ID, []string{"frontend"}) },
		func() error { return m.RemoveSessionTags(rec.ID, []string{"bugfix"}) },
	}
	for _, step := range steps {
		if err := step(); err != nil {
			t.Fatalf("step: %v", err)
		}
		if ev := waitFor(t, ch, events.SessionUpdated); ev.SessionID != rec.ID {
			t.Fatalf("SessionID = %q, esperaba %q", ev.SessionID, rec.ID)
		}
	}

	got, err := m.Get(rec.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(got.Tags) != 1 || got.Tags[0] != "frontend" {
		t.Fatalf("Tags = %v", got.Tags)
	}
}
