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
	Fields      []ChannelField `json:"fields"`
}

// supportedChannelTypes 是系统支持的 Channel 类型注册表。
var supportedChannelTypes = []ChannelTypeInfo{
	{
		Type:        "telegram",
		DisplayName: "Telegram",
		Description: "通过 Telegram Bot API 接收用户消息，使用 long polling 方式。",
		Icon:        "telegram",
		Fields: []ChannelField{
			{
				Key:      "token",
				Label:    "Bot Token",
				Type:     "password",
				Required: true,
				HelpText: "从 @BotFather 获取的 Bot Token",
			},
			{
				Key:     "pollTimeout",
				Label:   "Long Polling 超时（秒）",
				Type:    "number",
				Default: "30",
			},
			{
				Key:      "apiBaseUrl",
				Label:    "API 反代地址",
				Type:     "string",
				HelpText: "用于无法直连 api.telegram.org 的场景，留空使用默认",
			},
			{
				Key:     "parseMode",
				Label:   "消息格式化模式",
				Type:    "select",
				Default: "",
				Options: []string{"", "HTML", "MarkdownV2"},
			},
			{
				Key:     "allowedUpdates",
				Label:   "接收的更新类型",
				Type:    "string",
				Default: "message,edited_message",
				HelpText: "逗号分隔，留空接收所有类型",
			},
		},
	},
	{
		Type:        "misskey",
		DisplayName: "Misskey",
		Description: "通过 Misskey WebSocket streaming 接收提及和回复消息。",
		Icon:        "misskey",
		Fields: []ChannelField{
			{
				Key:      "host",
				Label:    "实例 URL",
				Type:     "string",
				Required: true,
				HelpText: "如 https://misskey.io",
			},
			{
				Key:      "token",
				Label:    "API Token",
				Type:     "password",
				Required: true,
				HelpText: "Misskey API Token（含 WebSocket streaming 和 HTTP API 权限）",
			},
			{
				Key:     "subscribeTimeline",
				Label:   "订阅时间线",
				Type:    "boolean",
				Default: "false",
				HelpText: "启用后 Bot 会收到时间线上的所有帖子（不仅仅是 @提及）",
			},
		},
	},
}

// SupportedChannelTypes 返回所有支持的 Channel 类型信息。
func SupportedChannelTypes() []ChannelTypeInfo {
	return supportedChannelTypes
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
