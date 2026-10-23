package command

import "strings"

// ParsedCommand 是从消息文本中解析出的命令。
type ParsedCommand struct {
	// Name 命令名（不含 /，已转小写）。
	Name string
	// Args 命令参数（已 trim，不含命令名本身）。
	Args string
}

// Parse 从消息文本中解析斜杠命令。
// 如果文本不是以 / 开头的命令，返回 nil。
//
// 解析规则：
//   - 开头允许有 @提及（群聊里命令几乎总是写成 "@bot /cmd"）
//   - 剥掉 @提及后文本必须以 / 开头（忽略前导空格）
//   - 第一个 token 是命令名（去掉 /，转小写）
//   - 剩余部分是参数（trim 后）
//
// 示例：
//
//	"/clear"            → {Name: "clear", Args: ""}
//	"/compact 5"        → {Name: "compact", Args: "5"}
//	"/help"             → {Name: "help", Args: ""}
//	"@mybot /chatid"    → {Name: "chatid", Args: ""}
//	"hello /world"      → nil（不以 / 开头）
//	"@mybot 你好"       → nil（剥完 @ 不是命令）
//	"/"                 → nil（空命令名）
func Parse(text string) *ParsedCommand {
	text = skipLeadingMention(strings.TrimSpace(text))
	if text == "" || text[0] != '/' {
		return nil
	}

	body := text[1:] // 去掉 /
	if body == "" {
		return nil
	}

	// 分割命令名和参数
	parts := strings.SplitN(body, " ", 2)
	name := strings.ToLower(strings.TrimSpace(parts[0]))
	if name == "" {
		return nil
	}

	args := ""
	if len(parts) > 1 {
		args = strings.TrimSpace(parts[1])
	}

	return &ParsedCommand{
		Name: name,
		Args: args,
	}
}

// skipLeadingMention 跳过文本开头连续的 @提及（如企微群里的 "@打工仔 "）。
//
// 为什么命令解析要自己剥一遍：剥 @ 前缀本来是各 channel 的职责
// （如 channel/wecom 的 stripMention），但它依赖配置里的机器人显示名与实际
// 群昵称完全一致。一旦机器人在平台侧改名（或客户端渲染成全角 ＠），
// 就会漏剥，文本变成 "@打工仔 /chatid" —— 命令识别全线失效，
// 且失败是静默的（消息被当成普通聊天喂给 LLM，bot 回一句"我不认识"）。
// 实测 2026-10-23 线上就是这么坏的。
//
// 只处理**开头**的 @token，正文中间的 @ 原样保留，避免误伤。
func skipLeadingMention(text string) string {
	for strings.HasPrefix(text, "@") || strings.HasPrefix(text, "\uFF20") {
		// @token 到第一个空白为止（与 channel/wecom.stripMention 的口径一致）；
		// 紧贴着的 / 也算 token 结束（"@bot/chatid" 这种没有空格的写法）。
		i := strings.IndexAny(text, " \t\u00a0/")
		if i < 0 {
			// 整串就是一个 @token，没有正文。
			return ""
		}
		if text[i] == '/' {
			return text[i:]
		}
		text = strings.TrimSpace(text[i:])
	}
	return text
}
