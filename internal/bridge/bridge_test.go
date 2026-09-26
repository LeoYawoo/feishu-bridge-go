package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"feishubridge/internal/agent"
	"feishubridge/internal/config"
	"feishubridge/internal/feishu"
)

// ---- fake sender ---------------------------------------------------------

// fakeSender records every card the bridge tries to send so tests can
// assert against it. It implements feishuSender without a websocket.
type fakeSender struct {
	mu     sync.Mutex
	cards  []map[string]any
	msgIDs []string
}

func (f *fakeSender) SendCard(ctx context.Context, chatID string, c map[string]any) (string, error) {
	return f.send(c)
}

func (f *fakeSender) SendCardInThread(ctx context.Context, chatID, threadID string, c map[string]any) (string, error) {
	return f.send(c)
}

func (f *fakeSender) ReplyCard(ctx context.Context, src string, c map[string]any, thread bool) (string, error) {
	return f.send(c)
}

func (f *fakeSender) ReplyCardThreaded(ctx context.Context, src string, c map[string]any, thread bool) (messageID, threadID string, err error) {
	id, _ := f.send(c)
	return id, "omt_" + id, nil
}

func (f *fakeSender) PatchCard(ctx context.Context, msgID string, c map[string]any) error {
	// Patch re-sends; tests assert against lastCard which will be the
	// patched state.
	_, _ = f.send(c)
	return nil
}

func (f *fakeSender) StartWS(ctx context.Context) error { return nil }

func (f *fakeSender) send(c map[string]any) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := fmt.Sprintf("msg_%d", len(f.cards)+1)
	f.cards = append(f.cards, c)
	f.msgIDs = append(f.msgIDs, id)
	return id, nil
}

func (f *fakeSender) lastCard() map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.cards) == 0 {
		return nil
	}
	return f.cards[len(f.cards)-1]
}

func (f *fakeSender) allCards() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]map[string]any, len(f.cards))
	copy(out, f.cards)
	return out
}

// ---- fake agent runner ---------------------------------------------------

// agentStub records every Run call. Default behaviour returns a
// well-formed result with an incremental session id; on --resume the
// same id is echoed back (matching real claude semantics).
type agentStub struct {
	mu      sync.Mutex
	prompts []string
	cfgs    []agent.Config
	resumes []agent.SessionID
	nextID  int
}

func (a *agentStub) Run(ctx context.Context, cfg agent.Config, prompt string, resume agent.SessionID, onEvent func(agent.Event)) (*agent.Result, error) {
	a.mu.Lock()
	a.prompts = append(a.prompts, prompt)
	a.cfgs = append(a.cfgs, cfg)
	a.resumes = append(a.resumes, resume)
	a.nextID++
	a.mu.Unlock()

	id := agent.SessionID(fmt.Sprintf("session_%02d", a.nextID))
	if resume != "" {
		id = resume
	}
	return &agent.Result{SessionID: id, Text: "stub reply"}, nil
}

// ---- test bridge factory -------------------------------------------------

// newTestBridge wires a Bridge with a fresh fakeSender and agentStub,
// backed by a temp dir for persistence. Every test gets isolated state.
func newTestBridge(t *testing.T) (*Bridge, *fakeSender, *agentStub) {
	t.Helper()
	dir := t.TempDir()

	cfg := &config.Config{
		Bots: []config.BotConfig{{
			ID:           "main",
			DisplayName:  "claude-pc",
			Workspace:    dir,
			AllowedUsers: []string{"*"},
		}},
		Agent: config.AgentConfig{
			Command:    "claude",
			TimeoutSec: 30,
		},
		Stream: config.StreamCfg{
			ThrottleMs: 0,
		},
	}

	logger := log.New(os.Stderr, "[test] ", 0)
	fs := &fakeSender{}
	as := &agentStub{}

	b := &Bridge{
		cfg:             cfg,
		cli:             fs,
		log:             logger,
		store:           newSessionStore(dir, logger.Printf),
		sessReaper:      newReaper(0, logger.Printf),
		recentDirs:      newRecentDirsStore(dir, logger.Printf),
		inflight:        make(map[string]*turn),
		pendingSessions: make(map[string]string),
		topicCwd:        make(map[string]string),
		runAgent:        as,
	}
	return b, fs, as
}

// ---- tests ---------------------------------------------------------------

// TestMainChatPlainText_SendsConsoleCard verifies the redesign's
// central interaction: plain text in the main chat does NOT reach
// claude, it gets a console card back with recent-directory buttons.
func TestMainChatPlainText_SendsConsoleCard(t *testing.T) {
	b, fs, as := newTestBridge(t)
	ctx := context.Background()

	b.recentDirs.Add("main", "/api")
	b.recentDirs.Add("main", "/work")

	if err := b.onMessage(ctx, &feishu.Message{
		ChatID:    "oc_1",
		ThreadID:  "",
		MessageID: "om_1",
		RawText:   "分析一下这个目录",
	}); err != nil {
		t.Fatalf("onMessage: %v", err)
	}

	if len(as.prompts) != 0 {
		t.Errorf("agent invoked %d times; main chat plain text must not call claude", len(as.prompts))
	}

	btns := cardButtons(fs.lastCard())
	if len(btns) == 0 {
		t.Fatal("console card has no buttons")
	}
	if len(btns) > 5 {
		t.Errorf("console card has %d buttons; max is 5", len(btns))
	}
	for _, btn := range btns {
		if btn.Value["action"] != "new_topic" {
			t.Errorf("button action = %q, want new_topic", btn.Value["action"])
		}
		if btn.Value["cwd"] == "" {
			t.Errorf("button cwd empty")
		}
	}
}

// TestNew_CreatesTopic_WithSessionButtons verifies /new opens a topic
// whose root card carries history-session buttons plus [🆕 新].
func TestNew_CreatesTopic_WithSessionButtons(t *testing.T) {
	b, fs, _ := newTestBridge(t)
	ctx := context.Background()
	cwd := b.cfg.Bots[0].Workspace

	// Seed a session under bot.Workspace so topic-root has history.
	b.store.Set(&session{
		ID:       agent.SessionID("S1"),
		BotID:    "main",
		ChatID:   "oc_other",
		ThreadID: "omt_old",
		Cwd:      cwd,
	})
	b.store.Save()

	if err := b.onMessage(ctx, &feishu.Message{
		ChatID:    "oc_1",
		ThreadID:  "",
		MessageID: "om_1",
		RawText:   "/new",
	}); err != nil {
		t.Fatalf("onMessage: %v", err)
	}

	btns := cardButtons(fs.lastCard())
	if len(btns) == 0 {
		t.Fatal("root card has no buttons")
	}
	var hasNew, hasResume bool
	for _, btn := range btns {
		switch btn.Value["action"] {
		case "new_session":
			hasNew = true
		case "resume_session":
			hasResume = true
		}
	}
	if !hasNew {
		t.Error("root card missing [🆕 新] button")
	}
	if !hasResume {
		t.Error("root card missing resume_session button (history not rendered)")
	}
}

// TestClickResumeSession_SetsPending verifies the resume_session card
// action sets a pending resume, and the next runTurn in that thread
// consumes it.
func TestClickResumeSession_SetsPending(t *testing.T) {
	b, _, as := newTestBridge(t)
	ctx := context.Background()

	_, _ = b.onCardAction(ctx, &feishu.CardAction{
		ChatID:    "oc_1",
		MessageID: "om_root",
		Operator:  "ou_user",
		Action: map[string]any{
			"action":  "resume_session",
			"thread":  "omt_1",
			"root":    "om_root",
			"session": "S1",
		},
	})

	_ = b.onMessage(ctx, &feishu.Message{
		ChatID:    "oc_1",
		ThreadID:  "omt_1",
		MessageID: "om_msg",
		RawText:   "继续",
	})

	if len(as.resumes) != 1 {
		t.Fatalf("resumes called %d times; want 1", len(as.resumes))
	}
	if as.resumes[0] != "S1" {
		t.Errorf("resumes[0] = %q, want S1", as.resumes[0])
	}
}

// TestClickNewSession_ClearsPending verifies [🆕 新] clears any
// pending resume so the next message starts a fresh session.
func TestClickNewSession_ClearsPending(t *testing.T) {
	b, _, as := newTestBridge(t)
	ctx := context.Background()

	b.setPendingSession("omt_1", "S1")
	_, _ = b.onCardAction(ctx, &feishu.CardAction{
		ChatID:    "oc_1",
		MessageID: "om_root",
		Operator:  "ou_user",
		Action: map[string]any{
			"action": "new_session",
			"thread": "omt_1",
			"root":   "om_root",
		},
	})

	_ = b.onMessage(ctx, &feishu.Message{
		ChatID:    "oc_1",
		ThreadID:  "omt_1",
		MessageID: "om_msg",
		RawText:   "hello",
	})

	if len(as.resumes) != 1 {
		t.Fatalf("resumes called %d times; want 1", len(as.resumes))
	}
	if as.resumes[0] != "" {
		t.Errorf("resumes[0] = %q, want empty (fresh session)", as.resumes[0])
	}
}

// TestRunTurn_SessionCwdIsAuthoritative verifies that once a session
// record is created with a Cwd, subsequent turns in that thread use
// that Cwd — not the bot's workspace and not some fallback.
func TestRunTurn_SessionCwdIsAuthoritative(t *testing.T) {
	b, _, as := newTestBridge(t)
	ctx := context.Background()

	cwd := filepath.Join(t.TempDir(), "custom-cwd")
	b.store.Set(&session{
		ID:       agent.SessionID("S1"),
		BotID:    "main",
		ChatID:   "oc_1",
		ThreadID: "omt_1",
		Cwd:      cwd,
	})

	_ = b.onMessage(ctx, &feishu.Message{
		ChatID:    "oc_1",
		ThreadID:  "omt_1",
		MessageID: "om_msg",
		RawText:   "hello",
	})

	if len(as.cfgs) != 1 {
		t.Fatalf("agent invoked %d times; want 1", len(as.cfgs))
	}
	if as.cfgs[0].Workspace != cwd {
		t.Errorf("agent workspace = %q, want %q", as.cfgs[0].Workspace, cwd)
	}
}

// TestNew_WithExplicitCwd verifies /new <cwd> uses the given cwd and
// promotes it into the recentDirs LRU.
func TestNew_WithExplicitCwd(t *testing.T) {
	b, fs, _ := newTestBridge(t)
	ctx := context.Background()

	work := filepath.Join(t.TempDir(), "work")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}

	_ = b.onMessage(ctx, &feishu.Message{
		ChatID:    "oc_1",
		ThreadID:  "",
		MessageID: "om_1",
		RawText:   "/new " + work,
	})

	if got := b.recentDirs.MostRecent("main"); got != work {
		t.Errorf("MostRecent = %q, want %q", got, work)
	}
	if !cardContains(fs.lastCard(), work) {
		t.Errorf("root card does not mention cwd %q", work)
	}
}

// TestNew_InvalidCwd_ErrorsWithoutTopic verifies that /new with a
// non-existent cwd returns an error card without creating a topic or
// promoting the cwd into the recentDirs LRU.
//
// The bad cwd is built relative to the temp dir so it's a real absolute
// path on both Windows and Unix — "/no/such/path" is treated as relative
// on Windows and would silently join to bot.Workspace, which isn't the
// case we want to test.
func TestNew_InvalidCwd_ErrorsWithoutTopic(t *testing.T) {
	b, fs, _ := newTestBridge(t)
	ctx := context.Background()

	bad := filepath.Join(t.TempDir(), "no", "such", "path")

	_ = b.onMessage(ctx, &feishu.Message{
		ChatID:    "oc_1",
		ThreadID:  "",
		MessageID: "om_1",
		RawText:   "/new " + bad,
	})

	if !cardContains(fs.lastCard(), bad) {
		j, _ := json.MarshalIndent(fs.lastCard(), "", "  ")
		t.Errorf("error card does not mention the bad cwd %q; card = %s", bad, j)
	}
	if got := b.recentDirs.MostRecent("main"); got != "" {
		t.Errorf("MostRecent = %q after failed /new; want empty", got)
	}
}

// TestNew_PromotesCwdOrder verifies /new twice with two different
// cwds leaves the LRU in the right order (most-recent first).
func TestNew_PromotesCwdOrder(t *testing.T) {
	b, _, _ := newTestBridge(t)
	ctx := context.Background()

	work := filepath.Join(t.TempDir(), "work")
	api := filepath.Join(t.TempDir(), "api")
	_ = os.MkdirAll(work, 0o755)
	_ = os.MkdirAll(api, 0o755)

	_ = b.onMessage(ctx, &feishu.Message{
		ChatID: "oc_1", MessageID: "om_1", RawText: "/new " + work,
	})
	_ = b.onMessage(ctx, &feishu.Message{
		ChatID: "oc_1", MessageID: "om_2", RawText: "/new " + api,
	})

	if got := b.recentDirs.MostRecent("main"); got != api {
		t.Errorf("MostRecent = %q, want %q", got, api)
	}
	got := b.recentDirs.List("main")
	if len(got) != 2 {
		t.Fatalf("recentDirs len = %d, want 2; got %v", len(got), got)
	}
}

// TestFirstTurnUsesTopicCwd verifies that when /new creates a topic with
// an explicit cwd, the very first turn in that topic runs the agent in
// that cwd — not bot.Workspace. Regression test for the bug where the
// session record wasn't created until after the first turn's agent.Run,
// so the agent would inherit bot.Workspace even though the topic's root
// card correctly advertised the target cwd.
func TestFirstTurnUsesTopicCwd(t *testing.T) {
	b, _, as := newTestBridge(t)
	ctx := context.Background()

	work := filepath.Join(t.TempDir(), "work")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}

	// /new <cwd> in the main chat creates the topic; the bridge records
	// the cwd under the topic's thread id so currentCwd can resolve it
	// on the first turn, before any session record exists.
	if err := b.onMessage(ctx, &feishu.Message{
		ChatID:    "oc_1",
		ThreadID:  "",
		MessageID: "om_root",
		RawText:   "/new " + work,
	}); err != nil {
		t.Fatalf("onMessage(/new): %v", err)
	}

	// First turn inside the new topic. The fake sender assigned the
	// topic root the message id "msg_1" and the topic a thread id
	// derived from it.
	threadID := "omt_msg_1"
	_ = b.onMessage(ctx, &feishu.Message{
		ChatID:    "oc_1",
		ThreadID:  threadID,
		MessageID: "om_msg",
		RawText:   "hello",
	})

	if len(as.cfgs) != 1 {
		t.Fatalf("agent invoked %d times; want 1", len(as.cfgs))
	}
	if as.cfgs[0].Workspace != work {
		t.Errorf("agent workspace = %q, want %q (topic cwd, not bot workspace)",
			as.cfgs[0].Workspace, work)
	}
}

// TestTopicReplyCard_HasNoButtons verifies the redesign's rule that
// claude replies inside a topic carry no buttons.
func TestTopicReplyCard_HasNoButtons(t *testing.T) {
	b, fs, _ := newTestBridge(t)
	ctx := context.Background()

	_ = b.onMessage(ctx, &feishu.Message{
		ChatID:    "oc_1",
		ThreadID:  "omt_1",
		MessageID: "om_msg",
		RawText:   "hello",
	})

	if btns := cardButtons(fs.lastCard()); len(btns) != 0 {
		t.Errorf("topic reply card has %d buttons; want 0", len(btns))
	}
}

// ---- card helpers --------------------------------------------------------

// testButton is a lightweight shim over a rendered Feishu card button.
type testButton struct {
	Text  string
	Value map[string]string
}

// cardButtons walks body.elements[] and returns only the button
// entries, extracting the plain_text content and the value map.
//
// Card builders produce body.elements as []map[string]any, not []any.
// Accept both so tests can be resilient to future changes in the
// card package's element type.
func cardButtons(c map[string]any) []testButton {
	out := []testButton{}
	body, _ := c["body"].(map[string]any)
	if body == nil {
		return out
	}
	elements, _ := body["elements"].([]any)
	if elements == nil {
		// Some builders produce []map[string]any directly; marshal+unmarshal
		// to normalise to []any (or just iterate the concrete slice).
		if typed, ok := body["elements"].([]map[string]any); ok {
			elements = make([]any, len(typed))
			for i, m := range typed {
				elements[i] = m
			}
		} else {
			return out
		}
	}
	for _, el := range elements {
		m, ok := el.(map[string]any)
		if !ok {
			continue
		}
		if tag, _ := m["tag"].(string); tag != "button" {
			continue
		}
		text := ""
		if t, ok := m["text"].(map[string]any); ok {
			if s, ok := t["content"].(string); ok {
				text = s
			}
		}
		value := map[string]string{}
		if v, ok := m["value"].(map[string]any); ok {
			for k, vv := range v {
				if s, ok := vv.(string); ok {
					value[k] = s
				}
			}
		}
		out = append(out, testButton{Text: text, Value: value})
	}
	return out
}

// cardContains reports whether the card contains the needle in any
// string value. Walks the tree recursively — cheap, order-independent
// assertion for "did this text show up in the card somewhere".
//
// We walk rather than JSON-marshall because Windows backslashes get
// escaped by json.Marshal, so a literal substring search on the
// marshalled bytes would fail for any path on Windows.
func cardContains(c map[string]any, needle string) bool {
	if c == nil || needle == "" {
		return false
	}
	return containsIn(c, needle)
}

func containsIn(v any, needle string) bool {
	switch x := v.(type) {
	case string:
		return strings.Contains(x, needle)
	case map[string]any:
		for _, vv := range x {
			if containsIn(vv, needle) {
				return true
			}
		}
	case []any:
		for _, vv := range x {
			if containsIn(vv, needle) {
				return true
			}
		}
	case []map[string]any:
		for _, m := range x {
			if containsIn(m, needle) {
				return true
			}
		}
	}
	return false
}
