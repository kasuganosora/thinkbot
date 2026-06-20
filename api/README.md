# api — HTTP API 服务层

基于 Gin 的 Web API 服务，提供 Bot 管理、用户认证、聊天交互、系统配置、工作流监控、定时任务、技能管理等 RESTful 接口。

## 设计原则

- **配置管理走 API，运行时控制走 Agent 工具**：Bot 配置、定时任务、技能开关等基础设施管理通过 REST API 暴露；工作流的创建和控制由 Agent 通过 `task` 系列工具完成，API 只提供只读监控
- **最小暴露面**：每个端点都有明确的存在理由，避免与 Agent 工具链重复暴露运行时控制接口
- **统一日志管道**：Gin 的所有输出（请求日志、panic 恢复、内部警告）通过 `zapRecovery` 和 `zapWriter` 集成到 `util/log` 配置的 zap 管道
- **审计追踪**：所有写操作通过 `auditLog()` 记录操作者、动作、关键参数；`requestLogger` 中间件自动附加 Trace ID 和用户身份

## 功能

- **RESTful API**：认证、用户管理、Bot 管理（CRUD + 启停）、Channel 配置、统计数据
- **SSE 流式聊天**：`WebChannel` 将用户消息注入 Bot Pipeline，回复以 SSE 事件流推送
- **认证中间件**：Cookie + Session 会话管理，按角色权限控制接口访问
- **SPA 静态服务**：自动检测 `static/` 目录并提供前端单页应用
- **梦境巩固管理**：按 Bot 配置/触发/监控梦境巩固管线
- **定时任务管理**：按 Bot 创建/暂停/恢复/触发 cron jobs
- **工作流监控**：只读查询工作流状态/节点/指标 + 崩溃恢复（创建和控制由 Agent 工具完成）
- **技能管理**：列出/启用/禁用已注册技能
- **记忆查询**：只读访问 Bot 的分层记忆（L0~L3）
- **系统监控**：运行时健康检查、事件总线指标

## 关键类型

| 类型 | 说明 |
|------|------|
| `Server` | Gin HTTP 服务器封装 |
| `BotService` | Bot 业务服务层（CRUD + 运行时生命周期 + 子系统访问） |
| `WorkflowService` | 工作流引擎服务（懒初始化，从 BotService 获取 LLM Provider；提供只读监控和崩溃恢复） |
| `WebChannel` | Web 聊天 Channel（输入端 + 输出端） |
| `CookieManager` | Cookie/会话管理（JWT） |
| `ChatHistoryService` | 聊天历史持久化与分页查询 |

## 中间件链

```
gin.DefaultWriter/ErrorWriter → zapWriter（Gin 内部输出统一走 zap）
zapRecovery → traceIDMiddleware → requestLogger → corsMiddleware → cookieAuth → requirePermission
```

| 中间件 | 职责 |
|--------|------|
| `zapRecovery` | 替代 `gin.Recovery()`，panic 时通过 zap 记录堆栈和请求上下文 |
| `traceIDMiddleware` | 为每个请求注入或复用 Trace ID（`X-Trace-ID`） |
| `requestLogger` | 记录 method/path/status/duration/ip/user/traceId，4xx+5xx 用 Warn |
| `corsMiddleware` | CORS 处理，空白名单时允许 localhost |
| `cookieAuth` | Cookie + JWT 会话认证 |
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

### 用户管理（admin）

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

### Channel 配置（admin，嵌套在 Bot 下）

```
GET    /api/bots/:id/channels         — 列出 Bot 的 Channel 配置
POST   /api/bots/:id/channels         — 创建 Channel 配置
PUT    /api/bots/:id/channels/:cid    — 更新 Channel 配置
DELETE /api/bots/:id/channels/:cid    — 删除 Channel 配置
GET    /api/channels/types            — Channel 类型列表（所有登录用户，驱动前端表单）
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

### 记忆查询（admin，嵌套在 Bot 下）

```
GET  /api/bots/:id/memory          — 查询分层记忆（?tier=L1&limit=20）
GET  /api/bots/:id/memory/stats    — 记忆统计
```

### 工作流监控（admin，只读 + 恢复）

> 工作流的创建（Submit）、流程控制（retry/terminate）由 Agent 通过 `task` / `task_control` 工具完成，不通过 REST API 暴露。终止操作由 session 生命周期信号触发，连通 pipeline 一起终止。

```
GET  /api/workflows                — 列出工作流
POST /api/workflows/recover        — 恢复中断的工作流
GET  /api/workflows/metrics        — 工作流引擎指标
GET  /api/workflows/:wfId          — 查询工作流状态
GET  /api/workflows/:wfId/nodes    — 查询节点列表（?format=flat|tree）
```

### 技能管理（admin）

```
GET  /api/skills                   — 列出所有技能
GET  /api/skills/:name             — 技能详情
PUT  /api/skills/:name/enable      — 启用技能
PUT  /api/skills/:name/disable     — 禁用技能
```

### 聊天（需 bot.use 权限）

```
GET  /api/chat/bots                — 可聊天 Bot 列表
GET  /api/chat/history             — 聊天历史（游标分页）
POST /api/chat/send                — SSE 流式聊天
```

### 系统配置（admin）

```
GET  /api/config                   — 获取全部配置
GET  /api/config/:key              — 获取单个配置项
PUT  /api/config/:key              — 设置配置项
PUT  /api/config                   — 批量设置配置项
```

### 统计数据（admin）

```
GET  /api/stats/overview           — 统计概览
GET  /api/stats/bots/:id           — Bot 统计
GET  /api/stats/bots/:id/daily     — Bot 每日统计
```

### 系统监控（admin）

```
GET  /api/system/health            — 详细健康检查（内存/goroutine/运行时间）
GET  /api/system/events/metrics    — 事件总线指标
GET  /health                       — 健康检查（公开，仅返回 ok）
```

### Swagger API 文档

```
GET  /swagger/index.html           — Swagger UI（交互式 API 文档）
GET  /swagger/swagger.json         — OpenAPI 3.0 JSON 规范
GET  /swagger/swagger.yaml         — OpenAPI 3.0 YAML 规范
```

启动服务后访问 `http://localhost:8080/swagger/index.html` 即可查看完整的交互式 API 文档。

**生成文档**（handler 注释变更后需重新生成）：

```bash
# 安装 swag CLI（仅需一次）
go install github.com/swaggo/swag/cmd/swag@latest

# 生成文档
swag init -g cmd/main.go -o docs --parseDependency --parseInternal
```

每个 handler 函数上方的 `// @Summary`、`// @Param`、`// @Router` 等注解会被 swag 解析，自动生成 OpenAPI 3.0 规范。
