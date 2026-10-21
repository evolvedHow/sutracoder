package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"sutracoder/internal/config"
	"sutracoder/internal/provider"
	"sutracoder/internal/store"
	"sutracoder/internal/tools"
)

func testConfig() config.Config {
	cfg := config.Defaults()
	cfg.MaxTurns = 10
	cfg.MaxTokensBudget = 100000
	cfg.ShellTimeoutSec = 5
	return cfg
}

// newTestStore opens a store in a temp dir and returns it plus its interactions dir.
func newTestStore(t *testing.T) (*store.Store, string) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "test.db"), filepath.Join(dir, "interactions"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st, filepath.Join(dir, "interactions")
}

func TestLoopForwardsReasoningDeltas(t *testing.T) {
	dir := t.TempDir()
	engine := tools.NewEngine(dir)

	// Script a reasoning-carrying answer that terminates the loop.
	fake := &provider.FakeProvider{Responses: []*provider.Response{
		{Message: provider.Message{
			Role:      provider.RoleAssistant,
			Content:   "the answer",
			Reasoning: "thinking hard",
		}, StopReason: "stop"},
	}}

	var kinds []EventKind
	var reasoning strings.Builder
	sink := func(ev Event) {
		kinds = append(kinds, ev.Kind)
		if ev.Kind == EventReasoningDelta {
			reasoning.WriteString(ev.Text)
		}
	}
	loop := New(testConfig(), fake, engine, nil, nil, sink)
	if err := loop.Start(context.Background(), "system", "go"); err != nil {
		t.Fatalf("loop error: %v", err)
	}
	if reasoning.String() != "thinking hard" {
		t.Fatalf("reasoning forwarded = %q", reasoning.String())
	}
	found := false
	for _, k := range kinds {
		if k == EventReasoningDelta {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected EventReasoningDelta in kinds %v", kinds)
	}
}

func TestGoldenLoopHappyPath(t *testing.T) {
	dir := t.TempDir()
	engine := tools.NewEngine(dir)
	st, _ := newTestStore(t)

	// Script: update_plan → write_file → complete_goal.
	fake := &provider.FakeProvider{Responses: []*provider.Response{
		provider.ScriptedToolCall("c1", "update_plan", map[string]any{
			"items": []map[string]any{{"text": "write a file", "status": "in_progress"}},
		}),
		provider.ScriptedToolCall("c2", "write_file", map[string]any{
			"path": "hello.txt", "content": "hi",
		}),
		provider.ScriptedToolCall("c3", "complete_goal", map[string]any{
			"summary": "wrote hello.txt",
		}),
		provider.ScriptedText("done"),
	}}

	loop := New(testConfig(), fake, engine, st, nil, nil)
	if err := loop.Start(context.Background(), "system", "write a hello file"); err != nil {
		t.Fatalf("loop error: %v", err)
	}

	if !engine.Completed() {
		t.Fatal("expected complete_goal to fire")
	}
	if engine.CompletionSummary() != "wrote hello.txt" {
		t.Fatalf("summary = %q", engine.CompletionSummary())
	}
	// The file should exist on disk.
	if _, err := os.Stat(filepath.Join(dir, "hello.txt")); err != nil {
		t.Fatalf("expected hello.txt: %v", err)
	}
	// Plan should be persisted on the engine.
	if len(engine.Plan()) != 1 {
		t.Fatalf("plan = %+v", engine.Plan())
	}
}

func TestGoldenLoopErrorCorrection(t *testing.T) {
	dir := t.TempDir()
	engine := tools.NewEngine(dir)

	// Script: edit a non-existent file (FAILURE), then write, then complete.
	fake := &provider.FakeProvider{Responses: []*provider.Response{
		provider.ScriptedToolCall("c1", "edit_file", map[string]any{
			"path": "missing.txt", "search_block": "a", "replace_block": "b",
		}),
		provider.ScriptedToolCall("c2", "write_file", map[string]any{
			"path": "missing.txt", "content": "a",
		}),
		provider.ScriptedToolCall("c3", "complete_goal", map[string]any{"summary": "recovered"}),
		provider.ScriptedText("done"),
	}}

	loop := New(testConfig(), fake, engine, nil, nil, nil)
	if err := loop.Start(context.Background(), "system", "fix it"); err != nil {
		t.Fatalf("loop error: %v", err)
	}
	if !engine.Completed() {
		t.Fatal("expected completion after correction")
	}
}

func TestGoldenLoopInteractionEmission(t *testing.T) {
	dir := t.TempDir()
	engine := tools.NewEngine(dir)
	st, interDir := newTestStore(t)

	fake := &provider.FakeProvider{Responses: []*provider.Response{
		provider.ScriptedToolCall("c1", "write_file", map[string]any{"path": "a.txt", "content": "data"}),
		provider.ScriptedToolCall("c2", "complete_goal", map[string]any{"summary": "ok"}),
		provider.ScriptedText("done"),
	}}
	loop := New(testConfig(), fake, engine, st, nil, nil)
	if err := loop.Start(context.Background(), "system", "goal"); err != nil {
		t.Fatalf("loop: %v", err)
	}

	// Read the JSONL sidecar for the session.
	sid := loop.SessionID()
	path := filepath.Join(interDir, sid+".jsonl")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("expected interaction log: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) == 0 {
		t.Fatal("no interaction lines")
	}
	var first map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatalf("bad JSONL: %v", err)
	}
	// Schema contract fields.
	for _, key := range []string{"schema", "session_id", "turn", "ts", "goal", "tool", "args", "status", "session_status"} {
		if _, ok := first[key]; !ok {
			t.Fatalf("interaction missing %q: %v", key, first)
		}
	}
	if first["schema"].(float64) != 1 {
		t.Fatalf("schema = %v", first["schema"])
	}
	if first["tool"].(string) != "write_file" {
		t.Fatalf("tool = %v", first["tool"])
	}
	if first["status"].(string) != tools.StatusSuccess {
		t.Fatalf("status = %v", first["status"])
	}
}

func TestLoopRespectsMaxTurns(t *testing.T) {
	engine := tools.NewEngine(t.TempDir())
	// Provider that always wants another tool, never completing.
	fake := &provider.FakeProvider{Responses: []*provider.Response{
		provider.ScriptedToolCall("c", "ls_recursive", map[string]any{}),
	}}
	cfg := testConfig()
	cfg.MaxTurns = 3
	loop := New(cfg, fake, engine, nil, nil, nil)
	err := loop.Start(context.Background(), "system", "loop forever")
	if err == nil {
		t.Fatal("expected max-turns fatal error")
	}
	var fatal ErrFatal
	if !errors.As(err, &fatal) {
		t.Fatalf("expected ErrFatal, got %v", err)
	}
	if !strings.Contains(err.Error(), "max turns") {
		t.Fatalf("error = %v", err)
	}
}

func TestAuthErrorIsFatal(t *testing.T) {
	engine := tools.NewEngine(t.TempDir())
	fake := &provider.FakeProvider{
		Responses: []*provider.Response{provider.ScriptedText("unused")},
		Errs:      map[int]error{0: &provider.StatusError{StatusCode: 401, Body: "bad key", Provider: "fake"}},
	}
	loop := New(testConfig(), fake, engine, nil, nil, nil)
	err := loop.Start(context.Background(), "system", "goal")
	var fatal ErrFatal
	if !errors.As(err, &fatal) {
		t.Fatalf("401 should be fatal, got %v", err)
	}
}

func TestTransientErrorRetriesThenSucceeds(t *testing.T) {
	engine := tools.NewEngine(t.TempDir())
	// The fake advances its counter even on a scripted error, so the retry
	// (call index 1) must carry the tool call we want to succeed.
	fake := &provider.FakeProvider{
		Responses: []*provider.Response{
			provider.ScriptedText("placeholder"), // index 0, errors via Errs
			provider.ScriptedToolCall("c1", "complete_goal", map[string]any{"summary": "ok"}),
			provider.ScriptedText("done"),
		},
		Errs: map[int]error{0: &provider.StatusError{StatusCode: 429, Body: "rate limited", Provider: "fake"}},
	}
	loop := New(testConfig(), fake, engine, nil, nil, nil)
	if err := loop.Start(context.Background(), "system", "goal"); err != nil {
		t.Fatalf("expected retry success, got %v", err)
	}
	if !engine.Completed() {
		t.Fatal("expected completion after retry")
	}
	if len(fake.Requests) < 2 {
		t.Fatalf("expected at least 2 provider calls (retry), got %d", len(fake.Requests))
	}
}

// blockProvider blocks Complete until the context is cancelled, then returns
// the context error — used to exercise the cancellation path deterministically.
type blockProvider struct{}

func (blockProvider) Name() string { return "block" }
func (blockProvider) Complete(ctx context.Context, req provider.Request, onDelta func(provider.Event) error) (*provider.Response, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestCancellationStopsLoop(t *testing.T) {
	engine := tools.NewEngine(t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(30 * time.Millisecond); cancel() }()
	loop := New(testConfig(), blockProvider{}, engine, nil, nil, nil)
	err := loop.Start(ctx, "system", "goal")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}
