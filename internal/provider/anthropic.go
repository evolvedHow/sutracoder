package provider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// AnthropicConfig configures the Anthropic Messages API adapter.
type AnthropicConfig struct {
	// APIKey is sent as the x-api-key header.
	APIKey string
	// BaseURL defaults to https://api.anthropic.com; overridable for proxies.
	BaseURL string
	// Model is the default model when a request does not override it.
	Model string
	// APIVersion pins the anthropic-version header.
	APIVersion string
	// HTTPClient overrides the default HTTP client (tests inject fakes).
	HTTPClient *http.Client
}

// AnthropicProvider implements Provider against the Anthropic Messages API.
type AnthropicProvider struct {
	cfg AnthropicConfig
}

// NewAnthropic constructs an Anthropic provider with sane defaults.
func NewAnthropic(cfg AnthropicConfig) *AnthropicProvider {
	if cfg.BaseURL == "" {
		cfg.BaseURL = "https://api.anthropic.com"
	}
	if cfg.Model == "" {
		cfg.Model = "claude-sonnet-4-20250514"
	}
	if cfg.APIVersion == "" {
		cfg.APIVersion = "2023-06-01"
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{}
	}
	return &AnthropicProvider{cfg: cfg}
}

// Name returns the provider's canonical name.
func (p *AnthropicProvider) Name() string { return "anthropic" }

// anthropicRequest is the wire format for POST /v1/messages.
type anthropicRequest struct {
	Model         string             `json:"model"`
	System        string             `json:"system,omitempty"`
	Messages      []anthropicMessage `json:"messages"`
	Tools         []anthropicTool    `json:"tools,omitempty"`
	MaxTokens     int                `json:"max_tokens"`
	Temperature   *float64           `json:"temperature,omitempty"`
	Stream        bool               `json:"stream"`
	AnthropicVer  string             `json:"-"`
	StopSequences []string           `json:"stop_sequences,omitempty"`
}

type anthropicMessage struct {
	Role    string           `json:"role"`
	Content []anthropicBlock `json:"content"`
}

// anthropicBlock is a typed content block (text, tool_use, tool_result).
type anthropicBlock struct {
	Type  string          `json:"type"`
	Text  string          `json:"text,omitempty"`
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
	// ToolUseID correlates a tool_result block with its tool_use.
	ToolUseID string `json:"tool_use_id,omitempty"`
	Content   string `json:"content,omitempty"`
}

type anthropicTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	InputSchema map[string]any `json:"input_schema"`
}

// toAnthropic converts the normalized history into Anthropic's block-based
// message list, collapsing consecutive same-role turns as the API requires.
func toAnthropic(msgs []Message) (system string, out []anthropicMessage) {
	for _, m := range msgs {
		switch m.Role {
		case RoleSystem:
			if system == "" {
				system = m.Content
			} else {
				system += "\n\n" + m.Content
			}
		case RoleUser:
			out = append(out, anthropicMessage{Role: "user", Content: []anthropicBlock{{Type: "text", Text: m.Content}}})
		case RoleAssistant:
			var blocks []anthropicBlock
			if m.Content != "" {
				blocks = append(blocks, anthropicBlock{Type: "text", Text: m.Content})
			}
			for _, tc := range m.ToolCalls {
				input := json.RawMessage(tc.Arguments)
				if len(input) == 0 {
					input = json.RawMessage(`{}`)
				}
				blocks = append(blocks, anthropicBlock{Type: "tool_use", ID: tc.ID, Name: tc.Name, Input: input})
			}
			if len(blocks) == 0 {
				blocks = append(blocks, anthropicBlock{Type: "text", Text: ""})
			}
			out = append(out, anthropicMessage{Role: "assistant", Content: blocks})
		case RoleTool:
			block := anthropicBlock{Type: "tool_result", ToolUseID: m.ToolCallID, Content: m.Content}
			out = append(out, anthropicMessage{Role: "user", Content: []anthropicBlock{block}})
		}
	}
	return system, out
}

// anthropicStreamEvent is one SSE event from the streaming Messages API.
type anthropicStreamEvent struct {
	Type  string          `json:"type"`
	Index int             `json:"index"`
	Delta json.RawMessage `json:"delta"`
	Usage *struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
	ContentBlock *anthropicBlock `json:"content_block"`
}

func (p *AnthropicProvider) Complete(ctx context.Context, req Request, onDelta func(Event) error) (*Response, error) {
	system, wire := toAnthropic(req.Messages)
	model := req.Model
	if model == "" {
		model = p.cfg.Model
	}
	maxTokens := req.MaxTokens
	if maxTokens == 0 {
		maxTokens = 4096
	}
	body := anthropicRequest{
		Model:        model,
		System:       system,
		Messages:     wire,
		MaxTokens:    maxTokens,
		Stream:       true,
		AnthropicVer: p.cfg.APIVersion,
	}
	if req.Temperature > 0 {
		t := req.Temperature
		body.Temperature = &t
	}
	for _, s := range req.Tools {
		body.Tools = append(body.Tools, anthropicTool{Name: s.Name, Description: s.Description, InputSchema: s.JSONSchema})
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal anthropic request: %w", err)
	}
	url := strings.TrimSuffix(p.cfg.BaseURL, "/") + "/v1/messages"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("build anthropic request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	httpReq.Header.Set("x-api-key", p.cfg.APIKey)
	httpReq.Header.Set("anthropic-version", p.cfg.APIVersion)

	resp, err := p.cfg.HTTPClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("provider anthropic: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, &StatusError{StatusCode: resp.StatusCode, Body: string(b), Provider: "anthropic"}
	}

	var (
		text       strings.Builder
		reasoning  strings.Builder
		toolCalls  []ToolCall
		usage      Usage
		finish     string
		curType    string
		curID      string
		curName    string
		curArgs    strings.Builder
		sawSuccess bool
	)
	flushTool := func() {
		if curType == "tool_use" && curName != "" {
			toolCalls = append(toolCalls, ToolCall{ID: curID, Name: curName, Arguments: curArgs.String()})
		}
		curType, curID, curName = "", "", ""
		curArgs.Reset()
	}
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		var ev anthropicStreamEvent
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			continue
		}
		switch ev.Type {
		case "message_start":
			if ev.Usage != nil {
				usage.PromptTokens = ev.Usage.InputTokens
				usage.CompletionTokens = ev.Usage.OutputTokens
			}
		case "content_block_start":
			if ev.ContentBlock != nil {
				curType = ev.ContentBlock.Type
				curID = ev.ContentBlock.ID
				curName = ev.ContentBlock.Name
				curArgs.Reset()
			}
		case "content_block_delta":
			var d struct {
				Type        string `json:"type"`
				Text        string `json:"text"`
				Thinking    string `json:"thinking"`
				PartialJSON string `json:"partial_json"`
			}
			_ = json.Unmarshal(ev.Delta, &d)
			switch d.Type {
			case "text_delta":
				text.WriteString(d.Text)
				if onDelta != nil {
					if err := onDelta(Event{Type: EventTextDelta, Text: d.Text}); err != nil {
						return nil, err
					}
				}
			case "thinking_delta":
				// Extended-thinking models stream their chain-of-thought here.
				// Display/persistence only; we do not enable thinking ourselves.
				if d.Thinking != "" {
					reasoning.WriteString(d.Thinking)
					if onDelta != nil {
						if err := onDelta(Event{Type: EventReasoningDelta, Text: d.Thinking}); err != nil {
							return nil, err
						}
					}
				}
			case "input_json_delta":
				curArgs.WriteString(d.PartialJSON)
			}
		case "content_block_stop":
			flushTool()
		case "message_delta":
			var d struct {
				StopReason string `json:"stop_reason"`
			}
			_ = json.Unmarshal(ev.Delta, &d)
			if d.StopReason != "" {
				finish = d.StopReason
			}
			if ev.Usage != nil && ev.Usage.OutputTokens > 0 {
				usage.CompletionTokens = ev.Usage.OutputTokens
			}
		case "error":
			if ev.Error != nil {
				return nil, fmt.Errorf("provider anthropic stream error: %s: %s", ev.Error.Type, ev.Error.Message)
			}
		}
		sawSuccess = true
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("provider anthropic stream read: %w", err)
	}
	flushTool()
	if !sawSuccess && finish == "" {
		return nil, fmt.Errorf("provider anthropic: empty stream response")
	}
	if onDelta != nil {
		_ = onDelta(Event{Type: EventDone})
	}
	stop := finish
	if stop == "" && len(toolCalls) > 0 {
		stop = "tool_use"
	}
	return &Response{
		Message:    Message{Role: RoleAssistant, Content: text.String(), Reasoning: reasoning.String(), ToolCalls: toolCalls},
		Usage:      usage,
		StopReason: stop,
	}, nil
}
