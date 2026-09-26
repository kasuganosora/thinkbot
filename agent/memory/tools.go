package memory

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/kasuganosora/thinkbot/agent/tools"
	"github.com/kasuganosora/thinkbot/llm"
	"github.com/kasuganosora/thinkbot/util/errs"
	"github.com/kasuganosora/thinkbot/util/idgen"
)

// leadingMentionPrefixRE 匹配模型偶发在记忆正文前加的空 mention 前缀
// （如 "[] " 或 "[] [] "）。这类前缀无语义，却会让同一观察因格式不一致产生
// 近重复记忆（memory_entries 实测 ~80 条带 "[]" 前缀的噪音，甚至累积成 "[] []"）。
// 写入前统一剥离，保证内容规范。
var leadingMentionPrefixRE = regexp.MustCompile(`^(\[\]\s*)+`)

// ============================================================================
// 记忆管理工具 — 单一压缩工具设计
//
// 将原先 5 个独立工具合并为单个 `memory` 工具，通过 action 参数分发。
// 显著减少 LLM 上下文中的工具 schema token 开销。
//
// 支持的操作：
//   - search:  搜索记忆
//   - add:     添加记忆（带威胁扫描）
//   - replace: 替换记忆（子串匹配，非 ID）
//   - remove:  删除记忆（子串匹配，非 ID）
//   - recent:  获取最近记忆
//   - count:   查询记忆数量
//   - batch:   批量原子操作（add+replace+remove 一次完成）
// ============================================================================

// memoryToolsPromptSection 是记忆工具的统一提示词段落。
var memoryToolsPromptSection = &tools.ToolPromptSection{
	Name:  "memory_tools",
	Order: 310,
	Content: `# Memory management

You have persistent memory. Use the ` + "`memory`" + ` tool to save, search and curate memories that survive across sessions.

## When to use it

- **Save proactively**: whenever the user states a preference, corrects you, or shares personal information, save it without being asked.
- **Search**: whenever you need to recall something from an earlier conversation.
- **Delete stale entries**: whenever you find a memory that is outdated or wrong.
- **Curate in bulk**: when you need to add and remove several entries, do it in a single ` + "`batch`" + ` operation.

## Time-span questions (read this before answering "when" questions)

Results are **always truncated by limit**, and the default order is newest-first. That means:

- "What is your earliest memory?" / "When did we first talk?" → action "search" with order="oldest" and limit=5.
  Do NOT answer from action "recent", and do NOT answer from the memories already in your system prompt —
  both only show you the recent tail, so you would report September when the real answer is August.
- "What happened in August?" → action "search" with since="2026-08-01", until="2026-08-31", order="oldest".
- Questions about your memory as a whole (not this conversation) → add scope_kind="all".
  The current channel is only one slice of your memory; a single-scope "earliest" is that channel's start, not yours.
- action "count" reports oldest_at / newest_at when available — use it to state a span precisely instead of guessing.

## Best practices

- A write takes effect in the system prompt of the **next** turn, not the current one.
- Store only what has long-term value. Priority: user preferences > environment facts > procedures.
- **Delete by ID is preferred**: ` + "`remove`" + ` accepts ` + "`id`" + ` (or ` + "`memory_id`" + `) for an unambiguous delete; this works even if the entry lives in a different scope (e.g. a bot-scope heartbeat note seen from a channel session). Use ` + "`search`" + ` first to get the exact ` + "`id`" + `.
- **Substring remove**: if ` + "`old_text`" + ` matches nothing in the current scope but DOES match in another scope, the tool tells you the ` + "`id`" + ` and ` + "`scope`" + ` to retry with — do not give up.
- When the store is full, use ` + "`batch`" + `: remove or replace old entries to free space first, then add the new ones.
- NEVER store information that is trivially rediscoverable, raw data dumps, or short-lived TODOs.`,
	Enabled: true,
}

// ToolConfig 配置记忆工具。
type ToolConfig struct {
	// Repo 记忆仓储（必须提供）。
	Repo Repository
	// Snapshot 可选的记忆快照引用。设置后，写入操作会自动调用 MarkDirty()，
	// 使下一轮系统提示反映最新记忆（仅 ModeLive/ModePeriodic 生效）。
	Snapshot *Snapshot
	// DefaultScopeKind 默认 scope 类型（默认 "channel"）。
	DefaultScopeKind ScopeKind
	// DefaultScopeID 默认 scope ID（默认空，使用会话的 channel/user ID）。
	// 通常留空，让 LLM 在参数中提供。
	DefaultScopeID string
	// BotID 当前 bot 标识。设置后，写入 channel 作用域的记忆会额外镜像一份到
	// bot 全局作用域（BotScope），使任意频道的会话都能在召回时看到其他平台的活动，
	// 解决跨平台记忆不可见问题。空值则不做镜像（保持旧行为）。
	BotID string
	// MaxMemoryChars memory（agent 笔记）的字符上限（默认 2200）。
	MaxMemoryChars int
	// MaxUserChars user（用户画像）的字符上限（默认 1375）。
	MaxUserChars int
	// EntrySeparator 条目分隔符（默认 "\n§\n"）。
	EntrySeparator string
}

// DefaultToolConfig 返回默认配置。
func DefaultToolConfig(repo Repository) ToolConfig {
	return ToolConfig{
		Repo:             repo,
		DefaultScopeKind: ScopeChannel,
		MaxMemoryChars:   2200,
		MaxUserChars:     1375,
		EntrySeparator:   "\n§\n",
	}
}

// markDirty 如果配置了 Snapshot 则标记快照为脏。
func (c *ToolConfig) markDirty() {
	if c.Snapshot != nil {
		c.Snapshot.MarkDirty()
	}
}

// Tools 返回记忆管理工具定义列表。
// 返回单个 `memory` 工具，通过 action 参数分发到不同操作。
func Tools(config ToolConfig) []tools.ToolDef {
	if config.Repo == nil {
		return nil
	}

	defaultKind := config.DefaultScopeKind
	if defaultKind == "" {
		defaultKind = ScopeChannel
	}
	defaultID := config.DefaultScopeID

	memoryTool := tools.ToolDef{
		Category:      "memory",
		Scopes:        []string{"private", "group"},
		PromptSection: memoryToolsPromptSection,
		Tool: tools.BuildTool(
			"memory",
			"Manage persistent memory that survives across sessions. "+
				"Use action to add/replace/remove/search/recent/count/batch. "+
				"Memory is injected into future sessions, keep entries compact and high-signal. "+
				"When making multiple changes, use batch (all-or-nothing against final char budget). For replace: ALWAYS pass both old_text AND content.",
			map[string]any{
				"type": "object",
				"properties": map[string]any{
					"action": map[string]any{
						"type":        "string",
						"enum":        []string{"add", "replace", "remove", "search", "recent", "count", "batch"},
						"description": "Operation to perform. Use 'batch' for atomic multi-op (add+replace+remove in one call).",
					},
					"content": map[string]any{
						"type":        "string",
						"description": "REQUIRED for action=add and action=replace — the full new entry text. For replace you MUST pass both old_text (locator) AND content (replacement). Omitting content on replace fails; use action=remove to delete.",
					},
					"old_text": map[string]any{
						"type":        "string",
						"description": "Short unique substring identifying the entry to replace or remove.",
					},
					"query": map[string]any{
						"type":        "string",
						"description": "Search keyword for 'search' action.",
					},
					"limit": map[string]any{
						"type":        "integer",
						"description": "Max results for 'search'/'recent'. Default: 10 (search), 5 (recent).",
						"default":     10,
					},
					"order": map[string]any{
						"type": "string",
						"enum": []string{"newest", "oldest"},
						"description": "Time order for 'search'/'recent'/'list'. Default 'newest'. " +
							"Use 'oldest' for time-span questions (\"what is your earliest memory\", \"what happened in August\") — " +
							"results are always truncated, so 'newest' can only ever see the recent tail.",
					},
					"since": map[string]any{
						"type":        "string",
						"description": "Only entries created at or after this time. Accepts 'YYYY-MM-DD' or RFC3339 (e.g. '2026-08-01').",
					},
					"until": map[string]any{
						"type":        "string",
						"description": "Only entries created at or before this time. Same formats as 'since'.",
					},
					"category": map[string]any{
						"type":        "string",
						"description": "Category for 'add'. Options: fact, preference, event, observation. Default: observation.",
						"default":     "observation",
					},
					"operations": map[string]any{
						"type": "array",
						"description": "Batch operations array. Each item: {action: add|replace|remove, content?, old_text?}. " +
							"Applied atomically against final char budget. Use to free space + add in one call.",
						"items": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"action": map[string]any{
									"type": "string",
									"enum": []string{"add", "replace", "remove"},
								},
								"content": map[string]any{
									"type":        "string",
									"description": "Entry content for add/replace.",
								},
								"old_text": map[string]any{
									"type":        "string",
									"description": "Substring identifying entry for replace/remove.",
								},
							},
							"required": []string{"action"},
						},
					},
					"scope_kind": map[string]any{
						"type": "string",
						"description": "Memory scope. Options: channel (default), user, bot, global, all. " +
							"'all' searches/counts across every scope — required for questions about your memory as a whole " +
							"(\"your earliest memory\"), since the current channel is only one slice of it.",
						"default": "channel",
					},
					"scope_id": map[string]any{
						"type":        "string",
						"description": "Scope identifier (channel ID, user ID). Empty for global.",
					},
					"memory_id": map[string]any{
						"type":        "string",
						"description": "Entry ID for remove by ID (alternative to old_text substring match).",
					},
				},
				"required": []string{"action"},
			},
			func(ctx *llm.ToolExecContext, input any) (any, error) {
				m, ok := input.(map[string]any)
				if !ok {
					return nil, fmt.Errorf("invalid input: expected object")
				}

				action, _ := m["action"].(string)
				if action == "" {
					return nil, fmt.Errorf("action is required")
				}

				repo := config.Repo
				scope := parseScope(m, defaultKind, defaultID)

				// 护栏：scope_kind="all" 只是**读取**用的通配符（跨 scope 检索/统计），
				// 不能当作写入目标 —— 否则会凭空造出一个 kind="all" 的孤儿 scope，
				// 里面的记忆以后谁也搜不到（写入和查询的 scope 对不上）。
				if scope.Kind == ScopeAll && action != "search" && action != "recent" && action != "count" {
					return nil, fmt.Errorf(
						"scope_kind 'all' is read-only (search/recent/count); "+
							"'%s' needs a concrete scope: channel, user, bot or global", action)
				}

				switch action {
				case "search":
					return handleSearch(ctx, repo, scope, m)

				case "add":
					result, err := handleAdd(ctx, repo, config, scope, m)
					if err == nil {
						config.markDirty()
					}
					return result, err

				case "replace":
					result, err := handleReplace(ctx, repo, config, scope, m)
					if err == nil {
						config.markDirty()
					}
					return result, err

				case "remove":
					result, err := handleRemove(ctx, repo, config, scope, m)
					if err == nil {
						config.markDirty()
					}
					return result, err

				case "recent":
					return handleRecent(ctx, repo, scope, m)

				case "count":
					return handleCount(ctx, repo, scope, m)

				case "batch":
					result, err := handleBatch(ctx, repo, config, scope, m)
					if err == nil {
						config.markDirty()
					}
					return result, err

				default:
					return nil, fmt.Errorf("unknown action '%s'. Use: add, replace, remove, search, recent, count, batch", action)
				}
			},
		),
	}

	return []tools.ToolDef{memoryTool}
}

// RegisterTools 将记忆工具注册到 ToolManager。
func RegisterTools(mgr *tools.ToolManager, config ToolConfig) error {
	defs := Tools(config)
	if len(defs) == 0 {
		return fmt.Errorf("memory tools require a non-nil repository")
	}
	return mgr.RegisterMany(defs...)
}

// ============================================================================
// Action handlers
// ============================================================================

func handleSearch(ctx *llm.ToolExecContext, repo Repository, scope Scope, m map[string]any) (any, error) {
	queryText, _ := m["query"].(string)
	category, _ := m["category"].(string)
	limit := 10
	if l := toInt(m["limit"]); l > 0 {
		limit = l
	}
	if limit > 50 {
		limit = 50
	}

	since, err := parseTimeParam(m["since"])
	if err != nil {
		return nil, err
	}
	until, err := parseTimeParam(m["until"])
	if err != nil {
		return nil, err
	}

	entries, err := repo.Retrieve(ctx, Query{
		Scopes:   scopesFor(scope),
		Text:     queryText,
		Category: category,
		Limit:    limit,
		Order:    orderFor(m),
		Since:    since,
		Until:    until,
	})
	if err != nil {
		return nil, errs.Wrap(err, "memory search failed")
	}

	return formatEntries(entries, "search"), nil
}

func handleAdd(ctx *llm.ToolExecContext, repo Repository, cfg ToolConfig, scope Scope, m map[string]any) (any, error) {
	content, _ := m["content"].(string)
	if content == "" {
		return nil, fmt.Errorf("content is required for 'add' action")
	}
	content = StripThinking(content)
	content = StripToolOutput(content)
	// 归一化：剥离模型偶发在正文前加的空 mention 前缀（"[] " / "[] [] "），
	// 避免同一观察因前缀不一致产生近重复记忆。
	content = leadingMentionPrefixRE.ReplaceAllString(content, "")
	content = strings.TrimSpace(content)

	// 威胁扫描
	if findings := ScanMemoryThreats(content); len(findings) > 0 {
		return map[string]any{
			"success": false,
			"error":   "Content blocked by security scan: " + ThreatSummary(findings),
		}, nil
	}

	category, _ := m["category"].(string)
	if category == "" {
		category = "observation"
	}

	importance := 0.5
	if imp, ok := m["importance"].(float64); ok {
		importance = imp
	}

	entry := Entry{
		Scope:      scope,
		Content:    content,
		Category:   category,
		Source:     "tool",
		Importance: importance,
	}

	// 预生成 ID，供跨平台镜像使用确定性镜像 ID（Append 对非空 ID 直接沿用）。
	entry.ID = idgen.New("mem")
	if err := repo.Append(ctx, entry); err != nil {
		return nil, errs.Wrap(err, "memory write failed")
	}

	// 跨平台镜像：channel 记忆额外写入 bot 全局作用域，使其他频道也能召回。
	mirrorChannelMemoryToBot(ctx, repo, cfg, entry.ID, entry)

	return successResponse(scope, "Entry added.", true), nil
}

func handleReplace(ctx *llm.ToolExecContext, repo Repository, cfg ToolConfig, scope Scope, m map[string]any) (any, error) {
	oldText, _ := m["old_text"].(string)
	content, _ := m["content"].(string)

	if oldText == "" {
		return nil, fmt.Errorf("old_text is required for 'replace' action")
	}
	if content == "" {
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		return nil, fmt.Errorf("content is required for 'replace' (use 'remove' to delete); got keys %v — pass content=<new text> together with old_text", keys)
	}
	content = StripThinking(content)

	// 威胁扫描
	if findings := ScanMemoryThreats(content); len(findings) > 0 {
		return map[string]any{
			"success": false,
			"error":   "Content blocked by security scan: " + ThreatSummary(findings),
		}, nil
	}

	// 子串匹配查找
	entries, err := repo.Retrieve(ctx, Query{
		Scopes: []Scope{scope},
		Limit:  100,
	})
	if err != nil {
		return nil, errs.Wrap(err, "memory search failed")
	}

	matches := findSubstringMatches(entries, oldText)
	if len(matches) == 0 {
		return map[string]any{
			"success": false,
			"error":   fmt.Sprintf("No entry matched '%s'.", oldText),
		}, nil
	}
	if len(matches) > 1 {
		return map[string]any{
			"success": false,
			"error":   fmt.Sprintf("Multiple entries matched '%s'. Be more specific.", oldText),
			"matches": previewEntries(matches),
		}, nil
	}

	// 替换：优先使用原子性 Replace（如果实现支持），否则降级为 Append-before-Delete
	old := matches[0]
	newEntry := Entry{
		ID:             old.ID,
		Scope:          scope,
		Content:        content,
		Category:       old.Category,
		Source:         old.Source,
		Importance:     old.Importance,
		Metadata:       old.Metadata,
		CreatedAt:      old.CreatedAt,
		LastAccessedAt: old.LastAccessedAt,
	}

	if replacer, ok := repo.(Replacer); ok {
		if err := replacer.Replace(ctx, scope, old.ID, newEntry); err != nil {
			return nil, errs.Wrap(err, "replace failed")
		}
	} else {
		// 降级路径：后端没有原子替换能力时，先删旧条目再写新条目。
		// 顺序不能颠倒 —— 新条目复用同一个 ID，先 Append 走的是 INSERT，
		// 必然撞上主键唯一约束而永久失败。写入失败则把原条目插回，避免记忆丢失。
		newEntry.ID = old.ID
		if err := repo.Delete(ctx, scope, old.ID); err != nil {
			return nil, errs.Wrap(err, "replace: delete of old entry failed")
		}
		if err := repo.Append(ctx, newEntry); err != nil {
			if restoreErr := repo.Append(ctx, old); restoreErr != nil {
				return nil, errs.Wrap(err, "replace write failed and the original entry could not be restored")
			}
			return nil, errs.Wrap(err, "replace write failed (original entry restored)")
		}
	}

	// 跨平台镜像：更新 bot 全局作用域下对应的镜像（不存在则新建）。
	mirrorChannelMemoryToBot(ctx, repo, cfg, old.ID, newEntry)

	return successResponse(scope, "Entry replaced.", true), nil
}

func handleRemove(ctx *llm.ToolExecContext, repo Repository, cfg ToolConfig, scope Scope, m map[string]any) (any, error) {
	// 支持 memory_id / id（按 ID 删除，跨 scope 解析）和 old_text（子串匹配）两种方式
	memoryID, _ := m["memory_id"].(string)
	if memoryID == "" {
		memoryID, _ = m["id"].(string)
	}
	oldText, _ := m["old_text"].(string)

	if memoryID != "" {
		homeScope, ok := resolveRemoveByID(ctx, repo, cfg, scope, memoryID)
		if !ok {
			return map[string]any{
				"success": false,
				"error":   fmt.Sprintf("No memory with id '%s' found in scope %s or other accessible scopes (bot/global).", memoryID, scope.Key()),
			}, nil
		}
		if err := repo.Delete(ctx, homeScope, memoryID); err != nil {
			return nil, errs.Wrap(err, "memory delete failed")
		}
		// 跨平台镜像：同步删除 bot 全局作用域下的镜像。
		if homeScope.Kind == ScopeChannel {
			unmirrorChannelMemoryFromBot(ctx, repo, cfg, memoryID)
		}
		return map[string]any{
			"success":   true,
			"message":   "Memory deleted.",
			"memory_id": memoryID,
			"scope":     homeScope.Key(),
		}, nil
	}

	if oldText == "" {
		return nil, fmt.Errorf("old_text or id/memory_id is required for 'remove' action")
	}

	entries, err := repo.Retrieve(ctx, Query{
		Scopes: []Scope{scope},
		Limit:  100,
	})
	if err != nil {
		return nil, errs.Wrap(err, "memory search failed")
	}

	matches := findSubstringMatches(entries, oldText)
	if len(matches) == 0 {
		// 子串在别的 scope（如 bot 全局）命中时给出可执行指引，避免死路。
		if others := probeOtherScopesForSubstring(ctx, repo, cfg, scope, oldText); len(others) > 0 {
			return map[string]any{
				"success": false,
				"error":   removeScopeHint(scope, oldText, others),
			}, nil
		}
		return map[string]any{
			"success": false,
			"error":   fmt.Sprintf("No entry matched '%s'.", oldText),
		}, nil
	}
	if len(matches) > 1 {
		return map[string]any{
			"success": false,
			"error":   fmt.Sprintf("Multiple entries matched '%s'. Be more specific.", oldText),
			"matches": previewEntries(matches),
		}, nil
	}

	if err := repo.Delete(ctx, scope, matches[0].ID); err != nil {
		return nil, errs.Wrap(err, "delete failed")
	}

	// 跨平台镜像：同步删除 bot 全局作用域下的镜像。
	if scope.Kind == ScopeChannel {
		unmirrorChannelMemoryFromBot(ctx, repo, cfg, matches[0].ID)
	}

	return successResponse(scope, "Entry removed.", true), nil
}

func handleRecent(ctx *llm.ToolExecContext, repo Repository, scope Scope, m map[string]any) (any, error) {
	limit := 5
	if l := toInt(m["limit"]); l > 0 {
		limit = l
	}
	if limit > 20 {
		limit = 20
	}

	// 走 Retrieve 而非 repo.Recent：Recent 语义上只能取最新的 N 条，无法表达
	// order=oldest / 时间范围。带上 order + since/until 后 recent 才覆盖得住
	// 「最早那几条」这类问法。
	since, err := parseTimeParam(m["since"])
	if err != nil {
		return nil, err
	}
	until, err := parseTimeParam(m["until"])
	if err != nil {
		return nil, err
	}

	entries, err := repo.Retrieve(ctx, Query{
		Scopes: scopesFor(scope),
		Limit:  limit,
		Order:  orderFor(m),
		Since:  since,
		Until:  until,
	})
	if err != nil {
		return nil, errs.Wrap(err, "memory recent failed")
	}

	return formatEntries(entries, "recent"), nil
}

func handleCount(ctx *llm.ToolExecContext, repo Repository, scope Scope, m map[string]any) (any, error) {
	// scope_kind=all 时跨 scope 统计：单 scope 的 Count 无法回答
	// 「我一共有多少记忆 / 最早是哪天」这类整体问题。
	if provider, ok := repo.(MemoryStatsProvider); ok && scope.Kind == ScopeAll {
		if info, err := provider.MemoryStats(ctx, nil); err == nil {
			return map[string]any{
				"scope":     scope.Key(),
				"count":     info.Total,
				"oldest_at": formatEntryTime(info.Oldest),
				"newest_at": formatEntryTime(info.Newest),
				"hint":      "Time span across all scopes. Use search with order:'oldest' to read the earliest entries.",
			}, nil
		}
	}

	count, err := repo.Count(ctx, scope)
	if err != nil {
		return nil, errs.Wrap(err, "memory count failed")
	}

	resp := map[string]any{
		"scope": scope.Key(),
		"count": count,
	}
	// 时间跨度：让「最早的记忆是什么时候」这类问题有权威答案，而不必让模型
	// 从截断后的列表里猜。后端不支持统计时静默省略（可选能力）。
	if provider, ok := repo.(MemoryStatsProvider); ok {
		if info, err := provider.MemoryStats(ctx, scopesFor(scope)); err == nil {
			resp["oldest_at"] = formatEntryTime(info.Oldest)
			resp["newest_at"] = formatEntryTime(info.Newest)
		}
	}
	return resp, nil
}

func handleBatch(ctx *llm.ToolExecContext, repo Repository, cfg ToolConfig, scope Scope, m map[string]any) (any, error) {
	opsRaw, ok := m["operations"]
	if !ok {
		return nil, fmt.Errorf("operations is required for 'batch' action")
	}

	opsList, ok := opsRaw.([]any)
	if !ok {
		return nil, fmt.Errorf("operations must be an array")
	}

	if len(opsList) == 0 {
		return nil, fmt.Errorf("operations list is empty")
	}

	// 先扫描所有 add/replace 内容的威胁模式
	for i, opRaw := range opsList {
		op, ok := opRaw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("operation %d: expected object", i+1)
		}
		opAction, _ := op["action"].(string)
		if opAction == "add" || opAction == "replace" {
			if content, _ := op["content"].(string); content != "" {
				if findings := ScanMemoryThreats(content); len(findings) > 0 {
					return map[string]any{
						"success": false,
						"error":   fmt.Sprintf("Operation %d blocked by security scan: %s", i+1, ThreatSummary(findings)),
					}, nil
				}
			}
		}
	}

	// 获取当前所有条目
	entries, err := repo.Retrieve(ctx, Query{
		Scopes: []Scope{scope},
		Limit:  1000,
	})
	if err != nil {
		return nil, errs.Wrap(err, "batch: retrieval failed")
	}

	// 在副本上验证操作（用于幂等检查和匹配验证）
	working := make([]Entry, len(entries))
	copy(working, entries)

	appliedCount := 0

	// 提交变更：记录需要添加、修改、删除的条目
	type batchChange struct {
		op          string // "add", "update", "delete"
		entry       *Entry // new entry for add/update
		target      string // existing entry ID for delete/update
		targetScope Scope  // home scope for delete (may differ from requested scope)
		origEntry   *Entry // original entry for update (preserves metadata)
	}
	var changes []batchChange

	for i, opRaw := range opsList {
		op := opRaw.(map[string]any)
		opAction, _ := op["action"].(string)
		opContent, _ := op["content"].(string)
		opContent = StripThinking(opContent)
		opOldText, _ := op["old_text"].(string)
		pos := fmt.Sprintf("Operation %d (%s)", i+1, opAction)

		switch opAction {
		case "add":
			if opContent == "" {
				return batchError(scope, pos+": content is required"), nil
			}
			// 幂等：跳过已存在的
			exists := false
			for _, e := range working {
				if e.Content == opContent {
					exists = true
					break
				}
			}
			if !exists {
				changes = append(changes, batchChange{op: "add", entry: &Entry{
					Scope:    scope,
					Content:  opContent,
					Category: "observation",
					Source:   "tool",
				}})
			}
			appliedCount++

		case "replace":
			if opOldText == "" {
				return batchError(scope, pos+": old_text is required"), nil
			}
			if opContent == "" {
				return batchError(scope, pos+": content is required (use remove to delete)"), nil
			}
			matches := findSubstringMatches(working, opOldText)
			if len(matches) == 0 {
				return batchError(scope, pos+": no entry matched '"+opOldText+"'"), nil
			}
			if len(matches) > 1 {
				return batchError(scope, pos+": '"+opOldText+"' matched multiple entries"), nil
			}
			targetID := matches[0].ID
			orig := matches[0]
			changes = append(changes, batchChange{
				op:        "update",
				target:    targetID,
				origEntry: &orig,
				entry: &Entry{
					Scope:    scope,
					Content:  opContent,
					Category: orig.Category,
					Source:   orig.Source,
				},
			})
			// 更新 working 副本以支持后续操作匹配
			for j := range working {
				if working[j].ID == targetID {
					working[j].Content = opContent
					break
				}
			}
			appliedCount++

		case "remove":
			opID, _ := op["id"].(string)
			if opID == "" {
				opID, _ = op["memory_id"].(string)
			}
			if opID != "" {
				homeScope, ok := resolveRemoveByID(ctx, repo, cfg, scope, opID)
				if !ok {
					return batchError(scope, pos+": no entry with id '"+opID+"' in any accessible scope (requested "+scope.Key()+", bot, global)"), nil
				}
				changes = append(changes, batchChange{op: "delete", target: opID, targetScope: homeScope})
				// 更新 working 副本以支持后续操作匹配
				newWorking := make([]Entry, 0, len(working))
				for _, e := range working {
					if e.ID != opID {
						newWorking = append(newWorking, e)
					}
				}
				working = newWorking
				appliedCount++
				break
			}
			if opOldText == "" {
				return batchError(scope, pos+": old_text or id is required"), nil
			}
			matches := findSubstringMatches(working, opOldText)
			if len(matches) == 0 {
				if others := probeOtherScopesForSubstring(ctx, repo, cfg, scope, opOldText); len(others) > 0 {
					return batchError(scope, pos+": "+removeScopeHint(scope, opOldText, others)), nil
				}
				return batchError(scope, pos+": no entry matched '"+opOldText+"'"), nil
			}
			if len(matches) > 1 {
				return batchError(scope, pos+": '"+opOldText+"' matched multiple entries"), nil
			}
			targetID := matches[0].ID
			changes = append(changes, batchChange{op: "delete", target: targetID})
			// 更新 working 副本以支持后续操作匹配
			newWorking := make([]Entry, 0, len(working))
			for _, e := range working {
				if e.ID != targetID {
					newWorking = append(newWorking, e)
				}
			}
			working = newWorking
			appliedCount++

		default:
			return batchError(scope, pos+": unknown action '"+opAction+"'. Use add, replace, remove"), nil
		}
	}

	// 提交变更到持久层
	commitErrorCount := 0
	for _, ch := range changes {
		switch ch.op {
		case "add":
			// 预生成 ID 供跨平台镜像使用确定性镜像 ID。
			ch.entry.ID = idgen.New("mem")
			if err := repo.Append(ctx, *ch.entry); err != nil {
				commitErrorCount++
			} else {
				mirrorChannelMemoryToBot(ctx, repo, cfg, ch.entry.ID, *ch.entry)
			}
		case "update":
			// 使用保存的原始条目元数据，避免重复查询
			if ch.origEntry != nil {
				e := ch.origEntry
				updatedEntry := Entry{
					ID:             ch.target,
					Scope:          ch.entry.Scope,
					Content:        ch.entry.Content,
					Category:       ch.entry.Category,
					Source:         ch.entry.Source,
					Importance:     e.Importance,
					Metadata:       e.Metadata,
					CreatedAt:      e.CreatedAt,
					LastAccessedAt: e.LastAccessedAt,
				}
				if replacer, ok := repo.(Replacer); ok {
					if err := replacer.Replace(ctx, scope, ch.target, updatedEntry); err != nil {
						commitErrorCount++
					} else {
						mirrorChannelMemoryToBot(ctx, repo, cfg, ch.target, updatedEntry)
					}
				} else {
					// 降级路径：同 handleReplace —— 必须先删后写，否则复用同一 ID
					// 的 INSERT 必然违反唯一约束。写入失败则把原条目插回。
					updatedEntry.ID = ch.target
					if err := repo.Delete(ctx, scope, ch.target); err != nil {
						commitErrorCount++
					} else if err := repo.Append(ctx, updatedEntry); err != nil {
						commitErrorCount++
						_ = repo.Append(ctx, *e)
					} else {
						mirrorChannelMemoryToBot(ctx, repo, cfg, ch.target, updatedEntry)
					}
				}
			}
		case "delete":
			delScope := ch.targetScope
			if delScope.Kind == "" {
				delScope = scope
			}
			if err := repo.Delete(ctx, delScope, ch.target); err != nil {
				commitErrorCount++
			} else if delScope.Kind == ScopeChannel {
				unmirrorChannelMemoryFromBot(ctx, repo, cfg, ch.target)
			}
		}
	}

	// 提交阶段有失败就不能报成功：successResponse 会附带「写入已保存，别重复」的
	// 提示，若此时谎报成功，模型会认为记忆已更新而不再重试，导致静默丢数据。
	if commitErrorCount > 0 {
		return map[string]any{
			"success": false,
			"scope":   scope.Key(),
			"error": fmt.Sprintf("%d of %d operation(s) failed to persist; %d succeeded. Retry the failed ones.",
				commitErrorCount, appliedCount, appliedCount-commitErrorCount),
		}, nil
	}

	return successResponse(scope, fmt.Sprintf("Applied %d operation(s).", appliedCount), true), nil
}

// ============================================================================
// Helpers
// ============================================================================

func parseScope(m map[string]any, defaultKind ScopeKind, defaultID string) Scope {
	kindStr, _ := m["scope_kind"].(string)
	if kindStr == "" {
		kindStr = string(defaultKind)
	}
	id, _ := m["scope_id"].(string)
	if id == "" {
		id = defaultID
	}
	return Scope{Kind: ScopeKind(kindStr), ID: id}
}

// ============================================================================
// 时间与排序参数解析
// ============================================================================
//
// 这一组能力是为「时间轴类问题」补的：检索结果永远被 limit 截断，而默认排序是
// 时间倒序 —— 于是模型无论问「最早的记忆」「8 月发生过什么」都只能看到最近的一批，
// 然后把「最近 N 条里最早的」当成「全部最早」回答（实测真实最早 2026-08-12
// 被答成「9 月」）。order=asc + since/until 让模型能真正走到时间轴的另一端。

// scopesFor 把解析出的 scope 转成 Query.Scopes。
// ScopeAll 返回空切片（检索/统计全部 scope）；其余按单 scope 精确限定。
func scopesFor(scope Scope) []Scope {
	if scope.Kind == ScopeAll {
		return nil
	}
	return []Scope{scope}
}

// orderFor 解析 order 参数（"newest" / "oldest"），默认 newest。
func orderFor(m map[string]any) string {
	if o, _ := m["order"].(string); o == "oldest" {
		return OrderAsc
	}
	return OrderDesc
}

// timeParamLayouts 接受的时间格式：完整 RFC3339 与常用日期/日期时间简写。
// 简写按**本地时区**解释 —— bot 和用户说「8 月」指的是本地日历，用 UTC 会把
// 月初/月末的几个小时算错月份。
var timeParamLayouts = []string{
	time.RFC3339,
	"2006-01-02 15:04:05",
	"2006-01-02 15:04",
	"2006-01-02",
}

// parseTimeParam 解析 since/until 参数，无法识别时返回错误（提示模型换格式）。
func parseTimeParam(v any) (time.Time, error) {
	s, _ := v.(string)
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, nil
	}
	for _, layout := range timeParamLayouts {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("cannot parse time %q; use RFC3339 (e.g. 2026-08-01T00:00:00+08:00) or 'YYYY-MM-DD'", s)
}

// ============================================================================
// 跨平台镜像：把 channel 作用域的记忆同步到 bot 全局作用域
// ============================================================================

// crossChannelMirrorPrefix 跨平台镜像条目 ID 的前缀。镜像 ID 由前缀 + 原始
// channel 条目 ID 组成，因此增/改/删都能稳定定位到同一条镜像，避免重复累积。
const crossChannelMirrorPrefix = "xch:"

// mirrorChannelMemoryToBot 将一条 channel 作用域的记忆镜像到 bot 全局作用域，
// 使任意频道的会话在召回（已加载 BotScope）时都能看到其他平台的活动。
//
// 仅在目标 scope 为 channel 且配置提供了 BotID 时生效；其他 scope 直接跳过。
// originalID 为原始 channel 记忆的条目 ID；entry 为要镜像的内容（原始或更新后）。
// 镜像正文以 [<channel>] 前缀标注来源频道，避免跨频道串台时丢失上下文。
func mirrorChannelMemoryToBot(ctx *llm.ToolExecContext, repo Repository, cfg ToolConfig, originalID string, entry Entry) {
	if cfg.BotID == "" || originalID == "" || entry.Scope.Kind != ScopeChannel {
		return
	}
	botScope := BotScope(cfg.BotID)
	mirror := Entry{
		ID:             crossChannelMirrorPrefix + originalID,
		Scope:          botScope,
		Content:        fmt.Sprintf("[%s] %s", entry.Scope.ID, entry.Content),
		Category:       entry.Category,
		Source:         entry.Source,
		Importance:     entry.Importance,
		Metadata:       entry.Metadata,
		CreatedAt:      entry.CreatedAt,
		LastAccessedAt: entry.LastAccessedAt,
	}
	// 镜像已存在则就地更新，不存在则退化为纯追加（Replace 对不存在的 deleteID 不报错）。
	if replacer, ok := repo.(Replacer); ok {
		_ = replacer.Replace(ctx, botScope, mirror.ID, mirror)
	} else {
		_ = repo.Append(ctx, mirror)
	}
}

// unmirrorChannelMemoryFromBot 删除 bot 全局作用域下对应的跨平台镜像（remove 时调用）。
func unmirrorChannelMemoryFromBot(ctx *llm.ToolExecContext, repo Repository, cfg ToolConfig, originalID string) {
	if cfg.BotID == "" || originalID == "" {
		return
	}
	_ = repo.Delete(ctx, BotScope(cfg.BotID), crossChannelMirrorPrefix+originalID)
}

func findSubstringMatches(entries []Entry, substr string) []Entry {
	var matches []Entry
	lower := strings.ToLower(substr)
	for _, e := range entries {
		if strings.Contains(strings.ToLower(e.Content), lower) {
			matches = append(matches, e)
		}
	}
	return matches
}

func previewEntries(entries []Entry) []string {
	previews := make([]string, 0, len(entries))
	for _, e := range entries {
		p := e.Content
		if len(p) > 80 {
			p = p[:80] + "..."
		}
		previews = append(previews, p)
	}
	return previews
}

// ============================================================================
// 跨 scope 删除辅助：让 bot 能清理自己写在别的 scope（如 bot 全局）的记忆
// ============================================================================
//
// 记忆工具按 scope 隔离：channel 会话默认只能读/写 channel scope，bot 心跳等机制
// 写入的记忆落在 bot scope。若 remove 只在请求 scope 内找，bot 无法清理自己别的
// scope 下的虚假/过期记忆（表现为 "no entry matched" 死路）。以下辅助在显式给出
// 确切 ID 时跨 scope 解析（授权明确，因为 ID 是 bot 经 search 拿到的），并在子串
// 匹配失败时给出可执行的 scope/id 指引，把死路变成可恢复操作。

// resolveRemoveByID locates a memory entry by exact ID, searching the requested scope
// first, then the bot and global scopes. Returning the entry's home scope lets the
// caller delete an entry that lives outside the current session scope.
func resolveRemoveByID(ctx context.Context, repo Repository, cfg ToolConfig, requested Scope, id string) (Scope, bool) {
	if _, ok := findEntryByIDInScope(ctx, repo, requested, id); ok {
		return requested, true
	}
	if cfg.BotID != "" {
		botScope := BotScope(cfg.BotID)
		if botScope.Key() != requested.Key() {
			if _, ok := findEntryByIDInScope(ctx, repo, botScope, id); ok {
				return botScope, true
			}
		}
	}
	globalScope := Scope{Kind: ScopeGlobal, ID: ""}
	if globalScope.Key() != requested.Key() {
		if _, ok := findEntryByIDInScope(ctx, repo, globalScope, id); ok {
			return globalScope, true
		}
	}
	return Scope{}, false
}

func findEntryByIDInScope(ctx context.Context, repo Repository, scope Scope, id string) (Entry, bool) {
	entries, err := repo.Retrieve(ctx, Query{Scopes: []Scope{scope}, Limit: 1000})
	if err != nil {
		return Entry{}, false
	}
	for _, e := range entries {
		if e.ID == id {
			return e, true
		}
	}
	return Entry{}, false
}

// probeOtherScopesForSubstring searches bot (and global) scopes for a substring that did
// not match in the requested scope, so callers can guide the model to the correct scope/id.
func probeOtherScopesForSubstring(ctx context.Context, repo Repository, cfg ToolConfig, requested Scope, substr string) []Entry {
	var out []Entry
	candidates := []Scope{}
	if cfg.BotID != "" {
		botScope := BotScope(cfg.BotID)
		if botScope.Key() != requested.Key() {
			candidates = append(candidates, botScope)
		}
	}
	globalScope := Scope{Kind: ScopeGlobal, ID: ""}
	if globalScope.Key() != requested.Key() {
		candidates = append(candidates, globalScope)
	}
	for _, s := range candidates {
		entries, err := repo.Retrieve(ctx, Query{Scopes: []Scope{s}, Limit: 1000})
		if err != nil {
			continue
		}
		out = append(out, findSubstringMatches(entries, substr)...)
	}
	return out
}

// removeScopeHint builds an actionable error when a substring matches in another scope.
func removeScopeHint(requested Scope, substr string, others []Entry) string {
	hints := make([]string, 0, len(others))
	for _, o := range others {
		hints = append(hints, fmt.Sprintf("id=%s scope=%s", o.ID, o.Scope.Key()))
	}
	return fmt.Sprintf("No entry matched '%s' in scope %s. Found %d in other scope(s): %s. Retry with id='<id>' (delete by exact ID works across scopes) or scope_kind='%s' scope_id='%s'.",
		substr, requested.Key(), len(others), strings.Join(hints, "; "), others[0].Scope.Kind, others[0].Scope.ID)
}

// EntryResult 是单条记忆的序列化结构。
type EntryResult struct {
	ID         string  `json:"id"`
	Content    string  `json:"content"`
	Category   string  `json:"category,omitempty"`
	Importance float64 `json:"importance,omitempty"`
	CreatedAt  string  `json:"created_at,omitempty"`
}

func formatEntries(entries []Entry, queryType string) any {
	if len(entries) == 0 {
		return map[string]any{
			"count":   0,
			"message": "No matching memory found.",
			"entries": []any{},
		}
	}

	results := make([]EntryResult, 0, len(entries))
	for _, e := range entries {
		results = append(results, EntryResult{
			ID:         e.ID,
			Content:    e.Content,
			Category:   e.Category,
			Importance: e.Importance,
			CreatedAt:  formatEntryTime(e.CreatedAt),
		})
	}

	return map[string]any{
		"count":   len(entries),
		"type":    queryType,
		"entries": results,
	}
}

func formatEntryTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format("2006-01-02 15:04")
}

// successResponse 构建 Terminal 成功响应。
// Terminal = 不回显完整条目列表，防止模型"找更多要修复"的 thrash。
func successResponse(scope Scope, message string, done bool) any {
	resp := map[string]any{
		"success": true,
		"scope":   scope.Key(),
	}
	if done {
		resp["done"] = true
	}
	if message != "" {
		resp["message"] = message
	}
	resp["note"] = "Write saved. This update is complete — do not repeat it."
	return resp
}

func batchError(scope Scope, message string) any {
	return map[string]any{
		"success": false,
		"error":   message + " No operations were applied (batch is all-or-nothing).",
		"scope":   scope.Key(),
	}
}

// toInt 从 any 值（int / float64 / int64 等）安全提取 int。
// JSON 反序列化后数字默认为 float64，但直接构造的 map 中可能是 int。
func toInt(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	default:
		return 0
	}
}
