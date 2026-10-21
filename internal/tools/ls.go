package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// lsTool implements ls_recursive(path) — a depth-limited tree view that prunes
// vendor directories.
type lsTool struct{ e *Engine }

func (t *lsTool) Name() string { return "ls_recursive" }
func (t *lsTool) Description() string {
	return "Show a depth-limited directory tree, pruning vendor directories like node_modules and .git."
}
func (t *lsTool) JSONSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path":  map[string]any{"type": "string", "description": "Directory to list; default the workspace root"},
			"depth": map[string]any{"type": "integer", "description": "Max depth to descend; default 4"},
		},
	}
}

const lsMaxEntries = 500

func (t *lsTool) Execute(ctx context.Context, args json.RawMessage) Result {
	var a struct {
		Path  string `json:"path"`
		Depth int    `json:"depth"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return Result{Status: StatusFailure, ToolError: "invalid arguments", Stderr: err.Error()}
	}
	root := t.e.Root
	if a.Path != "" {
		abs, err := t.e.resolvePath(a.Path)
		if err != nil {
			return Result{Status: StatusFailure, ToolError: err.Error(), Stderr: err.Error()}
		}
		root = abs
	}
	if a.Depth <= 0 {
		a.Depth = 4
	}
	var b strings.Builder
	count := 0
	var walk func(dir, prefix string, depth int)
	walk = func(dir, prefix string, depth int) {
		if depth > a.Depth || count >= lsMaxEntries {
			return
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		sort.Slice(entries, func(i, j int) bool {
			if entries[i].IsDir() != entries[j].IsDir() {
				return entries[i].IsDir()
			}
			return entries[i].Name() < entries[j].Name()
		})
		for i, ent := range entries {
			if count >= lsMaxEntries {
				return
			}
			if ctx.Err() != nil {
				return
			}
			if ent.IsDir() && skipDir(ent.Name()) {
				continue
			}
			last := i == len(entries)-1
			branch := "├── "
			nextPrefix := prefix + "│   "
			if last {
				branch = "└── "
				nextPrefix = prefix + "    "
			}
			if ent.IsDir() {
				fmt.Fprintf(&b, "%s%s%s/\n", prefix, branch, ent.Name())
				count++
				walk(filepath.Join(dir, ent.Name()), nextPrefix, depth+1)
			} else {
				fmt.Fprintf(&b, "%s%s%s\n", prefix, branch, ent.Name())
				count++
			}
		}
	}
	walk(root, "", 1)
	if count >= lsMaxEntries {
		fmt.Fprintf(&b, "…[truncated at %d entries]\n", lsMaxEntries)
	}
	s, tr := truncate(b.String(), t.e.MaxOutputBytes)
	return Result{Status: StatusSuccess, Stdout: s, Truncated: tr}
}
