package api

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/kasuganosora/thinkbot/config"
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
	ID         string         `json:"id"`
	Type       string         `json:"type"`
	Name       string         `json:"name"`
	Enabled    bool           `json:"enabled"`
	Configured bool           `json:"configured"`
	Config     map[string]any `json:"config"`
	Tools      []string       `json:"tools"`
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
	p.Configured = true

	if err := s.saveBotPlatforms(c, botID, platforms); err != nil {
		Fail(c, err)
		return
	}
	OK(c, *p)
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
	ID       string `json:"id,omitempty"`
	Type     string `json:"type"`   // "user" | "group" | "channel"
	Target   string `json:"target"` // 匹配目标
	Action   string `json:"action"` // "allow" | "deny"
	Priority int    `json:"priority"`
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
	defer f.Close()

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

// handleGetBotContainer 获取 Bot 容器信息。
// GET /api/bots/:id/container
func (s *Server) handleGetBotContainer(c *gin.Context) {
	botID := c.Param("id")
	info := s.getBotContainerInfo(botID)
	OK(c, info)
}

// handleGetBotContainerSnapshots 获取 Bot 容器快照列表。
// GET /api/bots/:id/container/snapshots
func (s *Server) handleGetBotContainerSnapshots(c *gin.Context) {
	botID := c.Param("id")
	snapshots := s.getBotContainerSnapshots(botID)
	OK(c, snapshots)
}

// handleStartBotContainer 启动 Bot 容器。
// POST /api/bots/:id/container/start
func (s *Server) handleStartBotContainer(c *gin.Context) {
	botID := c.Param("id")
	info := s.getBotContainerInfo(botID)
	info.ContainerStatus = "running"
	info.TaskStatus = "running"
	info.UpdatedAt = nowRFC3339()
	if err := s.saveBotContainerInfo(c, botID, info); err != nil {
		Fail(c, err)
		return
	}
	OK(c, info)
}

// handleStopBotContainer 停止 Bot 容器。
// POST /api/bots/:id/container/stop
func (s *Server) handleStopBotContainer(c *gin.Context) {
	botID := c.Param("id")
	info := s.getBotContainerInfo(botID)
	info.ContainerStatus = "stopped"
	info.TaskStatus = "stopped"
	info.UpdatedAt = nowRFC3339()
	if err := s.saveBotContainerInfo(c, botID, info); err != nil {
		Fail(c, err)
		return
	}
	OK(c, info)
}

// handleCreateBotContainerSnapshot 创建容器快照。
// POST /api/bots/:id/container/snapshots
func (s *Server) handleCreateBotContainerSnapshot(c *gin.Context) {
	botID := c.Param("id")

	var req struct {
		DisplayName string `json:"displayName"`
	}
	_ = c.ShouldBindJSON(&req)

	name := req.DisplayName
	if name == "" {
		name = fmt.Sprintf("snapshot-%d", len(s.getBotContainerSnapshots(botID))+1)
	}

	info := s.getBotContainerInfo(botID)
	snap := BotContainerSnapshot{
		ID:        fmt.Sprintf("snap-%s", generateModelID(name)),
		Name:      name,
		Version:   "-",
		Source:    "manual",
		Parent:    info.ContainerID,
		CreatedAt: nowRFC3339(),
	}

	snapshots := s.getBotContainerSnapshots(botID)
	snapshots = append([]BotContainerSnapshot{snap}, snapshots...)
	if err := s.saveBotContainerSnapshots(c, botID, snapshots); err != nil {
		Fail(c, err)
		return
	}
	OK(c, snap)
}

// handleExportBotContainer 导出 Bot 容器数据。
// POST /api/bots/:id/container/export
func (s *Server) handleExportBotContainer(c *gin.Context) {
	botID := c.Param("id")
	// TODO: 实际导出逻辑
	OK(c, gin.H{"url": fmt.Sprintf("/api/bots/%s/container/export/download", botID)})
}

// handleImportBotContainer 导入 Bot 容器数据。
// POST /api/bots/:id/container/import
func (s *Server) handleImportBotContainer(c *gin.Context) {
	// TODO: 实际导入逻辑
	OK(c, nil)
}

// handleRestoreBotContainer 恢复 Bot 容器。
// POST /api/bots/:id/container/restore
func (s *Server) handleRestoreBotContainer(c *gin.Context) {
	// TODO: 实际恢复逻辑
	OK(c, nil)
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
	OK(c, nil)
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

func (s *Server) getBotFileEntries(botID, path string) []BotFileEntry {
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
	defer dst.Close()

	if _, err := io.Copy(dst, content); err != nil {
		return errs.Internal("failed to write file: " + err.Error())
	}
	return nil
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

func (s *Server) getBotContainerSnapshots(botID string) []BotContainerSnapshot {
	raw, ok := s.store.Get(botDetailKey(botID, "container_snapshots"))
	if !ok || raw == "" {
		return []BotContainerSnapshot{}
	}
	var result []BotContainerSnapshot
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		return []BotContainerSnapshot{}
	}
	return result
}

func (s *Server) saveBotContainerSnapshots(c *gin.Context, botID string, snapshots []BotContainerSnapshot) error {
	data, _ := json.Marshal(snapshots)
	return s.store.Set(c.Request.Context(), botDetailKey(botID, "container_snapshots"), string(data))
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

// BotRhythmConfig 聊天节奏配置。
type BotRhythmConfig struct {
	Enabled       bool                   `json:"enabled"`
	Debounce      BotRhythmDebounce     `json:"debounce"`
	Timing        BotRhythmToggle       `json:"timing"`
	SpeakTendency float64               `json:"speakTendency"`
	Interrupt     BotRhythmInterrupt    `json:"interrupt"`
	IdleComp      BotRhythmIdleComp     `json:"idleComp"`
}

// BotRhythmDebounce 消息防抖。
type BotRhythmDebounce struct {
	QuietWait int `json:"quietWait"`
	MaxWait   int `json:"maxWait"`
}

// BotRhythmToggle 开关型子项。
type BotRhythmToggle struct {
	Enabled bool `json:"enabled"`
}

// BotRhythmInterrupt 计划中断。
type BotRhythmInterrupt struct {
	Enabled          bool `json:"enabled"`
	MaxConsecutive   int  `json:"maxConsecutive"`
	MaxRounds        int  `json:"maxRounds"`
}

// BotRhythmIdleComp 空闲补偿。
type BotRhythmIdleComp struct {
	Enabled  bool `json:"enabled"`
	IdleWindow int `json:"idleWindow"`
	MinIdle  int  `json:"minIdle"`
}

// defaultBotRhythmConfig 返回聊天节奏默认配置。
func defaultBotRhythmConfig() *BotRhythmConfig {
	return &BotRhythmConfig{
		Enabled:       true,
		Debounce:      BotRhythmDebounce{QuietWait: 3, MaxWait: 15},
		Timing:        BotRhythmToggle{Enabled: true},
		SpeakTendency: 0.5,
		Interrupt:     BotRhythmInterrupt{Enabled: true, MaxConsecutive: 3, MaxRounds: 5},
		IdleComp:      BotRhythmIdleComp{Enabled: false, IdleWindow: 30, MinIdle: 10},
	}
}

// handleGetBotRhythm 获取 Bot 聊天节奏配置。
// GET /api/bots/:id/chat-rhythm
func (s *Server) handleGetBotRhythm(c *gin.Context) {
	botID := c.Param("id")
	OK(c, s.getBotRhythmConfig(botID))
}

// handleUpdateBotRhythm 更新 Bot 聊天节奏配置。
// PUT /api/bots/:id/chat-rhythm
func (s *Server) handleUpdateBotRhythm(c *gin.Context) {
	botID := c.Param("id")

	var req BotRhythmConfig
	if err := c.ShouldBindJSON(&req); err != nil {
		Fail(c, errs.BadRequest("invalid request body: "+err.Error()))
		return
	}

	if err := s.saveBotRhythmConfig(c, botID, &req); err != nil {
		Fail(c, err)
		return
	}
	OK(c, nil)
}

func (s *Server) getBotRhythmConfig(botID string) *BotRhythmConfig {
	raw, ok := s.store.Get(botDetailKey(botID, "rhythm"))
	if !ok || raw == "" {
		return defaultBotRhythmConfig()
	}
	var cfg BotRhythmConfig
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		return defaultBotRhythmConfig()
	}
	return &cfg
}

func (s *Server) saveBotRhythmConfig(c *gin.Context, botID string, cfg *BotRhythmConfig) error {
	data, _ := json.Marshal(cfg)
	return s.store.Set(c.Request.Context(), botDetailKey(botID, "rhythm"), string(data))
}

// nowRFC3339 返回当前时间的 RFC3339 格式字符串。
func nowRFC3339() string {
	return time.Now().UTC().Format("2006-01-02T15:04:05Z")
}
