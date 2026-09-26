// Package bridge wires the Feishu client, the topic-scoped working directory
// and the Claude Code agent together.
//
// Main chat is a session-management console (/new only); topics are
// claude conversations with a cwd fixed at creation time. No shell
// process is spawned, so the package needs no external shell binary and
// builds the same on every architecture.
package bridge

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"feishubridge/internal/agent"
	"feishubridge/internal/card"
	"feishubridge/internal/config"
	"feishubridge/internal/feishu"
)

// feishuSender is the slice of feishu.Client that Bridge actually calls.
// Defined here (rather than in feishu) because it is a bridge-side concern:
// feishu.Client satisfies it structurally, but tests can supply a fake
// without a websocket.
type feishuSender interface {
	SendCard(ctx context.Context, chatID string, card map[string]any) (string, error)
	SendCardInThread(ctx context.Context, chatID, threadID string, card map[string]any) (string, error)
	PatchCard(ctx context.Context, messageID string, card map[string]any) error
	ReplyCard(ctx context.Context, sourceMessageID string, card map[string]any, thread bool) (string, error)
	ReplyCardThreaded(ctx context.Context, sourceMessageID string, card map[string]any, thread bool) (messageID, threadID string, err error)
	StartWS(ctx context.Context) error
}

// agentRunner is the shape of agent.Run. A single-method interface so tests
// can stub it without invoking a real `claude` subprocess.
type agentRunner interface {
	Run(ctx context.Context, cfg agent.Config, prompt string, resume agent.SessionID, onEvent func(agent.Event)) (*agent.Result, error)
}

// realAgentRunner adapts the package-level agent.Run to the interface.
type realAgentRunner struct{}

func (realAgentRunner) Run(ctx context.Context, cfg agent.Config, prompt string, resume agent.SessionID, onEvent func(agent.Event)) (*agent.Result, error) {
	return agent.Run(ctx, cfg, prompt, resume, onEvent)
}

// Bridge is the top-level coordinator.
type Bridge struct {
	cfg *config.Config
	cli feishuSender
	log *log.Logger

	// store is the session table. session.Cwd is the authoritative cwd
	// for a topic (there is no separate b.cwds map anymore).
	store      *sessionStore
	sessReaper *reaper
	// recentDirs is the bot-level LRU of recently opened cwds, persisted
	// next to the session table. newTopic pushes into it so the console
	// card's "recent directories" buttons can list it.
	recentDirs *recentDirsStore
	// runAgent is the entry point into the claude subprocess. Production
	// uses realAgentRunner; tests can swap in a stub that never shells out.
	runAgent agentRunner
	// inflight tracks turns currently running so /stop-equivalent flows can
	// cancel them. Retained for future use; the redesign drops the /stop
	// command but keeping the field means context cancellation at shutdown
	// can still be plumbed in later.
	//
	// mu protects inflight (and the reaper's snapshot of it, if any).
	// It is deliberately not sessionStore's lock — the store has its own,
	// and cross-locking them invites the exact deadlock CLAUDE.md §6 warns
	// about.
	mu       sync.Mutex
	inflight map[string]*turn
	// pendingSessions maps threadID -> resume session id. When the user
	// clicks a "▶ <session>" button on a topic's root card, this slot is
	// filled. The next plain message in that topic consumes it: the agent
	// is invoked with --resume <id>. Cleared after consumption so a stale
	// click doesn't re-resume an old session.
	//
	// Deliberately not persisted: a restart means pending state is lost,
	// and the user just starts a new session on the next message. Cheaper
	// than serialising ephemeral intent.
	//
	// Guarded by pendingMu. Kept separate from store's lock because the
	// consumption path (runTurn) holds store's lock while reading and
	// would otherwise risk re-entrancy if pendingSessions lived there.
	pendingMu       sync.Mutex
	pendingSessions map[string]string

	startedAt time.Time
}

// session is layer 2: one Claude conversation per thread.
//
// BotID/ChatID/ThreadID are stored explicitly rather than parsed out of the
// map key, so a persisted session can be re-hydrated and re-keyed without
// string surgery.
type session struct {
	ID       agent.SessionID
	BotID    string
	ChatID   string
	ThreadID string
	// Cwd is the working directory the session was created in. It is written
	// once when the session is first materialised and never mutated; /cd is
	// gone, so a topic's cwd is fixed for its lifetime. Persisted so a
	// restarted bridge can still resolve the topic's cwd from the session
	// record alone.
	Cwd       string
	CreatedAt time.Time
	LastSeen  time.Time
	Turns     int
}

// turn is one in-flight agent invocation.
type turn struct {
	Cancel context.CancelFunc
	ChatID string
}

// New constructs a bridge.
func New(cfg *config.Config, cli *feishu.Client, logger *log.Logger) *Bridge {
	if logger == nil {
		logger = log.Default()
	}
	b := &Bridge{
		cfg:             cfg,
		cli:             cli,
		log:             logger,
		inflight:        make(map[string]*turn),
		pendingSessions: make(map[string]string),
		runAgent:        realAgentRunner{},
	}
	b.store = newSessionStore(cfg.Bots[0].Workspace, logger.Printf)
	b.sessReaper = newReaper(cfg.Stream.SessionIdleSec, logger.Printf)
	b.recentDirs = newRecentDirsStore(cfg.Bots[0].Workspace, logger.Printf)

	cli.SetHandlers(&feishu.EventHandlers{
		OnMessage:    b.onMessage,
		OnCardAction: b.onCardAction,
	})
	return b
}

// Run starts the websocket loop and blocks until ctx is cancelled.
func (b *Bridge) Run(ctx context.Context) error {
	b.startedAt = time.Now()

	// Session records are reaped on an interval: they carry no process, so no
	// per-chat lock is needed and the cutoff is much longer. See sessions.go.
	go b.sessReaper.Run(ctx, b.collectIdleSessions, b.stopIdleSessions)

	return b.cli.StartWS(ctx)
}

// collectIdleSessions returns keys of session records that have gone quiet
// past the cutoff.
func (b *Bridge) collectIdleSessions() []string {
	idle := b.store.Idle(b.sessReaper.cutoff())
	keys := make([]string, 0, len(idle))
	for _, se := range idle {
		keys = append(keys, sessionKey(se.BotID, se.ChatID, se.ThreadID))
	}
	return keys
}

// stopIdleSessions drops stale session records and persists. There is no
// process to stop and no per-chat lock to acquire.
func (b *Bridge) stopIdleSessions(ctx context.Context, keys []string) {
	if len(keys) == 0 {
		return
	}
	n := b.store.ClearIdle(b.sessReaper.cutoff())
	if n == 0 {
		return
	}
	b.log.Printf("reaper: dropped %d idle session record(s) (idle>%s)",
		n, b.sessReaper.cutoff())
	b.store.Save()
}

// ---- inbound -------------------------------------------------------------

func (b *Bridge) onMessage(ctx context.Context, m *feishu.Message) error {
	if m.SenderType == "app" {
		return nil // ignore ourselves
	}

	bot := resolveBot(b.cfg, b.log, m.ChatID)
	if bot == nil {
		return nil
	}

	if !bot.AllowedUser(m.SenderID) {
		b.log.Printf("reject: %s not allowed for bot %s", m.SenderID, bot.ID)
		b.sendReject(ctx, m.ChatID, m.MessageID)
		return nil
	}

	text := strings.TrimSpace(m.RawText)
	if text == "" {
		return nil
	}

	// Group gate: require @bot mention in group chats.
	if m.ChatID != "" && strings.HasPrefix(m.ChatID, "oc_") {
		if !b.groupGate(ctx, bot, m) {
			return nil
		}
	}

	// Main chat is a strict console: /new is the only accepted command, and
	// plain text never reaches claude from here — it just gets a console
	// card back. Topics are pure claude conversations: anything that isn't
	// /new (there is only one, and there isn't one, in a topic) is a turn.
	if isBridgeCommand(text) {
		return b.handleCommand(ctx, bot, m, text)
	}
	if m.ThreadID == "" {
		// Main chat, plain text: not a command, not a turn. Reply with the
		// console card so the user sees the recent-directory shortcuts.
		return b.replyConsoleCard(ctx, bot, m)
	}
	return b.runTurn(ctx, bot, m, text)
}

// ---- command routing -----------------------------------------------------
//
// There is exactly one command: /new [cwd?]. See docs/design-session-and-test.md
// §3.3 for the reasoning — the console is a thin launcher, and everything
// else (interrupting, status, help, session management) belongs to the claude
// conversation in a topic or to the card buttons.

func (b *Bridge) handleCommand(ctx context.Context, bot *config.BotConfig, m *feishu.Message, text string) error {
	// isBridgeCommand already gated on first token being /new, so the
	// argument is everything after "/new ". Empty means "use most recent".
	cwd := parseNewArgs(text)
	return b.newTopic(ctx, bot, m, cwd)
}

// replyConsoleCard is the main-chat response to non-command text. The
// redesign's central insight is that plain text in the main chat should
// never reach claude — it's noise, and the user almost certainly wanted
// to open a topic. Show the console card with recent-directory buttons.
func (b *Bridge) replyConsoleCard(ctx context.Context, bot *config.BotConfig, m *feishu.Message) error {
	cwds := b.recentDirs.List(bot.ID)
	btns := recentDirButtons(bot, cwds)
	body := "**这里是会话管理控制台,不启动 claude。**\n\n**最近目录**(点击=用该 cwd 开新话题):"
	return b.replyCard(ctx, m, card.CommandCard(bot.DisplayName, "⚠️ 主会话是控制台", body, btns...))
}

// currentCwd returns the cwd the agent should run in for this message's
// topic. Precedence:
//  1. pendingSessions[thread] is not consulted here — that's a session id,
//     not a cwd.
//  2. The topic's session record, if any. session.Cwd is written by
//     runTurn on the first turn and is authoritative thereafter.
//  3. Fall back to the bot's configured workspace. This is only reached for
//     brand-new topics with no session yet (immediately after /new).
func (b *Bridge) currentCwd(bot *config.BotConfig, m *feishu.Message) string {
	if m.ThreadID != "" {
		if s := b.store.Get(bot.ID, m.ChatID, m.ThreadID); s != nil && s.Cwd != "" {
			return s.Cwd
		}
	}
	return bot.Workspace
}

// ---- the turn ------------------------------------------------------------

// takePendingSession returns the thread's pending resume id and clears the
// slot. Called at the top of runTurn so a "▶ <session>" click affects
// exactly the next turn, then falls back to the topic's current session
// thereafter.
func (b *Bridge) takePendingSession(threadID string) string {
	b.pendingMu.Lock()
	defer b.pendingMu.Unlock()
	id := b.pendingSessions[threadID]
	delete(b.pendingSessions, threadID)
	return id
}

// setPendingSession records which session the next message in this topic
// should resume. Empty string clears the slot (equivalent to clicking
// "🆕 新").
func (b *Bridge) setPendingSession(threadID, sessionID string) {
	b.pendingMu.Lock()
	defer b.pendingMu.Unlock()
	if sessionID == "" {
		delete(b.pendingSessions, threadID)
		return
	}
	b.pendingSessions[threadID] = sessionID
}

func (b *Bridge) runTurn(ctx context.Context, bot *config.BotConfig, m *feishu.Message, text string) error {
	// Show the thinking card immediately.
	msgID, err := b.cli.SendCardInThread(ctx, m.ChatID, m.ThreadID, card.Processing(bot.DisplayName))
	if err != nil {
		b.log.Printf("send processing card: %v", err)
	}

	// Resolve or create the Claude session for this thread. The store is
	// self-locking; it also holds the persistent session_id mapping.
	//
	// Priority for the resume target:
	//  1. A pending resume set by a click on a "▶ <session>" button in
	//     this topic's root card. Consumed once, then cleared, so the
	//     click affects exactly one turn.
	//  2. The topic's existing session (multi-turn continuation).
	//  3. Empty string: fresh session.
	sessionKey := sessionKey(bot.ID, m.ChatID, m.ThreadID)
	resume := ""
	if m.ThreadID != "" {
		if p := b.takePendingSession(m.ThreadID); p != "" {
			resume = p
		}
	}
	if resume == "" {
		if s := b.store.Get(bot.ID, m.ChatID, m.ThreadID); s != nil && s.ID != "" {
			resume = string(s.ID)
		}
	}

	// The agent inherits the user's cwd so it lands where /cd left the user,
	// not at the bot's configured workspace.
	workspace := b.currentCwd(bot, m)

	// Track the in-flight turn so /stop can cancel it.
	turnCtx, cancel := context.WithCancel(ctx)
	b.mu.Lock()
	b.inflight[sessionKey] = &turn{Cancel: cancel, ChatID: m.ChatID}
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		delete(b.inflight, sessionKey)
		b.mu.Unlock()
	}()

	cfg := agent.Config{
		Command:    b.cfg.Agent.Command,
		Model:      b.cfg.Agent.Model,
		Workspace:  workspace,
		Timeout:    time.Duration(b.cfg.Agent.TimeoutSec) * time.Second,
		AppendSys:  b.cfg.Agent.AppendSys,
		SettingSrc: b.cfg.Agent.SettingSrc,
	}

	// Stream events into the card with throttling.
	throttle := time.Duration(b.cfg.Stream.ThrottleMs) * time.Millisecond
	lastFlush := time.Time{}

	flush := func(ev agent.Event, force bool) {
		now := time.Now()
		if !force && now.Sub(lastFlush) < throttle {
			return
		}
		lastFlush = now
		text := ev.Text
		if text == "" {
			text = "（等待首个回复...）"
		}
		if msgID != "" {
			if e := b.cli.PatchCard(ctx, msgID, card.Streaming(bot.DisplayName, text)); e != nil {
				b.log.Printf("stream patch: %v", e)
			}
		}
	}

	// latestEv is captured so an interrupted turn still reports what it got.
	var latestEv agent.Event

	started := time.Now()
	res, err := b.runAgent.Run(turnCtx, cfg, text, agent.SessionID(resume), func(ev agent.Event) {
		latestEv = ev
		switch ev.Type {
		case "stream_event", "assistant":
			flush(ev, false)
		case "result":
			flush(ev, true)
		}
	})

	elapsed := time.Since(started)

	// Update the session record and persist it.
	if res != nil && string(res.SessionID) != "" {
		existing := b.store.Get(bot.ID, m.ChatID, m.ThreadID)
		se := &session{
			ID:       res.SessionID,
			BotID:    bot.ID,
			ChatID:   m.ChatID,
			ThreadID: m.ThreadID,
			Turns:    1,
		}
		se.LastSeen = time.Now()
		if existing != nil {
			se.CreatedAt = existing.CreatedAt
			se.Turns = existing.Turns + 1
			// Cwd is sticky: once written on the first turn it never moves.
			se.Cwd = existing.Cwd
		} else {
			se.CreatedAt = time.Now()
			// The workspace passed to the agent is authoritative: that's the
			// cwd the session actually ran in.
			se.Cwd = workspace
		}
		b.store.Set(se)
		b.store.Save()
	}

	if err != nil {
		b.log.Printf("turn error chat=%s: %v", m.ChatID, err)
		var body string
		if e := turnCtx.Err(); e != nil {
			body = "⏹ 任务已取消。\n\n`" + e.Error() + "`"
		} else if res != nil && res.Text != "" {
			body = res.Text
		} else {
			body = err.Error()
		}
		if latestEv.ToolName != "" && body == err.Error() {
			body += "\n\n（最后动作: `" + latestEv.ToolName + "`）"
		}
		b.patchOrSend(ctx, m, msgID, card.Error(bot.DisplayName, card.Sanitize(body)))
		return nil
	}

	if res.Text == "" {
		res.Text = "_（本次无输出）_"
	}

	// Inside a topic the reply names the session it is bound to, so the user
	// can reconcile the conversation with a local `claude --resume` session.
	body := card.Sanitize(res.Text)
	if m.ThreadID != "" {
		body += fmt.Sprintf("\n\n_本话题 Claude 会话 `%s` · 工作目录 `%s`_",
			shortID(string(res.SessionID)), workspace)
	}

	// The reply card carries no buttons: session management lives on the
	// topic-root card, not on every reply. See docs/design-session-and-test.md
	// §3.2.2.
	footer := card.FormatUsage(res.Usage, res.ModelUsage, res.CostUSD, elapsed)
	final := card.Done(bot.DisplayName, body, footer)
	b.patchOrSend(ctx, m, msgID, final)
	return nil
}

// ---- helpers -------------------------------------------------------------

func (b *Bridge) patchOrSend(ctx context.Context, m *feishu.Message, msgID string, c map[string]any) {
	if msgID != "" {
		if err := b.cli.PatchCard(ctx, msgID, c); err != nil {
			b.log.Printf("patch card failed, sending new: %v", err)
			b.cli.SendCardInThread(ctx, m.ChatID, m.ThreadID, c)
		}
		return
	}
	b.cli.SendCardInThread(ctx, m.ChatID, m.ThreadID, c)
}

func (b *Bridge) replyCard(ctx context.Context, m *feishu.Message, c map[string]any) error {
	_, err := b.cli.ReplyCard(ctx, m.MessageID, c, m.ThreadID != "")
	if err != nil {
		b.log.Printf("reply card: %v", err)
	}
	return err
}

func (b *Bridge) sendReject(ctx context.Context, chatID, messageID string) {
	c := card.CommandCard("Bridge", "无权限", "你不在该 bot 的允许名单中。")
	if messageID != "" {
		b.cli.ReplyCard(ctx, messageID, c, false)
		return
	}
	b.cli.SendCard(ctx, chatID, c)
}

// newTopic starts a fresh Claude session by starting a fresh Feishu topic.
//
// The user-visible object is the topic, not the session id: a "new session"
// that kept the old topic would be invisible, since every card in it still
// shows the old thread. Posting a message with reply_in_thread=true creates a
// topic whose root is that message, so replies afterwards land there.
//
// The optional cwd argument lets /new take a target directory. Empty cwd
// means "use the most recently used directory" (bot-level LRU). When both
// cwd is empty and the LRU is empty, fall back to the bot's configured
// workspace.
func (b *Bridge) newTopic(ctx context.Context, bot *config.BotConfig, m *feishu.Message, cwd string) error {
	// m.MessageID is the topic's root when the click came from inside a topic,
	// and the clicked card otherwise. Either is a valid anchor: reply
	// reply_in_thread=true forks a topic only when the anchor is not already
	// inside one.
	if m.MessageID == "" {
		b.log.Printf("new topic: no anchor (chat=%s)", shortKey(m.ChatID))
		return nil
	}

	// Resolve the target cwd. Precedence: explicit arg > most-recent LRU >
	// bot's configured workspace. Relative paths are joined against the
	// bot's workspace; only real, existing directories are accepted, so a
	// typo in /new cannot silently strand later agent runs.
	target, err := b.resolveCwd(bot, cwd)
	if err != nil {
		return b.replyCard(ctx, m, card.Error(bot.DisplayName, err.Error()))
	}

	// The notice carries the new topic's identity, but the topic has not been
	// created yet, so it is built with no buttons and updated once Feishu
	// returns a thread_id.
	body := fmt.Sprintf("cwd: `%s`\n\n点击历史 session 续它,或点 [🆕 新] 从头开始:", target)
	c := card.CommandCard(bot.DisplayName, "🟢 新话题已开启", body)

	msgID, threadID, err := b.cli.ReplyCardThreaded(ctx, m.MessageID, c, true)
	if err != nil {
		b.log.Printf("new topic: threaded reply failed, replying inline: %v", err)
		if m.ThreadID == "" {
			// No topic to fork: a plain reply is correct here.
			return b.replyCard(ctx, m,
				card.CommandCard(bot.DisplayName, "🟢 新话题已开启", body,
					recentDirButtons(bot, b.recentDirs.List(bot.ID))...))
		}
		return nil
	}

	// Topic is real. Promote its cwd into the bot-level LRU so a future
	// no-arg /new and the console card can find it. Only after the topic
	// is real: an inherited cwd that never materialises into a topic is
	// not "recently used".
	b.recentDirs.Add(bot.ID, target)

	// The notice is now the topic's root card. Attach session-picker buttons:
	// up to 5 history sessions (from the byCwd index) plus [🆕 新]. Both
	// thread and root ids are stamped on every button because the callback
	// only carries the clicked card's own id — see CLAUDE.md §1.
	history := b.store.SessionsForCwd(target, 5)
	notice := card.CommandCard(bot.DisplayName, "🟢 新话题已开启", body,
		topicRootButtons(bot, threadID, msgID, history)...)
	if perr := b.cli.PatchCard(ctx, msgID, notice); perr != nil {
		b.log.Printf("new topic: patch notice: %v", perr)
	} else {
		b.log.Printf("new topic: notice patched %s with %d buttons", shortID(msgID), len(notice))
	}

	// No session record is created here. The first plain message in the
	// topic will create one (with session.Cwd = target) via runTurn. That
	// keeps "topic exists but no claude run yet" a distinct, valid state —
	// the user may click a history session button before sending anything.
	b.log.Printf("new topic: %s in chat %s cwd=%s history=%d",
		shortID(threadID), shortKey(m.ChatID), target, len(history))
	return nil
}

// resolveCwd validates and normalises the target directory for a /new.
// Empty input means "use most recent LRU entry, or bot.Workspace if empty".
// Relative paths are joined against bot.Workspace; only real directories
// pass.
func (b *Bridge) resolveCwd(bot *config.BotConfig, cwd string) (string, error) {
	if cwd == "" {
		if r := b.recentDirs.MostRecent(bot.ID); r != "" {
			cwd = r
		} else {
			cwd = bot.Workspace
		}
	}
	if !filepath.IsAbs(cwd) {
		cwd = filepath.Join(bot.Workspace, cwd)
	}
	info, err := os.Stat(cwd)
	if err != nil {
		return "", fmt.Errorf("路径不存在: %s", cwd)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("不是目录: %s", cwd)
	}
	return cwd, nil
}

// stopCurrent cancels the in-flight turn for this thread. It reports whether
// anything was running so the caller does not claim to have stopped a task
// that was not started.
//
// Kept even though no command currently calls it: the redesign drops /stop
// but context cancellation at shutdown (or a future /cancel-equivalent)
// still needs this entry point. Delete only if inflight is retired.
func (b *Bridge) stopCurrent(botID, chatID, threadID string) bool {
	key := sessionKey(botID, chatID, threadID)
	b.mu.Lock()
	defer b.mu.Unlock()
	if t, ok := b.inflight[key]; ok && t.Cancel != nil {
		t.Cancel()
		delete(b.inflight, key)
		return true
	}
	return false
}

// groupGate decides whether a group message should be handled.
func (b *Bridge) groupGate(_ context.Context, bot *config.BotConfig, m *feishu.Message) bool {
	if !strings.HasPrefix(m.ChatID, "oc_") {
		return true // p2p chat
	}

	// bot.OwnerOpenID is the bot's own open_id (it is not the owner's).
	// Reading it per-bot rather than from the client makes multi-bot setups
	// correct: each bot gets its own mention identity.
	botID := bot.OwnerOpenID
	switch bot.GroupMode {
	case "disabled":
		return false
	case "mention-all":
		// No bot open_id configured: degrade to accepting all group messages
		// from allowed users. Without it there is nothing to match mentions
		// against, and the reference project takes the same pass-through.
		if botID == "" {
			return true
		}
		return mentionsBot(m, botID)
	case "owner-only":
		// owner-only gates on the *sender* being the operator, which is a
		// different id from the bot's own. That sender id is not configured
		// anywhere, so this mode only checks the mention once botID is set.
		if botID == "" {
			return false
		}
		return mentionsBot(m, botID)
	}
	return true
}

// mentionsBot reports whether the message @-mentions the bot.
func mentionsBot(m *feishu.Message, botOpenID string) bool {
	for _, mn := range m.Mentions {
		if mn.OpenID != "" && mn.OpenID == botOpenID {
			return true
		}
	}
	return false
}

// resolveBot maps a chat to its bot.
//
// Single bot: every chat goes to it. Multi-bot: the operator must list chats
// explicitly in bot_chats, because there is no signal in the message to infer
// which workspace owns a conversation. An unmapped chat in a multi-bot setup
// returns nil and the message is dropped — failing closed beats guessing a
// workspace that may have live credentials or a shared shell.
func resolveBot(cfg *config.Config, logger *log.Logger, chatID string) *config.BotConfig {
	if len(cfg.Bots) == 1 {
		return &cfg.Bots[0]
	}
	botID, ok := cfg.BotChats[chatID]
	if !ok {
		logger.Printf("no bot_chats entry for chat %s; ignoring message", chatID)
		return nil
	}
	return cfg.FindBot(botID)
}

func (b *Bridge) onCardAction(ctx context.Context, req *feishu.CardAction) (*feishu.CardResponse, error) {
	if req == nil || req.ChatID == "" {
		return nil, nil
	}

	bot := resolveBot(b.cfg, b.log, req.ChatID)
	if bot == nil {
		return nil, nil
	}

	// Re-authorise by open_id: the card can be clicked by anyone in the chat,
	// so operator != the allowed list means ignore.
	if !bot.AllowedUser(req.Operator) {
		b.log.Printf("reject card action from %s on bot %s", req.Operator, bot.ID)
		return nil, nil
	}

	action := strings.ToLower(val(req.Action, "action"))

	// The callback carries only the clicked card's message id. The builder
	// stamps the topic's thread id and root message id into the button value,
	// because neither survives the round trip. Same story for cwd (stamped
	// on recentDirButtons) and session (stamped on topicRootButtons).
	thread := val(req.Action, "thread")
	root := val(req.Action, "root")
	cwd := val(req.Action, "cwd")
	sessionID := val(req.Action, "session")
	if root == "" {
		// Older cards predate the root stamp; the clicked card is the best
		// available anchor.
		root = req.MessageID
	}

	msg := &feishu.Message{
		ChatID:   req.ChatID,
		ThreadID: thread,
		SenderID: req.Operator,
	}
	if root == "" {
		b.log.Printf("card action without anchor action=%s", action)
		return nil, nil
	}
	// Anchor on the topic's root message, never on the clicked card's own id.
	// reply_in_thread=true forks a new topic only when the anchor is not
	// itself already inside one; a card that is itself a topic reply carries
	// no thread context in the callback, so anchoring on it would post into
	// the chat instead of the topic.
	msg.MessageID = root
	msg.ParentID = root
	msg.RootID = root

	b.log.Printf("card action %q from %s (chat=%s thread=%s cwd=%q session=%q)",
		action, shortID(req.Operator), shortKey(req.ChatID), shortID(thread), cwd, shortID(sessionID))
	switch action {
	case "new_topic":
		// Recent-directory button on the console card. Click = /new <cwd>.
		// No reply card; newTopic itself posts the topic-root notice.
		return &feishu.CardResponse{}, b.newTopic(ctx, bot, msg, cwd)
	case "resume_session":
		// History-session button on a topic-root card. Sets the pending
		// resume; the next plain message in this thread consumes it. Silent
		// acknowledgement so we don't spam the topic.
		if thread == "" {
			b.log.Printf("resume_session action without thread; ignoring")
			return &feishu.CardResponse{}, nil
		}
		b.setPendingSession(thread, sessionID)
		return &feishu.CardResponse{}, nil
	case "new_session":
		// [🆕 新] button on a topic-root card. Clears any pending resume
		// for this thread so the next plain message starts a fresh session.
		if thread == "" {
			b.log.Printf("new_session action without thread; ignoring")
			return &feishu.CardResponse{}, nil
		}
		b.setPendingSession(thread, "")
		return &feishu.CardResponse{}, nil
	default:
		// Legacy actions (help / new / stop / status) from older cards that
		// may still be sitting in a conversation. Silently ignored: the
		// redesign removed those commands, and answering here would
		// resurrect the old UX.
		b.log.Printf("unknown card action %q (legacy card)", action)
		return nil, nil
	}
}

// val reads a string out of a card action value map, tolerating nil and
// non-string entries.
func val(m map[string]any, k string) string {
	if m == nil {
		return ""
	}
	s, _ := m[k].(string)
	return s
}

// ---- small helpers -------------------------------------------------------

func chatKey(chatID string) string { return chatID }

func sessionKey(botID, chatID, threadID string) string {
	if threadID == "" {
		return botID + "/" + chatID
	}
	return botID + "/" + chatID + "/" + threadID
}

func shortID(s string) string {
	if len(s) > 8 {
		return s[:8] + "…"
	}
	return s
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}
