// Package bridge wires the Feishu client, the per-chat working directory and
// the Claude Code agent together.
//
// The directory layer is deliberately small: it remembers a cwd per chat so
// /cd carries over into later /ls calls and into the agent's working
// directory. It does not run a shell process, so it needs no external shell
// binary and builds the same on every architecture.
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

// Bridge is the top-level coordinator.
type Bridge struct {
	cfg *config.Config
	cli *feishu.Client
	log *log.Logger

	// cwds is keyed by (botID, chatID): the directory the user has /cd'd to.
	// dirs and sessions are keyed the same way.
	mu         sync.Mutex
	cwds       map[string]string
	store      *sessionStore
	sessReaper *reaper
	// recentDirs is the bot-level LRU of recently opened cwds, persisted
	// next to the session table. The console card reads it (step 9); newTopic
	// pushes into it now so the LRU is populated as soon as /new lands.
	recentDirs *recentDirsStore
	// inflight tracks turns currently running, so /stop can cancel them.
	inflight map[string]*turn

	startedAt time.Time
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
		cfg:      cfg,
		cli:      cli,
		log:      logger,
		cwds:     make(map[string]string),
		inflight: make(map[string]*turn),
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
		return b.replyCard(ctx, m, card.CommandCard(bot.DisplayName, "目录",
			fmt.Sprintf("```\n%s\n```\n\n后续 /ls 与 Claude 会话都以此为工作目录。",
				b.currentCwd(bot, m)), standardButtons(bot, m)...))
	case "/cd":
		if rest == "" {
			return b.replyCard(ctx, m, card.Error(bot.DisplayName, "用法: /cd <路径>"))
		}
		if _, err := b.chdir(bot, m, rest); err != nil {
			return b.replyCard(ctx, m, card.Error(bot.DisplayName, fmt.Sprintf("```\n%v\n```", err)))
		}
		return b.replyCard(ctx, m, card.CommandCard(bot.DisplayName, "已切换目录",
			fmt.Sprintf("```\n%s\n```\n\n后续 /ls 与 Claude 会话都以此为工作目录。",
				b.currentCwd(bot, m)), standardButtons(bot, m)...))
	case "/ls":
		out, err := b.listDir(bot, m)
		if err != nil {
			return b.replyCard(ctx, m, card.Error(bot.DisplayName, fmt.Sprintf("```\n%v\n```", err)))
		}
		return b.replyCard(ctx, m, card.CommandCard(bot.DisplayName, b.currentCwd(bot, m),
			"```\n"+out+"\n```", standardButtons(bot, m)...))
	case "/new", "/reset":
		return b.newTopic(ctx, bot, m)
	case "/stop", "/cancel":
		if !b.stopCurrent(bot.ID, m.ChatID, m.ThreadID) {
			// Nothing running: a "已停止" reply would claim an action that did
			// not happen, and the card the user clicked already shows the
			// result. Silent is the honest answer here.
			return nil
		}
		return b.replyCard(ctx, m, card.CommandCard(bot.DisplayName, "已停止", "已请求取消当前任务。"))
	case "/status":
		return b.handleStatus(ctx, bot, m)
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

// ---- directory layer -----------------------------------------------------
//
// The working directory is the only per-chat mutable state. It is a plain
// string in a map, so there is no process to start, stop, snapshot or reap,
// and /cd survives a bridge restart for the lifetime of the process.

// cwdKey joins bot and chat into the key used for the cwd table.
func cwdKey(botID, chatID string) string { return botID + "/" + chatID }

// topicCwdKey is cwdKey plus the topic. The working directory is per topic,
// not per chat: a topic is the user's unit of work, and "new session" must
// inherit the directory the user was working in rather than resetting to the
// bot's default workspace.
func topicCwdKey(botID, chatID, threadID string) string {
	if threadID == "" {
		return cwdKey(botID, chatID)
	}
	return botID + "/" + chatID + "/" + threadID
}

// currentCwd returns the topic's working directory, falling back to the bot's
// configured workspace when nothing has been recorded.
func (b *Bridge) currentCwd(bot *config.BotConfig, m *feishu.Message) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if d, ok := b.cwds[topicCwdKey(bot.ID, m.ChatID, m.ThreadID)]; ok && d != "" {
		return d
	}
	return bot.Workspace
}

// chdir resolves the target and records it as the topic's working directory.
// Only existing directories are accepted, so /cd into a typo cannot silently
// strand later /ls calls.
func (b *Bridge) chdir(bot *config.BotConfig, m *feishu.Message, target string) (string, error) {
	if !filepath.IsAbs(target) {
		target = filepath.Join(b.currentCwd(bot, m), target)
	}
	info, err := os.Stat(target)
	if err != nil {
		return "", fmt.Errorf("路径不存在: %s", target)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("不是目录: %s", target)
	}
	b.mu.Lock()
	b.cwds[topicCwdKey(bot.ID, m.ChatID, m.ThreadID)] = target
	b.mu.Unlock()
	return target, nil
}

// listDir renders the chat's working directory. Output is capped so a huge
// directory cannot blow past the card size limit.
func (b *Bridge) listDir(bot *config.BotConfig, m *feishu.Message) (string, error) {
	dir := b.currentCwd(bot, m)
	ents, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	const maxLines = 150
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("%-5s %12s  %s\n", "DRW", "SIZE", "NAME"))
	for i, e := range ents {
		if i >= maxLines {
			sb.WriteString(fmt.Sprintf("… 还有 %d 项\n", len(ents)-maxLines))
			break
		}
		size := ""
		if !e.IsDir() {
			if fi, err := e.Info(); err == nil {
				size = fmt.Sprintf("%d", fi.Size())
			}
		}
		name := e.Name()
		if e.IsDir() {
			name += "/"
		}
		perm := ""
		if e.Type().IsDir() {
			perm = "d"
		}
		sb.WriteString(fmt.Sprintf("  %s %12s  %s\n", perm, size, name))
	}
	return sb.String(), nil
}

func (b *Bridge) handleStatus(ctx context.Context, bot *config.BotConfig, m *feishu.Message) error {
	dir := b.currentCwd(bot, m)

	b.mu.Lock()
	defer b.mu.Unlock()

	var sb strings.Builder
	// Inside a topic the scope lines are redundant with the topic card's
	// own context block, so only a chat-wide reply repeats them.
	if m.ThreadID == "" {
		sb.WriteString(fmt.Sprintf("**运行时间**: %s\n\n", time.Since(b.startedAt).Round(time.Second)))
		sb.WriteString(fmt.Sprintf("**工作目录**: `%s`\n\n", dir))
	}

	if s := b.store.Get(bot.ID, m.ChatID, m.ThreadID); s != nil {
		if m.ThreadID != "" {
			sb.WriteString(fmt.Sprintf("**Claude 会话**: `%s`\n\n", shortID(string(s.ID))))
			sb.WriteString(fmt.Sprintf("已处理 %d 轮 · 最后活动 %s 前\n",
				s.Turns, time.Since(s.LastSeen).Round(time.Second)))
		} else {
			sb.WriteString(fmt.Sprintf("**Claude 会话**: %s\n", shortID(string(s.ID))))
			sb.WriteString(fmt.Sprintf("**已处理轮次**: %d\n", s.Turns))
			sb.WriteString(fmt.Sprintf("**最后活动**: %s 前\n", time.Since(s.LastSeen).Round(time.Second)))
		}
		if b.sessReaper.cutoff() > 0 {
			sb.WriteString(fmt.Sprintf("（闲置 %s 后丢弃，不影响 Claude 本地记录）\n", b.sessReaper.cutoff()))
		}
	} else {
		sb.WriteString("**Claude 会话**: 尚无会话\n")
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

	// Inside a topic the reply names the session it is bound to, so the user
	// can reconcile the conversation with a local `claude --resume` session.
	body := card.Sanitize(res.Text)
	if m.ThreadID != "" {
		body += fmt.Sprintf("\n\n_本话题 Claude 会话 `%s` · 工作目录 `%s`_",
			shortID(string(res.SessionID)), workspace)
	}

	footer := card.FormatUsage(res.Usage, res.ModelUsage, res.CostUSD, elapsed)
	final := card.Done(bot.DisplayName, body, footer, standardButtons(bot, m)...)
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
func (b *Bridge) newTopic(ctx context.Context, bot *config.BotConfig, m *feishu.Message) error {
	// m.MessageID is the topic's root when the click came from inside a topic,
	// and the clicked card otherwise. Either is a valid anchor: reply
	// reply_in_thread=true forks a topic only when the anchor is not already
	// inside one.
	if m.MessageID == "" {
		b.log.Printf("new topic: no anchor (chat=%s)", shortKey(m.ChatID))
		return nil
	}

	had := b.resetSession(bot.ID, m.ChatID, m.ThreadID)

	// Capture the cwd before the reply: the new topic has no record yet, so
	// this is the only chance to inherit the directory the user was working
	// in. Otherwise a topic fork would silently drop back to the bot's
	// default workspace.
	inherit := b.currentCwd(bot, m)
	b.log.Printf("new topic: anchor=%s root=%s thread=%s cwd=%s cleared=%v",
		shortID(m.MessageID), shortID(m.RootID), shortID(m.ThreadID), inherit, had)

	// The notice carries the new topic's identity, but the topic has not been
	// created yet, so it is built with no buttons and updated once Feishu
	// returns a thread_id.
	body := "🆕 新会话已开启。在这个话题里继续，就是一条全新的 Claude 会话。"
	if !had {
		body = "本话题已开启新会话，下一条消息从头开始。"
	}
	c := card.CommandCard(bot.DisplayName, "新会话", body)

	msgID, threadID, err := b.cli.ReplyCardThreaded(ctx, m.MessageID, c, true)
	if err != nil {
		b.log.Printf("new topic: threaded reply failed, replying inline: %v", err)
		if m.ThreadID == "" {
			// No topic to fork: a plain reply is correct here.
			return b.replyCard(ctx, m,
				card.CommandCard(bot.DisplayName, "新会话", body, chatButtons()...))
		}
		return nil
	}

	// Now that the topic exists, show what it is bound to. The notice card
	// is the topic's root, so it gets the topic-scoped button set.
	b.setCwd(bot.ID, m.ChatID, threadID, inherit)
	// Promote this cwd in the bot-level LRU so a future no-arg /new and the
	// console card can find it. Only after the topic is real: an inherited
	// cwd that never materialises into a topic is not "recently used".
	b.recentDirs.Add(bot.ID, inherit)

	notice := card.CommandCard(bot.DisplayName, "新会话",
		body+"\n\n"+threadContext(nil, inherit), threadButtons(threadID, msgID)...)
	if perr := b.cli.PatchCard(ctx, msgID, notice); perr != nil {
		b.log.Printf("new topic: patch notice: %v", perr)
	} else {
		b.log.Printf("new topic: notice patched %s with %d buttons", shortID(msgID), len(notice))
	}

	b.log.Printf("new topic: %s in chat %s cwd=%s cleared=%v",
		shortID(threadID), shortKey(m.ChatID), inherit, had)
	return nil
}

// setCwd records a working directory for one topic.
func (b *Bridge) setCwd(botID, chatID, threadID, dir string) {
	b.mu.Lock()
	b.cwds[topicCwdKey(botID, chatID, threadID)] = dir
	b.mu.Unlock()
}

// resetSession drops the thread's session record and reports whether there
// was one to drop, so a reply can say whether it actually cleared anything.
func (b *Bridge) resetSession(botID, chatID, threadID string) bool {
	had := b.store.Get(botID, chatID, threadID) != nil
	b.store.Delete(botID, chatID, threadID)
	b.store.Save()
	return had
}

// stopCurrent cancels the in-flight turn for this thread. It reports whether
// anything was running so the caller does not claim to have stopped a task
// that was not started.
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
	// because neither survives the round trip.
	thread := val(req.Action, "thread")
	root := val(req.Action, "root")
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

	// No Toast here: handleCommand already replies with a card, so a second
	// piece of feedback for the same click would be noise.
	b.log.Printf("card action %q from %s (chat=%s)", action, shortID(req.Operator), shortKey(req.ChatID))
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

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}
