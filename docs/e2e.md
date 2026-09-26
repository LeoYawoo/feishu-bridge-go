# e2e 手工用例

第 1 层（单元）和第 2 层（集成）都跑在 `go test ./...` 里，CI 每次都跑。
这份文档列出**必须**在真实飞书客户端 + 真实 `claude` 上走一遍的场景 ——
自动化覆盖不到的正是这几类：飞书卡片渲染、话题路由、真 claude 会话创建、
持久化到磁盘后重启恢复。

改交互、改卡片、改持久化，或改 `internal/agent/` 之后，都必须过一遍这张表。
`make e2e` 会把这份文档在终端里高亮一遍，方便当 checklist 用。

---

## 前置

1. `config.json` 里填真实的 `app_id`/`app_secret`（或走 `${FEISHU_APP_ID}`
   `${FEISHU_APP_SECRET}` 环境变量）。
2. 至少 2 个真实目录，后面 E2/E6 用得上。建议：
   - `D:\workcode\work`（主）
   - `D:\workcode\work\test_claude`（次，用来测 LRU 顺序）
3. `claude` 在 PATH 上，`--resume` 语义可用（`claude -p --resume <id> -p hi` 能回
   一句非空）。
4. 用隔离的浏览器 profile（`BridgeTest`）打开飞书网页版，或直接用手机客户端。

启动：

```powershell
.\feishubridge.exe -config .\config.json -loglevel debug
```

准备一个空会话（把机器人单独拉进去），后续步骤都对着它发。

---

## 用例

### E1. 主会话普通文本 → 控制台卡片

**准备**：先点一次 [🆕 最近目录按钮]（如果有的话），让控制台有内容。第一次跑
可以直接跳过这步，看默认卡片。

**动作**：在主会话里发 `你好`（任意非 `/new` 文本）。

**期望**：
- 收到卡片，标题「⚠️ 主会话是控制台」。
- 卡片有 5 个以内按钮（首次可能是 0 个 —— 没有历史 cwd）。
- 每个按钮 `action=new_topic`，`value.cwd` 是完整路径。
- **不能**看到 claude 启动的痕迹（`bridge.log` 里应该没有 `agent run` 行）。

---

### E2. `/new <abs cwd>` → 新话题，根卡片含 cwd 与按钮

**动作**：主会话发 `/new D:\workcode\work`。

**期望**：
- 主会话收到一张绿色卡片「🟢 新话题已开启」，body 里含 `` cwd: `D:\workcode\work` ``。
- 这张卡片会**在话题里再出现一次**（飞书的话题列表里能看到刚开的话题）。
- 展开新话题，根卡片**已更新**：
  - body 同上面。
  - 最下面有 `🆕 新` 按钮；如果该 cwd 之前有过会话，还会有 `▶ <short-id>` 按钮（最多 5 个）。
- `bridge.log` 里能看到 `new topic: omt_xxx in chat oc_xxx cwd=D:\workcode\work history=N`。

---

### E3. 点历史 session 按钮 → 下一条消息 resume 该 session

**前置**：E2 里 `/new` 之后先在话题里发一条消息（例如 `列出当前目录`），等
claude 回完；然后**再 `/new D:\workcode\work`** 开一个话题（同 cwd）。此时新
话题的根卡片应该有一个 `▶ <short-id>` 按钮。

**动作**：点 `▶ <short-id>`，然后在话题里发 `继续` 或任意一条消息。

**期望**：
- `bridge.log` 里能看到 `resume session=<id>`（agent 收到的 resume id 与点按钮
  的短 id 前缀匹配）。
- claude 的回答**记得**上一轮对话（能感知到"列出当前目录"的结果）。

---

### E4. 点 `🆕 新` → 下一条消息开新 session

**前置**：E3 之后，同一话题根卡片。

**动作**：点 `🆕 新`，然后在话题里发 `你好`。

**期望**：
- `bridge.log` 里没有 `resume`（`agent: session=... (no resume)` 或类似标记）。
- claude 的回答**不记得** E3 的对话内容（全新 session）。

---

### E5. 无参 `/new` → 用最近目录

**前置**：先做过 E2。

**动作**：主会话发 `/new`（不带路径）。

**期望**：
- 新话题根卡片 body 里的 cwd 等于 E2 用的路径（`D:\workcode\work`）。

---

### E6. 点最近目录按钮 → 等效 `/new <cwd>`

**前置**：主会话先做过 E5，让 LRU 里有内容。

**动作**：主会话发任意文本触发控制台卡片，点其中一个目录按钮。

**期望**：
- 新话题根卡片的 cwd 与你点的按钮对应。
- 被点的那个目录会**置顶**到 LRU 最前（下次主会话触发控制台卡片时它在第一）。

---

### E7. 话题内 claude 回复不带按钮

**动作**：任意话题内发一条消息，等 claude 回复。

**期望**：claude 的回复卡片**底部没有任何按钮**。设计意图：话题是纯 claude
对话，所有控制面都在根卡片。

---

### E8. 会话文件被删 → resume 优雅失败

**前置**：一个话题里有 claude 会话进行中，能正常续（跑过 E3）。

**动作**：
1. 记下该话题当前的 session id（`bridge.log` 里 `resume session=...` 行）。
2. 在 Windows 资源管理器里删掉 `~/.claude/projects/<encoded-cwd>/<id>.jsonl`
   之类的会话文件（不同版本位置略有差异）。
3. 回飞书，点 `🆕 新` 之外的历史按钮 → 或直接发一条消息触发 resume。

**期望**：
- 收到错误卡片（不崩溃、不循环重试、不停发卡片）。
- `bridge.log` 里有明确的失败原因（claude 返回的 stderr 或 exit code）。
- 话题本身还能用：可以再点 `🆕 新` 开新会话继续。

---

## 收尾

跑完一遍后：

- 如果全部通过，把 commit/tag 记下来（比如 `e2e @ 0205f1a`）。
- 如果任何一步失败，先在 `docs/design-session-and-test.md` 里加一个 case，
  说明期望 vs 实际，再补一层自动化测试能覆盖的，不能覆盖的留在 e2e 里。
