package tools

import "strings"

// socialIntentKeywords 命中即判定为「社交操作意图」，社交写工具因此暴露。
//
// 设计取舍：仅列明确的社交动作，未命中则视为未知意图——社交写工具默认隐藏，
// 从架构上根绝跑偏（宁可少数模糊社交意图被临时隐藏，也不让代码任务误暴露写工具）。
// 单字与词组混合：单字（关注/赞）覆盖「关注X」「赞一个」等高频简写；
// 词组（发帖/发动态/点赞）避免单字「发」「赞」在日常非社交语境的误判。
//
// 注意：英文关键词做了小写化，匹配前会将输入统一 ToLower。
var socialIntentKeywords = []string{
	// 关注 / 取关
	"关注", "取关", "取消关注", "拉黑", "取消拉黑",
	"follow", "unfollow", "followuser", "follow user",
	// 发帖 / 动态 / 笔记 / 转发
	"发帖", "发一条", "发个", "发动态", "发笔记", "发条", "发布", "发表",
	"发微博", "发推", "转发", "转推",
	"post", "publish", "repost", "renote", "boost",
	// 点赞 / 表情
	"点赞", "点个赞", "赞一个", "加表情", "表情",
	"react", "like",
	// 提及 / 私信 / @
	"提及", "私信", "艾特", "发消息给", "提到",
	"mention", "dm", "@",
	// 回复并发布（带内容的主动发帖）
	"回复并", "引用回复", "写一条", "写个动态", "写条",
}

// ClassifyTaskIntent 根据本轮用户消息文本判定任务意图。
//
// 返回 TaskIntentSocial 表示明确社交操作（关注/发帖/点赞/提及等），
// 社交写工具（设了 "social" scope 的）因此暴露；
// 其余（含未知/模糊/非社交意图）返回空串，社交写工具默认隐藏。
//
// 调用点：envelopeToSessionContext（每轮消息构建工具会话上下文时）。
func ClassifyTaskIntent(text string) string {
	if strings.TrimSpace(text) == "" {
		return ""
	}
	lower := strings.ToLower(text)
	for _, kw := range socialIntentKeywords {
		if strings.Contains(lower, strings.ToLower(kw)) {
			return TaskIntentSocial
		}
	}
	return ""
}
