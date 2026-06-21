package tools

import (
	"crypto/md5"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"

	agenttools "github.com/kasuganosora/thinkbot/agent/tools"
	"github.com/kasuganosora/thinkbot/llm"
)

// ============================================================================
// 文本处理工具集
//   - text_hash: 计算哈希（MD5/SHA256）
//   - text_encode: Base64 编解码
//   - text_diff: 简单文本差异比较
//   - text_stats: 文本统计（行/词/字符数）
// ============================================================================

func textHashToolDef() agenttools.ToolDef {
	return agenttools.ToolDef{
		Tool: llm.Tool{
			Name:        "text_hash",
			Description: "计算文本的哈希值（支持 MD5 和 SHA256）。",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"text": map[string]any{
						"type":        "string",
						"description": "要计算哈希的文本",
					},
					"algorithm": map[string]any{
						"type":        "string",
						"description": "哈希算法：md5 或 sha256",
						"enum":        []string{"md5", "sha256"},
					},
				},
				"required": []string{"text"},
			},
			Execute: llm.ToolExecuteFunc(func(ctx *llm.ToolExecContext, input any) (any, error) {
				m, ok := input.(map[string]any)
				if !ok {
					return nil, fmt.Errorf("invalid input: expected object")
				}
				text, _ := m["text"].(string)
				if text == "" {
					return nil, fmt.Errorf("text is required")
				}
				algorithm, _ := m["algorithm"].(string)
				if algorithm == "" {
					algorithm = "sha256"
				}

				data := []byte(text)
				var hash string

				switch algorithm {
				case "md5":
					h := md5.Sum(data)
					hash = hex.EncodeToString(h[:])
				case "sha256":
					h := sha256.Sum256(data)
					hash = hex.EncodeToString(h[:])
				default:
					return nil, fmt.Errorf("unsupported algorithm: %s", algorithm)
				}

				return map[string]any{
					"algorithm": algorithm,
					"hash":      hash,
					"input_len": len(data),
				}, nil
			}),
		},
		Category: "utility",
	}
}

func textEncodeToolDef() agenttools.ToolDef {
	return agenttools.ToolDef{
		Tool: llm.Tool{
			Name:        "text_encode",
			Description: "Base64 编码或解码文本。",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"text": map[string]any{
						"type":        "string",
						"description": "要处理的文本",
					},
					"operation": map[string]any{
						"type":        "string",
						"description": "操作：encode（编码）或 decode（解码）",
						"enum":        []string{"encode", "decode"},
					},
				},
				"required": []string{"text", "operation"},
			},
			Execute: llm.ToolExecuteFunc(func(ctx *llm.ToolExecContext, input any) (any, error) {
				m, ok := input.(map[string]any)
				if !ok {
					return nil, fmt.Errorf("invalid input: expected object")
				}
				text, _ := m["text"].(string)
				if text == "" {
					return nil, fmt.Errorf("text is required")
				}
				operation, _ := m["operation"].(string)

				switch operation {
				case "encode":
					encoded := base64.StdEncoding.EncodeToString([]byte(text))
					return map[string]any{
						"operation": "encode",
						"input":     text,
						"result":    encoded,
					}, nil

				case "decode":
					decoded, err := base64.StdEncoding.DecodeString(text)
					if err != nil {
						return nil, fmt.Errorf("decode failed: %w", err)
					}
					return map[string]any{
						"operation": "decode",
						"input":     text,
						"result":    string(decoded),
					}, nil

				default:
					return nil, fmt.Errorf("unknown operation: %s", operation)
				}
			}),
		},
		Category: "utility",
	}
}

func textDiffToolDef() agenttools.ToolDef {
	return agenttools.ToolDef{
		Tool: llm.Tool{
			Name:        "text_diff",
			Description: "比较两段文本的差异，返回行级别的增删改。",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"text1": map[string]any{
						"type":        "string",
						"description": "第一段文本（原始）",
					},
					"text2": map[string]any{
						"type":        "string",
						"description": "第二段文本（修改后）",
					},
				},
				"required": []string{"text1", "text2"},
			},
			Execute: llm.ToolExecuteFunc(func(ctx *llm.ToolExecContext, input any) (any, error) {
				m, ok := input.(map[string]any)
				if !ok {
					return nil, fmt.Errorf("invalid input: expected object")
				}
				text1, _ := m["text1"].(string)
				text2, _ := m["text2"].(string)

				lines1 := strings.Split(text1, "\n")
				lines2 := strings.Split(text2, "\n")

				diff := simpleDiff(lines1, lines2)

				return map[string]any{
					"text1_lines":   len(lines1),
					"text2_lines":   len(lines2),
					"changed_lines": len(diff),
					"diff":          diff,
				}, nil
			}),
		},
		Category: "utility",
	}
}

func textStatsToolDef() agenttools.ToolDef {
	return agenttools.ToolDef{
		Tool: llm.Tool{
			Name:        "text_stats",
			Description: "统计文本的行数、词数、字符数等信息。",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"text": map[string]any{
						"type":        "string",
						"description": "要统计的文本",
					},
				},
				"required": []string{"text"},
			},
			Execute: llm.ToolExecuteFunc(func(ctx *llm.ToolExecContext, input any) (any, error) {
				m, ok := input.(map[string]any)
				if !ok {
					return nil, fmt.Errorf("invalid input: expected object")
				}
				text, _ := m["text"].(string)

				lines := strings.Count(text, "\n")
				if len(text) > 0 {
					lines++ // 最后一行
				}
				words := len(strings.Fields(text))
				chars := len(text)
				charsNoSpace := len(strings.ReplaceAll(text, " ", ""))
				paragraphs := 0
				for _, p := range strings.Split(text, "\n\n") {
					if strings.TrimSpace(p) != "" {
						paragraphs++
					}
				}

				return map[string]any{
					"lines":            lines,
					"words":            words,
					"characters":       chars,
					"chars_no_space":   charsNoSpace,
					"paragraphs":       paragraphs,
					"estimated_tokens": (words + chars/4) / 2,
				}, nil
			}),
		},
		Category: "utility",
	}
}

// simpleDiff 实现简单的行级 diff。
type diffEntry struct {
	Type    string `json:"type"`    // "add", "remove", "same"
	LineNum int    `json:"lineNum"` // 在 text2 中的行号
	Content string `json:"content"`
}

func simpleDiff(lines1, lines2 []string) []diffEntry {
	result := make([]diffEntry, 0)

	// 防止大文本导致内存爆炸（LCS 矩阵 O(n*m)）
	const maxDiffLines = 5000
	if len(lines1) > maxDiffLines || len(lines2) > maxDiffLines {
		return []diffEntry{{Type: "error", Content: "too many lines for diff (max 5000 per side)"}}
	}

	// 简单 LCS-based diff
	matrix := make([][]int, len(lines1)+1)
	for i := range matrix {
		matrix[i] = make([]int, len(lines2)+1)
	}

	for i := len(lines1) - 1; i >= 0; i-- {
		for j := len(lines2) - 1; j >= 0; j-- {
			if lines1[i] == lines2[j] {
				matrix[i][j] = matrix[i+1][j+1] + 1
			} else {
				matrix[i][j] = max2(matrix[i+1][j], matrix[i][j+1])
			}
		}
	}

	i, j := 0, 0
	for i < len(lines1) && j < len(lines2) {
		if lines1[i] == lines2[j] {
			result = append(result, diffEntry{Type: "same", LineNum: j + 1, Content: lines2[j]})
			i++
			j++
		} else if matrix[i+1][j] >= matrix[i][j+1] {
			result = append(result, diffEntry{Type: "remove", LineNum: i + 1, Content: lines1[i]})
			i++
		} else {
			result = append(result, diffEntry{Type: "add", LineNum: j + 1, Content: lines2[j]})
			j++
		}
	}

	for i < len(lines1) {
		result = append(result, diffEntry{Type: "remove", LineNum: i + 1, Content: lines1[i]})
		i++
	}
	for j < len(lines2) {
		result = append(result, diffEntry{Type: "add", LineNum: j + 1, Content: lines2[j]})
		j++
	}

	return result
}

func max2(a, b int) int {
	if a > b {
		return a
	}
	return b
}
