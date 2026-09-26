# 会话模型与测试体系 —— 分析与重构设计

**状态**: 待评审  
**日期**: 2026-09-26  
**范围**: 会话绑定模型、话题交互、测试自动化  
**不涵盖**: 多 bot 路由、权限模型、流式渲染性能

---

## 1. 现状问题清单

### P0-1 切目录后无法选择旧 session(硬伤)

`/cd D:\workcode\work` 之后,`runTurn` 仍然用 `sessionKey(bot, chat, thread)` 查 session,拿到旧 session id 并 `--resume` 它。但 `cmd.Dir` 已经变了。结果是:

- Claude 的记忆是"在 D:\workcode 里的对话",现在却跑到 D:\workcode\work 继续说
- 用户**没有任何入口**能选择"在这个新目录里开新会话"或"切回旧目录的会话"
- `/new` 是唯一选项,但它会**开新话题**——而用户常常只是想在同一个话题里换个目录继续

数据模型上这就是查不到的:

```go
// internal/bridge/sessions.go
byKey map[string]*session        // key = sessionKey(bot, chat, thread)

// key 里没有 cwd 维度
func sessionKey(botID, chatID, threadID string) string { ... }
```

`b.cwds` 是另一张表,`b.mu` 保护,跟 session store 没有关联。两张表各查各的,交集查询无从谈起。

### P0-2 测试体系缺失

`go test ./...` 输出 6 个包全是 `[no test files]`。所有验证都是手工:浏览器点按钮、读日志、肉眼比对。导致:

- 改一行代码也不知道有没有回归
- 上次的"回复没进话题"是**用户**发现的,不是测试
- 无法在 CI 里跑任何东西

### P1-3 会话内多轮对话未验证

`/new` 之后的第一条消息会创建 session,第二条应该 `--resume`。这个链路从没被自动化测过。手工验证时我撞上过 `agent exit: 0xc000013a`(`STATUS_CONTROL_C_EXIT`)——Claude 进程被终止。目前无法判断是偶发的还是 resume 路径的问题。

### P1-4 话题根卡片的按钮集错位

`newTopic` 用 `threadButtons(threadID, msgID)` 给新话题根卡片构造按钮。但这个 card 此刻**还没有 thread**——它是靠 `reply_in_thread=true` **创建** topic 的那条消息。卡片被 patch 时它已经在 topic 里了,所以 3 个按钮在功能上没错。但语义上根卡片是"话题声明",它和话题内其他卡片应该有区别。目前混用,后续如果要加"退出话题"、"复制 session id"之类的话题级操作,这里会纠缠。

### P1-5 cwd 记录在内存,重启即失

`b.cwds` 是 `map[string]string` 加 `sync.Mutex`。进程重启回到 `bot.Workspace` 默认值。而 session 记录是**持久化**的(`.feishu-bridge-sessions.json`)。两者生命周期不对称:重启后 session 还在,cwd 没了,于是 `--resume` 一个旧 session 却跑在默认目录——正是 P0-1 的另一个触发路径。

### P2-6 卡片回调上下文贫瘠,靠 stamp 补救

`event/dispatcher/callback/model.go` 的 `Context` 只有 `OpenMessageID` + `OpenChatID`。thread/root 全靠自己 stamp 进 `value` map。`onCardAction` 里 `root == ""` 时回落到 `req.MessageID`,这是给旧卡片的兼容路径,但旧卡片**永久存在**在会话历史里,这条兼容分支永远不会被删,也永远无法被删除后测试验证。

### P2-7 主会话与话题的语义不对称

主会话里的 `/new` 开话题,话题里的按钮没有 `/new`。主会话本身没有"绑定一个已有 session"的能力。主会话更像是"控制台",话题是"工作区",但控制台的 session 状态是隐式的——它其实就是"无 thread 的那个 session 记录"。这个隐式性让用户很难理解"我现在在跟哪个 Claude 会话说话"。

---

## 2. 设计目标

1. **cwd 成为 session 的一等维度**:切目录后能列出"此目录下我之前的会话",能选择继续或新开。
2. **话题与 session 1:1 绑定关系显式化**,并持久化。
3. **测试可在本地和 CI 运行**,覆盖会话内多轮、cwd 切换、话题创建与回复落点。
4. **不引入新进程模型**:仍然是一次 `claude -p --resume` 一个 turn。

非目标:实时同步 Claude 的 session 列表(Claude 的 `--list-sessions` 不在稳定 API 里)、多租户。

---

## 3. 重构设计

### 3.1 数据模型

#### session 记录加 cwd 与标签

```go
// internal/bridge/sessions.go
type session struct {
    ID       agent.SessionID `json:"id"`
    BotID    string          `json:"bot_id"`
    ChatID   string          `json:"chat_id"`
    ThreadID string          `json:"thread_id,omitempty"` // 空 = 主会话
    Cwd      string          `json:"cwd,omitempty"`       // 新增
    Label    string          `json:"label,omitempty"`     // 新增: 人工可读标题
    CreatedAt time.Time      `json:"created_at"`
    LastSeen  time.Time      `json:"last_seen"`
    Turns    int             `json:"turns"`
}
```

#### 新增: cwd 历史表(每 topic 独立)

```go
// 每 topic 一条 cwd 历史,持久化到 .feishu-bridge-cwd.json
type cwdEntry struct {
    Dir       string    `json:"dir"`
    SessionID string    `json:"session_id,omitempty"` // 该目录下绑定的 session
    LastUsed  time.Time `json:"last_used"`
}
```

`chdir` 不再只写一张 map,而是 upsert 进 cwdEntry 列表。这是"列出此 topic 下我切过的目录及其会话"的数据来源。

#### 索引重构

```go
type sessionStore struct {
    mu    sync.Mutex
    byKey map[string]*session          // 主键: botID/chatID/threadID
    byID  map[agent.SessionID]string   // session_id -> key
    // 新增: (botID, chatID, threadID, cwd) -> session_id
    // 用于"切目录后列旧会话"
    byCwd map[string][]*session
    // 新增: cwd 历史
    cwdHist map[string][]cwdEntry
    ...
}
```

`byCwd` 是派生索引,在 `Set` 时维护。不需要它做主键——主键仍是 topic,因为一个 topic 同一时刻只该有一个 active session。`byCwd` 是给"选择器"用的。

#### cwd 与 session 的绑定规则

关键决策:**session 创建时快照 cwd,不跟随 cwd 变。**

```
t0: /cd D:\workcode\work
t1: 发消息 "分析这个"  → 创建 session S1, S1.Cwd = D:\workcode\work
t2: /cd D:\workcode\api
t3: 发消息 "继续"      → 选项出现:
                          a) 继续 S1(cwd=work,但你现在在 api) —— 明确警告
                          b) 新开 session(cwd=api)             ← 默认
```

理由:Claude 的 session 是按工作目录理解上下文的。让 session 跟着 cwd 走会让记忆错乱,静默继续比报错更糟。所以默认新开,继续必须显式选择。

### 3.2 交互设计

#### `/cd` 之后不再静默绑定

```
ℹ️ 已切换目录
D:\workcode\api

该目录尚无会话。下一条消息将创建新会话。
如想继续之前的会话: 点下方按钮。
[▶ 继续 work 里的会话 (3 轮)] [🆕 确认新会话]
```

#### `/sessions` 新命令 —— 会话选择器

飞书没有真正的多选项表单,所以用"按钮墙"。每张按钮 = 一个可选 session:

```
ℹ️ 本话题可用会话
目录: D:\workcode\api

[▶ api · 2 轮 · 10 分钟前]
[▶ work · 5 轮 · 2 小时前]
[🆕 开新会话]
```

点击按钮 → `value` 里 stamp `session_id` + `cwd` → 回调里 `b.chdir` + `b.bindSession` → 当前 topic 切换绑定。

> **技术约束**: 飞书卡片按钮 `value` map 的值必须是 string。所以 `session_id`、`cwd`、`thread`、`root` 都以字符串形式 stamp。按钮数量上限需要确认(飞书卡片单卡最多 8 个按钮;超出时折叠为"查看更多")。

#### `/new` 语义收敛

`/new` 只干一件事:**开新话题**。不再隐式"新 session"。新话题的根卡片上给"新会话 / 选择已有会话"两个入口。这样"开新话题"和"新会话"两个概念彻底分开——之前它们混在 `/new` 一个动作里,是 P1-4 混乱的根源。

#### 主会话与话题统一

主会话不再是特殊路径。主会话 = `threadID == ""` 的 topic。`sessionKey` 三段式保持不变,但话题内 UI 文案不再区分"本话题"和"本会话"——统一叫"本会话",因为对用户来说"话题"只是飞书的呈现方式,不是概念。

### 3.3 代码改动落点

| 文件 | 改动 |
|---|---|
| `internal/bridge/sessions.go` | session 加 `Cwd`/`Label`;新增 `byCwd`、`cwdHist`;新增 `ListByCwd`、`ListHistory`、`BindSession` |
| `internal/bridge/bridge.go` | `chdir` 写 cwdHist;`runTurn` 检查 cwd 漂移并提示;新增 `handleSessions`;`onCardAction` 加 `session` action |
| `internal/bridge/command.go` | 新增 `sessionButtons`;`/sessions` 加入 `isBridgeCommand` |
| `internal/feishu/client.go` | 无改动(stamp 机制已够用) |
| `cmd/feishubridge/main.go` | 无改动 |

估算: sessions.go +120 行,bridge.go +180 行,command.go +40 行。

---

## 4. 测试体系设计

这是当前最大的缺口。目标:**CI 里能跑,本地一条命令跑,失败信息能定位**。

### 4.1 测试分层

```
第 1 层  纯单元测试        无外部依赖,毫秒级,CI 必跑
第 2 层  伪 SDK 集成测试   打桩 lark client,测桥接逻辑,CI 必跑
第 3 层  端到端(真飞书)   需 app 凭据 + 浏览器,手动或夜间跑
```

现在 6 个包 0 测试。第 1、2 层可以立刻补,不依赖飞书在线。

### 4.2 第 1 层:纯单元测试

**`internal/bridge/sessions_test.go`** —— session store 是最适合先测的,因为它是纯 map 操作:

```go
func TestSessionStore_SetGet(t *testing.T)
func TestSessionStore_Delete_RemovesReverseIndex(t *testing.T)
func TestSessionStore_SaveLoad_RoundTrip(t *testing.T)      // 用 t.TempDir()
func TestSessionStore_ClearIdle_Boundary(t *testing.T)      // idle == cutoff 不删, > cutoff 删
func TestSessionStore_ListByCwd_ReturnsNewestFirst(t *testing.T)   // 新增
func TestSessionStore_BindSession_UpdatesReverseIndex(t *testing.T) // 新增
func TestSessionKey_ThreadEmpty(t *testing.T)               // 主会话键 == bot/chat
func TestSessionKey_ThreadSet(t *testing.T)
```

**`internal/bridge/command_test.go`** —— 卡片构造是纯函数,最容易测:

```go
func TestChatButtons_HasNewSession(t *testing.T)           // 主会话 4 按钮,含 action=new
func TestThreadButtons_NoNewSession(t *testing.T)          // 话题 3 按钮,无 action=new
func TestThreadButtons_StampThreadAndRoot(t *testing.T)    // value 里 thread/root 都在
func TestThreadContext_WithSession(t *testing.T)           // 含 cwd + session id
func TestThreadContext_NoSession(t *testing.T)             // "尚未运行会话"
func TestIsBridgeCommand_ExcludesPathLike(t *testing.T)    // "/tmp/foo" 不是命令
func TestIsBridgeCommand_AllKnown(t *testing.T)
```

**`internal/feishu/parse_test.go`** —— 消息解析是易错的字符串处理:

```go
func TestParseMS_Valid / Invalid / Empty
func TestStripMentionKeys_RemovesPlaceholders(t *testing.T)
func TestStripMentionKeys_EmptyMentions
func TestWalkCard_NestedMarkdown
func TestCardButtons_ExtractsTextAndValue   // 新增,日志辅助函数
```

**`internal/agent/claude_test.go`**:

```go
func TestBuildArgs_NoResume(t *testing.T)                 // 无 --resume
func TestBuildArgs_WithResume(t *testing.T)               // --resume <id> 在 -- 之前
func TestBuildArgs_ModelAndSettingSources(t *testing.T)
func TestParseEvent_AssistantText / Result / SystemInit / MalformedJSON
```

**`internal/card/card_test.go`**:

```go
func TestCommandCard_SchemaVersion2_0(t *testing.T)       // 回归: 不能退回 schema 1.0
func TestCommandCard_NoActionWrapper(t *testing.T)         // 回归: 不能有 "action" element(200861)
func TestBuild_ButtonsAreTopLevel(t *testing.T)
func TestSanitize_UnterminatedFence
func TestFormatUsage_AllFields / EmptyUsage
```

### 4.3 第 2 层:伪 SDK 集成测试

**问题**: `internal/bridge` 直接持有 `*feishu.Client`(具体类型),没法打桩。需要重构。

**改法**: 抽出接口。

```go
// internal/feishu/client.go
// Sender 是 bridge 实际调用的出站能力。真实现是 *Client;测试里用 fake。
type Sender interface {
    SendCard(ctx context.Context, chatID string, card map[string]any) (string, error)
    SendCardInThread(ctx context.Context, chatID, threadID string, card map[string]any) (string, error)
    ReplyCard(ctx context.Context, sourceMessageID string, card map[string]any, thread bool) (string, error)
    ReplyCardThreaded(ctx context.Context, sourceMessageID string, card map[string]any, thread bool) (string, string, error)
    PatchCard(ctx context.Context, messageID string, card map[string]any) error
}
```

`bridge.New` 接受 `Sender` 而不是 `*feishu.Client`。`*Client` 自然满足。测试注入 fake:

```go
// internal/bridge/bridge_test.go
type fakeSender struct {
    mu       sync.Mutex
    replies  []replyCall                       // 记录每次调用
    nextMsgID int
}

func (f *fakeSender) ReplyCardThreaded(ctx context.Context, anchor string,
    card map[string]any, thread bool) (string, string, error) {
    f.mu.Lock()
    defer f.mu.Unlock()
    f.nextMsgID++
    id := fmt.Sprintf("om_test_%d", f.nextMsgID)
    tid := "omt_test_1"                         // 模拟飞书返回 thread_id
    f.replies = append(f.replies, replyCall{anchor: anchor, thread: thread, msgID: id, threadID: tid})
    return id, tid, nil
}
```

**核心场景测试**:

```go
// 场景 1: 主会话 /new → 开话题,新 topic 的 session 行 threadID 非空
func TestNewTopic_ReplyLandsInTopic(t *testing.T) {
    b := newTestBridge(t, fake)
    b.handleCommand(ctx, bot, &feishu.Message{ChatID: "oc_1", MessageID: "om_anchor"}, "/new")

    // 断言 1: 出站用了 thread=true
    require.Len(t, fake.replies, 1)
    require.True(t, fake.replies[0].thread)

    // 断言 2: cwd 被继承到新 topic
    require.Equal(t, "/workspace", b.currentCwd(bot, &feishu.Message{ChatID: "oc_1", ThreadID: "omt_test_1"}))

    // 断言 3: 新 topic 的卡片是 3 按钮(无 🆕)
    buttons := fake.lastCardButtons()
    require.Len(t, buttons, 3)
    for _, btn := range buttons {
        require.NotEqual(t, "new", btn.Value["action"])
    }
}

// 场景 2: 话题内点状态按钮 → 回复落在同一话题(thread_id 非空)
func TestTopicButtonStatus_ReplyStaysInTopic(t *testing.T) {
    b := newTestBridge(t, fake)
    action := &feishu.CardAction{
        ChatID: "oc_1", MessageID: "om_clicked",
        Action: map[string]any{"action": "status", "thread": "omt_test_1", "root": "om_root"},
        Operator: "ou_user",
    }
    b.onCardAction(ctx, action)

    got := fake.replies[len(fake.replies)-1]
    require.True(t, got.thread)
    require.Equal(t, "om_root", got.anchor)   // 锚在 root,不是 clicked card
}

// 场景 3: 切目录后 cwd 漂移检测
func TestRunTurn_CwdDrift_PromptsUser(t *testing.T) {
    b := newTestBridge(t, fake)
    b.chdir(bot, &feishu.Message{ChatID: "oc_1"}, "/dir/a")
    b.store.Set(&session{ID: "S1", BotID: "main", ChatID: "oc_1", Cwd: "/dir/a"})
    b.chdir(bot, &feishu.Message{ChatID: "oc_1"}, "/dir/b")

    // 发消息:session 记录 cwd=/dir/a,但当前 cwd=/dir/b
    err := b.runTurn(ctx, bot, &feishu.Message{ChatID: "oc_1"}, "继续")
    require.NoError(t, err)
    // 断言:卡片包含漂移提示,或没有 resume 旧 session
    body := fake.lastCardBody()
    require.Contains(t, body, "目录")
    require.Contains(t, body, "/dir/a")
}

// 场景 4: 多轮对话 session 复用
func TestRunTurn_SecondMessage_ResumesSession(t *testing.T) {
    b := newTestBridge(t, fake, agentStub)   // agentStub 可控的 agent 替身
    b.runTurn(ctx, bot, &feishu.Message{ChatID: "oc_1"}, "你好")
    b.runTurn(ctx, bot, &feishu.Message{ChatID: "oc_1"}, "继续")

    require.Equal(t, 2, agentStub.calls)
    require.Empty(t, agentStub.resumes[0])        // 第一轮无 --resume
    require.Equal(t, agentStub.createdID, agentStub.resumes[1])  // 第二轮 resume
}
```

**`agent` 也需要接口**——`runTurn` 现在硬编码 `agent.Run`。改成:

```go
// internal/agent/agent.go
type Runner interface {
    Run(ctx context.Context, cfg Config, prompt string, resume SessionID, onEvent func(Event)) (*Result, error)
}
```

测试里用 stub 返回预设 `Result{SessionID: "S1"}`,不用真起 claude 子进程。

### 4.4 第 3 层:端到端(真飞书)

保留手工通道,但**固定用例脚本化**。新增 `e2e/README.md`,列出每次改动后必须过的 6 个用例:

| # | 用例 | 验证点 | 判定依据 |
|---|---|---|---|
| E1 | 主会话发任意文本 | 收到卡片,thread_id 空 | `send reply ... thread_id=""` |
| E2 | 主会话 `/new` | 开新话题,继承 cwd | `new topic: omt_... cwd=<预期>` |
| E3 | 话题内发消息 | 回复落同话题 | `thread_id=omt_...`(非空) |
| E4 | 话题内第二条消息 | `--resume` 复用 session | session store Turns=2 |
| E5 | 话题内点 📊 状态 | 回复落同话题,卡片 3 按钮 | `send reply anchor=om_root thread=true` |
| E6 | 话题内 `/cd` + 发消息 | 提示 cwd 漂移 | 卡片含漂移文案 |

**执行方式**: 用 chrome-devtools MCP 的 `evaluate_script`(不是 `fill`——它对飞书 contenteditable 是静默 no-op):

```javascript
// 发消息
const ed = document.querySelector('[contenteditable="true"]');
ed.focus();
document.execCommand('insertText', false, '<prompt>');
ed.dispatchEvent(new KeyboardEvent('keydown', {key:'Enter', keyCode:13, bubbles:true}));

// 读最新卡片按钮(判定 3 还是 4 个)
[...document.querySelectorAll('.message-section')].map(s =>
  [...s.querySelectorAll('button')].map(b => b.innerText.trim()))
```

### 4.5 CI 接入

`.github/workflows/ci.yml` 跑第 1、2 层:

```yaml
name: ci
on: [push, pull_request]
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version: '1.26'
      - run: go vet ./...
      - run: go test -race ./...
```

第 3 层不进 CI(需要真实 app 凭据),但 `Makefile` 加 `make e2e` 输出用例清单。

---

## 5. 落地顺序

按依赖关系排,每步独立可交付、可回滚。

| 步 | 内容 | 依赖 | 预计改动 |
|---|---|---|---|
| 1 | 抽 `Sender` / `Runner` 接口 | 无 | +40 行,无行为变化 |
| 2 | 补第 1 层单元测试(session store、命令、卡片、parse) | 1 | +~400 行测试 |
| 3 | 补第 2 层集成测试(fakeSender + agentStub,4 个核心场景) | 1 | +~250 行测试 |
| 4 | CI 接入 | 2,3 | +1 个 workflow 文件 |
| 5 | session 记录加 `Cwd`/`Label`,新建 `byCwd` 索引 | 2,3 | +120 行,向后兼容(旧记录 Cwd 空) |
| 6 | `chdir` 写 cwdHist,持久化 | 5 | +60 行 |
| 7 | `/sessions` 命令 + `session` 按钮 action | 5 | +150 行 |
| 8 | `runTurn` cwd 漂移检测 | 5 | +30 行 |
| 9 | `/new` 语义收敛(只开话题),根卡片按钮集重定义 | 3 | +40 行 |
| 10 | e2e 用例文档化 + `make e2e` | 4 | 文档 |

**关键**: 步 1-4 是测试基础设施,不改变任何用户可见行为。先合进去,后面每一步都有测试保护。直接跳到步 5 改数据模型是危险的——现在 0 测试,改完只能靠手工验。

步 5 的向后兼容点:旧 session 记录 `Cwd` 字段为空。`byCwd` 索引跳过空 cwd 的记录。`runTurn` 发现 `session.Cwd == ""` 时视为"未记录目录",走默认新会话逻辑并补写 Cwd。

---

## 6. 明确不做

- **不做**实时拉取 Claude 的 session 列表。Claude 没有稳定的 `--list-sessions` API(有 `~/.claude/projects/` 下的 jsonl 但格式不保证稳定)。选择器只列**本桥接自己创建过**的 session。
- **不做**跨 bot 的 session 共享。`sessionKey` 第一段是 botID,刻意隔离。
- **不做**话题自动归档。飞书话题无归档 API,靠 reaper 淘汰 session 记录即可。
- **不做**卡片表单/多选项组件。飞书卡片 2.0 的 `form` 能力与 `streaming_mode` 冲突,且按钮墙已够用。
