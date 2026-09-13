package api

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/gin-gonic/gin"

	"github.com/kasuganosora/thinkbot/config"
	"github.com/kasuganosora/thinkbot/skill"
	"github.com/kasuganosora/thinkbot/util/errs"
	"github.com/kasuganosora/thinkbot/util/idgen"
)

// ============================================================================
// Bot 级技能管理 Handler
//
// 运行时加载两处来源（后者同名覆盖前者）：
//   1. 内置 bundled：仓库 skills/（只读，可启用/禁用）
//   2. 托管 managed：{data}/skills/{botId}/（CRUD）
//
// 启用状态键：bot.{botId}.skill.{name}.enabled，回退全局 skill.{name}.enabled。
// Bot 在跑时 CRUD / 开关会热更新该实例的 SkillManager。
// ============================================================================

// botSkillEntry 是返回给前端的技能实体。
type botSkillEntry struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	Description   string `json:"description"`
	Content       string `json:"content"`
	Source        string `json:"source"` // bundled | managed
	Status        string `json:"status"` // enabled | disabled
	Enabled       bool   `json:"enabled"`
	Editable      bool   `json:"editable"`
	Path          string `json:"path"`
	HasScripts    bool   `json:"hasScripts"`
	HasReferences bool   `json:"hasReferences"`
	HasAssets     bool   `json:"hasAssets"`
	CreatedAt     string `json:"createdAt"`
	UpdatedAt     string `json:"updatedAt"`
}

func (s *Server) bundledSkillsDir() string {
	if s != nil && s.bundledSkillsDirOverride != "" {
		return s.bundledSkillsDirOverride
	}
	return "skills"
}

func (s *Server) managedSkillsRoot() string {
	if s != nil && s.botSvc != nil {
		ws := s.botSvc.GetWorkspaceBaseDir()
		parent := filepath.Dir(ws)
		if parent != "" && parent != "." {
			return filepath.Join(parent, "skills")
		}
	}
	return filepath.Join("data", "skills")
}

func (s *Server) botSkillsDir(botID string) string {
	return filepath.Join(s.managedSkillsRoot(), botID)
}

// handleListBotSkills 列出指定 Bot 将使用的技能（内置 ∪ 托管，托管同名覆盖）。
func (s *Server) handleListBotSkills(c *gin.Context) {
	botID := c.Param("id")
	skills := s.collectBotSkills(botID)
	OK(c, gin.H{
		"skills": skills,
		"roots": gin.H{
			"bundled": s.bundledSkillsDir(),
			"managed": s.botSkillsDir(botID),
		},
	})
}

func (s *Server) collectBotSkills(botID string) []botSkillEntry {
	byName := map[string]botSkillEntry{}
	for _, sk := range s.scanSkillDir(s.bundledSkillsDir(), "bundled") {
		byName[sk.Name] = sk
	}
	for _, sk := range s.scanSkillDir(s.botSkillsDir(botID), "managed") {
		byName[sk.Name] = sk
	}

	if mgr, ok := s.runningSkillMgr(botID); ok {
		for name, sk := range byName {
			if info, found := mgr.GetInfo(name); found {
				sk.Enabled = info.Enabled
				sk.Status = skillStatus(info.Enabled)
				sk.HasScripts = info.HasScripts
				sk.HasReferences = info.HasReferences
				sk.HasAssets = info.HasAssets
				byName[name] = sk
			}
		}
	} else {
		for name, sk := range byName {
			sk.Enabled = s.skillEnabledFromStore(botID, name, sk.Enabled)
			sk.Status = skillStatus(sk.Enabled)
			byName[name] = sk
		}
	}

	out := make([]botSkillEntry, 0, len(byName))
	for _, sk := range byName {
		out = append(out, sk)
	}
	return out
}

func skillStatus(enabled bool) string {
	if enabled {
		return "enabled"
	}
	return "disabled"
}

func (s *Server) runningSkillMgr(botID string) (*skill.SkillManager, bool) {
	if s.botSvc == nil {
		return nil, false
	}
	return s.botSvc.RunningSkillManager(botID)
}

func (s *Server) skillEnabledFromStore(botID, name string, fallback bool) bool {
	if s.store == nil {
		return fallback
	}
	if val, ok := s.store.Get(config.BotSkillEnabledKey(botID, name)); ok {
		return val == "true"
	}
	if val, ok := s.store.Get("skill." + name + ".enabled"); ok {
		return val == "true"
	}
	return fallback
}

func (s *Server) scanSkillDir(root, source string) []botSkillEntry {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	var skills []botSkillEntry
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		skillDir := filepath.Join(root, entry.Name())
		sk, err := loadBotSkillEntry(skillDir, source)
		if err != nil {
			continue
		}
		skills = append(skills, *sk)
	}
	return skills
}

// handleGetBotSkill 获取单个 Skill 详情。
func (s *Server) handleGetBotSkill(c *gin.Context) {
	botID := c.Param("id")
	sid := c.Param("sid")
	sk, err := s.findBotSkill(botID, sid)
	if err != nil {
		Fail(c, errs.NotFound("skill not found"))
		return
	}
	OK(c, sk)
}

// handleCreateBotSkill 创建一个新 Skill（写入托管目录）。
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
	name = sanitizeSkillName(name)
	if name == "" {
		name = fmt.Sprintf("skill-%d", time.Now().UnixMilli())
	}

	dir := s.botSkillsDir(botID)
	skillDir := filepath.Join(dir, name)
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		Fail(c, errs.Wrap(err, "create skill dir"))
		return
	}

	skillPath := filepath.Join(skillDir, "SKILL.md")
	if err := os.WriteFile(skillPath, []byte(req.Content), 0o644); err != nil {
		Fail(c, errs.Wrap(err, "write SKILL.md"))
		return
	}

	id := idgen.New("skill")
	now := time.Now().UTC().Format(time.RFC3339)
	metaContent := fmt.Sprintf("id=%s\ncreatedAt=%s\nupdatedAt=%s\n", id, now, now)
	_ = os.WriteFile(filepath.Join(skillDir, ".meta"), []byte(metaContent), 0o644)

	sk := botSkillEntry{
		ID:          id,
		Name:        name,
		Description: description,
		Content:     req.Content,
		Source:      "managed",
		Status:      "enabled",
		Enabled:     true,
		Editable:    true,
		Path:        skillPath,
		CreatedAt:   now,
		UpdatedAt:   now,
	}

	if err := s.reloadManagedSkill(botID, skillDir); err != nil {
		s.warnSkill("reload skill after create failed", botID, name, err)
	}

	auditLog(c, s.logger, "create_bot_skill", "bot_id", botID, "skill", name)
	OK(c, sk)
}

// handleUpdateBotSkill 更新托管 Skill 内容。内置技能不可改。
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

	sk, err := s.findBotSkill(botID, sid)
	if err != nil {
		Fail(c, errs.NotFound("skill not found"))
		return
	}
	if sk.Source != "managed" {
		Fail(c, errs.BadRequest("bundled skills are read-only; clone into managed to edit"))
		return
	}

	newName, newDesc := parseSkillFrontMatter(req.Content)
	newName = sanitizeSkillName(newName)
	if newName == "" {
		newName = sk.Name
	}

	dir := s.botSkillsDir(botID)
	oldDir := filepath.Join(dir, sk.Name)
	newDir := filepath.Join(dir, newName)
	if sk.Name != newName {
		if err := os.Rename(oldDir, newDir); err != nil {
			Fail(c, errs.Wrap(err, "rename skill dir"))
			return
		}
		if mgr, ok := s.runningSkillMgr(botID); ok {
			mgr.Unregister(sk.Name)
		}
	}

	skillPath := filepath.Join(newDir, "SKILL.md")
	if err := os.WriteFile(skillPath, []byte(req.Content), 0o644); err != nil {
		Fail(c, errs.Wrap(err, "write SKILL.md"))
		return
	}

	now := time.Now().UTC().Format(time.RFC3339)
	metaPath := filepath.Join(newDir, ".meta")
	metaContent := fmt.Sprintf("id=%s\ncreatedAt=%s\nupdatedAt=%s\n", sk.ID, sk.CreatedAt, now)
	_ = os.WriteFile(metaPath, []byte(metaContent), 0o644)

	sk.Name = newName
	sk.Description = newDesc
	sk.Content = req.Content
	sk.Path = skillPath
	sk.UpdatedAt = now

	if err := s.reloadManagedSkill(botID, newDir); err != nil {
		s.warnSkill("reload skill after update failed", botID, newName, err)
	}

	auditLog(c, s.logger, "update_bot_skill", "bot_id", botID, "skill", newName)
	OK(c, sk)
}

// handleRemoveBotSkill 删除托管 Skill。内置技能不可删。
func (s *Server) handleRemoveBotSkill(c *gin.Context) {
	botID := c.Param("id")
	sid := c.Param("sid")

	sk, err := s.findBotSkill(botID, sid)
	if err != nil {
		Fail(c, errs.NotFound("skill not found"))
		return
	}
	if sk.Source != "managed" {
		Fail(c, errs.BadRequest("bundled skills cannot be deleted"))
		return
	}

	skillDir := filepath.Join(s.botSkillsDir(botID), sk.Name)
	if err := os.RemoveAll(skillDir); err != nil {
		Fail(c, errs.Wrap(err, "remove skill dir"))
		return
	}
	if mgr, ok := s.runningSkillMgr(botID); ok {
		mgr.Unregister(sk.Name)
	}

	auditLog(c, s.logger, "remove_bot_skill", "bot_id", botID, "skill", sk.Name)
	OK(c, nil)
}

// handleEnableBotSkill 启用技能（per-bot）。
func (s *Server) handleEnableBotSkill(c *gin.Context) {
	s.setBotSkillEnabled(c, true)
}

// handleDisableBotSkill 禁用技能（per-bot）。
func (s *Server) handleDisableBotSkill(c *gin.Context) {
	s.setBotSkillEnabled(c, false)
}

func (s *Server) setBotSkillEnabled(c *gin.Context, enabled bool) {
	botID := c.Param("id")
	sid := c.Param("sid")
	sk, err := s.findBotSkill(botID, sid)
	if err != nil {
		Fail(c, errs.NotFound("skill not found"))
		return
	}

	if mgr, ok := s.runningSkillMgr(botID); ok {
		var opErr error
		if enabled {
			opErr = mgr.Enable(sk.Name)
		} else {
			opErr = mgr.Disable(sk.Name)
		}
		if opErr != nil {
			Fail(c, errs.Wrap(opErr, "set skill enabled"))
			return
		}
	} else if s.store != nil {
		val := "false"
		if enabled {
			val = "true"
		}
		if err := s.store.Set(c.Request.Context(), config.BotSkillEnabledKey(botID, sk.Name), val); err != nil {
			Fail(c, errs.Wrap(err, "persist skill enabled"))
			return
		}
	}

	sk.Enabled = enabled
	sk.Status = skillStatus(enabled)
	action := "disable_bot_skill"
	if enabled {
		action = "enable_bot_skill"
	}
	auditLog(c, s.logger, action, "bot_id", botID, "skill", sk.Name)
	OK(c, sk)
}

func (s *Server) warnSkill(msg, botID, name string, err error) {
	if s != nil && s.logger != nil {
		s.logger.Warnw(msg, "bot", botID, "skill", name, "err", err)
	}
}

func (s *Server) reloadManagedSkill(botID, skillDir string) error {
	mgr, ok := s.runningSkillMgr(botID)
	if !ok {
		return nil
	}
	loader := skill.NewLoader(filepath.Dir(skillDir), s.logger)
	loader.Source = "managed"
	sk, err := loader.LoadSkill(skillDir)
	if err != nil {
		return err
	}
	mgr.Register(sk)
	return nil
}

func (s *Server) findBotSkill(botID, sid string) (*botSkillEntry, error) {
	for _, sk := range s.collectBotSkills(botID) {
		if sk.ID == sid || sk.Name == sid {
			cp := sk
			return &cp, nil
		}
	}
	return nil, fmt.Errorf("not found")
}

// ============================================================================
// 辅助函数
// ============================================================================

var reSkillFM = regexp.MustCompile(`(?m)^\s*(name|description)\s*:\s*(.*)$`)

func parseSkillFrontMatter(content string) (name, description string) {
	if !strings.HasPrefix(content, "---") {
		return "", ""
	}
	end := strings.Index(content[3:], "---")
	if end < 0 {
		return "", ""
	}
	fm := content[:end+6]

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

func sanitizeSkillName(name string) string {
	name = strings.TrimSpace(strings.ToLower(name))
	var b strings.Builder
	for _, r := range name {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '_':
			b.WriteRune(r)
		case r == ' ' || r == '.':
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-_")
	if len(out) > 80 {
		out = out[:80]
	}
	return out
}

func loadBotSkillEntry(skillDir, source string) (*botSkillEntry, error) {
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
		id = name
	}

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

	loader := skill.NewLoader(filepath.Dir(skillDir), nil)
	loader.Source = source
	parsed, _ := loader.LoadSkill(skillDir)
	enabled := true
	hasScripts, hasRefs, hasAssets := false, false, false
	if parsed != nil {
		enabled = parsed.Enabled
		hasScripts = len(parsed.Resources.Scripts) > 0
		hasRefs = len(parsed.Resources.References) > 0
		hasAssets = len(parsed.Resources.Assets) > 0
		if parsed.Name != "" {
			name = parsed.Name
		}
		if parsed.Description != "" {
			description = parsed.Description
		}
	}

	return &botSkillEntry{
		ID:            id,
		Name:          name,
		Description:   description,
		Content:       content,
		Source:        source,
		Status:        skillStatus(enabled),
		Enabled:       enabled,
		Editable:      source == "managed",
		Path:          skillPath,
		HasScripts:    hasScripts,
		HasReferences: hasRefs,
		HasAssets:     hasAssets,
		CreatedAt:     createdAt,
		UpdatedAt:     updatedAt,
	}, nil
}
