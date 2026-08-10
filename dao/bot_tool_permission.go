package dao

import "time"

// BotToolPermission 是 bot 维度的工具权限规则。
//
// 每条规则描述：在某个平台（platform）、对某个/某些用户（user_ids），
// 开放或禁止（decision）某个工具（tool）。
//
// tool / platform / user_ids 三者均支持 "*" 通配（匹配所有）。
//   - tool="*"：匹配全部工具
//   - platform="*"：匹配全部平台
//   - user_ids 含 "*"：匹配全部用户
//
// 评估语义（与 BotAccess 的「首条匹配生效」一致）：
//   - 仅 enabled=true 的规则参与评估
//   - 按 sort 升序遍历，第一个匹配全部维度的规则决定最终决策
//   - 无规则命中时按「该平台是否已有启用规则」决定默认值：
//     平台完全没有启用规则 → 默认「允许」（开放基线，未被约束的渠道不锁死）；
//     平台已有规则但都未命中当前 (tool,user) → 默认「禁止」（安全默认）
//
// web 平台另有一条 tool=* platform=web user_ids=["*"] decision=allow 的显式基线规则，
// 由权限服务在 bot 尚无覆盖 web 的规则时惰性播种（见 toolperm.Service.SeedWebDefault），
// 主要用于 UI 展示与「恢复默认」；语义上 web 即便无此规则也因「无规则→允许」而放行。
type BotToolPermission struct {
	// ID 主键（带 tp- 前缀）。
	ID string `gorm:"primaryKey;size:36" json:"id"`

	// BotID 所属 Bot ID。
	BotID string `gorm:"size:64;index;not null" json:"botId"`

	// Tool 工具名匹配模式；"*" 或空表示匹配全部工具，支持 * 通配（如 "sandbox_*"）。
	Tool string `gorm:"size:128;not null;default:'*'" json:"tool"`

	// Platform 平台类型（渠道类型：web / telegram / misskey / ...）；"*" 或空表示全部平台。
	Platform string `gorm:"size:32;not null;default:'*'" json:"platform"`

	// UserIDs 用户 ID 列表的 JSON 数组字符串；含 "*" 表示全部用户。
	UserIDs string `gorm:"type:text;not null;default:'[\"*\"]'" json:"-"`

	// Decision 决策：allow（开放）或 deny（禁止）。
	Decision string `gorm:"size:8;not null;default:'allow'" json:"decision"`

	// Enabled 是否启用；false 时该规则不参与评估。
	// 注意：不要加 default 标签——GORM 会把零值(false)视为「未设置」而套用 DB 默认值，
	// 导致显式禁用(false)被悄悄改成启用(true)。所有写路径都在代码里显式赋值。
	Enabled bool `gorm:"not null" json:"enabled"`

	// Sort 排序权重；升序遍历，数字越小越先评估（首条匹配生效）。
	Sort int `gorm:"default:0;index" json:"sort"`

	// CreatedAt 创建时间。
	CreatedAt time.Time `gorm:"autoCreateTime" json:"createdAt"`

	// UpdatedAt 更新时间。
	UpdatedAt time.Time `gorm:"autoUpdateTime" json:"updatedAt"`
}

// TableName 指定 GORM 表名。
func (BotToolPermission) TableName() string { return "bot_tool_permissions" }
