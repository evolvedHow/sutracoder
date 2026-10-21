package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// validPlanStatuses enumerates allowed checklist item states.
var validPlanStatuses = map[string]bool{
	"pending": true, "in_progress": true, "done": true, "failed": true,
}

// planTool implements update_plan(items) — replaces the agent's live
// checklist, the sole data source for the TUI's Plan panel.
type planTool struct{ e *Engine }

func (t *planTool) Name() string { return "update_plan" }
func (t *planTool) Description() string {
	return "Replace the agent's task checklist. Each item has text and a status of pending, in_progress, done, or failed."
}
func (t *planTool) JSONSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"items": map[string]any{
				"type":        "array",
				"description": "The full replacement checklist",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"text":   map[string]any{"type": "string"},
						"status": map[string]any{"type": "string", "enum": []string{"pending", "in_progress", "done", "failed"}},
					},
					"required": []string{"text", "status"},
				},
			},
		},
		"required": []string{"items"},
	}
}

func (t *planTool) Execute(ctx context.Context, args json.RawMessage) Result {
	var a struct {
		Items []PlanItem `json:"items"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return Result{Status: StatusFailure, ToolError: "invalid arguments", Stderr: err.Error()}
	}
	for i, it := range a.Items {
		if strings.TrimSpace(it.Text) == "" {
			return Result{Status: StatusFailure, ToolError: "empty item text",
				Stderr: fmt.Sprintf("item %d has empty text", i)}
		}
		if !validPlanStatuses[it.Status] {
			return Result{Status: StatusFailure, ToolError: "invalid status",
				Stderr: fmt.Sprintf("item %d has invalid status %q; use pending, in_progress, done, or failed", i, it.Status)}
		}
	}
	t.e.SetPlan(a.Items)
	var b strings.Builder
	fmt.Fprintf(&b, "plan updated: %d items\n", len(a.Items))
	for i, it := range a.Items {
		fmt.Fprintf(&b, "  %d. [%s] %s\n", i+1, it.Status, it.Text)
	}
	return Result{Status: StatusSuccess, Stdout: b.String()}
}

// completeTool implements complete_goal(summary) — ends the loop and records
// the verification summary.
type completeTool struct{ e *Engine }

func (t *completeTool) Name() string { return "complete_goal" }
func (t *completeTool) Description() string {
	return "Declare the goal achieved and verified. Ends the agent loop. Provide a short summary of what was done."
}
func (t *completeTool) JSONSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"summary": map[string]any{"type": "string", "description": "What was accomplished and how it was verified"},
		},
		"required": []string{"summary"},
	}
}

func (t *completeTool) Execute(ctx context.Context, args json.RawMessage) Result {
	var a struct {
		Summary string `json:"summary"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return Result{Status: StatusFailure, ToolError: "invalid arguments", Stderr: err.Error()}
	}
	if strings.TrimSpace(a.Summary) == "" {
		return Result{Status: StatusFailure, ToolError: "empty summary", Stderr: "summary must not be empty"}
	}
	t.e.mu.Lock()
	t.e.completed = true
	t.e.summary = a.Summary
	t.e.mu.Unlock()
	return Result{Status: StatusSuccess, Stdout: "goal marked complete; loop will stop"}
}
