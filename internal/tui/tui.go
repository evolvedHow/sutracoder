// Package tui implements Sutracoder's Bubble Tea interface: a split-screen
// Plan/Log dashboard with a live status bar, an opencode-style input that routes
// slash commands to the controller, an interrupt path, and a confirmation modal
// for restricted tool calls. It holds no agent state of its own — everything
// dynamic comes from the Controller and the event bus.
package tui

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"sutracoder/internal/agent"
	"sutracoder/internal/tools"
)

// logoArt is the startup wordmark: "sutracoder" with a macron over the u,
// rendered at the top of the log panel on first paint.
const logoArt = ` -
sutracoder`

// Controller is the surface the TUI calls to run goals, execute slash commands,
// interrupt runs, and read status. The app.App type implements it; defining it
// here keeps the view decoupled from the controller and trivially testable.
type Controller interface {
	// HandleCommand runs one slash command, returning synchronous feedback and
	// a quit flag for /quit and /exit.
	HandleCommand(line string) (string, bool)
	// Submit interprets typed input: interject, continue, or start a goal.
	Submit(text string)
	// Stop cancels the current run; Shutdown tears the controller down on exit.
	Stop()
	Shutdown()
	// Status accessors for the status bar.
	Provider() string
	Model() string
	Mode() string
	SessionID() string
	Tokens() int
	Turn() int
	Running() bool
	Connected() bool
}

// model is the root Bubble Tea model. All mutable state flows through Update's
// value return, so the controller goroutine never shares a model pointer.
type model struct {
	ctrl   Controller
	events <-chan agent.Event

	plan  viewport.Model
	log   viewport.Model
	input textinput.Model

	width  int
	height int
	ready  bool

	version string
	author  string

	state     agent.State
	blocks    []logBlock
	planItems []tools.PlanItem
	streaming strings.Builder
	quitting  bool

	// pendingApproval holds the reply channel for a modal currently shown.
	pendingApproval chan bool
	modalSummary    string
	confirmFocus    int // 0 = Yes, 1 = No
}

// blockKind discriminates log blocks.
type blockKind int

const (
	// blockText is ordinary transcript content (log lines, streaming answer).
	blockText blockKind = iota
	// blockReasoning is the model's chain-of-thought, collapsible to a twistie.
	blockReasoning
)

// logBlock is one unit of log content. Text blocks hold pre-split display
// rows; reasoning blocks hold the full chain-of-thought and can render either
// expanded (live while streaming, or when toggled open) or collapsed to a
// single twistie line.
type logBlock struct {
	// kind discriminates the block.
	kind blockKind
	// lines holds display rows for text blocks.
	lines []string
	// text is the full chain-of-thought for reasoning blocks.
	text string
	// open forces a finished reasoning block to render expanded.
	open bool
	// streaming marks a reasoning block still receiving deltas; it always
	// renders expanded and dimmed, and cannot be collapsed until it finishes.
	streaming bool
}

// lastTextBlock returns a pointer to the trailing text block, appending a fresh
// one when the log ends in a non-text block (so appends after reasoning start
// on their own line rather than merging into the chain-of-thought).
func (m *model) lastTextBlock() *logBlock {
	if n := len(m.blocks); n > 0 && m.blocks[n-1].kind == blockText {
		return &m.blocks[n-1]
	}
	m.blocks = append(m.blocks, logBlock{kind: blockText})
	return &m.blocks[len(m.blocks)-1]
}

// appendReasoningDelta folds one chain-of-thought fragment into the live
// reasoning block, opening a new one when the previous block has finished.
func (m *model) appendReasoningDelta(delta string) {
	if n := len(m.blocks); n > 0 && m.blocks[n-1].kind == blockReasoning && m.blocks[n-1].streaming {
		m.blocks[n-1].text += delta
	} else {
		m.blocks = append(m.blocks, logBlock{kind: blockReasoning, text: delta, streaming: true})
	}
}

// closeReasoning ends the live stream on the trailing reasoning block (if any)
// and collapses it to a twistie. Called when the visible answer, a tool call, or
// the turn's completion begins.
func (m *model) closeReasoning() {
	if n := len(m.blocks); n > 0 && m.blocks[n-1].kind == blockReasoning && m.blocks[n-1].streaming {
		m.blocks[n-1].streaming = false
		m.blocks[n-1].open = false
	}
}

// toggleReasoning flips the expansion state of the most recent finished
// reasoning block. The live (streaming) block is always expanded, so toggling
// during a stream is a no-op until it closes.
func (m *model) toggleReasoning() {
	for i := len(m.blocks) - 1; i >= 0; i-- {
		if m.blocks[i].kind == blockReasoning && !m.blocks[i].streaming {
			m.blocks[i].open = !m.blocks[i].open
			break
		}
	}
}

// Options configures the TUI.
type Options struct {
	Controller Controller
	Events     <-chan agent.Event
	// Version is stamped onto the startup splash (matches main's -ldflags).
	Version string
	// Author is credited on the startup splash.
	Author string
}

// New builds the initial model with a ready text input and the startup splash
// seeded into the log so the logo, version, and author render on first paint.
func New(opts Options) model {
	ti := textinput.New()
	ti.Placeholder = "type a goal, or /help — ctrl+s stop · ctrl+c quit"
	ti.Focus()
	m := model{
		ctrl:    opts.Controller,
		events:  opts.Events,
		state:   agent.StateIdle,
		input:   ti,
		version: opts.Version,
		author:  opts.Author,
	}
	m.seedSplash()
	return m
}

// seedSplash prepends the text logo, version, and author credit to the log as
// an initial text block (no render yet; the first WindowSizeMsg paints it).
func (m *model) seedSplash() {
	b := logBlock{kind: blockText}
	b.lines = append(b.lines, strings.Split(logoArt, "\n")...)
	if m.version != "" {
		b.lines = append(b.lines, fmt.Sprintf("sutracoder v%s", m.version))
	}
	if m.author != "" {
		b.lines = append(b.lines, "vibe-coded by "+m.author)
	}
	b.lines = append(b.lines, "")
	m.blocks = append(m.blocks, b)
}

func (m model) Init() tea.Cmd {
	return tea.Batch(m.waitEvent(), m.input.Cursor.BlinkCmd())
}

// waitEvent blocks on the event bus until the controller or loop emits something.
func (m model) waitEvent() tea.Cmd {
	return func() tea.Msg {
		ev, ok := <-m.events
		if !ok {
			return channelClosedMsg{}
		}
		return eventMsg{ev}
	}
}

type eventMsg struct{ ev agent.Event }
type channelClosedMsg struct{}

// approvalAnswerMsg is posted when the user answers the confirmation modal.
type approvalAnswerMsg struct{ allow bool }

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.layout()
		m.ready = true
		m.renderAll()
		return m, nil

	case channelClosedMsg:
		m.appendLogBlock("[system] event stream ended")
		return m, nil

	case eventMsg:
		return m.handleEvent(msg.ev)

	case approvalAnswerMsg:
		if m.pendingApproval != nil {
			m.pendingApproval <- msg.allow
			m.pendingApproval = nil
		}
		return m, m.waitEvent()

	case tea.KeyMsg:
		if m.pendingApproval != nil {
			return m.handleConfirmKey(msg)
		}
		return m.handleKey(msg)
	}
	return m, nil
}

// handleEvent applies an agent event and re-arms the event listener.
func (m model) handleEvent(ev agent.Event) (tea.Model, tea.Cmd) {
	switch ev.Kind {
	case agent.EventState:
		m.state = ev.State
	case agent.EventReasoningDelta:
		// Chain-of-thought streams live into a dimmed, expandable block.
		m.appendReasoningDelta(ev.Text)
	case agent.EventStreamDelta:
		// The visible answer is beginning: fold the reasoning away.
		m.closeReasoning()
		m.streaming.WriteString(ev.Text)
		m.appendDelta(ev.Text)
	case agent.EventAssistantText:
		m.closeReasoning()
		if m.streaming.Len() > 0 {
			m.appendLog("")
			m.streaming.Reset()
		}
		m.appendLog("assistant: " + ev.Text)
	case agent.EventToolStart:
		m.closeReasoning()
		m.appendLog(fmt.Sprintf("→ %s %s", ev.ToolName, compactArgs(ev.ToolArgs)))
	case agent.EventToolEnd:
		m.appendLog(fmt.Sprintf("← %s [%s] (%dms)", ev.ToolName, ev.Result.Status, ev.Result.DurationMS))
		if ev.Result.Status == tools.StatusFailure && ev.Result.Stderr != "" {
			m.appendLog("    " + firstLine(ev.Result.Stderr))
		}
	case agent.EventPlanUpdated:
		m.planItems = ev.Plan
	case agent.EventApproval:
		m.pendingApproval = ev.Approval.Reply
		m.modalSummary = ev.Approval.Call.Summary
		m.confirmFocus = 0
		m.appendLog("⚠ needs approval: " + ev.Approval.Call.Summary)
		m.renderAll()
		return m, tea.Batch(m.waitEvent())
	case agent.EventError:
		prefix := "warn"
		if ev.Fatal {
			prefix = "fatal"
		}
		m.appendLog(fmt.Sprintf("[%s] %v", prefix, ev.Err))
	case agent.EventDone:
		m.closeReasoning()
		m.appendLog("[done] " + ev.Text)
		m.state = agent.StateIdle
	case agent.EventSystem:
		m.appendLogBlock(ev.Text)
	}
	m.renderAll()
	return m, m.waitEvent()
}

// handleKey processes normal-mode key presses.
func (m model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c":
		if m.ctrl != nil {
			m.ctrl.Shutdown()
		}
		m.quitting = true
		return m, tea.Quit
	case "ctrl+s":
		if m.ctrl != nil {
			m.ctrl.Stop()
		}
		m.appendLog("[system] interrupted (ctrl+s)")
		m.state = agent.StateIdle
		m.renderAll()
		return m, nil
	case "ctrl+t":
		// Toggle the most recent finished chain-of-thought block. Bound to a
		// chord (not bare "t") so it never collides with typing in the input.
		m.toggleReasoning()
		m.renderLog()
		return m, nil
	case "enter":
		return m.submitLine()
	default:
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(msg)
		return m, cmd
	}
}

// submitLine dispatches the typed line: slash commands go to the controller's
// command handler, everything else is submitted as a goal or follow-up.
func (m model) submitLine() (tea.Model, tea.Cmd) {
	text := strings.TrimSpace(m.input.Value())
	if text == "" {
		return m, nil
	}
	m.input.SetValue("")

	if strings.HasPrefix(text, "/") {
		m.appendLog("› " + text)
		feedback, quit := m.ctrl.HandleCommand(text)
		if feedback != "" {
			m.appendLogBlock(feedback)
		}
		if quit {
			m.ctrl.Shutdown()
			m.quitting = true
			m.renderAll()
			return m, tea.Quit
		}
		m.renderAll()
		return m, nil
	}

	m.appendLog("you: " + text)
	m.ctrl.Submit(text)
	m.renderAll()
	return m, nil
}

// handleConfirmKey processes keys while the approval modal is open.
func (m model) handleConfirmKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "left", "right", "tab":
		m.confirmFocus = 1 - m.confirmFocus
		return m, nil
	case "y", "Y":
		return m, tea.Cmd(func() tea.Msg { return approvalAnswerMsg{allow: true} })
	case "n", "N", "esc":
		return m, tea.Cmd(func() tea.Msg { return approvalAnswerMsg{allow: false} })
	case "enter":
		allow := m.confirmFocus == 0
		return m, tea.Cmd(func() tea.Msg { return approvalAnswerMsg{allow: allow} })
	}
	return m, nil
}

func (m *model) layout() {
	planW := m.width * 30 / 100
	if planW < 24 {
		planW = 24
	}
	logW := m.width - planW - 1
	// Budget the non-panel rows: each bordered panel is bodyH+2 (top/bottom
	// border), plus the status bar, the prompt, and the hint line below.
	//bodyH = height - 2 (borders) - 1 (status) - 1 (prompt) - 1 (hint).
	bodyH := m.height - 5
	if bodyH < 3 {
		bodyH = 3
	}
	m.plan = viewport.New(planW, bodyH)
	m.log = viewport.New(logW, bodyH)
}

func (m *model) appendLog(line string) {
	b := m.lastTextBlock()
	b.lines = append(b.lines, line)
	m.renderLog()
}

// appendLogBlock adds multi-line feedback (help, status) as separate rows.
func (m *model) appendLogBlock(s string) {
	b := m.lastTextBlock()
	b.lines = append(b.lines, strings.Split(s, "\n")...)
	m.renderLog()
}

func (m *model) appendDelta(delta string) {
	b := m.lastTextBlock()
	if len(b.lines) == 0 {
		b.lines = append(b.lines, "")
	}
	b.lines[len(b.lines)-1] += delta
	m.renderLog()
}

// renderLog flattens the block list into display rows and repaints the viewport.
func (m *model) renderLog() {
	m.log.SetContent(strings.Join(m.renderLogLines(), "\n"))
	m.log.GotoBottom()
}

// renderLogLines expands or collapses each block per its state: reasoning
// streams live and dimmed while in flight, then folds to a twistie line unless
// toggled open. Text blocks render their rows verbatim.
func (m *model) renderLogLines() []string {
	width := m.log.Width
	if width <= 0 {
		width = 60
	}
	var out []string
	for _, b := range m.blocks {
		if b.kind == blockText {
			out = append(out, b.lines...)
			continue
		}
		wrapped := wrapText(b.text, max(1, width-2))
		if b.streaming || b.open {
			out = append(out, reasoningHeaderStyle.Render("✦ thinking…"))
			for _, line := range wrapped {
				out = append(out, reasoningTextStyle.Render(line))
			}
			continue
		}
		n := len(wrapped)
		if b.text == "" {
			n = 0
		}
		out = append(out, reasoningCollapsedStyle.Render(
			fmt.Sprintf("▸ thinking · %d line%s · ctrl+t to expand", n, plural(n))))
	}
	return out
}

// wrapText word-wraps s to width, preserving explicit newlines and hard-breaking
// over-long words. It always returns at least one (possibly empty) line.
func wrapText(s string, width int) []string {
	if width < 1 {
		width = 1
	}
	var out []string
	for _, para := range strings.Split(s, "\n") {
		words := strings.Fields(para)
		if len(words) == 0 {
			out = append(out, "")
			continue
		}
		line := words[0]
		for _, w := range words[1:] {
			if len(line)+1+len(w) <= width {
				line += " " + w
			} else {
				out = append(out, line)
				line = w
			}
		}
		for len(line) > width {
			out = append(out, line[:width])
			line = line[width:]
		}
		out = append(out, line)
	}
	if len(out) == 0 {
		out = append(out, "")
	}
	return out
}

// plural returns "s" for any count other than one (used in the twistie label).
func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func (m *model) renderPlan() {
	var b strings.Builder
	if len(m.planItems) == 0 {
		b.WriteString("(no plan yet)")
	}
	for _, it := range m.planItems {
		box := "[ ]"
		switch it.Status {
		case "in_progress":
			box = "[~]"
		case "done":
			box = "[x]"
		case "failed":
			box = "[!]"
		}
		fmt.Fprintf(&b, "%s %s\n", box, it.Text)
	}
	m.plan.SetContent(b.String())
}

// statusBarText renders a live snapshot straight from the controller.
func (m model) statusBarText() string {
	if m.ctrl == nil {
		return "[ starting ]"
	}
	conn := "○"
	if m.ctrl.Connected() {
		conn = "●"
	}
	model := m.ctrl.Model()
	if model == "" {
		model = m.ctrl.Provider() + "-default"
	}
	sid := m.ctrl.SessionID()
	if len(sid) > 8 {
		sid = sid[:8]
	}
	return fmt.Sprintf("%s %s · %s · %s · [%s] · tok %d · turn %d · %s",
		conn, m.ctrl.Provider(), model, m.ctrl.Mode(), m.state,
		m.ctrl.Tokens(), m.ctrl.Turn(), sid)
}

func (m *model) renderAll() {
	m.renderPlan()
	m.renderLog()
}

// Styles.
var (
	planStyle   = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("62")).Padding(0, 1)
	logStyle    = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("240")).Padding(0, 1)
	statusStyle = lipgloss.NewStyle().Background(lipgloss.Color("62")).Foreground(lipgloss.Color("230")).Bold(true)
	inputStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("212"))
	modalStyle  = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("212")).Padding(1, 2).Width(64)
	hintStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))

	// Reasoning is deliberately dimmed and italic so it reads as the model's
	// private chain-of-thought, distinct from the visible answer.
	reasoningHeaderStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("240")).Italic(true)
	reasoningTextStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("245")).Italic(true)
	reasoningCollapsedStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
)

// View renders the full interface.
func (m model) View() string {
	if !m.ready {
		return "starting sutracoder…"
	}
	if m.quitting {
		return "goodbye\n"
	}
	if m.pendingApproval != nil {
		return m.viewApproval()
	}
	left := planStyle.Width(m.plan.Width).Render(m.plan.View())
	right := logStyle.Width(m.log.Width).Render(m.log.View())
	body := lipgloss.JoinHorizontal(lipgloss.Top, left, right)
	prompt := inputStyle.Render("› ") + m.input.View()
	hint := hintStyle.Render("/help commands · ctrl+t thinking")
	return lipgloss.JoinVertical(lipgloss.Left, body, statusStyle.Render(m.statusBarText()), prompt, hint)
}

func (m model) viewApproval() string {
	yes := "[ Yes ]"
	no := "[ No ]"
	if m.confirmFocus == 0 {
		yes = lipgloss.NewStyle().Foreground(lipgloss.Color("212")).Bold(true).Render(yes)
	} else {
		no = lipgloss.NewStyle().Foreground(lipgloss.Color("212")).Bold(true).Render(no)
	}
	body := fmt.Sprintf("Allow this action?\n\n%s\n\n%s  %s\n\ny/n · ←/→ move · enter confirm",
		m.modalSummary, yes, no)
	return lipgloss.JoinVertical(lipgloss.Left, statusStyle.Render(m.statusBarText()), modalStyle.Render(body))
}

func compactArgs(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	s := string(raw)
	if len(s) > 100 {
		return s[:100] + "…"
	}
	return s
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
