// Package agent implements Sutracoder's headless Plan-Act-Observe-Correct
// loop. It drives a provider through tool-calling turns under hard bounds
// (max turns, token budget), classifies errors into recoverable ToolErrors
// versus loop-stopping FatalErrors, windows context, and emits structured
// interactions to the store.
package agent

import (
	"encoding/json"

	"sutracoder/internal/tools"
)

// State is the agent's high-level status, surfaced in the TUI status bar.
type State string

// Agent states.
const (
	StateIdle      State = "Idle"
	StateThinking  State = "Thinking"
	StateStreaming State = "Streaming"
	StateExecuting State = "Executing"
	StateAwaiting  State = "Awaiting approval"
	StateStopped   State = "Stopped"
	StateCompleted State = "Completed"
	StateLimitHit  State = "Limit reached"
	StateError     State = "Error"
)

// EventKind discriminates events pushed to the sink.
type EventKind int

// Event kinds.
const (
	EventState EventKind = iota
	EventStreamDelta
	// EventReasoningDelta carries an incremental fragment of the model's
	// chain-of-thought, streamed live for display (never fed back to the
	// provider).
	EventReasoningDelta
	EventAssistantText
	EventToolStart
	EventToolEnd
	EventPlanUpdated
	EventApproval
	EventDone
	EventError
	// EventSystem carries feedback lines from slash commands and the runtime
	// (connection status, errors, hints) that should appear in the log.
	EventSystem
)

// Approval carries a pending restricted-tool confirmation from the agent
// goroutine to the UI, with a reply channel the UI writes the decision to.
type Approval struct {
	// Call describes the action awaiting confirmation.
	Call tools.Call
	// Reply receives the user's decision (true = allow). Buffered size 1.
	Reply chan bool
}

// Event is one notification from the loop to the UI (or test harness).
type Event struct {
	// Kind discriminates the event.
	Kind EventKind
	// State is set on EventState.
	State State
	// Text holds streamed deltas and assistant text.
	Text string
	// ToolName and ToolArgs describe the in-flight/complete tool on tool events.
	ToolName string
	ToolArgs json.RawMessage
	// Result is set on EventToolEnd.
	Result tools.Result
	// Plan is set on EventPlanUpdated.
	Plan []tools.PlanItem
	// Err is set on EventError.
	Err error
	// Fatal distinguishes loop-stopping errors from recoverable ones on EventError.
	Fatal bool
	// Approval is set on EventApproval.
	Approval *Approval
}

// Sink receives events from the loop. It must not block indefinitely; the loop
// runs on its own goroutine and the UI consumes from a channel.
type Sink func(Event)

// noopSink drops events (used when no sink is supplied, e.g. pure tests).
func noopSink(Event) {}
