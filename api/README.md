# api — HTTP API 服务层

基于 Gin 的 Web API 服务，提供 Bot 管理、用户认证、聊天交互、系统配置、工作流监控、定时任务、技能管理等 RESTful 接口。

## 设计原则

- **配置管理走 API，运行时控制走 Agent 工具**：Bot 配置、定时任务、技能开关等基础设施管理通过 REST API 暴露；工作流的创建和控制由 Agent 通过 `task` 系列工具完成，API 只提供只读监控 + 崩溃恢复 + 节点重试
- **最小暴露面**：每个端点都有明确的存在理由，避免与 Agent 工具链重复暴露运行时控制接口
- **统一日志管道**：Gin 的所有输出（请求日志、panic 恢复、内部警告）通过 `zapRecovery` 和 `zapWriter` 集成到 `util/log` 配置的 zap 管道
- **审计追踪**：写操作通过 `auditLog()` 记录操作者、动作、关键参数；`requestLogger` 中间件自动附加 Trace ID、用户身份和脱敏后的请求体预览

## 功能

- **RESTful API**：认证、用户管理、Bot 管理（CRUD + 启停）、平台配置、Provider/模型管理、统计数据
- **SSE 流式聊天**：`WebChannel` 将用户消息注入 Bot Pipeline，回复以 SSE 事件流推送；支持中止、追加、按 traceID 断线重连
- **认证中间件**：Cookie + JWT 会话管理，按角色权限控制接口访问
- **SPA 静态服务**：自动检测 `static/` 目录并提供前端单页应用；带内容哈希的资源长缓存，`index.html` 强制 `no-store`
- **梦境巩固管理**：按 Bot 配置/触发/监控梦境巩固管线
- **定时任务管理**：按 Bot 创建/暂停/恢复/触发 cron jobs
- **工作流监控**：查询工作流状态/节点/指标、节点重试 + 崩溃恢复
- **技能与 MCP 管理**：全局技能启停、Bot 级技能与 MCP 服务器管理
- **记忆管理**：查询/新增/更新/删除 Bot 记忆条目及统计
- **容器与沙箱运维**：容器启停、快照、导入导出、终端执行、文件浏览与上传
- **系统监控**：运行时健康检查、事件总线指标

## Pipeline 集成

BotService 在装配 Pipeline 时，用以下中间件包装 LLMStage（`pipeline.WithMiddleware`，从外到内）：

| 中间件 | 职责 |
|--------|------|
| `stages.NoteCaptureMiddleware("exchange", umeWriter)` | 捕获 LLM 回复为 L0 工作记忆笔记（供 dreaming 巩固），并把用户入站原文写入 `user_message_events` 事件流 |
| `VerificationGateMiddleware` | 结果校验闸门 |
| `TokenQuotaMiddlewareWithState` | Token 月度配额：按 Bot/Channel/Chat 维度限额，超额拦截 |
| `LoopDetectionMiddleware` | 检测重复工具调用模式（不豁免任何工具），注入软/硬警告 |
| `LazyResponseMiddleware` | 检测敷衍/偷懒回复 |
| `TokenBudgetMiddlewareWithState` | 按 Channel 追踪累计 Token，阈值告警和硬限制 |

**全链路 Token 记账**：BotService 在装配时创建共享 `pipeline.NewTokenQuotaState().WithStatsRecorder(statsRecorder)`，并用 `llm.NewQuotaRecordingProvider` 包裹 LLM Provider（`bundle.Main`、`bundle.Light`、`bundle.Vision`，外层再套 `llm.NewStatsRecordingProvider`）。配额中间件将 dimension 注入 context，所有经过 Provider 的 LLM 调用（包括 SubAgent、Workflow、Memory 等绕过 pipeline 的调用点）都自动记账，防止漏记。用量最终聚合到 `stats_usage_daily` 表。

## 关键类型

| 类型 | 说明 |
|------|------|
| `Server` | Gin HTTP 服务器封装，持有全部 handler 依赖 |
| `BotService` | Bot 业务服务层（CRUD + 运行时生命周期 + 子系统访问） |
| `WorkflowService` | 工作流引擎服务（懒初始化；提供只读监控、崩溃恢复与卡死看门狗） |
| `WebChannel` | Web 聊天 Channel（输入端 + 输出端） |
| `CookieManager` | Cookie/会话管理（JWT，`SessionClaims`） |
| `ChatHistoryService` | 聊天历史持久化与游标分页查询（`HistoryPage`） |

fx 模块 `api.Module` 提供 EventBus、CookieManager、BotService、ChatHistoryService、WorkflowService、SkillManager、ToolPerm 服务和 Server；
`OnStart` 时启动 `status=running` 的 Bot、恢复中断工作流、启动看门狗（卡死回收 + 配额续跑）并在后台运行 HTTP Server。

## 中间件链

```
gin.DefaultWriter/ErrorWriter → zapWriter（Gin 内部输出统一走 zap）
zapRecovery → traceIDMiddleware → requestLogger → corsMiddleware → cookieAuth → requirePermission
```

| 中间件 | 职责 |
|--------|------|
| `zapRecovery` | 替代 `gin.Recovery()`，panic 时通过 zap 记录堆栈和请求上下文 |
| `traceIDMiddleware` | 为每个请求注入或复用 Trace ID（`X-Trace-ID`） |
| `requestLogger` | 记录 method/path/status/duration/ip/user/traceId 与脱敏 body，4xx+5xx 用 Warn |
| `corsMiddleware` | CORS 处理，空白名单时允许 localhost |
| `cookieAuth` | Cookie + JWT 会话认证，回查 DB 校验角色/状态实时性 |
| `requirePermission` | 基于角色的权限检查 |

## 主要路由

### 认证（公开）

```
POST /api/auth/login               — 登录
POST /api/auth/logout              — 登出
```

### 当前用户（需登录）

```
GET  /api/auth/me                  — 当前用户信息
PUT  /api/auth/password            — 修改密码
```

### 授权码与身份绑定（需登录）

```
POST   /api/bindcode               — 生成一次性绑定码
GET    /api/bindcode               — 列出自己的绑定码
GET    /api/bindings               — 列出已绑定的平台身份
DELETE /api/bindings/:id           — 解除绑定
```

### 用户管理（admin，`user.manage`）

```
GET    /api/users                  — 用户列表
POST   /api/users                  — 创建用户
GET    /api/users/:id              — 用户详情
PUT    /api/users/:id              — 更新用户资料
DELETE /api/users/:id              — 删除用户
PUT    /api/users/:id/role         — 修改角色
PUT    /api/users/:id/disable      — 禁用用户
PUT    /api/users/:id/enable       — 启用用户
PUT    /api/users/:id/password     — 重置密码
```

### Bot 管理

```
GET    /api/bots                   — Bot 列表（所有登录用户）
GET    /api/bots/:id               — Bot 详情（所有登录用户）
POST   /api/bots                   — 创建 Bot（admin）
PUT    /api/bots/:id               — 更新 Bot（admin）
DELETE /api/bots/:id               — 删除 Bot（admin）
POST   /api/bots/:id/start         — 启动 Bot（admin）
POST   /api/bots/:id/stop          — 停止 Bot（admin）
```

以下 Bot 子资源均需 `bot.manage` 权限。

### 平台配置（admin，嵌套在 Bot 下）

```
GET    /api/bots/:id/platforms         — 列出 Bot 的平台配置
POST   /api/bots/:id/platforms         — 创建平台配置
PUT    /api/bots/:id/platforms/:pid    — 更新平台配置
DELETE /api/bots/:id/platforms/:pid    — 删除平台配置
GET    /api/bots/platforms/tool-catalog — 平台工具目录（所有登录用户，驱动前端表单）
```

> 旧的 `/api/bots/:id/channels` 与 `/api/channels/types` 已废弃并从路由中移除，统一由上述 Platform API 取代。

### 工具权限与发言模式（admin，嵌套在 Bot 下）

```
GET    /api/bots/:id/tool-permissions               — 列出工具权限规则
POST   /api/bots/:id/tool-permissions               — 创建规则
PUT    /api/bots/:id/tool-permissions/:rid          — 更新规则
DELETE /api/bots/:id/tool-permissions/:rid          — 删除规则
POST   /api/bots/:id/tool-permissions/reset-defaults — 恢复默认规则
GET    /api/bots/:id/tools                          — Bot 已注册的工具列表

GET    /api/bots/:id/outbound                       — 各渠道发言模式（active/passive/mute）
PUT    /api/bots/:id/outbound                       — 设置发言模式
```

### 浏览器 Cookie 管理（admin，嵌套在 Bot 下）

```
GET    /api/bots/:id/browser/cookies            — 列表（value 掩码）
GET    /api/bots/:id/browser/cookies/:cid       — 单条（?reveal=true 返回完整值）
POST   /api/bots/:id/browser/cookies            — 新增单条
PUT    /api/bots/:id/browser/cookies/:cid       — 编辑单条
DELETE /api/bots/:id/browser/cookies/:cid       — 删除单条
DELETE /api/bots/:id/browser/cookies            — 清空（?domain= 可按域清）
POST   /api/bots/:id/browser/cookies/import     — 批量导入
GET    /api/bots/:id/browser/cookies/export     — 导出 storageState（?confirm=1）
```

### 定时任务（admin，嵌套在 Bot 下）

```
GET    /api/bots/:id/cron            — 列出 Bot 的定时任务
POST   /api/bots/:id/cron            — 创建定时任务
GET    /api/bots/:id/cron/:jobId     — 获取任务详情
PUT    /api/bots/:id/cron/:jobId     — 更新任务
DELETE /api/bots/:id/cron/:jobId     — 删除任务
POST   /api/bots/:id/cron/:jobId/pause   — 暂停任务
POST   /api/bots/:id/cron/:jobId/resume  — 恢复任务
POST   /api/bots/:id/cron/:jobId/trigger — 手动触发任务
```

### 梦境巩固（admin，嵌套在 Bot 下）

```
GET  /api/bots/:id/dreaming         — 获取梦境配置
PUT  /api/bots/:id/dreaming         — 更新梦境配置
GET  /api/bots/:id/dreaming/status  — 梦境运行时状态（cron job + 调度器摘要）
POST /api/bots/:id/dreaming/trigger — 手动触发一次梦境巩固
```

### 记忆管理（admin，嵌套在 Bot 下）

```
GET    /api/bots/:id/memory         — 查询分层记忆（?tier=L0/L1/L2/L3&limit=）
GET    /api/bots/:id/memory/stats   — 记忆统计（L1 计数 / L2 估算）
DELETE /api/bots/:id/memory/entry   — 删除单条分层记忆（?id=&tier=&scope=）
```

### 文件与容器（admin，嵌套在 Bot 下）

```
GET    /api/bots/:id/files                 — 列出工作空间文件
GET    /api/bots/:id/files/download        — 下载文件
POST   /api/bots/:id/files/mkdir           — 新建目录
POST   /api/bots/:id/files/upload          — 上传文件

GET    /api/bots/:id/container             — 容器信息
PUT    /api/bots/:id/container/config      — 更新容器配置
DELETE /api/bots/:id/container             — 移除容器
POST   /api/bots/:id/container/start       — 启动容器
POST   /api/bots/:id/container/stop        — 停止容器
GET    /api/bots/:id/container/snapshots   — 快照列表
POST   /api/bots/:id/container/snapshots   — 创建快照
POST   /api/bots/:id/container/export      — 导出容器
POST   /api/bots/:id/container/import      — 导入容器
POST   /api/bots/:id/container/restore     — 从快照恢复

GET    /api/bots/:id/runtime-checks        — 运行时检查（真实 sandbox 状态）
GET    /api/bots/:id/terminal              — 终端信息
POST   /api/bots/:id/terminal/exec         — 在容器中执行命令
```

### Bot 运行时配置（admin，嵌套在 Bot 下）

```
GET    /api/bots/:id/access                 — 访问控制（默认行为 + 规则）
PUT    /api/bots/:id/access                 — 更新访问控制

GET    /api/bots/:id/heartbeat              — 心跳配置
PUT    /api/bots/:id/heartbeat              — 更新心跳配置
GET    /api/bots/:id/heartbeat/logs         — 心跳日志
DELETE /api/bots/:id/heartbeat/logs         — 清空心跳日志

GET    /api/bots/:id/compaction             — 上下文压缩配置
PUT    /api/bots/:id/compaction             — 更新压缩配置
GET    /api/bots/:id/compaction/history     — 压缩历史
DELETE /api/bots/:id/compaction/history     — 清空压缩历史
```

聊天节奏（rhythm）已合并进平台配置：作为 `rhythm` 字段随 `GET/PUT /api/bots/:id/platforms` 读写（支持 telegram / misskey，web 不参与）。

### Bot 技能与 MCP（admin，嵌套在 Bot 下）

```
GET    /api/bots/:id/skills          — Bot 技能列表
GET    /api/bots/:id/skills/:sid     — 技能详情
POST   /api/bots/:id/skills          — 添加技能
PUT    /api/bots/:id/skills/:sid     — 更新技能
DELETE /api/bots/:id/skills/:sid     — 移除技能

GET    /api/bots/:id/mcp             — MCP 服务器列表
POST   /api/bots/:id/mcp             — 添加 MCP 服务器
PUT    /api/bots/:id/mcp/:mid        — 更新 MCP 服务器
DELETE /api/bots/:id/mcp/:mid        — 移除 MCP 服务器
POST   /api/bots/:id/mcp/import      — 批量导入 MCP 配置
```

### 会话管理（admin）

```
GET    /api/bots/:id/sessions        — 列出 Bot 的会话
POST   /api/bots/:id/sessions        — 新建会话
PUT    /api/sessions/:sid            — 更新会话（重命名等）
DELETE /api/sessions/:sid            — 删除会话

GET    /api/sessions/:sid/status             — 会话状态
POST   /api/sessions/:sid/compact            — 手动触发上下文压缩
GET    /api/sessions/:sid/terminal           — 会话终端信息
POST   /api/sessions/:sid/terminal/exec      — 会话内执行命令
GET    /api/sessions/:sid/files              — 会话工作目录文件列表
GET    /api/sessions/:sid/files/download     — 下载文件
POST   /api/sessions/:sid/files/mkdir        — 新建目录
POST   /api/sessions/:sid/files/upload       — 上传文件
```

### Provider 与模型管理（admin）

```
GET    /api/providers                       — Provider 列表
POST   /api/providers                       — 创建 Provider
PUT    /api/providers/:pid                  — 更新 Provider
DELETE /api/providers/:pid                  — 删除 Provider
POST   /api/providers/:pid/test             — 连通性测试
POST   /api/providers/:pid/models           — 添加模型
PUT    /api/providers/:pid/models/:mid      — 更新模型
DELETE /api/providers/:pid/models/:mid      — 删除模型
POST   /api/providers/:pid/models/import    — 批量导入模型
```

### 搜索提供方（admin）

```
GET    /api/search/providers          — 列表
POST   /api/search/providers          — 创建
PUT    /api/search/providers/:id      — 更新
DELETE /api/search/providers/:id      — 删除
PUT    /api/search/providers/:id/toggle — 启用/禁用
```

### 工作流监控

> 工作流的创建（Submit）与流程控制由 Agent 通过 `task` / `task_control` 工具完成。
> 状态/节点查询与节点重试对**任意已登录用户**开放（对话中的 workflow 卡片需要）；
> 列表、崩溃恢复、指标属运营操作，需 `bot.manage`。

```
GET  /api/workflows/:wfId                      — 查询工作流状态（登录用户）
GET  /api/workflows/:wfId/nodes                — 查询节点列表（登录用户）
POST /api/workflows/:wfId/nodes/:nodeId/retry  — 重试节点（登录用户）
POST /api/workflows/:wfId/continue             — 续跑（重注入续跑消息，用于续跑回复因重启丢失的恢复）（登录用户）
GET  /api/session-workflow                     — 按会话查最近一条工作流（登录用户）

GET  /api/workflows                — 列出工作流（admin）
POST /api/workflows/recover        — 恢复中断的工作流（admin）
GET  /api/workflows/metrics        — 工作流引擎指标（admin）
```

### 技能管理（admin）

```
GET  /api/skills                   — 列出所有技能
GET  /api/skills/:name             — 技能详情
PUT  /api/skills/:name/enable      — 启用技能
PUT  /api/skills/:name/disable     — 禁用技能
```

### 聊天（需 `bot.use` 权限）

```
GET  /api/chat/bots                — 可聊天 Bot 列表
GET  /api/chat/history             — 聊天历史（游标分页）
POST /api/chat/send                — SSE 流式聊天
POST /api/chat/abort               — 中止正在执行的聊天
POST /api/chat/append              — 生成中追加用户补充（同一轮）
GET  /api/chat/active              — 查询后台仍在执行的任务 traceID
GET  /api/chat/resume              — 按 traceID 重连续流（SSE）

POST /api/chat/token-budget/reset  — 重置 token 预算（admin，解除预算卡死）
```

### 系统配置（admin，`system.config`）

```
GET  /api/config                   — 获取全部配置
GET  /api/config/:key              — 获取单个配置项
PUT  /api/config/:key              — 设置配置项
PUT  /api/config                   — 批量设置配置项
```

### 统计数据（admin，`user.manage`）

```
GET  /api/stats/overview           — 统计概览
GET  /api/stats/daily              — 按日区间统计
GET  /api/stats/daily-by-bot       — 按日 + Bot 维度统计
GET  /api/stats/records            — 明细记录
GET  /api/stats/by-bot-model       — 按 Bot + 模型聚合
GET  /api/stats/bots/:id           — Bot 统计
GET  /api/stats/bots/:id/daily     — Bot 每日统计
```

### 系统监控

```
GET  /api/system/health            — 详细健康检查（admin，`system.config`）
GET  /api/system/events/metrics    — 事件总线指标（admin，`system.config`）
GET  /health                       — 健康检查（公开，仅返回 ok）
```

### Swagger API 文档

```
GET  /swagger/*any                 — Swagger UI 与规范文件
GET  /swagger/index.html           — 交互式 API 文档
GET  /swagger/doc.json             — OpenAPI 规范 JSON
```

启动服务后访问 `http://localhost:8080/swagger/index.html` 即可查看完整的交互式 API 文档。

**生成文档**（handler 注释变更后需重新生成）：

```bash
# 安装 swag CLI（仅需一次）
go install github.com/swaggo/swag/cmd/swag@latest

# 生成文档（输出到 docs/）
swag init -g cmd/main.go -o docs --parseDependency --parseInternal
```

每个 handler 函数上方的 `// @Summary`、`// @Param`、`// @Router` 等注解会被 swag 解析，自动生成规范文件。
