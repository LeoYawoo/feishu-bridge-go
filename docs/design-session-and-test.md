# 会话模型与交互重设计 —— 分析与重构设计

**状态**: 交互定稿,待实施  
**日期**: 2026-09-26  
**范围**: 会话绑定模型、话题交互、卡片按钮语义、测试自动化  
**不涵盖**: 多 bot 路由、权限模型、流式渲染性能、claude 常驻进程实现细节

交互设计稿见 `docs/feishu-redesign-mockup.png`(8 帧)。

---

## 1. 心智模型(最终版)

**核心关系**:

| 位置 | 处理什么 |
|---|---|
| **主会话**(chat 顶层) | 会话管理控制台。唯一命令 `/new [cwd?]`,收到普通消息回复控制台卡片(带最近 5 个目录按钮) |
| **话题** | Claude 会话(cwd 创建时定死,不可改)。用户发普通消息 → claude 回复,无其他命令 |

**推论**:

- **主会话不启动 claude**。它是控制台,所有会话管理动作都在这里发起。
- **话题 = claude 会话**,1:1 绑定。cwd 由 `/new` 时决定,后续不可改(想换 cwd 就开新话题)。
- **主会话唯一命令 `/new [cwd?]`**:
  - 无参:用**最近使用过的目录**(bot 级 LRU)开新话题
  - 带 cwd:用该 cwd 开新话题
- **话题内无任何文字命令**。没有 `/cd` `/sessions` `/resume` `/stop` `/status` `/help`。想中断/切 session 走卡片按钮,其他情况用户自己跟 claude 沟通。
- **卡片按钮语义**:点击 = 用户在输入框里输入对应命令。按钮不引入新动作。
- **cwd 存 session 记录**(不是独立的 `b.cwds` map),持久化跟 session 走。
- **飞书卡片能力约束**:卡片里用户能触发 bot 动作的只有 `button` 控件,`markdown` 里的蓝色链接只能跳 URL。所有"可点击行"都是真正的 button。卡片按钮上限 ~5-6 个。

**主会话发普通消息的行为**:回复控制台卡片,提示"这里是控制台",展示最近目录按钮。**不自动开话题**。

---

## 2. 现状问题清单

#### P0-1 主会话与话题边界模糊

现状主会话里的 `/new` 和"发普通消息"都走同一条 `newTopic` 路径,但**没有明确的"主会话是控制台"心智**。用户发 `/new` 时看到"新话题已创建",但没意识到"新话题 = 新 claude session"。用户在话题里也找不到"我到底跟哪个 claude 会话说话"的显式反馈。

现状主会话发普通消息会**静默开话题**,跟"主会话是控制台"的心智冲突。

#### P0-2 cwd 记录在内存,重启即失

`b.cwds` 是 `map[string]string` + `sync.Mutex`。进程重启回到 `bot.Workspace` 默认值。session 记录是**持久化**的(`.feishu-bridge-sessions.json`)。两者生命周期不对称:重启后 session 还在,但 cwd 没了。

修复方向:cwd 是 session 的属性,存 session 记录。

#### P0-3 测试体系缺失

`go test ./...` 输出 6 个包全是 `[no test files]`。所有验证都是手工:浏览器点按钮、读日志、肉眼比对。改一行代码也不知道有没有回归,CI 里跑不了任何东西。

#### P0-4 命令面与卡片按钮语义割裂

现状 11 个命令 + 7 个按钮,重叠 5 对(`/new` ↔ 🆕新会话、`/stop` ↔ ⏹停止、`/status` ↔ 📊状态、`/help` ↔ ❓帮助、`/h` ↔ `/help`)。问题不是"功能重复",而是**语义割裂**。卡片按钮应该是"点击 = 用户在输入框里输对应命令"的可视化快捷键。

#### P0-5 话题内文字命令与 claude 内置命令冲突

用户想在话题内切 session、切 cwd,得用飞书命令(`/resume`/`/cd`/`/sessions`)。但 claude 自己也有 `/resume` 类似语义的输入(直接发文本给 claude 时,`/` 开头的东西可能被解释)。而且话题心智是"跟 claude 说话",不该混着塞文字命令。

修复方向:话题内**完全无文字命令**,所有切换/停止/状态都走卡片按钮。

#### P0-6 飞书卡片"蓝色链接"是伪交互

markdown 里的 `[text](url)` 只能跳 URL,不能触发 bot 回调。想让"点击行切换 session",必须用真正的 `button` 控件。

#### P1-7 会话内多轮对话未验证

`/new` 之后的第一条消息会创建 session,第二条应该 `--resume`。这个链路从没被自动化测过。手工验证时撞上过 `agent exit: 0xc000013a`(`STATUS_CONTROL_C_EXIT`)。目前无法判断是偶发的还是 resume 路径的问题。

#### P1-8 话题根卡片的按钮集错位

`newTopic` 用 `threadButtons(threadID, msgID)` 给新话题根卡片构造按钮。但卡片此刻还没有 thread —— 它是靠 `reply_in_thread=true` **创建** topic 的那条消息。卡片被 patch 时它已经在 topic 里了,所以功能上没错。但语义上根卡片是"话题声明",它和话题内其他卡片应该有区别(根卡片应该带 session 选择器,普通回复卡片不应该带)。

#### P2-9 卡片回调上下文贫瘠,靠 stamp 补救

`event/dispatcher/callback/model.go` 的 `Context` 只有 `OpenMessageID` + `OpenChatID`。thread/root 全靠自己 stamp 进 `value` map。`onCardAction` 里 `root == ""` 时回落到 `req.MessageID`,这是给旧卡片的兼容路径,但旧卡片永久存在,这条兼容分支永远不会被删。

---

## 3. 交互设计(定稿)

### 3.1 主会话:控制台

#### 3.1.1 主会话卡片

主会话每次回复都发**同一张卡片模板**,包含:

- 标题:`⚠️ 主会话是控制台`
- 正文:`这里是会话管理控制台,不启动 claude。` + `最近目录(点击=用该 cwd 开新话题):`
- 按钮:**最多 5 个**,每个对应一个最近目录,文字形如 `🆕 <目录名>`,点击等效于 `/new <cwd>`
  - 第一个是最近用的目录,标签后加 `(最近)` 后缀
  - 目录名取 basename(避免过长)
  - 少于 5 个就列几个,空列表就不放按钮

**卡片构造**:
```go
card.CommandCard(botName, "⚠️ 主会话是控制台", "这里是控制台...").
    Buttons(recentDirs.Buttons(bot))  // 每个 Button{Text: "🆕 api", Value: {"action": "new_topic", "cwd": "D:\workcode\api"}}
```

#### 3.1.2 `/new` 命令

- `/new` 无参:取 `recentDirs.MostRecent()`(bot 级 LRU),开新话题
- `/new D:\workcode\api`:用指定 cwd 开新话题
- `/new <不存在的路径>`:报错卡片,不创建话题
- 主会话**不再自动开话题** —— 收到普通消息,回复控制台卡片,让用户主动选

**副作用**:创建新话题后,把该 cwd 提到 `recentDirs` 的 LRU 顶端。

### 3.2 话题:claude 会话

#### 3.2.1 话题根卡片(第一张)

用户 `/new` 后,飞书回复落到新话题里,内容:

- 标题:`🟢 新话题已开启`
- 正文:`cwd: D:\workcode\api` + 分隔线 + `点击历史 session 续它,或点 [🆕 新] 从头开始:`
- 按钮:**最多 5 个历史 session**(取该 cwd 下最近 5 个 session,按 LastSeen 排序)+ 1 个 `[🆕 新]` 按钮
  - 每个历史 session 按钮文字形如 `▶ <session_id 前 8 位>`,点击 → 该话题的下一条普通消息 `--resume <id>`
  - `[🆕 新]` 点击 → 该话题下一条消息**不** resume,开新 session

**语义**:点击历史 session 按钮**不是**立刻切 session,而是设置一个"下一条消息用哪个 session"的 pending 状态。真正的 session 绑定发生在下一条普通消息到达时。这样:
- 点了但没发消息 → 不影响任何 session
- 点了又改主意点另一个 → pending 状态更新
- 点了之后发普通消息 → resume 到该 session

**卡片构造**:
```go
card.CommandCard(botName, "🟢 新话题已开启", "cwd: D:\workcode\api ...").
    Buttons(
        sessionsForCwd.Buttons(bot, cwd, 5),   // 5 个历史 session 按钮
        card.Button{Text: "🆕 新", Value: map[string]string{"action": "new_session", "thread": threadID, "root": rootMsgID}},
    )
```

#### 3.2.2 用户发普通消息 → claude 回复

- 话题 root card 存在 pending `resume_session` → 用该 session 跑 claude
- 无 pending 但话题有历史 session(从飞书话题根消息推断) → 用**当前话题**绑定的 session(即第一次 `/new` 后选择的那个,或者是第一次发的消息时自动新建的)
- 完全没有 session → 自动新建一个,`session.Cwd = 话题 cwd`

回复卡片:
- 标题:`claude-pc`
- 正文:claude 输出
- 按钮:**无**

**语义**:话题内不再有任何卡片按钮。用户想中断,和 claude 说(或者发一个空消息让 bridge 侧中断);想切 session,回主会话 `/new` 开新话题。**claude 内部有自己 `/exit` `/compact` 等命令** —— 但用户发这些给飞书话题,会被当作普通消息转给 claude,claude 自己处理。飞书端不再解释。

#### 3.2.3 cwd 不可变

话题一旦创建,cwd 就定死。**没有 `/cd` 命令,没有"改 cwd"按钮**。想换 cwd 只能:
1. 回主会话,点最近目录按钮选新 cwd,或 `/new <新 cwd>`
2. 新话题里继续对话,旧话题保留

这样避免了一个语义混乱:`/cd` 到底改的是 session 的 cwd,还是当前话题的 cwd,还是 bridge 的全局 cwd。

### 3.3 命令收敛

从现状 12 个前缀 `/new /reset /cd /stop /cancel /status /help /h /sessions /model /resume /ls /pwd` 收敛到 **1 个**:

| 命令 | 位置 | 说明 |
|---|---|---|
| `/new [cwd?]` | 主会话 | 开新话题(cwd 可选,无参用最近) |

**砍掉 11 个**:

| 命令 | 原意 | 替代 |
|---|---|---|
| `/reset` | 清空 session | 主会话点最近目录按钮开新话题 |
| `/cd <dir>` | 改 cwd | 主会话开新话题带 cwd(话题内 cwd 不可改) |
| `/stop` | 中断 turn | 用户跟 claude 说,或不做(飞书端不管) |
| `/cancel` | 同 `/stop` | 同上 |
| `/status` | 查 session 状态 | 用户跟 claude 说,或不做 |
| `/help` | 帮助 | README,或砍 |
| `/h` | 别名 | 砍 |
| `/sessions` | 列 session | 主会话不显示;话题根卡片的历史 session 按钮 |
| `/model <name>` | 换模型 | 砍(通过 config 或环境变量配置,不做运行时切换) |
| `/resume <id>` | 续 session | 话题根卡片的 session 按钮 |
| `/ls` | 列文件 | 让用户让 claude 列 |
| `/pwd` | 显示 cwd | 卡片正文里已经显示 |

`isBridgeCommand` 从 12 个前缀缩到 1 个。

### 3.4 卡片按钮 stamp

现状 `threadButtons(thread, root)` stamp `thread`/`root` 到 `value` map,回调时取出来锚定话题。定稿后 stamp 更多:

```go
// 主会话卡片:最近目录按钮
Value: map[string]string{
    "action": "new_topic",
    "cwd": "D:\\workcode\\api",
}

// 话题根卡片:历史 session 按钮
Value: map[string]string{
    "action": "resume_session",
    "thread": threadID,
    "root": rootMsgID,
    "session": "3b2526fd...",
}

// 话题根卡片:新 session 按钮
Value: map[string]string{
    "action": "new_session",
    "thread": threadID,
    "root": rootMsgID,
}
```

`onCardAction` 分派:
- `action=new_topic` → 检查 cwd 存在,调 `newTopic`,回复落到主会话(不落在新话题里,让飞书创建话题后新话题根卡片自己展示)
- `action=resume_session` → 设置 `pendingSession[thread] = sessionID`,回个短确认(可选,或直接静默)
- `action=new_session` → 设置 `pendingSession[thread] = ""`(清空 pending),回个短确认(可选)

---

## 4. 数据模型

### 4.1 session 记录

```go
type session struct {
    ID        agent.SessionID `json:"id"`
    BotID     string          `json:"bot_id"`
    ChatID    string          `json:"chat_id"`
    ThreadID  string          `json:"thread_id"`   // 主会话 = "";话题 = omt_xxx
    Cwd       string          `json:"cwd"`         // 新增:工作目录(话题创建时定死)
    CreatedAt time.Time       `json:"created_at"`
    LastSeen  time.Time       `json:"last_seen"`
    Turns     int             `json:"turns"`
}
```

**关键决策**:

- `Cwd` 是 session 的可变属性(可变但通常不变 —— 只有话题创建时写入一次),不做索引维度
- `ThreadID` 是话题的 session,`""` 保留给未来"跨话题的 session 元数据"(现在不用)
- `Cwd` 为空的老 session(历史遗留):首次访问时用 `bot.Workspace` 默认值补写,不做数据迁移

### 4.2 干掉 `b.cwds` 独立表

```go
// before
type Bridge struct {
    cwds map[string]string  // topicCwdKey -> dir
    mu   sync.Mutex
}

// after
type Bridge struct {
    store     *sessionStore      // session.Cwd 就是权威源
    recentDirs *recentDirsStore  // bot 级 cwd LRU(新增)
    // cwds 消失
}
```

`currentCwd(bot, msg)` 变成"查当前话题的 session.Cwd,没有就查 pending,再没有就用 `bot.Workspace`"。

### 4.3 新增:recentDirsStore

```go
// internal/bridge/recent_dirs.go
type recentDirsStore struct {
    mu   sync.Mutex
    data map[string]*recentDirsEntry  // botID -> entry
    path string                        // 持久化文件路径
}

type recentDirsEntry struct {
    Recent []string `json:"recent"`  // 有序的 cwd 列表(最多 5)
}

// 用法
func (s *recentDirsStore) Add(botID, cwd string)
func (s *recentDirsStore) List(botID string) []string   // 按最近使用顺序,最多 5
func (s *recentDirsStore) MostRecent(botID string) string  // 空 LRU 时返回 ""
```

持久化到 `.feishu-bridge-recent-dirs.json`,和 session store 同目录。

### 4.4 pendingSession

```go
// 话题根卡片上点击 session 按钮时设置,下一条普通消息到达后清空
type pendingSessionMap map[string]string  // threadID -> sessionID ("" = 强制新 session)
```

存 bridge 内存即可(不持久化):进程重启后 pending 消失,下一条消息自然开新 session,不严重。

### 4.5 索引

```go
type sessionStore struct {
    mu    sync.Mutex
    byKey map[string]*session          // 主键: botID/chatID/threadID
    byID  map[agent.SessionID]string   // session_id -> key
    byCwd map[string][]string          // cwd -> []key(按 LastSeen 排序,取最近 5)
    // ...
}
```

`byCwd` 是新加的,专门给话题根卡片的"该 cwd 下的历史 session"按钮用。

---

## 5. 代码改动落点

| 文件 | 改动 |
|---|---|
| `internal/bridge/command.go` | `isBridgeCommand` 收敛到 1 个 `/new`;新增 `recentDirButtons(bot)` 和 `topicRootButtons(bot, thread, cwd)`;砍掉 `chatButtons`、`threadButtons`、`threadContext` |
| `internal/bridge/bridge.go` | `handleCommand` 只分发 `/new`;`onCardAction` 分派 `new_topic`/`resume_session`/`new_session`;干掉 `handleStop/handleStatus/handleHelp/handleSessions/handleResume/handleCd/handleModel/handleReset/handleCancel/handlePwd/handleLs`;新增 `handleNewTopic(cwd)`;`runTurn` 从 pending session map 取 resume ID;新增 `recentDirsStore` 依赖 |
| `internal/bridge/sessions.go` | session 加 `Cwd` 字段(持久化);加 `byCwd` 索引;`SetCwd` 直接改 session 记录 |
| `internal/bridge/recent_dirs.go` | 新文件:`recentDirsStore` |
| `internal/card/card.go` | 保留 `CommandCard` 工厂;`New`/`Streaming`/`Done`/`Processing`/`Error`/`Stopped` 可能可以合并或简化(不再需要 `Stopped`/`Error` 展示卡片按钮) |
| `internal/feishu/client.go` | 无改动(stamp 机制已够用) |
| `cmd/feishubridge/main.go` | 装配时注入 `recentDirsStore` |
| `README.md` | 命令表更新到 1 条 `/new [cwd?]`;加"卡片按钮 = 命令快捷键"说明 |
| `docs/design-session-and-test.md` | 本文档 |
| `docs/feishu-redesign-mockup.png` | 已定稿的 8 帧设计稿 |

**估算**:sessions.go +40 -60(净减 20),recent_dirs.go +120(新),bridge.go +120 -300(净减 180),command.go +80 -200(净减 120),card.go -100(砍掉不用的工厂)。整体是**大幅简化**。

---

## 6. 测试体系设计

保持原文档 §4 的分层方案(第 1 层纯单元测试、第 2 层伪 SDK 集成测试、第 3 层端到端手工用例)。定稿后需要新增/调整的用例:

### 6.1 第 1 层新增用例

```go
// internal/bridge/command_test.go
func TestIsBridgeCommand_OnlyNew(t *testing.T)    // 只有 /new,砍掉其他 11 个
func TestIsBridgeCommand_RejectedPrefixes(t *testing.T)  // /reset /cd /stop /sessions 等都返回 false,并给出提示文案
func TestRecentDirButtons_Max5(t *testing.T)      // 超过 5 个只列 5 个
func TestRecentDirButtons_OrderByMRU(t *testing.T)  // 按最近使用排序
func TestRecentDirButtons_Basename(t *testing.T)  // 显示用 basename,不是全路径
func TestTopicRootButtons_Max5PlusNew(t *testing.T)  // 最多 5 个历史 session + 1 个新
func TestTopicRootButtons_StampActionAndSession(t *testing.T)

// internal/bridge/recent_dirs_test.go
func TestRecentDirsStore_Add_MovesToTop(t *testing.T)
func TestRecentDirsStore_Add_MaxSize5(t *testing.T)
func TestRecentDirsStore_SaveLoad_RoundTrip(t *testing.T)  // t.TempDir()
func TestRecentDirsStore_Empty_MostRecentReturnsEmpty(t *testing.T)
func TestRecentDirsStore_DifferentBotsIsolated(t *testing.T)
```

### 6.2 第 2 层新增场景

```go
// 场景: 主会话发普通消息 → 收到控制台卡片,按钮 = 最近 5 个目录
func TestMainChatPlainText_SendsConsoleCard(t *testing.T) {
    b := newTestBridge(t, fake)
    b.handleMessage(ctx, bot, &feishu.Message{ChatID: "oc_1", ThreadID: ""}, "分析一下")
    got := fake.lastCard()
    buttons := got.Buttons()
    require.LessOrEqual(t, len(buttons), 5)
    for _, btn := range buttons {
        require.Equal(t, "new_topic", btn.Value["action"])
        require.NotEmpty(t, btn.Value["cwd"])
    }
}

// 场景: 主会话 /new → 开新话题,新话题根卡片带该 cwd 下的历史 session 按钮
func TestNew_CreatesTopic_WithSessionButtons(t *testing.T) {
    b := newTestBridge(t, fake)
    b.store.Set(&session{ID: "S1", BotID: "main", ChatID: "oc_1", ThreadID: "omt_old", Cwd: "/api"})
    b.handleCommand(ctx, bot, &feishu.Message{ChatID: "oc_1"}, "/new /api")
    got := fake.lastCard()
    buttons := got.Buttons()
    // 至少 1 个新 session 按钮
    var hasNew bool
    for _, btn := range buttons {
        if btn.Value["action"] == "new_session" { hasNew = true }
    }
    require.True(t, hasNew)
}

// 场景: 点话题根卡片的 resume_session 按钮 → 设置 pending,下一条消息 resume 该 session
func TestClickResumeSession_SetsPending(t *testing.T) {
    b := newTestBridge(t, fake, agentStub)
    b.onCardAction(ctx, &feishu.CardAction{
        ChatID: "oc_1", MessageID: "om_root",
        Action: map[string]any{"action": "resume_session", "thread": "omt_1", "session": "S1"},
        Operator: "ou_user",
    })
    b.runTurn(ctx, bot, &feishu.Message{ChatID: "oc_1", ThreadID: "omt_1"}, "继续")
    require.Equal(t, agent.SessionID("S1"), agentStub.resumes[0])
}

// 场景: cwd 是 session 属性,不漂移
func TestRunTurn_SessionCwdIsAuthoritative(t *testing.T) {
    b := newTestBridge(t, fake, agentStub)
    b.store.Set(&session{ID: "S1", BotID: "main", ChatID: "oc_1", ThreadID: "omt_1", Cwd: "/api"})
    b.runTurn(ctx, bot, &feishu.Message{ChatID: "oc_1", ThreadID: "omt_1"}, "hello")
    require.Equal(t, "/api", agentStub.cfgs[0].Workspace)
}
```

### 6.3 第 3 层:端到端手工用例

E1-E8 用例已抽到 `docs/e2e.md`,包含每步的"动作 / 期望 / 前置条件",
可以直接照着做。`make e2e` 会打印该文档的入口。

改交互、改卡片、改持久化或改 `internal/agent/` 之后必须过一遍 —— 第 1/2 层
测试都跑在进程内,只有第 3 层能验证飞书卡片渲染、话题路由和真 claude 会话。

---

## 7. 落地顺序

按依赖关系排,每步独立可交付、可回滚。

| 步 | 内容 | 依赖 | 预计改动 |
|---|---|---|---|
| 1 | 抽 `Sender` / `Runner` 接口(测试基础设施) | 无 | +40 行,无行为变化 |
| 2 | 新增 `recentDirsStore` 组件 | 无 | +120 行 |
| 3 | 补第 1 层测试(命令、卡片、parse、recentDirsStore) | 1, 2 | +~500 行测试 |
| 4 | 补第 2 层集成测试(fakeSender + agentStub) | 1 | +~300 行测试 |
| 5 | CI 接入 | 3, 4 | +1 workflow |
| 6 | **数据模型重排**:session 加 `Cwd`;干掉 `b.cwds`;`byCwd` 索引 | 3, 4 | +40 -80 行 |
| 7 | **命令面收敛**:砍 11 个命令,只剩 `/new`;更新 `isBridgeCommand` | 3 | 净减 ~100 行 |
| 8 | **卡片重构**:新 `recentDirButtons`、`topicRootButtons`;砍 `chatButtons`、`threadButtons` | 6, 7 | 净减 ~150 行 |
| 9 | **卡片动作分派**:新增 `onCardAction` 三分支(`new_topic`/`resume_session`/`new_session`);`pendingSession` map | 8 | +60 行 |
| 10 | **主会话普通消息 → 控制台卡片**(不自动开话题) | 8 | +20 行 |
| 11 | e2e 用例文档化 + `make e2e` | 5 | 文档 |

**关键**:步 1-5 是测试基础设施,步 6-10 是实际重构。步 7(命令面收敛)风险最低,可以最先上线。步 6 之后是数据模型重排,依赖前 5 步的测试保护。

**不在这次重构里的**:claude 常驻进程。现状是每轮 `claude -p --resume <id>`,保持不动。改成常驻是独立的技术项目,单独排期。交互设计对"每轮一次"和"常驻"两种实现都成立。

---

## 8. 明确不做

- **不做**实时拉取 Claude 的 session 列表。Claude 没有稳定的 `--list-sessions` API,选择器只列**本桥接自己创建过**的 session(`byCwd` 索引)。
- **不做**跨 bot 的 session 共享。`sessionKey` 第一段是 botID,刻意隔离。
- **不做**话题自动归档。飞书话题无归档 API,靠 session reaper 淘汰 session 记录即可。
- **不做**卡片表单/多选项组件。飞书卡片 2.0 的 `form` 能力与 `streaming_mode` 冲突。
- **不做** `/cd` `/sessions` `/resume` `/stop` `/status` `/help` `/model` 等命令。用户想干这些,回主会话点按钮或跟 claude 说。
- **不做**claude 常驻进程(单独项目)。
- **不做**运行时切换模型。模型通过 config 或环境变量固定。
