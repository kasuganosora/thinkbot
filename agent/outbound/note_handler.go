package outbound

import (
	"context"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	noop_trace "go.opentelemetry.io/otel/trace/noop"
	"go.uber.org/zap"

	"github.com/kasuganosora/thinkbot/agent/core"
	"github.com/kasuganosora/thinkbot/util/errs"
	"github.com/kasuganosora/thinkbot/util/idgen"
)

// ============================================================================
// NoteEntry — NoteHandler 写入的记忆条目（本地定义避免循环依赖）
// ============================================================================

// NoteEntry 表示 NoteHandler 要写入的一条记忆。
// 与 memory.Entry 字段一一对应，由 memory.Store 隐式满足 NoteWriter 接口。
type NoteEntry struct {
	ID             string
	ScopeKind      string // "channel" / "bot" / "user" / "global"
	ScopeID        string
	Content        string
	Category       string
	Source         string
	Importance     float64
	Metadata       map[string]any
	CreatedAt      time.Time
	LastAccessedAt time.Time
}

// NoteWriter 定义备注写入能力（最小接口，避免循环依赖 memory 包）。
// memory.Store (memory.MemoryRepository / storage.SQLiteRepository) 均隐式满足此接口。
//
// Go 接口隐式满足机制：memory.Store.Append(ctx, Entry) 签名需与此匹配。
// 由于 Entry 类型不同（不同包），我们使用适配器模式桥接。
type NoteWriter interface {
	// WriteNote 将一条备注写入记忆仓储。
	WriteNote(ctx context.Context, entry NoteEntry) error
}

// ============================================================================
// NoteHandler — 备注处理器（ActionHandler 实现）
// ============================================================================

// NoteHandler 处理 ActionNote 类型的 Action。
// 它从 Action 中提取备注信息，转换为 NoteEntry 通过 NoteWriter 写入记忆仓储。
// 这样 Bot 的自主思考/观察/备注统一纳入记忆体系，
// 后续 LLM 搜索记忆时可以回忆起自己曾经在想什么。
//
// 注册到 MultiDispatcher 后处理 ActionNote：
//
//	noteHandler := outbound.NewNoteHandler(writer, logger, tp)
//	dispatcher.Register(core.ActionNote, noteHandler)
//
// Action 字段约定（ActionNote）：
//   - Action.Payload：备注文本（string）
//   - Action.Channel：关联的会话空间标识
//   - Action.UserID：关联的用户 ID
//   - Action.Metadata["source_channel"]：来源 Channel（通用路由字段）
//   - Action.Metadata["message_id"]：触发此备注的原始消息 ID
//   - Action.Metadata["category"]：备注分类（可选，默认 "observation"）
//   - Action.Metadata["bot_id"]：所属 Bot ID
//   - Action.Metadata["importance"]：重要程度 float64（可选，默认 0.5）
type NoteHandler struct {
	writer NoteWriter
	logger *zap.SugaredLogger
	tracer trace.Tracer
}

// NewNoteHandler 创建备注处理器。
// writer 是记忆写入接口，备注将作为 Entry（Source="note"）写入统一记忆仓储。
func NewNoteHandler(writer NoteWriter, logger *zap.SugaredLogger, tp trace.TracerProvider) *NoteHandler {
	if tp == nil {
		tp = noop_trace.NewTracerProvider()
	}
	if logger == nil {
		logger = zap.NewNop().Sugar()
	}
	return &NoteHandler{
		writer: writer,
		logger: logger.With("component", "note_handler"),
		tracer: tp.Tracer("github.com/kasuganosora/thinkbot/agent/outbound/note"),
	}
}

// Handle 处理 ActionNote 类型的 Action。
// 将备注转换为 memory.Entry 写入统一记忆仓储。
// 实现 ActionHandler 接口。
func (h *NoteHandler) Handle(ctx context.Context, action core.Action) error {
	ctx, span := h.tracer.Start(ctx, "note.handle",
		trace.WithAttributes(
			attribute.String("action.type", string(action.Type)),
			attribute.String("action.channel", action.Channel),
		))
	defer span.End()

	// 提取备注文本
	text := ""
	if action.Payload != nil {
		if s, ok := action.Payload.(string); ok {
			text = s
		}
	}
	if text == "" {
		h.logger.Debugw("empty note, skipping", "channel", action.Channel)
		return nil
	}

	// 从 Metadata 提取关联信息
	botID := ""
	messageID := ""
	category := "observation" // 默认分类
	importance := 0.5         // 默认重要度
	if action.Metadata != nil {
		if v, ok := action.Metadata["bot_id"]; ok {
			if s, ok := v.(string); ok {
				botID = s
			}
		}
		if v, ok := action.Metadata["message_id"]; ok {
			if s, ok := v.(string); ok {
				messageID = s
			}
		}
		if v, ok := action.Metadata["category"]; ok {
			if s, ok := v.(string); ok && s != "" {
				category = s
			}
		}
		if v, ok := action.Metadata["importance"]; ok {
			if f, ok := v.(float64); ok {
				importance = f
			}
		}
	}

	// 确定 Scope
	scopeKind := "channel"
	scopeID := action.Channel
	if action.Channel == "" {
		if botID != "" {
			scopeKind = "bot"
			scopeID = botID
		} else {
			scopeKind = "global"
			scopeID = ""
		}
	}

	// 生成唯一 ID
	noteID := idgen.New("note")

	// 构建 metadata（保留原始信息便于溯源）
	entryMeta := map[string]any{
		"user_id": action.UserID,
	}
	if messageID != "" {
		entryMeta["message_id"] = messageID
	}
	if botID != "" {
		entryMeta["bot_id"] = botID
	}
	// 保留 action 中的其他 metadata
	if action.Metadata != nil {
		for k, v := range action.Metadata {
			if k != "bot_id" && k != "message_id" && k != "category" && k != "importance" {
				entryMeta[k] = v
			}
		}
	}

	entry := NoteEntry{
		ID:         noteID,
		ScopeKind:  scopeKind,
		ScopeID:    scopeID,
		Content:    text,
		Category:   category,
		Source:     "note",
		Importance: importance,
		Metadata:   entryMeta,
		CreatedAt:  time.Now(),
	}

	span.SetAttributes(
		attribute.String("entry.id", entry.ID),
		attribute.String("entry.scope_kind", scopeKind),
		attribute.String("entry.scope_id", scopeID),
		attribute.String("entry.category", entry.Category),
		attribute.String("entry.source", entry.Source),
	)

	h.logger.Infow("saving note as memory entry",
		"entry_id", entry.ID,
		"scope", scopeKind+":"+scopeID,
		"bot_id", botID,
		"channel", action.Channel,
		"user_id", action.UserID,
		"category", entry.Category,
		"text_len", len(text))

	if err := h.writer.WriteNote(ctx, entry); err != nil {
		h.logger.Errorw("note save failed",
			"entry_id", entry.ID, "err", err)
		return errs.Wrap(err, "note_handler: save failed")
	}

	return nil
}
