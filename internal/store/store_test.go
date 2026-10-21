package store

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestEmissionSchemaContract(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(filepath.Join(dir, "db.sqlite"), filepath.Join(dir, "interactions"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = st.Close() }()

	sid, err := st.NewSession("test goal")
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	rc := 1
	err = st.Emit(Interaction{
		SessionID: sid, Turn: 3, Goal: "test goal", Tool: "execute_shell",
		Args:   json.RawMessage(`{"command":"go test"}`),
		Status: "FAILURE", Stderr: "boom", ReturnCode: &rc,
		DurationMS: 12, ToolError: "exit code 1", Corrected: false,
		TokensPrompt: 100, TokensCompletion: 20, SessionStatus: "running",
	})
	if err != nil {
		t.Fatalf("emit: %v", err)
	}

	path := filepath.Join(dir, "interactions", sid+".jsonl")
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open jsonl: %v", err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	if !sc.Scan() {
		t.Fatal("no line")
	}
	var rec Interaction
	if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if rec.Schema != 1 {
		t.Fatalf("schema = %d", rec.Schema)
	}
	if rec.Tool != "execute_shell" || rec.Status != "FAILURE" || rec.ReturnCode == nil || *rec.ReturnCode != 1 {
		t.Fatalf("record = %+v", rec)
	}
	if rec.SessionStatus != "running" {
		t.Fatalf("session_status = %q", rec.SessionStatus)
	}
}

func TestTurnsAndResumeRoundTrip(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(filepath.Join(dir, "db.sqlite"), filepath.Join(dir, "interactions"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = st.Close() }()

	sid, _ := st.NewSession("goal")
	_ = st.AppendTurn(sid, 0, "system", "sys", "", "", "", "", nil)
	_ = st.AppendTurn(sid, 0, "user", "do it", "", "", "", "", nil)
	_ = st.AppendTurn(sid, 1, "assistant", "", `[{"id":"c1","name":"ls_recursive","arguments":"{}"}]`, "", "", "thinking it through", nil)
	_ = st.AppendTurn(sid, 1, "tool", "status: SUCCESS\n", "", "c1", "ls_recursive", "", nil)

	turns, err := st.ListTurns(sid)
	if err != nil {
		t.Fatalf("list turns: %v", err)
	}
	if len(turns) != 4 {
		t.Fatalf("turns = %d", len(turns))
	}
	if turns[2].ToolCalls == "" {
		t.Fatal("expected persisted tool_calls JSON")
	}

	sessions, err := st.ListSessions(10)
	if err != nil || len(sessions) != 1 || sessions[0].ID != sid {
		t.Fatalf("sessions = %+v err=%v", sessions, err)
	}
}
