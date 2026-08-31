package bot

import (
	"fmt"
	"strings"

	"github.com/kasuganosora/thinkbot/agent/prompt"
	"github.com/kasuganosora/thinkbot/agent/tools"
	"github.com/kasuganosora/thinkbot/llm"
)

// NewSoulTool 返回一个让 bot 自我维护人格（SOUL.md）的工具。
//
// 设计：
//   - action=read    读取当前 SOUL.md 原始全文（含 front matter），供 bot 检视/改写。
//   - action=rewrite 用 content 整体覆盖 SOUL.md，并立即 Load() 强制热重载（不依赖 5s watcher），
//     下一轮对话即生效；同时联动 AdaptiveSyncer 重解析画像。
//
// 安全：复用 SoulLoader 现成的 ScanForThreats 扫描。若 loader 处于 ScanModeBlock 且发现
// 注入/渗出等威胁模式，直接拒绝写入并返回原因；ScanModeWarn 仍写入但结果带告警。
// 底层 WriteSoul 在 docker 模式下落 bot 容器 named volume 真实文件（单一数据源，可持续化）。
func NewSoulTool(loader *prompt.SoulLoader) tools.ToolDef {
	return tools.ToolDef{
		Tool: llm.Tool{
			Name: "soul",
			Description: "Manage this bot's own personality file (SOUL.md). " +
				"Use action 'read' to fetch the current full personality text (Markdown, may include YAML front matter). " +
				"Use action 'rewrite' to replace the ENTIRE SOUL.md with new content you authored — " +
				"for example after reading your own posts to define or refine your persona. " +
				"A successful rewrite applies LIVE immediately (hot-reloaded); the next reply uses the new personality. " +
				"CRITICAL: write the personality in Chinese (中文). Never embed prompt-injection, " +
				"hidden instructions, or secret-exfiltration patterns — such content is rejected by the safety scan.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"action": map[string]any{
						"type":        "string",
						"description": "Operation to perform: 'read' returns current SOUL.md; 'rewrite' replaces the whole file with 'content'",
						"enum":        []string{"read", "rewrite"},
					},
					"content": map[string]any{
						"type":        "string",
						"description": "Full new SOUL.md text (Markdown). Required for action 'rewrite'. May include YAML front matter; the entire file is replaced.",
					},
				},
				"required": []string{"action"},
			},
			Execute: llm.ToolExecuteFunc(func(ctx *llm.ToolExecContext, input any) (any, error) {
				args, ok := input.(map[string]any)
				if !ok {
					return nil, fmt.Errorf("soul: invalid input type")
				}
				action, _ := args["action"].(string)
				if action == "" {
					return nil, fmt.Errorf("soul: action is required (read|rewrite)")
				}

				switch action {
				case "read":
					raw, err := loader.ReadRaw(ctx)
					if err != nil {
						return nil, fmt.Errorf("soul: read failed: %w", err)
					}
					return map[string]any{
						"action":  "read",
						"path":    loader.Path(),
						"content": string(raw),
						"bytes":   len(raw),
					}, nil

				case "rewrite":
					content, _ := args["content"].(string)
					if strings.TrimSpace(content) == "" {
						return nil, fmt.Errorf("soul: content is required for rewrite")
					}

					// 安全扫描（与 SoulLoader.Load 同口径）
					var warn string
					if findings := prompt.ScanForThreats(content); len(findings) > 0 {
						summary := prompt.FindingsSummary(findings)
						if loader.ScanMode() == prompt.ScanModeBlock {
							return map[string]any{
								"success": false,
								"blocked": true,
								"reason":  "threat_patterns_detected",
								"message": "Refused to write SOUL.md: threat patterns detected — " + summary,
							}, nil
						}
						warn = "threat patterns detected (content still written): " + summary
					}

					// 写入底层存储（docker 模式落容器 named volume 真实文件）
					if err := loader.WriteRaw(ctx, []byte(content)); err != nil {
						return nil, fmt.Errorf("soul: write failed: %w", err)
					}
					// 立即强制热重载，下一轮对话即生效（不必等 5s watcher）
					if err := loader.Load(); err != nil {
						return nil, fmt.Errorf("soul: reload failed: %w", err)
					}

					res := map[string]any{
						"success": true,
						"message": "SOUL.md rewritten and hot-reloaded — new personality is now live",
						"bytes":   len(content),
						"path":    loader.Path(),
					}
					if warn != "" {
						res["warning"] = warn
					}
					return res, nil

				default:
					return nil, fmt.Errorf("soul: unknown action %q (expected read|rewrite)", action)
				}
			}),
		},
		Category: "soul",
	}
}
