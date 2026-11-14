package main

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/loficore/wg-user/internal/config"
	"github.com/loficore/wg-user/internal/state"
)

// syncBuffer collects one of realMain's outputs. It is mutex-guarded because
// with -v wireguard-go logs from several goroutines at once and bytes.Buffer
// is not safe for concurrent writers.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// stageConfig copies src to <tmp>/etc/wireproxy.conf and returns the temp
// module root and the config path. Pointing -c inside a temp module keeps the
// state file at <tmp>/run/state instead of inside the source tree.
func stageConfig(t *testing.T, src string) (root, configPath string) {
	t.Helper()

	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("read fixture %s: %v", src, err)
	}

	root = t.TempDir()
	etc := filepath.Join(root, "etc")
	if err := os.MkdirAll(etc, 0o755); err != nil {
		t.Fatalf("create %s: %v", etc, err)
	}
	configPath = filepath.Join(etc, "wireproxy.conf")
	if err := os.WriteFile(configPath, data, 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return root, configPath
}

// startRun starts realMain in a goroutine and returns the channel carrying its
// exit code plus the captured output.
func startRun(ctx context.Context, args ...string) (<-chan int, *syncBuffer, *syncBuffer) {
	var stdout, stderr syncBuffer
	done := make(chan int, 1)
	go func() {
		done <- realMain(ctx, args, &stdout, &stderr)
	}()
	return done, &stdout, &stderr
}

// waitForFile waits until path exists. The startup state write is the last
// thing run does before waiting on the context, so its presence means realMain
// has started.
func waitForFile(t *testing.T, path string, timeout time.Duration) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", path)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// waitForExit waits for realMain to return and checks its exit code.
func waitForExit(t *testing.T, done <-chan int, stdout, stderr *syncBuffer) int {
	t.Helper()

	select {
	case code := <-done:
		return code
	case <-time.After(5 * time.Second):
		t.Fatalf("realMain did not return within 5s; stdout: %s; stderr: %s",
			stdout.String(), stderr.String())
		return -1
	}
}

func TestRealMain_ConfigtestOK(t *testing.T) {
	var stdout, stderr syncBuffer

	code := realMain(context.Background(),
		[]string{"configtest", "-c", "../../internal/config/testdata/minimal.conf"},
		&stdout, &stderr)

	if code != 0 {
		t.Fatalf("realMain() = %d, want 0; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Config OK") {
		t.Errorf("stdout = %q, want it to contain %q", stdout.String(), "Config OK")
	}
}

func TestRealMain_ConfigtestBadConfig(t *testing.T) {
	var stdout, stderr syncBuffer

	code := realMain(context.Background(),
		[]string{"configtest", "-c", "../../internal/config/testdata/transport-bad-mode.conf"},
		&stdout, &stderr)

	if code != 2 {
		t.Fatalf("realMain() = %d, want 2; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "unsupported transport mode") {
		t.Errorf("stderr = %q, want it to contain %q", stderr.String(), "unsupported transport mode")
	}
}

func TestRealMain_UnknownSubcommand(t *testing.T) {
	var stdout, stderr syncBuffer

	code := realMain(context.Background(), []string{"frobnicate"}, &stdout, &stderr)

	if code != 2 {
		t.Fatalf("realMain() = %d, want 2; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "unknown subcommand") {
		t.Errorf("stderr = %q, want it to contain %q", stderr.String(), "unknown subcommand")
	}
}

func TestRealMain_MissingConfig(t *testing.T) {
	var stdout, stderr syncBuffer
	missing := filepath.Join(t.TempDir(), "missing.conf")

	code := realMain(context.Background(), []string{"configtest", "-c", missing}, &stdout, &stderr)

	if code != 2 {
		t.Fatalf("realMain() = %d, want 2", code)
	}
	if stderr.String() == "" {
		t.Error("stderr is empty, want an error explaining the missing config")
	}
}

func TestRealMain_RunStopsOnContextCancel(t *testing.T) {
	root, configPath := stageConfig(t, "testdata/off.conf")
	statePath := filepath.Join(root, "run", "state")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done, stdout, stderr := startRun(ctx, "run", "-c", configPath)

	waitForFile(t, statePath, 5*time.Second)
	cancel()

	if code := waitForExit(t, done, stdout, stderr); code != 0 {
		t.Fatalf("realMain() = %d, want 0; stdout: %s; stderr: %s",
			code, stdout.String(), stderr.String())
	}
	if got := stdout.String(); got != "" {
		t.Errorf("stdout = %q, want empty", got)
	}
}

// TestRealMain_WritesAndRemovesStateFile pins the startup snapshot and the
// shutdown cleanup: the state file must exist while the daemon runs and be
// gone once it exits, so a stale pid cannot outlive the process.
func TestRealMain_WritesAndRemovesStateFile(t *testing.T) {
	root, configPath := stageConfig(t, "testdata/off.conf")
	statePath := filepath.Join(root, "run", "state")
	runDir := filepath.Join(root, "run")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done, stdout, stderr := startRun(ctx, "run", "-c", configPath)

	// run must create the sibling run/ directory before its first write: the
	// daemon can start before the shell has created it.
	waitForFile(t, statePath, 5*time.Second)

	if info, err := os.Stat(runDir); err != nil || !info.IsDir() {
		t.Fatalf("os.Stat(%s) = %v, %v; want a directory", runDir, info, err)
	}

	got, err := state.Read(statePath)
	if err != nil {
		t.Fatalf("state.Read(%s) unexpected error: %v", statePath, err)
	}
	if got.PID != os.Getpid() {
		t.Errorf("state pid = %d, want %d", got.PID, os.Getpid())
	}
	if got.Transport != config.ModeOff {
		t.Errorf("state transport = %q, want %q", got.Transport, config.ModeOff)
	}
	if got.TransportState != "connected" {
		t.Errorf("state transport_state = %q, want %q", got.TransportState, "connected")
	}
	if got.TransportDetail != "udp" {
		t.Errorf("state transport_detail = %q, want %q", got.TransportDetail, "udp")
	}

	cancel()

	if code := waitForExit(t, done, stdout, stderr); code != 0 {
		t.Fatalf("realMain() = %d, want 0; stdout: %s; stderr: %s",
			code, stdout.String(), stderr.String())
	}

	if _, err := os.Stat(statePath); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("os.Stat(%s) after shutdown = %v, want fs.ErrNotExist", statePath, err)
	}
}

// TestRealMain_VerboseDeviceLogsGoToStderr pins the logger wiring end to end:
// wireguard-go's lines must use the stderr realMain was given (with the
// wg-user prefix) and must never reach stdout. wireguard-go's NewLogger, which
// Create used to call, writes to the process's real stdout.
func TestRealMain_VerboseDeviceLogsGoToStderr(t *testing.T) {
	root, configPath := stageConfig(t, "testdata/off.conf")
	statePath := filepath.Join(root, "run", "state")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done, stdout, stderr := startRun(ctx, "run", "-v", "-c", configPath)

	waitForFile(t, statePath, 5*time.Second)
	cancel()

	if code := waitForExit(t, done, stdout, stderr); code != 0 {
		t.Fatalf("realMain() = %d, want 0; stdout: %s; stderr: %s",
			code, stdout.String(), stderr.String())
	}
	if got := stdout.String(); got != "" {
		t.Errorf("stdout = %q, want empty", got)
	}
	if got := stderr.String(); !strings.Contains(got, "wg-user: UAPI: Updating private key") {
		t.Errorf("stderr = %q, want it to contain %q", got, "wg-user: UAPI: Updating private key")
	}
}

// TestNewLogger pins the -v gate: both callbacks are wired (wireguard-go picks
// the level by which callbacks exist), so Verbosef itself must drop its lines
// when -v is absent.
func TestNewLogger(t *testing.T) {
	tests := []struct {
		name    string
		verbose bool
		want    string
	}{
		{
			name:    "quiet drops verbose lines",
			verbose: false,
			want:    "wg-user: error 2\n",
		},
		{
			name:    "verbose keeps both",
			verbose: true,
			want:    "wg-user: verbose 1\nwg-user: error 2\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf syncBuffer
			logger := newLogger(&buf, tt.verbose)

			logger.Verbosef("verbose %d", 1)
			logger.Errorf("error %d", 2)

			if got := buf.String(); got != tt.want {
				t.Errorf("logger output = %q, want %q", got, tt.want)
			}
		})
	}
}
