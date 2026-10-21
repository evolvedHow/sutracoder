package provider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// OpenAIConfig configures the OpenAI-compatible chat-completions adapter.
// The same adapter serves marga, api.openai.com, and Ollama's /v1 surface
// purely by varying BaseURL and credentials.
type OpenAIConfig struct {
	// Name is the canonical provider name reported by Name().
	Name string
	// BaseURL is the API root, e.g. "http://127.0.0.1:8010/v1" for marga.
	BaseURL string
	// APIKey is sent as a Bearer token; may be empty for unauthenticated
	// local endpoints such as Ollama.
	APIKey string
	// HTTPClient overrides the default HTTP client (tests inject fakes).
	HTTPClient *http.Client
}

// OpenAIProvider implements Provider against an OpenAI-compatible
// /chat/completions endpoint with streaming enabled.
type OpenAIProvider struct {
	cfg OpenAIConfig
}

// NewOpenAI constructs an OpenAI-compatible provider.
func NewOpenAI(cfg OpenAIConfig) *OpenAIProvider {
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 0}
	}
	return &OpenAIProvider{cfg: cfg}
}

// Name returns the provider's canonical name.
func (p *OpenAIProvider) Name() string { return p.cfg.Name }

// openAIRequest is the wire format for POST {base}/chat/completions.
type openAIRequest struct {
	Model       string          `json:"model"`
	Messages    []openAIMessage `json:"messages"`
	Tools       []openAITool    `json:"tools,omitempty"`
	MaxTokens   int             `json:"max_tokens,omitempty"`
	Temperature *float64        `json:"temperature,omitempty"`
	Stream      bool            `json:"stream"`
	// StreamOptions asks OpenAI-compatible backends to include usage in the
	// final stream chunk. Backends that reject the field still work; usage
	// then stays zero.
	StreamOptions *openAIStreamOptions `json:"stream_options,omitempty"`
}

type openAIStreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type openAIMessage struct {
	Role       string        `json:"role"`
	Content    string        `json:"content"`
	ToolCalls  []openAIToolC `json:"tool_calls,omitempty"`
	ToolCallID string        `json:"tool_call_id,omitempty"`
}

type openAIToolC struct {
	ID   string `json:"id"`
	Type string `json:"type"`
	// Index appears only in streaming deltas, never in requests; omitempty
	// keeps it off the wire when serializing tool_calls back to the API.
	Index    int `json:"index,omitempty"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type openAITool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string         `json:"name"`
		Description string         `json:"description"`
		Parameters  map[string]any `json:"parameters"`
	} `json:"function"`
}

// openAIStreamChunk is one SSE payload from the streaming endpoint.
type openAIStreamChunk struct {
	ID      string `json:"id"`
	Choices []struct {
		Index        int    `json:"index"`
		Delta        any    `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

// toWire converts normalized messages to the OpenAI wire format.
func toWire(msgs []Message) []openAIMessage {
	out := make([]openAIMessage, 0, len(msgs))
	for _, m := range msgs {
		wm := openAIMessage{Role: m.Role, Content: m.Content, ToolCallID: m.ToolCallID}
		for _, tc := range m.ToolCalls {
			oc := openAIToolC{ID: tc.ID, Type: "function"}
			oc.Function.Name = tc.Name
			oc.Function.Arguments = tc.Arguments
			wm.ToolCalls = append(wm.ToolCalls, oc)
		}
		out = append(out, wm)
	}
	return out
}

// wireTools converts normalized tool specs to the OpenAI wire format.
func wireTools(specs []ToolSpec) []openAITool {
	out := make([]openAITool, 0, len(specs))
	for _, s := range specs {
		t := openAITool{Type: "function"}
		t.Function.Name = s.Name
		t.Function.Description = s.Description
		t.Function.Parameters = s.JSONSchema
		out = append(out, t)
	}
	return out
}

// streamToolAssembler incrementally rebuilds tool calls from indexed deltas.
type streamToolAssembler struct {
	calls map[int]*openAIToolC
	order []int
}

func newToolAssembler() *streamToolAssembler {
	return &streamToolAssembler{calls: map[int]*openAIToolC{}}
}

func (a *streamToolAssembler) add(d openAIToolC) {
	c, ok := a.calls[d.Index]
	if !ok {
		nc := openAIToolC{ID: d.ID, Type: "function"}
		c = &nc
		a.calls[d.Index] = c
		a.order = append(a.order, d.Index)
	}
	if d.ID != "" {
		c.ID = d.ID
	}
	if d.Function.Name != "" {
		c.Function.Name += d.Function.Name
	}
	c.Function.Arguments += d.Function.Arguments
}

func (a *streamToolAssembler) toolCalls() []ToolCall {
	if len(a.order) == 0 {
		return nil
	}
	out := make([]ToolCall, 0, len(a.order))
	for _, idx := range a.order {
		c := a.calls[idx]
		out = append(out, ToolCall{ID: c.ID, Name: c.Function.Name, Arguments: c.Function.Arguments})
	}
	return out
}

// Complete performs one streaming chat completion against the configured
// OpenAI-compatible endpoint.
func (p *OpenAIProvider) Complete(ctx context.Context, req Request, onDelta func(Event) error) (*Response, error) {
	body := openAIRequest{
		Model:     req.Model,
		Messages:  toWire(req.Messages),
		Tools:     wireTools(req.Tools),
		MaxTokens: req.MaxTokens,
		Stream:    true,
	}
	if body.Model == "" {
		body.Model = defaultModel(p.cfg.Name)
	}
	if req.MaxTokens == 0 {
		body.MaxTokens = 4096
	}
	if req.Temperature > 0 {
		t := req.Temperature
		body.Temperature = &t
	}
	body.StreamOptions = &openAIStreamOptions{IncludeUsage: true}

	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}
	url := strings.TrimSuffix(p.cfg.BaseURL, "/") + "/chat/completions"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	if p.cfg.APIKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+p.cfg.APIKey)
	}

	resp, err := p.cfg.HTTPClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("provider %s: %w", p.cfg.Name, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, &StatusError{StatusCode: resp.StatusCode, Body: string(b), Provider: p.cfg.Name}
	}

	var (
		assembler  = newToolAssembler()
		text       strings.Builder
		reasoning  strings.Builder
		usage      Usage
		finish     string
		sawSuccess bool
	)
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, ":") {
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			break
		}
		var chunk openAIStreamChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue
		}
		if chunk.Error != nil {
			return nil, fmt.Errorf("provider %s stream error: %s", p.cfg.Name, chunk.Error.Message)
		}
		if chunk.Usage != nil {
			usage = Usage{PromptTokens: chunk.Usage.PromptTokens, CompletionTokens: chunk.Usage.CompletionTokens}
		}
		for _, c := range chunk.Choices {
			if c.FinishReason != "" {
				finish = c.FinishReason
			}
			if c.Delta == nil {
				continue
			}
			deltaMap, ok := c.Delta.(map[string]any)
			if !ok {
				continue
			}
			if content, ok := deltaMap["content"].(string); ok && content != "" {
				text.WriteString(content)
				if onDelta != nil {
					if err := onDelta(Event{Type: EventTextDelta, Text: content}); err != nil {
						if errors.Is(err, ErrStreamAborted) {
							break
						}
						return nil, err
					}
				}
			}
			// Reasoning models (DeepSeek-R1 and friends, served by llama.cpp)
			// emit their chain-of-thought under reasoning_content; some
			// backends use reasoning. Capture and stream both, display-only.
			for _, key := range [...]string{"reasoning_content", "reasoning"} {
				if rc, ok := deltaMap[key].(string); ok && rc != "" {
					reasoning.WriteString(rc)
					if onDelta != nil {
						if err := onDelta(Event{Type: EventReasoningDelta, Text: rc}); err != nil {
							if errors.Is(err, ErrStreamAborted) {
								break
							}
							return nil, err
						}
					}
				}
			}
			if rawTC, ok := deltaMap["tool_calls"].([]any); ok {
				for _, item := range rawTC {
					m, ok := item.(map[string]any)
					if !ok {
						continue
					}
					var d openAIToolC
					if b, err := json.Marshal(m); err == nil {
						_ = json.Unmarshal(b, &d)
					}
					if idx, ok := m["index"].(float64); ok {
						d.Index = int(idx)
					}
					assembler.add(d)
				}
			}
		}
		sawSuccess = true
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("provider %s stream read: %w", p.cfg.Name, err)
	}
	if !sawSuccess && finish == "" {
		return nil, fmt.Errorf("provider %s: empty stream response", p.cfg.Name)
	}

	msg := Message{Role: RoleAssistant, Content: text.String(), Reasoning: reasoning.String(), ToolCalls: assembler.toolCalls()}
	stop := finish
	if stop == "" && len(msg.ToolCalls) > 0 {
		stop = "tool_calls"
	}
	if onDelta != nil {
		_ = onDelta(Event{Type: EventDone})
	}
	return &Response{Message: msg, Usage: usage, StopReason: stop}, nil
}

// defaultModel returns a sensible default model name per provider.
func defaultModel(name string) string {
	switch name {
	case "marga":
		return "default"
	case "llamacpp":
		// llama.cpp serves whatever model it was loaded with and ignores the
		// request's model field; send a neutral placeholder.
		return "local"
	case "ollama":
		return "llama3.1"
	default:
		return "gpt-4o-mini"
	}
}
