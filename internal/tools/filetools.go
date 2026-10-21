package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// readTool implements read_file(path, start_line?, end_line?) — line-ranged
// reads with a default window so large files cannot blow the context window.
type readTool struct{ e *Engine }

func (t *readTool) Name() string { return "read_file" }
func (t *readTool) Description() string {
	return "Read a file's content, optionally by 1-based line range. Defaults to the first 2000 lines."
}
func (t *readTool) JSONSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path":       map[string]any{"type": "string", "description": "File path relative to the workspace root"},
			"start_line": map[string]any{"type": "integer", "description": "1-based start line (inclusive); default 1"},
			"end_line":   map[string]any{"type": "integer", "description": "1-based end line (inclusive); default start+1999"},
		},
		"required": []string{"path"},
	}
}

func (t *readTool) Execute(ctx context.Context, args json.RawMessage) Result {
	var a struct {
		Path      string `json:"path"`
		StartLine int    `json:"start_line"`
		EndLine   int    `json:"end_line"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return Result{Status: StatusFailure, ToolError: "invalid arguments", Stderr: err.Error()}
	}
	abs, err := t.e.resolvePath(a.Path)
	if err != nil {
		return Result{Status: StatusFailure, ToolError: err.Error(), Stderr: err.Error()}
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return Result{Status: StatusFailure, ToolError: err.Error(), Stderr: err.Error()}
	}
	lines := strings.Split(string(data), "\n")
	if a.StartLine <= 0 {
		a.StartLine = 1
	}
	if a.EndLine <= 0 {
		a.EndLine = a.StartLine + 1999
	}
	if a.StartLine > len(lines) {
		return Result{Status: StatusFailure, ToolError: "start_line beyond EOF",
			Stderr: fmt.Sprintf("start_line %d beyond EOF (%d lines)", a.StartLine, len(lines))}
	}
	if a.EndLine > len(lines) {
		a.EndLine = len(lines)
	}
	selected := lines[a.StartLine-1 : a.EndLine]
	var b strings.Builder
	for i, ln := range selected {
		fmt.Fprintf(&b, "%6d→%s\n", a.StartLine+i, ln)
	}
	out, truncated := truncate(b.String(), t.e.MaxOutputBytes)
	return Result{Status: StatusSuccess, Stdout: out, Truncated: truncated}
}

// writeTool implements write_file(path, content) — creates or overwrites a
// file atomically, creating parent directories as needed.
type writeTool struct{ e *Engine }

func (t *writeTool) Name() string { return "write_file" }
func (t *writeTool) Description() string {
	return "Create or overwrite a file with the given content. Creates parent directories as needed."
}
func (t *writeTool) JSONSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path":    map[string]any{"type": "string", "description": "File path relative to the workspace root"},
			"content": map[string]any{"type": "string", "description": "Full file content to write"},
		},
		"required": []string{"path", "content"},
	}
}

func (t *writeTool) Execute(ctx context.Context, args json.RawMessage) Result {
	var a struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return Result{Status: StatusFailure, ToolError: "invalid arguments", Stderr: err.Error()}
	}
	abs, err := t.e.resolvePath(a.Path)
	if err != nil {
		return Result{Status: StatusFailure, ToolError: err.Error(), Stderr: err.Error()}
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return Result{Status: StatusFailure, ToolError: err.Error(), Stderr: err.Error()}
	}
	if err := atomicWrite(abs, []byte(a.Content)); err != nil {
		return Result{Status: StatusFailure, ToolError: err.Error(), Stderr: err.Error()}
	}
	return Result{Status: StatusSuccess, Stdout: fmt.Sprintf("wrote %d bytes to %s", len(a.Content), a.Path)}
}

// editTool implements edit_file(path, search_block, replace_block) — precise,
// race-aware single-occurrence replacement. It never blind-writes.
type editTool struct{ e *Engine }

func (t *editTool) Name() string { return "edit_file" }
func (t *editTool) Description() string {
	return "Replace an exact search_block with replace_block. The search block must appear exactly once."
}
func (t *editTool) JSONSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path":          map[string]any{"type": "string", "description": "File path relative to the workspace root"},
			"search_block":  map[string]any{"type": "string", "description": "Exact text to find (must be unique in the file)"},
			"replace_block": map[string]any{"type": "string", "description": "Replacement text"},
		},
		"required": []string{"path", "search_block", "replace_block"},
	}
}

func (t *editTool) Execute(ctx context.Context, args json.RawMessage) Result {
	var a struct {
		Path         string `json:"path"`
		SearchBlock  string `json:"search_block"`
		ReplaceBlock string `json:"replace_block"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return Result{Status: StatusFailure, ToolError: "invalid arguments", Stderr: err.Error()}
	}
	abs, err := t.e.resolvePath(a.Path)
	if err != nil {
		return Result{Status: StatusFailure, ToolError: err.Error(), Stderr: err.Error()}
	}
	if a.SearchBlock == "" {
		return Result{Status: StatusFailure, ToolError: "empty search_block", Stderr: "search_block must not be empty"}
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return Result{Status: StatusFailure, ToolError: err.Error(), Stderr: err.Error()}
	}
	content := string(data)
	count := strings.Count(content, a.SearchBlock)
	if count == 0 {
		return Result{Status: StatusFailure, ToolError: "search_block not found",
			Stderr: "search_block not found in " + a.Path + "; re-read the file and match exactly"}
	}
	if count > 1 {
		return Result{Status: StatusFailure, ToolError: "ambiguous search_block",
			Stderr: fmt.Sprintf("search_block appears %d times; provide more surrounding context to make it unique", count)}
	}
	updated := strings.Replace(content, a.SearchBlock, a.ReplaceBlock, 1)
	if err := atomicWrite(abs, []byte(updated)); err != nil {
		return Result{Status: StatusFailure, ToolError: err.Error(), Stderr: err.Error()}
	}
	return Result{Status: StatusSuccess, Stdout: fmt.Sprintf("edited %s (1 replacement)", a.Path)}
}

// atomicWrite writes via a temp file in the same directory then renames, so a
// concurrent reader never observes a partially-written file.
func atomicWrite(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".sutra-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		os.Remove(tmpName)
		return err
	}
	return os.Rename(tmpName, path)
}
