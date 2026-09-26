// Package agent drives Claude Code (or a codex-compatible CLI) as a
// subprocess. Each call is one turn: `claude -p --resume <id>`.
//
// Stream-json output is parsed incrementally so the caller can update a
// Feishu card as tokens arrive, rather than waiting for the whole answer.
package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// Config for one agent invocation.
type Config struct {
	Command    string
	Model      string
	Workspace  string
	Timeout    time.Duration
	AppendSys  string
	SettingSrc string
	ExtraArgs  []string
}

// Event is one parsed line from the agent's stream.
type Event struct {
	Type       string // assistant | result | stream_event | rate_limit_event | raw
	Text       string // accumulated assistant text, or a delta for stream events
	ToolName   string
	Todos      []Todo
	IsError    bool
	SessionID  string
	StopReason string
	Usage      map[string]any
	ModelUsage map[string]any
	CostUSD    float64
	Raw        map[string]any
}

// Todo is a TodoWrite entry surfaced for progress display.
type Todo struct {
	Content    string
	Status     string
	ActiveForm string
}

// Result is the final answer of a turn.
type Result struct {
	Text       string
	SessionID  SessionID
	IsError    bool
	Usage      map[string]any
	ModelUsage map[string]any
	CostUSD    float64
	StopReason string
	Tools      []string
	Elapsed    time.Duration
}

// SessionID is a stable handle for resuming a conversation.
type SessionID string

// Run invokes the agent once and streams events to onEvent.
//
// resume == "" starts a fresh session; otherwise we continue the given one.
// On any failure a non-nil *Result is returned alongside the error, carrying
// whatever text and session id were accumulated so the caller can still show
// partial progress.
func Run(ctx context.Context, cfg Config, prompt string, resume SessionID, onEvent func(Event)) (*Result, error) {
	if cfg.Command == "" {
		cfg.Command = "claude"
	}
	if cfg.Workspace == "" {
		cfg.Workspace = "."
	}
	if _, err := exec.LookPath(cfg.Command); err != nil {
		return nil, fmt.Errorf("agent %q not found: %w", cfg.Command, err)
	}

	args := buildArgs(cfg, prompt, resume)
	cmd := exec.CommandContext(ctx, cfg.Command, args...)
	cmd.Dir = cfg.Workspace

	if cfg.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, cfg.Timeout)
		defer cancel()
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", cfg.Command, err)
	}

	start := time.Now()
	st := newStreamState()

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64*1024), 2*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, "{") {
			// Some CLIs emit plain progress text; surface it as a raw event.
			if onEvent != nil {
				onEvent(Event{Type: "raw", Text: line})
			}
			continue
		}
		ev, err := parseEvent([]byte(line))
		if err != nil {
			continue
		}
		st.observe(ev)
		if onEvent != nil {
			onEvent(ev)
		}
	}

	// The agent may still be finishing when the pipe closes.
	if err := cmd.Wait(); err != nil {
		res := st.result(SessionID(""))
		res.Elapsed = time.Since(start)
		if ctx.Err() != nil {
			return res, fmt.Errorf("agent timed out or cancelled: %v", ctx.Err())
		}
		return res, fmt.Errorf("agent exit: %w (stderr: %s)", err, tail(stderr.String(), 400))
	}

	res := st.result(SessionID(""))
	res.Elapsed = time.Since(start)
	return res, nil
}

// buildArgs constructs the argv for a single turn.
func buildArgs(cfg Config, prompt string, resume SessionID) []string {
	args := []string{cfg.Command, "-p"}
	if len(cfg.ExtraArgs) > 0 {
		args = append(args, cfg.ExtraArgs...)
	}
	if cfg.SettingSrc != "" {
		args = append(args, "--setting-sources", cfg.SettingSrc)
	}
	if cfg.Model != "" {
		args = append(args, "--model", cfg.Model)
	}
	if cfg.AppendSys != "" {
		args = append(args, "--append-system-prompt", cfg.AppendSys)
	}
	// stream-json gives us incremental text plus tool_use blocks.
	args = append(args, "--output-format", "stream-json", "--verbose", "--include-partial-messages")
	if resume != "" {
		args = append(args, "--resume", string(resume))
	}
	args = append(args, "--", prompt)
	return args
}

// parseEvent decodes one JSON line from the agent's stream.
func parseEvent(line []byte) (Event, error) {
	var raw map[string]any
	if err := json.Unmarshal(line, &raw); err != nil {
		return Event{}, err
	}
	ev := Event{Raw: raw, Type: str(raw["type"])}

	switch ev.Type {
	case "result":
		ev.SessionID = str(raw["session_id"])
		ev.IsError = boolv(raw["is_error"])
		ev.StopReason = str(raw["stop_reason"])
		ev.Text = str(raw["result"])
		ev.Usage, _ = raw["usage"].(map[string]any)
		ev.ModelUsage, _ = raw["modelUsage"].(map[string]any)
		ev.CostUSD = numv(raw["total_cost_usd"])
	case "assistant":
		msg, _ := raw["message"].(map[string]any)
		if msg == nil {
			break
		}
		ev.Usage, _ = msg["usage"].(map[string]any)
		if sid := str(msg["session_id"]); sid != "" {
			ev.SessionID = sid
		}
		content, _ := msg["content"].([]any)
		for _, c := range content {
			b, ok := c.(map[string]any)
			if !ok {
				continue
			}
			switch b["type"] {
			case "text":
				ev.Text = str(b["text"])
			case "tool_use":
				ev.ToolName = str(b["name"])
				if b["name"] == "TodoWrite" {
					if inp, ok := b["input"].(map[string]any); ok {
						ev.Todos = parseTodos(inp["todos"])
					}
				}
			}
		}
	case "stream_event":
		inner, _ := raw["event"].(map[string]any)
		if inner == nil {
			break
		}
		if str(inner["type"]) == "content_block_delta" {
			delta, _ := inner["delta"].(map[string]any)
			if delta != nil && str(delta["type"]) == "text_delta" {
				ev.Text = str(delta["text"])
			}
		}
	case "rate_limit_event":
		if rli, ok := raw["rate_limit_info"].(map[string]any); ok {
			ev.Usage = rli
		}
	}
	return ev, nil
}

func parseTodos(v any) []Todo {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]Todo, 0, len(arr))
	for _, item := range arr {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		out = append(out, Todo{
			Content:    str(m["content"]),
			Status:     str(m["status"]),
			ActiveForm: str(m["activeForm"]),
		})
	}
	return out
}

// streamState accumulates per-turn facts from the event stream.
type streamState struct {
	mu            sync.Mutex
	accumulated   string
	lastSessionID string
	finalResult   Event
	tools         []string
}

func newStreamState() *streamState { return &streamState{} }

func (s *streamState) observe(ev Event) {
	s.mu.Lock()
	defer s.mu.Unlock()

	switch ev.Type {
	case "stream_event":
		if ev.Text != "" {
			s.accumulated += ev.Text
		}
	case "assistant":
		if ev.SessionID != "" {
			s.lastSessionID = ev.SessionID
		}
		if ev.Text != "" {
			s.accumulated = ev.Text
		}
		if ev.ToolName != "" {
			s.tools = append(s.tools, ev.ToolName)
		}
	case "result":
		s.finalResult = ev
		if ev.SessionID != "" {
			s.lastSessionID = ev.SessionID
		}
	}
}

func (s *streamState) result(fallback SessionID) *Result {
	s.mu.Lock()
	defer s.mu.Unlock()

	sid := s.lastSessionID
	if sid == "" {
		sid = string(fallback)
	}
	text := s.finalResult.Text
	if text == "" {
		text = s.accumulated
	}
	return &Result{
		Text:       text,
		SessionID:  SessionID(sid),
		IsError:    s.finalResult.IsError,
		Usage:      s.finalResult.Usage,
		ModelUsage: s.finalResult.ModelUsage,
		CostUSD:    s.finalResult.CostUSD,
		StopReason: s.finalResult.StopReason,
		Tools:      s.tools,
	}
}

// ---- tiny helpers --------------------------------------------------------

func str(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func boolv(v any) bool {
	b, _ := v.(bool)
	return b
}

func numv(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	}
	return 0
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "..." + s[len(s)-n:]
}
