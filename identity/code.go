package identity

import (
	"crypto/rand"
	"math/big"
	"regexp"
	"strings"
)

// ============================================================================
// 授权码生成与校验
//
// 授权码格式: TB-XXXX-XXXX
//   - 前缀 TB 固定
//   - X 取自安全字母表（排除 0/O/1/I/L 等易混淆字符）
//   - 总长度 12 字符（含分隔符），含 8 个随机字符
//   - 搜索空间: 29^8 ≈ 2×10^11，足够防爆破
// ============================================================================

const (
	// codePrefix 授权码固定前缀。
	codePrefix = "TB"

	// codeAlphabet 安全字母表（排除易混淆字符）。
	codeAlphabet = "23456789ABCDEFGHJKMNPQRSTVWXYZ"

	// codeRandomLen 随机字符总数（不含前缀和分隔符）。
	codeRandomLen = 8
)

// bindCodeRegex 匹配授权码格式的正则（大小写不敏感）。
var bindCodeRegex = regexp.MustCompile(`(?i)^TB-[A-Z2-9]{4}-[A-Z2-9]{4}$`)

// generateCode 生成一个新的授权码。
func generateCode() string {
	alphaLen := big.NewInt(int64(len(codeAlphabet)))
	b := make([]byte, codeRandomLen)
	for i := range b {
		n, err := rand.Int(rand.Reader, alphaLen)
		if err != nil {
			// crypto/rand 失败极不可能，fallback 到取模
			b[i] = codeAlphabet[0]
			continue
		}
		b[i] = codeAlphabet[n.Int64()]
	}
	// 格式为 TB-XXXX-XXXX
	return codePrefix + "-" + string(b[:4]) + "-" + string(b[4:])
}

// IsBindCode 判断文本是否匹配授权码格式。
// 自动 trim 前后空白，大小写不敏感。
func IsBindCode(text string) bool {
	text = strings.TrimSpace(text)
	return bindCodeRegex.MatchString(text)
}

// NormalizeCode 将授权码归一化为标准格式（大写 + 标准 dash）。
// 输入可能含有多余空格或小写字母。
func NormalizeCode(text string) string {
	text = strings.TrimSpace(text)
	text = strings.ToUpper(text)
	// 处理用户可能输入 tb.xxxx.xxxx 或 tb xxxx xxxx 的情况
	text = strings.ReplaceAll(text, " ", "-")
	text = strings.ReplaceAll(text, ".", "-")
	if !bindCodeRegex.MatchString(text) {
		return ""
	}
	return text
}

// extractPlatform 从 Source 字符串提取平台类型。
// Source 示例: "telegram-bot1"、"misskey-bot1"、"web-bot1"。
// 未知前缀时返回原值。
func extractPlatform(source string) string {
	for _, p := range []string{"telegram", "misskey", "discord", "slack", "web"} {
		if strings.HasPrefix(source, p) {
			return p
		}
	}
	return source
}
