package api

// ============================================================================
// Channel 类型注册表 — 描述每种 Channel 类型的配置字段 schema
//
// 驱动前端动态表单渲染：前端根据 Fields[] 自动生成配置表单。
// ============================================================================

// ChannelOption 描述一个带显示名的选项（用于 select / multiselect）。
type ChannelOption struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

// ChannelField 描述一个 Channel 配置字段。
type ChannelField struct {
	Key      string   `json:"key"`
	Label    string   `json:"label"`
	Type     string   `json:"type"` // "string"|"password"|"number"|"select"|"multiselect"|"boolean"
	Required bool     `json:"required"`
	Default  string   `json:"default,omitempty"`
	HelpText string   `json:"helpText,omitempty"`
	Options  []string `json:"options,omitempty"`
	// OptionItems 用于 select/multiselect 需要「显示名 ≠ 值」时；优先于 Options。
	OptionItems []ChannelOption `json:"optionItems,omitempty"`
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
		var opts []any
		if len(f.OptionItems) > 0 {
			for _, o := range f.OptionItems {
				opts = append(opts, o)
			}
		} else {
			for _, o := range f.Options {
				opts = append(opts, o)
			}
		}
		out[i] = PlatformField{
			Key:         f.Key,
			Label:       f.Label,
			Type:        f.Type,
			Help:        f.HelpText,
			Placeholder: f.Default,
			Optional:    !f.Required,
			Options:     opts,
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
			{
				Key:   "timelineChannels",
				Label: "订阅时间线",
				Type:  "multiselect",
				HelpText: "选择要旁听的时间线（可多选，留空则仅接收 @提及/回复）。" +
					"名称与 Misskey streaming channel 一致。",
				OptionItems: []ChannelOption{
					{Value: "homeTimeline", Label: "主页时间线"},
					{Value: "localTimeline", Label: "本地时间线"},
					{Value: "hybridTimeline", Label: "社交时间线"},
					{Value: "globalTimeline", Label: "全局时间线"},
				},
			},
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
