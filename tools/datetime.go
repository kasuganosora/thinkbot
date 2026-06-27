package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	agenttools "github.com/kasuganosora/thinkbot/agent/tools"
	"github.com/kasuganosora/thinkbot/llm"
)

// ============================================================================
// datetime_calc — 日期时间计算工具
//
// 提供：
//   - 日期加减（天/周/月）
//   - 日期差计算
//   - 星期几查询
//   - 格式转换
// ============================================================================

func datetimeCalcToolDef() agenttools.ToolDef {
	return agenttools.ToolDef{
		Tool: llm.Tool{
			Name: "datetime_calc",
			Description: "日期时间计算工具。支持日期加减、日期差计算、星期查询、格式转换。" +
				"输入日期格式：YYYY-MM-DD 或 YYYY-MM-DD HH:MM:SS。",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"operation": map[string]any{
						"type":        "string",
						"description": "操作类型：add（日期加减）、diff（日期差）、weekday（星期几）、format（格式转换）",
						"enum":        []string{"add", "diff", "weekday", "format"},
					},
					"date": map[string]any{
						"type":        "string",
						"description": "基准日期，格式 YYYY-MM-DD 或 YYYY-MM-DD HH:MM:SS。留空使用当前时间。",
					},
					"value": map[string]any{
						"type":        "integer",
						"description": "加减的值（operation=add 时使用，正数加负数减）",
					},
					"unit": map[string]any{
						"type":        "string",
						"description": "加减的单位：days、weeks、months、years、hours、minutes",
						"enum":        []string{"days", "weeks", "months", "years", "hours", "minutes"},
					},
					"date2": map[string]any{
						"type":        "string",
						"description": "第二个日期（operation=diff 时使用）",
					},
					"format": map[string]any{
						"type":        "string",
						"description": "目标格式（operation=format 时使用），如 2006-01-02、01/02/2006、Jan 2, 2006",
					},
				},
				"required": []string{"operation"},
			},
			Execute: llm.ToolExecuteFunc(func(ctx *llm.ToolExecContext, input any) (any, error) {
				m, ok := input.(map[string]any)
				if !ok {
					return nil, fmt.Errorf("invalid input: expected object")
				}
				operation, _ := m["operation"].(string)

				// 解析日期
				dateStr, _ := m["date"].(string)
				baseTime, err := parseDateTime(dateStr)
				if err != nil {
					return nil, fmt.Errorf("invalid date format: %w", err)
				}

				switch operation {
				case "add":
					value, ok := toIntSearch(m["value"])
					if !ok {
						return nil, fmt.Errorf("value is required for add operation")
					}
					unit, _ := m["unit"].(string)
					result := addDuration(baseTime, value, unit)
					return map[string]any{
						"operation": "add",
						"original":  baseTime.Format("2006-01-02 15:04:05"),
						"result":    result.Format("2006-01-02 15:04:05"),
						"weekday":   result.Weekday().String(),
					}, nil

				case "diff":
					date2Str, _ := m["date2"].(string)
					if date2Str == "" {
						return nil, fmt.Errorf("date2 is required for diff operation")
					}
					time2, err := parseDateTime(date2Str)
					if err != nil {
						return nil, fmt.Errorf("invalid date2 format: %w", err)
					}
					diff := baseTime.Sub(time2)
					hours := int(diff.Hours())
					days := hours / 24
					return map[string]any{
						"operation":     "diff",
						"date1":         baseTime.Format("2006-01-02 15:04:05"),
						"date2":         time2.Format("2006-01-02 15:04:05"),
						"days":          days,
						"hours":         hours,
						"minutes":       int(diff.Minutes()),
						"absolute_days": absInt(days),
					}, nil

				case "weekday":
					return map[string]any{
						"operation":    "weekday",
						"date":         baseTime.Format("2006-01-02"),
						"weekday":      baseTime.Weekday().String(),
						"weekday_cn":   weekdayCN(baseTime.Weekday()),
						"week_of_year": weekOfYear(baseTime),
					}, nil

				case "format":
					formatStr, _ := m["format"].(string)
					if formatStr == "" {
						formatStr = "2006-01-02"
					}
					return map[string]any{
						"operation": "format",
						"original":  baseTime.Format(time.RFC3339),
						"result":    baseTime.Format(formatStr),
						"format":    formatStr,
					}, nil

				default:
					return nil, fmt.Errorf("unknown operation: %s", operation)
				}
			}),
		},
		Category: "utility",
	}
}

func parseDateTime(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Now(), nil
	}

	// 尝试常见格式
	formats := []string{
		"2006-01-02 15:04:05",
		"2006-01-02T15:04:05Z",
		"2006-01-02T15:04:05",
		"2006-01-02 15:04",
		"2006-01-02",
		"2006/01/02",
		"01/02/2006",
		"Jan 2, 2006",
		time.RFC3339,
	}

	for _, f := range formats {
		if t, err := time.Parse(f, s); err == nil {
			return t, nil
		}
	}

	return time.Time{}, fmt.Errorf("unrecognized date format: %s", s)
}

func addDuration(t time.Time, value int, unit string) time.Time {
	switch unit {
	case "days":
		return t.AddDate(0, 0, value)
	case "weeks":
		return t.AddDate(0, 0, value*7)
	case "months":
		return t.AddDate(0, value, 0)
	case "years":
		return t.AddDate(value, 0, 0)
	case "hours":
		return t.Add(time.Duration(value) * time.Hour)
	case "minutes":
		return t.Add(time.Duration(value) * time.Minute)
	default:
		return t.AddDate(0, 0, value) // 默认天
	}
}

func weekdayCN(w time.Weekday) string {
	names := []string{"星期日", "星期一", "星期二", "星期三", "星期四", "星期五", "星期六"}
	return names[w]
}

func weekOfYear(t time.Time) int {
	_, week := t.ISOWeek()
	return week
}

func absInt(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

// ============================================================================
// list_files — 通用文件系统列表工具（非沙箱）
// ============================================================================

func listFilesToolDef() agenttools.ToolDef {
	return agenttools.ToolDef{
		Tool: llm.Tool{
			Name: "list_files",
			Description: "列出指定目录下的文件和子目录。" +
				"返回名称、大小、修改时间等信息。" +
				"注意：这是读取宿主文件系统的只读操作，不修改任何文件。",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path": map[string]any{
						"type":        "string",
						"description": "目录路径。留空使用当前目录。",
					},
					"recursive": map[string]any{
						"type":        "boolean",
						"description": "是否递归列出子目录。默认 false。",
					},
				},
			},
			Execute: llm.ToolExecuteFunc(func(ctx *llm.ToolExecContext, input any) (any, error) {
				m, ok := input.(map[string]any)
				if !ok {
					return nil, fmt.Errorf("invalid input: expected object")
				}
				path, _ := m["path"].(string)
				if path == "" {
					path = "."
				}
				recursive := false
				if v, ok := m["recursive"].(bool); ok {
					recursive = v
				}

				entries, err := listFiles(path, recursive, 3)
				if err != nil {
					return nil, err
				}

				return map[string]any{
					"path":    path,
					"entries": entries,
					"count":   len(entries),
				}, nil
			}),
		},
		Category: "filesystem",
	}
}

func listFiles(path string, recursive bool, maxDepth int) ([]map[string]any, error) {
	return listFilesInternal(path, recursive, 0, maxDepth)
}

func listFilesInternal(path string, recursive bool, depth, maxDepth int) ([]map[string]any, error) {
	entries, err := readDirEntries(path)
	if err != nil {
		return nil, err
	}

	result := make([]map[string]any, 0, len(entries))
	for _, entry := range entries {
		item := map[string]any{
			"name":  entry.Name,
			"isDir": entry.IsDir,
			"size":  entry.Size,
		}

		if recursive && entry.IsDir && depth < maxDepth {
			subPath := filepath.Join(path, entry.Name)
			children, err := listFilesInternal(subPath, true, depth+1, maxDepth)
			if err == nil {
				item["children"] = children
			}
		}

		result = append(result, item)
	}

	return result, nil
}

// dirEntry 是目录条目。
type dirEntry struct {
	Name  string
	IsDir bool
	Size  int64
}

// readDirEntries 读取目录条目。
func readDirEntries(path string) ([]dirEntry, error) {
	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, err
	}

	result := make([]dirEntry, 0, len(entries))
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			continue
		}
		result = append(result, dirEntry{
			Name:  entry.Name(),
			IsDir: entry.IsDir(),
			Size:  info.Size(),
		})
	}
	return result, nil
}
