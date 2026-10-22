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
		Type:        "mastodon",
		DisplayName: "Mastodon",
		Description: "通过 Mastodon WebSocket streaming 接收提及、回复与收藏通知。",
		Icon:        "mastodon",
		Color:       "#6364ff",
		Fields: []ChannelField{
			{Key: "server", Label: "实例 URL", Type: "string", Required: true, HelpText: "如 https://mastodon.social"},
			{Key: "token", Label: "Access Token", Type: "password", Required: true, HelpText: "Mastodon OAuth access token（需 read/write 权限）"},
			{
				Key:   "streams",
				Label: "订阅时间线",
				Type:  "multiselect",
				HelpText: "选择要旁听的时间线（可多选，留空则仅接收 @提及/回复/私信）。" +
					"通知流始终订阅，不受此配置影响。",
				OptionItems: []ChannelOption{
					{Value: "user", Label: "关注时间线"},
					{Value: "public:local", Label: "本站时间线"},
					{Value: "public", Label: "联邦时间线"},
					{Value: "public:remote", Label: "远端时间线"},
					{Value: "direct", Label: "私信"},
				},
			},
			{
				Key:     "resolveReplyContext",
				Label:   "解析被回复内容",
				Type:    "boolean",
				Default: "true",
				HelpText: "开启后入站回复会额外拉取父帖，让 Bot 看到对方在回复什么。" +
					"关闭可节省 API 配额（Mastodon 限流 300 次/5 分钟）。",
			},
		},
	},
	{
		Type:        "wecom",
		DisplayName: "企业微信",
		Description: "通过企业微信智能机器人长连接接收单聊与群聊消息，无需公网 IP 或回调地址。",
		Icon:        "wecom",
		Color:       "#0079f2",
		Fields: []ChannelField{
			{Key: "robotId", Label: "BotID", Type: "string", Required: true, HelpText: "企业微信后台「智能机器人」页的 BotID（aib_ 前缀）"},
			{Key: "secret", Label: "Secret", Type: "password", Required: true, HelpText: "长连接专用 Secret（与 URL 回调模式的 Token/EncodingAESKey 不同）"},
			{
				Key:   "botName",
				Label: "机器人在群里的显示名",
				Type:  "string",
				HelpText: "如 mybot。回调帧里只有 aibotid、**没有显示名**，不配置就只能靠通用规则剥离 @ 前缀，" +
					"可能把 @别人 误判成 @本机器人。",
			},
			{Key: "endpoint", Label: "长连接地址", Type: "string", HelpText: "留空用官方默认地址；非官方地址需同时开启下方开关"},
			{Key: "allowCustomEndpoint", Label: "允许非官方长连接地址", Type: "boolean", HelpText: "默认关闭。开启后 Secret 会发往自定义地址，仅私有部署使用"},
			{
				Key:   "captionWindowMs",
				Label: "配文合并窗口（毫秒）",
				Type:  "number",
				// ⚠️ 默认 3000 不是随便取的：企微发文件时把配文拆成另一条回调，
				// 两条间隔实测 0.3~2.5s。留空/0 走同一默认值；填 -1 关闭合并
				// （回复零延迟，但「文字+文件」会变成两条回复）。
				Default:  "3000",
				HelpText: "发文件时配文会被拆成另一条回调，本窗口用来把它们合并成一条。默认 3000；填 -1 关闭合并（零延迟，但会双回复）",
			},
			{
				Key:   "streamReply",
				Label: "流式回复",
				Type:  "boolean",
				// 默认开：这是长连接模式的核心能力，实测 15/15 步全成功，
				// 且任何一步失败都会自动回退成整段发送，不会丢消息。
				Default:  "true",
				HelpText: "收到消息先显示「正在思考…」，出结果后原位替换成最终答案。关闭则等结果出来后直接发一条整段消息",
			},
			{
				Key:   "placeholderRefreshMs",
				Label: "占位刷新间隔（毫秒）",
				Type:  "number",
				// 一轮生成实测 17~55s，占位推出来后一动不动会被当成卡死。
				// 每 5s 刷一次并带上已用秒数（实测连推 13 次零失败，不占 30 条/分钟配额）。
				Default:  "5000",
				HelpText: "「正在思考…」的刷新间隔，每次会带上已用秒数，用来证明还在跑。默认 5000；填 -1 关闭刷新",
			},
			{
				Key:   "interruptPolicy",
				Label: "忙时新消息的处理",
				Type:  "string",
				// 默认 restart：聊天里连发两句通常是想另起话题，
				// 把两轮内容混在一条回复里比丢掉补充更难受。
				// 无论哪种，「停止」这类终止指令都只打断、不开新轮。
				Default:  "restart",
				HelpText: "restart（默认）= 打断旧回复、就新消息另起一轮；append = 把新消息塞进正在跑的那一轮继续跑",
			},
			{
				Key:   "stopCommands",
				Label: "额外终止指令",
				Type:  "string",
				// ⚠️ 整句完全匹配才生效，避免用户一开口带「停」字就被掐断。
				HelpText: "在内置词表（停止/停下/取消/终止/打住/stop/cancel 等）之外追加。多条用逗号分隔，必须整句完全相同才生效",
			},
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
