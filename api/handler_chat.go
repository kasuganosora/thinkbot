package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/kasuganosora/thinkbot/agent/core"
	"github.com/kasuganosora/thinkbot/agent/outbound"
	"github.com/kasuganosora/thinkbot/config"
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
	aborted := s.botSvc.AbortMessage(req.BotID, req.TraceID)
	OK(c, map[string]any{"aborted": aborted})
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

	accepted := s.botSvc.AppendToMessage(req.BotID, req.TraceID, req.Text)
	if accepted {
		// 持久化补充的用户消息，保证后续轮次上下文一致（与 /send 保存用户消息一致）。
		go func() {
			defer func() { _ = recover() }()
			if err := s.chatHistory.SaveMessage(req.BotID, userID, "user", req.Text, req.TraceID, req.SessionID); err != nil {
				s.logger.Warnw("failed to save appended user message", "err", err)
			}
		}()
	}
	OK(c, map[string]any{"accepted": accepted})
}

// handleChatActiveTasks 返回指定 bot 当前仍在后台执行的消息 traceID 列表。
// GET /api/chat/active?botId=xxx
//
// 用户断连后后台长任务继续跑，其 cancel 仍注册在 messageCancels 中（直到消息真正完成）。
// 前端重连后据此知道自己可以 resume / abort 哪些任务。
func (s *Server) handleChatActiveTasks(c *gin.Context) {
	botID := c.Query("botId")
	if botID == "" {
		Fail(c, errs.BadRequest("botId required"))
		return
	}
	OK(c, map[string]any{"traceIds": s.botSvc.ActiveMessageTraceIDs(botID)})
}

// handleChatResume 按 traceID 重连续流（SSE）。
// GET /api/chat/resume?botId=xxx&traceId=zzz
//
// 使用 EventBus.SubscribeWithReplay(traceID, 0) 回放历史事件并实时转发，
// 使断连后重连的前端能继续看到「仍在后台运行」的任务的真实进度，并据此手动终止。
func (s *Server) handleChatResume(c *gin.Context) {
	botID := c.Query("botId")
	traceID := c.Query("traceId")
	if botID == "" || traceID == "" {
		Fail(c, errs.BadRequest("botId and traceId required"))
		return
	}
	bus := s.botSvc.EventBus()
	memBus, ok := bus.(*outbound.MemoryEventBus)
	if !ok || memBus == nil {
		Fail(c, errs.Internal("event bus unavailable"))
		return
	}

	sub := memBus.SubscribeWithReplay(traceID, 0)
	defer memBus.Unsubscribe(sub)

	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")
	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		Fail(c, errs.Internal("streaming not supported"))
		return
	}

	writeSSE(c.Writer, sseStart, map[string]any{"traceId": traceID})
	flusher.Flush()

	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()

	for {
		select {
		case <-c.Request.Context().Done():
			return
		case <-heartbeat.C:
			writeSSE(c.Writer, ssePing, map[string]any{"ts": time.Now().Unix()})
			flusher.Flush()
		case event, ok := <-sub.C():
			if !ok {
				return
			}
			// 消息终态：转发 done 并结束重连续流。
			if event.Type == outbound.EventMessageDone || event.Type == outbound.EventMessageError ||
				event.Type == outbound.EventMessageDropped || event.Type == outbound.EventDispatchError {
				writeSSE(c.Writer, sseDone, map[string]any{})
				flusher.Flush()
				return
			}
			evtType, data := translateEventToSSE(event)
			if evtType == "" {
				continue
			}
			writeSSE(c.Writer, evtType, data)
			flusher.Flush()
		}
	}
}

// translateEventToSSE 将 EventBus 内部事件翻译为前端 SSE 事件（type + data）。
// 供 handleChatSend（正常流式）与 handleChatResume（重连续流）共用，避免两边
// 各自拼装 SSE 负载导致漂移。
func translateEventToSSE(event outbound.Event) (string, map[string]any) {
	switch event.Type {
	case outbound.EventLLMTextDelta:
		return sseTextDelta, map[string]any{"text": event.Data["text"]}
	case outbound.EventLLMToolCall:
		return sseToolCall, map[string]any{
			"toolCallId": event.Data["toolCallId"],
			"tool":       event.Data["tool"],
			"input":      event.Data["input"],
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

	// 获取 WebChannel
	webCh, ok := s.botSvc.GetWebChannel(req.BotID)
	if !ok {
		Fail(c, errs.NotFound("bot is not running or not available for chat"))
		return
	}

	user := currentUser(c)
	if user == nil {
		Fail(c, errs.Unauthorized("not logged in"))
		return
	}

	userID := fmt.Sprintf("%d", user.ID)

	// 生成 traceID（使用 crypto/rand，格式 "web-{24 hex}"）
	traceID := idgen.New("web")

	// 先加载历史（不含当前消息），再异步保存用户消息
	// 顺序很重要：如果先保存再加载，当前消息会出现在历史中，
	// 导致 MessageBuilder 重复追加，LLM 上下文中出现两次相同消息
	contextLimit := s.store.GetInt(config.KeyChatContextLimit, 20)
	history, err := s.chatHistory.LoadContext(req.BotID, userID, contextLimit, req.SessionID)
	if err != nil {
		s.logger.Warnw("failed to load chat history", "err", err)
		history = nil
	}

	// 保存用户消息到 DB（异步，不阻塞响应）
	go func() {
		defer func() {
			if r := recover(); r != nil {
				s.logger.Errorw("panic saving user message", "err", r)
			}
		}()
		if err := s.chatHistory.SaveMessage(req.BotID, userID, "user", req.Text, traceID, req.SessionID); err != nil {
			s.logger.Warnw("failed to save user message", "err", err)
		}
	}()

	// 注册回复 channel（用于接收最终完成信号）
	respCh := webCh.RegisterResponse(traceID, 16)
	defer webCh.UnregisterResponse(traceID)

	// 订阅 EventBus 接收流式文本增量
	var eventCh <-chan outbound.Event
	var eventSub *outbound.Subscription
	var memBus *outbound.MemoryEventBus
	bus := s.botSvc.EventBus()
	if bus != nil {
		if mb, ok := bus.(*outbound.MemoryEventBus); ok {
			memBus = mb
			eventSub = mb.Subscribe(traceID)
			eventCh = eventSub.C()
		}
	}
	// unsubscribeEventSub 释放 EventBus 订阅。仅在「连接已结束」或「后台 goroutine 接管」后
	// 调用一次，避免与断连后的后台落库 goroutine 重复退订。
	unsubscribeEventSub := func() {
		if memBus != nil && eventSub != nil {
			memBus.Unsubscribe(eventSub)
		}
	}

	// 注入消息到 Bot（携带聊天历史作为 LLM 上下文）
	extraMeta := map[string]any{}
	if len(history) > 0 {
		extraMeta["chat_history"] = history
	}
	if len(req.Attachments) > 0 {
		extraMeta["attachments"] = req.Attachments
	}

	// 目标模式自动路由：若用户需求表达「反复打磨 / 审查直到没有新问题」等收敛性验收意图，
	// 注入一条强制指令，要求模型必须用 task(goalMode: true) 提交、禁止用 subagent/delegate
	// 内联处理。原始文本仍按 req.Text 落库（见上方 goroutine），此处仅放大注入模型的文本。
	injectText := req.Text
	if workflow.DetectGoalModeIntent(req.Text) {
		injectText = workflow.GoalModeDirective(req.Text)
		extraMeta["goal_mode_intent"] = true
		s.logger.Infow("goal-mode auto-route", "bot_id", req.BotID, "trace_id", traceID, "text", req.Text)
	}
	if err := webCh.Inject(c.Request.Context(), traceID, userID, injectText, extraMeta); err != nil {
		Fail(c, errs.Wrap(err, "failed to send message to bot"))
		return
	}

	// 设置 SSE 响应头
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")

	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		Fail(c, errs.Internal("streaming not supported"))
		return
	}

	// 发送 start 事件
	writeSSE(c.Writer, sseStart, map[string]any{"traceId": traceID})
	flusher.Flush()

	// 设置空闲超时（收到任意流式事件会重置），避免总时长硬切长命令。
	idleTimeout := 120 * time.Second
	idleTimer := time.NewTimer(idleTimeout)
	defer idleTimer.Stop()
	resetIdle := func() {
		if !idleTimer.Stop() {
			select {
			case <-idleTimer.C:
			default:
			}
		}
		idleTimer.Reset(idleTimeout)
	}
	// 心跳，防止中间代理在长静默期断开 SSE。
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()

	fullText := ""
	botID := req.BotID
	// 累积本轮工具调用，用于回复完成后随 assistant 消息一起持久化。
	toolCalls := make([]map[string]any, 0)
	toolCallIdx := make(map[string]int)
	// ── 有序 parts：按 LLM 实际输出顺序记录文本片段和工具调用，
	//    用于前端按时间线交错渲染（而非文本全在前、工具全在后）。
	parts := make([]map[string]any, 0) // 有序: [{type:"text",content},{type:"tool",...}]

	// syncPartTool 将 toolCalls[idx] 的最新状态同步到 parts 数组中对应的 tool part。
	syncPartTool := func(toolCallID string) {
		idx, ok := toolCallIdx[toolCallID]
		if !ok || idx < 0 || idx >= len(toolCalls) {
			return
		}
		src := toolCalls[idx]
		for i := len(parts) - 1; i >= 0; i-- {
			if parts[i]["type"] == "tool" && parts[i]["id"] == toolCallID {
				// 合并 src 的字段到 part（不覆盖 type/id/name 等标识字段）
				p := parts[i]
				for k, v := range src {
					if k != "type" && k != "id" && k != "name" {
						p[k] = v
					}
				}
				break
			}
		}
	}

	// saveAssistant 将本轮 assistant 回复（文本 + 工具调用 + 有序 parts）持久化到 DB。
	// 供「正常完成」与「客户端断开」两条路径共用，确保工具调用终态（success/error/killed）
	// 都能落库，避免重连后卡片停留在 running。
	//
	// 落库走 UpsertAssistantByTrace（以 traceID 为幂等键），因此本函数可被安全地重复调用：
	// 流式过程中的增量落库与收尾落库写的是同一行。
	//
	// streaming=true 表示这是流式中间态；收尾时必须传 false，否则前端会把 running
	// 工具卡片当成「仍在跑」而永久转圈。
	saveAssistant := func(streaming bool) {
		if fullText == "" && len(toolCalls) == 0 {
			return
		}
		toolCallsJSON := ""
		if len(toolCalls) > 0 {
			if b, err := json.Marshal(toolCalls); err == nil {
				toolCallsJSON = string(b)
			}
		}
		partsJSON := ""
		if len(parts) > 0 {
			if b, err := json.Marshal(parts); err == nil {
				partsJSON = string(b)
			}
		}
		go func(content, tcJSON, pJSON string, streaming bool) {
			defer func() {
				if r := recover(); r != nil {
					s.logger.Errorw("panic saving assistant message", "err", r)
				}
			}()
			if err := s.chatHistory.UpsertAssistantByTrace(botID, userID, content, traceID, tcJSON, pJSON, req.SessionID, streaming); err != nil {
				s.logger.Warnw("failed to save assistant message", "err", err)
			}
		}(fullText, toolCallsJSON, partsJSON, streaming)
	}

	// ---- 流式增量落库 ----
	//
	// 目的：用户在 bot 回复过程中刷新页面，也能看到已经产出的内容。
	// 旧行为只在整轮结束时落库一次，中途刷新会丢掉全部流式内容（体验很差）。
	//
	// 为什么要节流：文本增量事件非常密集（每个 token 一次），逐条写库会把 SQLite 打满，
	// 并与同连接的历史查询争锁。这里按最小间隔合并写入；工具调用等关键节点强制立即落库，
	// 因为它们稀疏且信息量大，延迟落库最容易在刷新后丢状态。
	var lastFlush time.Time
	flushStream := func(force bool) {
		if fullText == "" && len(toolCalls) == 0 {
			return
		}
		if !force && time.Since(lastFlush) < streamPersistInterval {
			return
		}
		lastFlush = time.Now()
		saveAssistant(true)
	}

	// accumulate 将一条 EventBus 事件合并进本轮 assistant 回复的累积结构
	//（fullText / toolCalls / parts），供「断连后后台继续落库」与「正常完成落库」共用。
	// 仅更新累积结构，不负责向客户端写 SSE。
	accumulate := func(event outbound.Event) {
		switch event.Type {
		case outbound.EventLLMTextDelta:
			delta, _ := event.Data["text"].(string)
			if delta != "" {
				fullText += delta
				if len(parts) > 0 && parts[len(parts)-1]["type"] == "text" {
					parts[len(parts)-1]["content"] = parts[len(parts)-1]["content"].(string) + delta
				} else {
					parts = append(parts, map[string]any{"type": "text", "content": delta})
				}
			}
		case outbound.EventLLMToolCall:
			toolCallID, _ := event.Data["toolCallId"].(string)
			toolName, _ := event.Data["tool"].(string)
			if toolCallID == "" {
				toolCallID = idgen.New("tool")
			}
			if _, ok := toolCallIdx[toolCallID]; !ok {
				toolCalls = append(toolCalls, map[string]any{
					"id":     toolCallID,
					"name":   toolName,
					"title":  toolName,
					"status": "running",
					"input":  event.Data["input"],
					"output": map[string]any{"stdout": "", "stderr": "", "exitCode": nil, "truncated": false},
				})
				toolCallIdx[toolCallID] = len(toolCalls) - 1
				part := map[string]any{
					"type":   "tool",
					"id":     toolCallID,
					"name":   toolName,
					"title":  toolName,
					"status": "running",
					"input":  event.Data["input"],
				}
				if invID, ok := event.Data["invocationId"].(string); ok && invID != "" {
					part["invocationId"] = invID
				}
				parts = append(parts, part)
			}
		case outbound.EventLLMToolProgress:
			toolCallID, _ := event.Data["toolCallId"].(string)
			payload, _ := event.Data["payload"].(map[string]any)
			stream := "stdout"
			chunk := ""
			if payload != nil {
				if v, ok := payload["stream"].(string); ok && v != "" {
					stream = v
				}
				if v, ok := payload["chunk"].(string); ok {
					chunk = v
				}
			}
			if idx, ok := toolCallIdx[toolCallID]; ok && idx >= 0 && idx < len(toolCalls) {
				out, _ := toolCalls[idx]["output"].(map[string]any)
				if out == nil {
					out = map[string]any{"stdout": "", "stderr": "", "exitCode": nil, "truncated": false}
				}
				if stream == "stderr" {
					prev, _ := out["stderr"].(string)
					out["stderr"] = prev + chunk
				} else {
					prev, _ := out["stdout"].(string)
					out["stdout"] = prev + chunk
				}
				toolCalls[idx]["output"] = out
				if invID, ok := event.Data["invocationId"].(string); ok && invID != "" {
					toolCalls[idx]["invocationId"] = invID
				}
				syncPartTool(toolCallID)
			}
		case outbound.EventLLMToolResult:
			toolCallID, _ := event.Data["toolCallId"].(string)
			if idx, ok := toolCallIdx[toolCallID]; ok && idx >= 0 && idx < len(toolCalls) {
				if _, isErr := event.Data["error"]; isErr {
					toolCalls[idx]["status"] = "error"
				} else {
					toolCalls[idx]["status"] = "success"
				}
				if event.Data["output"] != nil {
					toolCalls[idx]["output"] = event.Data["output"]
				}
				if invID, ok := event.Data["invocationId"].(string); ok && invID != "" {
					toolCalls[idx]["invocationId"] = invID
				}
				syncPartTool(toolCallID)
			}
		}
	}

	// drainAndSaveInBackground 在客户端断开后由后台 goroutine 继续消费 EventBus 事件，
	// 直到消息终态（done/error/dropped/dispatch-error）再把最终 assistant 回复落库。
	// 这样断连不会腰斩后台长任务，也不会丢失最终结果——重连后经前端回放即可看到真实进度。
	drainAndSaveInBackground := func() {
		if eventSub == nil {
			return
		}
		bgCtx, bgCancel := context.WithTimeout(context.Background(), 20*time.Minute)
		defer bgCancel()
		for {
			select {
			case <-bgCtx.Done():
				saveAssistant(false)
				unsubscribeEventSub()
				return
			case event, ok := <-eventSub.C():
				if !ok {
					saveAssistant(false)
					unsubscribeEventSub()
					return
				}
				accumulate(event)
				switch event.Type {
				case outbound.EventMessageDone, outbound.EventMessageError,
					outbound.EventMessageDropped, outbound.EventDispatchError:
					saveAssistant(false)
					unsubscribeEventSub()
					return
				}
			}
		}
	}

	for {
		select {
		case <-c.Request.Context().Done():
			// 客户端断开：不再取消后台执行链路（msgCtx 仅由 stop 按钮控制），
			// 也不再写入「killed」残骸快照。改为把 EventBus 订阅移交给后台 goroutine，
			// 由其继续消费事件直到消息终态，再把最终 assistant 回复落库。
			// 这样断连既不会腰斩长任务，也不会丢失最终结果；用户重连后可由前端
			// 经回放订阅看到真实进度并手动终止（见 resume 端点）。
			if eventSub != nil {
				go drainAndSaveInBackground()
			}
			return

		case <-idleTimer.C:
			writeSSE(c.Writer, sseError, map[string]any{"message": "idle timeout"})
			flusher.Flush()
			unsubscribeEventSub()
			return

		case <-heartbeat.C:
			writeSSE(c.Writer, ssePing, map[string]any{"ts": time.Now().Unix()})
			flusher.Flush()

		// 流式增量：从 EventBus 接收 LLM 文本/工具事件
		case event, ok := <-eventCh:
			if !ok {
				continue
			}
			resetIdle()
			switch event.Type {
			case outbound.EventLLMTextDelta:
				delta, _ := event.Data["text"].(string)
				if delta != "" {
					fullText += delta
					writeSSE(c.Writer, sseTextDelta, map[string]any{"text": delta})
					flusher.Flush()
					// 有序 parts：合并到最后一个 text part 或新建
					if len(parts) > 0 && parts[len(parts)-1]["type"] == "text" {
						parts[len(parts)-1]["content"] = parts[len(parts)-1]["content"].(string) + delta
					} else {
						parts = append(parts, map[string]any{"type": "text", "content": delta})
					}
					// 节流落库：让中途刷新页面的用户能看到已产出的文本。
					flushStream(false)
				}

			case outbound.EventLLMToolCall:
				toolCallID, _ := event.Data["toolCallId"].(string)
				toolName, _ := event.Data["tool"].(string)
				if toolCallID == "" {
					toolCallID = idgen.New("tool")
				}
				writeSSE(c.Writer, sseToolCall, map[string]any{
					"toolCallId": toolCallID,
					"tool":       toolName,
					"input":      event.Data["input"],
				})
				flusher.Flush()
				if _, ok := toolCallIdx[toolCallID]; !ok {
					tc := map[string]any{
						"id":     toolCallID,
						"name":   toolName,
						"title":  toolName,
						"status": "running",
						"input":  event.Data["input"],
						"output": map[string]any{"stdout": "", "stderr": "", "exitCode": nil, "truncated": false},
					}
					toolCalls = append(toolCalls, tc)
					toolCallIdx[toolCallID] = len(toolCalls) - 1
					// 有序 parts：追加工具 part（保持调用顺序）
					part := map[string]any{
						"type":   "tool",
						"id":     toolCallID,
						"name":   toolName,
						"title":  toolName,
						"status": "running",
						"input":  event.Data["input"],
					}
					if invID, ok := event.Data["invocationId"].(string); ok && invID != "" {
						part["invocationId"] = invID
					}
					parts = append(parts, part)
				}
				// 工具调用是稀疏且高信息量的节点，强制立即落库：
				// 若等节流窗口，刷新后最容易丢失「正在调用哪个工具」这类关键状态。
				flushStream(true)

			case outbound.EventLLMToolProgress:
				toolCallID, _ := event.Data["toolCallId"].(string)
				toolName, _ := event.Data["tool"].(string)
				invocationID, _ := event.Data["invocationId"].(string)
				payload, _ := event.Data["payload"].(map[string]any)
				stream := "stdout"
				chunk := ""
				if payload != nil {
					if v, ok := payload["stream"].(string); ok && v != "" {
						stream = v
					}
					if v, ok := payload["chunk"].(string); ok {
						chunk = v
					}
				}
				writeSSE(c.Writer, sseToolProgress, map[string]any{
					"toolCallId":   toolCallID,
					"tool":         toolName,
					"invocationId": invocationID,
					"stream":       stream,
					"chunk":        chunk,
					"payload":      payload,
				})
				flusher.Flush()

				if idx, ok := toolCallIdx[toolCallID]; ok && idx >= 0 && idx < len(toolCalls) {
					out, _ := toolCalls[idx]["output"].(map[string]any)
					if out == nil {
						out = map[string]any{"stdout": "", "stderr": "", "exitCode": nil, "truncated": false}
					}
					if stream == "stderr" {
						prev, _ := out["stderr"].(string)
						out["stderr"] = prev + chunk
					} else {
						prev, _ := out["stdout"].(string)
						out["stdout"] = prev + chunk
					}
					toolCalls[idx]["output"] = out
					if invocationID != "" {
						toolCalls[idx]["invocationId"] = invocationID
					}
					syncPartTool(toolCallID)
				}

			case outbound.EventLLMToolResult:
				toolCallID, _ := event.Data["toolCallId"].(string)
				toolName, _ := event.Data["tool"].(string)
				invocationID, _ := event.Data["invocationId"].(string)
				payload := map[string]any{
					"toolCallId":   toolCallID,
					"tool":         toolName,
					"invocationId": invocationID,
					"output":       event.Data["output"],
				}
				if errMsg, ok := event.Data["error"]; ok {
					payload["error"] = errMsg
				}
				writeSSE(c.Writer, sseToolResult, payload)
				flusher.Flush()

				if idx, ok := toolCallIdx[toolCallID]; ok && idx >= 0 && idx < len(toolCalls) {
					if _, isErr := event.Data["error"]; isErr {
						toolCalls[idx]["status"] = "error"
					} else {
						toolCalls[idx]["status"] = "success"
					}
					if event.Data["output"] != nil {
						toolCalls[idx]["output"] = event.Data["output"]
					}
					if invocationID != "" {
						toolCalls[idx]["invocationId"] = invocationID
					}
					syncPartTool(toolCallID)
				}
				// 工具终态（success/error/killed）必须立即落库：
				// 否则刷新后卡片会停留在 running，用户以为还在跑。
				flushStream(true)
			}

		// 完成信号：从 WebChannel 收到最终 Action
		case action, ok := <-respCh:
			resetIdle()
			if !ok {
				// channel 关闭，结束
				writeSSE(c.Writer, sseDone, map[string]any{"text": fullText})
				flusher.Flush()
				unsubscribeEventSub()
				return
			}

			// ActionReply 表示 Bot 回复完成
			// 文本已通过 EventBus 流式推送，这里只需发送 done
			if action.Type == core.ActionReply {
				// 如果 fullText 为空（EventBus 不可用），用 Action 的 payload
				if fullText == "" {
					text, _ := action.Payload.(string)
					if text != "" {
						fullText = text
						writeSSE(c.Writer, sseTextDelta, map[string]any{"text": text})
						flusher.Flush()
					}
				}
				donePayload := map[string]any{"text": fullText}
				if len(toolCalls) > 0 {
					donePayload["toolCalls"] = toolCalls
				}
				writeSSE(c.Writer, sseDone, donePayload)
				flusher.Flush()

				// 保存 Bot 回复到 DB（含工具调用信息 + 有序 parts）
				saveAssistant(false)
				unsubscribeEventSub()
				return
			}
		}
	}
}

// writeSSE 写入一个 SSE 事件。
func writeSSE(w io.Writer, eventType string, data any) {
	jsonData, err := json.Marshal(data)
	if err != nil {
		return
	}
	_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", eventType, jsonData)
}
