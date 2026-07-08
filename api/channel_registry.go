package api

// ============================================================================
// Channel 类型注册表 — 描述每种 Channel 类型的配置字段 schema
//
// 驱动前端动态表单渲染：前端根据 Fields[] 自动生成配置表单。
// ============================================================================

// ChannelField 描述一个 Channel 配置字段。
type ChannelField struct {
	Key      string   `json:"key"`
	Label    string   `json:"label"`
	Type     string   `json:"type"` // "string"|"password"|"number"|"select"|"boolean"
	Required bool     `json:"required"`
	Default  string   `json:"default,omitempty"`
	HelpText string   `json:"helpText,omitempty"`
	Options  []string `json:"options,omitempty"`
}

// ChannelTypeInfo 描述一种 Channel 类型的元信息。
type ChannelTypeInfo struct {
	Type        string         `json:"type"`
	DisplayName string         `json:"displayName"`
	Description string         `json:"description"`
	Icon        string         `json:"icon,omitempty"`
	Color       string         `json:"color,omitempty"`
	Fields      []ChannelField `json:"fields"`
}

// ToPlatformType 转为前端 BotPlatforms 组件需要的格式。
func (c ChannelTypeInfo) ToPlatformType() PlatformType {
	return PlatformType{
		Type:        c.Type,
		Name:        c.DisplayName,
		Description: c.Description,
		Icon:        c.Icon,
		Color:       c.Color,
		Fields:      c.ToPlatformFields(),
	}
}

// ToPlatformFields 将 ChannelField 列表转为 PlatformField 列表。
func (c ChannelTypeInfo) ToPlatformFields() []PlatformField {
	out := make([]PlatformField, len(c.Fields))
	for i, f := range c.Fields {
		out[i] = PlatformField{
			Key:         f.Key,
			Label:       f.Label,
			Type:        f.Type,
			Help:        f.HelpText,
			Placeholder: f.Default,
			Optional:    !f.Required,
			Options:     f.Options,
		}
	}
	return out
}

// supportedChannelTypes 是系统支持的 Channel/Platform 类型注册表（统一）。
//
// 注意：此处只登记「后端 channel/ 目录下已有真实实现」的平台。
// 新增平台的正确顺序：先在 channel/<type>/ 实现 Channel，再在此注册字段 schema，
// 前端「添加平台」列表与配置表单会据此自动渲染，无需改动前端代码。
var supportedChannelTypes = []ChannelTypeInfo{
	{
		Type:        "telegram",
		DisplayName: "Telegram",
		Description: "通过 Telegram Bot API 接收用户消息，使用 long polling 方式。",
		Icon:        "telegram",
		Color:       "#2aabee",
		Fields: []ChannelField{
			{Key: "token", Label: "Bot Token", Type: "password", Required: true, HelpText: "从 @BotFather 获取"},
			{Key: "pollTimeout", Label: "Long Polling 超时（秒）", Type: "number", Default: "30"},
			{Key: "apiBaseUrl", Label: "API 反代地址", Type: "string", HelpText: "无法直连 api.telegram.org 时填写"},
			{Key: "parseMode", Label: "消息格式化模式", Type: "select", Default: "", Options: []string{"", "HTML", "MarkdownV2"}},
			{Key: "allowedUpdates", Label: "接收的更新类型", Type: "string", Default: "message,edited_message", HelpText: "逗号分隔"},
		},
	},
	{
		Type:        "misskey",
		DisplayName: "Misskey",
		Description: "通过 Misskey WebSocket streaming 接收提及和回复消息。",
		Icon:        "misskey",
		Color:       "#86b300",
		Fields: []ChannelField{
			{Key: "host", Label: "实例 URL", Type: "string", Required: true, HelpText: "如 https://misskey.io"},
			{Key: "token", Label: "API Token", Type: "password", Required: true, HelpText: "Misskey API Token"},
			{Key: "subscribeTimeline", Label: "订阅时间线", Type: "boolean", Default: "false", HelpText: "启用后接收时间线全部帖子"},
		},
	},
}

// SupportedChannelTypes 返回所有支持的 Channel 类型信息。
func SupportedChannelTypes() []ChannelTypeInfo {
	return supportedChannelTypes
}

// SupportedPlatformTypes 返回所有支持的平台类型（前端 BotPlatforms 组件格式）。
// 与 SupportedChannelTypes 共享同一个类型注册表。
func SupportedPlatformTypes() []PlatformType {
	types := SupportedChannelTypes()
	out := make([]PlatformType, len(types))
	for i, t := range types {
		out[i] = t.ToPlatformType()
	}
	return out
}

// GetChannelTypeInfo 根据类型标识查找 Channel 类型信息。
func GetChannelTypeInfo(channelType string) (*ChannelTypeInfo, bool) {
	for i := range supportedChannelTypes {
		if supportedChannelTypes[i].Type == channelType {
			return &supportedChannelTypes[i], true
		}
	}
	return nil, false
}

// IsValidChannelType 检查是否为支持的 Channel 类型。
func IsValidChannelType(channelType string) bool {
	_, ok := GetChannelTypeInfo(channelType)
	return ok
}
