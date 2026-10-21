package tools

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// grepTool implements grep_search(query, path?, glob?) with a three-tier
// fallback: ripgrep → grep → a pure-Go walk so the tool degrades gracefully
// when neither external binary is present.
type grepTool struct{ e *Engine }

func (t *grepTool) Name() string { return "grep_search" }
func (t *grepTool) Description() string {
	return "Search file contents for a regex pattern. Uses ripgrep or grep when available; falls back to a built-in search. Results are capped."
}
func (t *grepTool) JSONSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path":  map[string]any{"type": "string", "description": "Directory or file to search; default the workspace root"},
			"query": map[string]any{"type": "string", "description": "Regular expression to search for"},
			"glob":  map[string]any{"type": "string", "description": "Optional filename glob filter, e.g. *.go"},
		},
		"required": []string{"query"},
	}
}

const grepMaxResults = 200

func (t *grepTool) Execute(ctx context.Context, args json.RawMessage) Result {
	var a struct {
		Path  string `json:"path"`
		Query string `json:"query"`
		Glob  string `json:"glob"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return Result{Status: StatusFailure, ToolError: "invalid arguments", Stderr: err.Error()}
	}
	if a.Query == "" {
		return Result{Status: StatusFailure, ToolError: "empty query", Stderr: "query must not be empty"}
	}
	searchRoot := t.e.Root
	if a.Path != "" {
		abs, err := t.e.resolvePath(a.Path)
		if err != nil {
			return Result{Status: StatusFailure, ToolError: err.Error(), Stderr: err.Error()}
		}
		searchRoot = abs
	}

	if out, ok := t.tryRipgrep(ctx, searchRoot, a.Query, a.Glob); ok {
		return out
	}
	if out, ok := t.tryGrep(ctx, searchRoot, a.Query, a.Glob); ok {
		return out
	}
	return t.pureGo(ctx, searchRoot, a.Query, a.Glob)
}

func (t *grepTool) tryRipgrep(ctx context.Context, root, query, glob string) (Result, bool) {
	if _, err := exec.LookPath("rg"); err != nil {
		return Result{}, false
	}
	argv := []string{"--no-heading", "--line-number", "--color", "never",
		fmt.Sprintf("--max-count=%d", grepMaxResults), "--max-columns=300", "-e", query}
	if glob != "" {
		argv = append(argv, "--glob", glob)
	}
	argv = append(argv, root)
	cmd := exec.CommandContext(ctx, "rg", argv...)
	out, err := cmd.Output()
	if err != nil {
		if ee, isExit := err.(*exec.ExitError); isExit && ee.ExitCode() == 1 {
			return Result{Status: StatusSuccess, Stdout: "no matches"}, true // rg exit 1 = no match
		}
		if ctx.Err() != nil {
			return Result{Status: StatusInterrupted, Stderr: "interrupted"}, true
		}
		return Result{}, false // fall through to next tier on other errors
	}
	s, tr := truncate(string(out), t.e.MaxOutputBytes)
	return Result{Status: StatusSuccess, Stdout: s, Truncated: tr}, true
}

func (t *grepTool) tryGrep(ctx context.Context, root, query, glob string) (Result, bool) {
	if _, err := exec.LookPath("grep"); err != nil {
		return Result{}, false
	}
	argv := []string{"-rn", "--color=never", "-E", "-e", query}
	if glob != "" {
		argv = append(argv, "--include", glob)
	}
	argv = append(argv, root)
	cmd := exec.CommandContext(ctx, "grep", argv...)
	out, err := cmd.Output()
	if err != nil {
		if ee, isExit := err.(*exec.ExitError); isExit && ee.ExitCode() == 1 {
			return Result{Status: StatusSuccess, Stdout: "no matches"}, true
		}
		if ctx.Err() != nil {
			return Result{Status: StatusInterrupted, Stderr: "interrupted"}, true
		}
		return Result{}, false
	}
	s, tr := truncate(string(out), t.e.MaxOutputBytes)
	return Result{Status: StatusSuccess, Stdout: s, Truncated: tr}, true
}

func (t *grepTool) pureGo(ctx context.Context, root, query, glob string) Result {
	re, err := regexp.Compile(query)
	if err != nil {
		return Result{Status: StatusFailure, ToolError: "invalid regex", Stderr: err.Error()}
	}
	var b strings.Builder
	matches := 0
	walkErr := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // skip unreadable entries
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if info.IsDir() {
			if skipDir(info.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if glob != "" {
			if ok, _ := filepath.Match(glob, info.Name()); !ok {
				return nil
			}
		}
		if info.Size() > 2*1024*1024 { // skip huge files
			return nil
		}
		f, oerr := os.Open(path)
		if oerr != nil {
			return nil
		}
		defer f.Close()
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		lineNo := 0
		for sc.Scan() {
			lineNo++
			line := sc.Text()
			if re.MatchString(line) {
				fmt.Fprintf(&b, "%s:%d:%s\n", path, lineNo, truncateLine(line, 300))
				matches++
				if matches >= grepMaxResults {
					return fmt.Errorf("max results reached")
				}
			}
		}
		return nil
	})
	if walkErr != nil && !strings.Contains(walkErr.Error(), "max results") && ctx.Err() == nil {
		return Result{Status: StatusFailure, ToolError: walkErr.Error(), Stderr: walkErr.Error()}
	}
	if ctx.Err() != nil {
		return Result{Status: StatusInterrupted, Stderr: "interrupted"}
	}
	if matches == 0 {
		return Result{Status: StatusSuccess, Stdout: "no matches"}
	}
	s, tr := truncate(b.String(), t.e.MaxOutputBytes)
	return Result{Status: StatusSuccess, Stdout: s, Truncated: tr}
}

// skipDir reports whether a directory name should be pruned from searches.
func skipDir(name string) bool {
	switch name {
	case "node_modules", ".git", "vendor", "bin", "obj", "dist", "target", ".venv", "__pycache__":
		return true
	}
	return false
}

func truncateLine(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}
