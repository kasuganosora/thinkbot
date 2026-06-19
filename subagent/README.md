# SubAgent — 上下文隔离的轻量 Agent

## 设计目标

SubAgent 是一个**轻量级 LLM 调用封装**，核心目的是 **隔离上下文**。

当主 Agent 需要执行一个子任务（如代码审查、文档摘要、分类判断）时，直接在主对话中追加这些操作会**污染主对话的上下文**，导致后续对话质量下降。

SubAgent 通过独立的 `ContextManager` 解决这个问题：

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
| 生命周期 | 长期运行 | **临时**（用完 Close） |

## 快速开始

```go
import "github.com/kasuganosora/thinkbot/subagent"

// 从主 Agent 的 LLMBundle 创建 SubAgent
bundle, _ := bot.CreateLLMBundle(configStore, "mybot")

sub := subagent.New(bundle.Main, "glm-5.2",
    subagent.WithSystemPrompt("你是一个代码审查专家，请简洁回答"),
    subagent.WithMaxMessages(10),  // 滑动窗口：保留最近 5 轮
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

## 在 Stage 中使用

```go
func (s *ReviewStage) Process(ctx context.Context, env *core.Envelope) error {
    bundle := s.getLLMBundle() // 从 Stage 初始化时注入

    sub := subagent.New(bundle.Main, s.model,
        subagent.WithSystemPrompt(s.reviewPrompt),
        subagent.WithMaxTurns(3),
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

## API

### 核心

| 方法 | 说明 |
|------|------|
| `New(provider, model, opts...)` | 创建 SubAgent |
| `Chat(ctx, text)` | 发送消息，返回回复文本 |
| `ChatWithResult(ctx, text)` | 同上，返回完整 `GenerateResult` |
| `Stream(ctx, text)` | 流式发送 |
| `Clear()` | 重置上下文 |
| `Close()` | 关闭（释放资源） |

### 上下文管理

| 方法 | 说明 |
|------|------|
| `History()` | 获取上下文消息副本 |
| `TurnCount()` | 获取总对话轮数 |
| `SeedMessages(msgs)` | 预填充上下文 |
| `SetSystem(prompt)` | 动态修改系统提示词 |

### Options

| Option | 默认值 | 说明 |
|--------|--------|------|
| `WithSystemPrompt` | `""` | 系统提示词 |
| `WithTemperature` | `0.7` | 温度 |
| `WithMaxTokens` | `4096` | 最大输出 token |
| `WithMaxMessages` | `20` | 滑动窗口大小（0=无限制） |
| `WithTools` | `nil` | 工具定义 |
| `WithResponseFormat` | `nil` | 响应格式 |
| `WithID` / `WithName` | `""` | 标识符 |

## 滑动窗口机制

当消息数超过 `maxMessages` 时，自动从头部丢弃最早的消息：

```
窗口=4, 3轮对话后：

[u1, a1, u2, a2, u3, a3]
         ↓ 截断
[u2, a2, u3, a3]  ← 保留最近 2 轮
```

截断时保证不切断半轮对话（总是从 user 消息开始）。
