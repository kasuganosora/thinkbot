package api

import (
	"context"
	"fmt"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/kasuganosora/thinkbot/agent/bot"
	"github.com/kasuganosora/thinkbot/agent/core"
	"github.com/kasuganosora/thinkbot/agent/heartbeat"
	"github.com/kasuganosora/thinkbot/dao"
	"github.com/kasuganosora/thinkbot/util/errs"
)

// ============================================================================
// Bot 心跳管理 Handler
//
// 每个 Bot 独立管理心跳配置和日志，存储在 data/heartbeat/{botId}/。
// heartbeatStore 已迁移为 Server 字段，由 NewServer 注入，与 NewBundle 共享同一数据目录。
//
// 路由：
//   GET    /api/bots/:id/heartbeat       → 获取心跳配置
//   PUT    /api/bots/:id/heartbeat       → 更新心跳配置
//   GET    /api/bots/:id/heartbeat/logs  → 查询心跳日志
//   DELETE /api/bots/:id/heartbeat/logs  → 清空心跳日志
// ============================================================================

// newHeartbeatAdmissionFn 构造心跳准入关卡的信号探测函数（设计文档 §5.5）。
//
// 判定「自上次唤醒以来外界是否发生了值得 bot 重新思考的事」，纯 SQL 计数，
// 不消耗 LLM。无信号 → 心跳被拒绝为 0-step turn（仍落日志）。
//
// 信号源（均为确定性数据，不含推测）：
//  1. chat_messages：Web 聊天新消息（该表按 bot_id 精确归属）。
//  2. memory_entries：新落库记忆条目——覆盖非 Web 渠道（如 Misskey）的入站，
//     因为 note_capture 会把入站原文写成 L0 记忆。
//
// 注意：memory_entries 表无 bot_id 列（作用域为 scope_kind/scope_id），因此该计数
// 在多 bot 共库部署下可能被其他 bot 的记忆写入触发。后果仅是「多醒一次」，
// 方向上是安全的（宁可多醒，不可漏醒）；探测出错时同样保守放行。
func (s *BotService) newHeartbeatAdmissionFn(botID string) heartbeat.AdmissionFn {
	if s.db == nil {
		return nil
	}
	return func(ctx context.Context, since time.Time) (bool, string) {
		if since.IsZero() {
			return true, "进程启动后首次唤醒"
		}

		var msgCount int64
		if err := s.db.WithContext(ctx).Model(&dao.ChatMessage{}).
			Where("bot_id = ? AND created_at > ?", botID, since).
			Count(&msgCount).Error; err != nil {
			s.logger.Warnw("heartbeat admission: count chat_messages failed",
				"bot_id", botID, "err", err)
			return true, "信号探测失败，保守放行"
		}
		if msgCount > 0 {
			return true, fmt.Sprintf("新增聊天消息 %d 条", msgCount)
		}

		var memCount int64
		if err := s.db.WithContext(ctx).Model(&dao.EntryModel{}).
			Where("created_at > ?", since).
			Count(&memCount).Error; err != nil {
			s.logger.Warnw("heartbeat admission: count memory_entries failed",
				"bot_id", botID, "err", err)
			return true, "信号探测失败，保守放行"
		}
		if memCount > 0 {
			return true, fmt.Sprintf("新增记忆条目 %d 条", memCount)
		}

		return false, ""
	}
}

// heartbeatChannelLister 枚举本 bot 当前可主动发帖的真实渠道/会话，
// 作为心跳 LLM 决策的可选目标。nil/空表示心跳只能 silent 或内部 note。
func (s *BotService) heartbeatChannelLister(botID string) heartbeat.ChannelLister {
	return func(ctx context.Context) ([]heartbeat.ChannelTarget, error) {
		b := s.botInstances[botID]
		if b == nil {
			return nil, nil
		}
		var targets []heartbeat.ChannelTarget
		// 按发言模式过滤：仅 active 平台允许心跳主动发帖；
		// passive（仅被动回复）/ mute（潜水）都禁止心跳在该渠道主动发声。
		allowed := func(t heartbeat.ChannelTarget) bool {
			if s.permSvc == nil {
				return true
			}
			return s.permSvc.AllowProactivePost(botID, t.Type)
		}
		for _, ch := range b.Channels() {
			switch ch.Type() {
			case "misskey":
				// Misskey 时间线（顶层新帖）；回复某条具体帖子暂不在 v1 枚举范围。
				targets = append(targets, heartbeat.ChannelTarget{
					Channel: ch.Name(),
					Type:    "misskey",
					Label:   "Misskey 时间线（发新帖）",
				})
			case "telegram":
				// 近期聊过的 Telegram 会话：bot 才能在「憋了一天的群」里主动说点什么。
				if rcl, ok := ch.(core.RecentChatLister); ok {
					for _, rc := range rcl.RecentChats() {
						label := rc.Title
						if label == "" {
							label = fmt.Sprintf("Telegram 聊天 %d", rc.ID)
						}
						targets = append(targets, heartbeat.ChannelTarget{
							Channel:        ch.Name(),
							Type:           "telegram",
							ConversationID: fmt.Sprintf("%d", rc.ID),
							Label:          "Telegram: " + label,
						})
					}
				}
			// web 等无对外主动发帖语义，跳过。
			default:
				// 其它类型暂不枚举。
			}
		}
		// 剔除 passive/mute 平台：心跳不在这些渠道主动发帖（仅 active 允许）。
		filtered := targets[:0]
		for _, t := range targets {
			if allowed(t) {
				filtered = append(filtered, t)
			}
		}
		return filtered, nil
	}
}

// heartbeatChannelPoster 把心跳决策的内容发到选定的真实渠道。
//   - Misskey 顶层新帖 → core.TimelinePoster（PostTimeline）
//   - Misskey 回复 / Telegram → 构造 ActionReply 经该渠道 Sender.Send 出站
//
// 决不走伪频道 "heartbeat" 的 dispatcher（杜绝 Bug 2：no sender for channel heartbeat）。
func (s *BotService) heartbeatChannelPoster(botID string) heartbeat.ChannelPoster {
	return func(ctx context.Context, target heartbeat.ChannelTarget, content string) error {
		b := s.botInstances[botID]
		if b == nil {
			return fmt.Errorf("heartbeat poster: bot %q not found", botID)
		}
		var chosen bot.Channel
		for _, ch := range b.Channels() {
			if ch.Name() == target.Channel {
				chosen = ch
				break
			}
		}
		if chosen == nil {
			return fmt.Errorf("heartbeat poster: channel %q not found", target.Channel)
		}
		sender, ok := chosen.(bot.Sender)
		if !ok {
			return fmt.Errorf("heartbeat poster: channel %q is not a sender", target.Channel)
		}
		switch target.Type {
		case "misskey":
			if target.ConversationID == "" {
				// 顶层新帖：走 TimelinePoster（createNoteFull(text, "", ...)）。
				tp, ok := chosen.(core.TimelinePoster)
				if !ok {
					return fmt.Errorf("heartbeat poster: channel %q 不支持时间线发帖", target.Channel)
				}
				_, err := tp.PostTimeline(ctx, content, "home", "")
				return err
			}
			// 回复某条具体帖子。
			return sender.Send(ctx, core.Action{
				Type:    core.ActionReply,
				Channel: target.ConversationID,
				Payload: content,
				Metadata: map[string]any{
					"source_channel": target.Channel,
					"visibility":     "home",
				},
			})
		case "telegram":
			return sender.Send(ctx, core.Action{
				Type:    core.ActionReply,
				Channel: target.ConversationID,
				Payload: content,
				Metadata: map[string]any{
					"source_channel": target.Channel,
				},
			})
		default:
			return fmt.Errorf("heartbeat poster: 不支持的渠道类型 %q", target.Type)
		}
	}
}

// heartbeatNoteSaver 把心跳决策的内部笔记写入本 bot 长期记忆（复用 bot.SaveNote → NoteHandler 链路）。
func (s *BotService) heartbeatNoteSaver(botID string) heartbeat.NoteSaver {
	return func(ctx context.Context, content string) error {
		b := s.botInstances[botID]
		if b == nil {
			return fmt.Errorf("heartbeat note saver: bot %q not found", botID)
		}
		return b.SaveNote(ctx, content)
	}
}

// heartbeatStoreOf 取 BotService 持有的心跳存储；botSvc 缺失时退化为独立实例
// （仅出现在不启动 bot 的测试场景）。
func heartbeatStoreOf(botSvc *BotService) *heartbeat.Store {
	if st := botSvc.HeartbeatStore(); st != nil {
		return st
	}
	return heartbeat.NewStore("data/heartbeat")
}

// handleGetHeartbeatConfig 获取 Bot 心跳配置。
func (s *Server) handleGetHeartbeatConfig(c *gin.Context) {
	botID := c.Param("id")

	cfg, err := s.heartbeatStore.LoadConfig(botID)
	if err != nil {
		Fail(c, errs.Wrap(err, "load heartbeat config"))
		return
	}
	if cfg == nil {
		// 首次访问，返回默认配置
		def := heartbeat.DefaultConfig()
		cfg = &def
	}
	normalizeHeartbeatConfig(cfg)
	OK(c, cfg)
}

// normalizeHeartbeatConfig 补齐历史配置缺失的新字段，避免前端拿到 0 值当真。
func normalizeHeartbeatConfig(cfg *heartbeat.Config) {
	def := heartbeat.DefaultConfig()
	if cfg.Interval <= 0 {
		cfg.Interval = def.Interval
	}
	if cfg.MaxConsecutiveWakes <= 0 {
		cfg.MaxConsecutiveWakes = def.MaxConsecutiveWakes
	}
	if cfg.IdleWakeEvery <= 0 {
		cfg.IdleWakeEvery = def.IdleWakeEvery
	}
	if cfg.CooldownMin < 0 {
		cfg.CooldownMin = 0
	}
}

// handleUpdateHeartbeatConfig 更新 Bot 心跳配置。
func (s *Server) handleUpdateHeartbeatConfig(c *gin.Context) {
	botID := c.Param("id")

	var req struct {
		Enabled             *bool `json:"enabled"`
		Interval            *int  `json:"interval"`
		AllowPost           *bool `json:"allow_post"`
		MaxConsecutiveWakes *int  `json:"max_consecutive_wakes"`
		CooldownMin         *int  `json:"cooldown_min"`
		IdleWakeEvery       *int  `json:"idle_wake_every"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		Fail(c, errs.BadRequest("invalid request body"))
		return
	}

	// 加载现有配置
	cfg, err := s.heartbeatStore.LoadConfig(botID)
	if err != nil {
		Fail(c, errs.Wrap(err, "load heartbeat config"))
		return
	}
	if cfg == nil {
		def := heartbeat.DefaultConfig()
		cfg = &def
	}
	normalizeHeartbeatConfig(cfg)

	// 部分更新
	if req.Enabled != nil {
		cfg.Enabled = *req.Enabled
	}
	if req.Interval != nil {
		cfg.Interval = clampInt(*req.Interval, 1, 1440)
	}
	if req.AllowPost != nil {
		cfg.AllowPost = *req.AllowPost
	}
	if req.MaxConsecutiveWakes != nil {
		cfg.MaxConsecutiveWakes = clampInt(*req.MaxConsecutiveWakes, 1, 100)
	}
	if req.CooldownMin != nil {
		cfg.CooldownMin = clampInt(*req.CooldownMin, 0, 1440)
	}
	if req.IdleWakeEvery != nil {
		cfg.IdleWakeEvery = clampInt(*req.IdleWakeEvery, 1, 100)
	}

	// 保存
	if err := s.heartbeatStore.SaveConfig(botID, cfg); err != nil {
		Fail(c, errs.Wrap(err, "save heartbeat config"))
		return
	}

	auditLog(c, s.logger, "update_heartbeat_config", "bot_id", botID,
		"enabled", cfg.Enabled, "interval", cfg.Interval,
		"allow_post", cfg.AllowPost,
		"max_consecutive_wakes", cfg.MaxConsecutiveWakes,
		"cooldown_min", cfg.CooldownMin,
		"idle_wake_every", cfg.IdleWakeEvery)
	OK(c, cfg)
}

// clampInt 把值限制到 [min, max]。
func clampInt(v, min, max int) int {
	if v < min {
		return min
	}
	if v > max {
		return max
	}
	return v
}

// handleListHeartbeatLogs 查询心跳日志。
// Query params: status=all|acted|note|silent|suppressed|error
// （旧的 normal/alert 语义已随心跳重设计废弃，见 docs/heartbeat-redesign.md）
func (s *Server) handleListHeartbeatLogs(c *gin.Context) {
	botID := c.Param("id")
	status := c.DefaultQuery("status", "all")

	logStore, err := s.heartbeatStore.LoadLogs(botID)
	if err != nil {
		Fail(c, errs.Wrap(err, "load heartbeat logs"))
		return
	}

	allLogs := logStore.Logs
	totalHistory := len(allLogs) // 历史总数（滚动窗口内）

	logs := allLogs
	if status != "" && status != "all" {
		filtered := make([]heartbeat.Log, 0, len(allLogs))
		for _, l := range allLogs {
			if l.Status == status {
				filtered = append(filtered, l)
			}
		}
		logs = filtered
	}

	OK(c, gin.H{
		"logs":  logs,
		"total": totalHistory, // 历史总数
		"count": len(logs),    // 当前过滤后条数
	})
}

// handleClearHeartbeatLogs 清空心跳日志。
func (s *Server) handleClearHeartbeatLogs(c *gin.Context) {
	botID := c.Param("id")

	if err := s.heartbeatStore.ClearLogs(botID); err != nil {
		Fail(c, errs.Wrap(err, "clear heartbeat logs"))
		return
	}

	auditLog(c, s.logger, "clear_heartbeat_logs", "bot_id", botID)
	OK(c, nil)
}
