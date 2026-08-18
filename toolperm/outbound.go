// 渠道出站（对外发言）权限 + 三态「发言模式」。
//
// 背景：Bot 的「对外发言」有三条路径，管控点完全不同：
//
//	① LLM 主动调用工具发帖 —— misskey_create_note 等，走 ToolManager，受本模块工具规则约束；
//	② Pipeline 自动回复   —— ActionReply → Channel.Send，完全不经过工具层；
//	③ 心跳自主发帖         —— 经 ChannelPoster 直接发，既不走工具层也不走 __outbound_reply 守卫。
//
// 「发言模式」(SpeakMode) 把这三种意图统一成一个开关，复用同一张 bot_tool_permissions 表：
//   - active  （可发言）：默认，主动发帖（① ③）与被 @ 被动回复（②）都允许。
//   - passive （仅被动回复）：被 @ 才回（② 放行），但禁掉 ① 主动发文工具，且 ③ 心跳不主动发帖。
//   - mute    （潜水/只看不发）：被 @ 也不回（② 经 __outbound_reply deny 拦截），① ③ 同样禁。
//
// 配置载体：复用 bot_tool_permissions 表，不新表、不新接口。
//   - mute   写一条 __outbound_reply deny 规则（auto 维护）；
//   - passive 写一条 __speak_mode:passive 标记规则（auto，用于持久化状态）
//             + 对该平台每个主动发布类工具模式写一条 deny 规则（auto，用于拦截 ①）。
//   切换模式时先整体删除该平台下所有 auto 规则再重建，因此绝不误删用户在「规则列表」手动配的规则。
package toolperm

import (
	"context"
	"fmt"
	"strings"

	"github.com/kasuganosora/thinkbot/agent/core"
)

// 发言模式枚举。
const (
	// ModeActive 可发言：允许主动发帖（工具/心跳）与被 @ 被动回复。
	ModeActive = "active"
	// ModePassive 仅被动回复：被 @ 才回；禁主动发帖工具，心跳不在该平台主动发帖。
	ModePassive = "passive"
	// ModeMute 潜水/只看不发：被 @ 也不回（出站拦截），且无任何对外发言。
	ModeMute = "mute"
)

// OutboundReplyTool 表示「渠道自动回复能力」的保留工具名。
//
// 它不是真实 LLM 工具，而是把「被动出站回复」建模成一种可授权能力，
// 从而复用整套规则语义（platform / userIds / decision / sort / 通配）。
// 加 "__" 前缀有两重作用：
//   - 与真实工具名空间隔离，避免与将来某个真工具重名；
//   - presentToolList 已过滤 "__" 前缀，因此不会混进工具选择列表
//     （出站开关在 UI 上单独呈现，而非伪装成一个工具）。
const OutboundReplyTool = "__outbound_reply"

// speakModePassiveMarker 是 passive 模式的持久化标记规则（带 "__" 前缀，
// 不出现在工具选择器，也不被 AllowOutbound/Evaluate 误匹配）。
// 它的存在（auto=true 且启用）即表示「该平台处于 passive」。
// decision 取 allow，纯占位 —— 它不参与任何工具/出站评估，仅作为状态标记。
const speakModePassiveMarker = "__speak_mode:passive"

// AllowOutbound 判定某 bot 在指定渠道类型下是否允许被动对外回复（路径 ②）。
//
// 语义与工具规则一致，但默认放行：只有显式命中的 __outbound_reply deny 才拦截。
// 注意：本函数只管「被动回复」是否被潜水拦截，不管「主动发帖」是否被 passive 模式禁掉——
// 主动发帖走工具层（Evaluate）与心跳过滤（SpeakMode/AllowProactivePost），与这里无关。
func (s *Service) AllowOutbound(botID, platform, userID string) bool {
	rules, err := s.evalRules(botID)
	if err != nil {
		s.logger.Warnw("evaluate outbound perm failed, allowing", "bot", botID, "err", err)
		return true
	}
	for _, r := range rules {
		// 仅 __outbound_reply 规则参与出站判定。
		// 要求精确匹配而非通配：tool=* 的规则是给真实工具用的，
		// 若让它也命中出站，管理员配一条 "禁止全部工具" 就会连带禁掉回复，
		// 与「显式配置才拦截」的设计相悖。
		if !strings.EqualFold(r.Tool, OutboundReplyTool) {
			continue
		}
		if !matchPlatform(r.Platform, platform) {
			continue
		}
		if !matchUser(r.UserIDs, userID) {
			continue
		}
		return r.Decision == DecisionAllow
	}
	return true // 无显式配置 → 允许发言
}

// SpeakMode 返回某 bot 在指定渠道类型下的发言模式（active/passive/mute）。
// 判定优先级：mute（__outbound_reply deny）> passive（auto 的 __speak_mode:passive 标记）> active。
func (s *Service) SpeakMode(botID, platform string) string {
	rules, err := s.ListRules(botID)
	if err != nil {
		s.logger.Warnw("load rules for speak mode failed, default active", "bot", botID, "err", err)
		return ModeActive
	}
	for _, r := range rules {
		if r.Enabled && strings.EqualFold(r.Tool, OutboundReplyTool) && matchPlatform(r.Platform, platform) {
			return ModeMute
		}
	}
	if hasAutoPassiveMarker(rules, platform) {
		return ModePassive
	}
	return ModeActive
}

// hasAutoPassiveMarker 判断规则集中是否存在覆盖该平台的、auto 维护的 passive 标记规则。
func hasAutoPassiveMarker(rules []RuleDTO, platform string) bool {
	for _, r := range rules {
		if !r.Enabled || !r.Auto {
			continue
		}
		if !strings.EqualFold(r.Tool, speakModePassiveMarker) {
			continue
		}
		if !matchPlatform(r.Platform, platform) {
			continue
		}
		return true
	}
	return false
}

// ProactivePostToolPatterns 返回某平台「主动发布」类工具的通配模式。
// 这些是 LLM 可主动调用以发布新内容的渠道工具；passive 模式需禁掉它们，
// 但被动回复（被 @ 后 ActionReply 出站）不受影响 —— 它不走工具层。
// 平台无主动发布工具（telegram / web）返回 nil：其「不主动发帖」仅靠心跳过滤（见 SpeakMode/AllowProactivePost）。
func ProactivePostToolPatterns(platform string) []string {
	switch strings.ToLower(strings.TrimSpace(platform)) {
	case "misskey", "*":
		return []string{"misskey_create_*"}
	default:
		return nil
	}
}

// AllowProactivePost 返回某 bot 在指定平台是否允许「主动发帖」（心跳/工具主动发文）。
// 仅 active 模式允许；passive 与 mute 均禁止主动发帖，只保留（或被拦的）被动回复。
// 供心跳 ChannelLister 过滤发帖目标使用。
func (s *Service) AllowProactivePost(botID, platform string) bool {
	return s.SpeakMode(botID, platform) == ModeActive
}

// SetSpeakMode 设置某渠道的发言模式（active/passive/mute）。
// 实现：先删除该平台下所有 auto 维护规则，再按模式重建。
//   - mute：写一条 __outbound_reply deny（auto）。
//   - passive：写一条 __speak_mode:passive 标记（auto）+ 对该平台每个主动发布工具模式写 deny（auto）。
//   - active：仅清理 auto 规则（恢复主动 + 被动都允许）。
func (s *Service) SetSpeakMode(botID, platform, mode string) error {
	platform = strings.TrimSpace(platform)
	if platform == "" {
		platform = "*"
	}
	mode = strings.ToLower(strings.TrimSpace(mode))
	switch mode {
	case ModeActive, ModePassive, ModeMute:
		// 合法
	default:
		return fmt.Errorf("invalid speak mode: %q (want active|passive|mute)", mode)
	}

	// 先清理该平台下所有 auto 规则（含旧 passive 标记、旧主动发布 deny、旧 __outbound_reply auto）。
	// matchPlatform 保证 platform="*" 能清理具体平台的 auto 规则，反之亦然。
	if err := s.deleteAutoRules(botID, platform); err != nil {
		return err
	}
	switch mode {
	case ModeMute:
		if err := s.upsertOutboundDeny(botID, platform); err != nil {
			return err
		}
	case ModePassive:
		if err := s.createPassiveMarker(botID, platform); err != nil {
			return err
		}
		for _, pat := range ProactivePostToolPatterns(platform) {
			if err := s.createAutoDeny(botID, pat, platform); err != nil {
				return err
			}
		}
	}
	return nil
}

// deleteAutoRules 删除某 bot 下覆盖该平台的所有 auto 维护规则。
func (s *Service) deleteAutoRules(botID, platform string) error {
	rules, err := s.ListRules(botID)
	if err != nil {
		return err
	}
	for _, r := range rules {
		if !r.Auto {
			continue
		}
		if !matchPlatform(r.Platform, platform) {
			continue
		}
		if err := s.DeleteRule(botID, r.ID); err != nil {
			return err
		}
	}
	return nil
}

// createAutoDeny 创建一条 auto 维护的 deny 规则（用于 passive 拦截主动发布工具）。
func (s *Service) createAutoDeny(botID, tool, platform string) error {
	enabled := true
	sortVal := -50 // 主动发布拦截排在出站 deny(-100) 之后、用户规则之前，语义直观
	_, err := s.CreateRule(botID, RuleReq{
		Tool:     tool,
		Platform: platform,
		UserIDs:  []string{"*"},
		Decision: DecisionDeny,
		Enabled:  &enabled,
		Sort:     &sortVal,
		Auto:     &enabled,
	})
	return err
}

// createPassiveMarker 创建 passive 模式的持久化标记规则（auto，decision=allow 占位）。
func (s *Service) createPassiveMarker(botID, platform string) error {
	enabled := true
	sortVal := -50
	_, err := s.CreateRule(botID, RuleReq{
		Tool:     speakModePassiveMarker,
		Platform: platform,
		UserIDs:  []string{"*"},
		Decision: DecisionAllow,
		Enabled:  &enabled,
		Sort:     &sortVal,
		Auto:     &enabled,
	})
	return err
}

// upsertOutboundDeny 确保存在一条覆盖该平台的 __outbound_reply deny（auto）。
// 调用方已先删该平台全部 auto 规则，故此处直接新建即可。
func (s *Service) upsertOutboundDeny(botID, platform string) error {
	enabled := true
	sortVal := -100 // 与工具规则互不干扰（工具名不同），靠前保证语义直观
	_, err := s.CreateRule(botID, RuleReq{
		Tool:     OutboundReplyTool,
		Platform: platform,
		UserIDs:  []string{"*"},
		Decision: DecisionDeny,
		Enabled:  &enabled,
		Sort:     &sortVal,
		Auto:     &enabled,
	})
	return err
}

// IsReadOnly 返回某 bot 在指定渠道类型下是否处于只读（潜水/只看不发）状态。
// 等价于 SpeakMode == mute。保留以兼容既有调用方（pipeline / engagement / 出站守卫）。
func (s *Service) IsReadOnly(botID, platform string) bool {
	return s.SpeakMode(botID, platform) == ModeMute
}

// SetReadOnly 是「渠道只读」便捷方法的历史兼容封装：
// true → mute（潜水），false → active（恢复发言）。
// 新代码应直接调用 SetSpeakMode 以使用三态。
func (s *Service) SetReadOnly(botID, platform string, readOnly bool) error {
	if readOnly {
		return s.SetSpeakMode(botID, platform, ModeMute)
	}
	return s.SetSpeakMode(botID, platform, ModeActive)
}

// ----------------------------------------------------------------------------
// OutboundGuard 适配
// ----------------------------------------------------------------------------

// NewOutboundGuard 返回一个实现 agent/outbound.OutboundGuard 的守卫，
// 供 ChannelReplyHandler 在发送前检查渠道是否只读（被动回复是否被潜水拦截）。
//
// resolvePlatform 把 Channel 名称（如 "my-misskey-bot"）映射为渠道类型
// （如 "misskey"）—— 权限规则按类型配置，而 Action 只携带 Channel 名称。
// 传 nil 时退化为直接用 channelName 当平台匹配。
func (s *Service) NewOutboundGuard(botID string, resolvePlatform func(channelName string) string) *outboundGuard {
	return &outboundGuard{svc: s, botID: botID, resolve: resolvePlatform}
}

type outboundGuard struct {
	svc     *Service
	botID   string
	resolve func(channelName string) string
}

// AllowOutbound 实现 outbound.OutboundGuard（路径 ② 的被动回复拦截）。
// 注意：本守卫只判断「被动回复」是否被潜水拦截（mute），不判断主动发帖是否被 passive 禁掉——
// 主动发帖的拦截分别在工具层（Evaluate 命中 misskey_create_* deny）与心跳层（AllowProactivePost）。
func (g *outboundGuard) AllowOutbound(_ context.Context, channelName string, action core.Action) bool {
	platform := channelName
	if g.resolve != nil {
		if p := g.resolve(channelName); p != "" {
			platform = p
		}
	}
	return g.svc.AllowOutbound(g.botID, platform, action.UserID)
}
