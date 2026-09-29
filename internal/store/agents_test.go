package store

import (
	"testing"
	"time"
)

func TestRunningAgentsCountsAndExpires(t *testing.T) {
	st := newTestStore(t)
	for _, id := range []string{"s1", "s2"} {
		if err := st.CreateSession(&Session{ID: id, Cwd: "/tmp", Shell: "sh", Cols: 80, Rows: 24}); err != nil {
			t.Fatal(err)
		}
	}
	for _, a := range []string{"a1", "a2", "a2"} {
		if err := st.AddAgent("s1", a); err != nil {
			t.Fatal(err)
		}
	}
	// Uno que arrancó hace más de AgentStaleAfter y nunca avisó que terminó.
	old := time.Now().Add(-AgentStaleAfter - time.Minute).UnixMilli()
	if _, err := st.db.Exec(`INSERT INTO session_agents VALUES ('s2', 'viejo', ?)`, old); err != nil {
		t.Fatal(err)
	}

	count := func(id string) int {
		t.Helper()
		sess, err := st.GetSession(id)
		if err != nil {
			t.Fatal(err)
		}
		return sess.RunningAgents
	}
	if n := count("s1"); n != 2 {
		t.Fatalf("s1 = %d, esperaba 2", n)
	}
	if n := count("s2"); n != 0 {
		t.Fatalf("s2 = %d: el vencido no tiene que contar", n)
	}
	list, err := st.ListSessions()
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range list {
		if want := map[string]int{"s1": 2, "s2": 0}[s.ID]; s.RunningAgents != want {
			t.Fatalf("ListSessions %s = %d, esperaba %d", s.ID, s.RunningAgents, want)
		}
	}

	if err := st.RemoveAgent("s1", "a1"); err != nil {
		t.Fatal(err)
	}
	if n := count("s1"); n != 1 {
		t.Fatalf("después de RemoveAgent = %d", n)
	}
	// Reanudar el pty olvida los subagentes del Claude que murió con él.
	if err := st.MarkStarting("s1"); err != nil {
		t.Fatal(err)
	}
	if n := count("s1"); n != 0 {
		t.Fatalf("después de MarkStarting = %d", n)
	}
}
