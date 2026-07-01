package api

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/kasuganosora/thinkbot/util/errs"
	"github.com/kasuganosora/thinkbot/util/idgen"
)

// ============================================================================
// Bot 级技能管理 Handler — 每个 Bot 独立的 Skill CRUD
//
// Skill 以 SKILL.md 文件存储在 data/skills/{botId}/{skillName}/SKILL.md。
// Content 即完整的 SKILL.md 文本（含 YAML front matter）。
// 前端通过 content 字段创建/更新，后端从 front matter 解析 name、description。
//
// 路由：
//   GET    /api/bots/:id/skills          → 列表
//   GET    /api/bots/:id/skills/:sid     → 详情
//   POST   /api/bots/:id/skills          → 新增
//   PUT    /api/bots/:id/skills/:sid     → 更新
//   DELETE /api/bots/:id/skills/:sid     → 删除
// ============================================================================

// botSkillEntry 是返回给前端的技能实体。
type botSkillEntry struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Content     string `json:"content"`
	Source      string `json:"source"`
	Status      string `json:"status"`
	Path        string `json:"path"`
	CreatedAt   string `json:"createdAt"`
	UpdatedAt   string `json:"updatedAt"`
}

// botSkillsDir 返回 Bot 的 skills 根目录。
func botSkillsDir(botID string) string {
	return filepath.Join("data", "skills", botID)
}

// handleListBotSkills 列出指定 Bot 的所有 Skill。
func (s *Server) handleListBotSkills(c *gin.Context) {
	botID := c.Param("id")
	dir := botSkillsDir(botID)

	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			OK(c, gin.H{"skills": []botSkillEntry{}})
			return
		}
		Fail(c, errs.Wrap(err, "read skills dir"))
		return
	}

	var skills []botSkillEntry
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		skillDir := filepath.Join(dir, entry.Name())
		sk, err := loadBotSkillEntry(skillDir)
		if err != nil {
			continue
		}
		skills = append(skills, *sk)
	}

	OK(c, gin.H{"skills": skills})
}

// handleGetBotSkill 获取单个 Skill 详情。
func (s *Server) handleGetBotSkill(c *gin.Context) {
	botID := c.Param("id")
	sid := c.Param("sid")
	dir := botSkillsDir(botID)

	// sid 可能是 skill name 也可能是 id，遍历查找
	sk, err := findBotSkillByID(dir, sid)
	if err != nil {
		Fail(c, errs.NotFound("skill not found"))
		return
	}
	OK(c, sk)
}

// handleCreateBotSkill 创建一个新 Skill。
func (s *Server) handleCreateBotSkill(c *gin.Context) {
	botID := c.Param("id")
	var req struct {
		Content string `json:"content"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		Fail(c, errs.BadRequest("invalid request body"))
		return
	}
	if req.Content == "" {
		Fail(c, errs.BadRequest("content is required"))
		return
	}

	name, description := parseSkillFrontMatter(req.Content)
	if name == "" {
		name = fmt.Sprintf("skill-%d", time.Now().UnixMilli())
	}

	dir := botSkillsDir(botID)
	skillDir := filepath.Join(dir, name)

	// 确保目录存在
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		Fail(c, errs.Wrap(err, "create skill dir"))
		return
	}

	// 写入 SKILL.md
	skillPath := filepath.Join(skillDir, "SKILL.md")
	if err := os.WriteFile(skillPath, []byte(req.Content), 0o644); err != nil {
		Fail(c, errs.Wrap(err, "write SKILL.md"))
		return
	}

	// 写入元数据文件（存储 id、createdAt）
	id := idgen.New("skill")
	now := time.Now().UTC().Format(time.RFC3339)
	metaContent := fmt.Sprintf("id=%s\ncreatedAt=%s\nupdatedAt=%s\n", id, now, now)
	metaPath := filepath.Join(skillDir, ".meta")
	_ = os.WriteFile(metaPath, []byte(metaContent), 0o644)

	sk := botSkillEntry{
		ID:          id,
		Name:        name,
		Description: description,
		Content:     req.Content,
		Source:      "managed",
		Status:      "active",
		Path:        fmt.Sprintf("/data/skills/%s/%s/SKILL.md", botID, name),
		CreatedAt:   now,
		UpdatedAt:   now,
	}

	auditLog(c, s.logger, "create_bot_skill", "bot_id", botID, "skill", name)
	OK(c, sk)
}

// handleUpdateBotSkill 更新 Skill 内容。
func (s *Server) handleUpdateBotSkill(c *gin.Context) {
	botID := c.Param("id")
	sid := c.Param("sid")
	var req struct {
		Content string `json:"content"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		Fail(c, errs.BadRequest("invalid request body"))
		return
	}
	if req.Content == "" {
		Fail(c, errs.BadRequest("content is required"))
		return
	}

	dir := botSkillsDir(botID)
	sk, err := findBotSkillByID(dir, sid)
	if err != nil {
		Fail(c, errs.NotFound("skill not found"))
		return
	}

	// 解析新内容的 name
	newName, newDesc := parseSkillFrontMatter(req.Content)
	if newName == "" {
		newName = sk.Name
	}

	// 如果 name 变了，需要重命名目录
	oldDir := filepath.Join(dir, sk.Name)
	newDir := filepath.Join(dir, newName)
	if sk.Name != newName {
		if err := os.Rename(oldDir, newDir); err != nil {
			Fail(c, errs.Wrap(err, "rename skill dir"))
			return
		}
	}

	// 写入新内容
	skillPath := filepath.Join(newDir, "SKILL.md")
	if err := os.WriteFile(skillPath, []byte(req.Content), 0o644); err != nil {
		Fail(c, errs.Wrap(err, "write SKILL.md"))
		return
	}

	// 更新元数据
	now := time.Now().UTC().Format(time.RFC3339)
	metaPath := filepath.Join(newDir, ".meta")
	metaContent := fmt.Sprintf("id=%s\ncreatedAt=%s\nupdatedAt=%s\n", sk.ID, sk.CreatedAt, now)
	_ = os.WriteFile(metaPath, []byte(metaContent), 0o644)

	sk.Name = newName
	sk.Description = newDesc
	sk.Content = req.Content
	sk.Path = fmt.Sprintf("/data/skills/%s/%s/SKILL.md", botID, newName)
	sk.UpdatedAt = now

	auditLog(c, s.logger, "update_bot_skill", "bot_id", botID, "skill", newName)
	OK(c, sk)
}

// handleRemoveBotSkill 删除 Skill。
func (s *Server) handleRemoveBotSkill(c *gin.Context) {
	botID := c.Param("id")
	sid := c.Param("sid")
	dir := botSkillsDir(botID)

	sk, err := findBotSkillByID(dir, sid)
	if err != nil {
		Fail(c, errs.NotFound("skill not found"))
		return
	}

	skillDir := filepath.Join(dir, sk.Name)
	if err := os.RemoveAll(skillDir); err != nil {
		Fail(c, errs.Wrap(err, "remove skill dir"))
		return
	}

	auditLog(c, s.logger, "remove_bot_skill", "bot_id", botID, "skill", sk.Name)
	OK(c, nil)
}

// ============================================================================
// 辅助函数
// ============================================================================

var reSkillFM = regexp.MustCompile(`(?m)^\s*(name|description)\s*:\s*(.*)$`)

// parseSkillFrontMatter 简单解析 SKILL.md 的 front matter 中 name 和 description。
func parseSkillFrontMatter(content string) (name, description string) {
	// 找 front matter 区域
	if !strings.HasPrefix(content, "---") {
		return "", ""
	}
	end := strings.Index(content[3:], "---")
	if end < 0 {
		return "", ""
	}
	fm := content[:end+6] // 包含开头和结尾的 ---

	matches := reSkillFM.FindAllStringSubmatch(fm, -1)
	for _, m := range matches {
		key := strings.TrimSpace(m[1])
		val := strings.TrimSpace(m[2])
		switch key {
		case "name":
			name = val
		case "description":
			description = val
		}
	}
	return
}

// loadBotSkillEntry 从目录加载一个 skill 条目。
func loadBotSkillEntry(skillDir string) (*botSkillEntry, error) {
	skillPath := filepath.Join(skillDir, "SKILL.md")
	data, err := os.ReadFile(skillPath)
	if err != nil {
		return nil, err
	}

	content := string(data)
	name, description := parseSkillFrontMatter(content)
	if name == "" {
		name = filepath.Base(skillDir)
	}

	// 读取元数据
	id := ""
	createdAt := ""
	updatedAt := ""
	metaPath := filepath.Join(skillDir, ".meta")
	if metaData, err := os.ReadFile(metaPath); err == nil {
		for _, line := range strings.Split(string(metaData), "\n") {
			if strings.HasPrefix(line, "id=") {
				id = strings.TrimPrefix(line, "id=")
			} else if strings.HasPrefix(line, "createdAt=") {
				createdAt = strings.TrimPrefix(line, "createdAt=")
			} else if strings.HasPrefix(line, "updatedAt=") {
				updatedAt = strings.TrimPrefix(line, "updatedAt=")
			}
		}
	}
	if id == "" {
		id = idgen.New("skill")
	}

	// 如果没有元数据文件，用文件修改时间
	if createdAt == "" || updatedAt == "" {
		info, _ := os.Stat(skillPath)
		if info != nil {
			t := info.ModTime().UTC().Format(time.RFC3339)
			if createdAt == "" {
				createdAt = t
			}
			if updatedAt == "" {
				updatedAt = t
			}
		}
	}

	return &botSkillEntry{
		ID:          id,
		Name:        name,
		Description: description,
		Content:     content,
		Source:      "managed",
		Status:      "active",
		Path:        skillPath,
		CreatedAt:   createdAt,
		UpdatedAt:   updatedAt,
	}, nil
}

// findBotSkillByID 在目录中根据 ID 或名称查找 skill。
func findBotSkillByID(dir, sid string) (*botSkillEntry, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		skillDir := filepath.Join(dir, entry.Name())
		sk, err := loadBotSkillEntry(skillDir)
		if err != nil {
			continue
		}
		if sk.ID == sid || sk.Name == sid {
			return sk, nil
		}
	}
	return nil, fmt.Errorf("not found")
}
