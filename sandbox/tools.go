package sandbox

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/kasuganosora/thinkbot/agent/tools"
	"github.com/kasuganosora/thinkbot/llm"
)

// ============================================================================
// BotWorkspaceToolProvider — 动态工具提供者
//
// 实现 tools.ToolProvider 接口，为每个会话上下文提供 bot 工作空间工具。
// 工具通过 BotWorkspaceManager 获取 per-bot 持久化工作空间。
// ============================================================================

// botWorkspaceToolPromptSection 是 bot 工作空间工具的提示词段落。
var botWorkspaceToolPromptSection = &tools.ToolPromptSection{
	Name:  "bot_workspace_tools",
	Order: 310,
	Content: `# Bot 工作空间

你拥有一个持久化的工作空间，可以读写文件、执行命令。
每个 Bot 有独立的工作空间，文件在其中持久保存（不会因会话结束而丢失）。

## 可用工具

### 命令执行
- **exec** — 执行 shell 命令，返回 stdout/stderr/exitCode

### 文件操作
- **read_file** — 读取文件内容（支持 offset/limit 分段读取，返回带行号的内容）
- **write_file** — 写入文件（纯文本内容，自动创建父目录）
- **replace_in_file** — 精确替换文件中的字符串片段（old_str → new_str，要求唯一匹配）
- **delete_file** — 删除文件或目录
- **move_file** — 移动/重命名文件或目录
- **list_dir** — 列出目录内容

### 搜索
- **search_content** — 在文件中搜索内容（类似 grep -rn）

### 诊断
- **health** — 检查工作空间的健康状态（容器是否存活、目录是否可用、Docker 是否可用）

## 使用原则

### 文件操作
- **优先使用 replace_in_file 做小修改**，避免重写整个文件
- 读取大文件时使用 offset/limit 参数分段读取，避免一次性读取过多
- 如果不确定文件路径，先用 list_dir 列出目录内容
- 路径相对于工作空间根目录，禁止使用 .. 目录遍历
- 你可以在一次回复中并行调用多个工具来提高效率

### 命令执行
- exec 用于终端操作（如构建、测试、git 等）
- **不要用 exec 做文件操作**（读写、搜索文件），使用专用工具
- 命令有超时限制（默认 30 秒）
- 如果命令执行失败或行为异常，先用 health 工具诊断

### 搜索
- search_content 支持正则表达式，类似 grep -rn
- 使用更精确的搜索模式可以获得更聚焦的结果
- 如果结果太多，缩小搜索范围或使用更具体的 pattern

### 通用
- 工作空间是持久化的，重要数据（笔记、配置等）可以保存到文件中
- 不要编造工具结果，只使用实际返回的数据
- 工具调用失败时，说明失败原因并尝试替代方案`,
	Enabled: true,
}

// BotWorkspaceToolProvider 将 BotWorkspaceManager 适配为动态工具提供者。
type BotWorkspaceToolProvider struct {
	Manager *BotWorkspaceManager
}

// Tools 实现 tools.ToolProvider 接口。
// SubAgent 场景下不提供工作空间工具。
// 工具闭包捕获 BotID（从 sctx 获取），确保执行时能获取正确的 bot 工作空间。
func (p *BotWorkspaceToolProvider) Tools(ctx context.Context, sctx *tools.ToolSessionContext) ([]llm.Tool, error) {
	if sctx != nil && sctx.IsSubagent {
		return nil, nil
	}
	if p.Manager == nil {
		return nil, nil
	}
	if sctx == nil || sctx.BotID == "" {
		return nil, nil
	}

	// 在闭包中捕获 BotID
	botID := sctx.BotID
	return botWorkspaceToolDefs(p.Manager, botID), nil
}

// ============================================================================
// 工具定义
// ============================================================================

// botWorkspaceToolDefs 返回全部 bot 工作空间工具定义。
// botID 在闭包中捕获，确保工具执行时获取正确的 bot 工作空间。
func botWorkspaceToolDefs(mgr *BotWorkspaceManager, botID string) []llm.Tool {
	return []llm.Tool{
		buildExecTool(mgr, botID),
		buildReadFileTool(mgr, botID),
		buildWriteFileTool(mgr, botID),
		buildReplaceInFileTool(mgr, botID),
		buildDeleteFileTool(mgr, botID),
		buildMoveFileTool(mgr, botID),
		buildListDirTool(mgr, botID),
		buildSearchContentTool(mgr, botID),
		buildHealthTool(mgr, botID),
	}
}

// ============================================================================
// sandbox_exec — 执行 shell 命令
// ============================================================================

func buildExecTool(mgr *BotWorkspaceManager, botID string) llm.Tool {
	return llm.Tool{
		Name: "sandbox_exec",
		Description: "在工作空间中执行 shell 命令，返回 stdout、stderr 和 exitCode。" +
			"用于终端操作（如构建、测试、git、包管理等）。" +
			"不要用它做文件操作（读写、搜索文件），应使用专用工具。" +
			"命令有超时限制（默认 30 秒），可配置 timeout 参数。",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"command": map[string]any{
					"type":        "string",
					"description": "要执行的 shell 命令",
				},
				"workdir": map[string]any{
					"type":        "string",
					"description": "命令的工作目录（相对于工作空间根目录）。可选，默认为工作空间根目录。",
				},
				"timeout": map[string]any{
					"type":        "integer",
					"description": "超时时间（秒）。可选，默认 30 秒。",
				},
			},
			"required": []string{"command"},
		},
		Execute: llm.ToolExecuteFunc(func(ctx *llm.ToolExecContext, input any) (any, error) {
			m, ok := input.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("invalid input: expected object")
			}
			command, _ := m["command"].(string)
			if command == "" {
				return nil, fmt.Errorf("command is required")
			}
			workdir, _ := m["workdir"].(string)

			req := ExecRequest{
				Command: command,
				WorkDir: workdir,
			}
			if timeoutSec, ok := toInt(m["timeout"]); ok && timeoutSec > 0 {
				req.Timeout = durationFromSeconds(timeoutSec)
			}

			ws, err := mgr.GetOrCreate(botID)
			if err != nil {
				return nil, err
			}

			result, err := ws.Exec(ctx, req)
			if err != nil {
				return nil, err
			}

			return map[string]any{
				"exitCode":  result.ExitCode,
				"stdout":    result.Stdout,
				"stderr":    result.Stderr,
				"truncated": result.Truncated,
			}, nil
		}),
	}
}

// ============================================================================
// sandbox_read_file — 读取文件（纯文本，支持 offset/limit）
// ============================================================================

func buildReadFileTool(mgr *BotWorkspaceManager, botID string) llm.Tool {
	return llm.Tool{
		Name: "sandbox_read_file",
		Description: "读取工作空间中的文件内容，返回带行号的纯文本。" +
			"支持通过 offset（起始行号，从 1 开始）和 limit（读取行数）分段读取大文件。" +
			"如果省略 offset/limit，则读取整个文件。" +
			"在需要读取多个文件时，可以并行调用此工具。" +
			"避免读取过小的片段（如 30 行），需要更多上下文时读取更大的范围。" +
			"如果需要在大文件中查找特定内容，使用 search_content 工具更高效。",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{
					"type":        "string",
					"description": "文件路径（相对于工作空间根目录）",
				},
				"offset": map[string]any{
					"type":        "integer",
					"description": "起始行号（从 1 开始）。可选，默认为 1。",
				},
				"limit": map[string]any{
					"type":        "integer",
					"description": "读取的行数。可选，默认读取全部。",
				},
			},
			"required": []string{"path"},
		},
		Execute: llm.ToolExecuteFunc(func(ctx *llm.ToolExecContext, input any) (any, error) {
			m, ok := input.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("invalid input: expected object")
			}
			path, _ := m["path"].(string)
			if path == "" {
				return nil, fmt.Errorf("path is required")
			}

			ws, err := mgr.GetOrCreate(botID)
			if err != nil {
				return nil, err
			}

			data, err := ws.ReadFile(ctx, path)
			if err != nil {
				return nil, err
			}

			content := string(data)
			lines := strings.Split(content, "\n")

			offset, _ := toInt(m["offset"])
			if offset < 1 {
				offset = 1
			}
			limit, hasLimit := toInt(m["limit"])

			// 应用 offset/limit
			startIdx := offset - 1
			if startIdx >= len(lines) {
				return map[string]any{
					"path":       path,
					"content":    "",
					"totalLines": len(lines),
					"range":      fmt.Sprintf("%d-%d/%d", offset, offset-1, len(lines)),
				}, nil
			}

			endIdx := len(lines)
			if hasLimit && limit > 0 && startIdx+limit < endIdx {
				endIdx = startIdx + limit
			}

			selected := lines[startIdx:endIdx]
			// 添加行号前缀
			output := make([]string, 0, len(selected))
			for i, line := range selected {
				lineNum := startIdx + i + 1
				output = append(output, fmt.Sprintf("%5d: %s", lineNum, line))
			}

			return map[string]any{
				"path":       path,
				"content":    strings.Join(output, "\n"),
				"totalLines": len(lines),
				"range":      fmt.Sprintf("%d-%d/%d", offset, endIdx, len(lines)),
				"size":       len(data),
			}, nil
		}),
	}
}

// ============================================================================
// sandbox_write_file — 写入文件（纯文本）
// ============================================================================

func buildWriteFileTool(mgr *BotWorkspaceManager, botID string) llm.Tool {
	return llm.Tool{
		Name: "sandbox_write_file",
		Description: "向工作空间写入文件（纯文本内容）。" +
			"如果父目录不存在会自动创建。会覆盖已有文件。" +
			"优先使用 replace_in_file 做小修改，只有创建新文件或需要完全重写时才用此工具。",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{
					"type":        "string",
					"description": "文件路径（相对于工作空间根目录）",
				},
				"content": map[string]any{
					"type":        "string",
					"description": "文件内容（纯文本）",
				},
			},
			"required": []string{"path", "content"},
		},
		Execute: llm.ToolExecuteFunc(func(ctx *llm.ToolExecContext, input any) (any, error) {
			m, ok := input.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("invalid input: expected object")
			}
			path, _ := m["path"].(string)
			if path == "" {
				return nil, fmt.Errorf("path is required")
			}
			content, _ := m["content"].(string)

			data := []byte(content)

			ws, err := mgr.GetOrCreate(botID)
			if err != nil {
				return nil, err
			}

			if err := ws.WriteFile(ctx, path, data); err != nil {
				return nil, err
			}

			lineCount := strings.Count(content, "\n") + 1
			if content == "" {
				lineCount = 0
			}

			return map[string]any{
				"success": true,
				"path":    path,
				"size":    len(data),
				"lines":   lineCount,
			}, nil
		}),
	}
}

// ============================================================================
// sandbox_replace_in_file — 精确字符串替换
// ============================================================================

func buildReplaceInFileTool(mgr *BotWorkspaceManager, botID string) llm.Tool {
	return llm.Tool{
		Name: "sandbox_replace_in_file",
		Description: "在文件中进行精确字符串替换。" +
			"将文件中的 old_str 替换为 new_str。" +
			"默认 old_str 必须在文件中唯一存在；设置 replace_all=true 可替换所有匹配。" +
			"这是做小范围修改的首选方式，避免重写整个文件。" +
			"注意：确保 old_str 精确匹配文件内容（包括空白和缩进）。",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{
					"type":        "string",
					"description": "文件路径（相对于工作空间根目录）",
				},
				"old_str": map[string]any{
					"type":        "string",
					"description": "要替换的原始字符串（必须精确匹配，包括空白和缩进）。默认必须在文件中唯一。",
				},
				"new_str": map[string]any{
					"type":        "string",
					"description": "替换后的新字符串。传入空字符串表示删除 old_str。",
				},
				"replace_all": map[string]any{
					"type":        "boolean",
					"description": "是否替换所有匹配项（而非仅唯一匹配）。默认 false。用于批量替换如变量重命名。",
				},
			},
			"required": []string{"path", "old_str", "new_str"},
		},
		Execute: llm.ToolExecuteFunc(func(ctx *llm.ToolExecContext, input any) (any, error) {
			m, ok := input.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("invalid input: expected object")
			}
			path, _ := m["path"].(string)
			if path == "" {
				return nil, fmt.Errorf("path is required")
			}
			oldStr, _ := m["old_str"].(string)
			if oldStr == "" {
				return nil, fmt.Errorf("old_str is required")
			}
			newStr, _ := m["new_str"].(string)

			// normalize line endings (CRLF → LF) for cross-platform compatibility
			oldStr = strings.ReplaceAll(oldStr, "\r\n", "\n")
			newStr = strings.ReplaceAll(newStr, "\r\n", "\n")

			replaceAll := false
			if v, ok := m["replace_all"].(bool); ok {
				replaceAll = v
			}

			ws, err := mgr.GetOrCreate(botID)
			if err != nil {
				return nil, err
			}

			// 读取当前内容
			data, err := ws.ReadFile(ctx, path)
			if err != nil {
				return nil, err
			}

			content := string(data)
			// normalize file line endings too
			content = strings.ReplaceAll(content, "\r\n", "\n")

			// 检查 old_str 是否存在
			count := strings.Count(content, oldStr)
			if count == 0 {
				return nil, fmt.Errorf("old_str not found in file %q", path)
			}
			if count > 1 && !replaceAll {
				return nil, fmt.Errorf("old_str appears %d times in file %q — must be unique. Set replace_all=true to replace all, or provide a longer string with more surrounding context", count, path)
			}

			// 执行替换
			var newContent string
			replacedCount := 0
			if replaceAll {
				newContent = strings.ReplaceAll(content, oldStr, newStr)
				replacedCount = count
			} else {
				newContent = strings.Replace(content, oldStr, newStr, 1)
				replacedCount = 1
			}

			// 写回
			if err := ws.WriteFile(ctx, path, []byte(newContent)); err != nil {
				return nil, err
			}

			return map[string]any{
				"success":  true,
				"path":     path,
				"oldSize":  len(data),
				"newSize":  len(newContent),
				"replaced": replacedCount,
			}, nil
		}),
	}
}

// ============================================================================
// sandbox_delete_file — 删除文件或目录
// ============================================================================

func buildDeleteFileTool(mgr *BotWorkspaceManager, botID string) llm.Tool {
	return llm.Tool{
		Name:        "sandbox_delete_file",
		Description: "删除 bot 工作空间中的文件或目录（递归删除目录）。",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{
					"type":        "string",
					"description": "要删除的文件或目录路径（相对于工作空间根目录）",
				},
			},
			"required": []string{"path"},
		},
		Execute: llm.ToolExecuteFunc(func(ctx *llm.ToolExecContext, input any) (any, error) {
			m, ok := input.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("invalid input: expected object")
			}
			path, _ := m["path"].(string)
			if path == "" {
				return nil, fmt.Errorf("path is required")
			}

			ws, err := mgr.GetOrCreate(botID)
			if err != nil {
				return nil, err
			}

			// 用 rm -rf 删除（路径已通过 validatePath 校验）
			result, err := ws.Exec(ctx, ExecRequest{
				Command: fmt.Sprintf("rm -rf -- %s", shellQuote(path)),
			})
			if err != nil {
				return nil, err
			}
			if result.ExitCode != 0 {
				return nil, fmt.Errorf("delete failed: %s", result.Stderr)
			}

			return map[string]any{
				"success": true,
				"path":    path,
			}, nil
		}),
	}
}

// ============================================================================
// sandbox_move_file — 移动/重命名文件或目录
// ============================================================================

func buildMoveFileTool(mgr *BotWorkspaceManager, botID string) llm.Tool {
	return llm.Tool{
		Name:        "sandbox_move_file",
		Description: "移动或重命名 bot 工作空间中的文件或目录。",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"src": map[string]any{
					"type":        "string",
					"description": "源路径（相对于工作空间根目录）",
				},
				"dst": map[string]any{
					"type":        "string",
					"description": "目标路径（相对于工作空间根目录）",
				},
			},
			"required": []string{"src", "dst"},
		},
		Execute: llm.ToolExecuteFunc(func(ctx *llm.ToolExecContext, input any) (any, error) {
			m, ok := input.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("invalid input: expected object")
			}
			src, _ := m["src"].(string)
			if src == "" {
				return nil, fmt.Errorf("src is required")
			}
			dst, _ := m["dst"].(string)
			if dst == "" {
				return nil, fmt.Errorf("dst is required")
			}

			ws, err := mgr.GetOrCreate(botID)
			if err != nil {
				return nil, err
			}

			result, err := ws.Exec(ctx, ExecRequest{
				Command: fmt.Sprintf("mv -- %s %s", shellQuote(src), shellQuote(dst)),
			})
			if err != nil {
				return nil, err
			}
			if result.ExitCode != 0 {
				return nil, fmt.Errorf("move failed: %s", result.Stderr)
			}

			return map[string]any{
				"success": true,
				"src":     src,
				"dst":     dst,
			}, nil
		}),
	}
}

// ============================================================================
// sandbox_list_dir — 列出目录内容
// ============================================================================

func buildListDirTool(mgr *BotWorkspaceManager, botID string) llm.Tool {
	return llm.Tool{
		Name: "sandbox_list_dir",
		Description: "列出工作空间中指定目录的内容，返回文件和子目录列表。" +
			"如果不确定文件路径，先用此工具查看目录结构。" +
			"默认列出根目录。",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{
					"type":        "string",
					"description": "目录路径（相对于工作空间根目录）。默认为根目录。",
				},
			},
		},
		Execute: llm.ToolExecuteFunc(func(ctx *llm.ToolExecContext, input any) (any, error) {
			m, ok := input.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("invalid input: expected object")
			}
			path, _ := m["path"].(string)

			ws, err := mgr.GetOrCreate(botID)
			if err != nil {
				return nil, err
			}

			entries, err := ws.ListDir(ctx, path)
			if err != nil {
				return nil, err
			}

			items := make([]map[string]any, 0, len(entries))
			for _, e := range entries {
				items = append(items, map[string]any{
					"name":  e.Name,
					"isDir": e.IsDir,
					"size":  e.Size,
				})
			}

			return map[string]any{
				"path":    path,
				"entries": items,
				"count":   len(items),
			}, nil
		}),
	}
}

// ============================================================================
// sandbox_search_content — 在文件中搜索内容
// ============================================================================

func buildSearchContentTool(mgr *BotWorkspaceManager, botID string) llm.Tool {
	return llm.Tool{
		Name: "sandbox_search_content",
		Description: "在工作空间的文件中搜索内容（类似 grep -rn）。" +
			"支持正则表达式（如 \"log.*Error\"、\"function\\s+\\w+\"）和递归搜索目录。" +
			"返回匹配的文件名、行号和匹配行内容。" +
			"使用更精确的 pattern 可以获得更聚焦的结果。" +
			"如果需要按文件名查找文件，先用 list_dir 列出目录。",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"pattern": map[string]any{
					"type":        "string",
					"description": "搜索模式（正则表达式）",
				},
				"path": map[string]any{
					"type":        "string",
					"description": "搜索路径（文件或目录）。默认为工作空间根目录。",
				},
				"case_sensitive": map[string]any{
					"type":        "boolean",
					"description": "是否区分大小写。默认 false（不区分）。",
				},
				"max_results": map[string]any{
					"type":        "integer",
					"description": "最大返回结果数。默认 100。",
				},
			},
			"required": []string{"pattern"},
		},
		Execute: llm.ToolExecuteFunc(func(ctx *llm.ToolExecContext, input any) (any, error) {
			m, ok := input.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("invalid input: expected object")
			}
			pattern, _ := m["pattern"].(string)
			if pattern == "" {
				return nil, fmt.Errorf("pattern is required")
			}
			searchPath, _ := m["path"].(string)
			if searchPath == "" {
				searchPath = "."
			}

			caseSensitive := false
			if v, ok := m["case_sensitive"].(bool); ok {
				caseSensitive = v
			}

			maxResults := 100
			if v, ok := toInt(m["max_results"]); ok && v > 0 {
				maxResults = v
			}

			ws, err := mgr.GetOrCreate(botID)
			if err != nil {
				return nil, err
			}

			// 构建 grep 命令
			grepFlags := "-rn"
			if !caseSensitive {
				grepFlags = "-rni"
			}
			grepFlags += fmt.Sprintf(" --max-count=%d", maxResults)

			result, err := ws.Exec(ctx, ExecRequest{
				Command: fmt.Sprintf("grep %s -- %s %s 2>/dev/null || true",
					grepFlags, shellQuote(pattern), shellQuote(searchPath)),
			})
			if err != nil {
				return nil, err
			}

			// 解析 grep 输出: path:lineno:content
			type match struct {
				File    string `json:"file"`
				Line    int    `json:"line"`
				Content string `json:"content"`
			}
			matches := make([]match, 0)
			if result.Stdout != "" {
				for _, line := range strings.Split(result.Stdout, "\n") {
					line = strings.TrimRight(line, "\r")
					if line == "" {
						continue
					}
					// 格式: path:lineno:content
					parts := strings.SplitN(line, ":", 3)
					if len(parts) < 3 {
						continue
					}
					var lineNum int
					_, _ = fmt.Sscanf(parts[1], "%d", &lineNum)
					matches = append(matches, match{
						File:    parts[0],
						Line:    lineNum,
						Content: parts[2],
					})
					if len(matches) >= maxResults {
						break
					}
				}
			}

			return map[string]any{
				"pattern":    pattern,
				"path":       searchPath,
				"matchCount": len(matches),
				"matches":    matches,
				"truncated":  len(matches) >= maxResults,
			}, nil
		}),
	}
}

// ============================================================================
// sandbox_health — 检查工作空间健康状态
// ============================================================================

func buildHealthTool(mgr *BotWorkspaceManager, botID string) llm.Tool {
	return llm.Tool{
		Name: "sandbox_health",
		Description: "检查 bot 工作空间的健康状态。" +
			"返回工作空间是否可用、后端类型（docker/local）、状态和详细信息。" +
			"当命令执行失败或行为异常时，先调用此工具诊断问题。",
		Parameters: map[string]any{
			"type":       "object",
			"properties": map[string]any{},
		},
		Execute: llm.ToolExecuteFunc(func(ctx *llm.ToolExecContext, input any) (any, error) {
			status := mgr.HealthCheck(ctx, botID)
			return map[string]any{
				"healthy": status.Healthy,
				"backend": status.Backend,
				"status":  status.Status,
				"message": status.Message,
				"botID":   botID,
			}, nil
		}),
	}
}

// ============================================================================
// 辅助函数
// ============================================================================

// toInt 从 any 安全提取 int（JSON 数字可能解析为 float64）。
func toInt(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case int64:
		return int(n), true
	case float64:
		return int(n), true
	}
	return 0, false
}

// durationFromSeconds 将秒数转为 Duration。
func durationFromSeconds(sec int) time.Duration {
	return time.Duration(sec) * time.Second
}

// sessionKeyCtxKey 是 context value 的 key 类型（用于 SandboxManager 的会话级 API）。
type sessionKeyCtxKey struct{}

// ContextWithSessionKey 将 SessionKey 注入 context。
func ContextWithSessionKey(ctx context.Context, key SessionKey) context.Context {
	return context.WithValue(ctx, sessionKeyCtxKey{}, key)
}

// SessionKeyFromContext 从 context 中提取 SessionKey。
func SessionKeyFromContext(ctx context.Context) SessionKey {
	if v, ok := ctx.Value(sessionKeyCtxKey{}).(SessionKey); ok {
		return v
	}
	return SessionKey{}
}

// ============================================================================
// RegisterTools — 便捷注册函数
// ============================================================================

// RegisterBotWorkspaceTools 将 bot 工作空间工具注册到 ToolManager。
//
// 注册两部分：
//  1. 提示词段落（通过隐藏的 ToolDef 注册，scope "__never__" 确保该占位工具永不出现在工具列表中，
//     但其 PromptSection 会被注册到 prompt.Registry）
//  2. 动态 ToolProvider（会话感知，每次 Resolve 时从 ToolSessionContext 捕获 BotID）
//
// SubAgent 场景下不提供工作空间工具（防止递归使用）。
func RegisterBotWorkspaceTools(toolMgr *tools.ToolManager, mgr *BotWorkspaceManager) error {
	if mgr == nil {
		return nil
	}
	// 注册提示词段落（隐藏占位工具，永不出现在工具列表）
	_ = toolMgr.Register(tools.ToolDef{
		Tool:          llm.Tool{Name: "__bot_workspace_meta", Description: "internal: bot workspace prompt section"},
		Category:      "sandbox",
		Scopes:        []string{"__never__"},
		PromptSection: botWorkspaceToolPromptSection,
	})
	// 注册动态工具提供者（会话感知）
	toolMgr.AddProvider(&BotWorkspaceToolProvider{Manager: mgr})
	return nil
}

// BotWorkspaceToolDefs 返回 bot 工作空间工具的 ToolDef 列表（带元数据，用于静态注册）。
// botID 是 bot 标识符，通常用于测试或直接调用场景。
func BotWorkspaceToolDefs(mgr *BotWorkspaceManager, botID string) []tools.ToolDef {
	rawTools := botWorkspaceToolDefs(mgr, botID)
	defs := make([]tools.ToolDef, 0, len(rawTools))
	for _, t := range rawTools {
		defs = append(defs, tools.ToolDef{
			Tool:          t,
			Category:      "sandbox",
			Scopes:        []string{"private", "group"},
			PromptSection: botWorkspaceToolPromptSection,
		})
	}
	return defs
}
