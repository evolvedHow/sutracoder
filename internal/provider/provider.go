// Package provider defines the normalized LLM provider interface and wire
// types shared by all adapters (OpenAI-compatible, Anthropic, and the fake
// provider used in tests).
package provider

import (
	"context"
	"errors"
	"fmt"
)

// Message roles understood by the internal conversation model.
const (
	RoleSystem    = "system"
	RoleUser      = "user"
	RoleAssistant = "assistant"
	RoleTool      = "tool"
)

// ToolCall is a single tool invocation requested by the assistant.
type ToolCall struct {
	// ID is the provider-assigned identifier correlating the call with its result.
	ID string `json:"id"`
	// Name is the tool name the assistant chose to invoke.
	Name string `json:"name"`
	// Arguments is the raw JSON argument string produced by the model.
	Arguments string `json:"arguments"`
}

// Message is one turn in the normalized conversation model. Assistant
// messages carry ToolCalls; tool-result messages carry ToolCallID and Name.
type Message struct {
	// Role is one of system, user, assistant, or tool.
	Role string
	// Content is the text body of the message.
	Content string
	// ToolCalls is populated on assistant messages that invoke tools.
	ToolCalls []ToolCall
	// ToolCallID correlates a tool-result message with its invocation.
	ToolCallID string
	// Name is the tool name on tool-result messages.
	Name string
	// Reasoning holds the model's chain-of-thought for this turn when the
	// backend exposes it (e.g. llama.cpp "reasoning_content", Anthropic
	// "thinking" blocks). It is display/persistence-only: it is never sent
	// back to a provider, which regenerates it each turn.
	Reasoning string
}

// ToolSpec describes a callable tool in the JSON-schema form expected by
// OpenAI-style function calling (Anthropic's adapter reshapes it to
// input_schema).
type ToolSpec struct {
	// Name is the tool name.
	Name string
	// Description is the human/model-facing purpose statement.
	Description string
	// JSONSchema is the JSON Schema object for the tool arguments.
	JSONSchema map[string]any
}

// Usage reports token accounting from a completed provider response.
type Usage struct {
	// PromptTokens counts tokens in the submitted prompt.
	PromptTokens int
	// CompletionTokens counts tokens in the generated completion.
	CompletionTokens int
}

// Request is a single completion request handed to a provider.
type Request struct {
	// Messages is the full conversation context for this call.
	Messages []Message
	// Tools lists tools the model may invoke; empty means plain chat.
	Tools []ToolSpec
	// Model overrides the provider's default model when non-empty.
	Model string
	// MaxTokens caps the completion length.
	MaxTokens int
	// Temperature controls sampling randomness (0 = provider default).
	Temperature float64
}

// EventType enumerates streaming events emitted during completion.
type EventType int

const (
	// EventTextDelta carries an incremental fragment of assistant text.
	EventTextDelta EventType = iota
	// EventReasoningDelta carries an incremental fragment of the model's
	// chain-of-thought (reasoning/thinking). Streamed live, display-only.
	EventReasoningDelta
	// EventDone is emitted once when the stream finishes successfully.
	EventDone
)

// Event is a single streaming notification.
type Event struct {
	// Type discriminates the event kind.
	Type EventType
	// Text holds the delta payload for EventTextDelta/EventReasoningDelta.
	Text string
}

// Response is the fully-assembled result of a completion request.
type Response struct {
	// Message is the complete assistant message (text and/or tool calls).
	Message Message
	// Usage reports provider-side token accounting.
	Usage Usage
	// StopReason is the provider's reason for stopping ("stop", "tool_use",
	// "length", ...). Empty when the provider omits it.
	StopReason string
}

// StatusError is a non-2xx HTTP response from a provider endpoint. The loop
// uses Code to classify transient (429/5xx) versus fatal (401/403) failures.
type StatusError struct {
	// StatusCode is the HTTP response status code.
	StatusCode int
	// Body is the (truncated) response body for diagnostics.
	Body string
	// Provider names the adapter that produced the error.
	Provider string
}

// Error implements the error interface with a compact description.
func (e *StatusError) Error() string {
	return fmt.Sprintf("provider %s: HTTP %d: %s", e.Provider, e.StatusCode, e.Body)
}

// IsTransient reports whether the error is worth retrying with backoff
// (rate limits and server-side failures).
func (e *StatusError) IsTransient() bool {
	return e.StatusCode == 429 || e.StatusCode >= 500
}

// IsAuth reports whether the error indicates missing or invalid credentials.
func (e *StatusError) IsAuth() bool {
	return e.StatusCode == 401 || e.StatusCode == 403
}

// ErrStreamAborted is returned by onDelta handlers to stop a stream without
// treating the interruption as an error (used by cancellation paths).
var ErrStreamAborted = errors.New("stream aborted")

// Provider abstracts one LLM backend capable of streaming tool-calling
// completions.
type Provider interface {
	// Name returns the provider's canonical name (e.g. "marga", "openai").
	Name() string
	// Complete performs one chat completion, emitting streaming text deltas
	// to onDelta (which may be nil) and returning the assembled response.
	Complete(ctx context.Context, req Request, onDelta func(Event) error) (*Response, error)
}
