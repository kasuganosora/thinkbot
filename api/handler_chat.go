package api

import (
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
	history, err := s.chatHistory.LoadContext(req.BotID, userID, contextLimit)
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
		if err := s.chatHistory.SaveMessage(req.BotID, userID, "user", req.Text, traceID); err != nil {
			s.logger.Warnw("failed to save user message", "err", err)
		}
	}()

	// 注册回复 channel（用于接收最终完成信号）
	respCh := webCh.RegisterResponse(traceID, 16)
	defer webCh.UnregisterResponse(traceID)

	// 订阅 EventBus 接收流式文本增量
	var eventCh <-chan outbound.Event
	bus := s.botSvc.EventBus()
	if bus != nil {
		if memBus, ok := bus.(*outbound.MemoryEventBus); ok {
			eventSub := memBus.Subscribe(traceID)
			defer memBus.Unsubscribe(eventSub)
			eventCh = eventSub.C()
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
	if err := webCh.Inject(c.Request.Context(), traceID, userID, req.Text, extraMeta); err != nil {
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

	for {
		select {
		case <-c.Request.Context().Done():
			// 客户端断开
			return

		case <-idleTimer.C:
			writeSSE(c.Writer, sseError, map[string]any{"message": "idle timeout"})
			flusher.Flush()
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
					toolCalls = append(toolCalls, map[string]any{
						"id":     toolCallID,
						"name":   toolName,
						"title":  toolName,
						"status": "running",
						"input":  event.Data["input"],
						"output": map[string]any{"stdout": "", "stderr": "", "exitCode": nil, "truncated": false},
					})
					toolCallIdx[toolCallID] = len(toolCalls) - 1
				}

			case outbound.EventLLMToolProgress:
				toolCallID, _ := event.Data["toolCallId"].(string)
				toolName, _ := event.Data["tool"].(string)
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
					"toolCallId": toolCallID,
					"tool":       toolName,
					"stream":     stream,
					"chunk":      chunk,
					"payload":    payload,
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
				}

			case outbound.EventLLMToolResult:
				toolCallID, _ := event.Data["toolCallId"].(string)
				toolName, _ := event.Data["tool"].(string)
				payload := map[string]any{
					"toolCallId": toolCallID,
					"tool":       toolName,
					"output":     event.Data["output"],
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
				}
			}

		// 完成信号：从 WebChannel 收到最终 Action
		case action, ok := <-respCh:
			resetIdle()
			if !ok {
				// channel 关闭，结束
				writeSSE(c.Writer, sseDone, map[string]any{"text": fullText})
				flusher.Flush()
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

				// 保存 Bot 回复到 DB（含工具调用信息）
				if fullText != "" || len(toolCalls) > 0 {
					toolCallsJSON := ""
					if len(toolCalls) > 0 {
						if b, err := json.Marshal(toolCalls); err == nil {
							toolCallsJSON = string(b)
						}
					}
					go func(content, tcJSON string) {
						defer func() {
							if r := recover(); r != nil {
								s.logger.Errorw("panic saving assistant message", "err", r)
							}
						}()
						if err := s.chatHistory.SaveMessageWithTools(botID, userID, "assistant", content, traceID, tcJSON); err != nil {
							s.logger.Warnw("failed to save assistant message", "err", err)
						}
					}(fullText, toolCallsJSON)
				}
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
