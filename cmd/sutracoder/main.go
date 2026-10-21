// Command sutracoder is an interactive, autonomous agentic CLI coding harness.
// It launches straight into a TUI and runs a Plan-Act-Observe-Correct loop
// against an LLM provider (default: the marga proxy) to explore, edit, and test
// code within a workspace under hard bounds. Providers connect lazily via
// /connect, so the tool always starts even when no proxy is up.
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"sutracoder/internal/app"
	"sutracoder/internal/config"
	"sutracoder/internal/diag"
	"sutracoder/internal/prompt"
	"sutracoder/internal/store"
	"sutracoder/internal/tui"
)

// version is the CLI version, stamped by goreleaser.
var version = "dev"

// author is credited on the startup splash.
const author = "vgana"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "sutracoder:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		flagConfig     = flag.String("config", "", "path to config.toml (default ~/.config/sutracoder/config.toml)")
		flagProvider   = flag.String("provider", "", "override provider: marga, openai, anthropic, ollama")
		flagModel      = flag.String("model", "", "override model name")
		flagBaseURL    = flag.String("base-url", "", "override provider base URL")
		flagMode       = flag.String("mode", "", "autonomous or supervised")
		flagWorkspace  = flag.String("workspace", "", "workspace root (default: config or cwd)")
		flagResume     = flag.String("resume", "", "resume a session by ID")
		flagSessions   = flag.Bool("sessions", false, "list recent sessions and exit")
		flagSystemPath = flag.String("system-prompt", "", "path to a system prompt file (overrides the built-in)")
		flagVersion    = flag.Bool("version", false, "print version and exit")
	)
	flag.Parse()

	if *flagVersion {
		fmt.Println("sutracoder", version)
		return nil
	}

	cfg, err := config.Load(*flagConfig)
	if err != nil {
		return err
	}
	// Flags override config.
	if *flagProvider != "" {
		cfg.Provider = *flagProvider
	}
	if *flagModel != "" {
		cfg.Model = *flagModel
	}
	if *flagBaseURL != "" {
		cfg.BaseURL = *flagBaseURL
	}
	if *flagMode != "" {
		cfg.Mode = *flagMode
	}
	if *flagWorkspace != "" {
		cfg.Workspace = *flagWorkspace
	}
	if err := cfg.Validate(); err != nil {
		return err
	}

	log := diag.NewFileLogger(cfg.LogPath())
	log.Info("starting sutracoder", "version", version, "provider", cfg.Provider, "mode", cfg.Mode)

	st, err := store.Open(cfg.DBPath(), cfg.InteractionsDir())
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	if *flagSessions {
		return printSessions(st)
	}

	sysPrompt, err := loadSystemPrompt(*flagSystemPath)
	if err != nil {
		return err
	}

	// The controller owns the provider connection (lazy), engine, store, and the
	// event bus. It never requires connectivity to start.
	controller := app.New(cfg, log, st, sysPrompt)

	p := tea.NewProgram(tui.New(tui.Options{
		Controller: controller,
		Events:     controller.Events(),
		Version:    version,
		Author:     author,
	}), tea.WithAltScreen())

	// Seed an initial goal or resume before the TUI blocks; runs are asynchronous
	// and their events buffer until the TUI starts consuming them.
	if *flagResume != "" {
		controller.Resume(*flagResume)
	} else if goal := strings.TrimSpace(strings.Join(flag.Args(), " ")); goal != "" {
		controller.Submit(goal)
	}

	if _, err := p.Run(); err != nil {
		log.Error("tui exited", "err", err)
	}
	controller.Shutdown()
	return nil
}

func loadSystemPrompt(path string) (string, error) {
	if path == "" {
		return prompt.DefaultSystem, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read system prompt: %w", err)
	}
	return string(b), nil
}

func printSessions(st *store.Store) error {
	sessions, err := st.ListSessions(30)
	if err != nil {
		return err
	}
	if len(sessions) == 0 {
		fmt.Println("no sessions yet")
		return nil
	}
	fmt.Printf("%-10s  %-12s  %-20s  %s\n", "ID", "STATUS", "CREATED", "GOAL")
	for _, s := range sessions {
		goal := s.Goal
		if len(goal) > 40 {
			goal = goal[:40] + "…"
		}
		fmt.Printf("%-10s  %-12s  %-20s  %s\n", s.ID, s.Status, s.CreatedAt.Format("2006-01-02 15:04"), goal)
	}
	return nil
}
