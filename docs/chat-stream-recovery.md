# 聊天流与历史恢复

Web 聊天由 `api/chat_stream.go` 的每轮 collector 负责持久化，HTTP SSE 连接只负责订阅。关闭浏览器、切换会话或断开 HTTP 不会停止后台任务；停止操作走显式 abort 接口。

## 消息与事件边界

- 每轮使用独立 `traceId`，工作流续跑也创建新 trace；`sessionId` 仅表示所属会话。
- 用户消息在任务注入前保存。同一 Bot、同一 assistant trace 的快照串行写入，较旧序号不能覆盖较新快照，未完成快照不能重新打开终态。
- collector 保留文字与工具交错的 `parts`，在终态保存完整快照后再发布 `chat.done` / `chat.error`。`message.done` 本身不表示历史已持久化。
- SSE `id` 是 EventBus 全局单调序号。单条 trace 的序号可以不连续，客户端不能用“相邻序号相差 1”判断丢包。
- 事件发布、回放注册和实时投递使用同一顺序。订阅溢出会明确关闭并要求恢复，不继续发送缺少中间片段的流。

## 恢复协议

`GET /api/chat/resume?botId=…&sessionId=…&traceId=…&sinceSeq=…` 支持 `Last-Event-ID` 作为游标后备来源，并校验 Bot 和会话归属。

服务端先订阅，再读取历史快照，避免查询期间漏掉事件：

1. `start` 标识 trace 与 session。
2. `snapshot` 携带 `{ message: <历史消息 DTO> }`，替换该 trace 已显示的内容。
3. 发送快照 `streamSeq` 之后的文字和工具事件。
4. `done` 或终态 `error` 携带完整文字、工具和有序 parts。

历史 DTO 包含 `streaming`、`streamSeq`、`streamStatus`、`streamError`。终态区分 `completed`、`cancelled`、`dropped`、`error`；旧数据的空状态表示未知。服务重启时仍在 streaming 的快照标记为中断。

恢复旧会话时，如果没有运行任务和可重播事件，遗留的 streaming 回复会保留已有内容并标记中断；仅有用户消息且没有可恢复回复时返回明确错误，不保持空连接等待。

客户端以 trace 合并历史与临时 assistant 行，并用最新快照序号跳过重播增量。没有终态的 EOF 是断线。`error.recoverable=true` 表示重新获取历史与任务状态后恢复订阅，**不得重新 POST send**。错误终态同样要应用它携带的完整快照，再显示错误提示。

`persistenceError` 表示任务已接受或回复已完成，但历史保存失败。界面保留回复并提示复制留存，不把它当作可自动重发的请求。

事件缓存溢出且过程事件无法恢复时，collector 继续等待后台任务收尾，保留最终回复，并用 `streamError` 提示过程记录不完整。完成状态下的这类提示不代表任务失败。收尾通知独立于事件缓存，避免终态事件过期后一直显示运行中。

旧版本并发写入造成的重复 assistant 行，在历史查询与分页前选取同 trace 的有效快照；原数据库行保留，避免自动删除历史数据。

## 回归

```bash
cd web
npm run test:regression
```

后端从仓库根目录执行：

```bash
go test -race ./agent/outbound ./api -run 'Test(MemoryEventBus|Chat|UpsertAssistant|WebChannel|WebWorkflow|LoadContext)'
go test ./dao
```

`api/chat_stream_integration_test.go` 使用真实 HTTP 连接验证发送、断开连接、后台收尾、历史读取和 resume，模型与外部渠道由测试夹具代替。前端脚本覆盖分块 UTF-8、重复事件、快照推进、终态回退、跨会话过期响应、停止失败及草稿恢复。

`api/chat_stream_failure_test.go` 注入写库失败和事件缓存溢出，验证最终回复、保存失败提示与终态仍可送达。

右侧 `/api/sessions/:sid/*` 工具接口中的 `sid` 仍为 **Bot ID**，文件、终端和累计用量共享于 Bot；它与上述聊天 `sessionId` 的作用域不同。
