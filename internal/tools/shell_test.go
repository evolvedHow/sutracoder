package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestDenylistDetection(t *testing.T) {
	dangerous := []string{
		"sudo rm -rf /",
		"rm -rf /",
		"rm -fr ~",
		"mkfs.ext4 /dev/sda1",
		"git push --force origin main",
		"git push -f",
		"shutdown now",
		"reboot",
		"dd if=/dev/zero of=/dev/sda",
		":(){:|:&};:",
		"chmod -R 777 /",
	}
	for _, cmd := range dangerous {
		if !DenylistHit(cmd) {
			t.Errorf("expected denylist hit for %q", cmd)
		}
	}
	safe := []string{
		"go test ./...",
		"ls -la",
		"rm tempfile.txt",
		"git push origin main",
		"echo hello",
	}
	for _, cmd := range safe {
		if DenylistHit(cmd) {
			t.Errorf("unexpected denylist hit for %q", cmd)
		}
	}
}

func TestShellSuccessAndFailure(t *testing.T) {
	e := newTestEngine(t)
	res := mustRun(t, e, "execute_shell", map[string]any{"command": "echo hello"})
	if res.Status != StatusSuccess || !strings.Contains(res.Stdout, "hello") {
		t.Fatalf("shell success: %+v", res)
	}
	if res.ReturnCode == nil || *res.ReturnCode != 0 {
		t.Fatalf("expected exit 0, got %+v", res.ReturnCode)
	}

	res = mustRun(t, e, "execute_shell", map[string]any{"command": "exit 3"})
	if res.Status != StatusFailure || res.ReturnCode == nil || *res.ReturnCode != 3 {
		t.Fatalf("shell failure: %+v", res)
	}
}

func TestShellApprovalGateOnDenylist(t *testing.T) {
	e := newTestEngine(t)
	// Approver that always declines.
	e.Approver = func(Call) bool { return false }
	res := mustRun(t, e, "execute_shell", map[string]any{"command": "sudo rm -rf /"})
	if res.Status != StatusFailure || !strings.Contains(res.Stderr, "declined") {
		t.Fatalf("expected declined denylist command, got %+v", res)
	}
}

func TestShellApprovalAllowed(t *testing.T) {
	e := newTestEngine(t)
	approved := false
	e.Approver = func(Call) bool { approved = true; return true }
	// git push -f is denylisted (triggers the gate) and harmless to attempt in
	// a non-repo temp dir. We assert the gate opened, not that git succeeded.
	res := mustRun(t, e, "execute_shell", map[string]any{"command": "git push -f"})
	if !approved {
		t.Fatal("expected approver to be consulted and allow the call")
	}
	if strings.Contains(res.Stderr, "declined") {
		t.Fatalf("expected gate to open, got %+v", res)
	}
}

func TestSupervisedGatesEdits(t *testing.T) {
	e := newTestEngine(t)
	e.Supervised = true
	declined := false
	e.Approver = func(Call) bool { declined = true; return false }
	res := mustRun(t, e, "write_file", map[string]any{"path": "f.txt", "content": "x"})
	if res.Status != StatusFailure || !declined {
		t.Fatalf("expected supervised decline on write, got %+v declined=%v", res, declined)
	}
}

func TestShellTimeout(t *testing.T) {
	e := newTestEngine(t)
	e.ShellTimeout = 0 // immediate deadline via zero timeout -> use tiny
	// Use timeout_sec arg of 1 with a sleeping command.
	res := mustRun(t, e, "execute_shell", map[string]any{"command": "sleep 5", "timeout_sec": 1})
	if res.Status != StatusFailure || !strings.Contains(res.Stderr, "timed out") {
		t.Fatalf("expected timeout failure, got %+v", res)
	}
}

func TestMalformedArgs(t *testing.T) {
	e := newTestEngine(t)
	res := e.Run(context.Background(), "execute_shell", json.RawMessage("{not json"))
	if res.Status != StatusFailure {
		t.Fatalf("expected failure on malformed args, got %+v", res)
	}
}
