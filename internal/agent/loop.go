package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"sutracoder/internal/config"
	"sutracoder/internal/provider"
	"sutracoder/internal/store"
	"sutracoder/internal/tools"
)

// ErrFatal wraps a loop-stopping error (auth failure, exhausted retries,
// irrecoverable protocol error). Errors of this class halt the loop rather
// than feeding back to the model.
type ErrFatal struct{ Err error }

// Error implements the error interface.
func (e ErrFatal) Error() string { return "fatal: " + e.Err.Error() }
func (e ErrFatal) Unwrap() error { return e.Err }

// maxProviderRetries bounds transient-error retries within one turn.
const maxProviderRetries = 3

// Loop is a single agent session's execution engine.
type Loop struct {
	cfg    config.Config
	prov   provider.Provider
	engine *tools.Engine
	store  *store.Store
	log    *slog.Logger
	sink   Sink

	// mu guards the fields the UI polls (sessionID, goal, turn, tokens) and the
	// pendingUser interjection queue, which the UI writes and the loop swaps.
	mu          sync.Mutex
	sessionID   string
	goal        string
	turn        int
	tokensIn    int
	tokensOut   int
	pendingUser []string

	// messages is owned exclusively by the loop goroutine after Start/Resume.
	messages []provider.Message
	// lastFailed marks that the previous tool interaction failed, so the next
	// one is flagged corrected=true for the downstream engine.
	lastFailed bool
}

// New builds a Loop. A nil sink or store is tolerated (events dropped / no
// persistence).
func New(cfg config.Config, prov provider.Provider, engine *tools.Engine, st *store.Store, log *slog.Logger, sink Sink) *Loop {
	if sink == nil {
		sink = noopSink
	}
	if log == nil {
		log = slog.Default()
	}
	return &Loop{cfg: cfg, prov: prov, engine: engine, store: st, log: log, sink: sink}
}

// SessionID returns the current session identifier.
func (l *Loop) SessionID() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.sessionID
}

// Active reports whether a session exists that can be continued.
func (l *Loop) Active() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.sessionID != ""
}

// SetProvider swaps the provider for subsequent turns. Call only while idle.
func (l *Loop) SetProvider(p provider.Provider) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.prov = p
}

// SetModel swaps the requested model for subsequent turns. Call only while idle.
func (l *Loop) SetModel(m string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.cfg.Model = m
}

// SetMode toggles the engine's approval gate (autonomous vs supervised).
func (l *Loop) SetMode(mode string) {
	if l.engine != nil {
		l.engine.Supervised = mode == "supervised"
	}
}

// Goal returns the current session goal.
func (l *Loop) Goal() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.goal
}

// TokensUsed returns cumulative prompt+completion tokens.
func (l *Loop) TokensUsed() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.tokensIn + l.tokensOut
}

// Turn returns the number of completed provider calls.
func (l *Loop) Turn() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.turn
}

// Inject queues a user message to be inserted at the next turn boundary.
func (l *Loop) Inject(text string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.pendingUser = append(l.pendingUser, text)
}

// addUsage records token usage for the current turn.
func (l *Loop) addUsage(in, out int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.tokensIn += in
	l.tokensOut += out
}

// incTurn advances the turn counter.
func (l *Loop) incTurn() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.turn++
}

// startFresh initializes per-session counters and the goal under lock.
func (l *Loop) startFresh(goal string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.goal = goal
	l.turn, l.tokensIn, l.tokensOut = 0, 0, 0
	l.lastFailed = false
	l.pendingUser = nil
}

// setSession records the session id under lock.
func (l *Loop) setSession(id string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.sessionID = id
}

// recordResume loads a resumed session's identity and turn count under lock.
func (l *Loop) recordResume(id, goal string, turns int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.sessionID = id
	l.goal = goal
	l.turn = turns
}

// Start begins a fresh session for goal and runs the loop to completion, a
// hard limit, a fatal error, or context cancellation.
func (l *Loop) Start(ctx context.Context, systemPrompt, goal string) error {
	l.startFresh(goal)
	l.messages = []provider.Message{{Role: provider.RoleSystem, Content: systemPrompt}}
	l.messages = append(l.messages, provider.Message{Role: provider.RoleUser, Content: goal})

	if l.store != nil {
		id, err := l.store.NewSession(goal)
		if err != nil {
			return fmt.Errorf("create session: %w", err)
		}
		l.setSession(id)
		l.persistMessage(l.messages[0], nil, "")
		l.persistMessage(l.messages[1], nil, "")
	}
	return l.run(ctx)
}

// Resume reloads a session's conversation and continues it.
func (l *Loop) Resume(ctx context.Context, sessionID string) error {
	if l.store == nil {
		return fmt.Errorf("cannot resume without a store")
	}
	goal, status, _, err := l.store.GetSession(sessionID)
	if err != nil {
		return fmt.Errorf("load session %s: %w", sessionID, err)
	}
	if status == "completed" {
		return fmt.Errorf("session %s already completed", sessionID)
	}
	turns, err := l.store.ListTurns(sessionID)
	if err != nil {
		return err
	}
	l.messages = rebuildMessages(turns)
	l.recordResume(sessionID, goal, len(turns))
	return l.run(ctx)
}

// Continue appends a follow-up user message to the active session and runs the
// loop again. It reuses the existing conversation so multi-turn REPL sessions
// preserve context. It errors if no session has been started or resumed yet.
func (l *Loop) Continue(ctx context.Context, userInput string) error {
	if !l.Active() {
		return fmt.Errorf("no active session to continue; submit a new goal first")
	}
	m := provider.Message{Role: provider.RoleUser, Content: userInput}
	l.messages = append(l.messages, m)
	l.lastFailed = false
	l.persistMessage(m, nil, "")
	return l.run(ctx)
}

// run is the bounded Plan-Act-Observe-Correct loop.
func (l *Loop) run(ctx context.Context) error {
	var runErr error
	defer func() {
		if l.store != nil && l.sessionID != "" {
			status := "stopped"
			switch {
			case runErr == nil:
				status = "completed"
			case errors.Is(runErr, context.Canceled):
				status = "stopped"
			default:
				status = "error"
			}
			if l.engine.Completed() {
				status = "completed"
				_ = l.store.SetSessionSummary(l.sessionID, l.engine.CompletionSummary())
			}
			_ = l.store.SetSessionStatus(l.sessionID, status)
		}
	}()

	for {
		if err := ctx.Err(); err != nil {
			l.emitState(StateStopped)
			return err
		}
		if l.engine.Completed() {
			l.emitState(StateCompleted)
			return nil
		}
		if l.Turn() >= l.cfg.MaxTurns {
			l.emitState(StateLimitHit)
			return ErrFatal{fmt.Errorf("max turns (%d) reached", l.cfg.MaxTurns)}
		}
		if l.TokensUsed() >= l.cfg.MaxTokensBudget {
			l.emitState(StateLimitHit)
			return ErrFatal{fmt.Errorf("token budget (%d) exceeded", l.cfg.MaxTokensBudget)}
		}

		l.flushPendingUser(ctx)
		if err := l.manageContext(ctx); err != nil {
			l.log.Warn("context management degraded", "err", err)
		}

		if done, err := l.runProviderTurn(ctx); err != nil {
			runErr = err
			var fatal ErrFatal
			if errors.As(err, &fatal) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				if errors.Is(err, context.Canceled) {
					l.emitState(StateStopped)
				} else {
					l.emitState(StateError)
					l.emit(Event{Kind: EventError, Err: err, Fatal: true})
				}
				return err
			}
			// Recoverable: report and continue to next turn.
			l.emit(Event{Kind: EventError, Err: err, Fatal: false})
			l.log.Warn("recoverable provider error", "err", err)
			l.incTurn()
			continue
		} else if done {
			// Assistant returned text with no tool calls: it has nothing
			// further to act on. End the run cleanly.
			l.emitState(StateIdle)
			return nil
		}
		l.incTurn()
	}
}

// runProviderTurn performs one provider call and executes any returned tools.
// It reports done=true when the assistant produced no tool calls (terminal).
func (l *Loop) runProviderTurn(ctx context.Context) (bool, error) {
	l.emitState(StateThinking)
	resp, err := l.completeWithRetry(ctx)
	if err != nil {
		return false, err
	}
	l.addUsage(resp.Usage.PromptTokens, resp.Usage.CompletionTokens)

	assistant := resp.Message
	assistant.Role = provider.RoleAssistant
	l.messages = append(l.messages, assistant)
	l.persistMessage(assistant, assistant.ToolCalls, "")
	if assistant.Content != "" {
		l.emit(Event{Kind: EventAssistantText, Text: assistant.Content})
	}

	if len(assistant.ToolCalls) == 0 {
		// No action taken: terminal for this run.
		l.emit(Event{Kind: EventDone, Text: assistant.Content})
		return true, nil
	}

	for _, tc := range assistant.ToolCalls {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		result := l.executeTool(ctx, tc)
		if result.Status == tools.StatusFailure && l.engine.Tool(tc.Name) != nil && tc.Name != "complete_goal" {
			l.lastFailed = true
		}
		if l.store != nil && l.sessionID != "" {
			_ = l.store.Emit(l.buildInteraction(tc, result))
			l.persistToolResult(tc, result)
		}
		if l.engine.Completed() {
			break
		}
	}
	return false, nil
}

// executeTool runs one tool call, wiring the approval gate and emitting
// start/end events. It returns the structured result.
func (l *Loop) executeTool(ctx context.Context, tc provider.ToolCall) tools.Result {
	l.emitState(StateExecuting)
	l.emit(Event{Kind: EventToolStart, ToolName: tc.Name, ToolArgs: json.RawMessage(tc.Arguments)})

	args := json.RawMessage(tc.Arguments)
	if !json.Valid(args) {
		res := tools.Result{
			Status:    tools.StatusFailure,
			Tool:      tc.Name,
			Args:      args,
			ToolError: "malformed JSON arguments",
			Stderr:    "malformed JSON arguments: " + tc.Arguments,
		}
		l.emit(Event{Kind: EventToolEnd, ToolName: tc.Name, ToolArgs: args, Result: res})
		return res
	}
	res := l.engine.Run(ctx, tc.Name, args)
	if res.Status != tools.StatusFailure {
		l.lastFailed = false
	}
	l.emit(Event{Kind: EventToolEnd, ToolName: tc.Name, ToolArgs: args, Result: res})
	l.emitPlan()
	return res
}

// completeWithRetry calls the provider, retrying transient StatusErrors with
// exponential backoff. Auth and other non-transient errors are fatal.
func (l *Loop) completeWithRetry(ctx context.Context) (*provider.Response, error) {
	req := provider.Request{
		Messages:    l.messages,
		Tools:       toolSpecs(l.engine),
		Model:       l.cfg.Model,
		MaxTokens:   4096,
		Temperature: l.cfg.Temperature,
	}
	var lastErr error
	for attempt := 0; attempt < maxProviderRetries; attempt++ {
		resp, err := l.prov.Complete(ctx, req, func(ev provider.Event) error {
			switch ev.Type {
			case provider.EventTextDelta:
				l.emitState(StateStreaming)
				l.emit(Event{Kind: EventStreamDelta, Text: ev.Text})
			case provider.EventReasoningDelta:
				// Chain-of-thought streams live for display; it is never sent
				// back to the provider. Keep the status on Thinking so the bar
				// reflects that the model is still reasoning, not answering.
				l.emitState(StateThinking)
				l.emit(Event{Kind: EventReasoningDelta, Text: ev.Text})
			}
			return nil
		})
		if err == nil {
			return resp, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		var stErr *provider.StatusError
		if errors.As(err, &stErr) {
			if stErr.IsAuth() {
				return nil, ErrFatal{err}
			}
			if !stErr.IsTransient() {
				return nil, ErrFatal{err}
			}
		} else if !isTransientNet(err) {
			return nil, ErrFatal{err}
		}
		lastErr = err
		backoff := time.Duration(1<<attempt) * time.Second
		l.log.Warn("transient provider error, backing off", "attempt", attempt+1, "backoff", backoff, "err", err)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(backoff):
		}
	}
	return nil, ErrFatal{fmt.Errorf("provider retries exhausted: %w", lastErr)}
}

// buildInteraction assembles the JSONL emission record for one tool call. The
// assistant's chain-of-thought for this turn (when the backend exposed it) is
// attached so the downstream engine can learn from it.
func (l *Loop) buildInteraction(tc provider.ToolCall, res tools.Result) store.Interaction {
	rc := res.ReturnCode
	var reasoning string
	if n := len(l.messages); n > 0 {
		reasoning = l.messages[n-1].Reasoning
	}
	return store.Interaction{
		SessionID:        l.sessionID,
		Turn:             l.turn,
		Timestamp:        time.Now().UTC(),
		Goal:             l.goal,
		Tool:             tc.Name,
		Args:             json.RawMessage(tc.Arguments),
		Status:           res.Status,
		Stdout:           res.Stdout,
		Stderr:           res.Stderr,
		ReturnCode:       rc,
		DurationMS:       res.DurationMS,
		Truncated:        res.Truncated,
		ToolError:        res.ToolError,
		Corrected:        l.lastFailed,
		TokensPrompt:     l.tokensIn,
		TokensCompletion: l.tokensOut,
		SessionStatus:    "running",
		Reasoning:        reasoning,
	}
}

// persistMessage writes a non-tool-result message to the store (best-effort).
func (l *Loop) persistMessage(m provider.Message, toolCalls []provider.ToolCall, toolCallID string) {
	if l.store == nil || l.sessionID == "" {
		return
	}
	var tcJSON string
	if len(toolCalls) > 0 {
		if b, err := json.Marshal(toolCalls); err == nil {
			tcJSON = string(b)
		}
	}
	_ = l.store.AppendTurn(l.sessionID, l.turn, m.Role, m.Content, tcJSON, toolCallID, "", m.Reasoning, nil)
}

// persistToolResult writes the assistant's tool call and the tool's result as
// conversation rows (best-effort).
func (l *Loop) persistToolResult(tc provider.ToolCall, res tools.Result) {
	// The tool result message fed back to the model:
	obs := res.Observation()
	_ = l.store.AppendTurn(l.sessionID, l.turn, provider.RoleTool, obs, "", tc.ID, tc.Name, "", &store.Interaction{
		Status: res.Status, Stdout: res.Stdout, Stderr: res.Stderr,
		ReturnCode: res.ReturnCode, DurationMS: res.DurationMS, Truncated: res.Truncated,
		ToolError: res.ToolError, Corrected: l.lastFailed,
	})
	// Feed the observation back into the live conversation.
	l.messages = append(l.messages, provider.Message{
		Role: provider.RoleTool, Content: obs, ToolCallID: tc.ID, Name: tc.Name,
	})
}

// flushPendingUser injects queued user interjections as user messages.
func (l *Loop) flushPendingUser(ctx context.Context) {
	l.mu.Lock()
	queued := l.pendingUser
	l.pendingUser = nil
	l.mu.Unlock()
	if len(queued) == 0 {
		return
	}
	for _, text := range queued {
		m := provider.Message{Role: provider.RoleUser, Content: text}
		l.messages = append(l.messages, m)
		l.persistMessage(m, nil, "")
	}
}

func (l *Loop) emit(ev Event)     { l.sink(ev) }
func (l *Loop) emitState(s State) { l.sink(Event{Kind: EventState, State: s}) }
func (l *Loop) emitPlan()         { l.sink(Event{Kind: EventPlanUpdated, Plan: l.engine.Plan()}) }

// toolSpecs converts the engine's registered tools into provider specs.
func toolSpecs(e *tools.Engine) []provider.ToolSpec {
	names := e.Names()
	out := make([]provider.ToolSpec, 0, len(names))
	for _, n := range names {
		t := e.Tool(n)
		out = append(out, provider.ToolSpec{Name: t.Name(), Description: t.Description(), JSONSchema: t.JSONSchema()})
	}
	return out
}

// rebuildMessages reconstructs provider messages from persisted turns.
func rebuildMessages(turns []store.Turn) []provider.Message {
	var out []provider.Message
	for _, t := range turns {
		switch t.Role {
		case provider.RoleTool:
			out = append(out, provider.Message{
				Role: provider.RoleTool, Content: t.Content, ToolCallID: t.ToolCallID, Name: t.ToolName,
			})
		default:
			m := provider.Message{Role: t.Role, Content: t.Content}
			if t.ToolCalls != "" {
				_ = json.Unmarshal([]byte(t.ToolCalls), &m.ToolCalls)
			}
			out = append(out, m)
		}
	}
	return out
}

// isTransientNet reports whether a non-StatusError is likely transient (a
// network-level failure worth one retry). Matching is deliberately precise: a
// bare "timeout" substring would false-positive on tool args echoed inside a
// server error message (e.g. a JSON "timeout_sec" field), which is not
// transient and must fail fast rather than burn retries.
func isTransientNet(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "connection refused") ||
		strings.Contains(s, "i/o timeout") ||
		strings.Contains(s, "context deadline exceeded") ||
		strings.Contains(s, "Client.Timeout") ||
		strings.Contains(s, "timeout awaiting") ||
		strings.Contains(s, "EOF")
}
