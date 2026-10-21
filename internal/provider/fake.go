package provider

import (
	"context"
	"encoding/json"
	"fmt"
)

// FakeProvider is a scripted Provider for headless golden-loop tests. It
// replays a fixed sequence of Responses in order and records every request it
// receives so tests can assert on the exact conversation and tool specs sent.
type FakeProvider struct {
	// ProviderName is reported by Name(); defaults to "fake" when empty.
	ProviderName string
	// Responses are replayed in order; the final one repeats once exhausted.
	Responses []*Response
	// Errs, when non-empty and aligned with a Response index, is returned
	// instead of that response (for error-taxonomy tests).
	Errs map[int]error
	// OnDelta, when set, is invoked for each text delta of the replayed
	// response before Complete returns.
	OnDelta func(Event) error

	// Requests records every Complete call's request, in order.
	Requests []Request
	// calls counts Complete invocations.
	calls int
}

// Name implements Provider.
func (f *FakeProvider) Name() string {
	if f.ProviderName == "" {
		return "fake"
	}
	return f.ProviderName
}

// Complete replays the next scripted response.
func (f *FakeProvider) Complete(ctx context.Context, req Request, onDelta func(Event) error) (*Response, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.Requests = append(f.Requests, req)
	idx := f.calls
	f.calls++
	if err, ok := f.Errs[idx]; ok && err != nil {
		return nil, err
	}
	if len(f.Responses) == 0 {
		return nil, fmt.Errorf("fake provider: no scripted responses")
	}
	if idx >= len(f.Responses) {
		idx = len(f.Responses) - 1
	}
	resp := f.Responses[idx]
	if resp == nil {
		return nil, fmt.Errorf("fake provider: nil response at index %d", idx)
	}
	if onDelta != nil {
		// Stream reasoning first (if any), then content, mirroring how real
		// backends emit chain-of-thought ahead of the visible answer.
		for _, r := range []rune(resp.Message.Reasoning) {
			if err := onDelta(Event{Type: EventReasoningDelta, Text: string(r)}); err != nil {
				return nil, err
			}
		}
		if resp.Message.Content != "" {
			for _, r := range []rune(resp.Message.Content) {
				if err := onDelta(Event{Type: EventTextDelta, Text: string(r)}); err != nil {
					return nil, err
				}
			}
		}
		if resp.Message.Reasoning != "" || resp.Message.Content != "" {
			if err := onDelta(Event{Type: EventDone}); err != nil {
				return nil, err
			}
		}
	}
	return resp, nil
}

// ScriptedToolCall is a convenience builder that produces an assistant
// Response invoking the named tool with the given JSON arguments.
func ScriptedToolCall(id, name string, args map[string]any) *Response {
	raw, _ := json.Marshal(args)
	return &Response{
		Message: Message{
			Role:      RoleAssistant,
			ToolCalls: []ToolCall{{ID: id, Name: name, Arguments: string(raw)}},
		},
		StopReason: "tool_calls",
	}
}

// ScriptedText is a convenience builder that produces a plain-text assistant
// Response with no tool calls (terminates a loop turn).
func ScriptedText(content string) *Response {
	return &Response{
		Message:    Message{Role: RoleAssistant, Content: content},
		StopReason: "stop",
	}
}
