// 渠道出站（对外发言）权限。
//
// 背景：Bot 有两条对外发言路径，它们的管控点完全不同：
//
//	① LLM 主动调用工具发帖 —— misskey_create_note 等，走 ToolManager，受本模块工具规则约束；
//	② Pipeline 自动回复 —— ActionReply → Channel.Send，**完全不经过工具层**。
//
// 只禁工具只能挡住 ①。想做「只看不发」的潜水 bot（被@ 也不回），
// 必须同时管住 ②，即在出站链路拦截。本文件提供 ② 的判定实现。
//
// 配置载体：复用同一张 bot_tool_permissions 表，用**保留工具名**表达出站权限，
// 这样管理员在同一个「工具权限」界面即可完成配置，无需新表、新接口、新 UI。
package toolperm

import (
	"context"
	"strings"

	"github.com/kasuganosora/thinkbot/agent/core"
)

// OutboundReplyTool 是表示「渠道自动回复能力」的保留工具名。
//
// 它不是一个真实的 LLM 工具，而是把「出站发言」建模成一种可授权的能力，
// 从而复用整套规则语义（platform / userIds / decision / sort / 通配）。
// 加"__" 前缀有两重作用：
//   - 与真实工具名空间隔离，避免与将来某个真工具重名；
//   - `presentToolList` 已过滤 "__" 前缀，因此不会混进工具选择列表
//     （出站开关在 UI 上单独呈现，而非伪装成一个工具）。
const OutboundReplyTool = "__outbound_reply"

// AllowOutbound 判定某bot 在指定渠道类型下是否允许对外发送。
//
// 语义与工具规则保持一致，但**默认值相反地保守设计为「放行」**：
// 出站回复是 Bot 的核心功能，绝不能因为管理员配了一条无关的工具规则
// （例如禁用 sandbox_exec）就意外变成哑巴。因此只有存在**显式命中**
// 的 deny 规则时才拦截。
//
// 判定过程：按 sort 升序找第一条匹配 (__outbound_reply, platform, user) 的规则；
// 命中则按其 decision，未命中则放行。
func (s *Service) AllowOutbound(botID, platform, userID string) bool {
	rules, err := s.evalRules(botID)
	if err != nil {
		// 评估失败时放行：此处与工具评估的「保守拒绝」相反 ——
		// 让 bot 因为数据库抖动而集体失声，比偶发多发一条消息更糟。
		s.logger.Warnw("evaluate outbound perm failed, allowing", "bot", botID, "err", err)
		return true
	}
	for _, r := range rules {
		// 仅 __outbound_reply 规则参与出站判定。
		// 注意这里要求精确匹配而非通配：tool=* 的规则是给真实工具用的，
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

// SetReadOnly 是配置「渠道只读」的便捷方法：
// readOnly=true 时写入/更新一条 deny 规则，false 时删除该规则。
//
// 供 API 层的开关式接口调用，避免前端自己拼保留工具名。
func (s *Service) SetReadOnly(botID, platform string, readOnly bool) error {
	platform = strings.TrimSpace(platform)
	if platform == "" {
		platform = "*"
	}
	rules, err := s.ListRules(botID)
	if err != nil {
		return err
	}
	var existing *RuleDTO
	for i := range rules {
		if strings.EqualFold(rules[i].Tool, OutboundReplyTool) && rules[i].Platform == platform {
			existing = &rules[i]
			break
		}
	}

	if !readOnly {
		if existing == nil {
			return nil // 本来就没有限制
		}
		return s.DeleteRule(botID, existing.ID)
	}

	enabled := true
	if existing != nil {
		_, err = s.UpdateRule(botID, existing.ID, RuleReq{
			Decision: DecisionDeny,
			Enabled:  &enabled,
		})
		return err
	}
	// 只读规则排在最前（sort=-100）：它与工具规则互不干扰（工具名不同），
	// 但靠前可以保证语义直观，且不受后续工具规则重排影响。
	sortVal := -100
	_, err = s.CreateRule(botID, RuleReq{
		Tool:     OutboundReplyTool,
		Platform: platform,
		UserIDs:  []string{"*"},
		Decision: DecisionDeny,
		Enabled:  &enabled,
		Sort:     &sortVal,
	})
	return err
}

// IsReadOnly 返回某 bot 在指定渠道类型下是否处于只读（潜水）状态。
func (s *Service) IsReadOnly(botID, platform string) bool {
	return !s.AllowOutbound(botID, platform, "*")
}

// ----------------------------------------------------------------------------
// OutboundGuard 适配
// ----------------------------------------------------------------------------

// NewOutboundGuard 返回一个实现 agent/outbound.OutboundGuard 的守卫，
// 供 ChannelReplyHandler 在发送前检查渠道是否只读。
//
// resolvePlatform 把 Channel 名称（如 "my-misskey-bot"）映射为渠道类型
// （如 "misskey"）—— 权限规则按类型配置，而 Action 只携带 Channel 名称。
// 传nil 时退化为直接用 channelName 当平台匹配。
func (s *Service) NewOutboundGuard(botID string, resolvePlatform func(channelName string) string) *outboundGuard {
	return &outboundGuard{svc: s, botID: botID, resolve: resolvePlatform}
}

type outboundGuard struct {
	svc     *Service
	botID   string
	resolve func(channelName string) string
}

// AllowOutbound 实现 outbound.OutboundGuard。
func (g *outboundGuard) AllowOutbound(_ context.Context, channelName string, action core.Action) bool {
	platform := channelName
	if g.resolve != nil {
		if p := g.resolve(channelName); p != "" {
			platform = p
		}
	}
	return g.svc.AllowOutbound(g.botID, platform, action.UserID)
}
