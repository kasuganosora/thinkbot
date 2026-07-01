package api

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/gin-gonic/gin"

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
	Type   string          `json:"type"`
	Name   string          `json:"name"`
	Icon   string          `json:"icon"`
	Color  string          `json:"color"`
	Fields []PlatformField `json:"fields"`
}

// PlatformField 平台配置字段定义。
type PlatformField struct {
	Key         string `json:"key"`
	Label       string `json:"label"`
	Type        string `json:"type"`
	Placeholder string `json:"placeholder,omitempty"`
	Help        string `json:"help,omitempty"`
	Optional    bool   `json:"optional,omitempty"`
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

// 内置平台类型
var platformTypes = []PlatformType{
	{Type: "dingtalk", Name: "钉钉", Icon: "📨", Color: "#3b8fff", Fields: []PlatformField{
		{Key: "clientId", Label: "Client ID", Type: "text"},
		{Key: "clientSecret", Label: "Client Secret", Type: "password"},
	}},
	{Type: "discord", Name: "Discord", Icon: "🎮", Color: "#5865f2", Fields: []PlatformField{
		{Key: "token", Label: "Bot Token", Type: "password"},
	}},
	{Type: "feishu", Name: "飞书", Icon: "🪶", Color: "#3370ff", Fields: []PlatformField{
		{Key: "appId", Label: "App ID", Type: "text"},
		{Key: "appSecret", Label: "App Secret", Type: "password"},
	}},
	{Type: "matrix", Name: "Matrix", Icon: "🌐", Color: "#0dbd8b", Fields: []PlatformField{
		{Key: "homeserver", Label: "Homeserver", Type: "text", Placeholder: "https://matrix.org"},
		{Key: "token", Label: "Access Token", Type: "password"},
	}},
	{Type: "qq", Name: "QQ", Icon: "🐧", Color: "#12b7f5", Fields: []PlatformField{
		{Key: "appId", Label: "App ID", Type: "text"},
		{Key: "clientSecret", Label: "Client Secret", Type: "password"},
		{Key: "inputHint", Label: "Input Hint", Type: "switch", Optional: true, Help: "Send QQ input-notify hints for direct messages while the bot is processing."},
		{Key: "markdown", Label: "Markdown Support", Type: "switch", Optional: true, Help: "Enable QQ markdown message mode for C2C and group replies when the bot has permission."},
	}},
	{Type: "slack", Name: "Slack", Icon: "💬", Color: "#611f69", Fields: []PlatformField{
		{Key: "botToken", Label: "Bot Token", Type: "password", Placeholder: "xoxb-..."},
	}},
	{Type: "telegram", Name: "Telegram", Icon: "✈️", Color: "#2aabee", Fields: []PlatformField{
		{Key: "token", Label: "Bot Token", Type: "password", Placeholder: "从 @BotFather 获取"},
	}},
	{Type: "wechat_mp", Name: "微信服务号", Icon: "🟢", Color: "#2dc100", Fields: []PlatformField{
		{Key: "appId", Label: "App ID", Type: "text"},
		{Key: "appSecret", Label: "App Secret", Type: "password"},
	}},
	{Type: "wecom", Name: "企业微信", Icon: "🏢", Color: "#2f90ea", Fields: []PlatformField{
		{Key: "corpId", Label: "Corp ID", Type: "text"},
		{Key: "corpSecret", Label: "Corp Secret", Type: "password"},
	}},
	{Type: "wechat", Name: "微信", Icon: "💚", Color: "#07c160", Fields: []PlatformField{
		{Key: "token", Label: "Token", Type: "password"},
	}},
	{Type: "misskey", Name: "Misskey", Icon: "🟩", Color: "#86b300", Fields: []PlatformField{
		{Key: "token", Label: "Access Token", Type: "password"},
		{Key: "instanceUrl", Label: "Instance URL", Type: "text", Help: "Misskey instance URL (e.g. https://misskey.io)", Placeholder: "https://misskey.io"},
	}},
}

// handleBotToolCatalog 返回工具目录和平台类型。
// GET /api/bots/platforms/tool-catalog
func (s *Server) handleBotToolCatalog(c *gin.Context) {
	OK(c, gin.H{"catalog": botToolCatalog, "types": platformTypes})
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

// handleGetBotAccess 获取 Bot 访问控制配置。
// GET /api/bots/:id/access
func (s *Server) handleGetBotAccess(c *gin.Context) {
	botID := c.Param("id")
	cfg := s.getBotAccessConfig(botID)
	OK(c, cfg)
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

	if err := s.saveBotAccessConfig(c, botID, &req); err != nil {
		Fail(c, err)
		return
	}
	OK(c, nil)
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
func (s *Server) handleBotFileUpload(c *gin.Context) {
	botID := c.Param("id")

	var req struct {
		Path string `json:"path" binding:"required"`
		Name string `json:"name" binding:"required"`
		Size int64  `json:"size"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		Fail(c, errs.BadRequest("invalid request body: "+err.Error()))
		return
	}

	if err := s.botFileUpload(c, botID, req.Path, req.Name, req.Size); err != nil {
		Fail(c, err)
		return
	}
	OK(c, gin.H{"ok": true})
}

// --- 聊天节奏 (Chat Rhythm) ---

// BotChatRhythm 聊天节奏配置。
type BotChatRhythm struct {
	Enabled       bool            `json:"enabled"`
	Debounce      RhythmDebounce  `json:"debounce"`
	Timing        RhythmTiming    `json:"timing"`
	SpeakTendency float64         `json:"speakTendency"`
	Interrupt     RhythmInterrupt `json:"interrupt"`
	IdleComp      RhythmIdleComp  `json:"idleComp"`
}

// RhythmDebounce 防抖配置。
type RhythmDebounce struct {
	QuietWait int `json:"quietWait"`
	MaxWait   int `json:"maxWait"`
}

// RhythmTiming 打字节奏配置。
type RhythmTiming struct {
	Enabled bool `json:"enabled"`
}

// RhythmInterrupt 打断控制配置。
type RhythmInterrupt struct {
	Enabled        bool `json:"enabled"`
	MaxConsecutive int  `json:"maxConsecutive"`
	MaxRounds      int  `json:"maxRounds"`
}

// RhythmIdleComp 空闲补偿配置。
type RhythmIdleComp struct {
	Enabled    bool `json:"enabled"`
	IdleWindow int  `json:"idleWindow"`
	MinIdle    int  `json:"minIdle"`
}

// handleGetBotRhythm 获取 Bot 聊天节奏配置。
// GET /api/bots/:id/chat-rhythm
func (s *Server) handleGetBotRhythm(c *gin.Context) {
	botID := c.Param("id")
	cfg := s.getBotRhythmConfig(botID)
	OK(c, cfg)
}

// handleUpdateBotRhythm 更新 Bot 聊天节奏配置。
// PUT /api/bots/:id/chat-rhythm
func (s *Server) handleUpdateBotRhythm(c *gin.Context) {
	botID := c.Param("id")

	var req BotChatRhythm
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

func (s *Server) getBotAccessConfig(botID string) *BotAccessConfig {
	raw, ok := s.store.Get(botDetailKey(botID, "access"))
	if !ok || raw == "" {
		return &BotAccessConfig{Default: "allow", Rules: []BotAccessRule{}}
	}
	var cfg BotAccessConfig
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		return &BotAccessConfig{Default: "allow", Rules: []BotAccessRule{}}
	}
	if cfg.Rules == nil {
		cfg.Rules = []BotAccessRule{}
	}
	return &cfg
}

func (s *Server) saveBotAccessConfig(c *gin.Context, botID string, cfg *BotAccessConfig) error {
	data, _ := json.Marshal(cfg)
	return s.store.Set(c.Request.Context(), botDetailKey(botID, "access"), string(data))
}

func (s *Server) getBotFileEntries(botID, path string) []BotFileEntry {
	key := botDetailKey(botID, "files."+path)
	raw, ok := s.store.Get(key)
	if !ok || raw == "" {
		// 返回默认的根目录结构
		if path == "/" {
			return []BotFileEntry{{Name: "data", Type: "dir", Size: 0, Mtime: nowRFC3339()}}
		}
		return []BotFileEntry{}
	}
	var result []BotFileEntry
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		return []BotFileEntry{}
	}
	return result
}

func (s *Server) botFileMkdir(c *gin.Context, botID, path, name string) error {
	key := botDetailKey(botID, "files."+path)
	entries := s.getBotFileEntries(botID, path)

	// 检查是否已存在
	for _, e := range entries {
		if e.Name == name {
			return errs.Conflict("directory already exists")
		}
	}

	entries = append(entries, BotFileEntry{Name: name, Type: "dir", Size: 0, Mtime: nowRFC3339()})
	data, _ := json.Marshal(entries)
	return s.store.Set(c.Request.Context(), key, string(data))
}

func (s *Server) botFileUpload(c *gin.Context, botID, path, name string, size int64) error {
	key := botDetailKey(botID, "files."+path)
	entries := s.getBotFileEntries(botID, path)

	// 覆盖或新增
	found := false
	for i, e := range entries {
		if e.Name == name {
			entries[i] = BotFileEntry{Name: name, Type: "file", Size: size, Mtime: nowRFC3339()}
			found = true
			break
		}
	}
	if !found {
		entries = append(entries, BotFileEntry{Name: name, Type: "file", Size: size, Mtime: nowRFC3339()})
	}

	data, _ := json.Marshal(entries)
	return s.store.Set(c.Request.Context(), key, string(data))
}

func (s *Server) getBotRhythmConfig(botID string) *BotChatRhythm {
	raw, ok := s.store.Get(botDetailKey(botID, "chat_rhythm"))
	if !ok || raw == "" {
		return &BotChatRhythm{
			Enabled:       true,
			Debounce:      RhythmDebounce{QuietWait: 2, MaxWait: 15},
			Timing:        RhythmTiming{Enabled: true},
			SpeakTendency: 0.7,
			Interrupt:     RhythmInterrupt{Enabled: true, MaxConsecutive: 3, MaxRounds: 6},
			IdleComp:      RhythmIdleComp{Enabled: true, IdleWindow: 60, MinIdle: 5},
		}
	}
	var cfg BotChatRhythm
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		return &BotChatRhythm{Enabled: true, SpeakTendency: 0.7}
	}
	return &cfg
}

func (s *Server) saveBotRhythmConfig(c *gin.Context, botID string, cfg *BotChatRhythm) error {
	data, _ := json.Marshal(cfg)
	return s.store.Set(c.Request.Context(), botDetailKey(botID, "chat_rhythm"), string(data))
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

// nowRFC3339 返回当前时间的 RFC3339 格式字符串。
func nowRFC3339() string {
	return time.Now().UTC().Format("2006-01-02T15:04:05Z")
}
