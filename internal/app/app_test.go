package app

import (
	"strings"
	"testing"
	"time"

	"sutracoder/internal/agent"
	"sutracoder/internal/config"
)

// newTestApp builds an App wired to an unreachable provider so no test ever
// depends on a live proxy.
func newTestApp(t *testing.T) *App {
	t.Helper()
	cfg := config.Defaults()
	cfg.Provider = config.ProviderMarga
	// Port 9 (discard) is never listening, so the health preflight fails fast.
	cfg.BaseURL = "http://127.0.0.1:9/v1"
	return New(cfg, nil, nil, "system prompt")
}

func TestHelpCommand(t *testing.T) {
	a := newTestApp(t)
	out, quit := a.HandleCommand("/help")
	if quit {
		t.Fatal("/help should not quit")
	}
	for _, want := range []string{"/connect", "/model", "/resume", "/sessions"} {
		if !strings.Contains(out, want) {
			t.Errorf("/help missing %q", want)
		}
	}
}

func TestUnknownCommand(t *testing.T) {
	a := newTestApp(t)
	out, _ := a.HandleCommand("/bogus")
	if !strings.Contains(out, "unknown command") {
		t.Errorf("want unknown-command hint, got %q", out)
	}
}

func TestQuitCommand(t *testing.T) {
	a := newTestApp(t)
	for _, c := range []string{"/quit", "/exit"} {
		if _, quit := a.HandleCommand(c); !quit {
			t.Errorf("%s should signal quit", c)
		}
	}
}

func TestStatusCommand(t *testing.T) {
	a := newTestApp(t)
	out, _ := a.HandleCommand("/status")
	for _, want := range []string{"provider", "marga", "mode", "autonomous", "workspace", "state"} {
		if !strings.Contains(out, want) {
			t.Errorf("/status missing %q in %q", want, out)
		}
	}
}

func TestModeSwitchWhenIdle(t *testing.T) {
	a := newTestApp(t)
	out, _ := a.HandleCommand("/mode supervised")
	if !strings.Contains(out, "supervised") {
		t.Fatalf("got %q", out)
	}
	if a.Mode() != "supervised" {
		t.Errorf("mode = %q, want supervised", a.Mode())
	}
	if !a.engine.Supervised {
		t.Error("engine should be supervised after switch")
	}
}

func TestModeRejectsUnknown(t *testing.T) {
	a := newTestApp(t)
	out, _ := a.HandleCommand("/mode sideways")
	if !strings.Contains(out, "usage") {
		t.Errorf("got %q", out)
	}
	if a.Mode() == "sideways" {
		t.Error("mode should be unchanged")
	}
}

func TestModelSwitchWhenIdle(t *testing.T) {
	a := newTestApp(t)
	out, _ := a.HandleCommand("/model gpt-4o-mini")
	if !strings.Contains(out, "gpt-4o-mini") {
		t.Errorf("got %q", out)
	}
	if a.Model() != "gpt-4o-mini" {
		t.Errorf("model = %q", a.Model())
	}
}

func TestAuthReportsMissingKeyWithoutLeaking(t *testing.T) {
	t.Setenv("MARGA_AUTH_TOKEN", "")
	a := newTestApp(t)
	out, _ := a.HandleCommand("/auth")
	if !strings.Contains(out, "MARGA_AUTH_TOKEN") {
		t.Errorf("auth should name the env var, got %q", out)
	}
	if strings.Contains(out, "fake-secret-value") {
		t.Error("auth must never print a credential value")
	}
}

// TestSubmitWithoutProviderEmitsConnectionFailure verifies that submitting a
// goal with an unreachable provider surfaces a connection error over the event
// bus instead of crashing or hanging.
func TestSubmitWithoutProviderEmitsConnectionFailure(t *testing.T) {
	a := newTestApp(t)
	a.Submit("do something")

	deadline := time.After(5 * time.Second)
	for {
		select {
		case ev := <-a.Events():
			if ev.Kind == agent.EventSystem && strings.Contains(ev.Text, "connection failed") {
				if a.Connected() {
					t.Error("should not report connected")
				}
				return
			}
		case <-deadline:
			t.Fatal("timed out waiting for connection-failure event")
		}
	}
}
