package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/kasuganosora/thinkbot/agent/core"
	"github.com/kasuganosora/thinkbot/config"
	"github.com/kasuganosora/thinkbot/dao"
	"github.com/kasuganosora/thinkbot/sandbox"
	"github.com/kasuganosora/thinkbot/util/errs"
)

// ============================================================================
// Bot 详情面板 Handler — Platform / Memory / Access / Files / Rhythm / Container / Compaction
//
// 前端契约（均按 botId 归属）：
//   botPlatformApi  — /api/bots/:id/platforms[/:pid], /api/bots/platforms/tool-catalog
//   botMemoryApi    — /api/bots/:id/memory[/:mid]
//   botAccessApi    — /api/bots/:id/access
//   botFileApi      — /api/bots/:id/files[/mkdir|upload]
//   botRhythmApi    — /api/bots/:id/chat-rhythm
//   botContainerApi — /api/bots/:id/container[/...]
//   botCompactionApi— /api/bots/:id/compaction[/history]
// ============================================================================

// --- 平台管理 (Platform) ---

// BotPlatform 平台绑定定义（存储在 config store）。
type BotPlatform struct {
	ID         string           `json:"id"`
	Type       string           `json:"type"`
	Name       string           `json:"name"`
	Enabled    bool             `json:"enabled"`
	Configured bool             `json:"configured"`
	Config     map[string]any   `json:"config"`
	Tools      []string         `json:"tools"`
	Rhythm     *BotRhythmConfig `json:"rhythm,omitempty"`
}

// ToolCatalogGroup 工具分组目录。
type ToolCatalogGroup struct {
	Group string   `json:"group"`
	Tools []string `json:"tools"`
}

// PlatformType 平台类型定义。
type PlatformType struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Icon        string          `json:"icon"`
	Color       string          `json:"color"`
	Fields      []PlatformField `json:"fields"`
}

// PlatformField 平台配置字段定义。
type PlatformField struct {
	Key         string `json:"key"`
	Label       string `json:"label"`
	Type        string `json:"type"`
	Placeholder string `json:"placeholder,omitempty"`
	Help        string `json:"help,omitempty"`
	Optional    bool   `json:"optional,omitempty"`
	// Options 兼容两种形态：字符串数组（select）或 {label,value} 对象数组（multiselect）。
	Options []any `json:"options,omitempty"`
}

// 内置工具目录
var botToolCatalog = []ToolCatalogGroup{
	{Group: "Messaging", Tools: []string{"send", "reply", "react", "get_contacts", "speak"}},
	{Group: "Memory", Tools: []string{"search_memory"}},
	{Group: "Web", Tools: []string{"web_search", "web_fetch"}},
	{Group: "Schedule", Tools: []string{"list_schedule", "get_schedule", "create_schedule", "update_schedule", "delete_schedule"}},
	{Group: "Container", Tools: []string{"read", "write", "list", "edit", "exec", "bg_status"}},
	{Group: "Email", Tools: []string{"list_mail", "read_mail", "send_mail"}},
}

// handleBotToolCatalog 返回工具目录和平台类型（统一来自 channel_registry）。
// GET /api/bots/platforms/tool-catalog
func (s *Server) handleBotToolCatalog(c *gin.Context) {
	OK(c, gin.H{"catalog": botToolCatalog, "types": SupportedPlatformTypes()})
}

// handleListBotPlatforms 列出 Bot 绑定的平台。
// GET /api/bots/:id/platforms
func (s *Server) handleListBotPlatforms(c *gin.Context) {
	botID := c.Param("id")
	platforms := s.getBotPlatforms(botID)
	OK(c, platforms)
}

// handleCreateBotPlatform 为 Bot 创建平台绑定。
// POST /api/bots/:id/platforms
func (s *Server) handleCreateBotPlatform(c *gin.Context) {
	botID := c.Param("id")

	var req struct {
		Type   string         `json:"type" binding:"required"`
		Name   string         `json:"name"`
		Config map[string]any `json:"config"`
		Tools  []string       `json:"tools"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		Fail(c, errs.BadRequest("invalid request body: "+err.Error()))
		return
	}

	// 只允许后端注册表中真实支持的平台类型，避免创建出无 channel 实现的平台。
	if !IsValidChannelType(req.Type) {
		Fail(c, errs.BadRequest("unsupported platform type: "+req.Type))
		return
	}

	if req.Name == "" {
		req.Name = req.Type
	}
	if req.Tools == nil {
		req.Tools = []string{}
	}
	if req.Config == nil {
		req.Config = map[string]any{}
	}

	platform := BotPlatform{
		ID:         generateProviderID(req.Name + "-" + req.Type),
		Type:       req.Type,
		Name:       req.Name,
		Enabled:    false,
		Configured: false,
		Config:     req.Config,
		Tools:      req.Tools,
	}

	platforms := s.getBotPlatforms(botID)
	platforms = append(platforms, platform)
	if err := s.saveBotPlatforms(c, botID, platforms); err != nil {
		Fail(c, err)
		return
	}

	OK(c, platform)

	// 新平台默认 disabled，不会构建 channel；仅当用户以 enabled 创建时才需热重载。
	if platform.Enabled {
		s.reloadBotIfRunning(botID)
	}
}

// handleUpdateBotPlatform 更新 Bot 的平台绑定。
// PUT /api/bots/:id/platforms/:pid
func (s *Server) handleUpdateBotPlatform(c *gin.Context) {
	botID := c.Param("id")
	pid := c.Param("pid")

	platforms := s.getBotPlatforms(botID)
	idx := -1
	for i, p := range platforms {
		if p.ID == pid {
			idx = i
			break
		}
	}
	if idx < 0 {
		Fail(c, errs.NotFound("platform not found"))
		return
	}

	var req map[string]any
	if err := c.ShouldBindJSON(&req); err != nil {
		Fail(c, errs.BadRequest("invalid request body: "+err.Error()))
		return
	}

	// 部分更新
	p := &platforms[idx]
	if v, ok := req["name"]; ok {
		if s, ok := v.(string); ok {
			p.Name = s
		}
	}
	if v, ok := req["enabled"]; ok {
		if b, ok := v.(bool); ok {
			p.Enabled = b
		}
	}
	if v, ok := req["config"]; ok {
		if m, ok := v.(map[string]any); ok {
			p.Config = m
		}
	}
	if v, ok := req["tools"]; ok {
		if arr, ok := v.([]any); ok {
			tools := make([]string, 0, len(arr))
			for _, item := range arr {
				if s, ok := item.(string); ok {
					tools = append(tools, s)
				}
			}
			p.Tools = tools
		}
	}
	// 聊天节奏已合并进平台对象：随平台设置一起读写，无需独立入口。
	if v, ok := req["rhythm"]; ok && v != nil {
		if m, ok := v.(map[string]any); ok {
			if data, err := json.Marshal(m); err == nil {
				var rc BotRhythmConfig
				if json.Unmarshal(data, &rc) == nil {
					// 保存侧即补齐缺失子配置：BotRhythmParams 是值类型，
					// 「用户显式关闭(enabled=false 其余零值)」与「前端未提交该子项」
					// 在零值上无法区分。若只在读取侧补默认，用户的关闭会被
					// 默认值(group.Enabled=true)静默撤销。这里落库前就补全，
					// 使存储始终是完整结构，读取侧不再需要猜测意图。
					fillRhythmDefaults(&rc, p.Type)
					p.Rhythm = &rc
				}
			}
		}
	}
	p.Configured = true

	if err := s.saveBotPlatforms(c, botID, platforms); err != nil {
		Fail(c, err)
		return
	}
	OK(c, *p)

	// 平台配置/启停变更会改变运行中的 channel，热重载使其立即生效（无需重启服务）。
	s.reloadBotIfRunning(botID)
}

// handleDeleteBotPlatform 删除 Bot 的平台绑定。
// DELETE /api/bots/:id/platforms/:pid
func (s *Server) handleDeleteBotPlatform(c *gin.Context) {
	botID := c.Param("id")
	pid := c.Param("pid")

	platforms := s.getBotPlatforms(botID)
	found := false
	for i, p := range platforms {
		if p.ID == pid {
			platforms = append(platforms[:i], platforms[i+1:]...)
			found = true
			break
		}
	}
	if !found {
		Fail(c, errs.NotFound("platform not found"))
		return
	}

	if err := s.saveBotPlatforms(c, botID, platforms); err != nil {
		Fail(c, err)
		return
	}
	OK(c, nil)

	// 平台被删除会改变运行中的 channel 集合，热重载使其立即生效（无需重启服务）。
	s.reloadBotIfRunning(botID)
}

// reloadBotIfRunning 在平台配置变更后，若 Bot 正在运行则后台热重载（停→启），
// 让 channel 增删/配置修改/启停立即生效，无需重启整个 thinkbot 服务。
// 后台异步执行，不阻塞 API 响应；若 Bot 未运行则无需操作（下次启动自然生效）。
func (s *Server) reloadBotIfRunning(botID string) {
	if s.botSvc == nil || !s.botSvc.IsRunning(botID) {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		if err := s.botSvc.StartBot(ctx, botID); err != nil {
			s.logger.Errorw("platform change hot-reload failed", "bot_id", botID, "err", err)
			return
		}
		s.logger.Infow("platform change hot-reloaded bot", "bot_id", botID)
	}()
}

// --- 记忆管理 (Memory CRUD) ---

// BotMemoryEntry 记忆条目（用于 Bot 详情面板的 CRUD）。
type BotMemoryEntry struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	Content   string `json:"content"`
	UpdatedAt string `json:"updatedAt"`
}

// handleListBotMemoryEntries 列出 Bot 的记忆条目。
// GET /api/bots/:id/memory（注意：此接口与现有 handleQueryMemory 重叠，但结构不同）
// 此 handler 返回前端 botMemoryApi.list 需要的格式
//
//nolint:unused // 预留接口，计划后续注册到路由
func (s *Server) handleListBotMemoryEntries(c *gin.Context) {
	botID := c.Param("id")
	entries := s.getBotMemoryEntries(botID)
	OK(c, entries)
}

// handleCreateBotMemoryEntry 创建记忆条目。
// POST /api/bots/:id/memory
func (s *Server) handleCreateBotMemoryEntry(c *gin.Context) {
	botID := c.Param("id")

	var req struct {
		Title   string `json:"title"`
		Content string `json:"content"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		Fail(c, errs.BadRequest("invalid request body: "+err.Error()))
		return
	}

	entries := s.getBotMemoryEntries(botID)
	entry := BotMemoryEntry{
		ID:        fmt.Sprintf("mem_%d", len(entries)+1),
		Title:     req.Title,
		Content:   req.Content,
		UpdatedAt: nowRFC3339(),
	}
	entries = append([]BotMemoryEntry{entry}, entries...)

	if err := s.saveBotMemoryEntries(c, botID, entries); err != nil {
		Fail(c, err)
		return
	}
	OK(c, entry)
}

// handleUpdateBotMemoryEntry 更新记忆条目。
// PUT /api/bots/:id/memory/:mid
func (s *Server) handleUpdateBotMemoryEntry(c *gin.Context) {
	botID := c.Param("id")
	mid := c.Param("mid")

	entries := s.getBotMemoryEntries(botID)
	idx := -1
	for i, e := range entries {
		if e.ID == mid {
			idx = i
			break
		}
	}
	if idx < 0 {
		Fail(c, errs.NotFound("memory entry not found"))
		return
	}

	var req struct {
		Title   *string `json:"title"`
		Content *string `json:"content"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		Fail(c, errs.BadRequest("invalid request body: "+err.Error()))
		return
	}

	if req.Title != nil {
		entries[idx].Title = *req.Title
	}
	if req.Content != nil {
		entries[idx].Content = *req.Content
	}
	entries[idx].UpdatedAt = nowRFC3339()

	if err := s.saveBotMemoryEntries(c, botID, entries); err != nil {
		Fail(c, err)
		return
	}
	OK(c, entries[idx])
}

// handleDeleteBotMemoryEntry 删除记忆条目。
// DELETE /api/bots/:id/memory/:mid
func (s *Server) handleDeleteBotMemoryEntry(c *gin.Context) {
	botID := c.Param("id")
	mid := c.Param("mid")

	entries := s.getBotMemoryEntries(botID)
	found := false
	for i, e := range entries {
		if e.ID == mid {
			entries = append(entries[:i], entries[i+1:]...)
			found = true
			break
		}
	}
	if !found {
		Fail(c, errs.NotFound("memory entry not found"))
		return
	}

	if err := s.saveBotMemoryEntries(c, botID, entries); err != nil {
		Fail(c, err)
		return
	}
	OK(c, nil)
}

// --- 访问控制 (Access) ---

// BotAccessConfig 访问控制配置。
type BotAccessConfig struct {
	Default string          `json:"default"` // "allow" or "deny"
	Rules   []BotAccessRule `json:"rules"`
}

// BotAccessRule 访问控制规则。
type BotAccessRule struct {
	ID     string `json:"id,omitempty"`
	Field  string `json:"field"`  // 匹配字段：platform | userId | keyword
	Value  string `json:"value"`  // 匹配值
	Action string `json:"action"` // "allow" | "deny"
}

// --- 文件管理 (Files) ---

// BotFileEntry 文件/目录条目。
type BotFileEntry struct {
	Name  string `json:"name"`
	Type  string `json:"type"` // "dir" | "file"
	Size  int64  `json:"size"`
	Mtime string `json:"mtime"`
}

// handleListBotFiles 列出 Bot 指定路径的文件。
// GET /api/bots/:id/files?path=/data
func (s *Server) handleListBotFiles(c *gin.Context) {
	botID := c.Param("id")
	path := c.DefaultQuery("path", "/")

	entries := s.getBotFileEntries(botID, path)
	OK(c, entries)
}

// handleBotFileMkdir 在 Bot 文件系统中创建目录。
// POST /api/bots/:id/files/mkdir
func (s *Server) handleBotFileMkdir(c *gin.Context) {
	botID := c.Param("id")

	var req struct {
		Path string `json:"path" binding:"required"`
		Name string `json:"name" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		Fail(c, errs.BadRequest("invalid request body: "+err.Error()))
		return
	}

	if err := s.botFileMkdir(c, botID, req.Path, req.Name); err != nil {
		Fail(c, err)
		return
	}
	OK(c, gin.H{"ok": true})
}

// handleBotFileUpload 向 Bot 文件系统上传文件。
// POST /api/bots/:id/files/upload
// 接收 multipart/form-data：字段 file（文件）、path（目标路径）。
func (s *Server) handleBotFileUpload(c *gin.Context) {
	botID := c.Param("id")

	fileHeader, err := c.FormFile("file")
	if err != nil {
		Fail(c, errs.BadRequest("file is required: "+err.Error()))
		return
	}
	path := c.PostForm("path")
	if path == "" {
		path = "/"
	}

	f, err := fileHeader.Open()
	if err != nil {
		Fail(c, errs.Internal("failed to open uploaded file: "+err.Error()))
		return
	}
	defer func() { _ = f.Close() }()

	if err := s.botFileUpload(c, botID, path, fileHeader.Filename, f); err != nil {
		Fail(c, err)
		return
	}
	OK(c, gin.H{"ok": true})
}

// handleBotFileDownload 下载 Bot 文件系统中的单个文件。
// GET /api/bots/:id/files/download?path=/xxx.txt
func (s *Server) handleBotFileDownload(c *gin.Context) {
	botID := c.Param("id")
	s.serveBotFileDownload(c, botID, c.Query("path"))
}

// serveBotFileDownload 校验路径并以附件形式返回文件内容（供 bot / session 复用）。
func (s *Server) serveBotFileDownload(c *gin.Context, botID, path string) {
	if path == "" || path == "/" {
		Fail(c, errs.BadRequest("path is required"))
		return
	}

	// docker 隔离模式：从容器内读取文件流返回。
	if ws, ok := s.botFileWorkspace(botID); ok {
		data, err := ws.ReadFile(c.Request.Context(), path)
		if err != nil {
			Fail(c, errs.NotFound("file not found"))
			return
		}
		name := filepath.Base(filepath.ToSlash(path))
		c.Header("Content-Disposition", "attachment; filename=\""+name+"\"")
		c.Data(200, "application/octet-stream", data)
		return
	}

	root, err := s.botWorkspaceRoot(botID)
	if err != nil {
		Fail(c, errs.Internal("failed to resolve workspace root: "+err.Error()))
		return
	}
	fullPath, err := safeJoin(root, path)
	if err != nil {
		Fail(c, errs.BadRequest("invalid path"))
		return
	}
	info, err := os.Stat(fullPath)
	if err != nil || info.IsDir() {
		Fail(c, errs.NotFound("file not found"))
		return
	}
	c.FileAttachment(fullPath, filepath.Base(fullPath))
}

// --- 容器管理 (Container) ---

// BotContainerInfo 容器信息。
type BotContainerInfo struct {
	ContainerID     string `json:"containerId"`
	ContainerStatus string `json:"containerStatus"` // running | stopped | removed
	TaskStatus      string `json:"taskStatus"`
	Namespace       string `json:"namespace"`
	Image           string `json:"image"`
	CdiDevice       string `json:"cdiDevice"`
	ContainerPath   string `json:"containerPath"`
	KeepData        bool   `json:"keepData"`
	MemoryLimitMB   int64  `json:"memoryLimitMB"` // 0 = 不限制（使用系统默认）
	CreatedAt       string `json:"createdAt"`
	UpdatedAt       string `json:"updatedAt"`
}

// BotContainerSnapshot 容器快照。
type BotContainerSnapshot struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Version   string `json:"version"`
	Source    string `json:"source"`
	Parent    string `json:"parent"`
	CreatedAt string `json:"createdAt"`
}

// handleGetBotContainer 获取 Bot 容器信息（真实 sandbox 状态）。
// GET /api/bots/:id/container
func (s *Server) handleGetBotContainer(c *gin.Context) {
	botID := c.Param("id")
	info := s.realBotContainerInfo(c.Request.Context(), botID)
	OK(c, info)
}

// handleUpdateBotContainerConfig 更新 Bot 容器配置（如内存限制）。
// PUT /api/bots/:id/container/config
func (s *Server) handleUpdateBotContainerConfig(c *gin.Context) {
	botID := c.Param("id")

	var req struct {
		MemoryLimitMB int64 `json:"memoryLimitMB"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		Fail(c, errs.BadRequest("invalid request body: "+err.Error()))
		return
	}
	if req.MemoryLimitMB < 0 {
		Fail(c, errs.BadRequest("memoryLimitMB must be >= 0"))
		return
	}

	if s.botSvc == nil {
		Fail(c, errs.New("bot service unavailable"))
		return
	}
	if err := s.botSvc.UpdateDefinition(botID, map[string]any{
		"memory_limit_mb": req.MemoryLimitMB,
	}); err != nil {
		Fail(c, fmt.Errorf("更新容器配置失败: %w", err))
		return
	}

	OK(c, gin.H{"ok": true})
}

// handleGetBotContainerSnapshots 获取 Bot 容器快照列表（真实 docker 镜像）。
// GET /api/bots/:id/container/snapshots
func (s *Server) handleGetBotContainerSnapshots(c *gin.Context) {
	botID := c.Param("id")
	snapshots := s.realBotContainerSnapshots(c.Request.Context(), botID)
	OK(c, snapshots)
}

// realBotContainerSnapshots 从真实 docker 镜像列出该 bot 的快照。
func (s *Server) realBotContainerSnapshots(ctx context.Context, botID string) []BotContainerSnapshot {
	out := []BotContainerSnapshot{}
	if s.botSvc == nil {
		return out
	}
	mgr, err := s.botSvc.WorkspaceManagerForBot(botID)
	if err != nil {
		return out
	}
	list, err := mgr.ListBotSnapshots(ctx, botID)
	if err != nil {
		s.logger.Warnw("list bot snapshots failed", "bot", botID, "err", err)
		return out
	}
	for _, si := range list {
		created := ""
		if !si.CreatedAt.IsZero() {
			created = si.CreatedAt.Format(time.RFC3339)
		}
		out = append(out, BotContainerSnapshot{
			ID:        si.ID,
			Name:      si.Tag,
			Version:   si.Size,
			Source:    "docker-commit",
			Parent:    si.Repo,
			CreatedAt: created,
		})
	}
	return out
}

// handleStartBotContainer 启动 Bot 容器（真实 docker start/create）+ 启动 agent 实例。
// POST /api/bots/:id/container/start
//
// 必须同时启动 agent 实例：仅启动 docker 容器会让 chat 接口因 WebChannel 不存在而
// 返回 404（bot is not running）。这与停止时「一并停 agent」对称——启动也必须一并
// 启动，否则容器在跑但 bot 不在聊天可用状态。
func (s *Server) handleStartBotContainer(c *gin.Context) {
	botID := c.Param("id")
	ctx := c.Request.Context()
	if s.botSvc == nil {
		Fail(c, errs.New("bot service unavailable"))
		return
	}
	// 1) 启动 docker 容器并清除 stopped 标记（否则后续 ensure() 拒绝拉起，工具执行失败）。
	mgr, err := s.botSvc.WorkspaceManagerForBot(botID)
	if err != nil {
		Fail(c, err)
		return
	}
	// 0) 读取 per-bot 内存限制并设置到 manager（必须是同一个 mgr 实例，
	//    因为 WorkspaceManagerForBot 对未缓存的 bot 每次返回新实例；
	//    覆盖值设在 StartBot 所用的实例上，容器创建时才会生效）。
	if def, derr := s.botSvc.GetDefinition(botID); derr == nil {
		mgr.SetBotMemoryOverride(botID, def.MemoryLimitMB)
	}
	if err := mgr.StartBot(ctx, botID); err != nil {
		Fail(c, fmt.Errorf("启动容器失败: %w", err))
		return
	}
	// 2) 启动 agent 实例（注册 WebChannel，使聊天可用）。
	//    若 agent 已在运行则跳过，避免每次点启动都重启 agent。
	if !s.botSvc.IsRunning(botID) {
		if err := s.botSvc.StartBot(ctx, botID); err != nil {
			Fail(c, fmt.Errorf("启动 Bot 实例失败: %w", err))
			return
		}
	}
	// 3) DB 状态恢复为 running（与停止时置 stopped 对称）。
	s.botSvc.SetBotStatus(botID, dao.BotStatusRunning)
	OK(c, s.realBotContainerInfo(ctx, botID))
}

// handleStopBotContainer 停止 Bot 容器（真实 docker stop，保留数据）。
// POST /api/bots/:id/container/stop
func (s *Server) handleStopBotContainer(c *gin.Context) {
	botID := c.Param("id")
	if s.botSvc != nil {
		// 先停掉 bot 的 agent 实例：取消在跑的任务并使 DB status 置为 stopped，
		// 避免残留的 exec 又把刚停止的容器拉起来。
		s.botSvc.StopBot(botID)
		if mgr, err := s.botSvc.WorkspaceManagerForBot(botID); err == nil {
			if err := mgr.StopBot(botID); err != nil {
				Fail(c, fmt.Errorf("停止容器失败: %w", err))
				return
			}
		} else {
			Fail(c, err)
			return
		}
	}
	OK(c, s.realBotContainerInfo(c.Request.Context(), botID))
}

// handleCreateBotContainerSnapshot 创建容器快照（真实 docker commit）。
// POST /api/bots/:id/container/snapshots
func (s *Server) handleCreateBotContainerSnapshot(c *gin.Context) {
	botID := c.Param("id")

	var req struct {
		DisplayName string `json:"displayName"`
	}
	_ = c.ShouldBindJSON(&req)

	if s.botSvc == nil {
		Fail(c, fmt.Errorf("bot service unavailable"))
		return
	}
	mgr, err := s.botSvc.WorkspaceManagerForBot(botID)
	if err != nil {
		Fail(c, err)
		return
	}

	// 生成合法的镜像 tag（docker tag 只允许 [a-z0-9._-]，且不能大写）。
	tag := sanitizeSnapshotTag(req.DisplayName)
	if tag == "" {
		tag = fmt.Sprintf("snap-%d", time.Now().Unix())
	}

	id, err := mgr.SnapshotBot(c.Request.Context(), botID, tag)
	if err != nil {
		Fail(c, fmt.Errorf("创建快照失败: %w", err))
		return
	}

	OK(c, BotContainerSnapshot{
		ID:        id,
		Name:      tag,
		Version:   "-",
		Source:    "docker-commit",
		CreatedAt: nowRFC3339(),
	})
}

// sanitizeSnapshotTag 把用户输入转成合法的 docker 镜像 tag。
func sanitizeSnapshotTag(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	out := strings.Trim(b.String(), "-._")
	if len(out) > 100 {
		out = out[:100]
	}
	return out
}

// handleExportBotContainer 导出 Bot 容器数据。
// POST /api/bots/:id/container/export
func (s *Server) handleExportBotContainer(c *gin.Context) {
	Fail(c, fmt.Errorf("数据导出功能尚未实现，敬请期待"))
}

// handleImportBotContainer 导入 Bot 容器数据。
// POST /api/bots/:id/container/import
func (s *Server) handleImportBotContainer(c *gin.Context) {
	Fail(c, fmt.Errorf("数据导入功能尚未实现，敬请期待"))
}

// handleRestoreBotContainer 恢复 Bot 容器。
// POST /api/bots/:id/container/restore
func (s *Server) handleRestoreBotContainer(c *gin.Context) {
	Fail(c, fmt.Errorf("数据恢复功能尚未实现，敬请期待"))
}

// handleRemoveBotContainer 删除 Bot 容器。
// DELETE /api/bots/:id/container
func (s *Server) handleRemoveBotContainer(c *gin.Context) {
	botID := c.Param("id")

	var req struct {
		KeepData bool `json:"keepData"`
	}
	_ = c.ShouldBindJSON(&req)

	info := s.getBotContainerInfo(botID)
	info.ContainerStatus = "removed"
	info.TaskStatus = "stopped"
	info.KeepData = req.KeepData
	info.UpdatedAt = nowRFC3339()
	if err := s.saveBotContainerInfo(c, botID, info); err != nil {
		Fail(c, err)
		return
	}

	// 真实销毁 docker 持久容器（removeData 与 KeepData 相反）。
	if s.botSvc != nil {
		cfg := s.botSvc.SandboxConfigForBot()
		if mgr, err := sandbox.NewBotWorkspaceManager(s.botSvc.GetWorkspaceBaseDir(), cfg, s.logger); err == nil {
			if err := mgr.DestroyBot(botID, !req.KeepData); err != nil {
				s.logger.Warnw("destroy bot container failed", "bot", botID, "err", err)
			}
		}
	}
	OK(c, nil)
}

// shellQuoteArg 用单引号安全包裹一个 shell 参数。
func shellQuoteArg(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// --- 上下文压缩 (Compaction) ---

// BotCompactionConfig 上下文压缩配置。
type BotCompactionConfig struct {
	Enabled   bool   `json:"enabled"`
	Threshold int    `json:"threshold"`
	Ratio     int    `json:"ratio"`
	Model     string `json:"model"`
}

// BotCompactionRecord 压缩记录。
type BotCompactionRecord struct {
	ID     string  `json:"id"`
	Status string  `json:"status"` // "success" | "failed"
	Time   string  `json:"time"`
	Cost   float64 `json:"cost"`
	Error  string  `json:"error"`
}

// handleGetBotCompaction 获取 Bot 上下文压缩配置。
// GET /api/bots/:id/compaction
func (s *Server) handleGetBotCompaction(c *gin.Context) {
	botID := c.Param("id")
	cfg := s.getBotCompactionConfig(botID)
	OK(c, cfg)
}

// handleUpdateBotCompaction 更新 Bot 上下文压缩配置。
// PUT /api/bots/:id/compaction
func (s *Server) handleUpdateBotCompaction(c *gin.Context) {
	botID := c.Param("id")

	var req BotCompactionConfig
	if err := c.ShouldBindJSON(&req); err != nil {
		Fail(c, errs.BadRequest("invalid request body: "+err.Error()))
		return
	}

	if err := s.saveBotCompactionConfig(c, botID, &req); err != nil {
		Fail(c, err)
		return
	}
	OK(c, nil)
}

// handleGetBotCompactionHistory 获取上下文压缩历史记录。
// GET /api/bots/:id/compaction/history?status=all
func (s *Server) handleGetBotCompactionHistory(c *gin.Context) {
	botID := c.Param("id")
	status := c.DefaultQuery("status", "all")

	records := s.getBotCompactionHistory(botID)
	if status != "" && status != "all" {
		filtered := make([]BotCompactionRecord, 0)
		for _, r := range records {
			if r.Status == status {
				filtered = append(filtered, r)
			}
		}
		records = filtered
	}

	OK(c, gin.H{"records": records, "total": len(records)})
}

// handleClearBotCompactionHistory 清空上下文压缩历史记录。
// DELETE /api/bots/:id/compaction/history
func (s *Server) handleClearBotCompactionHistory(c *gin.Context) {
	botID := c.Param("id")
	if err := s.saveBotCompactionHistory(c, botID, []BotCompactionRecord{}); err != nil {
		Fail(c, err)
		return
	}
	OK(c, nil)
}

// ============================================================================
// 存储辅助方法 — 全部使用 config store 持久化
// ============================================================================

func botDetailKey(botID, sub string) string {
	return "bot." + botID + ".detail." + sub
}

func (s *Server) getBotPlatforms(botID string) []BotPlatform {
	raw, ok := s.store.Get(botDetailKey(botID, "platforms"))
	if !ok || raw == "" {
		return []BotPlatform{}
	}
	var result []BotPlatform
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		return []BotPlatform{}
	}
	return result
}

func (s *Server) saveBotPlatforms(c *gin.Context, botID string, platforms []BotPlatform) error {
	data, _ := json.Marshal(platforms)
	return s.store.Set(c.Request.Context(), botDetailKey(botID, "platforms"), string(data))
}

func (s *Server) getBotMemoryEntries(botID string) []BotMemoryEntry {
	raw, ok := s.store.Get(botDetailKey(botID, "memory_entries"))
	if !ok || raw == "" {
		return []BotMemoryEntry{}
	}
	var result []BotMemoryEntry
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		return []BotMemoryEntry{}
	}
	return result
}

func (s *Server) saveBotMemoryEntries(c *gin.Context, botID string, entries []BotMemoryEntry) error {
	data, _ := json.Marshal(entries)
	return s.store.Set(c.Request.Context(), botDetailKey(botID, "memory_entries"), string(data))
}

// botWorkspaceRoot 返回指定 bot 的工作目录根路径 {workspaceDir}/{botID}，
// 并确保该目录存在。
func (s *Server) botWorkspaceRoot(botID string) (string, error) {
	workspaceDir := config.NewBuilder(s.store, s.logger).GetWorkspaceDir()
	root := filepath.Join(workspaceDir, botID)
	if err := os.MkdirAll(root, 0o755); err != nil {
		return "", err
	}
	// 返回绝对路径，便于后续前缀校验的一致性。
	abs, err := filepath.Abs(root)
	if err != nil {
		return root, nil
	}
	return abs, nil
}

// safeJoin 将用户传入的相对路径安全地拼接到 root 之下，防止目录穿越。
// 返回的 fullPath 保证始终位于 root 之内（含 root 自身）。
// elems 可以是 path、name 等多个片段，会依次清理拼接。
func safeJoin(root string, elems ...string) (string, error) {
	// 先把所有片段拼成一个相对路径，再统一清理。
	rel := filepath.Join(elems...)
	// 剥离 agent 面向的虚拟根前缀 /data（local 模式下映射回宿主真实 root）。
	rel = filepath.ToSlash(rel)
	if rel == sandbox.VirtualRoot {
		rel = ""
	} else if strings.HasPrefix(rel, sandbox.VirtualRoot+"/") {
		rel = rel[len(sandbox.VirtualRoot)+1:]
	}
	rel = filepath.FromSlash(rel)
	// 去掉开头的分隔符，避免被当成绝对路径。
	rel = strings.TrimPrefix(rel, string(filepath.Separator))
	rel = filepath.Clean("/" + rel) // 归一化，消除 .. 逃逸到根之上的情况
	rel = strings.TrimPrefix(rel, "/")

	full := filepath.Join(root, rel)

	// 前缀校验：full 必须等于 root 或在 root/ 之下。
	if full != root && !strings.HasPrefix(full, root+string(filepath.Separator)) {
		return "", errs.BadRequest("invalid path: escapes workspace root")
	}
	// 双保险：用 Rel 检查是否包含向上逃逸。
	relCheck, err := filepath.Rel(root, full)
	if err != nil || relCheck == ".." || strings.HasPrefix(relCheck, ".."+string(filepath.Separator)) {
		return "", errs.BadRequest("invalid path: escapes workspace root")
	}
	return full, nil
}

// botFileWorkspace 返回 docker 持久容器模式下的容器工作空间；
// 若当前不是 docker 隔离模式（local 后端），返回 (nil, false)，调用方走宿主磁盘直读逻辑。
//
// 优先复用运行中 bot 的工作空间管理器（与运行时状态完全一致）；
// bot 未运行时按当前配置临时构造一个 manager（用于离线浏览容器文件）。
func (s *Server) botFileWorkspace(botID string) (sandbox.Workspace, bool) {
	var mgr *sandbox.BotWorkspaceManager
	if s.botSvc != nil {
		if m, ok := s.botSvc.RunningBotWorkspaceMgr(botID); ok {
			mgr = m
		}
	}
	if mgr == nil && s.botSvc != nil {
		cfg := s.botSvc.SandboxConfigForBot()
		m, err := sandbox.NewBotWorkspaceManager(s.botSvc.GetWorkspaceBaseDir(), cfg, s.logger)
		if err != nil {
			return nil, false
		}
		mgr = m
	}
	if mgr == nil || mgr.Backend() != "docker" {
		return nil, false // local 模式：走宿主磁盘
	}
	ws, err := mgr.GetOrCreate(botID)
	if err != nil {
		return nil, false
	}
	return ws, true
}

func (s *Server) getBotFileEntries(botID, path string) []BotFileEntry {
	// docker 隔离模式：文件在容器内，通过容器列目录。
	if ws, ok := s.botFileWorkspace(botID); ok {
		entries, err := ws.ListDir(context.Background(), path)
		if err != nil {
			return []BotFileEntry{}
		}
		result := make([]BotFileEntry, 0, len(entries))
		for _, e := range entries {
			be := BotFileEntry{Name: e.Name, Size: e.Size}
			if e.IsDir {
				be.Type = "dir"
				be.Size = 0
			} else {
				be.Type = "file"
			}
			if !e.ModTime.IsZero() {
				be.Mtime = e.ModTime.UTC().Format(time.RFC3339)
			} else {
				be.Mtime = nowRFC3339()
			}
			result = append(result, be)
		}
		return result
	}

	root, err := s.botWorkspaceRoot(botID)
	if err != nil {
		return []BotFileEntry{}
	}

	fullPath, err := safeJoin(root, path)
	if err != nil {
		return []BotFileEntry{}
	}

	dirEntries, err := os.ReadDir(fullPath)
	if err != nil {
		// 目录不存在或无法读取，返回空切片而非报错。
		return []BotFileEntry{}
	}

	entries := make([]BotFileEntry, 0, len(dirEntries))
	for _, de := range dirEntries {
		info, err := de.Info()
		if err != nil {
			continue
		}
		entry := BotFileEntry{
			Name:  de.Name(),
			Mtime: info.ModTime().UTC().Format(time.RFC3339),
		}
		if de.IsDir() {
			entry.Type = "dir"
			entry.Size = 0
		} else {
			entry.Type = "file"
			entry.Size = info.Size()
		}
		entries = append(entries, entry)
	}
	return entries
}

func (s *Server) botFileMkdir(c *gin.Context, botID, path, name string) error {
	// 校验 name 合法性：不含路径分隔符，不为 . 或 ..
	if name == "" || name == "." || name == ".." ||
		strings.ContainsRune(name, '/') || strings.ContainsRune(name, filepath.Separator) {
		return errs.BadRequest("invalid directory name")
	}

	// docker 隔离模式：在容器内创建目录。
	if ws, ok := s.botFileWorkspace(botID); ok {
		target := filepath.ToSlash(filepath.Join(path, name))
		res, err := ws.Exec(c.Request.Context(), sandbox.ExecRequest{
			Command: "mkdir -p " + shellQuoteArg("/workspace/"+strings.TrimPrefix(target, "/")),
		})
		if err != nil {
			return errs.Internal("failed to create directory: " + err.Error())
		}
		if res.ExitCode != 0 {
			return errs.Internal("failed to create directory: " + res.Stderr)
		}
		return nil
	}

	root, err := s.botWorkspaceRoot(botID)
	if err != nil {
		return errs.Internal("failed to resolve workspace root: " + err.Error())
	}

	target, err := safeJoin(root, path, name)
	if err != nil {
		return err
	}

	if _, statErr := os.Stat(target); statErr == nil {
		return errs.Conflict("directory already exists")
	}

	if err := os.MkdirAll(target, 0o755); err != nil {
		return errs.Internal("failed to create directory: " + err.Error())
	}
	return nil
}

func (s *Server) botFileUpload(c *gin.Context, botID, path, name string, content io.Reader) error {
	// 校验文件名合法性。
	if name == "" || name == "." || name == ".." ||
		strings.ContainsRune(name, '/') || strings.ContainsRune(name, filepath.Separator) {
		return errs.BadRequest("invalid file name")
	}

	// docker 隔离模式：写入容器内。
	if ws, ok := s.botFileWorkspace(botID); ok {
		data, err := io.ReadAll(content)
		if err != nil {
			return errs.Internal("failed to read upload: " + err.Error())
		}
		target := filepath.ToSlash(filepath.Join(path, name))
		if err := ws.WriteFile(c.Request.Context(), target, data); err != nil {
			return errs.Internal("failed to write file into container: " + err.Error())
		}
		return nil
	}

	root, err := s.botWorkspaceRoot(botID)
	if err != nil {
		return errs.Internal("failed to resolve workspace root: " + err.Error())
	}

	// 目标所在目录（path 部分）与文件路径分别做安全校验。
	dir, err := safeJoin(root, path)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return errs.Internal("failed to prepare directory: " + err.Error())
	}

	target, err := safeJoin(root, path, name)
	if err != nil {
		return err
	}

	dst, err := os.Create(target)
	if err != nil {
		return errs.Internal("failed to create file: " + err.Error())
	}
	defer func() { _ = dst.Close() }()

	if _, err := io.Copy(dst, content); err != nil {
		return errs.Internal("failed to write file: " + err.Error())
	}
	return nil
}

// realBotContainerInfo 从真实 sandbox 读取容器信息，映射为前端 BotContainerInfo。
// 状态字段（容器 ID / 状态 / 镜像 / 路径）以 sandbox 真实数据为准；
// keepData / 时间等可持久化偏好从 store 合并。
func (s *Server) realBotContainerInfo(ctx context.Context, botID string) *BotContainerInfo {
	stored := s.getBotContainerInfo(botID)
	if s.botSvc == nil {
		return stored
	}
	mgr, err := s.botSvc.WorkspaceManagerForBot(botID)
	if err != nil {
		return stored
	}
	// 只读获取容器信息：不调用 GetOrCreate（其内部 ensure() 会在容器 exited 时自动 restart），
	// 避免查状态时副作用地重启已停止的容器。
	ci := mgr.ContainerInfo(ctx, botID)

	info := &BotContainerInfo{
		Namespace:     "default",
		CdiDevice:     "未附加 GPU",
		KeepData:      stored.KeepData,
		MemoryLimitMB: stored.MemoryLimitMB,
		CreatedAt:     stored.CreatedAt,
		UpdatedAt:     nowRFC3339(),
	}
	// 从 bot definition DB 读取 memoryLimitMB（优先级高于 store 缓存）。
	if def, err := s.botSvc.GetDefinition(botID); err == nil && def.MemoryLimitMB > 0 {
		info.MemoryLimitMB = def.MemoryLimitMB
	}
	info.Image = ci.Image
	info.ContainerPath = ci.WorkDir

	if ci.Backend == "docker" && ci.Persistent {
		info.ContainerID = ci.ContainerID
		switch ci.State {
		case "running":
			info.ContainerStatus = "running"
			info.TaskStatus = "running"
		case "", "not-created":
			info.ContainerStatus = "stopped"
			info.TaskStatus = "not-created"
		case "docker-unavailable":
			info.ContainerStatus = "error"
			info.TaskStatus = "docker-unavailable"
		default: // exited / paused / created ...
			info.ContainerStatus = "stopped"
			info.TaskStatus = ci.State
		}
		if info.ContainerPath == "" && ci.Volume != "" {
			info.ContainerPath = ci.Volume + ":/workspace"
		}
	} else {
		// 本地模式：无独立容器
		info.ContainerID = ""
		info.ContainerStatus = "local"
		info.TaskStatus = "local"
		info.Image = "(local process)"
	}
	return info
}

func (s *Server) getBotContainerInfo(botID string) *BotContainerInfo {
	raw, ok := s.store.Get(botDetailKey(botID, "container"))
	if !ok || raw == "" {
		return &BotContainerInfo{
			ContainerID:     "",
			ContainerStatus: "stopped",
			TaskStatus:      "stopped",
			Namespace:       "default",
			Image:           "",
			CdiDevice:       "未附加 GPU",
			ContainerPath:   "",
			KeepData:        false,
			CreatedAt:       nowRFC3339(),
			UpdatedAt:       nowRFC3339(),
		}
	}
	var info BotContainerInfo
	if err := json.Unmarshal([]byte(raw), &info); err != nil {
		return &BotContainerInfo{ContainerStatus: "stopped", TaskStatus: "stopped"}
	}
	return &info
}

func (s *Server) saveBotContainerInfo(c *gin.Context, botID string, info *BotContainerInfo) error {
	data, _ := json.Marshal(info)
	return s.store.Set(c.Request.Context(), botDetailKey(botID, "container"), string(data))
}

func (s *Server) getBotCompactionConfig(botID string) *BotCompactionConfig {
	raw, ok := s.store.Get(botDetailKey(botID, "compaction"))
	if !ok || raw == "" {
		return &BotCompactionConfig{Enabled: true, Threshold: 131072, Ratio: 37, Model: "deepseek-v4-flash"}
	}
	var cfg BotCompactionConfig
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		return &BotCompactionConfig{Enabled: true, Threshold: 131072, Ratio: 37}
	}
	return &cfg
}

func (s *Server) saveBotCompactionConfig(c *gin.Context, botID string, cfg *BotCompactionConfig) error {
	data, _ := json.Marshal(cfg)
	return s.store.Set(c.Request.Context(), botDetailKey(botID, "compaction"), string(data))
}

func (s *Server) getBotCompactionHistory(botID string) []BotCompactionRecord {
	raw, ok := s.store.Get(botDetailKey(botID, "compaction_history"))
	if !ok || raw == "" {
		return []BotCompactionRecord{}
	}
	var result []BotCompactionRecord
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		return []BotCompactionRecord{}
	}
	return result
}

func (s *Server) saveBotCompactionHistory(c *gin.Context, botID string, records []BotCompactionRecord) error {
	data, _ := json.Marshal(records)
	return s.store.Set(c.Request.Context(), botDetailKey(botID, "compaction_history"), string(data))
}

// ============================================================================
// 聊天节奏 (Rhythm)
// ============================================================================

// ============================================================================
// 聊天节奏（按平台 + 会话类型细分）
// GET    /api/bots/:id/chat-rhythm            → 返回所有支持平台的配置(map)
// GET    /api/bots/:id/chat-rhythm/:platform  → 单个平台配置
// PUT    /api/bots/:id/chat-rhythm/:platform  → 保存单个平台配置
// 存储键：bot.<id>.rhythm.<platform>
// web 平台明确不参与节奏控制（在消费方 RhythmStage 硬性跳过）。
// ============================================================================

// botRhythmPlatforms 支持节奏配置的平台（web 不在列，永不参与）。
var botRhythmPlatforms = []string{"telegram", "misskey"}

// BotRhythmConfig 按平台配置的聊天节奏。每个平台一份。
type BotRhythmConfig struct {
	Enabled bool            `json:"enabled"` // 平台级总开关
	Private BotRhythmParams `json:"private"` // 单聊（1对1）参数
	Group   BotRhythmParams `json:"group"`   // 群聊 / 超级群参数
	Channel BotRhythmParams `json:"channel"` // 频道（只读广播）参数
}

// BotRhythmParams 某个会话类型的节奏参数。
type BotRhythmParams struct {
	Enabled       bool               `json:"enabled"` // 该会话类型是否启用节奏控制（false=完全关闭，即时回复）
	Debounce      BotRhythmDebounce  `json:"debounce"`
	Timing        BotRhythmToggle    `json:"timing"`
	SpeakTendency float64            `json:"speakTendency"`
	Interrupt     BotRhythmInterrupt `json:"interrupt"`
	IdleComp      BotRhythmIdleComp  `json:"idleComp"`
}

// BotRhythmDebounce 消息防抖：频道内两次发言间隔小于 QuietWait 视为连发，合并跳过。
type BotRhythmDebounce struct {
	QuietWait int `json:"quietWait"`
	MaxWait   int `json:"maxWait"`
}

// BotRhythmToggle 开关型子项。
type BotRhythmToggle struct {
	Enabled bool `json:"enabled"`
}

// BotRhythmInterrupt 连续发言中断。
type BotRhythmInterrupt struct {
	Enabled        bool `json:"enabled"`
	MaxConsecutive int  `json:"maxConsecutive"`
	MaxRounds      int  `json:"maxRounds"`
}

// BotRhythmIdleComp 空闲补偿。
type BotRhythmIdleComp struct {
	Enabled    bool `json:"enabled"`
	IdleWindow int  `json:"idleWindow"`
	MinIdle    int  `json:"minIdle"`
}

// defaultBotRhythmConfigForPlatform 返回某平台的默认节奏配置。
// 设计：单聊(private)默认关闭节奏（即时回复，不受控）；群聊/频道默认开启（受控不刷屏）。
func defaultBotRhythmConfigForPlatform(platform string) *BotRhythmConfig {
	group := BotRhythmParams{
		Enabled:       true,
		Debounce:      BotRhythmDebounce{QuietWait: 3, MaxWait: 15},
		Timing:        BotRhythmToggle{Enabled: true},
		SpeakTendency: 0.4,
		Interrupt:     BotRhythmInterrupt{Enabled: true, MaxConsecutive: 3, MaxRounds: 5},
		IdleComp:      BotRhythmIdleComp{Enabled: false, IdleWindow: 30, MinIdle: 10},
	}
	private := BotRhythmParams{
		Enabled:       false, // 单聊默认关闭节奏：即时回复
		Debounce:      BotRhythmDebounce{QuietWait: 1, MaxWait: 5},
		Timing:        BotRhythmToggle{Enabled: false},
		SpeakTendency: 1.0,
		Interrupt:     BotRhythmInterrupt{Enabled: false, MaxConsecutive: 0, MaxRounds: 0},
		IdleComp:      BotRhythmIdleComp{Enabled: false},
	}
	return &BotRhythmConfig{Enabled: true, Private: private, Group: group, Channel: group}
}

func botRhythmKey(botID, platform string) string {
	return "bot." + botID + ".rhythm." + platform
}

func isValidRhythmPlatform(p string) bool {
	for _, x := range botRhythmPlatforms {
		if x == p {
			return true
		}
	}
	return false
}

// getBotRhythmConfig 从平台对象读取聊天节奏配置（节奏已合并进平台设置）。
// 优先读 bot.<id>.detail.platforms 中 type==platform 的 BotPlatform.Rhythm；
// 若该字段为空（历史数据），回退读旧独立 key bot.<id>.rhythm.<platform>，
// 待用户下一次保存平台设置时自然迁移覆盖。两者皆空则返回默认。
func (s *BotService) getBotRhythmConfig(botID, platform string) *BotRhythmConfig {
	if !isValidRhythmPlatform(platform) {
		return defaultBotRhythmConfigForPlatform(platform)
	}
	// 优先：节奏已合并进平台对象（bot.<id>.detail.platforms）
	if raw, ok := s.store.Get(botDetailKey(botID, "platforms")); ok && raw != "" {
		var plats []BotPlatform
		if err := json.Unmarshal([]byte(raw), &plats); err == nil {
			for _, p := range plats {
				if p.Type == platform && p.Rhythm != nil {
					fillRhythmDefaults(p.Rhythm, platform)
					return p.Rhythm
				}
			}
		}
	}
	// 回退：历史独立 key（bot.<id>.rhythm.<platform>），待用户下一次保存平台设置时自然迁移覆盖
	if raw, ok := s.store.Get(botRhythmKey(botID, platform)); ok && raw != "" {
		var cfg BotRhythmConfig
		if err := json.Unmarshal([]byte(raw), &cfg); err == nil {
			fillRhythmDefaults(&cfg, platform)
			return &cfg
		}
	}
	return defaultBotRhythmConfigForPlatform(platform)
}

// fillRhythmDefaults 补齐缺失的子配置（private/group/channel），避免下游空指针。
//
// 判据是「全字段零值 = 未配置」。这在读取侧是一个**近似**判断：
// 用户显式关闭某会话类型（enabled=false）时，只要其它字段非零（前端始终提交
// 完整结构，如 quietWait=3）就不会被误判。为不依赖前端行为，
// handleUpdateBotPlatform 在**保存前**也会调用本函数，使落库数据始终完整。
func fillRhythmDefaults(cfg *BotRhythmConfig, platform string) {
	def := defaultBotRhythmConfigForPlatform(platform)
	if cfg.Private == (BotRhythmParams{}) {
		cfg.Private = def.Private
	}
	if cfg.Group == (BotRhythmParams{}) {
		cfg.Group = def.Group
	}
	if cfg.Channel == (BotRhythmParams{}) {
		cfg.Channel = def.Channel
	}
}

// selectRhythmParams 按会话类型选取参数；supergroup 归入 group，未知回退 group（更保守）。
func selectRhythmParams(cfg *BotRhythmConfig, chatType string) BotRhythmParams {
	switch chatType {
	case core.ChatPrivate:
		return cfg.Private
	case core.ChatGroup, core.ChatSupergroup:
		return cfg.Group
	case core.ChatChannel:
		return cfg.Channel
	default:
		return cfg.Group
	}
}

// nowRFC3339 返回当前时间的 RFC3339 格式字符串。
func nowRFC3339() string {
	return time.Now().UTC().Format("2006-01-02T15:04:05Z")
}

// ============================================================================
// 访问控制（图3：默认行为 + 规则列表）
// GET/PUT /api/bots/:id/access
// 类型 BotAccessConfig / BotAccessRule 见上方“--- 访问控制 (Access) ---”区块。
// ============================================================================

// defaultBotAccessConfig 返回访问控制默认配置（默认放行）。
func defaultBotAccessConfig() *BotAccessConfig {
	return &BotAccessConfig{
		Default: "allow",
		Rules:   []BotAccessRule{},
	}
}

// handleGetBotAccess 获取 Bot 访问控制配置。
// GET /api/bots/:id/access
func (s *Server) handleGetBotAccess(c *gin.Context) {
	botID := c.Param("id")
	OK(c, s.getBotAccessConfig(botID))
}

// handleUpdateBotAccess 更新 Bot 访问控制配置。
// PUT /api/bots/:id/access
func (s *Server) handleUpdateBotAccess(c *gin.Context) {
	botID := c.Param("id")

	var req BotAccessConfig
	if err := c.ShouldBindJSON(&req); err != nil {
		Fail(c, errs.BadRequest("invalid request body: "+err.Error()))
		return
	}
	// 规范化默认值
	if req.Default != "deny" {
		req.Default = "allow"
	}
	for i := range req.Rules {
		if req.Rules[i].Action != "deny" {
			req.Rules[i].Action = "allow"
		}
	}

	if err := s.saveBotAccessConfig(c, botID, &req); err != nil {
		Fail(c, err)
		return
	}
	OK(c, nil)
}

func (s *Server) getBotAccessConfig(botID string) *BotAccessConfig {
	raw, ok := s.store.Get(botDetailKey(botID, "access"))
	if !ok || raw == "" {
		return defaultBotAccessConfig()
	}
	var cfg BotAccessConfig
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		return defaultBotAccessConfig()
	}
	if cfg.Default != "deny" {
		cfg.Default = "allow"
	}
	return &cfg
}

func (s *Server) saveBotAccessConfig(c *gin.Context, botID string, cfg *BotAccessConfig) error {
	data, _ := json.Marshal(cfg)
	return s.store.Set(c.Request.Context(), botDetailKey(botID, "access"), string(data))
}
