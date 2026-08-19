package dao

import "time"

// BotBrowserCookie 存储某 bot 浏览器的 cookie（账号凭据级，等同于账号本体）。
//
// 权威格式对齐 Playwright storageState：domain / name / value / path / expires /
// httpOnly / secure / sameSite。运行时由浏览器封装层读取并注入（addCookies），
// 会话结束再导出回写（见 docs/sandbox-browser-image-design.md §10）。
//
// 唯一性：(bot_id, domain, name, path) 唯一——这正是浏览器判定「同一个 cookie」的键。
// 有了 DB 层约束，回收/导入可用原子 upsert（clause.OnConflict）替代「先查后改」，
// 既消除并发竞态，也杜绝同一 cookie 产生多行（多行会被 addCookies 注入成相互覆盖
// 的重复项，登录态表现为随机失效）。
//
// 安全约束（与现有 provider apikey 一致——当前无凭据加密工具）：
//   - value 明文存储，列表接口默认掩码，单条 reveal 才返回完整值；
//   - 任何日志路径都不得记录 value（仅记 domain + 条数）；
//   - 绝不向 agent 暴露读 cookie 的工具（cookie 值不能进 prompt / L0 记忆 / 日志）；
//   - 删除 bot 时须同步清表（见 docs §6#14 / §8#12），否则“删了 bot”≠“删了账号凭据”。
type BotBrowserCookie struct {
	// ID 主键（带 bc- 前缀）。
	ID string `gorm:"primaryKey;size:36" json:"id"`

	// BotID 所属 Bot ID。
	BotID string `gorm:"size:64;index;not null;uniqueIndex:uk_bot_cookie" json:"botId"`

	// Domain cookie 所属域（如 .x.com / www.xiaohongshu.com）。
	Domain string `gorm:"size:255;index;not null;default:'';uniqueIndex:uk_bot_cookie" json:"domain"`

	// Name cookie 名。
	Name string `gorm:"size:255;not null;default:'';uniqueIndex:uk_bot_cookie" json:"name"`

	// Value cookie 值（凭据本体）。json:"-" 使其永不进入列表/默认序列化。
	Value string `gorm:"type:text;not null" json:"-"`

	// Path cookie 路径，默认 "/"。
	Path string `gorm:"size:255;not null;default:'/';uniqueIndex:uk_bot_cookie" json:"path"`

	// Expires 过期时间（epoch 秒）；0 表示会话级（session cookie）。
	Expires int64 `gorm:"not null;default:0" json:"expires"`

	// HTTPOnly 是否禁止 JS 访问。
	HTTPOnly bool `gorm:"not null;default:false" json:"httpOnly"`

	// Secure 是否仅 HTTPS 传输。
	Secure bool `gorm:"not null;default:false" json:"secure"`

	// SameSite Strict / Lax / None / 空。
	SameSite string `gorm:"size:16;not null;default:''" json:"sameSite"`

	// CreatedAt 创建时间。
	CreatedAt time.Time `gorm:"autoCreateTime" json:"createdAt"`

	// UpdatedAt 更新时间。
	UpdatedAt time.Time `gorm:"autoUpdateTime" json:"updatedAt"`
}

// TableName 指定 GORM 表名。
func (BotBrowserCookie) TableName() string { return "bot_browser_cookies" }

// BrowserCookieView 是返回给前端的 cookie 视图（value 视场景掩码或完整）。
type BrowserCookieView struct {
	ID        string `json:"id"`
	BotID     string `json:"botId"`
	Domain    string `json:"domain"`
	Name      string `json:"name"`
	Value     string `json:"value"` // 列表时掩码，reveal 时完整
	Path      string `json:"path"`
	Expires   int64  `json:"expires"`
	HTTPOnly  bool   `json:"httpOnly"`
	Secure    bool   `json:"secure"`
	SameSite  string `json:"sameSite"`
	CreatedAt string `json:"createdAt"`
	UpdatedAt string `json:"updatedAt"`
}

// ToView 把模型转换为视图，masked=true 时对 value 做掩码。
func (c BotBrowserCookie) ToView(masked bool) BrowserCookieView {
	v := c.Value
	if masked {
		v = maskCookieValue(c.Value)
	}
	return BrowserCookieView{
		ID:        c.ID,
		BotID:     c.BotID,
		Domain:    c.Domain,
		Name:      c.Name,
		Value:     v,
		Path:      c.Path,
		Expires:   c.Expires,
		HTTPOnly:  c.HTTPOnly,
		Secure:    c.Secure,
		SameSite:  c.SameSite,
		CreatedAt: c.CreatedAt.UTC().Format(time.RFC3339),
		UpdatedAt: c.UpdatedAt.UTC().Format(time.RFC3339),
	}
}

// maskCookieValue 对 cookie 值做掩码：保留首尾各 1 字符，中间用 *** 替代。
func maskCookieValue(v string) string {
	r := []rune(v)
	switch len(r) {
	case 0:
		return ""
	case 1:
		return "***"
	case 2:
		return string(r[0]) + "***"
	default:
		return string(r[0]) + "***" + string(r[len(r)-1])
	}
}
