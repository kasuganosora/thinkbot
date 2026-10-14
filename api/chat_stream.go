package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/kasuganosora/thinkbot/agent/core"
	"github.com/kasuganosora/thinkbot/agent/outbound"
	"github.com/kasuganosora/thinkbot/agent/stages"
	"github.com/kasuganosora/thinkbot/dao"
)

// These events are emitted only after the complete assistant snapshot's final
// persistence attempt. A failed write carries persistenceError; pipeline
// completion alone does not guarantee durable history.
const (
	eventChatDone  outbound.EventType = "chat.done"
	eventChatError outbound.EventType = "chat.error"
)

var managedChatTurns sync.Map // botID + traceID -> *chatTurn

type chatStreamState struct {
	text         string
	canonical    string
	hasCanonical bool
	tools        []map[string]any
	parts        []map[string]any
	seq          uint64
}

func (state *chatStreamState) tool(id string) map[string]any {
	for _, tool := range state.tools {
		if tool["id"] == id {
			return tool
		}
	}
	return nil
}

func (state *chatStreamState) syncTool(tool map[string]any) {
	for _, part := range state.parts {
		if part["type"] == "tool" && part["id"] == tool["id"] {
			for key, value := range tool {
				part[key] = value
			}
			return
		}
	}
}

func (state *chatStreamState) apply(event outbound.Event) bool {
	if event.Seq > 0 && event.Seq <= state.seq {
		return false
	}
	state.seq = event.Seq
	data := event.Data
	id, _ := data["toolCallId"].(string)
	switch event.Type {
	case outbound.EventLLMTextDelta:
		delta, _ := data["text"].(string)
		state.text += delta
		if delta != "" {
			if n := len(state.parts); n > 0 && state.parts[n-1]["type"] == "text" {
				text, _ := state.parts[n-1]["content"].(string)
				state.parts[n-1]["content"] = text + delta
			} else {
				state.parts = append(state.parts, map[string]any{"type": "text", "content": delta})
			}
		}
	case outbound.EventLLMToolCall:
		if id == "" {
			id = fmt.Sprintf("tool-%d", event.Seq)
		}
		name, _ := data["tool"].(string)
		if idx := findPhantomToolCall(state.tools, name, data["input"]); idx >= 0 {
			markToolCallSuperseded(state.tools[idx])
			state.syncTool(state.tools[idx])
		}
		tool := state.tool(id)
		if tool == nil {
			tool = map[string]any{"id": id, "name": name, "title": name, "status": "running", "input": data["input"]}
			state.tools = append(state.tools, tool)
			part := map[string]any{"type": "tool"}
			for key, value := range tool {
				part[key] = value
			}
			state.parts = append(state.parts, part)
		} else if input := data["input"]; input != nil {
			tool["input"] = input
		}
		if invocation := data["invocationId"]; invocation != nil {
			tool["invocationId"] = invocation
		}
		state.syncTool(tool)
		return true
	case outbound.EventLLMToolProgress:
		tool := state.tool(id)
		if tool == nil {
			return false
		}
		payload := jsonObject(data["payload"])
		output, _ := tool["output"].(map[string]any)
		if output == nil {
			output = map[string]any{}
		}
		stream := "stdout"
		if payload["stream"] == "stderr" {
			stream = "stderr"
		}
		if chunk, _ := payload["chunk"].(string); chunk != "" {
			previous, _ := output[stream].(string)
			output[stream] = previous + chunk
		}
		// Interactive prompts generate their questionId at runtime, outside
		// the LLM input. Persist it while waiting, not only after the answer.
		for key, value := range payload {
			if key != "stream" && key != "chunk" {
				output[key] = value
			}
		}
		tool["output"] = output
		if invocation := data["invocationId"]; invocation != nil {
			tool["invocationId"] = invocation
		}
		state.syncTool(tool)
		return payload["questionId"] != nil
	case outbound.EventLLMToolResult:
		tool := state.tool(id)
		if tool == nil {
			return false
		}
		tool["status"] = "success"
		if status := extractToolStatus(data["output"]); status != "" {
			tool["status"] = status
		}
		if status, _ := data["status"].(string); status != "" {
			tool["status"] = status
		}
		if message, _ := data["error"].(string); message != "" {
			tool["status"], tool["error"] = "error", message
		}
		if output := data["output"]; output != nil {
			tool["output"] = output
		}
		if invocation := data["invocationId"]; invocation != nil {
			tool["invocationId"] = invocation
		}
		state.syncTool(tool)
		return true
	}
	return false
}

func jsonObject(value any) map[string]any {
	if object, ok := value.(map[string]any); ok {
		return object
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	var object map[string]any
	_ = json.Unmarshal(encoded, &object)
	return object
}

func (state *chatStreamState) finish(status, message string) {
	if state.hasCanonical {
		state.text = stages.StripReplyControlBlock(state.canonical)
		if state.text == "" {
			kept := make([]map[string]any, 0, len(state.parts))
			for _, part := range state.parts {
				if part["type"] != "text" {
					kept = append(kept, part)
				}
			}
			state.parts = kept
		} else {
			state.parts = reconcileTextParts(state.parts, state.text)
		}
	}
	if state.text == "" && len(state.tools) == 0 {
		switch status {
		case "cancelled":
			state.text = "已停止生成。"
		case "error":
			state.text = "请求处理失败：" + message
		case "dropped":
			state.text = "本次请求未生成回复。"
		}
		state.parts = reconcileTextParts(state.parts, state.text)
	}
	for _, tool := range state.tools {
		if isPhantomRunning(tool) {
			markToolCallSuperseded(tool)
		} else if tool["status"] == "running" {
			tool["status"] = "killed"
		}
		state.syncTool(tool)
	}
}

func (state *chatStreamState) payload(botID, sessionID, traceID string) map[string]any {
	return map[string]any{
		"botId": botID, "sessionId": sessionID, "traceId": traceID,
		"text": stages.StripReplyControlBlock(state.text), "toolCalls": state.tools, "parts": state.parts,
		"streamSeq": state.seq,
	}
}

type chatTurn struct {
	svc                      *BotService
	channel                  *WebChannel
	bus                      *outbound.MemoryEventBus
	sub                      *outbound.Subscription
	responses                chan core.Action
	botID, userID, sessionID string
	traceID                  string
	fail                     chan error
	lifecycleDone            chan struct{}
	done                     chan struct{}
	state                    chatStreamState
	historyIncomplete        bool
}

// prepareChatTurn gives one goroutine ownership of all snapshots and the
// final reply. It remains alive across HTTP disconnects, so fallback writers
// cannot race a detached SSE handler or erase its tool history.
func (s *BotService) prepareChatTurn(channel *WebChannel, botID, userID, sessionID, traceID string) *chatTurn {
	bus, ok := s.eventBus.(*outbound.MemoryEventBus)
	if !ok || bus == nil || s.chatHistory == nil {
		return nil
	}
	turn := &chatTurn{
		svc: s, channel: channel, bus: bus,
		botID: botID, userID: userID, sessionID: sessionID, traceID: traceID,
		responses: channel.RegisterResponse(traceID, 16),
		sub:       bus.SubscribeNewTrace(traceID), fail: make(chan error, 1), done: make(chan struct{}),
		lifecycleDone: make(chan struct{}, 1),
	}
	managedChatTurns.Store(messageCancelKey(botID, traceID), turn)
	go turn.run()
	return turn
}

// notifyChatTurnDone is independent of the bounded replay ring. A collector
// recovering from overflow must still learn that the actual pipeline exited.
func (s *BotService) notifyChatTurnDone(botID, traceID string) {
	if value, ok := managedChatTurns.Load(messageCancelKey(botID, traceID)); ok {
		turn := value.(*chatTurn)
		select {
		case turn.lifecycleDone <- struct{}{}:
		default:
		}
	}
}

func (turn *chatTurn) save(streaming bool) error {
	tools, err := json.Marshal(turn.state.tools)
	if err != nil {
		return err
	}
	parts, err := json.Marshal(turn.state.parts)
	if err != nil {
		return err
	}
	return turn.svc.chatHistory.UpsertAssistantSnapshot(turn.botID, turn.userID,
		stages.StripReplyControlBlock(turn.state.text), turn.traceID, string(tools), string(parts), turn.sessionID, streaming, turn.state.seq)
}

func (turn *chatTurn) run() {
	defer close(turn.done)
	defer managedChatTurns.Delete(messageCancelKey(turn.botID, turn.traceID))
	defer turn.channel.UnregisterResponse(turn.traceID)
	defer func() { turn.bus.Unsubscribe(turn.sub) }()
	lastSave := time.Time{}
	dirty := false
	flushTicker := time.NewTicker(streamPersistInterval)
	defer flushTicker.Stop()
	flush := func() {
		if !dirty {
			return
		}
		if err := turn.save(true); err != nil {
			turn.svc.logger.Warnw("chat snapshot failed", "trace_id", turn.traceID, "err", err)
		} else {
			dirty, lastSave = false, time.Now()
		}
	}
	drainCanonical := func() {
		// Dispatch puts the canonical ActionReply in this channel before
		// publishing message.done. Drain it even when select picked done first.
		for draining := true; draining; {
			select {
			case action, open := <-turn.responses:
				if !open {
					draining = false
				} else if action.Type == core.ActionReply {
					turn.state.canonical, _ = action.Payload.(string)
					turn.state.hasCanonical = true
				}
			default:
				draining = false
			}
		}
	}
	finish := func(status, message string) {
		drainCanonical()
		if turn.historyIncomplete {
			warning := "部分执行过程因事件缓冲区溢出未完整保存。"
			if turn.state.hasCanonical {
				warning = "部分执行过程因事件缓冲区溢出未完整保存，最终回复已保留。"
			}
			if message == "" {
				message = warning
			} else {
				message += "；" + warning
			}
		}
		turn.state.finish(status, message)
		payload := turn.state.payload(turn.botID, turn.sessionID, turn.traceID)
		payload["status"] = status
		if turn.historyIncomplete {
			payload["historyIncomplete"], payload["message"], payload["streamError"] = true, message, message
		}
		tools, _ := json.Marshal(turn.state.tools)
		parts, _ := json.Marshal(turn.state.parts)
		if err := turn.svc.chatHistory.FinalizeAssistantSnapshot(turn.botID, turn.userID,
			stages.StripReplyControlBlock(turn.state.text), turn.traceID, string(tools), string(parts), turn.sessionID,
			turn.state.seq, status, message); err != nil {
			turn.svc.logger.Errorw("chat final snapshot failed", "trace_id", turn.traceID, "err", err)
			payload["persistenceError"] = "回复未能保存到历史记录"
		}
		eventType := eventChatDone
		if status == "error" {
			eventType = eventChatError
			payload["message"], payload["recoverable"] = message, false
		}
		turn.bus.Publish(context.Background(), outbound.Event{Type: eventType, BotID: turn.botID, TraceID: turn.traceID, Data: payload})
	}
	lifecycleEnded := false
	for {
		if turn.historyIncomplete && lifecycleEnded && !turn.sub.Overflowed() && len(turn.sub.C()) == 0 {
			// Prefer any retained lifecycle event (including errors) over this
			// fallback. The callback happens after dispatch and lifecycle
			// publication, so a drained response channel is authoritative here.
			drainCanonical()
			if turn.state.hasCanonical {
				finish("completed", "")
			} else {
				finish("error", "任务已结束，但完成事件和最终回复未能恢复")
			}
			return
		}
		select {
		case <-turn.lifecycleDone:
			lifecycleEnded = true
		case <-flushTicker.C:
			// Persist the trailing delta even when the model/tool subsequently
			// stays quiet. A throttle without this tick can leave an old cursor
			// forever and make an otherwise healthy task impossible to resume.
			flush()
		case err := <-turn.fail:
			finish("error", err.Error())
			return
		case action, open := <-turn.responses:
			if !open {
				finish("cancelled", "bot stopped")
				return
			}
			if action.Type == core.ActionReply {
				turn.state.canonical, _ = action.Payload.(string)
				turn.state.hasCanonical = true
			}
		case event, open := <-turn.sub.C():
			if !open {
				if turn.sub.Overflowed() {
					next := turn.bus.SubscribeWithReplay(turn.traceID, turn.state.seq)
					turn.sub = next
					if next.ReplayTruncated {
						// Losing tool progress does not stop the pipeline. Keep
						// its response registered so the canonical final answer
						// cannot be discarded by a premature terminal snapshot.
						turn.historyIncomplete = true
						turn.svc.logger.Warnw("chat history incomplete; preserving final reply receiver", "trace_id", turn.traceID)
					}
					continue
				} else {
					finish("cancelled", "event bus closed")
				}
				return
			}
			if event.BotID != "" && event.BotID != turn.botID {
				continue
			}
			phantomID := ""
			if event.Type == outbound.EventLLMToolCall {
				name, _ := event.Data["tool"].(string)
				if idx := findPhantomToolCall(turn.state.tools, name, event.Data["input"]); idx >= 0 {
					phantomID, _ = turn.state.tools[idx]["id"].(string)
				}
			}
			force := turn.state.apply(event)
			dirty = true
			if phantomID != "" {
				turn.bus.Publish(context.Background(), outbound.Event{Type: outbound.EventLLMToolResult, BotID: turn.botID, TraceID: turn.traceID,
					Data: map[string]any{"toolCallId": phantomID, "tool": event.Data["tool"], "status": "superseded", "output": phantomSupersededMsg}})
			}
			switch event.Type {
			case outbound.EventMessageDone:
				finish("completed", "")
				return
			case outbound.EventMessageDropped:
				finish("dropped", "")
				return
			case outbound.EventMessageError, outbound.EventDispatchError:
				message, _ := event.Data["error"].(string)
				if message == "" {
					message = "请求处理失败"
				}
				status := "error"
				if strings.Contains(message, context.Canceled.Error()) {
					status = "cancelled"
				}
				finish(status, message)
				return
			}
			if force || time.Since(lastSave) >= streamPersistInterval {
				flush()
			}
		}
	}
}

func snapshotPayload(snapshot *dao.ChatMessage) map[string]any {
	dto := toChatMessageDTOs([]dao.ChatMessage{*snapshot})[0]
	status := snapshot.StreamStatus
	if status == "" {
		status = "completed"
	}
	return map[string]any{
		"botId": snapshot.BotID, "sessionId": snapshot.SessionID, "traceId": snapshot.TraceID,
		"text": snapshot.Content, "toolCalls": dto.ToolCalls, "parts": dto.Parts,
		"streamSeq": snapshot.StreamSeq, "status": status,
		"message": snapshot.StreamError, "streamError": snapshot.StreamError, "recoverable": false,
	}
}

func snapshotSSEType(snapshot *dao.ChatMessage) string {
	if snapshot.StreamStatus == "error" {
		return sseError
	}
	return sseDone
}

func writeSSEWithID(w io.Writer, kind string, data any, seq uint64) error {
	encoded, err := json.Marshal(data)
	if err != nil {
		return err
	}
	if seq != 0 {
		if _, err = fmt.Fprintf(w, "id: %d\n", seq); err != nil {
			return err
		}
	}
	_, err = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", kind, encoded)
	return err
}

// serveChatEvents has no responsibility for task lifetime or persistence.
// Closing its HTTP request only releases this particular subscriber.
func (s *Server) serveChatEvents(c *gin.Context, bus *outbound.MemoryEventBus, sub *outbound.Subscription,
	botID, sessionID, traceID string, snapshot *dao.ChatMessage, sinceSeq uint64) {
	defer bus.Unsubscribe(sub)
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")
	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		return
	}
	write := func(kind string, data any, seq uint64) bool {
		if err := writeSSEWithID(c.Writer, kind, data, seq); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}
	if !write(sseStart, map[string]any{"traceId": traceID, "sessionId": sessionID}, 0) {
		return
	}
	if snapshot != nil {
		sinceSeq = snapshot.StreamSeq
		if !write("snapshot", map[string]any{"message": toChatMessageDTOs([]dao.ChatMessage{*snapshot})[0]}, 0) {
			return
		}
		if !snapshot.Streaming {
			write(snapshotSSEType(snapshot), snapshotPayload(snapshot), sinceSeq)
			return
		}
	}
	recoverStream := func(message string) {
		write(sseError, map[string]any{"message": message, "recoverable": true,
			"traceId": traceID, "sessionId": sessionID}, 0)
	}
	if sub.ReplayAfterSeq > sinceSeq {
		recoverStream("历史事件已过期，请重新加载会话以同步最新记录")
		return
	}
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	var pendingTerminal *outbound.Event
	finishLegacy := func(event outbound.Event) {
		latest, err := s.chatHistory.LoadAssistantByTrace(botID, traceID)
		if err == nil && latest != nil && !latest.Streaming {
			write("snapshot", map[string]any{"message": toChatMessageDTOs([]dao.ChatMessage{*latest})[0]}, 0)
			write(snapshotSSEType(latest), snapshotPayload(latest), event.Seq)
		} else if event.Type == outbound.EventMessageError || event.Type == outbound.EventDispatchError {
			message, _ := event.Data["error"].(string)
			write(sseError, map[string]any{"message": message, "recoverable": false, "traceId": traceID, "sessionId": sessionID}, event.Seq)
		} else {
			write(sseDone, map[string]any{"traceId": traceID, "sessionId": sessionID}, event.Seq)
		}
	}
	for {
		if sub.Overflowed() {
			recoverStream("连接接收速度不足，请重新连接以恢复最新记录")
			return
		}
		if pendingTerminal != nil {
			if _, managed := managedChatTurns.Load(messageCancelKey(botID, traceID)); !managed && len(sub.C()) == 0 {
				// A managed collector queues chat.done/error before removing
				// itself. Drain those events first, including canonical data
				// when its final DB write failed; only legacy producers need
				// to fall back to the raw lifecycle event.
				finishLegacy(*pendingTerminal)
				return
			}
		}
		select {
		case <-c.Request.Context().Done():
			return
		case <-heartbeat.C:
			if !write(ssePing, map[string]any{"ts": time.Now().Unix()}, 0) {
				return
			}
		case event, open := <-sub.C():
			if !open {
				recoverStream("事件连接已关闭，请重新连接")
				return
			}
			if event.BotID != "" && event.BotID != botID || event.Seq <= sinceSeq {
				continue
			}
			sinceSeq = event.Seq
			switch event.Type {
			case eventChatDone:
				write(sseDone, event.Data, event.Seq)
				return
			case eventChatError:
				write(sseError, event.Data, event.Seq)
				return
			case outbound.EventMessageDone, outbound.EventMessageDropped, outbound.EventMessageError, outbound.EventDispatchError:
				terminal := event
				pendingTerminal = &terminal
				continue
			}
			kind, data := translateEventToSSE(event)
			if kind != "" && !write(kind, data, event.Seq) {
				return
			}
		}
	}
}
