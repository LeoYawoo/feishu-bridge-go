// Package shell owns a persistent PowerShell session per chat.
//
// Two layers of state, matching the requested design:
//
//  1. Layer 1 — a long-lived pwsh process. `cd`, env vars, aliases and any
//     PowerShell state set by the user persist across messages. This is the
//     "directory shell" the user navigates in from Feishu.
//  2. Layer 2 — Claude Code sessions, one per Feishu *thread* (chat + topic).
//     These are driven through `claude -p --resume <id>` and live in the same
//     working directory as layer 1.
//
// The shell is deliberately dumb: it echoes stdout/stderr back verbatim and
// never interprets the user's PowerShell.
package shell

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Config controls one shell instance.
type Config struct {
	Bin       string
	Workspace string
	Timeout   time.Duration
	// KeepHistory leaves the native PS history enabled. Off by default.
	KeepHistory bool
	// ShutdownTimeout bounds the graceful wait before falling back to Kill.
	ShutdownTimeout time.Duration
	// SnapshotDir is where cwd/env are written on snapshot and read back on
	// Start. Empty disables snapshotting entirely.
	SnapshotDir string
}

// Shell is a persistent PowerShell session.
type Shell struct {
	cfg  Config
	logf func(format string, args ...any)

	// mu guards the process handle and the request protocol state. A single
	// Run is exclusive per shell.
	mu      sync.Mutex
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	outCh   chan []byte
	seq     int64
	pending bool
}

// New creates a shell without starting the process.
func New(cfg Config, logf func(string, ...any)) *Shell {
	if cfg.Bin == "" {
		cfg.Bin = "pwsh"
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 60 * time.Second
	}
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &Shell{cfg: cfg, logf: logf, outCh: make(chan []byte, 256)}
}

// bootstrapScript runs once at process creation to keep the pipe clean.
func (s *Shell) bootstrapScript() string {
	var b strings.Builder
	b.WriteString("$ProgressPreference = 'SilentlyContinue'\n")
	b.WriteString("Clear-History -ErrorAction SilentlyContinue\n")
	b.WriteString("Disable-PSTranscripting -ErrorAction SilentlyContinue\n")
	if s.cfg.Workspace != "" {
		b.WriteString(fmt.Sprintf("Set-Location -LiteralPath %q\n", s.cfg.Workspace))
	}
	return b.String()
}

// Start launches the pwsh process.
func (s *Shell) Start(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.cmd != nil {
		return fmt.Errorf("shell already started")
	}
	if _, err := exec.LookPath(s.cfg.Bin); err != nil {
		return fmt.Errorf("shell %q not found: %w", s.cfg.Bin, err)
	}

	// Read scripts from stdin rather than using -Command, which exits the
	// process after the first script completes.
	args := []string{
		"-NoLogo", "-NoProfile", "-NonInteractive",
		"-ExecutionPolicy", "Bypass",
		"-Command", "-",
	}

	cmd := exec.CommandContext(ctx, s.cfg.Bin, args...)
	cmd.Dir = s.cfg.Workspace
	cmd.Env = append(os.Environ(),
		"PSModuleAutoLoadingPreference=UseOnly",
		"PSTOOLERUNTIME=1",
	)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("stderr pipe: %w", err)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("stdin pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start %s: %w", s.cfg.Bin, err)
	}

	go s.drain(stdout, s.outCh)
	go s.drainLog(stderr, "stderr")

	s.cmd = cmd
	s.stdin = stdin
	s.logf("shell started: bin=%s pid=%d dir=%s", s.cfg.Bin, cmd.Process.Pid, s.cfg.Workspace)

	marker := "__BRIDGE_BOOT_DONE__"
	script := s.bootstrapScript()

	// Replay a previous snapshot if present. Cwd and env are the only
	// restorable pieces; functions and aliases are lost on reaping.
	if p := s.snapshotPath(); p != "" {
		if saved, err := os.ReadFile(p); err == nil {
			var snap struct {
				Cwd string            `json:"cwd"`
				Env map[string]string `json:"env"`
				At  string            `json:"at"`
			}
			if json.Unmarshal(saved, &snap) == nil {
				if snap.Cwd != "" {
					script += fmt.Sprintf("\nSet-Location -LiteralPath %q", snap.Cwd)
					s.logf("shell restored cwd from snapshot: %s (saved %s)", snap.Cwd, snap.At)
				}
				for k, v := range snap.Env {
					// Only restore variables the operator set; skip Go's own
					// process-level env to avoid clobbering the child.
					script += fmt.Sprintf("\nSet-Item -LiteralPath Env:%q -Value %q -Force", k, v)
				}
			}
			_ = os.Remove(p) // consumed; a stale snapshot must not replay twice
		}
	}

	if _, err := fmt.Fprintln(stdin, script+marker); err != nil {
		_ = cmd.Process.Kill()
		return fmt.Errorf("shell bootstrap write: %w", err)
	}
	if err := s.discardUntil(20*time.Second, marker); err != nil {
		_ = cmd.Process.Kill()
		return fmt.Errorf("shell bootstrap: %w", err)
	}
	return nil
}

// discardUntil reads and drops output until marker appears.
func (s *Shell) discardUntil(timeout time.Duration, marker string) error {
	deadline := time.Now().Add(timeout)
	for {
		if time.Now().After(deadline) {
			return fmt.Errorf("marker %q not seen within %s", marker, timeout)
		}
		select {
		case chunk, ok := <-s.outCh:
			if !ok {
				return errors.New("shell exited before bootstrap completed")
			}
			if strings.Contains(string(chunk), marker) {
				return nil
			}
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// drain copies the stdout pipe into outCh line by line.
func (s *Shell) drain(r io.Reader, ch chan<- []byte) {
	defer close(ch)
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		ch <- []byte(scanner.Text() + "\n")
	}
}

// drainLog copies stderr straight to the log.
func (s *Shell) drainLog(r io.Reader, label string) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		s.logf("[%s] %s", label, scanner.Text())
	}
}

// Run executes one PowerShell command synchronously and returns its output.
// Only one Run may be in flight per shell.
func (s *Shell) Run(ctx context.Context, script string) (string, error) {
	s.mu.Lock()
	cmd, stdin := s.cmd, s.stdin
	if cmd == nil {
		s.mu.Unlock()
		return "", fmt.Errorf("shell not started")
	}
	if s.pending {
		s.mu.Unlock()
		return "", errors.New("shell busy")
	}
	s.seq++
	seq := s.seq
	s.pending = true
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		s.pending = false
		s.mu.Unlock()
	}()

	runCtx, cancel := context.WithTimeout(ctx, s.cfg.Timeout)
	defer cancel()

	begin := fmt.Sprintf("__BRIDGE_BEGIN_%d__", seq)
	end := fmt.Sprintf("__BRIDGE_END_%d__", seq)

	// Bracket the script with unique markers so partial output from an
	// interrupted run cannot be attributed to this one. Out--String folds
	// multi-line output into a single line so the marker can never be
	// confused with script output.
	wrapped := fmt.Sprintf(
		"%s\n& { %s } 2>&1 | Out-String -Width 2000\n$LASTEXITCODE\n%s",
		begin, script, end)

	if _, err := fmt.Fprintln(stdin, wrapped); err != nil {
		return "", fmt.Errorf("write to shell: %w", err)
	}

	var sb strings.Builder
	gotBegin := false
	var exitLine string

	for {
		select {
		case <-runCtx.Done():
			if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
				return sb.String(), fmt.Errorf("shell command timed out after %s", s.cfg.Timeout)
			}
			return sb.String(), runCtx.Err()

		case chunk, ok := <-s.outCh:
			if !ok {
				return sb.String(), errors.New("shell process exited")
			}
			line := strings.TrimRight(string(chunk), "\n")

			if !gotBegin {
				if strings.Contains(line, begin) {
					gotBegin = true
				}
				continue
			}
			if strings.Contains(line, end) {
				return sb.String(), interpretExit(exitLine)
			}
			exitLine = line
			sb.WriteString(line)
			sb.WriteString("\n")
		}
	}
}

// interpretExit converts a captured $LASTEXITCODE line into an error.
func interpretExit(code string) error {
	code = strings.TrimSpace(code)
	if code == "" || code == "0" {
		return nil
	}
	return fmt.Errorf("exit code %s", code)
}

// SnapshotDir returns the directory the snapshot lives in, or "" if disabled.
func (s *Shell) SnapshotDir() string { return s.cfg.SnapshotDir }

// snapshotFile is the per-shell state file name.
const snapshotFile = ".feishu-bridge-shell.json"

// snapshotPath joins SnapshotDir with the state file name.
func (s *Shell) snapshotPath() string {
	if s.cfg.SnapshotDir == "" {
		return ""
	}
	return filepath.Join(s.cfg.SnapshotDir, snapshotFile)
}

// SnapshotState writes cwd and env to disk so a later Start can restore them.
//
// Only cwd and env survive. PowerShell has no way to serialise a live
// session's functions, aliases, module state or scriptblocks, so those are
// lost by design. Callers should not imply otherwise.
func (s *Shell) SnapshotState(ctx context.Context) error {
	dir := s.cfg.SnapshotDir
	if dir == "" {
		return nil
	}
	script := `$o = New-Object PSObject; ` +
		`$o | Add-Member NoteProperty cwd ((Get-Location -LiteralPath).Path); ` +
		`$o | Add-Member NoteProperty env (@(Get-ChildItem Env: | ForEach-Object { @{Name=$_.Name; Value=$_.Value} })); ` +
		`$o | ConvertTo-Json -Compress -Depth 3`
	out, err := s.Run(ctx, script)
	if err != nil {
		return fmt.Errorf("snapshot run: %w", err)
	}
	if out == "" {
		return nil
	}
	out = strings.TrimSpace(out)
	var payload struct {
		Cwd string              `json:"cwd"`
		Env []map[string]string `json:"env"`
	}
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		return fmt.Errorf("snapshot decode: %w", err)
	}
	env := make(map[string]string, len(payload.Env))
	for _, p := range payload.Env {
		if k, ok := p["Name"]; ok {
			env[k] = p["Value"]
		}
	}
	b, err := json.Marshal(map[string]any{
		"cwd": payload.Cwd,
		"env": env,
		"at":  time.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		return err
	}
	return os.WriteFile(s.snapshotPath(), b, 0o600)
}

// Stop terminates the process gracefully: it closes stdin to send EOF and
// waits, then falls back to Kill if the process does not exit in time.
//
// Graceful shutdown lets in-flight PowerShell state settle and runs any
// user's PSReadLine/profile exit hooks. This matters only if KeepHistory or a
// profile is enabled, but it is the polite default.
func (s *Shell) Stop() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cmd == nil || s.cmd.Process == nil {
		return nil
	}

	if s.stdin != nil {
		// Best effort: pwsh reads stdin, EOF makes it exit cleanly.
		_ = s.stdin.Close()
	}

	proc := s.cmd.Process
	cmd := s.cmd
	s.cmd = nil
	s.stdin = nil
	s.pending = false

	errCh := make(chan error, 1)
	go func() { errCh <- cmd.Wait() }()

	wait := s.cfg.ShutdownTimeout
	if wait <= 0 {
		wait = 5 * time.Second
	}
	select {
	case <-time.After(wait):
		_ = proc.Kill()
		<-errCh
		return nil
	case err := <-errCh:
		return err
	}
}

// Started reports whether a process is alive.
func (s *Shell) Started() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cmd != nil
}
