package tui

import (
	"fmt"
	"strings"
	"testing"

	"github.com/charmbracelet/bubbles/viewport"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
)

func TestWrapTextPreservesNewlinesAndWidth(t *testing.T) {
	lines := wrapText("alpha beta\ngamma", 10)
	if len(lines) < 2 {
		t.Fatalf("lines = %v", lines)
	}
	if lines[0] == "" {
		t.Fatalf("first line empty: %v", lines)
	}
	for _, l := range lines {
		if len(l) > 10 {
			t.Fatalf("line exceeds width: %q", l)
		}
	}
	// A single over-long word is hard-broken to the width.
	hard := wrapText("abcdefghij", 4)
	if hard[0] != "abcd" {
		t.Fatalf("hard break = %v", hard)
	}
}

func TestReasoningRendersCollapsedThenExpands(t *testing.T) {
	m := model{blocks: []logBlock{{
		kind: blockReasoning,
		text: "one\ntwo\nthree",
	}}}
	lines := m.renderLogLines()
	if len(lines) != 1 {
		t.Fatalf("collapsed should be one twistie line, got %v", lines)
	}
	if !strings.Contains(lines[0], "thinking") || !strings.Contains(lines[0], "3 lines") {
		t.Fatalf("twistie = %q", lines[0])
	}

	m.blocks[0].open = true
	lines = m.renderLogLines()
	if len(lines) <= 1 {
		t.Fatalf("expanded should add header + text lines, got %v", lines)
	}
	if !strings.Contains(lines[0], "thinking") {
		t.Fatalf("header = %q", lines[0])
	}
}

func TestStreamingReasoningStaysExpanded(t *testing.T) {
	m := model{blocks: []logBlock{{
		kind:      blockReasoning,
		text:      "live thought",
		streaming: true,
	}}}
	lines := m.renderLogLines()
	if len(lines) < 2 {
		t.Fatalf("streaming should render header + text, got %v", lines)
	}
}

func TestCloseReasoningCollapsesLiveBlock(t *testing.T) {
	m := model{blocks: []logBlock{{
		kind:      blockReasoning,
		text:      "live",
		streaming: true,
	}}}
	m.closeReasoning()
	if m.blocks[0].streaming || m.blocks[0].open {
		t.Fatalf("expected collapsed after close: %+v", m.blocks[0])
	}
}

func TestToggleReasoningFlipsLatestFinished(t *testing.T) {
	m := model{blocks: []logBlock{
		{kind: blockText, lines: []string{"intro"}},
		{kind: blockReasoning, text: "thought"},
	}}
	m.toggleReasoning()
	if !m.blocks[1].open {
		t.Fatal("expected toggle to open the reasoning block")
	}
	m.toggleReasoning()
	if m.blocks[1].open {
		t.Fatal("expected second toggle to close it")
	}
}

func TestAnswerStartsNewBlockNotAppendedToPrompt(t *testing.T) {
	m := model{follow: true}
	m.appendLine(roleUser, "you: fix the bug")
	m.appendDelta("here is the fix")

	if len(m.blocks) != 2 {
		t.Fatalf("expected separate user and assistant blocks, got %d", len(m.blocks))
	}
	if m.blocks[0].role != roleUser || m.blocks[1].role != roleAssistant {
		t.Fatalf("roles = %d, %d", m.blocks[0].role, m.blocks[1].role)
	}
	if m.blocks[0].lines[0] != "you: fix the bug" {
		t.Fatalf("prompt mutated: %q", m.blocks[0].lines[0])
	}
	if m.blocks[1].lines[0] != "here is the fix" {
		t.Fatalf("answer = %q", m.blocks[1].lines[0])
	}
}

func TestConsecutiveSameRoleSharesBlock(t *testing.T) {
	m := model{follow: true}
	m.appendLine(roleTool, "→ one")
	m.appendLine(roleTool, "← two")
	if len(m.blocks) != 1 {
		t.Fatalf("tool lines should share a block, got %d", len(m.blocks))
	}
	if len(m.blocks[0].lines) != 2 {
		t.Fatalf("lines = %v", m.blocks[0].lines)
	}
}

func TestUserPromptShadedDistinctly(t *testing.T) {
	// Lipgloss strips ANSI when stdout is not a TTY (as in tests), so pin a
	// color profile explicitly and restore it afterward.
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.ANSI256)
	t.Cleanup(func() { lipgloss.SetColorProfile(prev) })

	m := model{blocks: []logBlock{
		{kind: blockText, role: roleUser, lines: []string{"you: hi"}},
		{kind: blockText, role: roleAssistant, lines: []string{"hello"}},
	}}
	lines := m.renderLogLines()
	if len(lines) != 2 {
		t.Fatalf("lines = %v", lines)
	}
	// The user line must carry ANSI styling; the plain answer must not.
	if !strings.Contains(lines[0], "\x1b[") {
		t.Fatalf("user prompt expected styled output, got %q", lines[0])
	}
	if strings.Contains(lines[1], "\x1b[") {
		t.Fatalf("assistant expected terminal-default output, got %q", lines[1])
	}
}

func TestSeparatorRendersHorizontalRule(t *testing.T) {
	m := model{blocks: []logBlock{{kind: blockText, lines: []string{"answer"}}, {kind: blockSep}}}
	m.log = viewport.New(40, 10)
	lines := m.renderLogLines()
	if len(lines) != 2 {
		t.Fatalf("lines = %v", lines)
	}
	if !strings.Contains(lines[1], "─") {
		t.Fatalf("separator = %q", lines[1])
	}
}

func TestAppendSeparatorIsIdempotent(t *testing.T) {
	m := model{follow: true}
	m.appendLine(rolePlain, "[done] ok")
	m.appendSeparator()
	m.appendSeparator()
	seps := 0
	for _, b := range m.blocks {
		if b.kind == blockSep {
			seps++
		}
	}
	if seps != 1 {
		t.Fatalf("separators = %d, want 1", seps)
	}
}

func TestScrollUpSuspendsFollowAndBottomResumes(t *testing.T) {
	m := model{follow: true}
	m.log = viewport.New(40, 5)
	var lines []string
	for i := 0; i < 30; i++ {
		lines = append(lines, fmt.Sprintf("line %d", i))
	}
	m.blocks = append(m.blocks, logBlock{kind: blockText, lines: lines})

	m.scroll(func(v *viewport.Model) { v.PageUp() })
	if m.follow {
		t.Fatal("scrolling up should suspend follow")
	}
	if m.log.AtBottom() {
		t.Fatal("expected viewport to have left the bottom")
	}

	m.scroll(func(v *viewport.Model) { v.GotoBottom() })
	if !m.follow {
		t.Fatal("returning to bottom should resume follow")
	}
}

func TestRenderLogHonoursFollow(t *testing.T) {
	m := model{follow: false}
	m.log = viewport.New(40, 5)
	var lines []string
	for i := 0; i < 30; i++ {
		lines = append(lines, fmt.Sprintf("line %d", i))
	}
	m.blocks = append(m.blocks, logBlock{kind: blockText, lines: lines})
	m.log.GotoTop()

	m.renderLog()
	if m.log.YOffset != 0 {
		t.Fatalf("follow=false must not snap to bottom; YOffset=%d", m.log.YOffset)
	}

	m.follow = true
	m.renderLog()
	if !m.log.AtBottom() {
		t.Fatalf("follow=true should pin to bottom; YOffset=%d", m.log.YOffset)
	}
}
