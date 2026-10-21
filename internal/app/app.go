// Package app hosts Sutracoder's interactive controller: it owns the resolved
// configuration, the (lazily-established) provider connection, the tool engine,
// the store, and the event bus, and exposes an opencode-style slash-command
// surface plus run control to the TUI. Provider connectivity is never required
// to start — the TUI always launches and connects on demand.
package app

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"sutracoder/internal/agent"
	"sutracoder/internal/config"
	"sutracoder/internal/provider"
	"sutracoder/internal/store"
	"sutracoder/internal/tools"
	"sutracoder/internal/vault"
)

// App is the interactive controller shared between main and the TUI. All
// exported methods are safe for concurrent use: the TUI calls them from its
// update loop while agent runs execute on background goroutines.
type App struct {
	mu        sync.Mutex
	baseCtx   context.Context
	cancel    context.CancelFunc
	cfg       config.Config
	log       *slog.Logger
	store     *store.Store
	engine    *tools.Engine
	sysPrompt string

	prov    provider.Provider
	loop    *agent.Loop
	events  chan agent.Event
	running bool
	runStop context.CancelFunc
}

// New builds the App, wiring the tool engine, the event bus, and the approval
// gate. It does not connect to any provider — that happens on first submit or
// via /connect.
func New(cfg config.Config, log *slog.Logger, st *store.Store, sysPrompt string) *App {
	events := make(chan agent.Event, 256)

	engine := tools.NewEngine(cfg.Workspace)
	engine.MaxOutputBytes = cfg.MaxToolOutputBytes
	engine.ShellTimeout = time.Duration(cfg.ShellTimeoutSec) * time.Second
	engine.Supervised = cfg.Mode == "supervised"
	engine.Approver = approvalFunc(events)

	baseCtx, cancel := context.WithCancel(context.Background())
	if log == nil {
		log = slog.Default()
	}
	return &App{
		baseCtx:   baseCtx,
		cancel:    cancel,
		cfg:       cfg,
		log:       log,
		store:     st,
		engine:    engine,
		sysPrompt: sysPrompt,
		events:    events,
	}
}

// Events returns the read side of the event bus the TUI renders from.
func (a *App) Events() <-chan agent.Event { return a.events }

// emitSystem posts a feedback line (slash-command output, runtime notices) to
// the log panel via the event bus.
func (a *App) emitSystem(format string, args ...any) {
	a.events <- agent.Event{Kind: agent.EventSystem, Text: fmt.Sprintf(format, args...)}
}

// approvalFunc returns a tools.Approver that routes a confirmation request to
// the UI over the event bus and blocks until the user answers. It only touches
// the shared channel, never the model, so it is race-free.
func approvalFunc(events chan agent.Event) tools.Approver {
	return func(call tools.Call) bool {
		reply := make(chan bool, 1)
		events <- agent.Event{Kind: agent.EventApproval, Approval: &agent.Approval{Call: call, Reply: reply}}
		return <-reply
	}
}

// ---- status accessors (read by the TUI status bar) ----

// Provider returns the selected provider name.
func (a *App) Provider() string { a.mu.Lock(); defer a.mu.Unlock(); return a.cfg.Provider }

// Model returns the configured model (empty means provider default).
func (a *App) Model() string { a.mu.Lock(); defer a.mu.Unlock(); return a.cfg.Model }

// Mode returns "autonomous" or "supervised".
func (a *App) Mode() string { a.mu.Lock(); defer a.mu.Unlock(); return a.cfg.Mode }

// Workspace returns the absolute workspace root.
func (a *App) Workspace() string { a.mu.Lock(); defer a.mu.Unlock(); return a.engine.Root }

// Connected reports whether a provider handle is established.
func (a *App) Connected() bool { a.mu.Lock(); defer a.mu.Unlock(); return a.prov != nil }

// Running reports whether a background run is in flight.
func (a *App) Running() bool { a.mu.Lock(); defer a.mu.Unlock(); return a.running }

// SessionID returns the active session id ("" before the first goal).
func (a *App) SessionID() string {
	a.mu.Lock()
	loop := a.loop
	a.mu.Unlock()
	if loop == nil {
		return ""
	}
	return loop.SessionID()
}

// Tokens returns cumulative tokens for the active session.
func (a *App) Tokens() int {
	a.mu.Lock()
	loop := a.loop
	a.mu.Unlock()
	if loop == nil {
		return 0
	}
	return loop.TokensUsed()
}

// Turn returns the completed provider-call count for the active session.
func (a *App) Turn() int {
	a.mu.Lock()
	loop := a.loop
	a.mu.Unlock()
	if loop == nil {
		return 0
	}
	return loop.Turn()
}

// ---- run control ----

// Submit interprets input from the TUI: it interjects into a running loop,
// continues an existing session when idle, or starts a fresh session (connecting
// a provider first if needed). It returns immediately; work runs in background.
func (a *App) Submit(text string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}

	a.mu.Lock()
	if a.running {
		loop := a.loop
		a.mu.Unlock()
		if loop != nil {
			loop.Inject(text)
			a.emitSystem("[queued] input will be delivered at the next turn boundary")
		}
		return
	}
	var cont *agent.Loop
	if a.loop != nil && a.loop.Active() {
		cont = a.loop
	}
	a.mu.Unlock()

	if cont != nil {
		loop := cont
		a.runBackground(func(ctx context.Context) error {
			return loop.Continue(ctx, text)
		})
		return
	}

	goal := text
	a.runBackground(func(ctx context.Context) error {
		if err := a.ensureConnected(ctx); err != nil {
			a.emitSystem("✗ connection failed: %v", err)
			return nil // already reported; avoid a duplicate error line
		}
		a.mu.Lock()
		loop := agent.New(a.cfg, a.prov, a.engine, a.store, a.log, func(ev agent.Event) {
			a.events <- ev
		})
		a.loop = loop
		a.mu.Unlock()
		return loop.Start(ctx, a.sysPrompt, goal)
	})
}

// Resume loads a prior session by id and continues it in the background.
func (a *App) Resume(id string) {
	id = strings.TrimSpace(id)
	if id == "" {
		a.emitSystem("usage: /resume <session-id>  (see /sessions)")
		return
	}
	a.runBackground(func(ctx context.Context) error {
		if err := a.ensureConnected(ctx); err != nil {
			a.emitSystem("✗ connection failed: %v", err)
			return nil
		}
		a.mu.Lock()
		loop := agent.New(a.cfg, a.prov, a.engine, a.store, a.log, func(ev agent.Event) {
			a.events <- ev
		})
		a.loop = loop
		a.mu.Unlock()
		return loop.Resume(ctx, id)
	})
}

// Stop cancels the in-flight run, if any. The session remains resumable.
func (a *App) Stop() {
	a.mu.Lock()
	stop := a.runStop
	a.mu.Unlock()
	if stop != nil {
		stop()
	}
}

// Shutdown cancels any run and releases the base context. Call after the TUI
// exits.
func (a *App) Shutdown() {
	a.Stop()
	if a.cancel != nil {
		a.cancel()
	}
}

// runBackground marks a run active, derives a cancellable context, and executes
// fn on a goroutine, clearing the running flag when it returns.
func (a *App) runBackground(fn func(ctx context.Context) error) {
	a.mu.Lock()
	if a.running {
		a.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(a.baseCtx)
	a.runStop = cancel
	a.running = true
	a.mu.Unlock()

	go func() {
		defer func() {
			a.mu.Lock()
			a.running = false
			if a.runStop != nil {
				a.runStop()
				a.runStop = nil
			}
			a.mu.Unlock()
		}()
		if err := fn(ctx); err != nil && !isCtxDone(err) {
			a.log.Warn("run ended", "err", err)
		}
	}()
}

// ensureConnected establishes the provider if it is not already up.
func (a *App) ensureConnected(ctx context.Context) error {
	a.mu.Lock()
	if a.prov != nil {
		a.mu.Unlock()
		return nil
	}
	cfg := a.cfg
	a.mu.Unlock()

	prov, err := buildProvider(ctx, cfg)
	if err != nil {
		return err
	}
	a.mu.Lock()
	a.prov = prov
	a.mu.Unlock()
	return nil
}

// connectProvider switches to the named provider and connects, updating the live
// loop's handle when one exists.
func (a *App) connectProvider(ctx context.Context, name string) error {
	a.mu.Lock()
	cfg := a.cfg
	cfg.Provider = name
	// A base_url override was meant for the original provider; drop it on switch
	// unless it still points at the same scheme family (leave as-is: explicit
	// --base-url wins). We only reset the provider field here.
	a.mu.Unlock()

	prov, err := buildProvider(ctx, cfg)
	if err != nil {
		return err
	}
	a.mu.Lock()
	a.prov = prov
	a.cfg.Provider = name
	loop := a.loop
	a.mu.Unlock()
	if loop != nil {
		loop.SetProvider(prov)
	}
	return nil
}

// ---- slash commands ----

// HandleCommand executes one slash command. It returns synchronous feedback to
// append to the log (empty for async commands that report via the event bus) and
// a quit flag for /quit and /exit.
func (a *App) HandleCommand(line string) (string, bool) {
	fields := strings.Fields(strings.TrimSpace(line))
	if len(fields) == 0 {
		return "", false
	}
	cmd := strings.ToLower(fields[0])
	args := fields[1:]

	switch cmd {
	case "/help", "/h", "/?":
		return helpText, false
	case "/status":
		return a.statusText(), false
	case "/connect", "/provider":
		return a.cmdConnect(args)
	case "/model":
		return a.cmdModel(args)
	case "/mode":
		return a.cmdMode(args)
	case "/auth":
		return a.cmdAuth(), false
	case "/workspace":
		return a.cmdWorkspace(args)
	case "/sessions":
		return a.cmdSessions(), false
	case "/resume":
		if len(args) == 0 {
			return "usage: /resume <session-id>  (see /sessions)", false
		}
		a.Resume(args[0])
		return "resuming session " + args[0] + "…", false
	case "/new":
		return a.cmdNew(), false
	case "/stop":
		if a.Running() {
			a.Stop()
			return "stopping…", false
		}
		return "nothing is running", false
	case "/quit", "/exit":
		return "bye", true
	default:
		return fmt.Sprintf("unknown command %s — try /help", cmd), false
	}
}

// cmdConnect establishes or switches the provider asynchronously and reports the
// outcome through the event bus so the TUI never blocks on the network.
func (a *App) cmdConnect(args []string) (string, bool) {
	name := a.Provider()
	if len(args) > 0 {
		name = strings.ToLower(args[0])
	}
	if err := validateProvider(name); err != nil {
		return err.Error(), false
	}
	go func() {
		a.emitSystem("connecting to %s…", name)
		if err := a.connectProvider(a.baseCtx, name); err != nil {
			a.emitSystem("✗ connect failed: %v", err)
			return
		}
		a.emitSystem("✓ connected to %s (model %s)", name, a.displayModel())
	}()
	return "", false
}

// cmdModel switches the model when idle.
func (a *App) cmdModel(args []string) (string, bool) {
	if len(args) == 0 {
		m := a.Model()
		if m == "" {
			return "model: " + a.Provider() + " default", false
		}
		return "model: " + m, false
	}
	if a.Running() {
		return "stop the current run (ctrl+s or /stop) before switching models", false
	}
	model := args[0]
	a.mu.Lock()
	a.cfg.Model = model
	loop := a.loop
	a.mu.Unlock()
	if loop != nil {
		loop.SetModel(model)
	}
	return "model set to " + model, false
}

// cmdMode switches between autonomous and supervised when idle.
func (a *App) cmdMode(args []string) (string, bool) {
	if len(args) == 0 {
		return "mode: " + a.Mode(), false
	}
	mode := strings.ToLower(args[0])
	if mode != "autonomous" && mode != "supervised" {
		return "usage: /mode <autonomous|supervised>", false
	}
	if a.Running() {
		return "stop the current run (ctrl+s or /stop) before switching modes", false
	}
	a.mu.Lock()
	a.cfg.Mode = mode
	a.engine.Supervised = mode == "supervised"
	loop := a.loop
	a.mu.Unlock()
	if loop != nil {
		loop.SetMode(mode)
	}
	return "mode set to " + mode, false
}

// cmdAuth reports whether the current provider's credential resolves, without
// ever printing the secret. Providers that need no key are reported as such.
func (a *App) cmdAuth() string {
	prov := a.Provider()
	envKey, ctxName := credentialNames(prov)
	if ctxName == "" {
		return "provider " + prov + " needs no credential"
	}
	if v := strings.TrimSpace(getenv(envKey)); v != "" {
		return fmt.Sprintf("✓ %s set (source: environment)", envKey)
	}
	ctx, cancel := context.WithTimeout(a.baseCtx, 5*time.Second)
	defer cancel()
	if k := vault.ResolveKey(ctx, envKey, ctxName); k != "" {
		return fmt.Sprintf("✓ %s resolved (source: vault context %q)", envKey, ctxName)
	}
	if prov == config.ProviderLlamaCPP {
		return "no " + envKey + " set — fine, the local llama.cpp server is unauthenticated by default"
	}
	return fmt.Sprintf("✗ %s not found in environment or vault — /connect will fail", envKey)
}

// cmdWorkspace shows or sets the workspace root when idle.
func (a *App) cmdWorkspace(args []string) (string, bool) {
	if len(args) == 0 {
		return "workspace: " + a.Workspace(), false
	}
	if a.Running() {
		return "stop the current run (ctrl+s or /stop) before changing the workspace", false
	}
	root := args[0]
	abs, err := absPath(root)
	if err != nil {
		return "invalid path: " + err.Error(), false
	}
	a.mu.Lock()
	a.cfg.Workspace = abs
	a.engine.Root = abs
	a.mu.Unlock()
	return "workspace set to " + abs, false
}

// cmdSessions lists recent sessions from the store.
func (a *App) cmdSessions() string {
	if a.store == nil {
		return "no session store available"
	}
	sessions, err := a.store.ListSessions(20)
	if err != nil {
		return "list sessions: " + err.Error()
	}
	if len(sessions) == 0 {
		return "no sessions yet — submit a goal to create one"
	}
	var b strings.Builder
	b.WriteString("recent sessions:\n")
	for _, s := range sessions {
		goal := s.Goal
		if len(goal) > 44 {
			goal = goal[:44] + "…"
		}
		fmt.Fprintf(&b, "  %-10s %-10s %s\n", s.ID, s.Status, goal)
	}
	b.WriteString("resume with: /resume <id>")
	return b.String()
}

// cmdNew discards the active session so the next submit starts fresh.
func (a *App) cmdNew() string {
	if a.Running() {
		return "stop the current run (ctrl+s or /stop) first"
	}
	a.mu.Lock()
	a.loop = nil
	a.mu.Unlock()
	return "started a new session — submit a goal"
}

// statusText renders the /status report.
func (a *App) statusText() string {
	conn := "not connected"
	if a.Connected() {
		conn = "connected"
	}
	state := "idle"
	if a.Running() {
		state = "running"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "provider  %s (%s)\n", a.Provider(), conn)
	fmt.Fprintf(&b, "model     %s\n", a.displayModel())
	fmt.Fprintf(&b, "mode      %s\n", a.Mode())
	fmt.Fprintf(&b, "workspace %s\n", a.Workspace())
	fmt.Fprintf(&b, "state     %s\n", state)
	fmt.Fprintf(&b, "session   %s  turn %d  tokens %d", a.SessionID(), a.Turn(), a.Tokens())
	return b.String()
}

// displayModel returns the model label, falling back to the provider default.
func (a *App) displayModel() string {
	if m := a.Model(); m != "" {
		return m
	}
	return a.Provider() + " default"
}

const helpText = `sutracoder commands
  /help              show this help
  /status            provider, model, mode, workspace, session
  /connect [prov]    connect (or switch to) a provider: llamacpp, marga, openai, anthropic, ollama
  /provider <name>   alias for /connect <name>
  /model <name>      switch the model (idle only)
  /mode <m>          autonomous | supervised (idle only)
  /auth              check whether the current credential resolves
  /workspace [path]  show or set the workspace root (idle only)
  /sessions          list recent sessions
  /resume <id>       continue a prior session
  /new               start a fresh session
  /stop              stop the current run
  /quit              exit (ctrl+c also works)

Default provider is llamacpp (the llama.cpp server at 127.0.0.1:8080).
Type a goal at the prompt to run; type follow-ups to continue the session.
ctrl+s interrupts a run; restricted actions prompt for approval in supervised mode.`

// ---- provider construction ----

// buildProvider constructs the configured provider, resolving credentials from
// the environment or the vault and preflighting marga's /health endpoint.
func buildProvider(ctx context.Context, cfg config.Config) (provider.Provider, error) {
	switch cfg.Provider {
	case config.ProviderLlamaCPP:
		base := cfg.BaseURL
		if base == "" {
			base = "http://127.0.0.1:8080/v1"
		}
		if err := pingHealth(ctx, strings.TrimSuffix(base, "/v1")); err != nil {
			return nil, fmt.Errorf("llama.cpp server unreachable at %s (%w); start llama-server on the Windows host or pass --provider", base, err)
		}
		// llama.cpp needs no key by default, but one can be set if the server
		// was started with --api-key. Resolve it opportunistically.
		key := vault.ResolveKey(ctx, "LLAMACPP_API_KEY", "llamacpp")
		return provider.NewOpenAI(provider.OpenAIConfig{Name: "llamacpp", BaseURL: base, APIKey: key}), nil
	case config.ProviderMarga:
		base := cfg.BaseURL
		if base == "" {
			base = "http://127.0.0.1:8010/v1"
		}
		if err := pingHealth(ctx, strings.TrimSuffix(base, "/v1")); err != nil {
			return nil, fmt.Errorf("marga proxy unreachable at %s (%w); start it, run /connect openai, or set a provider", base, err)
		}
		key := vault.ResolveKey(ctx, "MARGA_AUTH_TOKEN", "marga")
		return provider.NewOpenAI(provider.OpenAIConfig{Name: "marga", BaseURL: base, APIKey: key}), nil
	case config.ProviderOpenAI:
		base := cfg.BaseURL
		if base == "" {
			base = "https://api.openai.com/v1"
		}
		key := vault.ResolveKey(ctx, "OPENAI_API_KEY", "openai")
		if key == "" {
			return nil, fmt.Errorf("OPENAI_API_KEY not set and not found in vault")
		}
		return provider.NewOpenAI(provider.OpenAIConfig{Name: "openai", BaseURL: base, APIKey: key}), nil
	case config.ProviderOllama:
		base := cfg.BaseURL
		if base == "" {
			base = "http://127.0.0.1:11434/v1"
		}
		return provider.NewOpenAI(provider.OpenAIConfig{Name: "ollama", BaseURL: base}), nil
	case config.ProviderAnthropic:
		key := vault.ResolveKey(ctx, "ANTHROPIC_API_KEY", "anthropic")
		if key == "" {
			return nil, fmt.Errorf("ANTHROPIC_API_KEY not set and not found in vault")
		}
		return provider.NewAnthropic(provider.AnthropicConfig{APIKey: key, BaseURL: cfg.BaseURL, Model: cfg.Model}), nil
	default:
		return nil, fmt.Errorf("unknown provider %q", cfg.Provider)
	}
}

// pingHealth checks a provider's health endpoint so we fail fast when a proxy
// is down rather than silently falling back.
func pingHealth(ctx context.Context, base string) error {
	c := &http.Client{Timeout: 3 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(base, "/")+"/health", nil)
	if err != nil {
		return err
	}
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("health returned %d", resp.StatusCode)
	}
	return nil
}

// ---- small helpers ----

// validateProvider rejects an unknown provider name for /connect.
func validateProvider(name string) error {
	switch name {
	case config.ProviderLlamaCPP, config.ProviderMarga, config.ProviderOpenAI, config.ProviderAnthropic, config.ProviderOllama:
		return nil
	default:
		return fmt.Errorf("unknown provider %q (want llamacpp, marga, openai, anthropic, or ollama)", name)
	}
}

// credentialNames maps a provider to its env var and vault context. An empty
// vaultContext means the provider needs no credential (e.g. a local server).
func credentialNames(provider string) (envKey, vaultContext string) {
	switch provider {
	case config.ProviderLlamaCPP:
		return "LLAMACPP_API_KEY", "llamacpp"
	case config.ProviderMarga:
		return "MARGA_AUTH_TOKEN", "marga"
	case config.ProviderOpenAI:
		return "OPENAI_API_KEY", "openai"
	case config.ProviderAnthropic:
		return "ANTHROPIC_API_KEY", "anthropic"
	default:
		return "", ""
	}
}

// getenv reads an environment variable (kept as a seam for tests).
func getenv(key string) string { return os.Getenv(key) }

// absPath resolves a path to an absolute form for workspace confinement.
func absPath(p string) (string, error) { return filepath.Abs(p) }

func isCtxDone(err error) bool {
	return err != nil && (strings.Contains(err.Error(), "context canceled") ||
		strings.Contains(err.Error(), "context deadline"))
}
