package stages

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"

	"github.com/kasuganosora/thinkbot/agent/core"
	"github.com/kasuganosora/thinkbot/llm"
)

// ============================================================================
// HITL 工具审批续跑锚点
// ============================================================================
//
// 当某工具标记 RequireApproval 且 ApprovalHandler 返回 deferred 时，编排层暂停
// （工具未执行，result.Text 是半成品回复）。LLMStage 在返回前：
//   1. 持久化一条 DeferredApproval 记录（锚点），使审批在进程重启后依然可恢复；
//   2. 发射 hitl/deferred 事件（进入可观测轨迹）；
//   3. 标记 KVLLMDeferred，阻断半成品回复（回复本就在 ActionReply 生成前 return）。
// 人类确认后调用 ResumeDeferredApproval(approvalID, decision, reason)：
//   加载记录 → 标记 resolved → 把决策按「工具名」注入预批准 context → 重新编排
//   原始消息（携带该 context），使被 defer 的工具这次直接采用人类决策而非再次挂起。
//
// 默认路径（无 ApprovalHandler）此分支永不触发，store/resume 均为 nil，行为完全不变。

// DeferredApproval 被 defer 的工具审批待确认记录（HITL 续跑锚点）。
type DeferredApproval struct {
	ApprovalID string `gorm:"primaryKey;size:64" json:"approvalId"`
	BotID      string `gorm:"size:64;index" json:"botId"`
	MessageID  string `gorm:"size:128" json:"messageId"`
	TraceID    string `gorm:"size:128" json:"traceId"`
	Channel    string `gorm:"size:128" json:"channel"`
	UserID     string `gorm:"size:128" json:"userId"`
	ToolName   string `gorm:"size:128" json:"toolName"`
	ToolCallID string `gorm:"size:128" json:"toolCallId"`
	InputJSON  string `gorm:"type:text" json:"input"`
	// MessageJSON 原始入站 core.Message 的序列化，供续跑重建 Envelope（保留
	// Metadata / reply_target 等下游路由所需字段）。
	MessageJSON string `gorm:"type:text" json:"message"`
	// Decision 原始 defer 决策（恒为 "deferred"）。
	Decision string `gorm:"size:32" json:"decision"`
	Reason   string `gorm:"type:text" json:"reason"`
	// Status pending / resolved。
	Status string `gorm:"size:32;index" json:"status"`
	// ResolvedDecision / ResolvedReason 人类确认时的决策与理由。
	ResolvedDecision string `gorm:"size:32" json:"resolvedDecision"`
	ResolvedReason   string `gorm:"type:text" json:"resolvedReason"`
	CreatedAt        time.Time `json:"createdAt"`
	UpdatedAt        time.Time `json:"updatedAt"`
}

// TableName 指定 GORM 表名。
func (DeferredApproval) TableName() string { return "deferred_approvals" }

// DeferredApprovalStore 持久化被 defer 的审批，供续跑恢复。
type DeferredApprovalStore interface {
	// Persist 写入/更新一条待确认记录。
	Persist(ctx context.Context, rec *DeferredApproval) error
	// Load 按 ApprovalID 读取；不存在返回 (nil, nil)。
	Load(ctx context.Context, approvalID string) (*DeferredApproval, error)
	// MarkResolved 标记记录为人类已确认（status=resolved + 决策）。
	MarkResolved(ctx context.Context, approvalID, decision, reason string) error
}

type gormDeferredApprovalStore struct {
	db *gorm.DB
}

// NewDeferredApprovalStore 创建基于 GORM 的审批存储（自动迁移表）。
func NewDeferredApprovalStore(db *gorm.DB) (DeferredApprovalStore, error) {
	if db == nil {
		return nil, fmt.Errorf("hitl: nil db")
	}
	if err := db.AutoMigrate(&DeferredApproval{}); err != nil {
		return nil, err
	}
	return &gormDeferredApprovalStore{db: db}, nil
}

func (s *gormDeferredApprovalStore) Persist(ctx context.Context, rec *DeferredApproval) error {
	if rec.CreatedAt.IsZero() {
		rec.CreatedAt = time.Now()
	}
	rec.UpdatedAt = time.Now()
	if rec.Status == "" {
		rec.Status = "pending"
	}
	return s.db.WithContext(ctx).Save(rec).Error
}

func (s *gormDeferredApprovalStore) Load(ctx context.Context, approvalID string) (*DeferredApproval, error) {
	var rec DeferredApproval
	if err := s.db.WithContext(ctx).First(&rec, "approval_id = ?", approvalID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return &rec, nil
}

func (s *gormDeferredApprovalStore) MarkResolved(ctx context.Context, approvalID, decision, reason string) error {
	return s.db.WithContext(ctx).Model(&DeferredApproval{}).
		Where("approval_id = ?", approvalID).
		Updates(map[string]any{
			"status":            "resolved",
			"resolved_decision": decision,
			"resolved_reason":   reason,
			"updated_at":        time.Now(),
		}).Error
}

// BuildDeferredApproval 从被 defer 的审批结果与原始入站消息构造持久化记录。
func BuildDeferredApproval(da *llm.ToolApprovalResult, msg core.Message) (*DeferredApproval, error) {
	msgJSON, err := json.Marshal(msg)
	if err != nil {
		return nil, err
	}
	var inputJSON string
	if da.Input != nil {
		if b, err := json.Marshal(da.Input); err == nil {
			inputJSON = string(b)
		}
	}
	return &DeferredApproval{
		ApprovalID:  da.ApprovalID,
		BotID:       msg.BotID,
		MessageID:   msg.ID,
		TraceID:     msg.TraceID,
		Channel:     msg.Channel,
		UserID:      msg.UserID,
		ToolName:    da.ToolName,
		ToolCallID:  da.ToolCallID,
		InputJSON:   inputJSON,
		MessageJSON: string(msgJSON),
		Decision:    string(da.Decision),
		Reason:      da.Reason,
		Status:      "pending",
	}, nil
}

// ensure interface compliance (compile-time guard).
var _ DeferredApprovalStore = (*gormDeferredApprovalStore)(nil)
