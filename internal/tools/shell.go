package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// denylistPatterns are command substrings/regexes that always require human
// confirmation, even in autonomous mode. Matched case-sensitively against the
// whole command line.
var denylistPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(^|[;&|]\s*)sudo\b`),
	regexp.MustCompile(`\brm\s+(-[a-zA-Z]*[rf][a-zA-Z]*\s+)*(/|~|\$HOME)\b`),
	regexp.MustCompile(`\brm\s+-[a-zA-Z]*r[a-zA-Z]*f|\brm\s+-[a-zA-Z]*f[a-zA-Z]*r`),
	regexp.MustCompile(`\bmkfs(\.\w+)?\b`),
	regexp.MustCompile(`\bgit\s+push\s+.*(--force|-f)\b`),
	regexp.MustCompile(`\b(shutdown|reboot|poweroff|halt)\b`),
	regexp.MustCompile(`\bdd\s+.*of=/dev/`),
	regexp.MustCompile(`:\(\)\s*\{.*\};:`), // fork bomb
	regexp.MustCompile(`\bchmod\s+(-R\s+)?777\s+/`),
	regexp.MustCompile(`>\s*/dev/(sd|nvme|hd)`),
}

// DenylistHit reports whether a shell command matches a dangerous pattern and
// therefore needs explicit confirmation.
func DenylistHit(command string) bool {
	for _, re := range denylistPatterns {
		if re.MatchString(command) {
			return true
		}
	}
	return false
}

// shellTool implements execute_shell(command, timeout_sec?) — runs a command
// in the workspace root under a bounded, cancellable context.
type shellTool struct{ e *Engine }

func (t *shellTool) Name() string { return "execute_shell" }
func (t *shellTool) Description() string {
	return "Run a shell command in the workspace root and return its combined output. Bounded by a timeout."
}
func (t *shellTool) JSONSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"command":     map[string]any{"type": "string", "description": "The shell command to run"},
			"timeout_sec": map[string]any{"type": "integer", "description": "Optional timeout in seconds; default is the engine shell timeout"},
		},
		"required": []string{"command"},
	}
}

func (t *shellTool) Execute(ctx context.Context, args json.RawMessage) Result {
	var a struct {
		Command    string `json:"command"`
		TimeoutSec int    `json:"timeout_sec"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return Result{Status: StatusFailure, ToolError: "invalid arguments", Stderr: err.Error()}
	}
	if strings.TrimSpace(a.Command) == "" {
		return Result{Status: StatusFailure, ToolError: "empty command", Stderr: "command must not be empty"}
	}
	timeout := t.e.ShellTimeout
	if a.TimeoutSec > 0 {
		timeout = time.Duration(a.TimeoutSec) * time.Second
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(cctx, "sh", "-c", a.Command)
	cmd.Dir = t.e.Root
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	runErr := cmd.Run()

	stdout := out.String()
	rc := 0
	if runErr != nil {
		if ee, isExit := runErr.(*exec.ExitError); isExit {
			rc = ee.ExitCode()
		} else {
			// spawn-level failure (e.g. context deadline)
			rc = -1
		}
	}
	if cctx.Err() == context.DeadlineExceeded {
		s, tr := truncate(stdout, t.e.MaxOutputBytes)
		return Result{Status: StatusFailure, Stdout: s, Truncated: tr, ReturnCode: &rc,
			ToolError: fmt.Sprintf("command timed out after %s", timeout), Stderr: "command timed out"}
	}
	if ctx.Err() != nil {
		rc = -1
		s, tr := truncate(stdout, t.e.MaxOutputBytes)
		return Result{Status: StatusInterrupted, Stdout: s, Truncated: tr, ReturnCode: &rc}
	}
	s, tr := truncate(stdout, t.e.MaxOutputBytes)
	if runErr != nil {
		return Result{Status: StatusFailure, Stdout: s, Truncated: tr, ReturnCode: &rc,
			ToolError: fmt.Sprintf("exit code %d", rc)}
	}
	return Result{Status: StatusSuccess, Stdout: s, Truncated: tr, ReturnCode: &rc}
}
