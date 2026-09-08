# SubAgent — 上下文隔离的轻量 Agent

## 设计目标

SubAgent 是一个**轻量级 LLM 调用封装**，核心目的是 **隔离上下文**。

当主 Agent 需要执行子任务（如代码审查、文档摘要、分类判断）时，直接在主对话中追加这些操作会**污染主对话的上下文**，导致后续对话质量下降。SubAgent 通过独立的 `ContextManager` 解决这个问题：

```
主 Agent 对话上下文           SubAgent 对话上下文
┌─────────────────┐          ┌─────────────────┐
│ user: 帮我写代码  │          │ user: 审查这段代码 │
│ assistant: ...   │          │ assistant: ...   │
│ user: 再加个功能  │          │ user: 有什么风险？ │
│ assistant: ...   │          │ assistant: ...   │
└─────────────────┘          └─────────────────┘
        ↑ 互不干扰 ↑
```

## 与主 Agent 的关系

| 特性 | 主 Agent (Bot) | SubAgent |
|------|---------------|----------|
| LLM Provider | 自有（LLMBundle） | **继承主 Agent** |
| 对话上下文 | 独立 | **独立**（隔离） |
| 记忆系统 | 有（MemoryStage） | **无** |
| Pipeline | 有 | **无** |
| Channel 监听 | 有 | **无**（只能被调用） |
| 工具执行 | 有（ToolManager） | **可选**：`SubAgentManager.SetToolResolver` 注入主 Agent 工作空间工具后，子 Agent 可经多步编排回路执行（自动排除 spawn 防套娃；可再经 `WithToolAllowlist` 收敛）；独立 SubAgent 也能用 `WithTools`+`WithToolSteps` 启用 |
| 上下文压缩 | — | **可选**：`WithAutoCompact` / `WithCompactor`（LLM 语义摘要）+ `WithReduction`（工具输出缩减） |
| 生命周期 | 长期运行 | **临时 / 持久化**（取决于使用模式） |

---

## 快速开始

### 方式一：直接使用 SubAgent（临时实例）

```go
import "github.com/kasuganosora/thinkbot/subagent"

// 从主 Agent 的 LLMBundle 创建
bundle, _ := bot.CreateLLMBundle(configStore, "mybot")

sub := subagent.New(bundle.Main, "glm-5.2",
    subagent.WithSystemPrompt("你是一个代码审查专家，请简洁回答"),
    subagent.WithMaxMessages(10),  // 滑动窗口：保留最近 10 条消息
    subagent.WithName("code-reviewer"),
)
defer sub.Close()

// 第一轮
reply, err := sub.Chat(ctx, "审查这段 Go 代码: func main() { ... }")

// 第二轮（带上第一轮上下文，但不影响主 Agent）
reply2, err := sub.Chat(ctx, "这个函数有什么并发安全问题？")

// 重置上下文（不重新创建实例）
sub.Clear()
```

### 方式二：通过 Manager — 一次性委托（Delegate）

```go
mgr := subagent.NewSubAgentManager(bundle.Main, "glm-5.2")
defer mgr.CloseAll()

// 创建临时 SubAgent → 执行 → 自动关闭
result, err := mgr.Delegate(ctx,
    "你是一个翻译专家",
    "将以下内容翻译为英文：你好世界",
)
```

### 方式三：通过 Manager — 并发批量委托（DelegateMany）

```go
results := mgr.DelegateMany(ctx,
    "你是一个代码审查专家",
    []string{
        "审查 auth.go 的安全性",
        "审查 api.go 的错误处理",
        "审查 dao.go 的 SQL 注入风险",
    },
)

for _, r := range results {
    if r.Success {
        fmt.Printf("✓ %s\n%s\n\n", r.Task, r.Text)
    } else {
        fmt.Printf("✗ %s: %s\n", r.Task, r.Error)
    }
}
```

### 方式四：通过 Manager — 持久化多轮对话（Spawn/Chat/Close）

```go
// 创建持久化 SubAgent（返回 ID）
id, _ := mgr.Spawn("你是一个数据分析助手", "data-analyst")

// 多轮对话
reply1, turns1, _ := mgr.Chat(ctx, id, "分析这组数据: [1,2,3,4,5]")
reply2, turns2, _ := mgr.Chat(ctx, id, "计算标准差")

// 查看活跃的 SubAgent
for _, info := range mgr.List() {
    fmt.Printf("%s (%s) — %d turns\n", info.Name, info.ID, info.Turns)
}

// 用完关闭
mgr.Close(id)
```

---

## 作为 LLM 工具注册（spawn 工具）

将 SubAgent 能力暴露为主 Agent 的工具，LLM 可自主决定何时委托子任务：

```go
saMgr := subagent.NewSubAgentManager(bundle.Main, "glm-5.2",
    subagent.WithMaxMessages(10),
    subagent.WithTemperature(0.3),
)
defer saMgr.CloseAll()

// 注册到 ToolManager
subagent.RegisterTools(toolMgr, saMgr)
```

LLM 调用示例：

```json
spawn({
  "tasks": ["分析这段代码的安全风险", "检查性能瓶颈"],
  "system_prompt": "你是一个代码审查专家"
})
```

- `tasks`：任务列表，每个任务在独立 SubAgent 中并行执行，最多 5 个（超出截断并告警）
- `system_prompt`：子 Agent 角色定义（可选）
- 返回所有任务结果（同步等待）；执行期间每 30s 发送 heartbeat 进度重置前端计时器，并通过 `WithDelegateProgress` 把每个子 Agent 的启动/完成实时推到 UI

`SpawnToolDef` 同时注册了 Prompt Section（Order=305），指导 LLM 何时使用委托。

---

## API 参考

### SubAgent 核心

| 方法 | 签名 | 说明 |
|------|------|------|
| `New` | `(provider, model, opts...) → *SubAgent` | 创建 SubAgent |
| `Chat` | `(ctx, text) → (string, error)` | 发送消息，返回回复文本，自动更新上下文 |
| `ChatWithResult` | `(ctx, text) → (*GenerateResult, error)` | 同上，返回完整结果（含 Usage） |
| `Stream` | `(ctx, text) → (*StreamResult, error)` | 流式发送，流结束后更新上下文 |
| `Clear` | `()` | 重置上下文（保留配置，只清除历史） |
| `Close` | `()` | 关闭，释放上下文（可安全多次调用） |

### SubAgent 上下文与元数据

| 方法 | 说明 |
|------|------|
| `History()` | 返回当前消息列表副本 |
| `TurnCount()` | 总对话轮数（不受滑动窗口截断影响） |
| `SeedMessages(msgs)` | 预填充上下文（首次 Chat 前调用） |
| `SetSystem(prompt)` | 动态修改系统提示词（影响后续所有调用） |
| `ID()` / `Name()` | 标识符 |
| `String()` | 可读描述：`SubAgent(name, model=xxx, turns=N)` |

### Options

| Option | 默认值 | 说明 |
|--------|--------|------|
| `WithSystemPrompt(prompt)` | `""` | 系统提示词 |
| `WithTemperature(temp)` | `0.7` | LLM 温度（0.0 ~ 2.0） |
| `WithMaxTokens(n)` | `4096` | 最大输出 token |
| `WithMaxMessages(n)` | `20` | 滑动窗口大小（0 = 无限制） |
| `WithTools(tools...)` | `nil` | 注入带 Execute 的工具；配合 `WithToolSteps(>0)` 时经多步编排回路自动执行，无步数预算则仅作为定义传给 LLM |
| `WithResponseFormat(fmt)` | `nil` | 响应格式（如 JSON 模式） |
| `WithID(id)` | `""` | 标识符 |
| `WithName(name)` | `""` | 显示名称 |
| `WithToolSteps(n)` | `0` | 带工具执行回路时的最大 LLM 步数预算（0 = 包默认 20；需配合 `WithTools`） |
| `WithCallTimeout(d)` | `0` | 覆盖 `Delegate`/`DelegateMany` 的固定超时（对 `DelegateStream` 无效） |
| `WithStuckTimeout(d)` | `0` | `DelegateStream` 卡死看门狗阈值（默认 180s，对固定超时的一刀切替代） |
| `WithChatTimeout(d)` | `10min` | 持久 subagent（Spawn/Chat）单次交互的墙钟硬上限，防无限阻塞 |
| `WithSkipTools()` | — | 跳过 `SubAgentManager` 的工具注入（纯 LLM 任务，避免误走多步编排循环） |
| `WithToolAllowlist(names...)` | 空 | 工具白名单：未列出的工具在注入前即被过滤，对 SubAgent 完全不可见（空 = 不限制；`WithSkipTools` 优先级更高） |
| `WithTopP(p)` | nil | 核采样参数（不设则由 Provider 用默认 top_p） |
| `WithFrequencyPenalty(p)` / `WithPresencePenalty(p)` | `0.1` / `0.05` | 重复抑制（GLM-5.x 推荐），防长工具链下退化 |
| `WithCompactor(c)` | nil | 显式注入语义压缩器；建议每个 subagent 独立实例（内部有跨调用状态） |
| `WithAutoCompact()` | 关闭 | compactor 为空时自动建独立 Compactor，并默认开启 `WithReduction(DefaultReductionConfig)` |
| `WithReduction(rc)` | 零值 | 工具输出缩减（in-loop 按 step 裁剪超大/过旧工具结果）；零值 = 关闭 |

---

## SubAgentManager

管理 SubAgent 的生命周期，支持三种使用模式：

### 模式对比

| 模式 | 方法 | 上下文 | 生命周期 | 适用场景 |
|------|------|--------|---------|---------|
| **Delegate** | `Delegate(ctx, prompt, task)` | 一次性 | 用完自动 Close | 单次任务（翻译、摘要、分类） |
| **DelegateMany** | `DelegateMany(ctx, prompt, tasks[])` | 每任务独立 | 全部完成后自动 Close | 并行批量处理 |
| **Spawn/Chat/Close** | `Spawn` → `Chat` → `Close` | 持久化 | 手动管理 | 多轮对话（分析、迭代审查） |

### 卡死看门狗（Delegate/DelegateStream/DelegateMany 共用）

委托系调用不再用固定超时一刀切，而是区分「慢但活着」与「真卡死」：

- **stuck**：连续无任何流片段输出（默认 180s，`WithStuckTimeout` 覆盖）才判卡死。任意片段都算活跃——文本/推理 token、工具调用/结果皆是，长 exec 不会被误杀；另有首片段宽限期（`stuck/2`，上限 60s）容忍思考阶段
- **hard**：总运行时长的墙钟硬上限（`stuck × 10`；DelegateMany 下若超过 `delegateTimeout` 则收口到它），只拦「永远在吐 token 但永不结束」的失控流
- **leak**：`hard + 30s` 后流仍未关闭（provider 不响应取消）则强制截断 drain，防消费 goroutine 泄漏
- 看门狗轮询间隔按 `min(stuck, hard)/2` 收紧（100ms ~ 5s），保证比轮询间隔更短的阈值也能被检测到
- provider 不支持流式时退化为单次生成 + hard 超时
- 被终止时返回区分 `stuck` / `hard` / `leak` 的错误并落 Warn 日志

### Manager 配置

| 方法 | 默认值 | 说明 |
|------|--------|------|
| `SetDelegateTimeout(d)` | 120s | DelegateMany 单任务的硬上限收口值（Delegate/DelegateStream 由看门狗接管，不受此约束） |
| `DelegateTimeout()` | — | 返回当前委托超时（供上层装配断言） |
| `SetMaxConcurrency(n)` | 5 | DelegateMany 的最大并发数（仅接受 >0；可用全局配置 `subagent.max_concurrency` 覆盖） |
| `SetAutoCompact(bool)` | 关闭 | 为所有委托/派生的 subagent 默认开启压缩：`WithAutoCompact` + `WithReduction(DefaultReductionConfig)` 追加进默认选项 |
| `List()` | — | 返回所有活跃 SubAgent 信息 |
| `CloseAll()` | — | 关闭所有持久化 SubAgent |

### Manager 方法

| 方法 | 说明 |
|------|------|
| `Delegate(ctx, systemPrompt, task, opts...)` | 一次性委托，返回 `(string, error)`；已收敛为 `DelegateStream` 的薄封装，同享看门狗 |
| `DelegateMany(ctx, systemPrompt, tasks[], opts...)` | 并发批量委托，返回 `[]TaskResult` |
| `DelegateStream(ctx, systemPrompt, task, opts...)` | 流式委托，卡死看门狗保护，返回 `(string, error)` |
| `Spawn(systemPrompt, name, opts...)` | 创建持久化 SubAgent，返回 `(id, error)` |
| `Chat(ctx, id, message)` | 向持久化 SubAgent 发消息，返回 `(reply, turns, error)`；ctx 无截止时间时套 `chatTimeout` 墙钟硬上限 |
| `Close(id)` | 关闭并移除指定 SubAgent |
| `List()` | 列出所有活跃 SubAgent |
| `CloseAll()` | 关闭全部 |
| `SetToolResolver(toolMgr, base)` | 让委托创建的子 Agent 继承主 Agent 工作空间工具（自动排除 spawn 防套娃） |
| `SetDefaultToolSteps(n)` | 带工具回路时的默认最大 LLM 步数预算（0 = 包默认） |

### TaskResult

```go
type TaskResult struct {
    Task    string // 原始任务描述
    Text    string // LLM 回复（成功时）
    Success bool   // 是否成功
    Error   string // 错误信息（失败时）
}
```

### SubAgentInfo

```go
type SubAgentInfo struct {
    ID    string // 标识符
    Name  string // 名称
    Turns int    // 对话轮数
}
```

### 并发控制

`DelegateMany` 使用信号量（semaphore）控制并发：

```go
mgr.SetMaxConcurrency(3)  // 最多同时 3 个 SubAgent 调用 LLM
results := mgr.DelegateMany(ctx, "翻译为英文", []string{
    "文本1", "文本2", "文本3", "文本4", "文本5",
})
// 前三个并行执行，完成后执行剩余两个
```

- 每个 SubAgent 有独立的超时 context
- 结果顺序与输入一致（通过 index 映射）
- 默认并发 5，可通过 `SetMaxConcurrency` 调整

---

## ContextManager — 滑动窗口

`SubAgent` 内部使用 `ContextManager` 管理对话历史：

```go
type ContextManager struct {
    messages    []llm.Message
    maxMessages int  // 0 = 无限制

    // 溢出时可把最早整批消息压缩成单条摘要消息替代删除（压缩优先）
    summarizeHead func(ctx, head) (llm.Message, bool)
}
```

### 截断机制

当消息数超过 `maxMessages` 时：

- **压缩优先**：若注入了 `summarizeHead`（SubAgent 持有 compactor + provider 时由 `New` 注入），溢出的最早整批消息被 LLM 压缩为单条 `[Conversation Summary]` 摘要消息，早期上下文不再永久丢失；摘要后仍超上限则继续压缩直到收敛，摘要不减少消息数时回退纯删除
- **回退纯删除**：丢弃最早的消息，且保证**不切断半轮对话**——如果截断点恰好是 assistant 消息，会继续向后找到下一个 user 消息作为起始点，确保上下文总是从 user 消息开始（找不到 user 消息时退化为保留最后 `maxMessages` 条）

```
窗口=4, 3 轮对话后：

[u1, a1, u2, a2, u3, a3]
         ↓ 截断
[u2, a2, u3, a3]  ← 保留最近 2 轮（或：[摘要, u2, a2, u3, a3]）
```

### ContextManager API

| 方法 | 说明 |
|------|------|
| `NewContextManager(max)` | 创建（max=0 不限制） |
| `Append(msg)` | 追加单条消息（无 ctx：溢出只能纯删除） |
| `AppendWithCtx(ctx, msg)` | 同上，携带 ctx 以便溢出时触发 LLM 摘要（正常对话持久化用） |
| `AppendTurn(user, assistant)` | 追加一轮对话（user + assistant；无 ctx 变体） |
| `AppendTurnWithCtx(ctx, user, assistant)` | 同上，携带 ctx 触发摘要 |
| `Messages()` | 返回消息切片（直接引用，不应修改） |
| `Clear()` | 清空 |
| `Len()` | 当前消息数 |

---

## 线程安全

| 组件 | 并发安全 | 机制 |
|------|---------|------|
| `SubAgent` | ✓ | `sync.Mutex`（Chat/Stream/Clear/Close 互斥） |
| `SubAgentManager` | ✓ | `sync.RWMutex`（Spawn/Chat/Close/List 互斥；读路径 RLock，避免并发委托被串行化） |
| `ContextManager` | ✓ | `sync.Mutex`（所有方法互斥） |

`SubAgentManager.List()` 采用两段式锁（先在锁内快照 SubAgent 引用，释放锁后再读 Sa 内部状态），避免锁层级依赖。

---

## Stream 行为细节

`Stream()` 返回包装后的 channel，行为：

1. 用户读取 `StreamResult.Stream` channel 获取 `StreamPart`
2. 后台 goroutine 逐个转发 part，同时累积文本
3. 流结束后自动将 user 消息 + 完整回复追加到上下文
4. context 取消时 goroutine 安全退出，不泄漏

```go
result, _ := sub.Stream(ctx, "写一首关于春天的诗")
for part := range result.Stream {
    if delta, ok := part.(*llm.TextDeltaPart); ok {
        fmt.Print(delta.Text)
    }
}
// 流结束后上下文已自动更新，下一轮 Chat 会带上这轮对话
```

带工具时 `Stream` 走 `llm.OrchestrateStream` 多步流式回路：先转发全部片段（含工具调用/结果），流关闭后把 user 消息与完整编排消息（`result.Messages`）依次写回上下文，保证多轮连贯。

---

## 自动上下文防御（防 context 爆炸）

子 Agent 带工具长循环（读大文件 → 跑命令 → 再读）容易把上下文养爆，喂到硬上限看门狗仍无法收敛。两层机制针对不同失控模式：

| 机制 | 粒度 | 防护对象 | 开启方式 |
|------|------|---------|---------|
| 语义压缩（Compactor） | 对话轮次 | 多轮场景：滑窗溢出时把旧头部 LLM 摘要为单条 `[Conversation Summary]`，早期上下文不丢 | `WithAutoCompact()`（每 subagent 独立 Compactor）或 `WithCompactor(c)` |
| 工具输出缩减（Reduction） | 编排 step | 单轮委托：工具执行后截断超大单条结果（OnToolResults）+ 每步 LLM 调用前裁剪历史中过旧大结果（PrepareStep） | `WithReduction(rc)`，零值 = 关闭 |

两套钩子只在带工具的编排回路挂载（`PrepareStep` / `OnToolResults`），未启用时零开销；与主 Agent 的 llmroute 接同一套机制。推荐经 `SubAgentManager.SetAutoCompact(true)` 为所有委托统一开启。

---

## 在 Stage 中使用

```go
func (s *ReviewStage) Process(ctx context.Context, env *core.Envelope) error {
    bundle := s.getLLMBundle()

    sub := subagent.New(bundle.Main, s.model,
        subagent.WithSystemPrompt(s.reviewPrompt),
        subagent.WithMaxMessages(6),
        subagent.WithTemperature(0.3),
    )
    defer sub.Close()

    result, err := sub.ChatWithResult(ctx, env.Message.Text)
    if err != nil {
        return err
    }

    env.AddAction(core.NewActionReply(result.Text))
    return nil
}
```

---

## 文件结构

| 文件 | 职责 |
|------|------|
| `subagent.go` | `SubAgent` 类型、Chat/Stream/Clear/Close、参数构建、压缩/缩减钩子 |
| `manager.go` | `SubAgentManager`、Delegate/DelegateMany/Spawn/Chat/Close、并发控制、卡死看门狗、进度回调 |
| `context.go` | `ContextManager`、滑动窗口截断（压缩优先，回退纯删除） |
| `options.go` | Functional Options、工具白名单过滤、`String()` |
| `tools.go` | `SpawnToolDef` / `RegisterTools`、spawn 工具定义（DeferredLoad 延迟加载）+ 心跳保活 + 流式进度 + Prompt Section |
