package strutil

import (
	"bytes"
	"strings"
)

// 凭据遮蔽的长度分档。
//
// 登记侧只校验非空（短口令 / PIN / 验证码同样可能是真实凭据），但遮蔽侧
// 不能对所有值做无条件子串替换：1~5 字节的值（"1"、"true"、"a"）在任意
// 文本里都是高频子串，替换会把整份日志或模型回复打码成占位符，系统反而
// 不可用。因此分三档：
//
//   - < MinMaskableSecretLen：仍可登记、仍能被解析还原，但不做出站/日志遮蔽
//     （登记方需给出 WARN，让运维知道这条防线覆盖不到）；
//   - MinMaskableSecretLen <= len < FullMaskSecretLen：短值，只在两侧都是
//     token 边界时替换，避免切断日期、编号、单词或字段名；
//   - >= FullMaskSecretLen：历史行为，全量子串替换（长 token 不会误伤）。
const (
	MinMaskableSecretLen = 6
	FullMaskSecretLen    = 12
)

// ReplaceSecretValue 把 text 中出现的真实凭据 value 替换成 repl。
// 返回的字符串可以直接出站；value 本身不会被回显。
func ReplaceSecretValue(text, value, repl string) string {
	if text == "" || value == "" || len(value) < MinMaskableSecretLen {
		return text
	}
	if !strings.Contains(text, value) {
		return text
	}
	if len(value) >= FullMaskSecretLen {
		return strings.ReplaceAll(text, value, repl)
	}
	return string(ReplaceSecretValueBytes([]byte(text), value, []byte(repl)))
}

// ReplaceSecretValueBytes 是 ReplaceSecretValue 的 []byte 版本，供日志写入
// 路径直接在编码后的 entry 上做遮蔽，避免额外的字符串转换。
// 无匹配时返回原切片（不复制）。
func ReplaceSecretValueBytes(p []byte, value string, repl []byte) []byte {
	if len(p) == 0 || value == "" || len(value) < MinMaskableSecretLen {
		return p
	}
	val := []byte(value)
	if !bytes.Contains(p, val) {
		return p
	}
	if len(value) >= FullMaskSecretLen {
		return bytes.ReplaceAll(p, val, repl)
	}

	var out []byte
	last, i := 0, 0
	for {
		j := bytes.Index(p[i:], val)
		if j < 0 {
			break
		}
		j += i
		end := j + len(val)
		if boundedSecretMatch(p, j, end, val) {
			if out == nil {
				out = make([]byte, 0, len(p))
			}
			out = append(out, p[last:j]...)
			out = append(out, repl...)
			last = end
		}
		i = end
	}
	if out == nil {
		return p
	}
	return append(out, p[last:]...)
}

// boundedSecretMatch 判断 [start,end) 处的短值是否是一个独立 token。
//
// 值的边缘若由字母/数字/_/-/. 构成，则相邻字节不能也是同类字符 —— 这样
// "123456" 不会把 "2026-123456-11" 里的日期段打码，"secret" 不会把
// "secret_name" 里的字段名打码。边缘是符号或 Unicode 时不额外限制相邻
// 字符，保证中文短口令等仍能被遮蔽。
func boundedSecretMatch(p []byte, start, end int, val []byte) bool {
	if isTokenByte(val[0]) && start > 0 && isTokenByte(p[start-1]) {
		return false
	}
	if isTokenByte(val[len(val)-1]) && end < len(p) && isTokenByte(p[end]) {
		return false
	}
	return true
}

func isTokenByte(b byte) bool {
	return b >= '0' && b <= '9' ||
		b >= 'a' && b <= 'z' ||
		b >= 'A' && b <= 'Z' ||
		b == '_' || b == '-' || b == '.'
}
