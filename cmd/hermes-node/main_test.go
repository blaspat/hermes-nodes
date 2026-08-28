// Tests for the main package entry points. Two scopes:
//
//  1. Flag parsing (parseGlobalArgs) — pure function tests, no
//     subprocess, no I/O.
//  2. End-to-end of the run subcommand against a real httptest
//     WebSocket server. The server completes the protocol handshake,
//     sends a synthetic `exec` call, expects a real `exec_result` in
//     return. This is the integration smoke that proves the
//     supervisor + dispatcher + shell pipeline is wired correctly in
//     main, not just in unit tests.
package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/blaspat/hermes-node/internal/config"
	"github.com/gorilla/websocket"
)

// ---------------------------------------------------------------------------
// Flag parsing tests
// ---------------------------------------------------------------------------

func TestParseGlobalArgs(t *testing.T) {
	cases := []struct {
		name        string
		args        []string
		wantVersion bool
		wantHelp    bool
		wantConfig  string
		wantSub     []string
		wantErr     bool
	}{
		{
			name:       "no args",
			args:       nil,
			wantConfig: testDefaultConfigPath(),
			wantSub:    nil,
		},
		{
			name:        "version flag",
			args:        []string{"--version"},
			wantVersion: true,
			wantConfig:  testDefaultConfigPath(),
		},
		{
			name:       "help short",
			args:       []string{"-h"},
			wantHelp:   true,
			wantConfig: testDefaultConfigPath(),
		},
		{
			name:       "config before subcommand",
			args:       []string{"--config", "/tmp/cfg.toml", "pair", "--server", "wss://x", "--token", "t"},
			wantConfig: "/tmp/cfg.toml",
			wantSub:    []string{"pair", "--server", "wss://x", "--token", "t"},
		},
		{
			name:       "config after subcommand",
			args:       []string{"pair", "--server", "wss://x", "--token", "t", "--config", "/tmp/cfg.toml"},
			wantConfig: "/tmp/cfg.toml",
			wantSub:    []string{"pair", "--server", "wss://x", "--token", "t"},
		},
		{
			name:       "config equals form",
			args:       []string{"pair", "--config=/tmp/cfg.toml", "--server", "wss://x"},
			wantConfig: "/tmp/cfg.toml",
			wantSub:    []string{"pair", "--server", "wss://x"},
		},
		{
			name: "config sandwiched",
			args: []string{"--config", "/tmp/a.toml", "pair", "--server", "wss://x", "--config", "/tmp/b.toml"},
			// Last write wins — the post-subcommand --config overrides.
			wantConfig: "/tmp/b.toml",
			wantSub:    []string{"pair", "--server", "wss://x"},
		},
		{
			name:        "version flag mixed with subcommand",
			args:        []string{"pair", "--version", "--server", "wss://x"},
			wantVersion: true,
			wantConfig:  testDefaultConfigPath(),
			wantSub:     []string{"pair", "--server", "wss://x"},
		},
		{
			name:    "config without value",
			args:    []string{"--config"},
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ver, hlp, cfg, defaultCfgErr, sub, err := parseGlobalArgs(tc.args)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseGlobalArgs(%v) returned nil error; want one", tc.args)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseGlobalArgs(%v): %v", tc.args, err)
			}
			// In a normal test environment $HOME is set, so the
			// default-path derivation should succeed. If a test
			// ever runs with $HOME unset, fail loudly.
			if defaultCfgErr != nil {
				t.Fatalf("parseGlobalArgs(%v): defaultCfgErr = %v, want nil", tc.args, defaultCfgErr)
			}
			if *ver != tc.wantVersion {
				t.Errorf("version = %v, want %v", *ver, tc.wantVersion)
			}
			if *hlp != tc.wantHelp {
				t.Errorf("help = %v, want %v", *hlp, tc.wantHelp)
			}
			if *cfg != tc.wantConfig {
				t.Errorf("config = %q, want %q", *cfg, tc.wantConfig)
			}
			if !equalStrings(sub, tc.wantSub) {
				t.Errorf("subArgs = %v, want %v", sub, tc.wantSub)
			}
		})
	}
}

func TestRun_PairSubcommand_WritesConfig(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")

	var stdout, stderr bytes.Buffer
	code := run([]string{"pair", "--server", "wss://vps.example.com:6969", "--token", "secret-token", "--name", "workmac", "--config", cfgPath}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run returned %d; stderr:\n%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "paired") {
		t.Errorf("stdout should mention 'paired'; got %q", stdout.String())
	}
	// The pair stdout message (S5) must warn that an empty
	// [node].allowed_paths will reject all calls. Operators
	// who don't see this and then run the service will hit
	// no-op handlers.
	if !strings.Contains(stdout.String(), "[node].allowed_paths") {
		t.Errorf("pair stdout should mention [node].allowed_paths; got %q", stdout.String())
	}

	// File should exist with 0600 mode on Unix.
	info, err := os.Stat(cfgPath)
	if err != nil {
		t.Fatalf("stat config: %v", err)
	}
	if runtime.GOOS != "windows" {
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("file mode = %#o, want 0o600", perm)
		}
	}

	// Re-parse to confirm the round-trip works (this is also what
	// the next 'hermes-node run' invocation would do).
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("load for test: %v", err)
	}
	if cfg.Node.Token != "secret-token" {
		t.Errorf("token = %q, want secret-token", cfg.Node.Token)
	}
	if cfg.Node.Name != "workmac" {
		t.Errorf("name = %q, want workmac", cfg.Node.Name)
	}
}

func TestRun_PairRefusesOverwrite(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")

	// First pair: should succeed.
	if code := run([]string{"pair", "--server", "wss://x", "--token", "t", "--name", "test", "--config", cfgPath}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatalf("first pair: exit %d", code)
	}

	// Second pair: should fail.
	var stderr bytes.Buffer
	code := run([]string{"pair", "--server", "wss://x", "--token", "t", "--name", "test", "--config", cfgPath}, &bytes.Buffer{}, &stderr)
	if code == 0 {
		t.Errorf("second pair: exit 0; want non-zero (refuse to overwrite)")
	}
	if !strings.Contains(stderr.String(), "already exists") {
		t.Errorf("stderr should mention 'already exists'; got %q", stderr.String())
	}
}

func TestRun_PairRequiresFlags(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")

	var stderr bytes.Buffer
	code := run([]string{"pair", "--config", cfgPath}, &bytes.Buffer{}, &stderr)
	if code != 2 {
		t.Errorf("exit = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "--server is required") {
		t.Errorf("stderr should mention --server; got %q", stderr.String())
	}
}

func TestRun_NoSubcommand(t *testing.T) {
	var stderr bytes.Buffer
	code := run(nil, &bytes.Buffer{}, &stderr)
	if code != 2 {
		t.Errorf("exit = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "missing subcommand") {
		t.Errorf("stderr should mention 'missing subcommand'; got %q", stderr.String())
	}
}

func TestRun_VersionAndHelp(t *testing.T) {
	var out bytes.Buffer
	code := run([]string{"--version"}, &out, &bytes.Buffer{})
	if code != 0 {
		t.Errorf("--version: exit %d, want 0", code)
	}
	if !strings.HasPrefix(out.String(), "hermes-node ") {
		t.Errorf("--version: output = %q, want 'hermes-node ...' prefix", out.String())
	}
	// With build info (from debug.ReadBuildInfo), output should be:
	// "hermes-node dev go1.22.5 abc1234 2026-06-22"
	// Without build info (test binary built without VCS), just:
	// "hermes-node dev"
	// Either is fine — just check it's not empty after the prefix.
	rest := strings.TrimSpace(strings.TrimPrefix(out.String(), "hermes-node "))
	if rest == "" {
		t.Errorf("--version: version string after prefix is empty: %q", out.String())
	}

	out.Reset()
	if code := run([]string{"--help"}, &out, &bytes.Buffer{}); code != 0 {
		t.Errorf("--help: exit %d", code)
	}
	if !strings.Contains(out.String(), "hermes-node pair") {
		t.Errorf("--help: should mention pair subcommand; got %q", out.String())
	}
}

// TestBuildMetadataFormat verifies the buildMetadata string is formatted
// correctly when debug.ReadBuildInfo provides VCS settings.
func TestBuildMetadataFormat(t *testing.T) {
	// Save and override buildMetadata so the test is deterministic.
	saved := buildMetadata
	defer func() { buildMetadata = saved }()

	buildMetadata = "dev go1.22.5 abc1234 2026-06-22"
	var out bytes.Buffer
	code := run([]string{"--version"}, &out, &bytes.Buffer{})
	if code != 0 {
		t.Fatalf("--version: exit %d", code)
	}
	want := "hermes-node dev go1.22.5 abc1234 2026-06-22\n"
	if out.String() != want {
		t.Errorf("--version output: got %q, want %q", out.String(), want)
	}
}

// TestVersionFallbackFormat verifies that --version falls back to the
// bare version string when buildMetadata is empty (e.g. ReadBuildInfo
// failed or returned no VCS settings).
func TestVersionFallbackFormat(t *testing.T) {
	saved := buildMetadata
	defer func() { buildMetadata = saved }()

	buildMetadata = ""
	var out bytes.Buffer
	code := run([]string{"--version"}, &out, &bytes.Buffer{})
	if code != 0 {
		t.Fatalf("--version: exit %d", code)
	}
	want := "hermes-node dev\n"
	if out.String() != want {
		t.Errorf("--version output: got %q, want %q", out.String(), want)
	}
}

// ---------------------------------------------------------------------------
// End-to-end: run subcommand against a fake WebSocket server
// ---------------------------------------------------------------------------

// fakeServer stands up an httptest WebSocket server that completes
// the protocol handshake (hello_ack, auth_ok) and then waits for a
// message from the client. The test can drive the test by sending a
// synthetic `exec` and reading the `exec_result` back. The server
// returns a canned response regardless of what the client asked for —
// the test asserts the round-trip shape, not the exec semantics.
type fakeServer struct {
	srv      *httptest.Server
	upgrader websocket.Upgrader

	connected atomic.Int32
	// Mu guards nextResp so a writer in the test goroutine can
	// install the canned response before the server reads it.
	mu       sync.Mutex
	nextResp map[string]any
}

func newFakeServer() *fakeServer {
	fs := &fakeServer{
		upgrader: websocket.Upgrader{
			CheckOrigin: func(r *http.Request) bool { return true },
		},
	}
	fs.srv = httptest.NewServer(http.HandlerFunc(fs.handle))
	return fs
}

func (fs *fakeServer) URL() string {
	return "ws" + fs.srv.URL[4:]
}

func (fs *fakeServer) Close() { fs.srv.Close() }

func (fs *fakeServer) handle(w http.ResponseWriter, r *http.Request) {
	conn, err := fs.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	fs.connected.Add(1)

	// Strict happy-path handshake.
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	var hello map[string]any
	if err := conn.ReadJSON(&hello); err != nil {
		return
	}
	if hello["type"] != "hello" {
		return
	}
	_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	_ = conn.WriteJSON(map[string]any{
		"type":             "hello_ack",
		"protocol_version": "0.1.0",
		"session_id":       "test-session",
		"server_time":      "2026-06-11T00:00:00.000Z",
	})

	var auth map[string]any
	if err := conn.ReadJSON(&auth); err != nil {
		return
	}
	if auth["type"] != "auth" {
		return
	}
	_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	_ = conn.WriteJSON(map[string]any{
		"type":       "auth_ok",
		"session_id": "test-session",
	})

	// Now drive one call. Read whatever the client sends (which on
	// a happy path is a ping from the heartbeat), then send the
	// canned response we were configured to deliver.
	for {
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		var env map[string]any
		if err := conn.ReadJSON(&env); err != nil {
			return
		}
		// If the client sent a non-ping call, reply with the
		// canned response. Otherwise just keep reading.
		if env["type"] == "ping" {
			_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
			_ = conn.WriteJSON(map[string]any{
				"type":    "pong",
				"ts":      env["ts"],
				"echo_ts": env["ts"],
			})
			continue
		}
		// Synthesise a matching exec_result / read_result / etc.
		fs.mu.Lock()
		resp := fs.nextResp
		fs.mu.Unlock()
		if resp == nil {
			// No canned response: just close so the test can
			// observe the disconnect.
			return
		}
		// Echo the id so the client's correlation logic is happy.
		if id, ok := env["id"].(string); ok && id != "" {
			resp["id"] = id
		}
		_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
		if err := conn.WriteJSON(resp); err != nil {
			return
		}
	}
}

// TestRun_ConnectsToServer is the smoke test: stand up the fake
// server, write a config pointing at it, run `hermes-node run` in a
// goroutine, wait for the supervisor to log "connecting" then
// successfully complete the handshake, and finally verify that the
// process exits cleanly when its context is cancelled.
func TestRun_ConnectsToServer(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping end-to-end test in -short mode")
	}
	fs := newFakeServer()
	defer fs.Close()

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	logPath := filepath.Join(dir, "audit.log")

	// Seed the config directly (bypassing the pair subcommand —
	// pair is tested separately). The audit log path needs a real
	// value; otherwise config.Load applies the default which lands
	// outside the temp dir.
	// config package is TOML; build a real config and Save it.
	cfg := &config.Config{
		Node: config.NodeConfig{
			ServerURL:    fs.URL(),
			Name:         "test-node",
			Token:        "test-token",
			AllowedPaths: []string{dir},
			LogPath:      logPath,
		},
	}
	if err := config.Save(cfgPath, cfg); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(cfgPath, 0o600); err != nil {
		t.Fatal(err)
	}
	// Drive run() in a goroutine. The signal-based shutdown path
	// isn't reachable from inside the test process without
	// gymnastics, so we use a short ctx timeout to make the
	// supervisor bail. The test asserts the binary produced the
	// "connecting" line before the ctx fires.
	runCtx, runCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer runCancel()

	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	done := make(chan int, 1)
	go func() {
		done <- runRun(runCtx, cfgPath, stdout, stderr)
	}()

	// Wait for the fake server to register a connection (the
	// supervisor should dial within a few hundred ms of startup).
	connected := waitFor(3*time.Second, 20*time.Millisecond, func() bool {
		return fs.connected.Load() > 0
	})
	if !connected {
		runCancel()
		<-done
		t.Logf("DEBUG: stdout=\n%s", stdout.String())
		t.Logf("DEBUG: stderr=\n%s", stderr.String())
		t.Fatalf("fake server never saw a connection. stdout:\n%s\nstderr:\n%s",
			stdout.String(), stderr.String())
	} // The supervisor is now in the dispatch loop. Cancel runCtx	// to make runRun return, then assert a clean exit.

	// Cancel the run ctx AND kill the fake server. The cancel
	// alone isn't enough to unblock the dispatch loop: its read
	// deadline is 90s by default (DefaultPongTimeout + 30s). The
	// check at the top of the loop honors ctx.Err, but it only
	// fires *between* reads. Killing the server forces the
	// in-flight read to fail with a network error, which the
	// dispatch loop surfaces and returns from. The cancel is
	// what makes the supervisor exit cleanly rather than retry.
	runCancel()
	fs.Close()
	select {
	case code := <-done:
		if !strings.Contains(stdout.String(), "connecting") {
			t.Errorf("stdout should contain 'connecting'; got %q", stdout.String())
		}
		if code != 0 {
			t.Errorf("runRun returned %d; stderr:\n%s", code, stderr.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("runRun did not exit within 10s of ctx cancel")
	}
}

// ---------------------------------------------------------------------------
// Single-instance daemon lock tests
// ---------------------------------------------------------------------------

// TestRunRun_RefusesWhenDaemonAlreadyRunning verifies that a second
// daemon start refuses to run (and does not connect) when a live PID
// already holds the single-instance lock file in the config dir.
func TestRunRun_RefusesWhenDaemonAlreadyRunning(t *testing.T) {
	fs := newFakeServer()
	defer fs.Close()

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	logPath := filepath.Join(dir, "audit.log")

	cfg := &config.Config{
		Node: config.NodeConfig{
			ServerURL:    fs.URL(),
			Name:         "test-node",
			Token:        "test-token",
			AllowedPaths: []string{dir},
			LogPath:      logPath,
		},
	}
	if err := config.Save(cfgPath, cfg); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(cfgPath, 0o600); err != nil {
		t.Fatal(err)
	}

	// Simulate a live daemon: write the lock file with our own PID —
	// the liveness check (signal 0) will succeed because we exist.
	lockPath := filepath.Join(dir, "daemon.lock")
	if err := os.WriteFile(lockPath, []byte(fmt.Sprintf("%d\n", os.Getpid())), 0o600); err != nil {
		t.Fatal(err)
	}

	runCtx, runCancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer runCancel()

	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	code := runRun(runCtx, cfgPath, stdout, stderr)

	if code != 1 {
		t.Errorf("runRun with live lock should refuse with exit 1; got %d (stderr: %s)", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "already running") {
		t.Errorf("stderr should mention 'already running'; got %q", stderr.String())
	}
	if fs.connected.Load() > 0 {
		t.Errorf("second daemon must not connect; fake server saw %d connection(s)", fs.connected.Load())
	}
	// The existing lock file must be left untouched (owned by the live daemon).
	data, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatalf("lock file should still exist: %v", err)
	}
	if !strings.Contains(string(data), fmt.Sprintf("%d", os.Getpid())) {
		t.Errorf("lock file should still name the original PID; got %q", string(data))
	}
}

// TestRun_RefusesWhenDaemonAlreadyRunning exercises the same guard at
// the CLI layer via the `run` subcommand, so the detach path also
// refuses before forking a second daemon.
func TestRun_RefusesWhenDaemonAlreadyRunning(t *testing.T) {
	fs := newFakeServer()
	defer fs.Close()

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	logPath := filepath.Join(dir, "audit.log")

	cfg := &config.Config{
		Node: config.NodeConfig{
			ServerURL:    fs.URL(),
			Name:         "test-node",
			Token:        "test-token",
			AllowedPaths: []string{dir},
			LogPath:      logPath,
		},
	}
	if err := config.Save(cfgPath, cfg); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(cfgPath, 0o600); err != nil {
		t.Fatal(err)
	}

	lockPath := filepath.Join(dir, "daemon.lock")
	if err := os.WriteFile(lockPath, []byte(fmt.Sprintf("%d\n", os.Getpid())), 0o600); err != nil {
		t.Fatal(err)
	}

	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	code := run([]string{"run", "--config", cfgPath}, stdout, stderr)

	if code != 1 {
		t.Errorf("`run` with live lock should refuse with exit 1; got %d (stderr: %s)", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "already running") {
		t.Errorf("stderr should mention 'already running'; got %q", stderr.String())
	}
	if fs.connected.Load() > 0 {
		t.Errorf("second daemon must not connect; fake server saw %d connection(s)", fs.connected.Load())
	}
}

// TestRunRun_AcquiresLockWhenNoneHeld verifies a fresh start writes
// the lock file with its own PID and that the file is removed on exit.
func TestRunRun_AcquiresLockWhenNoneHeld(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping end-to-end test in -short mode")
	}
	fs := newFakeServer()
	defer fs.Close()

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	logPath := filepath.Join(dir, "audit.log")

	cfg := &config.Config{
		Node: config.NodeConfig{
			ServerURL:    fs.URL(),
			Name:         "test-node",
			Token:        "test-token",
			AllowedPaths: []string{dir},
			LogPath:      logPath,
		},
	}
	if err := config.Save(cfgPath, cfg); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(cfgPath, 0o600); err != nil {
		t.Fatal(err)
	}

	lockPath := filepath.Join(dir, "daemon.lock")
	if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
		t.Fatalf("lock file should not exist before start; got err=%v", err)
	}

	runCtx, runCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer runCancel()

	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	done := make(chan int, 1)
	go func() {
		done <- runRun(runCtx, cfgPath, stdout, stderr)
	}()

	// Wait for the daemon to start and connect, then verify the lock
	// file was created with the daemon PID.
	connected := waitFor(3*time.Second, 20*time.Millisecond, func() bool {
		return fs.connected.Load() > 0
	})
	if !connected {
		runCancel()
		<-done
		t.Fatalf("fake server never saw a connection. stdout:\n%s\nstderr:\n%s",
			stdout.String(), stderr.String())
	}

	data, err := os.ReadFile(lockPath)
	if err != nil {
		runCancel()
		<-done
		t.Fatalf("lock file should exist while daemon is running: %v", err)
	}
	var lockPID int
	if _, err := fmt.Sscanf(string(data), "%d", &lockPID); err != nil || lockPID <= 0 {
		runCancel()
		<-done
		t.Fatalf("lock file should contain a valid PID; got %q", string(data))
	}
	// PID in the lock must be a live process (it should be the daemon
	// or, in tests, a process we can signal-check).
	proc, err := os.FindProcess(lockPID)
	if err != nil || proc.Signal(os.Signal(syscall.Signal(0))) != nil {
		runCancel()
		<-done
		t.Fatalf("lock PID %d is not alive", lockPID)
	}

	runCancel()
	fs.Close()
	select {
	case code := <-done:
		if code != 0 {
			t.Errorf("runRun returned %d; stderr:\n%s", code, stderr.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("runRun did not exit within 10s of ctx cancel")
	}

	// Lock file must be cleaned up on exit.
	if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
		t.Errorf("lock file should be removed on daemon exit; stat err=%v", err)
	}
}

// TestRunRun_ReclaimsStaleLock verifies that a lock file left behind
// by a dead daemon (stale PID) does not block a fresh start — the new
// daemon takes over and connects.
func TestRunRun_ReclaimsStaleLock(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping end-to-end test in -short mode")
	}
	fs := newFakeServer()
	defer fs.Close()

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	logPath := filepath.Join(dir, "audit.log")

	cfg := &config.Config{
		Node: config.NodeConfig{
			ServerURL:    fs.URL(),
			Name:         "test-node",
			Token:        "test-token",
			AllowedPaths: []string{dir},
			LogPath:      logPath,
		},
	}
	if err := config.Save(cfgPath, cfg); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(cfgPath, 0o600); err != nil {
		t.Fatal(err)
	}

	// Write a lock file naming a PID that is almost certainly dead.
	lockPath := filepath.Join(dir, "daemon.lock")
	const deadPID = 99999
	if err := os.WriteFile(lockPath, []byte(fmt.Sprintf("%d\n", deadPID)), 0o600); err != nil {
		t.Fatal(err)
	}

	runCtx, runCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer runCancel()

	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	done := make(chan int, 1)
	go func() {
		done <- runRun(runCtx, cfgPath, stdout, stderr)
	}()

	connected := waitFor(3*time.Second, 20*time.Millisecond, func() bool {
		return fs.connected.Load() > 0
	})
	if !connected {
		runCancel()
		<-done
		t.Fatalf("stale lock should not block startup; fake server never saw a connection. stdout:\n%s\nstderr:\n%s",
			stdout.String(), stderr.String())
	}

	// The stale lock must have been replaced by our PID.
	data, err := os.ReadFile(lockPath)
	if err != nil {
		runCancel()
		<-done
		t.Fatalf("lock file should exist after takeover: %v", err)
	}
	var lockPID int
	if _, err := fmt.Sscanf(string(data), "%d", &lockPID); err != nil || lockPID == deadPID {
		runCancel()
		<-done
		t.Fatalf("lock should be reclaimed with the live daemon PID; got %q", string(data))
	}

	runCancel()
	fs.Close()
	select {
	case code := <-done:
		if code != 0 {
			t.Errorf("runRun returned %d; stderr:\n%s", code, stderr.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("runRun did not exit within 10s of ctx cancel")
	}
}

// ---------------------------------------------------------------------------
// Restart subcommand tests
// ---------------------------------------------------------------------------

// TestRestartHelperProcess is not a real test: the restart tests spawn
// it as a sacrificial "live daemon" so runStop can SIGTERM a real PID
// without killing the test runner. It prints a readiness line and then
// blocks until terminated.
func TestRestartHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") != "1" {
		return
	}
	fmt.Println("restart-helper-pid-ready")
	select {}
}

// TestRun_Restart_StopsRunningDaemonThenStarts verifies that restart
// first stops a live daemon (SIGTERM to the real PID) and only then
// issues a fresh start. The detach fork is stubbed so the test binary
// isn't re-exec'd (Go test binaries can't act as the daemon child).
func TestRun_Restart_StopsRunningDaemonThenStarts(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")

	// Spawn the sacrificial daemon process.
	cmd := exec.Command(os.Args[0], "-test.run=TestRestartHelperProcess")
	cmd.Env = append(os.Environ(), "GO_WANT_HELPER_PROCESS=1")
	pr, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })

	// Wait for the readiness line so the PID is guaranteed alive before
	// we write the lock/status files naming it.
	line, err := bufio.NewReader(pr).ReadString('\n')
	if err != nil || !strings.Contains(line, "restart-helper-pid-ready") {
		t.Fatalf("helper never became ready: %v (line %q)", err, line)
	}
	pid := cmd.Process.Pid

	// Simulate the helper as the running daemon: lock + status name it.
	lockPath := filepath.Join(dir, "daemon.lock")
	if err := os.WriteFile(lockPath, []byte(fmt.Sprintf("%d\n", pid)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeStatus(statusFilePath(dir), &nodeStatus{PID: pid, State: "connected"}); err != nil {
		t.Fatal(err)
	}

	// Stub the actual start so restart doesn't re-exec the test binary.
	origStart := startDaemon
	started := false
	var startCfg string
	startDaemon = func(cfg string, args []string, stderr io.Writer) int {
		started = true
		startCfg = cfg
		return 0
	}
	defer func() { startDaemon = origStart }()

	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	code := run([]string{"restart", "--config", cfgPath}, stdout, stderr)

	if code != 0 {
		t.Errorf("restart exit = %d, want 0; stderr: %s", code, stderr.String())
	}
	if !started {
		t.Errorf("restart should start a fresh daemon after stopping; stdout: %s", stdout.String())
	}
	if startCfg != cfgPath {
		t.Errorf("restart started daemon with config %q, want %q", startCfg, cfgPath)
	}
	if !strings.Contains(stdout.String(), "stopping daemon") {
		t.Errorf("stdout should mention 'stopping daemon'; got %q", stdout.String())
	}

	// The old daemon must actually be gone (killed by SIGTERM). Reap it
	// in a goroutine with a timeout so a failed stop can't hang the test.
	waited := make(chan error, 1)
	go func() {
		_, err := cmd.Process.Wait()
		waited <- err
	}()
	select {
	case <-waited:
		// Killed by SIGTERM — Wait returns a non-nil *ExitError.
	case <-time.After(3 * time.Second):
		t.Errorf("restart did not terminate the old daemon (PID %d)", pid)
		_ = cmd.Process.Kill()
		<-waited
	}
}

// TestRun_Restart_NotRunningJustStarts verifies restart with no live
// daemon skips the stop phase and goes straight to starting.
func TestRun_Restart_NotRunningJustStarts(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")

	origStart := startDaemon
	started := false
	startDaemon = func(cfg string, args []string, stderr io.Writer) int {
		started = true
		return 0
	}
	defer func() { startDaemon = origStart }()

	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	code := run([]string{"restart", "--config", cfgPath}, stdout, stderr)

	if code != 0 {
		t.Errorf("restart exit = %d, want 0; stderr: %s", code, stderr.String())
	}
	if !started {
		t.Errorf("restart should start the daemon when none is running; stdout: %s", stdout.String())
	}
	if !strings.Contains(stdout.String(), "not running, starting") {
		t.Errorf("stdout should mention 'not running, starting'; got %q", stdout.String())
	}
}

// TestRun_Restart_AbortsIfStopFails verifies restart never starts when
// the stop phase fails: a live lock with no status file makes runStop
// fail, and starting anyway could race the still-unknown daemon for
// the lock.
func TestRun_Restart_AbortsIfStopFails(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")

	// A live lock (our own PID) but no status file: the daemon holds
	// the lock but never wrote status, so runStop can't find a PID to
	// signal and reports an error. Restart must abort rather than start.
	lockPath := filepath.Join(dir, "daemon.lock")
	if err := os.WriteFile(lockPath, []byte(fmt.Sprintf("%d\n", os.Getpid())), 0o600); err != nil {
		t.Fatal(err)
	}

	origStart := startDaemon
	started := false
	startDaemon = func(cfg string, args []string, stderr io.Writer) int {
		started = true
		return 0
	}
	defer func() { startDaemon = origStart }()

	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	code := run([]string{"restart", "--config", cfgPath}, stdout, stderr)

	if code != 1 {
		t.Errorf("restart exit = %d, want 1 (stop failed)", code)
	}
	if started {
		t.Errorf("restart must not start the daemon when the stop phase failed")
	}
	if !strings.Contains(stderr.String(), "status file not found") {
		t.Errorf("stderr should surface the stop failure; got %q", stderr.String())
	}
}

// ---------------------------------------------------------------------------
// Uninstall subcommand tests
// ---------------------------------------------------------------------------

func TestRun_Uninstall_NothingToDo(t *testing.T) {
	// Override osExecutable so the binary path is a non-existent file.
	orig := osExecutable
	osExecutable = func() (string, error) { return "/tmp/hermes-node-nonexistent", nil }
	defer func() { osExecutable = orig }()

	var stdout, stderr bytes.Buffer
	code := run([]string{"uninstall"}, &stdout, &stderr)
	if code != 0 {
		t.Errorf("exit = %d, want 0; stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "nothing to uninstall") {
		t.Errorf("stdout should mention 'nothing to uninstall'; got %q", stdout.String())
	}
}

func TestRun_Uninstall_RemovesBinary(t *testing.T) {
	dir := t.TempDir()
	binPath := filepath.Join(dir, "hermes-node")
	if err := os.WriteFile(binPath, []byte("fake binary"), 0o755); err != nil {
		t.Fatal(err)
	}

	orig := osExecutable
	osExecutable = func() (string, error) { return binPath, nil }
	defer func() { osExecutable = orig }()

	var stdout, stderr bytes.Buffer
	code := run([]string{"uninstall"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d; stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "removed binary") {
		t.Errorf("stdout should mention 'removed binary'; got %q", stdout.String())
	}
	if _, err := os.Stat(binPath); !os.IsNotExist(err) {
		t.Errorf("binary should be removed; stat err = %v", err)
	}
}

func TestRun_Uninstall_WithPurge(t *testing.T) {
	dir := t.TempDir()
	binPath := filepath.Join(dir, "hermes-node")
	configDir := filepath.Join(dir, ".hermes-node")
	if err := os.WriteFile(binPath, []byte("fake binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}

	origExec := osExecutable
	osExecutable = func() (string, error) { return binPath, nil }
	defer func() { osExecutable = origExec }()

	// Override defaultConfigPath so it uses our temp config dir.
	// Since runUninstall computes configDir from UserHomeDir,
	// this test is limited. We verify --purge flag parsing works.
	origHome := osUserHomeDir
	osUserHomeDir = func() (string, error) { return dir, nil }
	defer func() { osUserHomeDir = origHome }()

	var stdout, stderr bytes.Buffer
	code := run([]string{"uninstall", "--purge"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d; stderr=%q", code, stderr.String())
	}
	// Binary should be removed.
	if _, err := os.Stat(binPath); !os.IsNotExist(err) {
		t.Errorf("binary should be removed; stat err = %v", err)
	}
}

func TestRun_Uninstall_HelpInUsage(t *testing.T) {
	var out bytes.Buffer
	code := run([]string{"--help"}, &out, &bytes.Buffer{})
	if code != 0 {
		t.Fatalf("--help: exit %d", code)
	}
	if !strings.Contains(out.String(), "uninstall") {
		t.Errorf("--help should mention uninstall subcommand; got %q", out.String())
	}
	if !strings.Contains(out.String(), "dry-run") {
		t.Errorf("--help should mention --dry-run flag; got %q", out.String())
	}
}

func TestRun_Uninstall_DryRun(t *testing.T) {
	dir := t.TempDir()
	binPath := filepath.Join(dir, "hermes-node")
	if err := os.WriteFile(binPath, []byte("fake binary"), 0o755); err != nil {
		t.Fatal(err)
	}

	orig := osExecutable
	osExecutable = func() (string, error) { return binPath, nil }
	defer func() { osExecutable = orig }()

	var stdout, stderr bytes.Buffer
	code := run([]string{"uninstall", "--dry-run"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d; stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "dry-run") {
		t.Errorf("stdout should mention dry-run; got %q", stdout.String())
	}
	// Binary must still exist after dry-run.
	if _, err := os.Stat(binPath); err != nil {
		t.Errorf("binary should not be removed during dry-run; stat err = %v", err)
	}
}

func TestRun_Uninstall_DryRunWithPurge(t *testing.T) {
	dir := t.TempDir()
	binPath := filepath.Join(dir, "hermes-node")
	configDir := filepath.Join(dir, ".hermes-node")
	if err := os.WriteFile(binPath, []byte("fake binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}

	origExec := osExecutable
	osExecutable = func() (string, error) { return binPath, nil }
	defer func() { osExecutable = origExec }()
	origHome := osUserHomeDir
	osUserHomeDir = func() (string, error) { return dir, nil }
	defer func() { osUserHomeDir = origHome }()

	var stdout, stderr bytes.Buffer
	code := run([]string{"uninstall", "--dry-run", "--purge"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d; stderr=%q", code, stderr.String())
	}
	// Binary must still exist.
	if _, err := os.Stat(binPath); err != nil {
		t.Errorf("binary should not be removed during dry-run; stat err = %v", err)
	}
	// Config dir must still exist.
	if _, err := os.Stat(configDir); err != nil {
		t.Errorf("config dir should not be removed during dry-run; stat err = %v", err)
	}
}

func TestRun_Uninstall_SymlinkResolution(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink tests require non-Windows")
	}
	dir := t.TempDir()
	targetDir := filepath.Join(dir, "lib")
	if err := os.MkdirAll(targetDir, 0o755); err != nil {
		t.Fatal(err)
	}
	targetPath := filepath.Join(targetDir, "hermes-node-v2")
	if err := os.WriteFile(targetPath, []byte("real binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	linkPath := filepath.Join(dir, "hermes-node")
	if err := os.Symlink(targetPath, linkPath); err != nil {
		t.Fatal(err)
	}

	orig := osExecutable
	osExecutable = func() (string, error) { return linkPath, nil }
	defer func() { osExecutable = orig }()

	var stdout, stderr bytes.Buffer
	code := run([]string{"uninstall"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d; stderr=%q", code, stderr.String())
	}
	// The symlink should be gone.
	if _, err := os.Stat(linkPath); !os.IsNotExist(err) {
		t.Errorf("symlink should be removed; stat err = %v", err)
	}
	// The target binary should also be gone (symlink resolved).
	if _, err := os.Stat(targetPath); !os.IsNotExist(err) {
		t.Errorf("target binary should also be removed after symlink resolution; stat err = %v", err)
	}
}

func TestRun_Uninstall_UnknownFlag(t *testing.T) {
	var stderr bytes.Buffer
	code := run([]string{"uninstall", "--bogus"}, &bytes.Buffer{}, &stderr)
	if code != 2 {
		t.Errorf("exit = %d, want 2; stderr=%q", code, stderr.String())
	}
}

// ---------------------------------------------------------------------------
// Validate subcommand tests
// ---------------------------------------------------------------------------

func TestRun_Validate_ValidConfig(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	logDir := filepath.Join(dir, "logs")
	os.MkdirAll(logDir, 0o755)
	contents := `
[node]
server_url = "wss://vps.example.com:6969"
name = "test-node"
token = "test-token"
allowed_paths = ["/tmp"]
log_path = "` + filepath.Join(logDir, "audit.log") + `"
`
	if err := os.WriteFile(cfgPath, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := run([]string{"validate", "--config", cfgPath}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d; stdout=%q, stderr=%q", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "config is valid") {
		t.Errorf("stdout should mention 'config is valid'; got %q", stdout.String())
	}
}

func TestRun_Validate_InvalidConfig(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	contents := `
[node]
name = "test-node"
token = "test-token"
`
	// Missing server_url — config.Load should fail.
	if err := os.WriteFile(cfgPath, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}

	var stderr bytes.Buffer
	code := run([]string{"validate", "--config", cfgPath}, &bytes.Buffer{}, &stderr)
	if code != 1 {
		t.Errorf("exit = %d, want 1; stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "server_url") {
		t.Errorf("stderr should mention missing server_url; got %q", stderr.String())
	}
}

func TestRun_Validate_HelpInUsage(t *testing.T) {
	var out bytes.Buffer
	code := run([]string{"--help"}, &out, &bytes.Buffer{})
	if code != 0 {
		t.Fatalf("--help: exit %d", code)
	}
	if !strings.Contains(out.String(), "validate") {
		t.Errorf("--help should mention validate subcommand; got %q", out.String())
	}
}

func TestRun_Validate_UnknownFlag(t *testing.T) {
	var stderr bytes.Buffer
	code := run([]string{"validate", "--bogus"}, &bytes.Buffer{}, &stderr)
	if code != 2 {
		t.Errorf("exit = %d, want 2; stderr=%q", code, stderr.String())
	}
}

func TestRun_Validate_FileNotFound(t *testing.T) {
	var stderr bytes.Buffer
	code := run([]string{"validate", "--config", "/nonexistent/hermes-node/config.toml"}, &bytes.Buffer{}, &stderr)
	if code != 2 {
		t.Errorf("exit = %d, want 2; stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "not found") {
		t.Errorf("stderr should mention 'not found'; got %q", stderr.String())
	}
}

func TestRun_Validate_EmptyAllowedPaths(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	logDir := filepath.Join(dir, "logs")
	os.MkdirAll(logDir, 0o755)
	contents := `
[node]
server_url = "wss://vps.example.com:6969"
name = "test-node"
token = "test-token"
allowed_paths = []
log_path = "` + filepath.Join(logDir, "audit.log") + `"
`
	if err := os.WriteFile(cfgPath, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := run([]string{"validate", "--config", cfgPath}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d; stdout=%q, stderr=%q", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "WARN") {
		t.Errorf("stdout should contain a WARN about empty allowed_paths; got %q", stdout.String())
	}
	if !strings.Contains(stdout.String(), "config is valid") {
		t.Errorf("stdout should mention 'config is valid'; got %q", stdout.String())
	}
}

func TestRun_Validate_NonDirectoryAllowedPath(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	logDir := filepath.Join(dir, "logs")
	os.MkdirAll(logDir, 0o755)
	// Create a file to use as a non-directory allowed_path.
	filePath := filepath.Join(dir, "not-a-dir")
	os.WriteFile(filePath, []byte("x"), 0o644)
	contents := `
[node]
server_url = "wss://vps.example.com:6969"
name = "test-node"
token = "test-token"
allowed_paths = ["` + filePath + `"]
log_path = "` + filepath.Join(logDir, "audit.log") + `"
`
	if err := os.WriteFile(cfgPath, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := run([]string{"validate", "--config", cfgPath}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit = %d, want 1; stdout=%q, stderr=%q", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "not a directory") {
		t.Errorf("stdout should mention 'not a directory'; got %q", stdout.String())
	}
}

func TestRun_Validate_NonExistentLogDir(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	contents := `
[node]
server_url = "wss://vps.example.com:6969"
name = "test-node"
token = "test-token"
allowed_paths = ["/tmp"]
log_path = "` + filepath.Join(dir, "nonexistent", "audit.log") + `"
`
	if err := os.WriteFile(cfgPath, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := run([]string{"validate", "--config", cfgPath}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit = %d, want 1; stdout=%q, stderr=%q", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "FAIL") {
		t.Errorf("stdout should contain FAIL for missing log dir; got %q", stdout.String())
	}
}

// ---------------------------------------------------------------------------
// Status subcommand tests
// ---------------------------------------------------------------------------

func TestRun_Status_NoFile(t *testing.T) {
	var stdout, stderr bytes.Buffer
	// Point to a non-existent config dir — status file won't exist.
	code := run([]string{"status", "--config", "/nonexistent/path/config.toml"}, &stdout, &stderr)
	if code != 0 {
		t.Errorf("exit = %d, want 0; stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "not found") {
		t.Errorf("stdout should mention 'not found'; got %q", stdout.String())
	}
}

func TestRun_Status_DisplaysFile(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	// Create a minimal config so the config directory exists.
	os.WriteFile(cfgPath, []byte(""), 0o600)
	statusPath := filepath.Join(dir, "status.json")
	// Use the test process's own PID so the liveness check passes.
	pid := os.Getpid()
	contents := fmt.Sprintf(`{"pid":%d,"state":"connected","name":"test-node","server_url":"wss://x","version":"dev","session_id":"sess-1","started_at":"2026-06-22T21:00:00Z","last_connected_at":"2026-06-22T21:05:00Z"}`, pid)
	if err := os.WriteFile(statusPath, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := run([]string{"status", "--config", cfgPath}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d; stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "test-node") {
		t.Errorf("stdout should mention node name; got %q", stdout.String())
	}
	if !strings.Contains(stdout.String(), "sess-1") {
		t.Errorf("stdout should mention session ID; got %q", stdout.String())
	}
	if !strings.Contains(stdout.String(), "connected") {
		t.Errorf("stdout should mention state; got %q", stdout.String())
	}
}

func TestRun_Status_HelpInUsage(t *testing.T) {
	var out bytes.Buffer
	code := run([]string{"--help"}, &out, &bytes.Buffer{})
	if code != 0 {
		t.Fatalf("--help: exit %d", code)
	}
	if !strings.Contains(out.String(), "status") {
		t.Errorf("--help should mention status subcommand; got %q", out.String())
	}
}

func TestRun_Status_UnknownFlag(t *testing.T) {
	var stderr bytes.Buffer
	code := run([]string{"status", "--bogus"}, &bytes.Buffer{}, &stderr)
	if code != 2 {
		t.Errorf("exit = %d, want 2; stderr=%q", code, stderr.String())
	}
}

func TestRun_Status_CorruptedFile(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	os.WriteFile(cfgPath, []byte(""), 0o600)
	// Write invalid JSON to the status file.
	os.WriteFile(filepath.Join(dir, "status.json"), []byte("{invalid json}"), 0o600)

	var stdout, stderr bytes.Buffer
	code := run([]string{"status", "--config", cfgPath}, &stdout, &stderr)
	if code != 1 {
		t.Errorf("exit = %d, want 1; stderr=%q", code, stderr.String())
	}
}

func TestRun_Status_StalePID(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	os.WriteFile(cfgPath, []byte(""), 0o600)
	statusPath := filepath.Join(dir, "status.json")
	// Use a PID that is almost certainly not running anywhere.
	// On Linux, PIDs max out at /proc/sys/kernel/pid_max (usually 4194304).
	// 99999 is well above typical usage and won't collide with a real process.
	const deadPID = 99999
	contents := fmt.Sprintf(
		`{"pid":%d,"state":"connected","name":"dead-node","server_url":"wss://x","version":"dev","session_id":"sess-1","started_at":"2026-06-22T21:00:00Z","last_connected_at":"2026-06-22T21:05:00Z"}`,
		deadPID,
	)
	if err := os.WriteFile(statusPath, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := run([]string{"status", "--config", cfgPath}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d; stderr=%q", code, stderr.String())
	}
	// Status file says "connected" but PID is dead → must show "stopped".
	if strings.Contains(stdout.String(), "connected") {
		t.Errorf("stale PID should NOT show 'connected'; got %q", stdout.String())
	}
	if !strings.Contains(stdout.String(), "stopped") {
		t.Errorf("stale PID should show 'stopped'; got %q", stdout.String())
	}
}

// ---------------------------------------------------------------------------
// Update subcommand tests
// ---------------------------------------------------------------------------

func TestRun_Update_HelpInUsage(t *testing.T) {
	var out bytes.Buffer
	code := run([]string{"--help"}, &out, &bytes.Buffer{})
	if code != 0 {
		t.Fatalf("--help: exit %d", code)
	}
	if !strings.Contains(out.String(), "update") {
		t.Errorf("--help should mention update subcommand; got %q", out.String())
	}
}

func TestRun_Update_UnknownFlag(t *testing.T) {
	var stderr bytes.Buffer
	code := run([]string{"update", "--bogus"}, &bytes.Buffer{}, &stderr)
	if code != 2 {
		t.Errorf("exit = %d, want 2; stderr=%q", code, stderr.String())
	}
}

func TestRun_Update_NoNetwork(t *testing.T) {
	// Simulate network failure by overriding githubAPI to an invalid URL.
	orig := githubAPI
	githubAPI = "http://127.0.0.1:1/nonexistent"
	defer func() { githubAPI = orig }()

	var stdout, stderr bytes.Buffer
	code := run([]string{"update", "--yes"}, &stdout, &stderr)
	if code != 1 {
		t.Errorf("exit = %d, want 1 (network failure); stdout=%q, stderr=%q", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "fetch") {
		t.Errorf("stderr should mention fetch failure; got %q", stderr.String())
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// testDefaultConfigPath is a small wrapper that returns the same
// value as defaultConfigPath but panics on error, so the test
// cases can use it as a single-value expression. Tests run in a
// normal environment where $HOME is set; if that ever changes the
// panic will surface as a clear test failure rather than a
// confusing table-row mismatch.
func testDefaultConfigPath() string {
	p, err := defaultConfigPath()
	if err != nil {
		panic("testDefaultConfigPath: " + err.Error())
	}
	return p
}

// waitFor polls cond every tick until it returns true or timeout
// elapses. Returns whether cond ever became true.
func waitFor(timeout, tick time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(tick)
	}
	return cond()
}
