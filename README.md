# feishubridge

基于飞书 Golang SDK 的消息桥接：**飞书 WebSocket → Claude Code**。

在飞书里运行 `claude` / `codex`,通过飞书消息与会话交互。卡片流式更新
(打字机效果),底部带交互按钮。

参考实现: `feishu-bridge/`(Python 版)。本实现只保留核心路径,
约 1000 行 Go,无 cgo 依赖。

## 架构

**心智模型**: 主会话 = 会话管理控制台,话题 = claude 会话。

```
┌─────────────────────────────────────────────────────┐
│ 主会话 (chat 顶层)                                     │
│   /new [cwd?]  ← 唯一命令                             │
│   普通消息 → 控制台卡片(5 个"最近目录"按钮)              │
└──────────────────────┬──────────────────────────────┘
                       │ /new → 开新话题
                       ▼
┌─────────────────────────────────────────────────────┐
│ 话题 (thread) = claude 会话                           │
│   cwd 创建时定死,不可改                                │
│   话题根卡片: 5 个历史 session 按钮 + [🆕 新]           │
│   普通消息 → claude 回复(卡片无按钮)                    │
│   claude -p --resume <id>                             │
└─────────────────────────────────────────────────────┘
```

**没有文字命令的话题**: 想中断/切 session,走卡片按钮或跟 claude 说。
飞书端不再解释 claude 的 `/exit` `/compact` 等内置命令,直接转发。

**为什么不需要 shell 进程**: cwd 现在是 `session.Cwd` 字段(持久化到
`.feishu-bridge-sessions.json`),跨平台天然成立。早期版本 `/pwd` `/cd`
`/ls` 是常驻 `pwsh` 进程,现在全部砍掉 —— 主会话只做会话管理,不做文件浏览。

## 前置条件

- Go 1.22+
- Claude Code (`claude`) 在 PATH 中（`codex` 也可以）
- 飞书开放平台机器人，已开启「机器人接收消息」与「卡片回调」能力

**不需要** PowerShell。

## 安装

```bash
git clone <repo>
cd feishu-bridge-go
go mod tidy
go build -o feishubridge ./cmd/feishubridge
```

### 交叉编译（多平台）

```bash
make release          # 全矩阵：windows/linux/darwin × amd64/arm64 -> dist/
make release-windows  # 仅 windows
make release-linux    # 仅 linux
make test             # go test ./...
make vet              # go vet ./...
make clean            # 清理 dist/ 和本地二进制
```

版本号通过 `-ldflags` 注入，默认取 `git describe`，可用 `VERSION=v1.2.3 make release` 覆盖。

`build.bat` 是本地一键构建脚本（`vet` + `test` + `build`），里面写死了你机器上的
Go 工具链路径，所以已加入 `.gitignore`——不提交。跨平台构建用 Makefile。

> **Windows 用户**：本机 make 叫 `mingw32-make`（mingw64 自带），例如
> `C:\soft\mingw\mingw64\bin\mingw32-make.exe`。Makefile 已针对它验证过，
> 用法完全一样：`mingw32-make release`。`build.bat` 已把该路径加入 PATH。
>
> 没有 make 的话也可以直接跑等价的 go 命令：
>
> ```bash
> for p in windows/amd64 linux/amd64 linux/arm64; do
>   os=${p%%/*}; arch=${p##*/}
>   GOOS=$os GOARCH=$arch go build -trimpath -ldflags "-s -w" \
>     -o dist/feishubridge-$os-$arch ./cmd/feishubridge
> done
> ```

## 配置

复制示例配置并按注释填写：

```bash
cp config.example.json ~/.config/feishu-bridge/config.json
```

或 Windows：`%USERPROFILE%\.config\feishu-bridge\config.json`

凭证建议走环境变量，配置里写 `${FEISHU_APP_ID}` 占位符：

```powershell
$env:FEISHU_APP_ID = "cli_xxx"
$env:FEISHU_APP_SECRET = "xxx"
```

配置发现顺序：`-config` 参数 > `$FEISHU_BRIDGE_CONFIG` >
`~/.config/feishu-bridge/config.json`。

### 字段说明

| 字段 | 说明 |
|---|---|
| `app_id` / `app_secret` | 飞书应用凭证，支持 `${VAR}` 占位 |
| `domain` | `feishu`（国内）或 `lark`（海外 Lark） |
| `agent.command` | `claude` 或 `codex`，也可填绝对路径 |
| `agent.model` | 模型名，空则用 claude 默认 |
| `agent.timeout_seconds` | 单次会话硬超时（默认 300） |
| `agent.append_system_prompt` | 追加到系统提示词 |
| `bots[]` | 多 bot 列表，每个独立 workspace + ACL |
| `bots[].allowed_users` | open_id 白名单，`["*"]` 放行全部 |
| `bots[].group_mode` | `disabled` / `mention-all` / `owner-only` |
| `bots[].owner_open_id` | 机器人自己的 open_id，群聊 @ 判定用它（见下） |
| `bot_chats` | `chat_id` → `bot id` 映射，多 bot 时必填（见下） |
| `streaming.throttle_ms` | 卡片流式更新间隔（默认 800ms） |
| `streaming.session_idle_seconds` | Claude 会话记录闲置多久后丢弃（默认 604800s = 7 天） |

`bots[].shell` 仍能被解析（老配置不报错）但被忽略，可以删掉。

### 关于 `bot_chats`

单 bot 时不需要填——所有会话都路由给它。多 bot 时它是唯一的分配依据：
消息里没有任何信号能说明这个会话属于哪个工作区，所以必须由你显式声明。

- 未映射的 `chat_id` **不会** fallback 到第一个 bot，而是直接丢弃并记日志。
  多 bot 场景下每个 bot 有自己的 workspace 和凭证，猜错比不响应危险。
- `bot_chats` 里的 `bot id` 写错会在启动时报配置错误，不会静默走兜底。
- 取 `chat_id`：把 bot 拉进目标群，发任意消息，日志里的 `ChatID` 字段就是。

### 关于 `owner_open_id`

飞书 SDK 没有"查询当前机器人自己 open_id"的接口，所以必须手动填。
取法：把 bot 私聊一遍，日志里会打印对方的 `open_id` —— 那行就是 bot 自己的
open_id；或者在飞书开放平台的"凭证与基础信息"页查。

- `mention-all` 下留空可运行，但会退化为"群里所有白名单用户的消息都响应"，
  @ 判定失效。
- `owner-only` 下留空则 bot 完全不响应群消息。

### 卡片按钮

结果卡片底部有交互按钮,点击等价于输入对应命令,并且只在被点击人通过
`allowed_users` 校验时才生效。主会话卡片是"最近目录"按钮(点击=用该 cwd
开新话题),话题根卡片是"历史 session"按钮 + `[🆕 新]`(点击=设置 pending,
下一条消息续/新建 session)。

### cwd 生命周期

cwd 现在是 session 的属性(`session.Cwd`),持久化到 `.feishu-bridge-sessions.json`
(见下)。话题创建后 cwd 不可变 —— 想换 cwd 只能回主会话开新话题。

### Claude 会话持久化

`session_id` ↔ 飞书话题的映射写在 `<workspace>/.feishu-bridge-sessions.json`，
每条记录带 `bot_id`、`chat_id`、`thread_id`、`cwd`、`last_seen`、`turns`。

- **重启后能接回旧会话**。bridge 重启后,同一个话题发的第一条消息会用
  `--resume` 续上之前的 Claude 上下文,而不是从头开始。
- **cwd 是 session 的属性**。想恢复某个 cwd 下的历史 session,主会话点控制台
  卡片上的 `🆕 <dir>` 按钮,新话题根卡片就会列出该 cwd 下的最近 5 个 session。
- 文件损坏不会阻断启动——映射丢失,但 Claude 自己的 transcript 文件还在磁盘上。

**另外持久化** `<workspace>/.feishu-bridge-recent-dirs.json`: bot 级"最近
使用过的 cwd"LRU(最多 5 个),供主会话控制台卡片和 `/new` 无参模式使用。
- 闲置超过 `session_idle_seconds`（默认 7 天）的记录被 reaper 丢弃，
  Claude 本地的 transcript 不受影响，只是 bridge 不再知道它属于哪个话题。
- `/new` 删除当前话题的记录；`/status` 显示总会话数。

### 多实例部署

一个 bridge 独占一个 workspace 和一套飞书凭证。**同一个 app_id 不要连两台机器**
——飞书长连接会被抢占。多机部署的正确做法是每台机器一个飞书应用，各自配
`bot_chats` 指向自己负责的群，天然隔离。

## 运行

```bash
feishubridge -config ~/.config/feishu-bridge/config.json -loglevel debug
```

## 在飞书里使用

**心智模型**:

- **主会话**(chat 顶层)= 会话管理控制台。唯一命令 `/new [cwd?]`。
  发普通消息收到控制台卡片(带最近目录按钮),不会启动 claude。
- **话题** = claude 会话。cwd 创建时定死,话题内无文字命令。
  所有切换/中断走卡片按钮。

| 位置 | 输入 | 结果 |
|---|---|---|
| 主会话 | `/new` | 用**最近使用过的目录**(bot 级 LRU)开新话题 |
| 主会话 | `/new D:\workcode\api` | 用该 cwd 开新话题(不存在的路径报错) |
| 主会话 | 其他文本 | 收到控制台卡片(5 个以内"最近目录"按钮) |
| 主会话 · 卡片 | 点 `🆕 api (最近)` | 等效 `/new D:\workcode\api` |
| 话题根卡片 · 卡片 | 点 `▶ 3b2526fd` | 下一条消息 `--resume 3b2526fd` |
| 话题根卡片 · 卡片 | 点 `🆕 新` | 下一条消息开新 session(不 resume) |
| 话题 | 任意文本 | 作为提示词发给 claude(卡片无按钮) |

**卡片按钮 = 命令快捷键**:点击 = 用户在输入框里输入对应命令。按钮不引入新动作。

**没有的**:
- 话题内**没有**文字命令(`/cd` `/sessions` `/resume` `/stop` `/status` `/help` `/model` 全部砍掉)。
- **没有** `/cd`。想换 cwd,回主会话开新话题。
- **没有**运行时切换模型。通过 config 或环境变量固定。
- **没有**实时列 claude 的 session。话题根卡片只列**本桥接自己创建过**的 session。

群聊需要 @机器人 才会响应(`group_mode: mention-all`)。

**交互设计稿**:见 `docs/feishu-redesign-mockup.png` 和 `docs/design-session-and-test.md`。

## 与参考实现的差异

参考项目 20000+ 行 Python，本实现刻意省略了：

- **后台任务**（`bg_supervisor` / `bg_tasks.db` / wake socket）— 需要独立的
  reconciler 进程，不是核心交互路径
- **配额控制**（`quota.py`）— 依赖飞书计费 API
- **多 runtime**（`runtime_pi` / `runtime_omp`）— 只保留 claude/codex
- **session journal / resume index**（`session_journal.py` / `session_resume.py`）
  — 直接用 `--resume`，不做本地索引
- **memory 注入**（`compact-context.md` / `MEMORY.md` 解析）
- **消息去重 TTL**（`MessageDedup`）
- **合并转发展开**（`merge_forward` 需要额外的批量 GET API）
- **图片/文件消息**（只处理 `text` 与 `interactive`）
- **常驻 shell 进程**（用 `os.ReadDir` 替代，见架构说明）

保留的核心行为：

- 单进程 WS → 进程 → `claude -p`，不引入中间层
- 同一 `claude` 二进制、同一 `cwd`，CLAUDE.md / hooks / skills 全部生效
- 流式卡片 + 交互按钮 + 卡片回调
- 超时预算、静默超时、取消语义
- 群聊门禁、用户白名单
- 卡片体积限制（28KB payload / 10000 div chars）

## 目录结构

```
cmd/feishubridge/main.go        入口
internal/config/                配置加载与校验
internal/feishu/                飞书 SDK 封装（WS / 发消息 / 卡片 / 回调）
internal/agent/                 claude -p 调用 + stream-json 解析
internal/card/                  卡片构建（流式 + 交互按钮）
internal/bridge/                组装层：消息路由、命令、会话管理、工作目录
```

## 已验证

- `gofmt` / `go vet` / `go test ./...` 通过
- 单元测试: `recentDirsStore` 9 个用例(增/查/持久化/损坏文件恢复/多 bot 隔离)
- 飞书端到端待重跑(交互重设计后,原 `/pwd` `/cd` `/ls` `/status` `/help`
  用例已不适用,新用例 E1-E8 见 `docs/design-session-and-test.md` §6.3)
- 卡片 schema 2.0 按钮渲染验证(不再触发 ErrCode 200861)
