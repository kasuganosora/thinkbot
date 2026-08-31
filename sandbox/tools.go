package sandbox

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/kasuganosora/thinkbot/agent/core"
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
	Content: `# Bot Workspace

You have a persistent workspace where you can read and write files and run shell commands.
Every bot gets its own isolated workspace, and its files survive across sessions — nothing is lost when a conversation ends.

## Available tools

### Command execution
- **exec** — run a shell command; returns stdout, stderr and exitCode

### File operations
- **read_file** — read file contents (supports offset/limit paging, returns line-numbered content)
- **write_file** — write a file (plain text; parent directories are created automatically)
- **replace_in_file** — replace an exact string in a file (old_str → new_str, match must be unique)
- **delete_file** — delete a file or directory
- **move_file** — move or rename a file or directory
- **list_dir** — list directory contents

### Search
- **search_content** — search inside files (similar to grep -rn)

### Diagnostics
- **health** — check workspace health (is the container alive, is the directory usable, is Docker available)

## Rules

### File operations
- **ALWAYS prefer replace_in_file for small edits.** Do not rewrite an entire file to change a few lines.
- Page through large files with offset/limit instead of reading everything at once.
- If you are unsure of a path, run list_dir first. NEVER guess file paths.
- Paths are relative to the workspace root. NEVER use ".." to traverse outside it.
- You can call several independent tools in parallel within a single reply.

### Command execution
- Use exec for terminal work: builds, tests, git, package managers.
- **NEVER use exec for file operations** (reading, writing, searching). Use the dedicated file tools instead.
- Commands are subject to a timeout (30 seconds by default).
- If a command fails or the workspace behaves unexpectedly, run health first to diagnose.

### Search
- search_content accepts regular expressions, similar to grep -rn.
- A more precise pattern gives more focused results.
- If there are too many matches, narrow the search path or tighten the pattern.

### General
- The workspace is persistent — store anything worth keeping (notes, configs, intermediate results) in files.
- CRITICAL: NEVER invent tool results. Use only data a tool actually returned.
- When a tool call fails, state why it failed and try a concrete alternative.

### Language
- These tool descriptions are written in English, but you reply to the user in Chinese (中文) by default — if the user writes in another language, match theirs.`,
	Enabled: true,
}

// BotWorkspaceToolProvider 将 BotWorkspaceManager 适配为动态工具提供者。
type BotWorkspaceToolProvider struct {
	Manager *BotWorkspaceManager
}

// Tools 实现 tools.ToolProvider 接口。
// 子 Agent（IsSubagent=true）同样返回工作空间工具：子 Agent 与主 Agent 共用同一个
// per-bot 沙箱（同一 BotID 的工作空间），因此能像主 Agent 一样 exec/读写/列目录。
// 唯一的递归防护由 spawn 工具的 scope 实现（spawn 仅对 private/group 场景可见，
// 子 Agent 场景不可见），防止子 Agent 再 spawn 子 Agent 形成套娃。
// 工具闭包捕获 BotID（从 sctx 获取），确保执行时能获取正确的 bot 工作空间。
func (p *BotWorkspaceToolProvider) Tools(ctx context.Context, sctx *tools.ToolSessionContext) ([]llm.Tool, error) {
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
		buildRunCodeTool(mgr, botID),
	}
}

// ============================================================================
// sandbox_exec — 执行 shell 命令
// ============================================================================

func buildExecTool(mgr *BotWorkspaceManager, botID string) llm.Tool {
	return llm.Tool{
		Name: "sandbox_exec",
		Description: "Run a shell command in the workspace and return stdout, stderr and exitCode. " +
			"Use it for terminal work such as builds, tests, git and package managers. " +
			"NEVER use it for file operations (reading, writing, searching files) — use the dedicated file tools instead. " +
			"Execution is protected by a stuck watchdog: a command that keeps producing output is never killed, however slow it is; " +
			"it is only declared stuck and terminated after it produces no output for longer than the stuck threshold (180s by default, override with stuck_timeout). " +
			"A hard ceiling acts as a backstop (600s by default, override with timeout) — once exceeded the command is terminated unconditionally to prevent an infinite hang. " +
			"IMPORTANT: NEVER append `| head` / `| tail` / `| less` or similar pipes just to limit output. " +
			"The sandbox already truncates output at MaxOutput bytes, so such a pipe is redundant, and it can hang the command forever " +
			"when the inspected process dies (a child process keeps the write end of the pipe open). For less output, lower timeout or " +
			"let the command limit itself (e.g. golangci-lint --out-format). " +
			"The result also carries reliability signals: reliable (was the command complete and trustworthy), aborted (did it fail midway), " +
			"oomKilled (was it killed by the OOM killer), warnings (why it is untrustworthy). " +
			"CRITICAL: when reliable is false the command did NOT run to completion (OOM, timeout, or killed) — " +
			"do not treat its output as a full result; raise the sandbox memory limit or change the approach, then retry.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"command": map[string]any{
					"type":        "string",
					"description": "The shell command to run.",
				},
				"workdir": map[string]any{
					"type":        "string",
					"description": "Working directory for the command, relative to the workspace root. Optional; defaults to the workspace root.",
				},
				"timeout": map[string]any{
					"type":        "integer",
					"description": "Hard ceiling in seconds. Optional; 0 (the default) means automatic = stuck threshold × 3 (15 minutes by default, controlled by the sandbox.timeout config). This is the backstop on total run time: once exceeded the command is force-terminated. A normal slow command that keeps producing output is never killed by this value — the stuck watchdog lets it through.",
				},
				"stuck_timeout": map[string]any{
					"type":        "integer",
					"description": "Stuck-watchdog threshold in seconds. Optional; defaults to 300 (5 minutes, controlled by the sandbox.stuck_timeout config). A command that produces no output for longer than this is declared stuck and terminated; as long as it keeps producing output, however slowly, it is left alone. Use it to distinguish a slow compile from a deadlock.",
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
			// 剥离 LLM 自行追加的 `| head`/`| tail` 输出限制管道：
			// 既冗余（sandbox 已按 MaxOutput 截断）又是命令永久挂死的常见根因。
			stripped := false
			command, stripped = stripOutputLimitingPipe(command)
			workdir, _ := m["workdir"].(string)

			req := ExecRequest{
				Command: command,
				WorkDir: workdir,
			}
			if timeoutSec, ok := toInt(m["timeout"]); ok && timeoutSec > 0 {
				req.Timeout = durationFromSeconds(timeoutSec)
			}
			if stuckSec, ok := toInt(m["stuck_timeout"]); ok && stuckSec > 0 {
				req.StuckTimeout = durationFromSeconds(stuckSec)
			}

			ws, err := mgr.GetOrCreate(botID)
			if err != nil {
				return nil, err
			}

			// 流式输出（若后端支持且调用方提供进度回调）。
			onChunk := func(c ExecChunk) {
				if ctx.SendProgress != nil {
					switch c.Stream {
					case "stderr":
						ctx.SendProgress(map[string]any{"stream": "stderr", "chunk": c.Data})
					case "heartbeat":
						// 保活心跳：不携带 chunk，前端收到仅刷新「存活」状态，不污染输出。
						ctx.SendProgress(map[string]any{"stream": "heartbeat"})
					default:
						ctx.SendProgress(map[string]any{"stream": "stdout", "chunk": c.Data})
					}
				}
			}
			streamable, _ := ws.(StreamWorkspace)

			var res *ExecResult
			if streamable != nil && ctx.SendProgress != nil {
				res, err = streamable.ExecStream(ctx, req, onChunk)
			} else {
				res, err = ws.Exec(ctx, req)
			}
			if err != nil {
				return nil, err
			}

			// 层2（agent 门禁·强版）：验证型命令 OOM 时自动重试一次（临时提升沙箱内存）。
			// 首次执行已在上方完成，传入首结果避免重复执行。
			if res.OOMKilled && isVerificationCommand(command) {
				if retryRes, rerr := mgr.RetryOOMWithElevatedMemory(ctx, botID, res, req, onChunk); rerr == nil && retryRes != nil {
					res = retryRes
				}
			}

			// 若剥离了 `| head`/`| tail` 管道，提示 bot 该操作已被自动处理。
			if stripped {
				res.Warnings = append(res.Warnings,
					"Stripped a trailing `| head`/`| tail` output-limiting pipe from the command (the sandbox already truncates output at MaxOutput, and such pipes can hang the command).")
			}

			return execResultToToolOutput(res, ws.WorkDir()), nil
		}),
	}
}

// ============================================================================
// run_code — 编程式工具调用（Programmatic Tool Calling，对应 harness 的 code 模式）
// ============================================================================
//
// 让模型把「多轮工具编排」下推成一段脚本，在沙箱内一次执行，只把最终 curated 结果
// 回传给模型上下文；中间过程（命令输出、文件读写）留在沙箱，不进上下文 → 大幅压低
// 多步骤任务的 token 成本（这正是 harness code 模式的 PTC 思想）。
// 与 sandbox_exec 同隔离级别、同等权限，始终可用；要强制只走代码编排可把 pipeline 模式
// 设为 "code"（配置 pipeline.mode / bot.<id>.pipeline_mode）。
//
// 注意：harness 的 run_code 是让模型在 async 函数体里 `await tools.x()` 调其它工具
// （in-process 函数）；thinkbot 没有该调度器，故退化为「在沙箱跑脚本、脚本内部自行调用
// 命令/文件工具」——语义等价（多步归一并只回结果），且复用既有 Workspace.Exec 隔离。

func buildRunCodeTool(mgr *BotWorkspaceManager, botID string) llm.Tool {
	return llm.Tool{
		Name: "run_code",
		Description: "Run a multi-step script in the workspace and return ONLY its final curated output. " +
			"Use it to orchestrate several dependent operations (shell commands, file reads/writes, searches) in ONE call: " +
			"write the whole sequence as code and print ONLY what you actually need back — intermediate output stays in the " +
			"sandbox and is NOT returned, keeping the conversation small (Programmatic Tool Calling). " +
			"lang is 'bash' (default), 'python', or 'node'. " +
			"Prefer this over emitting many separate tool calls when later steps depend on earlier results.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"code": map[string]any{
					"type":        "string",
					"description": "The script source. For bash: a sequence of shell commands. For python/node: a program that prints the final result to stdout.",
				},
				"lang": map[string]any{
					"type":        "string",
					"description": "Script language: 'bash' (default), 'python', or 'node'.",
				},
				"workdir": map[string]any{
					"type":        "string",
					"description": "Working directory relative to workspace root. Optional; defaults to workspace root.",
				},
				"timeout": map[string]any{
					"type":        "integer",
					"description": "Hard ceiling in seconds. Optional; 0 means automatic (stuck threshold x3, backstop ~600s).",
				},
				"stuck_timeout": map[string]any{
					"type":        "integer",
					"description": "Stuck-watchdog threshold in seconds. Optional; default 300.",
				},
			},
			"required": []string{"code"},
		},
		Execute: llm.ToolExecuteFunc(func(ctx *llm.ToolExecContext, input any) (any, error) {
			m, ok := input.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("invalid input: expected object")
			}
			code, _ := m["code"].(string)
			if strings.TrimSpace(code) == "" {
				return nil, fmt.Errorf("code is required")
			}
			lang, _ := m["lang"].(string)
			if lang == "" {
				lang = "bash"
			}
			ext, interp, err := runCodeLang(lang)
			if err != nil {
				return nil, err
			}

			ws, err := mgr.GetOrCreate(botID)
			if err != nil {
				return nil, err
			}

			// 事件轨迹（append-only）：tool/call（log-only，不进模型）。
			core.EventSinkFromContext(ctx).Emit(ctx, core.Event{
				Kind:    core.EventToolCall,
				Source:  "tool:run_code",
				Surface: false,
				Payload: map[string]any{"lang": lang},
			})

			fileName := fmt.Sprintf("tool-output/run_code_%d%s", time.Now().UnixNano(), ext)
			if err := ws.WriteFile(ctx, fileName, []byte(code)); err != nil {
				return nil, fmt.Errorf("write script: %w", err)
			}

			req := ExecRequest{Command: interp + " " + fileName}
			if wd, _ := m["workdir"].(string); wd != "" {
				req.WorkDir = wd
			}
			if timeoutSec, ok := toInt(m["timeout"]); ok && timeoutSec > 0 {
				req.Timeout = durationFromSeconds(timeoutSec)
			}
			if stuckSec, ok := toInt(m["stuck_timeout"]); ok && stuckSec > 0 {
				req.StuckTimeout = durationFromSeconds(stuckSec)
			}

			res, err := ws.Exec(ctx, req)
			if err != nil {
				return nil, err
			}

			// 仅回 curated 结果（surface）：stdout 为主体；失败时附简短 stderr 尾。
			out := map[string]any{
				"exit_code": res.ExitCode,
				"stdout":    res.Stdout,
			}
			if res.ExitCode != 0 && res.Stderr != "" {
				stderr := res.Stderr
				if len(stderr) > 2000 {
					stderr = stderr[len(stderr)-2000:]
				}
				out["stderr_tail"] = stderr
			}
			if !res.Reliable {
				out["reliable"] = false
				out["warnings"] = res.Warnings
			}

			// 事件轨迹（append-only）：tool/result（进入模型上下文，surface=true）。
			core.EventSinkFromContext(ctx).Emit(ctx, core.Event{
				Kind:    core.EventToolResult,
				Source:  "tool:run_code",
				Surface: true,
				Payload: map[string]any{"exit_code": res.ExitCode, "stdout_len": len(res.Stdout)},
			})
			return out, nil
		}),
	}
}

// runCodeLang 将 lang 映射到脚本文件扩展名与解释器命令。
func runCodeLang(lang string) (ext, interp string, err error) {
	switch lang {
	case "bash", "sh":
		return ".sh", "bash", nil
	case "python", "py":
		return ".py", "python3", nil
	case "node", "js":
		return ".js", "node", nil
	default:
		return "", "", fmt.Errorf("unsupported lang %q (want bash/python/node)", lang)
	}
}

// ============================================================================
// sandbox_read_file — 读取文件（纯文本，支持 offset/limit）
// ============================================================================

func buildReadFileTool(mgr *BotWorkspaceManager, botID string) llm.Tool {
	return llm.Tool{
		Name: "sandbox_read_file",
		Description: "Read a file from the workspace and return its contents as line-numbered plain text. " +
			"Use offset (start line, 1-based) and limit (number of lines) to page through large files. " +
			"Omit both to read the entire file. " +
			"Call this tool in parallel when you need to read several files. " +
			"Avoid tiny reads (e.g. 30 lines) — read a wider range when you need more context. " +
			"To locate specific content inside a large file, use search_content instead; it is far more efficient.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{
					"type":        "string",
					"description": "File path, relative to the workspace root.",
				},
				"offset": map[string]any{
					"type":        "integer",
					"description": "Start line, 1-based. Optional; defaults to 1.",
				},
				"limit": map[string]any{
					"type":        "integer",
					"description": "Number of lines to read. Optional; reads to the end of the file by default.",
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
		Description: "Write a file into the workspace (plain text content). " +
			"Missing parent directories are created automatically. An existing file is overwritten. " +
			"IMPORTANT: prefer replace_in_file for small edits; use this tool only to create a new file or to rewrite one completely.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{
					"type":        "string",
					"description": "File path, relative to the workspace root.",
				},
				"content": map[string]any{
					"type":        "string",
					"description": "File content (plain text).",
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
		Description: "Perform an exact string replacement inside a file. " +
			"Replaces old_str with new_str. " +
			"By default old_str MUST occur exactly once in the file; set replace_all=true to replace every occurrence. " +
			"This is the preferred way to make small edits — it avoids rewriting the whole file. " +
			"IMPORTANT: old_str must match the file content exactly, including whitespace and indentation.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{
					"type":        "string",
					"description": "File path, relative to the workspace root.",
				},
				"old_str": map[string]any{
					"type":        "string",
					"description": "The original string to replace. Must match exactly, including whitespace and indentation. Must be unique in the file unless replace_all is true.",
				},
				"new_str": map[string]any{
					"type":        "string",
					"description": "The replacement string. Pass an empty string to delete old_str.",
				},
				"replace_all": map[string]any{
					"type":        "boolean",
					"description": "Replace every match instead of requiring a unique match. Defaults to false. Use it for bulk edits such as renaming a variable.",
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
		Name:         "sandbox_delete_file",
		DeferredLoad: true, // 破坏性操作，非日常高频，初始仅暴露名称+描述
		Description:  "Delete a file or directory in the bot workspace (directories are removed recursively).",
		// Description 已英文化，而用户常用中文提问。tool_search 只对
		// name/description/keywords 做子串匹配，故必须显式保留中文检索词。
		Keywords: []string{"删除文件", "删除目录", "移除文件", "清理文件", "delete file"},
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{
					"type":        "string",
					"description": "Path of the file or directory to delete, relative to the workspace root.",
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
		Name:         "sandbox_move_file",
		DeferredLoad: true, // 重命名/移动，非日常高频，初始仅暴露名称+描述
		Description:  "Move or rename a file or directory in the bot workspace.",
		Keywords:     []string{"移动文件", "重命名", "改名", "move file", "rename"},
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"src": map[string]any{
					"type":        "string",
					"description": "Source path, relative to the workspace root.",
				},
				"dst": map[string]any{
					"type":        "string",
					"description": "Destination path, relative to the workspace root.",
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
		Description: "List the contents of a directory in the workspace, returning its files and subdirectories. " +
			"ALWAYS run this first when you are unsure of a path — do not guess the directory structure. " +
			"Lists the workspace root by default.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{
					"type":        "string",
					"description": "Directory path, relative to the workspace root. Defaults to the root.",
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
		Description: "Search file contents in the workspace, backed by ripgrep (far more capable than BusyBox grep). " +
			"Supports full regular expressions (e.g. \"log.*Error\", \"function\\s+\\w+\", \"\\d{4}-\\d{2}\"), " +
			"searches directories recursively and includes hidden files. Returns the matching file name, line number and line content. " +
			"A more precise pattern gives more focused results. " +
			"To find files by name rather than by content, use list_dir instead.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"pattern": map[string]any{
					"type":        "string",
					"description": "Search pattern (regular expression).",
				},
				"path": map[string]any{
					"type":        "string",
					"description": "File or directory to search. Defaults to the workspace root.",
				},
				"case_sensitive": map[string]any{
					"type":        "boolean",
					"description": "Match case-sensitively. Defaults to false (case-insensitive).",
				},
				"max_results": map[string]any{
					"type":        "integer",
					"description": "Maximum number of matches to return. Defaults to 100.",
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

			// 优先使用 ripgrep（rg）：能力远强于 BusyBox grep，支持真正的正则
			// （\s \w \d、量词、分组等）、递归、隐藏文件、更快。若容器内未安装 rg
			// （ExitCode == 127），自动回退到 grep。
			icase := ""
			if !caseSensitive {
				icase = " -i"
			}
			rgCmd := fmt.Sprintf("rg --line-number --no-heading --hidden --no-ignore%s --max-count=%d -- %s %s 2>/dev/null",
				icase, maxResults, shellQuote(pattern), shellQuote(searchPath))
			grepFlags := "-rn"
			if !caseSensitive {
				grepFlags = "-rni"
			}
			grepFlags += fmt.Sprintf(" --max-count=%d", maxResults)

			res, err := ws.Exec(ctx, ExecRequest{Command: rgCmd})
			if err != nil {
				return nil, err
			}
			stdout := res.Stdout
			if res.ExitCode == 127 {
				// rg 不可用，回退到 grep（保持旧行为）
				res2, err2 := ws.Exec(ctx, ExecRequest{Command: fmt.Sprintf("grep %s -- %s %s 2>/dev/null || true",
					grepFlags, shellQuote(pattern), shellQuote(searchPath))})
				if err2 != nil {
					return nil, err2
				}
				stdout = res2.Stdout
			}

			// 解析 grep 输出: path:lineno:content
			matches := parseSearchMatches(stdout, maxResults)

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
		Name:         "sandbox_health",
		DeferredLoad: true, // 诊断工具，非常规使用，初始仅暴露名称+描述
		Description: "Check the health of the bot workspace. " +
			"Returns whether the workspace is usable, the backend type (docker/local), its status and details. " +
			"ALWAYS call this tool first to diagnose when a command fails or the workspace behaves unexpectedly.",
		Keywords: []string{"沙箱健康", "工作空间状态", "环境诊断", "容器状态", "health", "诊断"},
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
// 子 Agent 场景同样提供工作空间工具（与主 Agent 共用同一 per-bot 沙箱），
// 仅由 spawn 工具的 scope 排除子 Agent，防止递归套娃。
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

// ============================================================================
// 工具结果 → LLM 输出（含完整性 / 可信度信号）
// ============================================================================

// execResultToToolOutput 将底层 ExecResult 转为 LLM 工具返回结构，并注入可靠性信号。
// 当结果不可信（reliable=false）时，额外设置 reliabilityWarning 字段，并把显著警告
// 前置到 stdout，确保 LLM 无论如何都能看到「结果不完整」提示（agent 门禁·轻量版）。
func execResultToToolOutput(res *ExecResult, workdir string) map[string]any {
	out := map[string]any{
		"exitCode":  res.ExitCode,
		"stdout":    res.Stdout,
		"stderr":    res.Stderr,
		"truncated": res.Truncated,
		"reliable":  res.Reliable,
		"aborted":   res.Aborted,
		"oomKilled": res.OOMKilled,
		"warnings":  res.Warnings,
		"workdir":   workdir,
	}
	if !res.Reliable {
		warn := buildReliabilityWarning(res)
		out["reliabilityWarning"] = warn
		// 前置到 stdout，确保 LLM 无论如何都能看到不可信提示。
		out["stdout"] = warn + "\n" + res.Stdout
	}
	return out
}

// buildReliabilityWarning 生成面向 LLM 的不可信警告文案。
func buildReliabilityWarning(res *ExecResult) string {
	var b strings.Builder
	b.WriteString("⚠️ [INCOMPLETE / UNTRUSTWORTHY TOOL RESULT] ")
	switch {
	case res.OOMKilled:
		b.WriteString("The command was likely OOM-killed (the sandbox ran out of memory); only partial output was captured.")
	case res.Aborted:
		b.WriteString("The command failed midway (killed by a signal or timed out); only partial output was captured.")
	default:
		b.WriteString("The command result is not trustworthy.")
	}
	if len(res.Warnings) > 0 {
		b.WriteString(" Reason: " + strings.Join(res.Warnings, "; "))
	}
	b.WriteString(" Do NOT treat this as a complete result; to obtain one, raise the sandbox memory limit or change the execution approach, then retry.")
	return b.String()
}

// verificationCommandMarkers 命中即视为「验证型命令」（lint/test/build 等）——
// 其输出若不完整会对 LLM 决策造成致命误导，故享受 OOM 自动重试等强门禁。
var verificationCommandMarkers = []string{
	"golangci-lint", "go test", "go build", "go vet", "go run",
	"pytest", "pytest-", "npm test", "npm run build", "yarn test", "yarn build",
	"make test", "make build", "cargo test", "cargo build", "tox", "mvn test", "gradle test",
	"grep -c", "wc -l",
}

// isVerificationCommand 判断命令是否为验证型（大小写不敏感子串匹配）。
func isVerificationCommand(cmd string) bool {
	c := strings.ToLower(cmd)
	for _, mk := range verificationCommandMarkers {
		if strings.Contains(c, mk) {
			return true
		}
	}
	return false
}

// outputLimitPipeRE 匹配命令末尾用于「限制输出行数」的管道段，例如：
//
//	| head -300   | head -n 300   | head   | tail -20   | tail -n 20
//
// golangci-lint / go test 等命令经 LLM 自行追加这类管道时，若被测进程被 OOM /
// 信号杀死，子进程可能仍持有管道写端，导致 head/tail 永不退出、命令永久挂起
// （即「执行中」永不停）。而 sandbox 已按 MaxOutput 字节截断输出，该管道既冗余
// 又是挂死源，故在执行前将其剥离。
var outputLimitPipeRE = regexp.MustCompile(`(?i)\|\s*(?:head|tail)(?:\s+-n\s+\d+|\s+-\d+)?\s*$`)

// stripOutputLimitingPipe 若命令以 `| head`/`| tail` 结尾则剥离该管道段，
// 返回（清理后的命令, 是否剥离了管道）。被剥离的部分对结果无实质影响
// （sandbox 已按 MaxOutput 截断），但能消除挂死风险。
func stripOutputLimitingPipe(cmd string) (string, bool) {
	if outputLimitPipeRE.MatchString(cmd) {
		cleaned := outputLimitPipeRE.ReplaceAllString(cmd, "")
		// 去掉管道前的多余空白（避免留下孤立的 "| "）。
		cleaned = strings.TrimRight(cleaned, " \t|")
		return strings.TrimSpace(cleaned), true
	}
	return cmd, false
}

// searchMatch 是 search_content 单条匹配结果。
type searchMatch struct {
	File    string `json:"file"`
	Line    int    `json:"line"`
	Content string `json:"content"`
}

// parseSearchMatches 解析 ripgrep / grep 的 "path:line:content" 输出。
// rg 与 grep -n 输出格式一致（file:line:content），故同一解析逻辑复用。
func parseSearchMatches(stdout string, maxResults int) []searchMatch {
	matches := make([]searchMatch, 0)
	if stdout == "" {
		return matches
	}
	for _, line := range strings.Split(stdout, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		// 格式: path:line:content
		parts := strings.SplitN(line, ":", 3)
		if len(parts) < 3 {
			continue
		}
		var lineNum int
		_, _ = fmt.Sscanf(parts[1], "%d", &lineNum)
		matches = append(matches, searchMatch{
			File:    parts[0],
			Line:    lineNum,
			Content: parts[2],
		})
		if len(matches) >= maxResults {
			break
		}
	}
	return matches
}
