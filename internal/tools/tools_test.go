package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newTestEngine returns an Engine rooted at a fresh temp dir.
func newTestEngine(t *testing.T) *Engine {
	t.Helper()
	return NewEngine(t.TempDir())
}

func mustRun(t *testing.T, e *Engine, name string, args map[string]any) Result {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	return e.Run(context.Background(), name, raw)
}

func TestReadWriteEditRoundTrip(t *testing.T) {
	e := newTestEngine(t)

	// write_file creates the file and parents.
	res := mustRun(t, e, "write_file", map[string]any{"path": "a/b.txt", "content": "hello world"})
	if res.Status != StatusSuccess {
		t.Fatalf("write_file: %+v", res)
	}

	// read_file returns line-numbered content.
	res = mustRun(t, e, "read_file", map[string]any{"path": "a/b.txt"})
	if res.Status != StatusSuccess || !strings.Contains(res.Stdout, "hello world") {
		t.Fatalf("read_file: %+v", res)
	}

	// edit_file replaces the unique block.
	res = mustRun(t, e, "edit_file", map[string]any{"path": "a/b.txt", "search_block": "hello", "replace_block": "goodbye"})
	if res.Status != StatusSuccess {
		t.Fatalf("edit_file: %+v", res)
	}
	res = mustRun(t, e, "read_file", map[string]any{"path": "a/b.txt"})
	if !strings.Contains(res.Stdout, "goodbye world") {
		t.Fatalf("edit did not apply: %+v", res)
	}
}

func TestEditAmbiguousAndMissing(t *testing.T) {
	e := newTestEngine(t)
	_ = mustRun(t, e, "write_file", map[string]any{"path": "x.txt", "content": "aa bb aa"})

	// Ambiguous (appears twice) must fail without writing.
	res := mustRun(t, e, "edit_file", map[string]any{"path": "x.txt", "search_block": "aa", "replace_block": "cc"})
	if res.Status != StatusFailure {
		t.Fatalf("expected FAILURE for ambiguous block, got %+v", res)
	}

	// Missing block must fail.
	res = mustRun(t, e, "edit_file", map[string]any{"path": "x.txt", "search_block": "zzz", "replace_block": "cc"})
	if res.Status != StatusFailure || !strings.Contains(res.Stderr, "not found") {
		t.Fatalf("expected not-found FAILURE, got %+v", res)
	}
}

func TestPathConfinement(t *testing.T) {
	e := newTestEngine(t)
	for _, p := range []string{"../escape.txt", "/etc/passwd", ".."} {
		res := mustRun(t, e, "read_file", map[string]any{"path": p})
		if res.Status != StatusFailure {
			t.Fatalf("path %q should be confined, got %+v", p, res)
		}
	}
}

func TestTruncation(t *testing.T) {
	e := newTestEngine(t)
	e.MaxOutputBytes = 100
	big := strings.Repeat("x", 5000)
	_ = mustRun(t, e, "write_file", map[string]any{"path": "big.txt", "content": big})
	res := mustRun(t, e, "read_file", map[string]any{"path": "big.txt"})
	if !res.Truncated {
		t.Fatalf("expected truncated result, got %+v", res)
	}
	if !strings.Contains(res.Stdout, "truncated") {
		t.Fatalf("expected truncation marker, got %q", res.Stdout)
	}
}

func TestReadLineRange(t *testing.T) {
	e := newTestEngine(t)
	_ = mustRun(t, e, "write_file", map[string]any{"path": "n.txt", "content": "one\ntwo\nthree\nfour"})
	res := mustRun(t, e, "read_file", map[string]any{"path": "n.txt", "start_line": 2, "end_line": 3})
	if res.Status != StatusSuccess {
		t.Fatalf("read range: %+v", res)
	}
	if strings.Contains(res.Stdout, "one") || !strings.Contains(res.Stdout, "two") || !strings.Contains(res.Stdout, "three") {
		t.Fatalf("range read wrong content: %q", res.Stdout)
	}
}

func TestGrepFallbackFindsMatches(t *testing.T) {
	e := newTestEngine(t)
	_ = mustRun(t, e, "write_file", map[string]any{"path": "g.go", "content": "package g\nfunc Payment() {}\n"})
	_ = mustRun(t, e, "write_file", map[string]any{"path": "h.go", "content": "package h\n"})
	res := mustRun(t, e, "grep_search", map[string]any{"query": "Payment", "glob": "*.go"})
	if res.Status != StatusSuccess || !strings.Contains(res.Stdout, "Payment") {
		t.Fatalf("grep: %+v", res)
	}
}

func TestUpdatePlanAndComplete(t *testing.T) {
	e := newTestEngine(t)
	res := mustRun(t, e, "update_plan", map[string]any{"items": []map[string]any{
		{"text": "step one", "status": "pending"},
		{"text": "step two", "status": "in_progress"},
	}})
	if res.Status != StatusSuccess {
		t.Fatalf("update_plan: %+v", res)
	}
	plan := e.Plan()
	if len(plan) != 2 || plan[0].Text != "step one" {
		t.Fatalf("plan not stored: %+v", plan)
	}

	// Invalid status rejected.
	res = mustRun(t, e, "update_plan", map[string]any{"items": []map[string]any{{"text": "x", "status": "bogus"}}})
	if res.Status != StatusFailure {
		t.Fatalf("expected invalid status rejection, got %+v", res)
	}

	if e.Completed() {
		t.Fatal("should not be complete yet")
	}
	res = mustRun(t, e, "complete_goal", map[string]any{"summary": "all done"})
	if res.Status != StatusSuccess || !e.Completed() || e.CompletionSummary() != "all done" {
		t.Fatalf("complete_goal: %+v completed=%v", res, e.Completed())
	}
}

func TestUnknownTool(t *testing.T) {
	e := newTestEngine(t)
	res := mustRun(t, e, "no_such_tool", map[string]any{})
	if res.Status != StatusFailure || !strings.Contains(res.Stderr, "unknown tool") {
		t.Fatalf("unknown tool: %+v", res)
	}
}

func TestLsPrunesVendor(t *testing.T) {
	dir := t.TempDir()
	e := NewEngine(dir)
	_ = os.MkdirAll(filepath.Join(dir, "node_modules", "pkg"), 0o755)
	_ = os.WriteFile(filepath.Join(dir, "node_modules", "pkg", "index.js"), []byte("x"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main"), 0o644)
	res := mustRun(t, e, "ls_recursive", map[string]any{})
	if res.Status != StatusSuccess {
		t.Fatalf("ls: %+v", res)
	}
	if strings.Contains(res.Stdout, "node_modules") {
		t.Fatalf("ls should prune node_modules: %q", res.Stdout)
	}
	if !strings.Contains(res.Stdout, "main.go") {
		t.Fatalf("ls should list main.go: %q", res.Stdout)
	}
}
