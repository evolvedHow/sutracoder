package tui

import (
	"strings"
	"testing"
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
