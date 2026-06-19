package memory

import (
	"fmt"
	"time"

	"github.com/kasuganosora/thinkbot/agent/tools"
	"github.com/kasuganosora/thinkbot/llm"
	"github.com/kasuganosora/thinkbot/util/errs"
)

// ============================================================================
// 记忆管理工具 — 让 AI 自主搜索、写入、删除记忆
//
// 提供以下工具：
//   - memory_search:  按关键词/分类搜索记忆
//   - memory_write:   写入一条新记忆
//   - memory_delete:  按 ID 删除单条记忆
//   - memory_recent:  获取当前 scope 最近 N 条记忆
//   - memory_count:   查询记忆数量
//
// 所有工具通过闭包持有 Repository，scope 由 LLM 在参数中指定。
// ============================================================================

// memoryToolsPromptSection 是记忆工具的统一提示词段落。
var memoryToolsPromptSection = &tools.ToolPromptSection{
	Name:  "memory_tools",
	Order: 310,
	Content: `# 记忆管理

你拥有自主管理长期记忆的能力。可以使用以下工具搜索、写入、删除记忆。

## 何时使用

- **搜索记忆**：当你需要回忆之前对话中的信息（用户偏好、已知事实、历史决策等）时使用 ` + "`memory_search`" + `
- **写入记忆**：当你判断某条信息有长期价值（用户个人偏好、重要事实、约定事项等）时使用 ` + "`memory_write`" + `
- **删除记忆**：当发现记忆条目过时、不准确或用户要求遗忘时使用 ` + "`memory_delete`" + `
- **查看最近**：快速回顾最近的记忆时使用 ` + "`memory_recent`" + `

## Scope 说明

记忆按 scope 分桶存储：
- ` + "`channel`" + `：会话级（同一频道/群聊内可见），最常用
- ` + "`user`" + `：用户级（跨频道，同一用户的所有对话共享）
- ` + "`bot`" + `：Bot 级（Bot 自身的知识库）
- ` + "`global`" + `：全局（所有会话共享）

不指定 scope 时默认使用 channel scope。

## 使用原则

- 只存储有长期价值的信息，不要存储闲聊内容
- 写入时选择正确的 scope：个人偏好用 user，会话相关用 channel
- 搜索时优先搜索当前 channel scope，然后是 user scope
- 记忆是辅助性的，不要过度依赖记忆工具，能直接从对话上下文回答的就不用搜索
- content 字段应简洁、信息密集，避免冗长描述`,
	Enabled: true,
}

// ToolConfig 配置记忆工具。
type ToolConfig struct {
	// Repo 记忆仓储（必须提供）。
	Repo Repository
	// DefaultScopeKind 默认 scope 类型（默认 "channel"）。
	DefaultScopeKind ScopeKind
	// DefaultScopeID 默认 scope ID（默认空，使用会话的 channel/user ID）。
	// 通常留空，让 LLM 在参数中提供。
	DefaultScopeID string
}

// Tools 返回记忆管理工具定义列表。
// 包含 memory_search、memory_write、memory_delete、memory_recent、memory_count 五个工具。
func Tools(config ToolConfig) []tools.ToolDef {
	repo := config.Repo
	if repo == nil {
		return nil
	}

	defaultKind := config.DefaultScopeKind
	if defaultKind == "" {
		defaultKind = ScopeChannel
	}
	defaultID := config.DefaultScopeID

	// parseScope 从 LLM 输入中解析 scope 参数。
	parseScope := func(m map[string]any) Scope {
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

	// --- memory_search ---
	searchTool := tools.ToolDef{
		Category: "memory",
		Scopes:   []string{"private", "group"},
		Tool: tools.BuildTool(
			"memory_search",
			"搜索记忆库中的条目。支持按关键词模糊搜索，可选按分类过滤。返回匹配的记忆条目列表。",
			map[string]any{
				"type": "object",
				"properties": map[string]any{
					"query": map[string]any{
						"type":        "string",
						"description": "搜索关键词。匹配记忆内容的子串（不区分大小写）。留空时返回该 scope 下所有记忆。",
					},
					"scope_kind": map[string]any{
						"type":        "string",
						"description": "搜索的作用域类型。可选值：channel（会话级，默认）、user（用户级）、bot（Bot级）、global（全局）。",
						"default":     "channel",
					},
					"scope_id": map[string]any{
						"type":        "string",
						"description": "作用域标识（如频道 ID、用户 ID）。global scope 时留空。",
					},
					"category": map[string]any{
						"type":        "string",
						"description": "按分类过滤。常见值：fact、preference、event、observation、summary。留空时不按分类过滤。",
					},
					"limit": map[string]any{
						"type":        "integer",
						"description": "最多返回条目数（默认 10，最大 50）。",
						"default":     10,
					},
				},
			},
			func(ctx *llm.ToolExecContext, input any) (any, error) {
				m, ok := input.(map[string]any)
				if !ok {
					return nil, fmt.Errorf("invalid input: expected object")
				}
				queryText, _ := m["query"].(string)
				category, _ := m["category"].(string)
				limit := 10
				if l := toInt(m["limit"]); l > 0 {
					limit = l
				}
				if limit > 50 {
					limit = 50
				}

				scope := parseScope(m)

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
			},
		),
		PromptSection: memoryToolsPromptSection,
	}

	// --- memory_write ---
	writeTool := tools.ToolDef{
		Category: "memory",
		Scopes:   []string{"private", "group"},
		Tool: tools.BuildTool(
			"memory_write",
			"写入一条新记忆到记忆库。用于存储有长期价值的信息（用户偏好、重要事实、约定事项等）。",
			map[string]any{
				"type": "object",
				"properties": map[string]any{
					"content": map[string]any{
						"type":        "string",
						"description": "记忆内容。应简洁、信息密集，包含足够的上下文使其在未来可被检索和理解。",
					},
					"category": map[string]any{
						"type":        "string",
						"description": "记忆分类。推荐值：fact（事实）、preference（偏好）、event（事件）、observation（观察）、summary（摘要）。默认 observation。",
						"default":     "observation",
					},
					"importance": map[string]any{
						"type":        "number",
						"description": "重要程度（0.0~1.0）。0.5 为中等，1.0 为极高。用于排序和淘汰策略。默认 0.5。",
						"default":     0.5,
					},
					"scope_kind": map[string]any{
						"type":        "string",
						"description": "存储的作用域类型。可选值：channel（默认）、user、bot、global。个人偏好建议用 user，会话相关用 channel。",
						"default":     "channel",
					},
					"scope_id": map[string]any{
						"type":        "string",
						"description": "作用域标识（如频道 ID、用户 ID）。global scope 时留空。",
					},
				},
				"required": []string{"content"},
			},
			func(ctx *llm.ToolExecContext, input any) (any, error) {
				m, ok := input.(map[string]any)
				if !ok {
					return nil, fmt.Errorf("invalid input: expected object")
				}

				content, _ := m["content"].(string)
				if content == "" {
					return nil, fmt.Errorf("content is required")
				}
				content = StripThinking(content)
				content = StripToolOutput(content)

				category, _ := m["category"].(string)
				if category == "" {
					category = "observation"
				}

				importance := 0.5
				if imp, ok := m["importance"].(float64); ok {
					importance = imp
				}

				scope := parseScope(m)

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

				return map[string]any{
					"success":  true,
					"message":  "记忆已写入",
					"scope":    scope.Key(),
					"content":  content,
					"category": category,
				}, nil
			},
		),
	}

	// --- memory_delete ---
	deleteTool := tools.ToolDef{
		Category: "memory",
		Scopes:   []string{"private", "group"},
		Tool: tools.BuildTool(
			"memory_delete",
			"按 ID 删除单条记忆。需要先用 memory_search 找到要删除的记忆的 ID。",
			map[string]any{
				"type": "object",
				"properties": map[string]any{
					"memory_id": map[string]any{
						"type":        "string",
						"description": "要删除的记忆条目 ID（从 memory_search 或 memory_recent 的结果中获取）。",
					},
					"scope_kind": map[string]any{
						"type":        "string",
						"description": "记忆所在的作用域类型。可选值：channel（默认）、user、bot、global。",
						"default":     "channel",
					},
					"scope_id": map[string]any{
						"type":        "string",
						"description": "作用域标识（如频道 ID、用户 ID）。",
					},
				},
				"required": []string{"memory_id"},
			},
			func(ctx *llm.ToolExecContext, input any) (any, error) {
				m, ok := input.(map[string]any)
				if !ok {
					return nil, fmt.Errorf("invalid input: expected object")
				}

				memoryID, _ := m["memory_id"].(string)
				if memoryID == "" {
					return nil, fmt.Errorf("memory_id is required")
				}

				scope := parseScope(m)

				if err := repo.Delete(ctx, scope, memoryID); err != nil {
					return nil, errs.Wrap(err, "memory delete failed")
				}

				return map[string]any{
					"success":   true,
					"message":   "记忆已删除",
					"memory_id": memoryID,
					"scope":     scope.Key(),
				}, nil
			},
		),
	}

	// --- memory_recent ---
	recentTool := tools.ToolDef{
		Category: "memory",
		Scopes:   []string{"private", "group"},
		Tool: tools.BuildTool(
			"memory_recent",
			"获取指定作用域下最近的记忆条目。按时间倒序返回，适合快速回顾最近的记忆。",
			map[string]any{
				"type": "object",
				"properties": map[string]any{
					"scope_kind": map[string]any{
						"type":        "string",
						"description": "作用域类型。可选值：channel（默认）、user、bot、global。",
						"default":     "channel",
					},
					"scope_id": map[string]any{
						"type":        "string",
						"description": "作用域标识（如频道 ID、用户 ID）。global scope 时留空。",
					},
					"limit": map[string]any{
						"type":        "integer",
						"description": "返回条目数（默认 5，最大 20）。",
						"default":     5,
					},
				},
			},
			func(ctx *llm.ToolExecContext, input any) (any, error) {
				m, ok := input.(map[string]any)
				if !ok {
					return nil, fmt.Errorf("invalid input: expected object")
				}

				limit := 5
				if l := toInt(m["limit"]); l > 0 {
					limit = l
				}
				if limit > 20 {
					limit = 20
				}

				scope := parseScope(m)

				entries, err := repo.Recent(ctx, scope, limit)
				if err != nil {
					return nil, errs.Wrap(err, "memory recent failed")
				}

				return formatEntries(entries, "recent"), nil
			},
		),
	}

	// --- memory_count ---
	countTool := tools.ToolDef{
		Category: "memory",
		Scopes:   []string{"private", "group"},
		Tool: tools.BuildTool(
			"memory_count",
			"查询指定作用域下的记忆条目总数。",
			map[string]any{
				"type": "object",
				"properties": map[string]any{
					"scope_kind": map[string]any{
						"type":        "string",
						"description": "作用域类型。可选值：channel（默认）、user、bot、global。",
						"default":     "channel",
					},
					"scope_id": map[string]any{
						"type":        "string",
						"description": "作用域标识（如频道 ID、用户 ID）。global scope 时留空。",
					},
				},
			},
			func(ctx *llm.ToolExecContext, input any) (any, error) {
				m, ok := input.(map[string]any)
				if !ok {
					return nil, fmt.Errorf("invalid input: expected object")
				}

				scope := parseScope(m)

				count, err := repo.Count(ctx, scope)
				if err != nil {
					return nil, errs.Wrap(err, "memory count failed")
				}

				return map[string]any{
					"scope": scope.Key(),
					"count": count,
				}, nil
			},
		),
	}

	return []tools.ToolDef{searchTool, writeTool, deleteTool, recentTool, countTool}
}

// RegisterTools 将记忆工具注册到 ToolManager。
//
// 使用示例：
//
//	repo := memory.NewMemoryRepository()
//	memory.RegisterTools(toolMgr, memory.ToolConfig{Repo: repo})
func RegisterTools(mgr *tools.ToolManager, config ToolConfig) error {
	defs := Tools(config)
	if len(defs) == 0 {
		return fmt.Errorf("memory tools require a non-nil repository")
	}
	return mgr.RegisterMany(defs...)
}

// ============================================================================
// Helpers
// ============================================================================

// EntryResult 是单条记忆的序列化结构。
type EntryResult struct {
	ID         string  `json:"id"`
	Content    string  `json:"content"`
	Category   string  `json:"category,omitempty"`
	Importance float64 `json:"importance,omitempty"`
	CreatedAt  string  `json:"created_at,omitempty"`
}

// formatEntries 将记忆条目列表格式化为工具返回值。
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

// formatEntryTime 格式化记忆创建时间。
func formatEntryTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format("2006-01-02 15:04")
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
