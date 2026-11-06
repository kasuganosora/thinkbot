package memory

// DSML 工具调用 DSL 的出站清洗。
//
// 部分推理型模型在长工具轨迹、思考模式下，会把内部工具调用 DSL 当成普通文本
// 写进 content，而不是走结构化的 tool_calls（实测 DeepSeek 长轮次，正常请求返回的
// 是标准 delta.tool_calls）。它长这样：
//
//	<｜｜DSML｜｜ calls>
//	<｜｜DSML｜｜ invoke name="exec">
//	<｜｜DSML｜｜ parameter name="command" string="true">cd &1</｜｜DSML｜｜ parameter>
//	</｜｜DSML｜｜ invoke>
//	</｜｜DSML｜｜ calls>
//
// 这不是任何 wire protocol，项目也没有 DSML 解析器。标签以全角竖线开头，既不匹配
// 出站残留标签正则（要求 '<' 后紧跟 ASCII 字母），也不被裸思考检测命中，于是会：
//   - 作为流式 delta 实时推给用户；
//   - 经 reply-control 失败兜底变成正式回复发到渠道；
//   - 落进 chat_messages.content，后续作为历史再喂回模型，形成自我强化。
//
// 因此必须整块拒收，而不是只删标签（参数值如 "cd &1" 同样是内部内容）。
//
// 规则（出站清洗与流式过滤器共用，保证渠道发送 / 落库 / Web 流式三处一致）：
// 从第一个 DSML 开标签前缀起，其后内容一律丢弃。模型一旦开始吐工具调用 DSL，
// 后续输出已不可信，fail-closed 优于把内部协议残片拼进公开回复；开标签之前的
// 正常回复保留。只认固定前缀，不猜标签变体，避免误伤含 "DSML" 字样的普通文本。
var dsmlOpenPrefixes = []string{
	"<｜｜DSML｜｜", // 全角竖线（生产实测形态）
	"<||DSML||", // 半角竖线变体
}

// DSMLPrefixIndex 返回 s 中最早出现的 DSML 开标签前缀字节下标；没有则 -1。
// 用 hasASCIIPrefixFold 只折 ASCII 大小写：全角竖线是多字节序列，不受影响。
func DSMLPrefixIndex(s string) int {
	for i := 0; i < len(s); i++ {
		if s[i] != '<' {
			continue
		}
		for _, p := range dsmlOpenPrefixes {
			if hasASCIIPrefixFold(s[i:], p) {
				return i
			}
		}
	}
	return -1
}

// HasDSMLMark 报告文本里是否出现 DSML 开标签前缀。
func HasDSMLMark(s string) bool { return DSMLPrefixIndex(s) >= 0 }

// StripDSMLBlocks 去掉模型当成普通文本输出的工具调用 DSL 及其后所有内容。
// 未闭合/被截断的开标签同样按前缀命中（流式截断场景），不要求标签完整。
func StripDSMLBlocks(text string) string {
	if cut := DSMLPrefixIndex(text); cut >= 0 {
		return text[:cut]
	}
	return text
}

// DSMLOpenPrefixAt 供流式过滤器判定 s（以 '<' 开头的缓冲区）是否是 DSML 开标签：
//   - match：已凑齐固定前缀，n 是应消费的字节数，之后进入丢弃模式；
//   - incomplete：s 只是某个前缀的真前缀，非流尾时须扣住等后续 chunk；
//   - 都不是：交给调用方按普通标签逻辑处理。
//
// 刻意不要求 '>' 或标签名：前缀一旦齐备即可安全丢弃，避免参数属性（可能很长、
// 跨多个 chunk）把状态机拖进无界等待。
func DSMLOpenPrefixAt(s string) (n int, match bool, incomplete bool) {
	for _, p := range dsmlOpenPrefixes {
		if hasASCIIPrefixFold(s, p) {
			return len(p), true, false
		}
	}
	for _, p := range dsmlOpenPrefixes {
		if len(s) < len(p) && hasASCIIPrefixFold(p, s) {
			return 0, false, true
		}
	}
	return 0, false, false
}
