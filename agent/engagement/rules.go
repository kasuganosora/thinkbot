package engagement

import (
	"strings"
	"sync"
	"time"

	"github.com/kasuganosora/thinkbot/agent/core"
)

// ============================================================================
// Rule — Tier 1 规则接口
// ============================================================================

// Rule 是 Tier 1 规则引擎中的一条规则。
// 返回 (allow, reason)：
//   - allow=false 表示这条规则否决了消息（短路，不再检查后续规则）
//   - allow=true 表示这条规则放行，继续检查下一条规则
type Rule interface {
	// Allow 评估消息是否通过此规则。
	Allow(msg *core.Message) (allow bool, reason string)
}

// RuleFunc 函数适配器。
type RuleFunc func(msg *core.Message) (bool, string)

// Allow 实现 Rule。
func (f RuleFunc) Allow(msg *core.Message) (bool, string) {
	return f(msg)
}

// RuleEngine 是 Tier 1 规则引擎，组合多条规则。
//
// 执行策略：AND 语义——所有规则必须通过才允许。
// 规则按注册顺序执行，第一条否决的规则短路返回。
//
// lastReason 字段记录最近一次否决原因，Pipeline 串行处理单条消息，
// 不需要加锁。
type RuleEngine struct {
	rules      []Rule
	lastReason string
}

// NewRuleEngine 创建规则引擎。
func NewRuleEngine(rules ...Rule) *RuleEngine {
	return &RuleEngine{rules: rules}
}

// Allow 评估所有规则。任一规则否决则返回 false。
func (e *RuleEngine) Allow(msg *core.Message) bool {
	e.lastReason = ""
	for _, r := range e.rules {
		allow, reason := r.Allow(msg)
		if !allow {
			e.lastReason = reason
			return false
		}
	}
	return true
}

// LastReason 返回最近一次 Allow() 调用中的否决原因。
func (e *RuleEngine) LastReason() string {
	return e.lastReason
}

// ====================================================================
// 内置规则
// ====================================================================

// --- KeywordRule: 关键词/兴趣话题匹配 ---

// KeywordRule 检查消息文本是否包含配置的关键词。
//
// 匹配策略（任一命中即通过）：
//   - 大小写不敏感包含匹配
//   - 支持中英文关键词
type KeywordRule struct {
	keywords []string
}

// NewKeywordRule 创建关键词规则。
func NewKeywordRule(keywords ...string) *KeywordRule {
	// 预处理：统一转小写
	processed := make([]string, len(keywords))
	for i, kw := range keywords {
		processed[i] = strings.ToLower(strings.TrimSpace(kw))
	}
	return &KeywordRule{keywords: processed}
}

// Allow 实现 Rule。无关键词配置时放行所有消息。
func (r *KeywordRule) Allow(msg *core.Message) (bool, string) {
	if len(r.keywords) == 0 {
		return true, ""
	}
	text := strings.ToLower(msg.Text)
	for _, kw := range r.keywords {
		if kw != "" && strings.Contains(text, kw) {
			return true, "matched keyword: " + kw
		}
	}
	return false, "no keyword matched"
}

// --- BlocklistRule: 黑名单 ---

// BlocklistRule 排除特定用户或来源的消息。
type BlocklistRule struct {
	blockedUsers   map[string]bool
	blockedSources map[string]bool
}

// NewBlocklistRule 创建黑名单规则。
// blockedUsers 和 blockedSources 是要排除的用户 ID 和来源标识。
func NewBlocklistRule(blockedUsers, blockedSources []string) *BlocklistRule {
	r := &BlocklistRule{
		blockedUsers:   make(map[string]bool),
		blockedSources: make(map[string]bool),
	}
	for _, u := range blockedUsers {
		r.blockedUsers[u] = true
	}
	for _, s := range blockedSources {
		r.blockedSources[s] = true
	}
	return r
}

// Allow 实现 Rule。
func (r *BlocklistRule) Allow(msg *core.Message) (bool, string) {
	if r.blockedUsers[msg.UserID] {
		return false, "user blocked: " + msg.UserID
	}
	if r.blockedSources[msg.Source] {
		return false, "source blocked: " + msg.Source
	}
	return true, ""
}

// --- LengthRule: 消息长度过滤 ---

// LengthRule 根据消息文本长度过滤。
type LengthRule struct {
	minLen int
	maxLen int
}

// NewLengthRule 创建长度规则。
// minLen=0 表示无最小限制，maxLen=0 表示无最大限制。
func NewLengthRule(minLen, maxLen int) *LengthRule {
	return &LengthRule{minLen: minLen, maxLen: maxLen}
}

// Allow 实现 Rule。
func (r *LengthRule) Allow(msg *core.Message) (bool, string) {
	textLen := len([]rune(msg.Text))
	if r.minLen > 0 && textLen < r.minLen {
		return false, "message too short"
	}
	if r.maxLen > 0 && textLen > r.maxLen {
		return false, "message too long"
	}
	return true, ""
}

// --- SelfExclusionRule: 排除 Bot 自己的消息 ---

// SelfCheckerFunc 是判断用户 ID 是否属于 Bot 自身的函数类型。
// 通常绑定到 inbound.Ingress.IsSelfMessage 或 SelfIDSet.Contains。
type SelfCheckerFunc func(userID string) bool

// SelfExclusionRule 排除 Bot 自己发送的消息。
//
// 支持两种模式：
//   - 静态模式：通过 NewSelfExclusionRule(botUserID) 传入固定 ID（向后兼容）
//   - 动态模式：通过 NewSelfExclusionRuleFunc(checker) 传入检查函数，
//     与 Ingress 的 SelfIDSet 共享，能实时感知 Channel 注册的新 ID
type SelfExclusionRule struct {
	botUserID string
	checker   SelfCheckerFunc // 动态检查器（优先于 botUserID）
}

// NewSelfExclusionRule 创建自我排除规则（静态模式）。
func NewSelfExclusionRule(botUserID string) *SelfExclusionRule {
	return &SelfExclusionRule{botUserID: botUserID}
}

// NewSelfExclusionRuleFunc 创建自我排除规则（动态模式）。
// checker 通常为 inbound.Ingress.IsSelfMessage 或 SelfIDSet.Contains。
func NewSelfExclusionRuleFunc(checker SelfCheckerFunc) *SelfExclusionRule {
	return &SelfExclusionRule{checker: checker}
}

// Allow 实现 Rule。
func (r *SelfExclusionRule) Allow(msg *core.Message) (bool, string) {
	// 动态检查器优先
	if r.checker != nil && r.checker(msg.UserID) {
		return false, "self message"
	}
	// 静态 ID 回退
	if r.botUserID != "" && msg.UserID == r.botUserID {
		return false, "self message"
	}
	return true, ""
}

// --- RenoteExclusionRule: 排除纯转发 (Misskey 特有) ---

// RenoteExclusionRule 排除纯转发/Boost 消息（无文本内容，只有 renote_id）。
type RenoteExclusionRule struct{}

// NewRenoteExclusionRule 创建转发排除规则。
func NewRenoteExclusionRule() *RenoteExclusionRule {
	return &RenoteExclusionRule{}
}

// Allow 实现 Rule。
func (RenoteExclusionRule) Allow(msg *core.Message) (bool, string) {
	// Misskey 纯转发：有 renote_id 但文本为空或极短
	renoteID, _ := msg.Metadata["renote_id"].(string)
	if renoteID != "" && strings.TrimSpace(msg.Text) == "" {
		return false, "pure renote/boost"
	}
	return true, ""
}

// --- CooldownRule: 用户冷却 ---

// CooldownRule 对同一用户实施冷却时间。
// 同一用户在 cooldown 期间的消息不会被主动参与。
type CooldownRule struct {
	mu       sync.Mutex
	cooldown time.Duration
	lastSeen map[string]time.Time
}

// NewCooldownRule 创建用户冷却规则。
func NewCooldownRule(cooldown time.Duration) *CooldownRule {
	return &CooldownRule{
		cooldown: cooldown,
		lastSeen: make(map[string]time.Time),
	}
}

// Allow 实现 Rule。
func (r *CooldownRule) Allow(msg *core.Message) (bool, string) {
	if msg.UserID == "" || r.cooldown <= 0 {
		return true, ""
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	now := time.Now()

	// 惰性 GC：每 100 次调用清理一次过期条目
	if len(r.lastSeen) > 100 {
		for uid, t := range r.lastSeen {
			if now.Sub(t) > r.cooldown*2 {
				delete(r.lastSeen, uid)
			}
		}
	}

	if last, ok := r.lastSeen[msg.UserID]; ok {
		elapsed := now.Sub(last)
		if elapsed < r.cooldown {
			remaining := r.cooldown - elapsed
			return false, "user on cooldown, remaining: " + remaining.String()
		}
	}
	r.lastSeen[msg.UserID] = now
	return true, ""
}

// Reset 重置指定用户的冷却（Bot 成功回复后调用）。
func (r *CooldownRule) Reset(userID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.lastSeen, userID)
}
