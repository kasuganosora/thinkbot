// Package toolperm 提供 bot 维度的工具权限管理。
//
// 数据模型对应 DAO 表 bot_tool_permissions，每条规则由
// (bot_id, tool, platform, user_ids, decision, enabled, sort) 描述。
//
// 核心语义（与前端「工具权限」配置页一致）：
//   - 仅 enabled=true 的规则参与评估；
//   - 按 sort 升序遍历，第一个同时匹配 tool/platform/user 的规则决定最终决策
//     （管理员的显式规则永远优先，包括对基础工具的 deny）；
//   - 一个平台（渠道）若「完全没有任何启用规则覆盖它」→ 保守默认：**基础工具放行、
//     敏感工具禁止**（风险分级，见 risk.go）。这是 5142 修复后的默认行为——
//     新建 Bot 不应天然处于最宽权限，含代码执行/沙箱命令的敏感能力必须显式放开；
//     web 即此情形，但因 SeedWebDefault 显式播种了一条 tool=* platform=web 的
//     allow 基线规则（命中即全开），web 会话默认保持全开以保证人机对话开箱可用；
//   - 一个平台「已有规则」但没有任何规则命中当前 (tool,user) → **按工具风险区分**：
//     基础工具（计算、文本处理、记忆、只读状态查询，见 risk.go）默认放行；
//     敏感工具（联网、执行命令、读写文件、派生子智能体）默认禁止。
//     这样管理员只需为想限制的危险工具配规则，不必为每个无害工具补 allow；
//   - 系统/内部会话（cron、心跳、梦境巩固等，sctx.IsSystem=true）不受本模块约束，直接放行全部工具；
//   - 双重防线：① ResolveTools 过滤工具列表（LLM 看不到未授权工具）；
//     ② 每个工具被执行时（call-time）用同一会话上下文再复核一次权限，防止列表过滤被绕过。
//
// 通配约定：
//   - tool / platform 为 "*" 或空 → 匹配所有；
//   - tool 支持 * 通配（如 "sandbox_*"）；
//   - user_ids 含 "*" → 匹配所有用户；其余按精确（或 * 通配）匹配。
package toolperm

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
	"gorm.io/gorm"

	"github.com/kasuganosora/thinkbot/agent/tools"
	"github.com/kasuganosora/thinkbot/dao"
	"github.com/kasuganosora/thinkbot/llm"
	"github.com/kasuganosora/thinkbot/util/idgen"
)

// 决策常量。
const (
	DecisionAllow = "allow"
	DecisionDeny  = "deny"
)

// RuleDTO 是 API 层使用的规则视图（user_ids 展开为数组）。
type RuleDTO struct {
	ID        string    `json:"id"`
	BotID     string    `json:"botId"`
	Tool      string    `json:"tool"`
	Platform  string    `json:"platform"`
	UserIDs   []string  `json:"userIds"`
	Decision  string    `json:"decision"`
	Enabled   bool      `json:"enabled"`
	Sort      int       `json:"sort"`
	Auto      bool      `json:"auto"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// RuleReq 是创建 / 更新规则的请求体。
//
// 创建（CreateRule）：缺失字段按默认值归一化（tool/platform="*"、userIds=["*"]、
// decision=allow、enabled=true、sort=末尾）。
//
// 更新（UpdateRule）：采用「部分更新」语义 —— 只有显式提供的字段才会被修改，
// 缺失字段保持库中原值。这一点很重要：早期版本对更新也走全量归一化，导致前端
// 只回传 {enabled:false} 时，一条 (sandbox_exec, telegram, deny) 规则会被静默
// 重置为 (*, *, allow) —— 即「关一个开关反而放开全部工具」的提权事故。
type RuleReq struct {
	// Tool 工具名模式（"*" = 全部）。更新时留空表示不修改。
	Tool string `json:"tool"`

	// Platform 平台类型（"*" = 全部）。更新时留空表示不修改。
	Platform string `json:"platform"`

	// UserIDs 用户 ID 列表；含 "*" 表示全部。更新时为空表示不修改。
	UserIDs []string `json:"userIds"`

	// Decision 决策：allow / deny。更新时留空表示不修改；创建时非法值归一化为 allow。
	Decision string `json:"decision"`

	// Enabled 是否启用；nil 时由服务端给默认值（创建=true，更新=保持原值）。
	Enabled *bool `json:"enabled"`

	// Sort 排序权重；nil 时由服务端给默认值（创建=追加到末尾，更新=保持原值）。
	Sort *int `json:"sort"`

	// Auto 标记该规则是否由「发言模式」便捷开关自动维护。
	// nil 时由服务端给默认值（创建=false，更新=保持原值）。
	Auto *bool `json:"auto"`
}

// Service 是 bot 工具权限服务。
type Service struct {
	db     *gorm.DB
	logger *zap.SugaredLogger

	mu    sync.RWMutex
	cache map[string]cacheEntry
	ttl   time.Duration
}

type cacheEntry struct {
	rules   []RuleDTO
	expires time.Time
}

// NewService 创建权限服务。
func NewService(db *gorm.DB, logger *zap.SugaredLogger) *Service {
	if logger == nil {
		logger = zap.NewNop().Sugar()
	}
	return &Service{
		db:     db,
		logger: logger.With("component", "tool_perm"),
		cache:  make(map[string]cacheEntry),
		ttl:    30 * time.Second,
	}
}

// ----------------------------------------------------------------------------
// 缓存
// ----------------------------------------------------------------------------

func (s *Service) getCache(botID string) ([]RuleDTO, bool) {
	s.mu.RLock()
	e, ok := s.cache[botID]
	s.mu.RUnlock()
	if !ok || time.Now().After(e.expires) {
		return nil, false
	}
	return e.rules, true
}

func (s *Service) setCache(botID string, rules []RuleDTO) {
	s.mu.Lock()
	s.cache[botID] = cacheEntry{rules: rules, expires: time.Now().Add(s.ttl)}
	s.mu.Unlock()
}

func (s *Service) invalidate(botID string) {
	s.mu.Lock()
	delete(s.cache, botID)
	s.mu.Unlock()
}

// ----------------------------------------------------------------------------
// 读路径
// ----------------------------------------------------------------------------

// ListRules 返回某 bot 的全部规则（含禁用），按 sort 升序。结果缓存 30s。
// 若 bot 尚无任何覆盖 web 的规则，惰性播种 web 基线规则后再返回
// （与 bot 是否有其他平台规则无关，保证 web 始终是可见的开放基线）。
func (s *Service) ListRules(botID string) ([]RuleDTO, error) {
	if r, ok := s.getCache(botID); ok {
		return r, nil
	}
	rules, err := s.loadFromDB(botID)
	if err != nil {
		return nil, err
	}
	if !hasWebCoverage(rules) {
		if err := s.SeedWebDefault(botID); err != nil {
			s.logger.Warnw("seed web default failed", "bot", botID, "err", err)
		}
		rules, err = s.loadFromDB(botID)
		if err != nil {
			return nil, err
		}
	}
	sortRules(rules)
	s.setCache(botID, rules)
	return rules, nil
}

func (s *Service) loadFromDB(botID string) ([]RuleDTO, error) {
	var rows []dao.BotToolPermission
	if err := s.db.Where("bot_id = ?", botID).Order("sort ASC, created_at ASC, id ASC").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]RuleDTO, 0, len(rows))
	for i := range rows {
		out = append(out, toDTO(&rows[i]))
	}
	return out, nil
}

// evalRules 返回某 bot 参与评估的规则（仅启用），按 sort 升序。
func (s *Service) evalRules(botID string) ([]RuleDTO, error) {
	all, err := s.ListRules(botID) // 走缓存
	if err != nil {
		return nil, err
	}
	out := make([]RuleDTO, 0, len(all))
	for _, r := range all {
		if r.Enabled {
			out = append(out, r)
		}
	}
	sortRules(out)
	return out, nil
}

// Evaluate 判定某 bot 在给定工具/平台/用户下是否允许使用。
//
// 判定顺序：
//  1. 按 sort 升序遍历启用规则，首条同时匹配 (tool, platform, user) 的规则决定结果
//     —— 管理员的显式配置永远优先，包括对基础工具的 deny；
//  2. 无规则命中时，该平台完全没有启用规则 → **保守默认（风险分级，修复 5142）**：
//     基础工具与对外发言工具放行，敏感工具（联网/命令执行/文件写/派生子智能体）
//     默认禁止。即「含危害面的能力必须显式放开，而非天然可用」，但正常发帖与
//     基础表达能力不被锁死，新建 Bot 不会变成哑巴。web 因显式种子 allow 基线规则
//     （命中即全开）仍保持全开，保证人机对话开箱可用；
//  3. 无规则命中且该平台已有规则 → **白名单模式（收紧）**：仅基础工具放行，
//     对外发言与敏感工具均禁止（需显式 allow 才放开）。
//
// 第 3 步的白名单模式是关键：管理员一旦开始配置某平台，未命中的工具即按最严
// 默认处理，避免「配一条 deny 反而放开其它」的提权；基础工具仍不受牵连。
func (s *Service) Evaluate(botID, tool, platform, userID string) bool {
	rules, err := s.evalRules(botID)
	if err != nil {
		s.logger.Warnw("evaluate tool perm failed", "bot", botID, "err", err)
		return false // 评估失败时保守拒绝
	}
	// 首条匹配生效（管理员显式规则优先于任何默认值）
	for _, r := range rules {
		if !matchTool(r.Tool, tool) {
			continue
		}
		if !matchPlatform(r.Platform, platform) {
			continue
		}
		if !matchUser(r.UserIDs, userID) {
			continue
		}
		return r.Decision == DecisionAllow
	}
	// 无匹配规则：该平台完全没有启用规则 → 保守默认（修复 5142 的 fail-open）。
	// 基础工具与对外发言工具放行（Bot 正常运作所需），敏感工具（联网/命令/文件/
	// 子智能体）默认禁止 —— 含危害面的能力必须显式放开，而非天然可用。
	if !platformHasEnabledRule(rules, platform) {
		return ToolRisk(tool) != RiskSensitive
	}
	// 该平台已进入白名单模式（已有规则但无命中）：仅基础工具放行，
	// 对外发言与敏感工具均禁止 —— 管理员的配置意图是「收紧」，未显式放开即拒绝。
	return IsBasicTool(tool)
}

// ----------------------------------------------------------------------------
// 写路径
// ----------------------------------------------------------------------------

// CreateRule 创建规则。Sort 为 nil 时追加到末尾（当前最大 sort + 1）。
func (s *Service) CreateRule(botID string, req RuleReq) (*RuleDTO, error) {
	m, err := s.buildModel(botID, req, true)
	if err != nil {
		return nil, err
	}
	if err := s.db.Create(m).Error; err != nil {
		return nil, err
	}
	s.invalidate(botID)
	d := toDTO(m)
	return &d, nil
}

// UpdateRule 更新规则（仅同 bot 下的规则可改）。
func (s *Service) UpdateRule(botID, ruleID string, req RuleReq) (*RuleDTO, error) {
	var m dao.BotToolPermission
	if err := s.db.Where("id = ? AND bot_id = ?", ruleID, botID).First(&m).Error; err != nil {
		return nil, err // 未找到时为 gorm.ErrRecordNotFound
	}
	applyReq(&m, req)
	if err := s.db.Save(&m).Error; err != nil {
		return nil, err
	}
	s.invalidate(botID)
	d := toDTO(&m)
	return &d, nil
}

// DeleteRule 删除规则。
func (s *Service) DeleteRule(botID, ruleID string) error {
	res := s.db.Where("id = ? AND bot_id = ?", ruleID, botID).Delete(&dao.BotToolPermission{})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return gorm.ErrRecordNotFound
	}
	s.invalidate(botID)
	return nil
}

// DeleteAllForBot 删除某 bot 的全部工具权限规则（bot 删除时清理孤儿行）。
func (s *Service) DeleteAllForBot(botID string) error {
	if err := s.db.Where("bot_id = ?", botID).Delete(&dao.BotToolPermission{}).Error; err != nil {
		return err
	}
	s.invalidate(botID)
	return nil
}

// SeedWebDefault 惰性播种 web 默认全开规则。
// 仅当该 bot 没有任何「覆盖 web」的规则（platform=web 或 platform=*）时写入，
// 避免覆盖用户自定义的 web 规则，也确保仅有其他平台规则的 bot 仍有 web 基线可见。
func (s *Service) SeedWebDefault(botID string) error {
	var cnt int64
	if err := s.db.Model(&dao.BotToolPermission{}).
		Where("bot_id = ? AND (platform = ? OR platform = ?)", botID, "web", "*").
		Count(&cnt).Error; err != nil {
		return err
	}
	if cnt > 0 {
		return nil
	}
	m := &dao.BotToolPermission{
		ID:       idgen.New("tp"),
		BotID:    botID,
		Tool:     "*",
		Platform: "web",
		UserIDs:  `["*"]`,
		Decision: DecisionAllow,
		Enabled:  true,
		Sort:     0,
	}
	if err := s.db.Create(m).Error; err != nil {
		return err
	}
	s.invalidate(botID)
	return nil
}

// ResetDefaults 清空某 bot 的全部规则并重新播种 web 默认规则。
// 供前端「恢复默认」按钮调用。
func (s *Service) ResetDefaults(botID string) error {
	if err := s.db.Where("bot_id = ?", botID).Delete(&dao.BotToolPermission{}).Error; err != nil {
		return err
	}
	s.invalidate(botID)
	return s.SeedWebDefault(botID)
}

// ----------------------------------------------------------------------------
// 模型转换 / 归一化
// ----------------------------------------------------------------------------

func toDTO(m *dao.BotToolPermission) RuleDTO {
	var uids []string
	_ = json.Unmarshal([]byte(m.UserIDs), &uids)
	if uids == nil {
		uids = []string{}
	}
	return RuleDTO{
		ID:        m.ID,
		BotID:     m.BotID,
		Tool:      m.Tool,
		Platform:  m.Platform,
		UserIDs:   uids,
		Decision:  m.Decision,
		Enabled:   m.Enabled,
		Sort:      m.Sort,
		Auto:      m.Auto,
		CreatedAt: m.CreatedAt,
		UpdatedAt: m.UpdatedAt,
	}
}

// buildModel 将请求归一化为 DAO 模型。create=true 时 Sort 为 nil 则追加到末尾。
func (s *Service) buildModel(botID string, req RuleReq, create bool) (*dao.BotToolPermission, error) {
	m := &dao.BotToolPermission{BotID: botID}
	applyCommon(m, req)
	if create {
		m.ID = idgen.New("tp")
		if req.Sort != nil {
			m.Sort = *req.Sort
		} else {
			m.Sort = s.nextSort(botID)
		}
		if req.Enabled != nil {
			m.Enabled = *req.Enabled
		} else {
			m.Enabled = true
		}
		if req.Auto != nil {
			m.Auto = *req.Auto
		}
	}
	return m, nil
}

// applyCommon 将请求归一化后写入模型（创建场景：缺失字段取默认值）。
func applyCommon(m *dao.BotToolPermission, req RuleReq) {
	tool := strings.TrimSpace(req.Tool)
	if tool == "" {
		tool = "*"
	}
	m.Tool = tool

	platform := strings.TrimSpace(req.Platform)
	if platform == "" {
		platform = "*"
	}
	m.Platform = platform

	uids := req.UserIDs
	if len(uids) == 0 {
		uids = []string{"*"}
	}
	m.UserIDs = marshalUserIDs(uids)

	decision := strings.ToLower(strings.TrimSpace(req.Decision))
	if decision != DecisionAllow && decision != DecisionDeny {
		decision = DecisionAllow
	}
	m.Decision = decision
}

// marshalUserIDs 清洗并序列化用户 ID 列表；结果至少为 ["*"]。
func marshalUserIDs(uids []string) string {
	clean := make([]string, 0, len(uids))
	for _, u := range uids {
		u = strings.TrimSpace(u)
		if u != "" {
			clean = append(clean, u)
		}
	}
	if len(clean) == 0 {
		clean = []string{"*"}
	}
	if b, err := json.Marshal(clean); err == nil {
		return string(b)
	}
	return `["*"]`
}

// applyReq 将请求应用到已加载的模型（更新场景，部分更新语义）。
//
// 只有显式提供的字段才会被修改，缺失字段保留原值。绝不能改回全量归一化：
// 前端「启用」开关只回传 {enabled} 时，全量归一化会把该规则的
// tool/platform/userIds/decision 一并重置为 (*, *, ["*"], allow)，
// 等于把一条精准的 deny 规则变成放开全部工具的 allow 规则（提权）。
func applyReq(m *dao.BotToolPermission, req RuleReq) {
	if tool := strings.TrimSpace(req.Tool); tool != "" {
		m.Tool = tool
	}
	if platform := strings.TrimSpace(req.Platform); platform != "" {
		m.Platform = platform
	}
	if len(req.UserIDs) > 0 {
		m.UserIDs = marshalUserIDs(req.UserIDs)
	}
	if decision := strings.ToLower(strings.TrimSpace(req.Decision)); decision != "" {
		// 仅接受合法值，非法值忽略（保持原值），避免拼写错误静默改成 allow
		if decision == DecisionAllow || decision == DecisionDeny {
			m.Decision = decision
		}
	}
	if req.Sort != nil {
		m.Sort = *req.Sort
	}
	if req.Enabled != nil {
		m.Enabled = *req.Enabled
	}
	if req.Auto != nil {
		m.Auto = *req.Auto
	}
}

func (s *Service) nextSort(botID string) int {
	var max int
	if err := s.db.Model(&dao.BotToolPermission{}).
		Where("bot_id = ?", botID).Select("COALESCE(MAX(sort), -1)").Scan(&max).Error; err != nil {
		return 0
	}
	return max + 1
}

func sortRules(rules []RuleDTO) {
	sort.SliceStable(rules, func(i, j int) bool {
		if rules[i].Sort != rules[j].Sort {
			return rules[i].Sort < rules[j].Sort
		}
		return rules[i].ID < rules[j].ID
	})
}

// hasWebCoverage 判断规则集合中是否存在覆盖 web 平台的规则
// （platform=web 或 platform=*）。用于决定是否需要惰性播种 web 基线。
func hasWebCoverage(rules []RuleDTO) bool {
	for _, r := range rules {
		if r.Platform == "web" || r.Platform == "*" {
			return true
		}
	}
	return false
}

// platformHasEnabledRule 判断规则集中是否存在「覆盖该平台」的启用规则
// （platform 通配或精确匹配当前平台）。用于区分默认放行与默认禁止：
//   - 平台无规则 → 开放基线（放行）
//   - 平台有规则但均未命中 → 安全默认（禁止）
func platformHasEnabledRule(rules []RuleDTO, platform string) bool {
	for _, r := range rules {
		if r.Enabled && matchPlatform(r.Platform, platform) {
			return true
		}
	}
	return false
}

// ----------------------------------------------------------------------------
// 通配匹配
// ----------------------------------------------------------------------------

// matchTool 工具名匹配：pattern 为 "*" 或空匹配全部；否则按 * 通配。
func matchTool(pattern, name string) bool {
	if pattern == "" || pattern == "*" {
		return true
	}
	return matchGlob(pattern, name)
}

// matchPlatform 平台匹配：pattern 为 "*" 或空匹配全部；否则精确相等。
func matchPlatform(pattern, platform string) bool {
	if pattern == "" || pattern == "*" {
		return true
	}
	return pattern == platform
}

// matchUser 用户匹配：列表含 "*" 匹配全部；否则按精确（或 * 通配）匹配 userID。
func matchUser(userIDs []string, userID string) bool {
	if len(userIDs) == 0 {
		return false
	}
	for _, u := range userIDs {
		if u == "*" || u == "" {
			return true
		}
		if matchGlob(u, userID) {
			return true
		}
	}
	return false
}

// matchGlob 简易 glob：仅支持 * 作为多字符通配。
//   - 无 *：精确匹配
//   - "a*"：前缀；"*a"：后缀；"a*b"：前缀+后缀+中间按序出现
func matchGlob(pattern, name string) bool {
	if pattern == "*" || pattern == "" {
		return true
	}
	if !strings.Contains(pattern, "*") {
		return pattern == name
	}
	parts := strings.Split(pattern, "*")
	if len(parts) == 1 {
		return parts[0] == name
	}
	if parts[0] != "" && !strings.HasPrefix(name, parts[0]) {
		return false
	}
	last := parts[len(parts)-1]
	if last != "" && !strings.HasSuffix(name, last) {
		return false
	}
	remaining := name
	if parts[0] != "" {
		remaining = remaining[len(parts[0]):]
	}
	if last != "" {
		remaining = remaining[:len(remaining)-len(last)]
	}
	for _, p := range parts[1 : len(parts)-1] {
		if p == "" {
			continue
		}
		idx := strings.Index(remaining, p)
		if idx < 0 {
			return false
		}
		remaining = remaining[idx+len(p):]
	}
	return true
}

// ----------------------------------------------------------------------------
// ToolManager 评估器适配
// ----------------------------------------------------------------------------

// NewEvaluator 返回一个实现 agent/tools.ToolAccessEvaluator 的实例，
// 供 ToolManager.ResolveTools 按完整会话上下文过滤工具。
func (s *Service) NewEvaluator() tools.ToolAccessEvaluator {
	return &evaluator{svc: s}
}

// evaluator 是 ToolAccessEvaluator 的实现：根据 bot 权限表过滤工具列表。
type evaluator struct {
	svc *Service
}

// FilterTools 按 (botID, 工具名, 平台=SourceChannelType, userID) 过滤工具，
// 并对每个放行的工具包裹一层「调用时二次复核」的防御，确保即便工具列表过滤被绕过，
// 实际执行前仍会用同一会话上下文复核权限。
//
// 系统/内部会话（cron、心跳、梦境巩固等）放行**除对外发言工具以外**的全部工具：
// 这些会话没有真人在场审阅输出，若连发帖/转发/表态也一并豁免，等于给定时任务
// 开了一条无人监督的对外广播通道。因此 broadcast 类工具即便 IsSystem 也必须走
// 权限表评估 —— 想让 cron 定时发帖，就为该工具配一条显式 allow 规则。
func (e *evaluator) FilterTools(_ context.Context, toolList []llm.Tool, sctx *tools.ToolSessionContext) ([]llm.Tool, error) {
	if sctx == nil {
		// 无会话上下文时无法判定归属维度，保守放行（与「无规则 → 开放基线」一致）。
		// 注意：不可 panic —— ResolveTools 的调用方可能传入零值上下文。
		return toolList, nil
	}
	platform := sctx.SourceChannelType
	if platform == "" {
		platform = sctx.Channel
	}
	// 快照会话维度：sctx 是指针，调用方后续可能复用/改写它，
	// 而 call-time 复核发生在本函数返回之后，必须捕获值而非解引用指针。
	botID, userID, isSystem := sctx.BotID, sctx.UserID, sctx.IsSystem
	out := make([]llm.Tool, 0, len(toolList))
	for _, t := range toolList {
		if !e.allow(botID, t.Name, platform, userID, isSystem, sctx.IsSubagent) {
			continue
		}
		// 二次防御：调用时再用会话上下文复核权限，防止列表过滤被绕过
		// （例如工具经其它路径解析、缓存、或解析后规则发生变更）。
		toolName := t.Name
		orig := t.Execute
		wt := t
		wt.Execute = func(ctx *llm.ToolExecContext, input any) (any, error) {
			if !e.allow(botID, toolName, platform, userID, isSystem, sctx.IsSubagent) {
				return nil, fmt.Errorf("tool %q is not permitted for bot %q on platform %q", toolName, botID, platform)
			}
			if orig == nil {
				return nil, fmt.Errorf("tool %q has no execute handler", toolName)
			}
			return orig(ctx, input)
		}
		out = append(out, wt)
	}
	return out, nil
}

// allow 是单工具判定，统一处理两处豁免边界：
//
//  1. 系统会话（cron / 心跳 / 梦境巩固）豁免一切**除对外发言之外**的工具；
//     对外发言工具一律走权限表，防止无人监督的定时任务偷偷发帖。
//  2. 对外发言工具对子智能体（isSubagent）一律拒绝，与平台无关。
//     这是必要的兜底：workflow 内部子代理现在会带上真实平台上下文（如 web，
//     其 `*` 规则放开工作空间工具），若不显式拦发言，子代理会继承该平台的发帖
//     权限、绕过主会话的禁发帖配置去发帖。空平台的历史逻辑（platform=="" 即拒
//     发言）作为兜底保留——子代理无论平台为空还是带了 web，都不许发言。
//     非发言工具按平台规则评估（web 的 `*` 放开工作空间，使「审查并修复代码」
//     类节点能真正 exec/读写），不再被空平台的「敏感工具默认禁止」误伤。
func (e *evaluator) allow(botID, tool, platform, userID string, isSystem, isSubagent bool) bool {
	broadcast := IsBroadcastTool(tool)
	if broadcast && (platform == "" || isSubagent) {
		return false
	}
	if isSystem && !broadcast {
		return true
	}
	return e.svc.Evaluate(botID, tool, platform, userID)
}
