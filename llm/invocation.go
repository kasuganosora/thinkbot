package llm

import (
	"crypto/rand"
	"fmt"
	"time"
)

// newInvocationID 生成一个 UUID v4 字符串，用作一次工具执行的唯一标识
// （invocationID）。它与模型下发的 ToolCallID 相互独立：模型可能因多步循环
// 复用或不可控地生成 ToolCallID，而 invocationID 由服务端在每次「实际执行」
// 时生成，可稳定地用于日志追踪与前端区分「来自哪次调用」。
//
// 使用 crypto/rand，不依赖任何第三方库。
func newInvocationID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// 极小概率：rand 失败兜底用时间戳，仍保证单次执行内唯一
		return fmt.Sprintf("invk-%d", time.Now().UnixNano())
	}
	// 设置 version(4) 与 variant(10xx)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
