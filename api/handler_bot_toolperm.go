package api

import (
	"errors"
	"strings"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	agenttools "github.com/kasuganosora/thinkbot/agent/tools"
	"github.com/kasuganosora/thinkbot/toolperm"
	"github.com/kasuganosora/thinkbot/util/errs"
)

// ============================================================================
// Bot 工具权限管理 Handler
//
// 路由前缀：/api/bots/:id/tool-permissions
// 权限：requirePermission(auth.PermBotManage)
// ============================================================================

// handleListBotToolPerms 列出某 bot 的全部工具权限规则。
// GET /api/bots/:id/tool-permissions
func (s *Server) handleListBotToolPerms(c *gin.Context) {
	botID := c.Param("id")
	rules, err := s.permSvc.ListRules(botID)
	if err != nil {
		Fail(c, err)
		return
	}
	OK(c, rules)
}

// handleCreateBotToolPerm 创建一条工具权限规则。
// POST /api/bots/:id/tool-permissions
func (s *Server) handleCreateBotToolPerm(c *gin.Context) {
	botID := c.Param("id")
	var req toolperm.RuleReq
	if err := c.ShouldBindJSON(&req); err != nil {
		Fail(c, errs.BadRequest("invalid request body: "+err.Error()))
		return
	}
	rule, err := s.permSvc.CreateRule(botID, req)
	if err != nil {
		Fail(c, err)
		return
	}
	OK(c, rule)
}

// handleUpdateBotToolPerm 更新一条工具权限规则。
// PUT /api/bots/:id/tool-permissions/:rid
func (s *Server) handleUpdateBotToolPerm(c *gin.Context) {
	botID := c.Param("id")
	rid := c.Param("rid")
	var req toolperm.RuleReq
	if err := c.ShouldBindJSON(&req); err != nil {
		Fail(c, errs.BadRequest("invalid request body: "+err.Error()))
		return
	}
	rule, err := s.permSvc.UpdateRule(botID, rid, req)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			Fail(c, errs.NotFound("工具权限规则不存在"))
			return
		}
		Fail(c, err)
		return
	}
	OK(c, rule)
}

// handleDeleteBotToolPerm 删除一条工具权限规则。
// DELETE /api/bots/:id/tool-permissions/:rid
func (s *Server) handleDeleteBotToolPerm(c *gin.Context) {
	botID := c.Param("id")
	rid := c.Param("rid")
	if err := s.permSvc.DeleteRule(botID, rid); err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			Fail(c, errs.NotFound("工具权限规则不存在"))
			return
		}
		Fail(c, err)
		return
	}
	OK(c, nil)
}

// handleResetBotToolPermDefaults 清空并恢复 web 默认全开规则。
// POST /api/bots/:id/tool-permissions/reset-defaults
func (s *Server) handleResetBotToolPermDefaults(c *gin.Context) {
	botID := c.Param("id")
	if err := s.permSvc.ResetDefaults(botID); err != nil {
		Fail(c, err)
		return
	}
	OK(c, nil)
}

// handleGetBotOutbound 查询某bot 各渠道的「只读（潜水）」状态。
// GET /api/bots/:id/outbound
func (s *Server) handleGetBotOutbound(c *gin.Context) {
	botID := c.Param("id")
	out := make([]outboundState, 0, len(outboundPlatforms))
	for _, p := range outboundPlatforms {
		out = append(out, outboundState{Platform: p, ReadOnly: s.permSvc.IsReadOnly(botID, p)})
	}
	OK(c, out)
}

// handleSetBotOutbound 设置某渠道是否只读。
// PUT /api/bots/:id/outbound
func (s *Server) handleSetBotOutbound(c *gin.Context) {
	botID := c.Param("id")
	var req struct {
		Platform string `json:"platform"`
		ReadOnly *bool  `json:"readOnly"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		Fail(c, errs.BadRequest("invalid request body: "+err.Error()))
		return
	}
	if req.ReadOnly == nil {
		Fail(c, errs.BadRequest("readOnly is required"))
		return
	}
	if err := s.permSvc.SetReadOnly(botID, req.Platform, *req.ReadOnly); err != nil {
		Fail(c, err)
		return
	}
	OK(c, outboundState{Platform: req.Platform, ReadOnly: *req.ReadOnly})
}

// outboundState 是单个渠道的出站状态视图。
type outboundState struct {
	Platform string `json:"platform"`
	ReadOnly bool   `json:"readOnly"`
}

// outboundPlatforms 是出站开关覆盖的渠道类型。
// 与前端平台下拉保持一致；"*" 表示一次性覆盖全部渠道。
var outboundPlatforms = []string{"*", "web", "telegram", "misskey"}

// handleListBotTools 列出某Bot 已注册的全部工具（名称+描述+分类），
// 用于工具权限页面的工具选择器。
// 返回结果已过滤内部工具，且description/category 均已本地化为中文短文案，
// 不会暴露 LLM 内部提示词。
// GET /api/bots/:id/tools
func (s *Server) handleListBotTools(c *gin.Context) {
	botID := c.Param("id")
	tools := s.botSvc.ListBotTools(botID)
	if len(tools) == 0 {
		tools = fallbackToolList()
	}
	OK(c, presentToolList(tools))
}

// userToolDesc 把工具的 LLM 原始描述替换为用户友好的中文短描述。
// 键为 tool name，值为面向 UI 的简短说明（尽量 ≤20 字）。
// LLM 原始 description 是给模型看的详细指令（含"you MUST"、"BLOCKING" 等），
// 直接展示给管理员既冗长又泄露实现细节，故在此统一改写。
var userToolDesc = map[string]string{
	// 通用
	"calculate":     "计算数学表达式",
	"datetime_calc": "日期时间计算与格式转换",
	"now":           "获取当前日期时间",
	"random":        "生成随机数或随机选择",
	"uuid":          "生成随机 UUID 标识符",
	// 文本处理
	"text_diff":   "比较两段文本的行级差异",
	"text_encode": "Base64 编码 / 解码",
	"text_hash":   "计算文本哈希摘要",
	"text_stats":  "统计字数、行数与阅读时长",
	// 网络
	"web_search": "搜索互联网获取实时信息",
	"web_fetch":  "抓取指定网址的页面内容",
	// 记忆
	"memory":          "管理跨会话的持久化记忆",
	"memory_snapshot": "保存当前对话记忆快照",
	"memory_tools":    "管理持久化记忆条目",
	// 子智能体/ 工作流
	"spawn":        "创建子智能体并行执行任务",
	"task":         "提交复杂多步任务（含审查）",
	"task_control": "控制任务运行（暂停/取消等）",
	"task_detail":  "查询任务各子步骤的详细状态",
	// 沙箱
	"sandbox_exec":            "在沙箱容器中执行命令",
	"sandbox_read_file":       "读取工作空间文件内容",
	"sandbox_write_file":      "写入或创建文件",
	"sandbox_replace_in_file": "在文件中替换文本",
	"sandbox_delete_file":     "删除文件",
	"sandbox_move_file":       "移动或重命名文件",
	"sandbox_list_dir":        "列出目录内容",
	"sandbox_search_content":  "在文件中搜索文本",
	"sandbox_health":          "检查沙箱健康状态",
}

// toolCategoryLabel 把工具注册时使用的英文 category 映射为中文分组名，
// 供前端按分组展示工具选择器。未收录的分类原样返回。
var toolCategoryLabel = map[string]string{
	"utility":  "通用工具",
	"search":   "信息检索",
	"memory":   "记忆",
	"sandbox":  "沙箱与文件",
	"subagent": "子智能体",
	"workflow": "任务与工作流",
}

// presentToolList 把内部工具列表整理成适合前端展示的形态：
//  1. 过滤 "__" 前缀的内部工具（提示词段落载体，不是可授权的真实工具）；
//  2. description 替换为中文短描述，未收录的截断至 60 字符；
//  3. category 映射为中文分组名；
//  4. 填充 Risk 风险级别，供前端标记「基础工具默认开放/ 敏感工具需管控」。
func presentToolList(tools []agenttools.ToolInfo) []agenttools.ToolInfo {
	out := make([]agenttools.ToolInfo, 0, len(tools))
	for _, t := range tools {
		if strings.HasPrefix(t.Name, "__") {
			continue // 内部提示词载体，不可授权
		}
		if desc, ok := userToolDesc[t.Name]; ok {
			t.Description = desc
		} else if len(t.Description) > 60 {
			t.Description = t.Description[:57] + "…"
		}
		if label, ok := toolCategoryLabel[t.Category]; ok {
			t.Category = label
		} else if t.Category == "" {
			t.Category = "其他"
		}
		t.Risk = toolperm.ToolRisk(t.Name)
		// Parameters 是给LLM 的 JSON Schema，选择器不需要，去掉以减小响应体
		t.Parameters = nil
		out = append(out, t)
	}
	return out
}

// fallbackToolList 在 Bot 未运行时返回系统已知工具的静态清单。
// 这些是所有 Bot 共享的静态注册工具（不含按 bot 动态注册的工作空间工具），
// 足以支撑权限规则的工具选择。
// category 使用与运行时一致的英文键，由 presentToolList 统一映射为中文分组。
func fallbackToolList() []agenttools.ToolInfo {
	return []agenttools.ToolInfo{
		{Name: "web_search", Category: "search"},
		{Name: "web_fetch", Category: "utility"},
		{Name: "calculate", Category: "utility"},
		{Name: "datetime_calc", Category: "utility"},
		{Name: "now", Category: "utility"},
		{Name: "random", Category: "utility"},
		{Name: "uuid", Category: "utility"},
		{Name: "text_diff", Category: "utility"},
		{Name: "text_encode", Category: "utility"},
		{Name: "text_hash", Category: "utility"},
		{Name: "text_stats", Category: "utility"},
		{Name: "memory", Category: "memory"},
		{Name: "spawn", Category: "subagent"},
		{Name: "task", Category: "workflow"},
		{Name: "task_control", Category: "workflow"},
		{Name: "task_detail", Category: "workflow"},
		{Name: "sandbox_exec", Category: "sandbox"},
		{Name: "sandbox_read_file", Category: "sandbox"},
		{Name: "sandbox_write_file", Category: "sandbox"},
		{Name: "sandbox_replace_in_file", Category: "sandbox"},
		{Name: "sandbox_delete_file", Category: "sandbox"},
		{Name: "sandbox_move_file", Category: "sandbox"},
		{Name: "sandbox_list_dir", Category: "sandbox"},
		{Name: "sandbox_search_content", Category: "sandbox"},
		{Name: "sandbox_health", Category: "sandbox"},
	}
}
