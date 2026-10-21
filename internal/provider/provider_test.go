package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOpenAIStreamParsesTextAndUsage(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"choices":[{"index":0,"delta":{"content":"Hello"}}]}`,
		``,
		`data: {"choices":[{"index":0,"delta":{"content":" world"}}]}`,
		``,
		`data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(sse))
	}))
	defer srv.Close()

	p := NewOpenAI(OpenAIConfig{Name: "test", BaseURL: srv.URL})
	var deltas []string
	resp, err := p.Complete(context.Background(), Request{Messages: []Message{{Role: RoleUser, Content: "hi"}}}, func(ev Event) error {
		if ev.Type == EventTextDelta {
			deltas = append(deltas, ev.Text)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Message.Content != "Hello world" {
		t.Fatalf("content = %q", resp.Message.Content)
	}
	if resp.Usage.PromptTokens != 10 || resp.Usage.CompletionTokens != 5 {
		t.Fatalf("usage = %+v", resp.Usage)
	}
	if strings.Join(deltas, "") != "Hello world" {
		t.Fatalf("deltas = %v", deltas)
	}
}

func TestOpenAIStreamAssemblesToolCalls(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"read_file","arguments":""}}]}}]}`,
		``,
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"path\":\"a.txt\"}"}}]}}]}`,
		``,
		`data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(sse))
	}))
	defer srv.Close()

	p := NewOpenAI(OpenAIConfig{Name: "test", BaseURL: srv.URL})
	resp, err := p.Complete(context.Background(), Request{}, nil)
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if len(resp.Message.ToolCalls) != 1 {
		t.Fatalf("tool calls = %+v", resp.Message.ToolCalls)
	}
	tc := resp.Message.ToolCalls[0]
	if tc.ID != "call_1" || tc.Name != "read_file" || tc.Arguments != `{"path":"a.txt"}` {
		t.Fatalf("assembled tool call = %+v", tc)
	}
	if resp.StopReason != "tool_calls" {
		t.Fatalf("stop = %q", resp.StopReason)
	}
}

func TestStatusErrorClassification(t *testing.T) {
	mk := func(code int) *StatusError { return &StatusError{StatusCode: code} }
	if !mk(429).IsTransient() || !mk(500).IsTransient() || !mk(503).IsTransient() {
		t.Fatal("429/5xx should be transient")
	}
	if mk(400).IsTransient() {
		t.Fatal("400 should not be transient")
	}
	if !mk(401).IsAuth() || !mk(403).IsAuth() {
		t.Fatal("401/403 should be auth")
	}
	if mk(500).IsAuth() {
		t.Fatal("500 should not be auth")
	}
}

func TestOpenAIErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"message":"bad key"}}`, http.StatusUnauthorized)
	}))
	defer srv.Close()
	p := NewOpenAI(OpenAIConfig{Name: "test", BaseURL: srv.URL})
	_, err := p.Complete(context.Background(), Request{}, nil)
	st, ok := err.(*StatusError)
	if !ok || st.StatusCode != 401 {
		t.Fatalf("expected 401 StatusError, got %v", err)
	}
	if !st.IsAuth() {
		t.Fatal("expected IsAuth")
	}
}

func TestOpenAIStreamParsesReasoningContent(t *testing.T) {
	// Reasoning models (DeepSeek-R1 via llama.cpp) emit chain-of-thought under
	// reasoning_content ahead of the visible answer.
	sse := strings.Join([]string{
		`data: {"choices":[{"index":0,"delta":{"reasoning_content":"let me think"}}]}`,
		``,
		`data: {"choices":[{"index":0,"delta":{"reasoning_content":" step by step"}}]}`,
		``,
		`data: {"choices":[{"index":0,"delta":{"content":"42"}}]}`,
		``,
		`data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(sse))
	}))
	defer srv.Close()

	p := NewOpenAI(OpenAIConfig{Name: "test", BaseURL: srv.URL})
	var reasoning, content []string
	resp, err := p.Complete(context.Background(), Request{}, func(ev Event) error {
		switch ev.Type {
		case EventReasoningDelta:
			reasoning = append(reasoning, ev.Text)
		case EventTextDelta:
			content = append(content, ev.Text)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Message.Reasoning != "let me think step by step" {
		t.Fatalf("reasoning = %q", resp.Message.Reasoning)
	}
	if resp.Message.Content != "42" {
		t.Fatalf("content = %q", resp.Message.Content)
	}
	if strings.Join(reasoning, "") != "let me think step by step" {
		t.Fatalf("reasoning deltas = %v", reasoning)
	}
	if strings.Join(content, "") != "42" {
		t.Fatalf("content deltas = %v", content)
	}
}

func TestAnthropicStreamParsesThinking(t *testing.T) {
	// Extended-thinking models emit a "thinking" content block whose deltas
	// carry the chain-of-thought under the "thinking" key.
	sse := strings.Join([]string{
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking"}}`,
		``,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"hmm"}}`,
		``,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":" indeed"}}`,
		``,
		`data: {"type":"content_block_stop","index":0}`,
		``,
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"text"}}`,
		``,
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"answer"}}`,
		``,
		`data: {"type":"content_block_stop","index":1}`,
		``,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"}}`,
		``,
	}, "\n")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(sse))
	}))
	defer srv.Close()

	p := NewAnthropic(AnthropicConfig{APIKey: "k", BaseURL: srv.URL})
	var reasoning []string
	resp, err := p.Complete(context.Background(), Request{}, func(ev Event) error {
		if ev.Type == EventReasoningDelta {
			reasoning = append(reasoning, ev.Text)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Message.Reasoning != "hmm indeed" {
		t.Fatalf("reasoning = %q", resp.Message.Reasoning)
	}
	if resp.Message.Content != "answer" {
		t.Fatalf("content = %q", resp.Message.Content)
	}
	if strings.Join(reasoning, "") != "hmm indeed" {
		t.Fatalf("reasoning deltas = %v", reasoning)
	}
}
