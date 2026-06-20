package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/kasuganosora/thinkbot/agent/core"
	"github.com/kasuganosora/thinkbot/config"
	"github.com/kasuganosora/thinkbot/util/errs"
	"github.com/kasuganosora/thinkbot/util/idgen"
)

// ============================================================================
// 聊天 Handler — SSE 流式对话 + 可聊 Bot 列表
// ============================================================================

// SSE 事件类型
const (
	sseTextDelta = "text_delta" // LLM 文本增量
	sseDone      = "done"       // 生成完成
	sseError     = "error"      // 错误
	sseStart     = "start"      // 开始处理
)

// handleChatBots 返回当前可聊天的 Bot 列表（状态为 running）。
// GET /api/chat/bots
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

// handleChatSend SSE 流式聊天。
// POST /api/chat/send
//
// 请求体: { "botId": "xxx", "text": "hello" }
// 响应: text/event-stream
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
		if err := s.chatHistory.SaveMessage(req.BotID, userID, "user", req.Text, traceID); err != nil {
			s.logger.Warnw("failed to save user message", "err", err)
		}
	}()

	// 注册回复 channel
	respCh := webCh.RegisterResponse(traceID, 16)
	defer webCh.UnregisterResponse(traceID)

	// 注入消息到 Bot（携带聊天历史作为 LLM 上下文）
	extraMeta := map[string]any{}
	if len(history) > 0 {
		extraMeta["chat_history"] = history
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

	// 设置超时
	timeout := time.NewTimer(120 * time.Second)
	defer timeout.Stop()

	fullText := ""
	botID := req.BotID

	for {
		select {
		case <-c.Request.Context().Done():
			// 客户端断开
			return

		case <-timeout.C:
			writeSSE(c.Writer, sseError, map[string]any{"message": "timeout"})
			flusher.Flush()
			return

		case action, ok := <-respCh:
			if !ok {
				// channel 关闭，结束
				writeSSE(c.Writer, sseDone, map[string]any{"text": fullText})
				flusher.Flush()
				return
			}

			// 处理 Action
			text, _ := action.Payload.(string)
			if text != "" {
				fullText += text
				writeSSE(c.Writer, sseTextDelta, map[string]any{"text": text})
				flusher.Flush()
			}

			// ActionReply 后发送 done 并保存回复
			if action.Type == core.ActionReply || text != "" {
				writeSSE(c.Writer, sseDone, map[string]any{"text": fullText})
				flusher.Flush()

				// 保存 Bot 回复到 DB
				if fullText != "" {
					go func(content string) {
						if err := s.chatHistory.SaveMessage(botID, userID, "assistant", content, traceID); err != nil {
							s.logger.Warnw("failed to save assistant message", "err", err)
						}
					}(fullText)
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
