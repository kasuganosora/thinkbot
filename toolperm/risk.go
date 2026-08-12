// 工具风险分级。
//
// 为什么需要它：权限模型原本是「平台一旦有任何规则 → 该平台进入白名单模式，
// 未被规则命中的工具全部禁止」。这在实践中过于粗暴 —— 管理员通常只想禁掉
// 一两个危险工具（如 sandbox_exec），却会连带把 calculate / now / memory
// 这类无害的基础工具一并锁死，Bot 直接变成哑巴。
//
// 因此把工具分成三级：
//   - 基础工具（RiskBasic）：纯计算、文本处理、记忆读写、状态查询。
//     没有对外副作用，不联网、不执行代码、不写文件。**默认始终开放**，
//     不受「平台白名单模式」牵连。
//   - 敏感工具（RiskSensitive）：联网、执行命令、读写/删除文件、派生子智能体。
//     这些是权限管理的主要对象，沿用「平台有规则但未命中 → 禁止」。
//   - 对外发言工具（RiskBroadcast）：会在**站点/群里留下他人可见的痕迹**——
//     发帖、转发、表态（reaction）、关注、置顶、封禁、删他人消息。
//     这类工具的后果不可撤回（别人已经看到了），因此额外享有两条硬约束：
//     ① 即便是系统/内部会话（cron、心跳、梦境巩固，IsSystem=true）也**不自动放行**，
//     必须有显式 allow 规则 —— 防止定时任务在独立 session 里偷偷发帖；
//     ② 在UI 里单独成组高亮，方便配置「只看不发」的潜水 bot。
//
// 注意：分级只影响「没有任何规则命中时的默认值」。
// 管理员显式写的规则依然优先生效（首条匹配语义不变）——
// 想连基础工具也锁死，配一条 tool=* 的 deny 即可；
// 想让cron 定时发帖，配一条针对该工具的显式 allow 即可。
//
// 未收录的工具（MCP 动态工具、其它 Channel 专属工具等）一律按 **敏感** 处理，
// 遵循安全默认：不认识的能力不该自动开放。
package toolperm

import "strings"

// 工具风险级别。
const (
	// RiskBasic 基础工具：无对外副作用，默认始终开放。
	RiskBasic = "basic"
	// RiskSensitive 敏感工具：有实际危害面，受权限规则约束。
	RiskSensitive = "sensitive"
	// RiskBroadcast 对外发言工具：会产生他人可见且不可撤回的痕迹，
	// 受权限规则约束，且不被系统会话自动豁免。
	RiskBroadcast = "broadcast"
)

// basicTools 是明确判定为「无害基础能力」的工具白名单。
//
// 收录标准（必须全部满足）：
//  1. 不发起网络请求；
//  2. 不执行任意代码 / 命令；
//  3. 不写入、移动或删除文件；
//  4. 不派生新的执行体（子智能体 / 任务）；
//  5. 不产生对外可见的痕迹。
//
// 记忆类工具收录在内：记忆是 Bot 的会话上下文基础设施，禁掉它 Bot 会失忆，
// 而其数据始终限定在本 bot 自己的存储范围内，不构成对外危害。
var basicTools = map[string]struct{}{
	// 纯计算 / 生成
	"calculate":     {},
	"datetime_calc": {},
	"now":           {},
	"random":        {},
	"uuid":          {},

	// 文本处理（纯字符串运算，输入输出都在对话内）
	"text_diff":   {},
	"text_encode": {},
	"text_hash":   {},
	"text_stats":  {},

	// 记忆 / 会话上下文（Bot 的基础设施，作用域限本 bot）
	"memory":          {},
	"memory_snapshot": {},
	"memory_tools":    {},

	// 只读状态查询（不改变任何状态）
	"task_detail":    {},
	"sandbox_health": {},

	// Channel 只读查询：只是看，不留痕迹
	"misskey_search_user":              {},
	"misskey_list_following":           {},
	"misskey_get_user_notes":           {}, // 拉取某人帖子，只读
	"misskey_search_notes":            {}, // 按关键词搜帖子，只读
	"telegram_get_chat_info":           {},
	"telegram_get_chat_member_count":   {},
	"telegram_get_chat_administrators": {},
}

// broadcastTools 是「会产生他人可见痕迹」的工具集合。
//
// 收录标准：执行后，**本 bot 之外的人**能在站点/群里察觉到 —— 无论是一条帖子、
// 一个表情反应、一条关注通知，还是一条被删掉的消息。
// 这类动作发出即不可撤回（对方可能已经看到/收到通知），因此比普通敏感工具更严格。
//
// 注意 delete/unfollow/unreact/unban 这类「撤销型」动作也收录在内：
// 它们同样改变外部可见状态，且删除本身不可逆。
var broadcastTools = map[string]struct{}{
	// Misskey：发帖与转发（最直接的对外发言）
	"misskey_create_note":   {},
	"misskey_create_renote": {},
	// Misskey：删帖（不可逆地改变已公开内容）
	"misskey_delete_note": {},
	// Misskey：表态与社交关系（会给对方推送通知）
	"misskey_react_to_note":   {},
	"misskey_unreact_to_note": {},
	"misskey_follow_user":     {},
	"misskey_unfollow_user":   {},

	// Telegram：群内可见操作
	"telegram_pin_message":    {}, // 默认通知全群
	"telegram_delete_message": {}, // 删他人消息
	"telegram_ban_member":     {}, // 封禁，可连带清空该用户历史消息
	"telegram_unban_member":   {},
}

// broadcastPrefixes 兜住尚未逐个收录的 Channel 写操作工具。
//
// 命中这些前缀且**不在 basicTools 只读白名单里**的工具，一律按对外发言处理。
// 这样将来给 Channel 加新工具（如 misskey_quote_note）时，默认就是最严级别，
// 不会因为忘记登记而悄悄获得发言能力。
var broadcastPrefixes = []string{
	"misskey_",
	"telegram_",
}

// sensitivePrefixes 是敏感工具的名称前缀。
// 用于兜住尚未逐个收录的同族工具（如未来新增的 sandbox_xxx / web_xxx），
// 确保新工具默认落到「敏感」而非被误判为基础工具。
var sensitivePrefixes = []string{
	"sandbox_", // 沙箱：执行命令、读写文件
	"web_",     // 联网：搜索、抓取
	"http_",    // 联网
	"shell",    // 执行命令
	"exec",     // 执行命令
	"mcp_",     // 外部 MCP 工具，能力不可知
	"misskey_", // Channel 写操作（只读项已在 basicTools 中显式例外）
	"telegram_",
}

// sensitivePrefixExceptions 是「命中敏感前缀但实际无害」的显式例外。
// 这些工具虽然名字带敏感前缀，但确实只读、不留痕迹。
var sensitivePrefixExceptions = map[string]struct{}{
	"sandbox_health":                   {}, // 纯健康探测
	"misskey_search_user":              {}, // 只读查询
	"misskey_list_following":           {},
	"misskey_get_user_notes":           {}, // 只读查询
	"misskey_search_notes":            {}, // 只读查询
	"telegram_get_chat_info":           {},
	"telegram_get_chat_member_count":   {},
	"telegram_get_chat_administrators": {},
}

// ToolRisk 返回工具的风险级别（basic / sensitive / broadcast）。
// 未收录的工具一律视为敏感（安全默认）。
func ToolRisk(name string) string {
	if IsBroadcastTool(name) {
		return RiskBroadcast
	}
	if IsBasicTool(name) {
		return RiskBasic
	}
	return RiskSensitive
}

// IsBroadcastTool 判断工具是否会产生「他人可见的对外痕迹」。
//
// 判定顺序：显式集合 → 前缀兜底（排除只读例外）。
// 只读例外必须先排除，否则 misskey_search_user 这类纯查询会被误判为发言。
func IsBroadcastTool(name string) bool {
	lower := strings.ToLower(strings.TrimSpace(name))
	if lower == "" {
		return false
	}
	if _, ok := broadcastTools[lower]; ok {
		return true
	}
	// 只读例外不算对外发言
	if _, isReadOnly := sensitivePrefixExceptions[lower]; isReadOnly {
		return false
	}
	for _, p := range broadcastPrefixes {
		if strings.HasPrefix(lower, p) {
			return true
		}
	}
	return false
}

// IsBasicTool 判断工具是否属于「默认始终开放」的基础工具。
//
// 判定顺序很重要：先排除对外发言工具，再看敏感前缀（除显式例外），
// 最后查基础白名单。前缀规则作为兜底护栏 —— 即便某个 sandbox_/web_/misskey_
// 工具被误加进 basicTools，也不会被放行；宁可误判为敏感，也不误判为无害。
func IsBasicTool(name string) bool {
	lower := strings.ToLower(strings.TrimSpace(name))
	if lower == "" {
		return false
	}
	if IsBroadcastTool(lower) {
		return false
	}
	if _, isException := sensitivePrefixExceptions[lower]; !isException {
		for _, p := range sensitivePrefixes {
			if strings.HasPrefix(lower, p) {
				return false
			}
		}
	}
	_, ok := basicTools[lower]
	return ok
}
