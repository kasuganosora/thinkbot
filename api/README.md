# api — HTTP API 服务层

基于 Gin 的 Web API 服务，提供 Bot 管理、用户认证、聊天交互、系统配置等 RESTful 接口。

## 功能

- **RESTful API**：认证、用户管理、Bot 管理（CRUD + 启停）、Channel 配置、统计数据
- **SSE 流式聊天**：`WebChannel` 将用户消息注入 Bot Pipeline，回复以 SSE 事件流推送
- **认证中间件**：Cookie + Session 会话管理，按角色权限控制接口访问
- **SPA 静态服务**：自动检测 `static/` 目录并提供前端单页应用

## 关键类型

| 类型 | 说明 |
|------|------|
| `Server` | Gin HTTP 服务器封装 |
| `BotService` | Bot 业务服务层 |
| `WebChannel` | Web 聊天 Channel（输入端 + 输出端） |
| `CookieManager` | Cookie/会话管理（JWT） |
| `ChatHistoryService` | 聊天历史持久化与分页查询 |

## 主要路由

```
POST /api/auth/login       — 登录
GET  /api/bots             — Bot 列表
POST /api/chat/send        — SSE 流式聊天
GET  /api/stats/overview   — 统计概览
PUT  /api/config/:key      — 设置配置项
```
