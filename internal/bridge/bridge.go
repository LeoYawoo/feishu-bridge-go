// Package bridge wires the Feishu client, the persistent PowerShell session
// and the Claude Code agent together.
package bridge

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"feishubridge/internal/agent"
	"feishubridge/internal/card"
	"feishubridge/internal/config"
	"feishubridge/internal/feishu"
	"feishubridge/internal/shell"
)

// Bridge is the top-level coordinator.
type Bridge struct {
	cfg *config.Config
	cli *feishu.Client
	log *log.Logger

	// shells and sessions are keyed by (botID, chatID) and by
	// (botID, chatID, threadID) respectively.
	mu     sync.Mutex
	shells map[string]*chatShell
	// shellLocks serialises first-touch creation of each chat's shell.
	shellLocks map[string]*sync.Mutex
	store      *sessionStore
	reaper     *reaper
	sessReaper *reaper
	// inflight tracks turns currently running, so /stop can cancel them.
	inflight map[string]*turn

	startedAt time.Time
}

// chatShell is layer 1: one pwsh per chat.
type chatShell struct {
	*shell.Shell
	Cwd string

	// lastSeen is the most recent time this chat asked for anything. It is
	// written under mu and read by the reaper under mu, so no atomic needed.
	lastSeen time.Time
}

// session is layer 2: one Claude conversation per thread.
//
// BotID/ChatID/ThreadID are stored explicitly rather than parsed out of the
// map key, so a persisted session can be re-hydrated and re-keyed without
// string surgery.
type session struct {
	ID        agent.SessionID
	BotID     string
	ChatID    string
	ThreadID  string
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
		cfg:        cfg,
		cli:        cli,
		log:        logger,
		shells:     make(map[string]*chatShell),
		shellLocks: make(map[string]*sync.Mutex),
		inflight:   make(map[string]*turn),
	}
	b.store = newSessionStore(cfg.Bots[0].Workspace, logger.Printf)
	b.reaper = newReaper(cfg.Stream.ShellIdleSec, logger.Printf)
	b.sessReaper = newReaper(cfg.Stream.SessionIdleSec, logger.Printf)

	cli.SetHandlers(&feishu.EventHandlers{
		OnMessage:    b.onMessage,
		OnCardAction: b.onCardAction,
	})
	return b
}

// Run starts the websocket loop and blocks until ctx is cancelled.
func (b *Bridge) Run(ctx context.Context) error {
	b.startedAt = time.Now()

	// Idle shell reaping runs alongside the websocket loop. See reaper below.
	go b.reaper.Run(ctx, b.collectIdleShells, b.stopIdle)

	// Session records are reaped independently: they carry no process, so no
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

// stopIdleSessions drops stale session records and persists. Unlike shell
// reaping there is no process and no per-chat lock to acquire.
func (b *Bridge) stopIdleSessions(ctx context.Context, keys []string) {
	if len(keys) == 0 {
		return
	}
	n := b.store.ClearIdle(b.sessReaper.cutoff())
	if n == 0 {
		return
	}
	b.log.Printf("reaper: dropped %d idle Claude session record(s) (idle>%s)",
		n, b.sessReaper.cutoff())
	b.store.Save()
}

// collectIdleShells reports shell keys idle beyond the configured threshold.
// Called with mu held; must not block or recurse into stopIdle.
func (b *Bridge) collectIdleShells() []string {
	cutoff := b.reaper.cutoff()
	if cutoff <= 0 {
		return nil // reaping disabled
	}
	var idle []string
	for key, cs := range b.shells {
		if time.Since(cs.lastSeen) > cutoff {
			idle = append(idle, key)
		}
	}
	return idle
}

// stopIdle tears down each idle shell and records its cwd so a later restart
// knows where the user left off.
//
// It re-acquires the per-chat creation lock for every victim, matching how
// ensureShell does it. Without that a restart racing a stop could observe the
// map entry, find the process gone, and start a second process against the
// same chat.
func (b *Bridge) stopIdle(ctx context.Context, keys []string) {
	for _, key := range keys {
		b.mu.Lock()
		cs, ok := b.shells[key]
		delete(b.shells, key)
		clk := b.shellLocks[key]
		b.mu.Unlock()

		if !ok || clk == nil {
			continue
		}
		clk.Lock()
		if cs.Shell.Started() {
			b.log.Printf("reaper: stopping idle shell %s (cwd=%s, idle>%s)",
				shortKey(key), cs.Cwd, b.reaper.cutoff())
			// Snapshot cwd+env before the process goes so a restart can pick
			// up where the user left off. Best effort: a failed snapshot still
			// leaves the user with a working shell, just at the workspace root.
			snapCtx, snapCancel := context.WithTimeout(ctx, 15*time.Second)
			if err := cs.Shell.SnapshotState(snapCtx); err != nil {
				b.log.Printf("reaper: snapshot shell %s: %v", shortKey(key), err)
			}
			snapCancel()
			if err := cs.Shell.Stop(); err != nil {
				b.log.Printf("reaper: stop shell %s: %v", shortKey(key), err)
			}
		}
		clk.Unlock()
	}
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

	// Commands are handled synchronously; everything else is a turn.
	if isBridgeCommand(text) {
		return b.handleCommand(ctx, bot, m, text)
	}
	return b.runTurn(ctx, bot, m, text)
}

// ---- command routing -----------------------------------------------------

func (b *Bridge) handleCommand(ctx context.Context, bot *config.BotConfig, m *feishu.Message, text string) error {
	fields := strings.Fields(text)
	cmd := strings.ToLower(fields[0])
	rest := ""
	if len(fields) > 1 {
		rest = strings.TrimSpace(text[len(fields[0]):])
	}

	switch cmd {
	case "/help", "/h":
		return b.replyCard(ctx, m, card.CommandCard(bot.DisplayName, "命令列表", helpText, standardButtons(bot, m)...))
	case "/pwd":
		return b.handleShellCommand(ctx, bot, m, "Get-Location -LiteralPath")
	case "/cd":
		if rest == "" {
			return b.replyCard(ctx, m, card.Error(bot.DisplayName, "用法: /cd <路径>"))
		}
		return b.handleShellCommand(ctx, bot, m, fmt.Sprintf("Set-Location -LiteralPath %s", quotePath(rest)))
	case "/ls":
		return b.handleShellCommand(ctx, bot, m, "Get-ChildItem -Force | Select-Object Mode, Length, Name | Format-Table -AutoSize | Out-String -Width 200")
	case "/ps":
		// Escape hatch for arbitrary PowerShell. Runs in the same persistent
		// session as /cd and /ls, so state carries over.
		if rest == "" {
			return b.replyCard(ctx, m, card.Error(bot.DisplayName, "用法: /ps <PowerShell 命令>"))
		}
		return b.handleShellCommand(ctx, bot, m, rest)
	case "/new", "/reset":
		b.resetSession(bot.ID, m.ChatID, m.ThreadID)
		return b.replyCard(ctx, m, card.CommandCard(bot.DisplayName, "新会话", "已开启新的 Claude 会话。"))
	case "/stop", "/cancel":
		b.stopCurrent(bot.ID, m.ChatID, m.ThreadID)
		return b.replyCard(ctx, m, card.CommandCard(bot.DisplayName, "已停止", "已请求取消当前任务。"))
	case "/status":
		return b.handleStatus(ctx, bot, m)
	case "/clear":
		return b.handleShellCommand(ctx, bot, m, "Clear-Host; Write-Host 'cleared'")
	case "/model":
		if rest == "" {
			model := b.cfg.Agent.Model
			return b.replyCard(ctx, m, card.CommandCard(bot.DisplayName, "模型", fmt.Sprintf("当前: `%s`（留空则用默认）", orDefault(model, "默认"))))
		}
		b.cfg.Agent.Model = rest
		return b.replyCard(ctx, m, card.CommandCard(bot.DisplayName, "模型已切换", fmt.Sprintf("当前模型: `%s`\n\n下次会话生效。", rest)))
	default:
		// Unknown commands fall through to the agent. This mirrors the
		// reference project, which only exact-matches known commands.
		return b.runTurn(ctx, bot, m, text)
	}
}

// ---- shell-layer commands ------------------------------------------------

func (b *Bridge) handleShellCommand(ctx context.Context, bot *config.BotConfig, m *feishu.Message, script string) error {
	sh, err := b.ensureShell(ctx, bot.ID, m.ChatID)
	if err != nil {
		return b.replyCard(ctx, m, card.Error(bot.DisplayName, fmt.Sprintf("shell 错误: %v", err)))
	}
	b.touch(sh)
	out, err := sh.Run(ctx, script)
	if out != "" {
		out = card.Sanitize(strings.TrimSpace(out))
	}
	if err != nil {
		text := out
		if text == "" {
			text = err.Error()
		}
		return b.replyCard(ctx, m, card.Error(bot.DisplayName, "```\n"+text+"\n```"))
	}
	if out == "" {
		out = "_（无输出）_"
	}
	return b.replyCard(ctx, m, card.CommandCard(bot.DisplayName, bot.Shell, "```\n"+out+"\n```", standardButtons(bot, m)...))
}

func (b *Bridge) handleStatus(ctx context.Context, bot *config.BotConfig, m *feishu.Message) error {
	key := m.ChatID
	b.mu.Lock()
	defer b.mu.Unlock()

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("**运行时间**: %s\n\n", time.Since(b.startedAt).Round(time.Second)))

	sh, ok := b.shells[key]
	if ok && sh.Shell.Started() {
		sb.WriteString(fmt.Sprintf("**Shell**: %s\n", bot.Shell))
		if sh.Cwd != "" {
			sb.WriteString(fmt.Sprintf("**当前目录**: `%s`\n", sh.Cwd))
		}
		sb.WriteString(fmt.Sprintf("**最后活动**: %s 前\n", time.Since(sh.lastSeen).Round(time.Second)))
		if b.reaper.cutoff() > 0 {
			sb.WriteString(fmt.Sprintf("（闲置 %s 后自动回收）\n", b.reaper.cutoff()))
		}
	} else {
		sb.WriteString("**Shell**: 未启动（首次命令时创建）\n")
		if sh != nil && sh.Cwd != "" {
			sb.WriteString(fmt.Sprintf("上次停止前目录: `%s`\n", sh.Cwd))
		}
	}

	if s := b.store.Get(bot.ID, m.ChatID, m.ThreadID); s != nil {
		sb.WriteString(fmt.Sprintf("\n**Claude 会话**: %s\n", shortID(string(s.ID))))
		sb.WriteString(fmt.Sprintf("**已处理轮次**: %d\n", s.Turns))
		sb.WriteString(fmt.Sprintf("**最后活动**: %s 前\n", time.Since(s.LastSeen).Round(time.Second)))
		if b.sessReaper.cutoff() > 0 {
			sb.WriteString(fmt.Sprintf("（闲置 %s 后丢弃，不影响 Claude 本地记录）\n", b.sessReaper.cutoff()))
		}
	} else {
		sb.WriteString("\n**Claude 会话**: 尚无会话\n")
	}
	sb.WriteString(fmt.Sprintf("**总会话数**: %d\n", b.store.Count()))
	return b.replyCard(ctx, m, card.CommandCard(bot.DisplayName, "状态", sb.String(), standardButtons(bot, m)...))
}

// ---- the turn ------------------------------------------------------------

func (b *Bridge) runTurn(ctx context.Context, bot *config.BotConfig, m *feishu.Message, text string) error {
	// Show the thinking card immediately.
	msgID, err := b.cli.SendCardInThread(ctx, m.ChatID, m.ThreadID, card.Processing(bot.DisplayName))
	if err != nil {
		b.log.Printf("send processing card: %v", err)
	}

	// Resolve or create the Claude session for this thread. The store is
	// self-locking; it also holds the persistent session_id mapping.
	sessionKey := sessionKey(bot.ID, m.ChatID, m.ThreadID)
	s := b.store.Get(bot.ID, m.ChatID, m.ThreadID)
	resume := ""
	if s != nil {
		resume = string(s.ID)
	}

	// Ensure the shell is alive so the agent inherits the user's cwd.
	sh, err := b.ensureShell(ctx, bot.ID, m.ChatID)
	if err != nil {
		b.log.Printf("shell ensure failed, falling back to workspace: %v", err)
	}
	workspace := bot.Workspace
	if sh != nil {
		b.touch(sh)
		// Resolve the shell's current directory so the agent inherits the
		// user's cwd rather than the bot's configured workspace.
		cwdCtx, cwdCancel := context.WithTimeout(ctx, 5*time.Second)
		cwd, e := sh.Run(cwdCtx, "(Get-Location -LiteralPath).Path")
		cwdCancel()
		if e == nil {
			if cwd = strings.TrimSpace(cwd); cwd != "" {
				workspace = cwd
				b.mu.Lock()
				sh.Cwd = cwd
				b.mu.Unlock()
			}
		} else {
			b.log.Printf("cwd lookup failed, using workspace: %v", e)
		}
	}

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
	res, err := agent.Run(turnCtx, cfg, text, agent.SessionID(resume), func(ev agent.Event) {
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
		} else {
			se.CreatedAt = time.Now()
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
	footer := card.FormatUsage(res.Usage, res.ModelUsage, res.CostUSD, elapsed)
	final := card.Done(bot.DisplayName, card.Sanitize(res.Text), footer, standardButtons(bot, m)...)
	b.patchOrSend(ctx, m, msgID, final)
	return nil
}

// ---- helpers -------------------------------------------------------------

// ensureShell returns the chat's persistent shell, creating it if needed.
//
// ensureShell returns the chat's persistent shell, creating it if needed.
//
// It uses a per-chat creation lock so two concurrent first-touches cannot
// both start a process; the second caller picks up the first's result.
func (b *Bridge) ensureShell(ctx context.Context, botID, chatID string) (*chatShell, error) {
	key := chatID
	b.mu.Lock()
	if cs, ok := b.shells[key]; ok && cs.Shell.Started() {
		b.mu.Unlock()
		return cs, nil
	}
	if _, ok := b.shellLocks[key]; !ok {
		b.shellLocks[key] = &sync.Mutex{}
	}
	clk := b.shellLocks[key]
	b.mu.Unlock()

	clk.Lock()
	defer clk.Unlock()

	// Re-check under the per-chat lock.
	b.mu.Lock()
	if cs, ok := b.shells[key]; ok && cs.Shell.Started() {
		b.mu.Unlock()
		return cs, nil
	}
	b.mu.Unlock()

	boot := b.cfg.FindBot(botID)
	if boot == nil {
		return nil, fmt.Errorf("no bot %q", botID)
	}

	cs := &chatShell{Shell: shell.New(shell.Config{
		Bin:             boot.Shell,
		Workspace:       boot.Workspace,
		Timeout:         60 * time.Second,
		ShutdownTimeout: 5 * time.Second,
		SnapshotDir:     boot.Workspace,
	}, b.log.Printf)}
	if err := cs.Shell.Start(ctx); err != nil {
		return nil, err
	}

	b.mu.Lock()
	cs.lastSeen = time.Now()
	b.shells[key] = cs
	b.mu.Unlock()
	return cs, nil
}

// touch marks a shell as recently used, pushing it past the reaper cutoff.
//
// Cheap enough to call from the hot path; it also records the last cwd seen
// so a reaped-and-restarted shell reports where it was before it went idle.
func (b *Bridge) touch(cs *chatShell) {
	b.mu.Lock()
	cs.lastSeen = time.Now()
	b.mu.Unlock()
}

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

func (b *Bridge) resetSession(botID, chatID, threadID string) {
	b.store.Delete(botID, chatID, threadID)
	b.store.Save()
}

func (b *Bridge) stopCurrent(botID, chatID, threadID string) {
	key := sessionKey(botID, chatID, threadID)
	b.mu.Lock()
	defer b.mu.Unlock()
	if t, ok := b.inflight[key]; ok && t.Cancel != nil {
		t.Cancel()
	}
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

	thread := val(req.Action, "thread")
	action := strings.ToLower(val(req.Action, "action"))

	// A synthetic Message carries just enough for the shared command path to
	// reply into the right thread.
	msg := &feishu.Message{
		ChatID:   req.ChatID,
		ThreadID: thread,
		SenderID: req.Operator,
	}
	if req.MessageID == "" {
		// Card actions always arrive with a message_id in context; without it
		// replyCard cannot thread, so fall back to a plain send.
		b.log.Printf("card action without message_id action=%s", action)
		return nil, nil
	}
	msg.MessageID = req.MessageID

	// No Toast here: handleCommand already replies with a card, so a second
	// piece of feedback for the same click would be noise.
	switch action {
	case "", "help":
		return &feishu.CardResponse{}, b.handleCommand(ctx, bot, msg, "/help")
	case "new":
		return &feishu.CardResponse{}, b.handleCommand(ctx, bot, msg, "/new")
	case "stop":
		return &feishu.CardResponse{}, b.handleCommand(ctx, bot, msg, "/stop")
	case "status":
		return &feishu.CardResponse{}, b.handleCommand(ctx, bot, msg, "/status")
	default:
		b.log.Printf("unknown card action %q", action)
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

func quotePath(p string) string {
	return "'" + strings.ReplaceAll(p, "'", "''") + "'"
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}
