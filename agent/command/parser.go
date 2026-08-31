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
//   - 文本必须以 / 开头（忽略前导空格）
//   - 第一个 token 是命令名（去掉 /，转小写）
//   - 剩余部分是参数（trim 后）
//
// 示例：
//
//	"/clear"        → {Name: "clear", Args: ""}
//	"/compact 5"    → {Name: "compact", Args: "5"}
//	"/help"         → {Name: "help", Args: ""}
//	"hello /world"  → nil（不以 / 开头）
//	"/"             → nil（空命令名）
func Parse(text string) *ParsedCommand {
	text = strings.TrimSpace(text)
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
