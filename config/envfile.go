package config

import (
	"bufio"
	"github.com/kasuganosora/thinkbot/util/errs"
	"os"
	"strings"
)

// ParseEnvFile 解析 .env 文件内容，返回 key→value 映射。
// 支持的语法：
//   - KEY=VALUE
//   - export KEY=VALUE  （可选 export 前缀）
//   - KEY="quoted value"  （双引号，支持转义）
//   - KEY='quoted value'  （单引号，不转义）
//   - # 注释行
//   - 空行跳过
//
// 不支持多行值、变量插值（${VAR}）等高级特性。
func ParseEnvFile(content string) map[string]string {
	result := make(map[string]string)
	scanner := bufio.NewScanner(strings.NewReader(content))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		// 去除可选 export 前缀
		line = strings.TrimPrefix(line, "export ")
		line = strings.TrimPrefix(line, "export\t")

		// 分割 KEY=VALUE
		key, rawVal, found := strings.Cut(line, "=")
		if !found {
			continue
		}
		key = strings.TrimSpace(key)
		rawVal = strings.TrimSpace(rawVal)
		if key == "" {
			continue
		}

		// 去除注释（仅当不在引号内时）
		val := stripInlineComment(rawVal)

		// 去除引号
		val = unquoteValue(val)

		result[key] = val
	}
	return result
}

// stripInlineComment 去除行内注释（# 后面的内容）。
// 仅当 # 不在引号内时才视为注释。
func stripInlineComment(s string) string {
	inSingle := false
	inDouble := false
	for i := 0; i < len(s); i++ {
		ch := s[i]
		switch ch {
		case '\'':
			if !inDouble {
				inSingle = !inSingle
			}
		case '"':
			if !inSingle {
				inDouble = !inDouble
			}
		case '#':
			if !inSingle && !inDouble {
				// 注释前保留空白
				return strings.TrimRight(s[:i], " \t")
			}
		}
	}
	return s
}

// unquoteValue 去除值的引号包裹。
func unquoteValue(s string) string {
	if len(s) < 2 {
		return s
	}
	// 双引号：处理转义
	if s[0] == '"' && s[len(s)-1] == '"' {
		inner := s[1 : len(s)-1]
		// 处理常见转义序列
		inner = strings.NewReplacer(
			`\"`, `"`,
			`\\`, `\`,
			`\n`, "\n",
			`\t`, "\t",
		).Replace(inner)
		return inner
	}
	// 单引号：原样保留内容
	if s[0] == '\'' && s[len(s)-1] == '\'' {
		return s[1 : len(s)-1]
	}
	return s
}

// LoadEnvFile 从文件路径加载 .env 文件。
// 文件不存在时返回 nil error 和空 map（静默跳过）。
func LoadEnvFile(path string) (map[string]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, errs.Wrapf(err, "config: read .env file %q", path)
	}
	return ParseEnvFile(string(data)), nil
}
