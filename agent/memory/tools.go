package memory

import (
	"fmt"
	"strings"
	"time"

	"github.com/kasuganosora/thinkbot/agent/tools"
	"github.com/kasuganosora/thinkbot/llm"
	"github.com/kasuganosora/thinkbot/util/errs"
)

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
	Content: `# 记忆管理

你拥有持久记忆能力。使用 ` + "`memory`" + ` 工具保存、搜索和管理跨会话的记忆。

## 何时使用

- **主动保存**：当用户陈述偏好、纠正或个人信息时，主动保存
- **搜索记忆**：当你需要回忆之前对话中的信息时搜索
- **删除过时记忆**：当发现记忆过时或不准确时删除
- **批量整理**：当需要同时添加+删除多个条目时，用 batch 操作一次完成

## 最佳实践

- 写入的记忆会在**下一轮对话**的系统提示中自动生效
- 只存储有长期价值的信息：用户偏好 > 环境事实 > 流程
- **子串操作**：replace 和 remove 使用唯一子串匹配条目，不需要 ID
- 当记忆库满时，用 batch 操作：先 remove/replace 旧条目释放空间，再 add 新条目
- 不要存储可轻松重新发现的信息、原始数据转储、临时 TODO`,
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
				"When making multiple changes, use batch (all-or-nothing against final char budget).",
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
						"description": "Entry content for 'add' or 'replace'. Required for add/replace.",
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
						"type":        "string",
						"description": "Memory scope. Options: channel (default), user, bot, global.",
						"default":     "channel",
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

				switch action {
				case "search":
					return handleSearch(ctx, repo, scope, m)

				case "add":
					result, err := handleAdd(ctx, repo, scope, m)
					if err == nil {
						config.markDirty()
					}
					return result, err

				case "replace":
					result, err := handleReplace(ctx, repo, scope, m)
					if err == nil {
						config.markDirty()
					}
					return result, err

				case "remove":
					result, err := handleRemove(ctx, repo, scope, m)
					if err == nil {
						config.markDirty()
					}
					return result, err

				case "recent":
					return handleRecent(ctx, repo, scope, m)

				case "count":
					return handleCount(ctx, repo, scope)

				case "batch":
					result, err := handleBatch(ctx, repo, scope, m)
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

	entries, err := repo.Retrieve(ctx, Query{
		Scopes:   []Scope{scope},
		Text:     queryText,
		Category: category,
		Limit:    limit,
	})
	if err != nil {
		return nil, errs.Wrap(err, "memory search failed")
	}

	return formatEntries(entries, "search"), nil
}

func handleAdd(ctx *llm.ToolExecContext, repo Repository, scope Scope, m map[string]any) (any, error) {
	content, _ := m["content"].(string)
	if content == "" {
		return nil, fmt.Errorf("content is required for 'add' action")
	}
	content = StripThinking(content)
	content = StripToolOutput(content)

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

	if err := repo.Append(ctx, entry); err != nil {
		return nil, errs.Wrap(err, "memory write failed")
	}

	return successResponse(scope, "Entry added.", true), nil
}

func handleReplace(ctx *llm.ToolExecContext, repo Repository, scope Scope, m map[string]any) (any, error) {
	oldText, _ := m["old_text"].(string)
	content, _ := m["content"].(string)

	if oldText == "" {
		return nil, fmt.Errorf("old_text is required for 'replace' action")
	}
	if content == "" {
		return nil, fmt.Errorf("content is required for 'replace' (use 'remove' to delete)")
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

	// 替换：先写入新条目，成功后再删除旧条目
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

	if err := repo.Append(ctx, newEntry); err != nil {
		return nil, errs.Wrap(err, "replace write failed")
	}
	// 删除旧条目（新条目有不同 ID，原 ID 由 Append 自动生成）
	if err := repo.Delete(ctx, scope, old.ID); err != nil {
		return nil, errs.Wrap(err, "cleanup of old entry failed")
	}

	return successResponse(scope, "Entry replaced.", true), nil
}

func handleRemove(ctx *llm.ToolExecContext, repo Repository, scope Scope, m map[string]any) (any, error) {
	// 支持 memory_id（向后兼容）和 old_text（子串匹配）两种方式
	memoryID, _ := m["memory_id"].(string)
	oldText, _ := m["old_text"].(string)

	if memoryID != "" {
		if err := repo.Delete(ctx, scope, memoryID); err != nil {
			return nil, errs.Wrap(err, "memory delete failed")
		}
		return map[string]any{
			"success":   true,
			"message":   "记忆已删除",
			"memory_id": memoryID,
			"scope":     scope.Key(),
		}, nil
	}

	if oldText == "" {
		return nil, fmt.Errorf("old_text or memory_id is required for 'remove' action")
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

	entries, err := repo.Recent(ctx, scope, limit)
	if err != nil {
		return nil, errs.Wrap(err, "memory recent failed")
	}

	return formatEntries(entries, "recent"), nil
}

func handleCount(ctx *llm.ToolExecContext, repo Repository, scope Scope) (any, error) {
	count, err := repo.Count(ctx, scope)
	if err != nil {
		return nil, errs.Wrap(err, "memory count failed")
	}

	return map[string]any{
		"scope": scope.Key(),
		"count": count,
	}, nil
}

func handleBatch(ctx *llm.ToolExecContext, repo Repository, scope Scope, m map[string]any) (any, error) {
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
		op        string // "add", "update", "delete"
		entry     *Entry // new entry for add/update
		target    string // existing entry ID for delete/update
		origEntry *Entry // original entry for update (preserves metadata)
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
			if opOldText == "" {
				return batchError(scope, pos+": old_text is required"), nil
			}
			matches := findSubstringMatches(working, opOldText)
			if len(matches) == 0 {
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
			if err := repo.Append(ctx, *ch.entry); err != nil {
				commitErrorCount++
			}
		case "update":
			// 使用保存的原始条目元数据，避免重复查询
			if ch.origEntry != nil {
				e := ch.origEntry
				updatedEntry := Entry{
					ID:             e.ID,
					Scope:          ch.entry.Scope,
					Content:        ch.entry.Content,
					Category:       ch.entry.Category,
					Source:         ch.entry.Source,
					Importance:     e.Importance,
					Metadata:       e.Metadata,
					CreatedAt:      e.CreatedAt,
					LastAccessedAt: e.LastAccessedAt,
				}
				// 先写入新条目，再删除旧条目（避免 Delete 后 Append 失败导致数据丢失）
				if err := repo.Append(ctx, updatedEntry); err != nil {
					commitErrorCount++
				} else {
					_ = repo.Delete(ctx, scope, ch.target)
				}
			}
		case "delete":
			if err := repo.Delete(ctx, scope, ch.target); err != nil {
				commitErrorCount++
			}
		}
	}

	msg := fmt.Sprintf("Applied %d operation(s).", appliedCount)
	if commitErrorCount > 0 {
		msg += fmt.Sprintf(" %d commit error(s) occurred.", commitErrorCount)
	}

	return successResponse(scope, msg, true), nil
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
			"message": "没有找到匹配的记忆",
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
