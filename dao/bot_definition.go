package dao

import "time"

// BotDefinition 持久化的 Bot 定义。
// 管理员通过 API 创建/编辑 Bot 定义，系统启动时从 DB 加载并实例化。
type BotDefinition struct {
	// ID Bot 唯一标识（如 "customer-service"）。
	ID string `gorm:"primaryKey;size:64" json:"id"`

	// Name 显示名称。
	Name string `gorm:"size:128;not null" json:"name"`

	// Avatar 头像（emoji 或 URL）。
	Avatar string `gorm:"size:256;default:''" json:"avatar"`

	// SystemPrompt 系统提示词。
	SystemPrompt string `gorm:"type:text" json:"systemPrompt"`

	// LLMMain 主力 LLM 模型 ID（对应 config llm.models.<id>）。
	LLMMain string `gorm:"size:64;default:''" json:"llmMain"`

	// LLMLight 低成本 LLM 模型 ID。
	LLMLight string `gorm:"size:64;default:''" json:"llmLight"`

	// Model 模型标识（如 "gpt-4o"），兼容 BotConfig.Model。
	Model string `gorm:"size:128;default:''" json:"model"`

	// Temperature 温度参数。
	Temperature float64 `gorm:"default:0.7" json:"temperature"`

	// MaxTokens 最大输出 token 数。
	MaxTokens int `gorm:"default:4096" json:"maxTokens"`

	// ReasoningEffort 深度思考程度（""=禁用, "minimal", "low", "medium", "high"）。
	ReasoningEffort string `gorm:"size:16;default:''" json:"reasoningEffort"`

	// Workers 并发 worker 数量。
	Workers int `gorm:"default:4" json:"workers"`

	// Status 运行状态：stopped | running。
	Status string `gorm:"size:32;not null;default:'stopped'" json:"status"`

	// CreatedAt 创建时间。
	CreatedAt time.Time `gorm:"autoCreateTime" json:"createdAt"`

	// UpdatedAt 更新时间。
	UpdatedAt time.Time `gorm:"autoUpdateTime" json:"updatedAt"`
}

// TableName 指定 GORM 表名。
func (BotDefinition) TableName() string { return "bot_definitions" }

// Bot 定义状态常量。
const (
	BotStatusStopped = "stopped"
	BotStatusRunning = "running"
)
