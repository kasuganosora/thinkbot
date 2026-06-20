package api

// ============================================================================
// 请求 / 响应 DTO 定义
// ============================================================================

// --- 认证 ---

// LoginReq 登录请求。
type LoginReq struct {
	Username string `json:"username" binding:"required"`
	Password string `json:"password" binding:"required"`
}

// LoginResp 登录响应。
type LoginResp struct {
	ID          uint    `json:"id"`
	Username    string  `json:"username"`
	Role        string  `json:"role"`
	DisplayName string  `json:"displayName"`
	Avatar      string  `json:"avatar"`
	LastLoginAt *string `json:"lastLoginAt,omitempty"`
}

// ChangePasswordReq 修改密码请求。
type ChangePasswordReq struct {
	OldPassword string `json:"oldPassword" binding:"required"`
	NewPassword string `json:"newPassword" binding:"required,min=6"`
}

// --- 用户管理 ---

// CreateUserReq 创建用户请求（admin）。
type CreateUserReq struct {
	Username    string `json:"username" binding:"required,min=3"`
	Password    string `json:"password" binding:"required,min=6"`
	Email       string `json:"email"`
	Role        string `json:"role"`
	DisplayName string `json:"displayName"`
}

// UpdateUserReq 更新用户资料请求（admin）。
type UpdateUserReq struct {
	Email       *string `json:"email"`
	DisplayName *string `json:"displayName"`
	Avatar      *string `json:"avatar"`
}

// UpdateRoleReq 修改角色请求（admin）。
type UpdateRoleReq struct {
	Role string `json:"role" binding:"required"`
}

// --- Bot 管理 ---

// CreateBotReq 创建 Bot 请求（admin）。
type CreateBotReq struct {
	ID           string  `json:"id" binding:"required"`
	Name         string  `json:"name" binding:"required"`
	SystemPrompt string  `json:"systemPrompt"`
	LLMMain      string  `json:"llmMain"`
	LLMLight     string  `json:"llmLight"`
	Model        string  `json:"model"`
	Temperature  float64 `json:"temperature"`
	MaxTokens    int     `json:"maxTokens"`
	Workers      int     `json:"workers"`
}

// UpdateBotReq 更新 Bot 请求（admin）。
type UpdateBotReq struct {
	Name         *string  `json:"name"`
	SystemPrompt *string  `json:"systemPrompt"`
	LLMMain      *string  `json:"llmMain"`
	LLMLight     *string  `json:"llmLight"`
	Model        *string  `json:"model"`
	Temperature  *float64 `json:"temperature"`
	MaxTokens    *int     `json:"maxTokens"`
	Workers      *int     `json:"workers"`
}

// --- 聊天 ---

// ChatReq 发送聊天消息请求。
type ChatReq struct {
	BotID string `json:"botId" binding:"required"`
	Text  string `json:"text" binding:"required"`
}

// --- Channel 管理 ---

// CreateChannelReq 创建 Channel 配置请求。
type CreateChannelReq struct {
	Name   string `json:"name" binding:"required"`
	Type   string `json:"type" binding:"required"`
	Config string `json:"config"` // JSON 字符串
}

// UpdateChannelReq 更新 Channel 配置请求。
type UpdateChannelReq struct {
	Name    *string `json:"name"`
	Config  *string `json:"config"`
	Enabled *bool   `json:"enabled"`
}

// --- 配置 ---

// SetConfigReq 设置单个配置项请求。
type SetConfigReq struct {
	Value string `json:"value" binding:"required"`
}

// BatchSetConfigReq 批量设置配置项请求。
type BatchSetConfigReq struct {
	Items map[string]string `json:"items" binding:"required"`
}
