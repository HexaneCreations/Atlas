package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestSecondInstanceRefusesToStart is an end-to-end check of the
// single-instance lock (internal/platform/lock) as actually wired into the
// binary's startup path: build the real atlas-agent binary, start one
// instance against a fresh data directory, then start a second instance
// against the *same* data directory and confirm it exits immediately with a
// clear error instead of proceeding to bootstrap/enroll.
//
// The control plane URL is intentionally unreachable — bootstrap is
// expected to retry in the background for the first instance; what this
// test verifies is that the second instance never gets that far at all.
func TestSecondInstanceRefusesToStart(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a real binary; skipped in -short")
	}

	binary := filepath.Join(t.TempDir(), "atlas-agent-under-test")
	build := exec.Command("go", "build", "-o", binary, ".")
	build.Dir = "."
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build atlas-agent: %v\n%s", err, out)
	}

	dataDir := t.TempDir()
	env := append(os.Environ(),
		"ATLAS_AGENT_DATA_DIR="+dataDir,
		"ATLAS_AGENT_CONTROL_PLANE_URL=https://127.0.0.1:1",
		"ATLAS_AGENT_TOKEN=test-token",
	)

	first := exec.Command(binary)
	first.Env = env
	if err := first.Start(); err != nil {
		t.Fatalf("start first instance: %v", err)
	}
	defer func() {
		first.Process.Kill()
		first.Wait()
	}()

	// Give the first instance time to acquire the lock (well before it
	// could exhaust its own 5-minute bootstrap retry budget).
	deadline := time.Now().Add(10 * time.Second)
	lockPath := filepath.Join(dataDir, "atlas-agent.lock")
	for {
		if _, err := os.Stat(lockPath); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("lock file %s never appeared", lockPath)
		}
		time.Sleep(50 * time.Millisecond)
	}

	var stderr bytes.Buffer
	second := exec.Command(binary)
	second.Env = env
	second.Stderr = &stderr
	err := second.Run()

	if err == nil {
		t.Fatal("second instance: expected non-zero exit, got success")
	}
	exitErr, ok := err.(*exec.ExitError)
	if !ok {
		t.Fatalf("second instance: expected *exec.ExitError, got %T (%v)", err, err)
	}
	if exitErr.ExitCode() != 1 {
		t.Errorf("second instance: exit code = %d, want 1", exitErr.ExitCode())
	}

	msg := stderr.String()
	for _, want := range []string{"already running", "pid:", "data_dir: " + dataDir} {
		if !strings.Contains(msg, want) {
			t.Errorf("second instance stderr missing %q; got: %s", want, msg)
		}
	}
}
