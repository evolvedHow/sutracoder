package agent

import (
	"context"
	"fmt"
	"strings"

	"sutracoder/internal/provider"
)

// contextFillRatio is the fraction of the context window at which
// windowing/summarization triggers.
const contextFillRatio = 0.8

// summarizeKeepRecent is the number of most-recent messages always kept raw
// when summarizing older history.
const summarizeKeepRecent = 6

// manageContext keeps the prompt within the model's window. When the estimated
// size exceeds contextFillRatio of the limit, the oldest excess messages are
// replaced by a single summary. Summarization failures degrade to dropping the
// oldest non-essential messages so the loop never stalls on windowing.
func (l *Loop) manageContext(ctx context.Context) error {
	limit := l.cfg.ContextLimit
	if limit <= 0 {
		return nil
	}
	est := estimateTokens(l.messages)
	if est <= int(float64(limit)*contextFillRatio) {
		return nil
	}
	// Preserve the leading system message and always keep recent turns raw.
	if len(l.messages) <= summarizeKeepRecent+1 {
		return nil
	}
	// Split: keep [0]=system + last summarizeKeepRecent; summarize the middle.
	midEnd := len(l.messages) - summarizeKeepRecent
	if midEnd <= 1 {
		return nil
	}
	older := l.messages[1:midEnd]
	summary, err := l.summarize(ctx, older)
	if err != nil {
		l.log.Warn("summarization failed; dropping oldest turns instead", "err", err)
		return l.dropOldest(midEnd)
	}
	summaryMsg := provider.Message{
		Role:    provider.RoleUser,
		Content: "<context_summary>\n" + summary + "\n</context_summary>\n(Continuing from the summary above; the most recent turns follow.)",
	}
	l.messages = append([]provider.Message{l.messages[0], summaryMsg}, l.messages[midEnd:]...)
	return nil
}

// summarize asks the provider to condense older messages into a running
// summary. It issues a tool-less completion so the model cannot act here.
func (l *Loop) summarize(ctx context.Context, older []provider.Message) (string, error) {
	var transcript strings.Builder
	for _, m := range older {
		switch m.Role {
		case provider.RoleSystem:
			continue
		case provider.RoleUser:
			transcript.WriteString("USER: " + m.Content + "\n\n")
		case provider.RoleAssistant:
			if m.Content != "" {
				transcript.WriteString("ASSISTANT: " + m.Content + "\n\n")
			}
			for _, tc := range m.ToolCalls {
				fmt.Fprintf(&transcript, "ASSISTANT called %s %s\n\n", tc.Name, tc.Arguments)
			}
		case provider.RoleTool:
			transcript.WriteString("TOOL RESULT: " + firstLines(m.Content, 40) + "\n\n")
		}
	}
	if transcript.Len() == 0 {
		return "", fmt.Errorf("nothing to summarize")
	}
	req := provider.Request{
		Messages: []provider.Message{
			{Role: provider.RoleSystem, Content: "You compress conversation history into a dense factual summary. Preserve file paths, decisions, errors, and unresolved tasks. Do not add commentary."},
			{Role: provider.RoleUser, Content: "Summarize this agent conversation so work can continue seamlessly:\n\n" + transcript.String()},
		},
		Model:     l.cfg.Model,
		MaxTokens: 1024,
	}
	resp, err := l.prov.Complete(ctx, req, nil)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(resp.Message.Content) == "" {
		return "", fmt.Errorf("empty summary")
	}
	return resp.Message.Content, nil
}

// dropOldest trims messages down to the system prompt plus the most recent
// keep messages, used when summarization is unavailable.
func (l *Loop) dropOldest(midEnd int) error {
	if midEnd <= 1 || midEnd >= len(l.messages) {
		return nil
	}
	trimmed := append([]provider.Message{l.messages[0]}, l.messages[midEnd:]...)
	l.messages = trimmed
	return nil
}

// estimateTokens approximates token count as total characters / 4. This is a
// pre-flight heuristic; authoritative usage comes from provider responses.
func estimateTokens(msgs []provider.Message) int {
	total := 0
	for _, m := range msgs {
		total += len(m.Content)
		for _, tc := range m.ToolCalls {
			total += len(tc.Name) + len(tc.Arguments)
		}
		total += 8 // per-message overhead
	}
	return total / 4
}

// firstLines returns at most n lines of s, for compact summaries.
func firstLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) <= n {
		return s
	}
	return strings.Join(lines[:n], "\n") + "\n…"
}
