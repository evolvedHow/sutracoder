// Package tools implements Sutracoder's tool engine — the "hands" the LLM
// drives to explore, edit, and execute within a workspace. Every tool returns
// a structured Result carrying the SUCCESS/FAILURE status and stdout/stderr/
// returncode the downstream self-improvement engine ingests (see spec §3E).
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Tool execution status values (the interaction-emission contract).
const (
	StatusSuccess     = "SUCCESS"
	StatusFailure     = "FAILURE"
	StatusInterrupted = "INTERRUPTED"
)

// PlanItem is one entry in the agent's live checklist.
type PlanItem struct {
	// Text is the human-readable step description.
	Text string `json:"text"`
	// Status is one of pending, in_progress, done, failed.
	Status string `json:"status"`
}

// Result is the structured outcome of a single tool execution. It doubles as
// the observation fed back to the LLM and the record appended to the JSONL
// interaction log.
type Result struct {
	// Status is SUCCESS, FAILURE, or INTERRUPTED.
	Status string
	// Stdout is the tool's standard output (truncated per MaxOutputBytes).
	Stdout string
	// Stderr carries error detail on failure, or the truncation marker.
	Stderr string
	// ReturnCode is set for process-executing tools (execute_shell).
	ReturnCode *int
	// DurationMS is wall-clock execution time.
	DurationMS int64
	// Truncated reports whether output was clipped by the size cap.
	Truncated bool
	// Tool is the tool name (filled by the Engine).
	Tool string
	// Args is the raw JSON arguments (filled by the Engine).
	Args json.RawMessage
	// ToolError is the recoverable error text on FAILURE (empty on success).
	ToolError string
}

// Observation renders the Result as the text block fed back to the LLM. It
// always leads with the status so the model can self-correct on failure.
func (r Result) Observation() string {
	var b strings.Builder
	fmt.Fprintf(&b, "status: %s", r.Status)
	if r.ReturnCode != nil {
		fmt.Fprintf(&b, " (exit %d)", *r.ReturnCode)
	}
	b.WriteString("\n")
	if r.Stdout != "" {
		b.WriteString(r.Stdout)
		if !strings.HasSuffix(r.Stdout, "\n") {
			b.WriteString("\n")
		}
	}
	if r.Stderr != "" {
		b.WriteString("stderr:\n")
		b.WriteString(r.Stderr)
		if !strings.HasSuffix(r.Stderr, "\n") {
			b.WriteString("\n")
		}
	}
	if r.Truncated {
		b.WriteString("[output truncated]\n")
	}
	return b.String()
}

// Tool is a single callable capability exposed to the LLM.
type Tool interface {
	// Name is the function-calling name (e.g. "read_file").
	Name() string
	// Description is the model-facing purpose statement.
	Description() string
	// JSONSchema returns the argument schema for function calling.
	JSONSchema() map[string]any
	// Execute runs the tool against the given raw JSON arguments.
	Execute(ctx context.Context, args json.RawMessage) Result
}

// Approver is consulted before restricted tools run. It returns true to allow
// the call. A nil Approver allows everything (used in headless tests).
type Approver func(call Call) bool

// Call describes a pending tool invocation for approval prompts.
type Call struct {
	// Tool is the tool name.
	Tool string
	// Args is the raw JSON arguments.
	Args json.RawMessage
	// Summary is a one-line human-readable description for the prompt.
	Summary string
	// Restricted marks denylist/supervised hits that always need confirmation.
	Restricted bool
}

// Engine holds tool configuration (workspace root, limits) and the shared,
// thread-safe mutable state (plan, completion) that update_plan and
// complete_goal mutate.
type Engine struct {
	// Root is the workspace root; all path arguments are confined here.
	Root string
	// MaxOutputBytes caps every tool's stdout+stderr.
	MaxOutputBytes int
	// ShellTimeout bounds execute_shell.
	ShellTimeout time.Duration
	// Supervised forces approval for shell/edit/write even in autonomous use.
	Supervised bool
	// Approver gates restricted calls; nil allows all.
	Approver Approver

	mu        sync.Mutex
	plan      []PlanItem
	completed bool
	summary   string
	tools     map[string]Tool
	order     []string
}

// NewEngine builds the Engine with the full default toolset registered and
// returns it. Root may be empty (tools resolve against the process CWD).
func NewEngine(root string) *Engine {
	if root == "" {
		root, _ = filepath.Abs(".")
	} else {
		root, _ = filepath.Abs(root)
	}
	e := &Engine{
		Root:           root,
		MaxOutputBytes: 10 * 1024,
		ShellTimeout:   120 * time.Second,
		tools:          map[string]Tool{},
	}
	e.register(&lsTool{e})
	e.register(&readTool{e})
	e.register(&writeTool{e})
	e.register(&editTool{e})
	e.register(&grepTool{e})
	e.register(&shellTool{e})
	e.register(&planTool{e})
	e.register(&completeTool{e})
	return e
}

func (e *Engine) register(t Tool) {
	if _, dup := e.tools[t.Name()]; !dup {
		e.order = append(e.order, t.Name())
	}
	e.tools[t.Name()] = t
}

// Tool returns the named tool, or nil.
func (e *Engine) Tool(name string) Tool { return e.tools[name] }

// Names returns tool names in registration order.
func (e *Engine) Names() []string {
	out := make([]string, len(e.order))
	copy(out, e.order)
	return out
}

// Run looks up the named tool and executes it, filling Result metadata and
// enforcing the approval gate for restricted calls. A missing tool yields a
// FAILURE result (recoverable — the LLM is told the tool does not exist).
func (e *Engine) Run(ctx context.Context, name string, args json.RawMessage) Result {
	t := e.tools[name]
	if t == nil {
		return Result{
			Status:    StatusFailure,
			Tool:      name,
			Args:      args,
			ToolError: fmt.Sprintf("unknown tool %q", name),
			Stderr:    fmt.Sprintf("unknown tool %q; available: %s", name, strings.Join(e.Names(), ", ")),
		}
	}
	if e.needsApproval(name, args) {
		if e.Approver == nil || !e.Approver(Call{Tool: name, Args: args, Summary: summarizeCall(name, args), Restricted: true}) {
			return Result{
				Status:    StatusFailure,
				Tool:      name,
				Args:      args,
				ToolError: "user declined execution",
				Stderr:    "execution declined by user",
			}
		}
	}
	start := time.Now()
	res := t.Execute(ctx, args)
	res.DurationMS = time.Since(start).Milliseconds()
	res.Tool = name
	res.Args = args
	if res.Status == StatusInterrupted && res.Stderr == "" {
		res.Stderr = "interrupted by user"
	}
	return res
}

// needsApproval reports whether a call must be gated. Shell is always gated in
// supervised mode or on a denylist hit; edit/write are gated in supervised
// mode.
func (e *Engine) needsApproval(name string, args json.RawMessage) bool {
	switch name {
	case "execute_shell":
		if e.Supervised {
			return true
		}
		var a struct {
			Command string `json:"command"`
		}
		_ = json.Unmarshal(args, &a)
		return DenylistHit(a.Command)
	case "edit_file", "write_file":
		return e.Supervised
	default:
		return false
	}
}

// Plan returns a copy of the current checklist.
func (e *Engine) Plan() []PlanItem {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]PlanItem, len(e.plan))
	copy(out, e.plan)
	return out
}

// SetPlan replaces the checklist (invoked by update_plan).
func (e *Engine) SetPlan(items []PlanItem) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.plan = items
}

// Completed reports whether complete_goal has fired.
func (e *Engine) Completed() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.completed
}

// CompletionSummary returns the goal summary from complete_goal.
func (e *Engine) CompletionSummary() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.summary
}

// summarizeCall renders a one-line human-readable description of a tool
// invocation for the approval prompt.
func summarizeCall(name string, args json.RawMessage) string {
	switch name {
	case "execute_shell":
		var a struct {
			Command string `json:"command"`
		}
		_ = json.Unmarshal(args, &a)
		return fmt.Sprintf("run shell: %s", a.Command)
	case "edit_file":
		var a struct {
			Path string `json:"path"`
		}
		_ = json.Unmarshal(args, &a)
		return fmt.Sprintf("edit file: %s", a.Path)
	case "write_file":
		var a struct {
			Path string `json:"path"`
		}
		_ = json.Unmarshal(args, &a)
		return fmt.Sprintf("write file: %s", a.Path)
	default:
		if len(args) > 120 {
			return fmt.Sprintf("%s %s…", name, string(args[:120]))
		}
		return fmt.Sprintf("%s %s", name, string(args))
	}
}

// truncate clips s to maxBytes, returning the clipped text and whether the
// head+tail marker was applied. It keeps a head and tail so paging agents
// retain orientation.
func truncate(s string, maxBytes int) (string, bool) {
	if maxBytes <= 0 || len(s) <= maxBytes {
		return s, false
	}
	head := maxBytes * 2 / 3
	tail := maxBytes - head
	var b strings.Builder
	b.WriteString(s[:head])
	fmt.Fprintf(&b, "\n…[truncated %d bytes]…\n", len(s)-head-tail)
	b.WriteString(s[len(s)-tail:])
	return b.String(), true
}

// resolvePath confines p to the workspace root: relative paths join the root,
// and any escape (via .. or an absolute path outside) is rejected. It returns
// the absolute confined path and an error when the path is out of bounds.
func (e *Engine) resolvePath(p string) (string, error) {
	if strings.TrimSpace(p) == "" {
		return "", fmt.Errorf("empty path")
	}
	var abs string
	if filepath.IsAbs(p) {
		abs = filepath.Clean(p)
	} else {
		abs = filepath.Join(e.Root, filepath.Clean(p))
	}
	rel, err := filepath.Rel(e.Root, abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q escapes workspace root %s", p, e.Root)
	}
	return abs, nil
}
