package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/gin-gonic/gin"

	"github.com/kasuganosora/thinkbot/agent/command"
	"github.com/kasuganosora/thinkbot/agent/memory"
	"github.com/kasuganosora/thinkbot/agent/outbound"
	"github.com/kasuganosora/thinkbot/agent/stages"
	agenttools "github.com/kasuganosora/thinkbot/agent/tools"
	"github.com/kasuganosora/thinkbot/config"
	"github.com/kasuganosora/thinkbot/dao"
	"github.com/kasuganosora/thinkbot/llm"
	"github.com/kasuganosora/thinkbot/util/errs"
	"github.com/kasuganosora/thinkbot/util/idgen"
	"github.com/kasuganosora/thinkbot/workflow"
)

// ============================================================================
// 聊天 Handler — SSE 流式对话 + 可聊 Bot 列表
// ============================================================================

// SSE 事件类型
const (
	sseTextDelta    = "text_delta"    // LLM 文本增量
	sseDone         = "done"          // 生成完成
	sseError        = "error"         // 错误
	sseStart        = "start"         // 开始处理
	sseToolCall     = "tool_call"     // 工具调用
	sseToolProgress = "tool_progress" // 工具增量输出
	sseToolResult   = "tool_result"   // 工具结果
	ssePing         = "ping"          // 心跳
)

// phantomSupersededMsg 是 phantom tool call 被真实调用取代时展示的说明。
//
// phantom call 指某些 LLM（如 GLM）流式输出工具调用时先发的空参数占位 call：
// 它永远不会被执行、也永远收不到结果，若不主动收敛就会永久停在「执行中」。
const phantomSupersededMsg = "已被后续同工具调用取代（LLM 流式中间态）"

// extractToolStatus 从工具返回的 output 中提取真实终态 status。
// user_choice 等工具在 output map 里返回 status 字段（timeout/answered/cancelled），
// 需要透传到 SSE 事件和落库的 toolCalls.status，否则前端刷新后无法区分
// "工具成功完成"和"工具等待超时"，导致已过期的 choice 卡片被恢复为可交互态。
func extractToolStatus(output any) string {
	m, ok := output.(map[string]any)
	if !ok {
		return ""
	}
	s, _ := m["status"].(string)
	// 只认已知的合法终态值，避免把无关字段当 status
	switch s {
	case "timeout", "cancelled", "killed", "answered", "resolved":
		return s
	default:
		return ""
	}
}

// isEmptyToolInput 判断工具入参是否为空占位。
//
// 空参数是 phantom call 的判定特征：真实调用必然带参数（url / maxChars 等），
// 而流式中间态的占位调用参数恒为 {}。注意必须是「解析出 map 且长度为 0」，
// nil 或非 map 不算 —— 那是事件缺字段，不能据此断定为 phantom。
func isEmptyToolInput(input any) bool {
	m, ok := input.(map[string]any)
	return ok && len(m) == 0
}

// isPhantomRunning 判断一条累积的工具调用是否为「仍在 running 的空参数 phantom」。
func isPhantomRunning(tc map[string]any) bool {
	return tc["status"] == "running" && isEmptyToolInput(tc["input"])
}

// findPhantomToolCall 在累积列表中查找应被 newInput 这次真实调用取代的 phantom 下标。
// 返回 -1 表示无需取代（newInput 本身是空参数，或不存在同名 phantom）。
func findPhantomToolCall(toolCalls []map[string]any, toolName string, newInput any) int {
	if isEmptyToolInput(newInput) {
		return -1
	}
	for i, tc := range toolCalls {
		if tc["name"] == toolName && isPhantomRunning(tc) {
			return i // 只取代最先命中的一个 phantom
		}
	}
	return -1
}

// markToolCallSuperseded 把一条工具调用标记为已被取代。
func markToolCallSuperseded(tc map[string]any) {
	tc["status"] = "superseded"
	tc["output"] = phantomSupersededMsg
}

// metaKeyChatSessionID 是注入消息 metadata 时前端会话 ID 的 key。
// 与 agenttools.ExtraKeyChatSessionID 同名——后者负责把它从 metadata 搬进
// ToolSessionContext.Extra，供工具（如工作流提交）记录来源会话。
const metaKeyChatSessionID = agenttools.ExtraKeyChatSessionID

// streamPersistInterval 是流式回复增量落库的最小间隔。
//
// 文本增量事件每个 token 触发一次，逐条写库会打满 SQLite 并与历史查询争锁，
// 因此按此间隔合并写入。工具调用等稀疏关键节点会强制立即落库，不受该间隔限制。
// 取值权衡：太大则刷新后丢失的尾部内容多，太小则写放大明显。2s 下最坏丢约 2s 文本。
const streamPersistInterval = 2 * time.Second

// handleChatBots 返回当前可聊天的 Bot 列表（状态为 running）。
// GET /api/chat/bots
//
// @Summary      可聊 Bot 列表
// @Description  返回当前处于运行状态的 Bot 列表
// @Tags         聊天
// @Produce      json
// @Success      200  {object}  Response
// @Security     CookieAuth
// @Router       /api/chat/bots [get]
func (s *Server) handleChatBots(c *gin.Context) {
	defs, err := s.botSvc.ListDefinitions()
	if err != nil {
		Fail(c, err)
		return
	}

	type chatBot struct {
		ID      string `json:"id"`
		Name    string `json:"name"`
		Running bool   `json:"running"`
	}

	var result []chatBot
	for _, def := range defs {
		if s.botSvc.IsRunning(def.ID) {
			result = append(result, chatBot{
				ID:      def.ID,
				Name:    def.Name,
				Running: true,
			})
		}
	}

	OK(c, result)
}

// handleChatAbort 中止一条正在执行的聊天请求。
// POST /api/chat/abort
//
// @Summary      中止聊天请求
// @Description  按 botId + traceId 中止一条正在执行的聊天链路（包括工具执行）
// @Tags         聊天
// @Accept       json
// @Produce      json
// @Param        body  body      ChatAbortReq  true  "中止请求"
// @Success      200   {object}  Response
// @Failure      400   {object}  Response
// @Failure      401   {object}  Response
// @Security     CookieAuth
// @Router       /api/chat/abort [post]
func (s *Server) handleChatAbort(c *gin.Context) {
	var req ChatAbortReq
	if err := c.ShouldBindJSON(&req); err != nil {
		Fail(c, errs.BadRequest("invalid request body: "+err.Error()))
		return
	}
	// 三个粒度：traceId（精确一条）> sessionId（该会话全部）> all（该 bot 全部，
	// 含解析不出会话归属的系统回合）。墙钟硬上限默认关闭后人工中止是唯一的收口手段，
	// 三种入口都要留痕（见 abortActor）。
	who := abortActor(c)
	switch {
	case req.TraceID != "":
		aborted := s.botSvc.AbortMessage(req.BotID, req.TraceID)
		s.logger.Infow("chat abort by trace",
			"bot_id", req.BotID, "trace_id", req.TraceID, "aborted", aborted, "user", who)
		OK(c, map[string]any{"aborted": aborted})
	case req.SessionID != "":
		n := s.botSvc.AbortSession(req.BotID, req.SessionID)
		// 可观测：人工兜底中止是判断「哪一会话经常挂住」的唯一来源，必须留痕
		// （内容型日志用 Infow，且带上操作者，事后能回答「谁停的、停了几个」）。
		s.logger.Infow("chat abort by session",
			"bot_id", req.BotID, "session_id", req.SessionID, "aborted_count", n, "user", who)
		OK(c, map[string]any{"aborted": n > 0, "count": n})
	case req.All:
		n := s.botSvc.AbortBot(req.BotID)
		s.logger.Infow("chat abort all turns of bot",
			"bot_id", req.BotID, "aborted_count", n, "user", who)
		OK(c, map[string]any{"aborted": n > 0, "count": n})
	default:
		Fail(c, errs.BadRequest("traceId, sessionId or all required"))
	}
}

// abortActor 取发起中止的操作者用户名（审计用）；未登录时记为 anonymous。
func abortActor(c *gin.Context) string {
	if u := currentUser(c); u != nil && u.Username != "" {
		return u.Username
	}
	return "anonymous"
}

// handleChatAppend 在一条正在执行的聊天（生成中）过程中，接收用户中途追加的内容，
// 注入 botID+traID 这同一轮对话（Claude-CLI 风格的「边思考/边输出边补充」）。
//
// 与 /send 的区别：它不开启新一轮、不新建 traceID、不返回 SSE；当前 /send 的 SSE
// 流继续把模型结合补充后的输出推给原客户端。若本轮已结束（accepted=false），
// 前端应退化为一次普通的 /send。被接受的补充会异步落库为用户消息，保证后续轮次
// 上下文一致。
// POST /api/chat/append
func (s *Server) handleChatAppend(c *gin.Context) {
	var req ChatAppendReq
	if err := c.ShouldBindJSON(&req); err != nil {
		Fail(c, errs.BadRequest("invalid request body: "+err.Error()))
		return
	}

	if _, ok := s.botSvc.GetWebChannel(req.BotID); !ok {
		Fail(c, errs.NotFound("bot is not running or not available for chat"))
		return
	}
	user := currentUser(c)
	if user == nil {
		Fail(c, errs.Unauthorized("not logged in"))
		return
	}
	userID := fmt.Sprintf("%d", user.ID)
	// The trace determines the target session. A stale browser tab must not
	// inject into one task while recording its supplement in another session.
	for _, task := range s.botSvc.ActiveMessageTasks(req.BotID, "") {
		if task.TraceID != req.TraceID {
			continue
		}
		if req.SessionID != "" && task.SessionID != "" && req.SessionID != task.SessionID {
			Fail(c, errs.BadRequest("chat turn belongs to another session"))
			return
		}
		if task.SessionID != "" {
			req.SessionID = task.SessionID
		}
		break
	}

	accepted := s.botSvc.AppendToMessage(req.BotID, req.TraceID, req.Text)
	if accepted {
		if err := s.chatHistory.SaveMessage(req.BotID, userID, "user", req.Text, req.TraceID, req.SessionID); err != nil {
			s.logger.Warnw("failed to save appended user message", "err", err)
			// The task already accepted this supplement; reporting rejection
			// here would cause the client to execute it twice as a new request.
			OK(c, map[string]any{"accepted": true, "persistenceError": "补充内容已投递，但未能保存到历史记录"})
			return
		}
	}
	OK(c, map[string]any{"accepted": accepted})
}

// handleChatActiveTasks 返回指定 bot 当前仍在后台执行的消息任务。
// GET /api/chat/active?botId=xxx&sessionId=yyy
//
// sessionId 可选：传入时只返回该会话的任务，避免前端把 A 会话的 SSE 续到 B 会话。
// 响应同时带 tasks（含 sessionId）与 traceIds（兼容旧前端）。
func (s *Server) handleChatActiveTasks(c *gin.Context) {
	botID := c.Query("botId")
	if botID == "" {
		Fail(c, errs.BadRequest("botId required"))
		return
	}
	sessionID := c.Query("sessionId")
	tasks := s.botSvc.ActiveMessageTasks(botID, sessionID)
	traceIDs := make([]string, 0, len(tasks))
	for _, t := range tasks {
		traceIDs = append(traceIDs, t.TraceID)
	}
	// unattributed：解析不出会话归属、画不进会话列表的系统回合（heartbeat / outreach /
	// cron）。前端据此显示「系统任务运行中」并给出 bot 级中止入口，避免「看得见的都停了、
	// 看不见的还在跑」。只在未指定 sessionId（bot 级查询）时有意义。
	resp := map[string]any{"tasks": tasks, "traceIds": traceIDs}
	if sessionID == "" {
		resp["unattributed"] = s.botSvc.UnattributedRunning(botID)
	}
	OK(c, resp)
}

// handleChatResume 按 traceID 重连续流（SSE）。
// GET /api/chat/resume?botId=xxx&traceId=zzz
//
// 使用 EventBus.SubscribeWithReplay(traceID, 0) 回放历史事件并实时转发，
// 使断连后重连的前端能继续看到「仍在后台运行」的任务的真实进度，并据此手动终止。
func (s *Server) handleChatResume(c *gin.Context) {
	botID, traceID := c.Query("botId"), c.Query("traceId")
	if botID == "" || traceID == "" {
		Fail(c, errs.BadRequest("botId and traceId required"))
		return
	}
	bus, ok := s.botSvc.EventBus().(*outbound.MemoryEventBus)
	if !ok || bus == nil {
		Fail(c, errs.Internal("event bus unavailable"))
		return
	}
	var sinceSeq uint64
	cursor := c.Query("sinceSeq")
	if cursor == "" {
		cursor = c.GetHeader("Last-Event-ID")
	}
	if cursor != "" {
		var err error
		sinceSeq, err = strconv.ParseUint(cursor, 10, 64)
		if err != nil {
			Fail(c, errs.BadRequest("invalid event cursor"))
			return
		}
	}
	// Subscribe before reading the DB: events committed while the snapshot
	// query runs are buffered. Its persisted cursor removes any overlap.
	sub := bus.SubscribeWithReplay(traceID, 0)
	defer bus.Unsubscribe(sub)
	snapshot, err := s.chatHistory.LoadAssistantByTrace(botID, traceID)
	if err != nil {
		Fail(c, errs.Wrap(err, "failed to load reply snapshot"))
		return
	}
	var sessionID string
	known := snapshot != nil
	if snapshot != nil {
		sessionID = snapshot.SessionID
	} else {
		var message dao.ChatMessage
		result := s.chatHistory.db.Select("session_id").Where("bot_id = ? AND trace_id = ?", botID, traceID).Limit(1).Find(&message)
		if result.Error != nil {
			Fail(c, errs.Wrap(result.Error, "failed to locate chat turn"))
			return
		}
		known = result.RowsAffected > 0
		sessionID = message.SessionID
		if !known {
			for _, task := range s.botSvc.ActiveMessageTasks(botID, "") {
				if task.TraceID == traceID {
					known, sessionID = true, task.SessionID
					break
				}
			}
		}
	}
	if !known {
		Fail(c, errs.NotFound("chat turn not found for this bot"))
		return
	}
	if expected := c.Query("sessionId"); expected != "" && expected != sessionID {
		Fail(c, errs.BadRequest("chat turn belongs to another session"))
		return
	}
	if snapshot == nil || snapshot.Streaming {
		_, managed := managedChatTurns.Load(messageCancelKey(botID, traceID))
		s.botSvc.mu.RLock()
		_, running := s.botSvc.messageStarts[messageCancelKey(botID, traceID)]
		s.botSvc.mu.RUnlock()
		if !managed && !running && len(sub.C()) == 0 {
			// An old user-only turn or abandoned streaming snapshot cannot
			// produce more events. Do not leave its browser waiting forever.
			if snapshot == nil {
				Fail(c, errs.NotFound("chat turn has ended without a saved reply"))
				return
			}
			state := chatStreamState{text: snapshot.Content, seq: snapshot.StreamSeq}
			_ = json.Unmarshal([]byte(snapshot.ToolCalls), &state.tools)
			_ = json.Unmarshal([]byte(snapshot.PartsJSON), &state.parts)
			state.finish("cancelled", "任务已中断，已恢复最后保存的内容")
			tools, _ := json.Marshal(state.tools)
			parts, _ := json.Marshal(state.parts)
			if err := s.chatHistory.FinalizeAssistantSnapshot(botID, snapshot.UserID, state.text, traceID,
				string(tools), string(parts), sessionID, snapshot.StreamSeq,
				"cancelled", "任务已中断，已恢复最后保存的内容"); err != nil {
				Fail(c, errs.Wrap(err, "failed to finalize interrupted reply"))
				return
			}
			snapshot, err = s.chatHistory.LoadAssistantByTrace(botID, traceID)
			if err != nil {
				Fail(c, errs.Wrap(err, "failed to reload interrupted reply"))
				return
			}
		}
	}
	s.serveChatEvents(c, bus, sub, botID, sessionID, traceID, snapshot, sinceSeq)
}

// translateEventToSSE 将 EventBus 内部事件翻译为前端 SSE 事件（type + data）。
// 供 handleChatSend（正常流式）与 handleChatResume（重连续流）共用，避免两边
// 各自拼装 SSE 负载导致漂移。
func translateEventToSSE(event outbound.Event) (string, map[string]any) {
	switch event.Type {
	case outbound.EventLLMTextDelta:
		return sseTextDelta, map[string]any{"text": event.Data["text"]}
	case outbound.EventLLMToolCall:
		id, _ := event.Data["toolCallId"].(string)
		if id == "" {
			id = fmt.Sprintf("tool-%d", event.Seq)
		}
		return sseToolCall, map[string]any{
			"toolCallId": id,
			"tool":       event.Data["tool"], "input": event.Data["input"],
			"invocationId": event.Data["invocationId"],
		}
	case outbound.EventLLMToolProgress:
		stream := "stdout"
		chunk := ""
		if payload, _ := event.Data["payload"].(map[string]any); payload != nil {
			if v, ok := payload["stream"].(string); ok && v != "" {
				stream = v
			}
			if v, ok := payload["chunk"].(string); ok {
				chunk = v
			}
		}
		return sseToolProgress, map[string]any{
			"toolCallId":   event.Data["toolCallId"],
			"tool":         event.Data["tool"],
			"invocationId": event.Data["invocationId"],
			"stream":       stream,
			"chunk":        chunk,
			"payload":      event.Data["payload"],
		}
	case outbound.EventLLMToolResult:
		payload := map[string]any{
			"toolCallId":   event.Data["toolCallId"],
			"tool":         event.Data["tool"],
			"invocationId": event.Data["invocationId"],
			"output":       event.Data["output"],
		}
		if errMsg, ok := event.Data["error"]; ok {
			payload["error"] = errMsg
		}
		if status := extractToolStatus(event.Data["output"]); status != "" {
			payload["status"] = status
		}
		if status, _ := event.Data["status"].(string); status != "" {
			payload["status"] = status
		}
		return sseToolResult, payload
	}
	return "", nil
}

// handleResetTokenBudget 重置所有 channel 的 token 预算追踪。
// POST /api/chat/token-budget/reset
//
// 当某 channel 累计 token 超过硬限制后，Pipeline 会在每次请求前直接中止，
// 若不重置则该 channel 将永久拒绝新消息（bot 表现为"已读不回"）。
// 空闲 1 小时会自动清零，但本接口可立即恢复。需 admin 权限。
func (s *Server) handleResetTokenBudget(c *gin.Context) {
	s.botSvc.ResetTokenBudgets()
	OKMsg(c, "token budgets reset", nil)
}

// handleChatSend SSE 流式聊天。
// POST /api/chat/send
//
// 请求体: { "botId": "xxx", "text": "hello", "attachments": [{name,type,size,dataUrl}] }
// 响应: text/event-stream
//
// @Summary      发送消息（SSE）
// @Description  向指定 Bot 发送消息，通过 SSE 流式返回回复
// @Tags         聊天
// @Accept       json
// @Produce      text/event-stream
// @Param        body  body      ChatReq  true  "聊天请求"
// @Success      200   {string}  string          "SSE 事件流"
// @Failure      400   {object}  Response
// @Failure      401   {object}  Response
// @Failure      404   {object}  Response
// @Security     CookieAuth
// @Router       /api/chat/send [post]
func (s *Server) handleChatSend(c *gin.Context) {
	var req ChatReq
	if err := c.ShouldBindJSON(&req); err != nil {
		Fail(c, errs.BadRequest("invalid request body: "+err.Error()))
		return
	}
	user := currentUser(c)
	if user == nil {
		Fail(c, errs.Unauthorized("not logged in"))
		return
	}
	webCh, ok := s.botSvc.GetWebChannel(req.BotID)
	if !ok {
		Fail(c, errs.NotFound("bot is not running or not available for chat"))
		return
	}
	userID := strconv.FormatUint(uint64(user.ID), 10)
	if req.SessionID == "" {
		req.SessionID = s.chatHistory.resolveWritableSession(req.BotID, userID, "")
		if req.SessionID == "" {
			Fail(c, errs.Internal("failed to resolve chat session"))
			return
		}
	} else {
		var count int64
		if err := s.chatHistory.db.Model(&dao.ChatSession{}).Where("id = ? AND bot_id = ?", req.SessionID, req.BotID).Count(&count).Error; err != nil {
			Fail(c, errs.Wrap(err, "failed to resolve chat session"))
			return
		} else if count == 0 {
			Fail(c, errs.NotFound("chat session not found for this bot"))
			return
		}
	}
	if cmd := command.Parse(req.Text); cmd != nil {
		s.handleSlashCommand(c, cmd, &req)
		return
	}
	bus, ok := s.botSvc.EventBus().(*outbound.MemoryEventBus)
	if !ok || bus == nil {
		Fail(c, errs.Internal("event bus unavailable"))
		return
	}
	traceID := idgen.New("web")
	contextLimit := s.store.GetInt(config.KeyChatContextLimit, 20)
	history, err := s.chatHistory.LoadContext(req.BotID, userID, contextLimit, req.SessionID)
	if err != nil {
		Fail(c, errs.Wrap(err, "failed to load chat history"))
		return
	}
	history = s.chatHistory.ApplyContextCheckpoint(traceID, req.BotID, req.SessionID, history)
	// Commit the user row before work starts, preserving both chronological
	// order and the next request's context even when the first reply is fast.
	if err := s.chatHistory.SaveMessage(req.BotID, userID, "user", req.Text, traceID, req.SessionID); err != nil {
		Fail(c, errs.Wrap(err, "failed to save chat message"))
		return
	}
	s.botSvc.RememberMessageSession(req.BotID, traceID, req.SessionID)
	turn := s.botSvc.prepareChatTurn(webCh, req.BotID, userID, req.SessionID, traceID)
	if turn == nil {
		Fail(c, errs.Internal("chat persistence unavailable"))
		return
	}
	sub := bus.SubscribeWithReplay(traceID, 0)
	defer bus.Unsubscribe(sub)
	extraMeta := map[string]any{metaKeyChatSessionID: req.SessionID}
	if len(history) > 0 {
		extraMeta["chat_history"] = history
	}
	if len(req.Attachments) > 0 {
		extraMeta["attachments"] = req.Attachments
	}
	injectText := req.Text
	if workflow.DetectGoalModeIntent(req.Text) {
		injectText = workflow.GoalModeDirective(req.Text)
		extraMeta["goal_mode_intent"] = true
	}
	if err := webCh.Inject(c.Request.Context(), traceID, userID, injectText, extraMeta); err != nil {
		turn.fail <- err
		s.botSvc.UnregisterMessageCancel(req.BotID, traceID)
		Fail(c, errs.Wrap(err, "failed to send message to bot"))
		return
	}
	s.serveChatEvents(c, bus, sub, req.BotID, req.SessionID, traceID, nil, 0)
}

// writeSSE 写入一个 SSE 事件。
func writeSSE(w io.Writer, eventType string, data any) {
	jsonData, err := json.Marshal(data)
	if err != nil {
		return
	}
	_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", eventType, jsonData)
}

// ---- 斜杠命令处理 ----

// handleSlashCommand 处理 Web 聊天中的斜杠命令（/clear、/help 等）。
// 命令在 pipeline 之前拦截，通过 SSE 返回结果，不经过 LLM。
func (s *Server) handleSlashCommand(c *gin.Context, cmd *command.ParsedCommand, req *ChatReq) {
	user := currentUser(c)
	if user == nil {
		Fail(c, errs.Unauthorized("not logged in"))
		return
	}

	switch cmd.Name {
	case "clear":
		s.cmdClear(c, req.BotID, req.SessionID)
	case "compact":
		s.cmdCompact(c, req.BotID, req.SessionID, fmt.Sprintf("%d", user.ID), parseKeep(cmd.Args))
	case "help":
		s.cmdHelp(c)
	default:
		// 未知命令：走正常 LLM 流程（不放行——避免 /xxx 被 bot 当普通文本回复）
		s.cmdUnknown(c, cmd.Name)
	}
}

// cmdClear 清空当前会话的聊天历史（仅删消息，保留会话记录），
// 并解除该会话关联的工作流（避免刷新后卡片重新出现）。
func (s *Server) cmdClear(c *gin.Context, botID, sessionID string) {
	if sessionID == "" {
		s.replyCommandError(c, "⚠️ 没有活跃会话，无法执行 /clear。请先在一个会话中发送消息。")
		return
	}

	msgCount, err := s.chatHistory.ClearSessionMessages(sessionID)
	if err != nil {
		s.logger.Warnw("cmd clear failed", "session_id", sessionID, "err", err)
		s.replyCommandError(c, "❌ 清空会话失败，请稍后重试。")
		return
	}

	// 解除该会话下工作流的关联（置空 SessionID）。失败不影响清消息主流程。
	wfCount := 0
	if mgr, merr := s.workflowSvc.Manager(); merr == nil {
		if n, derr := mgr.DissociateSessionWorkflows(botID, sessionID); derr == nil {
			wfCount = n
		} else {
			s.logger.Warnw("cmd clear: dissociate workflows failed", "session_id", sessionID, "err", derr)
		}
	}

	reply := fmt.Sprintf("✅ 已清空会话上下文（移除 %d 条消息，解除 %d 个工作流关联）。", msgCount, wfCount)
	s.replyCommandSSE(c, reply, map[string]any{"command": "clear", "cleared": msgCount})
}

// cmdHelp 列出所有可用的斜杠命令。
func (s *Server) cmdHelp(c *gin.Context) {
	var sb strings.Builder
	sb.WriteString("📋 **可用命令列表**\n\n")
	sb.WriteString("- `/clear` — 清空当前会话上下文\n")
	sb.WriteString("- `/help` — 显示本帮助信息\n\n")
	sb.WriteString("_在聊天输入框输入命令即可执行_")

	s.replyCommandSSE(c, sb.String(), map[string]any{"command": "help"})
}

// parseKeep 解析 /compact 后的保留条数参数，非法或缺失时回退默认值 3。
func parseKeep(args string) int {
	if args == "" {
		return 3
	}
	if n, err := strconv.Atoi(strings.TrimSpace(args)); err == nil && n > 0 {
		return n
	}
	return 3
}

// cmdCompact 执行 /compact：LLM 摘要旧消息替代静默截断，并顺带触发该会话对应
// scope 的记忆压缩（带 pre-LLM 预压缩的生产路径）。
func (s *Server) cmdCompact(c *gin.Context, botID, sessionID, userID string, keep int) {
	if sessionID == "" {
		s.replyCommandError(c, "⚠️ 没有活跃会话，无法压缩。请先在一个会话中发送消息。")
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), compactLLMTimeout)
	defer cancel()

	// ① 记忆压缩：当前 web 会话对应 scope = ChannelScope("web:"+userID)
	//    异步执行，避免阻塞聊天响应（压缩可能耗时数分钟，单批上限见
	//    sqlite_compactor.compactLLMTimeout）。结果记入日志，不阻塞 SSE 回复。
	if memRepo, ok := s.botSvc.GetMemoryRepo(botID); ok {
		scope := memory.ChannelScope("web:" + userID)
		go func() {
			cctx, ccancel := context.WithTimeout(context.Background(), 30*time.Minute)
			defer ccancel()
			if err := memRepo.CompactScope(cctx, scope); err != nil {
				s.logger.Warnw("cmd compact: memory compaction failed", "bot_id", botID, "scope", scope, "err", err)
			} else {
				s.logger.Infow("cmd compact: memory compaction done", "bot_id", botID, "scope", scope)
			}
		}()
	}

	// ② 聊天历史 LLM 摘要（替代截断）— 同步单次调用，结果随 SSE 回复返回
	summary, kept, removed := s.compactChatHistory(ctx, botID, sessionID, userID, keep)

	reply := fmt.Sprintf(
		"✅ 已压缩会话上下文：保留最近 %d 条，摘要 %d 条旧消息；记忆压缩已后台触发。",
		kept, removed,
	)
	if summary != "" {
		reply += "\n\n📝 已生成历史摘要并保留在上下文开头。"
	}
	s.replyCommandSSE(c, reply, map[string]any{
		"command":         "compact",
		"kept":            kept,
		"removed":         removed,
		"memoryCompacted": true,
	})
}

// compactChatHistory 加载会话历史，对最旧的 (总数-keep) 条用 bot 的 LLM 生成结构化
// 摘要，删除旧段、把摘要插入到「最近保留段」之前，实现「摘要而非截断」。返回摘要
// 文本、保留条数、删除条数。
func (s *Server) compactChatHistory(ctx context.Context, botID, sessionID, userID string, keep int) (string, int, int) {
	history, err := s.chatHistory.LoadContextBySession(botID, sessionID, 1000)
	if err != nil {
		s.logger.Warnw("compact: load history failed", "err", err)
		return "", 0, 0
	}
	if len(history) <= keep {
		return "", len(history), 0
	}

	old := history[:len(history)-keep]
	recent := history[len(history)-keep:]

	// 组装待摘要的旧消息
	var head []llm.Message
	for _, m := range old {
		role := llm.MessageRoleUser
		if m.Role == dao.ChatRoleAssistant {
			role = llm.MessageRoleAssistant
		}
		head = append(head, llm.Message{
			Role:    role,
			Content: []llm.MessagePart{llm.TextPart{Text: m.Content}},
		})
	}

	summaryText := ""
	if bundle, ok := s.botSvc.GetLLMBundle(botID); ok && len(head) > 0 {
		// 压缩预算由配置模块（compaction.*）驱动，集中可配、前端可改；
		// 未配置时回退 llm.DefaultCompactionConfig() 的内部兜底默认值。
		compactor := llm.NewCompactor(*compactionConfigFromConfig(
			config.NewBuilder(s.store, s.logger).GetCompactionConfig(), bundle.MainDef.ContextLength, bundle.MainDef.MaxTokens))
		// reasoning_effort / 输出封顶按内部调用策略（summarize_head，默认 low）。
		botEffort := ""
		if def, derr := s.botSvc.GetDefinition(botID); derr == nil && def != nil {
			botEffort = def.ReasoningEffort
		}
		compactor.SetInternalPolicy(newBotInternalPolicy(s.store, s.logger, botID, botEffort, bundle))
		if sum, serr := compactor.SummarizeHead(ctx, bundle.Main, bundle.MainDef.Model, head, bundle.MainDef.MaxTokens); serr == nil {
			summaryText = sum
		} else {
			s.logger.Warnw("compact: summarize failed, fall back to truncation", "err", serr)
		}
	}

	// 删除旧段（按 ID 精确删除，保留最近 keep 条）
	ids := make([]uint64, 0, len(old))
	for _, m := range old {
		ids = append(ids, m.ID)
	}
	removed := 0
	if n, derr := s.chatHistory.DeleteMessages(botID, sessionID, ids); derr != nil {
		s.logger.Warnw("compact: delete old messages failed", "err", derr)
	} else {
		removed = int(n)
	}

	// 把摘要插入到最近保留段之前，保持时间顺序
	if summaryText != "" {
		_ = s.chatHistory.SaveMessageAt(
			botID, userID, dao.ChatRoleAssistant,
			"【历史摘要】\n"+summaryText,
			idgen.New("compact"), sessionID,
			recent[0].CreatedAt.Add(-time.Second),
		)
	}

	return summaryText, len(recent), removed
}

// cmdUnknown 未知命令提示。
func (s *Server) cmdUnknown(c *gin.Context, name string) {
	s.replyCommandSSE(c, fmt.Sprintf("ℹ️ 未知命令 `/%s`。输入 `/help` 查看可用命令。", name),
		map[string]any{"command": "unknown", "name": name})
}

// replyCommandSSE 通过 SSE 返回命令执行结果（与正常聊天的 SSE 格式一致）。
// 前端收到 command 类型响应后可做特殊处理（如 /clear 后清空本地消息列表）。
func (s *Server) replyCommandSSE(c *gin.Context, text string, extra map[string]any) {
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")

	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		Fail(c, errs.Internal("streaming not supported"))
		return
	}

	traceID := idgen.New("web")

	// start 事件
	writeSSE(c.Writer, sseStart, map[string]any{"traceId": traceID})
	flusher.Flush()

	// 文本内容（一次性返回完整命令回复）
	cleanText := stages.StripReplyControlBlock(text)
	writeSSE(c.Writer, sseTextDelta, map[string]any{"text": cleanText})
	flusher.Flush()

	// done 事件：携带 command 标识和额外元数据
	donePayload := map[string]any{
		"text":    cleanText,
		"command": true,
	}
	for k, v := range extra {
		donePayload[k] = v
	}
	writeSSE(c.Writer, sseDone, donePayload)
	flusher.Flush()

	s.logger.Infow("slash command executed",
		"command", extra["command"],
		"trace_id", traceID,
		"user_id", fmt.Sprintf("%d", currentUser(c).ID))
}

// replyCommandSSE 错误响应（无 start 事件，前端按 error 处理）。
func (s *Server) replyCommandError(c *gin.Context, msg string) {
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")

	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		Fail(c, errs.Internal("streaming not supported"))
		return
	}

	writeSSE(c.Writer, sseError, map[string]any{"message": msg})
	flusher.Flush()
}

// reconcileTextParts 让有序 parts 里的文本片段与最终出站内容 canonical 对齐，
// 保证刷新 / 回放（前端优先按 parts_json 渲染）看到的正文与其他渠道发送的一致。
//
//   - 各文本片段（去首尾空白后）能按顺序在 canonical 里依次对上、且覆盖全部内容：
//     保留原有的文本 / 工具交错顺序，仅去掉空白片段；
//   - 否则（流式预览与最终内容有出入，例如 <public> 之前的裸文本、被剥离的内部指标）：
//     去掉所有文本片段，保留工具片段原顺序，在末尾追加一个 canonical 文本片段。
func reconcileTextParts(parts []map[string]any, canonical string) []map[string]any {
	if canonical == "" {
		return parts
	}
	remaining := canonical
	mapped := make([]map[string]any, 0, len(parts))
	ok := true
	for _, p := range parts {
		if p["type"] != "text" {
			mapped = append(mapped, p)
			continue
		}
		content, _ := p["content"].(string)
		seg := strings.TrimSpace(content)
		if seg == "" {
			continue
		}
		rest := strings.TrimLeftFunc(remaining, unicode.IsSpace)
		if !strings.HasPrefix(rest, seg) {
			ok = false
			break
		}
		remaining = rest[len(seg):]
		mapped = append(mapped, map[string]any{"type": "text", "content": seg})
	}
	if ok && strings.TrimSpace(remaining) == "" {
		return mapped
	}
	out := make([]map[string]any, 0, len(parts)+1)
	for _, p := range parts {
		if p["type"] != "text" {
			out = append(out, p)
		}
	}
	return append(out, map[string]any{"type": "text", "content": canonical})
}
