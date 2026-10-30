// Package secret 提供进程内密钥保险库（Vault）。
//
// 目标：LLM 与其供应商永远只能看到占位符 {{secret:name}}，真实值仅在工具
// 执行前的唯一咽喉点被还原，工具返回后立即被遮蔽回占位符。
//
// 隔离：条目按 bot 归属登记。Resolve / Get / Catalog 只看「本 bot 条目 + 全局
// 条目（BotID 为空，如早期登记的遗留项）」，同名时本 bot 优先——bot A 的
// {{secret:x}} 绝不会被 bot B 还原。Redact / StreamHoldLen 则对**所有**已登记值
// 生效：遮蔽是出站防线，多遮不漏，跨 bot 的值出现在任何输出里都应被盖住。
//
// 存储：明文落在 data/secrets/vault.json（0600，/data/ 已被 .gitignore 覆盖），
// v1 不加密——主密钥自举只会把问题平移，靠文件权限 + 出站硬闸收敛。
// 登记时同步 util/log.RegisterSecret，让日志层也遮蔽这些值。
package secret

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/kasuganosora/thinkbot/util/errs"
	ulog "github.com/kasuganosora/thinkbot/util/log"
)

// DefaultPath 是默认持久化路径；/data/ 已在 .gitignore 中被忽略。
const DefaultPath = "data/secrets/vault.json"

// MinValueLen 与 util/log.RegisterSecret 的下限一致：短于 12 字符的值拒绝
// 登记，避免把 "true" / "8080" 这类普通字符串误当密钥遮蔽。
const MinValueLen = 12

// PlaceholderPrefix 是占位符的固定命名空间，避免与通知类 {{name}} 冲突。
const PlaceholderPrefix = "secret:"

var (
	// ErrValueTooShort 值短于 MinValueLen。调用方据此区分「用户发的不是密钥」
	// 与「保险库本身不可用」，前者可以重新发起请求。
	ErrValueTooShort = errs.New("secret: 密钥值过短，拒绝登记")
	// ErrInvalidName 登记名不合法。
	ErrInvalidName = errs.New("secret: 非法密钥名")
	// ErrUnavailable 保险库加载失败：拒绝登记，避免新值只活在内存、重启即丢，
	// 也避免一次 save 把磁盘上那份读不出来的旧文件整个覆盖掉。
	ErrUnavailable = errs.New("secret: 保险库加载失败，暂不可登记")
)

var (
	// nameRE 约束登记名：字母/数字开头，允许点、下划线、连字符。
	nameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)
	// placeholderRE 匹配 {{secret:name}}（括号内侧允许空白）。
	// 采用单遍替换语义：替换进去的真实值不会被再次解析，防注入。
	placeholderRE = regexp.MustCompile(`\{\{\s*secret:([A-Za-z0-9_.-]+)\s*\}\}`)
)

// Entry 是一条已登记的密钥。
type Entry struct {
	// BotID 归属 bot；空表示全局条目（所有 bot 可见）。
	BotID       string `json:"bot_id,omitempty"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Value       string `json:"value"`
	UpdatedAt   int64  `json:"updated_at"`
}

// CatalogEntry 是给 LLM 看的目录项：只有名字与用途，绝不含值。
type CatalogEntry struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

// entryKey 是 (botID, name) 的内部存储键。name 的字符集不含 '/'，不会与 botID 混淆。
type entryKey struct {
	botID string
	name  string
}

// Vault 是并发安全的密钥注册表。
type Vault struct {
	mu       sync.RWMutex
	entries  map[entryKey]Entry
	filePath string // 空表示纯内存（测试用）
	loadErr  error  // 非 nil 表示加载失败，拒绝登记
}

// New 构造 Vault 并从 filePath 载入已有条目。文件不存在时返回空库。
//
// 兼容两种文件格式：早期的 map[name]Entry（无归属，载入为全局条目）与当前的
// []Entry（带 bot_id）。保存一律写成后者。
func New(filePath string) (*Vault, error) {
	v := &Vault{entries: make(map[entryKey]Entry), filePath: filePath}
	if filePath == "" {
		return v, nil
	}
	data, err := os.ReadFile(filePath)
	if err != nil {
		if os.IsNotExist(err) {
			return v, nil
		}
		return nil, fmt.Errorf("secret: read vault: %w", err)
	}
	var loaded []Entry
	trimmed := bytes.TrimSpace(data)
	switch {
	case len(trimmed) == 0:
		return v, nil
	case trimmed[0] == '{':
		var legacy map[string]Entry
		if err := json.Unmarshal(trimmed, &legacy); err != nil {
			return nil, fmt.Errorf("secret: decode vault: %w", err)
		}
		for name, e := range legacy {
			e.Name = name
			e.BotID = ""
			loaded = append(loaded, e)
		}
	default:
		if err := json.Unmarshal(trimmed, &loaded); err != nil {
			return nil, fmt.Errorf("secret: decode vault: %w", err)
		}
	}
	for _, e := range loaded {
		if e.Value == "" || e.Name == "" {
			continue
		}
		ulog.RegisterSecret(e.Value)
		v.entries[entryKey{e.BotID, e.Name}] = e
	}
	return v, nil
}

var (
	defaultMu    sync.RWMutex
	defaultVault *Vault
)

// Default 返回进程级单例（懒加载）。
//
// 加载失败时返回一个**拒绝登记**的空库（读路径照常可用、不 panic），并打 ERROR：
// 静默降级成纯内存库会让用户提供的密钥重启即丢，且无任何线索。
func Default() *Vault {
	defaultMu.RLock()
	v := defaultVault
	defaultMu.RUnlock()
	if v != nil {
		return v
	}
	defaultMu.Lock()
	defer defaultMu.Unlock()
	if defaultVault == nil {
		nv, err := New(DefaultPath)
		if err != nil {
			zap.S().Errorw("secret: vault load failed, registration disabled until fixed",
				"path", DefaultPath, "err", err)
			nv = &Vault{entries: make(map[entryKey]Entry), loadErr: err}
		}
		defaultVault = nv
	}
	return defaultVault
}

// SetDefault 在启动期注入自定义实例（测试或自定义路径）。
func SetDefault(v *Vault) {
	defaultMu.Lock()
	defaultVault = v
	defaultMu.Unlock()
}

// ---- 归属 bot 的 context 传递 ----

type botCtxKey struct{}

// WithBot 把「本次编排属于哪个 bot」注入 context，供执行咽喉点按 bot 还原占位符。
// 由编排入口（LLM stage / reply stage / 工作流调度）注入。
func WithBot(ctx context.Context, botID string) context.Context {
	if botID == "" {
		return ctx
	}
	return context.WithValue(ctx, botCtxKey{}, botID)
}

// BotFromContext 取 WithBot 注入的 botID（没有则返回空串 = 只能看到全局条目）。
func BotFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if v, ok := ctx.Value(botCtxKey{}).(string); ok {
		return v
	}
	return ""
}

// Register 以 botID 归属登记或覆盖一条密钥，并同步注册到日志脱敏层。
// botID 为空表示全局条目。持久化失败时回滚内存，保证「返回错误 = 没登记上」。
func (v *Vault) Register(botID, name, value, description string) error {
	if v.loadErr != nil {
		return ErrUnavailable
	}
	name = strings.TrimSpace(name)
	if !nameRE.MatchString(name) {
		return ErrInvalidName
	}
	value = strings.TrimSpace(value)
	if len(value) < MinValueLen {
		return ErrValueTooShort
	}
	key := entryKey{botID, name}
	v.mu.Lock()
	old, hadOld := v.entries[key]
	v.entries[key] = Entry{
		BotID:       botID,
		Name:        name,
		Description: description,
		Value:       value,
		UpdatedAt:   time.Now().Unix(),
	}
	v.mu.Unlock()

	// 先登记日志遮蔽再落盘：即便落盘失败回滚，这个值也已经在用户消息里出现过，
	// 日志层多遮一个值没有代价。
	ulog.RegisterSecret(value)
	if err := v.save(); err != nil {
		v.mu.Lock()
		if hadOld {
			v.entries[key] = old
		} else {
			delete(v.entries, key)
		}
		v.mu.Unlock()
		return err
	}
	return nil
}

// Get 取真实值：先找 botID 名下的条目，再回落全局条目。
// 仅供执行咽喉点调用，禁止把返回值写进任何出站文本。
func (v *Vault) Get(botID, name string) (string, bool) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	if botID != "" {
		if e, ok := v.entries[entryKey{botID, name}]; ok {
			return e.Value, true
		}
	}
	if e, ok := v.entries[entryKey{"", name}]; ok {
		return e.Value, true
	}
	return "", false
}

// Catalog 返回 botID 可见的目录（本 bot + 全局，同名本 bot 优先），按名字排序，不含值。
func (v *Vault) Catalog(botID string) []CatalogEntry {
	v.mu.RLock()
	byName := make(map[string]CatalogEntry)
	for k, e := range v.entries {
		switch {
		case k.botID == botID && botID != "":
			byName[k.name] = CatalogEntry{Name: e.Name, Description: e.Description}
		case k.botID == "":
			if _, own := v.entries[entryKey{botID, k.name}]; own && botID != "" {
				continue
			}
			byName[k.name] = CatalogEntry{Name: e.Name, Description: e.Description}
		}
	}
	v.mu.RUnlock()
	out := make([]CatalogEntry, 0, len(byName))
	for _, c := range byName {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Placeholder 返回某名字对应的占位符文本。
func Placeholder(name string) string {
	return "{{" + PlaceholderPrefix + name + "}}"
}

// Resolve 递归把 input 中 botID 可见的占位符还原为真实值。未知占位符（含别的
// bot 名下的）原样保留；单遍替换保证真实值内若含 {{...}} 不会被二次解析。
func (v *Vault) Resolve(botID string, input any) any {
	switch t := input.(type) {
	case string:
		return v.resolveString(botID, t)
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			out[k] = v.Resolve(botID, val)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = v.Resolve(botID, val)
		}
		return out
	default:
		return input
	}
}

func (v *Vault) resolveString(botID, s string) string {
	if !strings.Contains(s, "{{"+PlaceholderPrefix) {
		return s
	}
	var b strings.Builder
	last := 0
	for _, m := range placeholderRE.FindAllStringSubmatchIndex(s, -1) {
		b.WriteString(s[last:m[0]])
		name := s[m[2]:m[3]]
		if val, ok := v.Get(botID, name); ok {
			b.WriteString(val)
		} else {
			b.WriteString(s[m[0]:m[1]])
		}
		last = m[1]
	}
	b.WriteString(s[last:])
	return b.String()
}

// Redact 把文本中出现的任何已登记真实值（不分归属）遮蔽为对应占位符。
// 按值长度降序替换，避免短值先替换破坏长值。
func (v *Vault) Redact(text string) string {
	if text == "" {
		return text
	}
	type pair struct{ name, val string }
	v.mu.RLock()
	pairs := make([]pair, 0, len(v.entries))
	for _, e := range v.entries {
		if e.Value != "" {
			pairs = append(pairs, pair{e.Name, e.Value})
		}
	}
	v.mu.RUnlock()
	sort.Slice(pairs, func(i, j int) bool { return len(pairs[i].val) > len(pairs[j].val) })
	for _, p := range pairs {
		text = strings.ReplaceAll(text, p.val, Placeholder(p.name))
	}
	return text
}

// StreamHoldLen 返回 s 尾部需要扣住、暂不推送的字节数，用于流式跨 chunk 遮蔽。
//
// 背景：流式增量可能在某条已登记密钥的中间被切开，单独对一个 chunk 做 Redact
// 匹配不到跨界的完整值——前半段会被当成普通文本推送出去。调用方应在推送前保留
// 尾部 StreamHoldLen 字节到下一次增量，拼接后再 Redact：这样任何完整值在被推送前
// 必然完整地落在某次拼接里被遮蔽。
//
// 返回值是「s 的某个后缀恰好是某条已登记真实值的真前缀」时的最大长度；否则 0。
// 只扣住真正可能成值的前缀，普通文本零延迟放行——不做固定长度窗口，避免长密钥
// 把整条流扣到 Flush 才吐出、破坏实时性。与 Redact 一致，不分归属。
//
// 复杂度上界 O(值数量 × 最长值²)（自大到小命中即停）；典型 token 级密钥下远低于此。
// 若将来需要登记文件级超长密钥且流式吞吐很高，可改为前缀索引，这里保持最简正确解。
func (v *Vault) StreamHoldLen(s string) int {
	if s == "" {
		return 0
	}
	v.mu.RLock()
	vals := make([]string, 0, len(v.entries))
	for _, e := range v.entries {
		if e.Value != "" {
			vals = append(vals, e.Value)
		}
	}
	v.mu.RUnlock()

	best := 0
	for _, val := range vals {
		n := len(val)
		if n <= best+1 { // 该值不可能产生比 best 更长的真前缀，跳过
			continue
		}
		limit := n - 1 // 真前缀：不含完整值本身（完整值交给 Redact 处理）
		if limit > len(s) {
			limit = len(s)
		}
		for k := limit; k > best; k-- {
			if val[:k] == s[len(s)-k:] {
				best = k
				break
			}
		}
	}
	return best
}

// RedactValue 是 Redact 的递归版本，保持入参类型结构（string/map/slice）。
func (v *Vault) RedactValue(x any) any {
	switch t := x.(type) {
	case string:
		return v.Redact(t)
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			out[k] = v.RedactValue(val)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = v.RedactValue(val)
		}
		return out
	default:
		return x
	}
}

// IsTooShort 判断 Register 返回的错误是否为「值过短」。
func IsTooShort(err error) bool { return errors.Is(err, ErrValueTooShort) }

// save 原子写入持久化文件（0600）。内存库（filePath 为空）为空操作。
func (v *Vault) save() error {
	if v.filePath == "" {
		return nil
	}
	v.mu.RLock()
	entries := make([]Entry, 0, len(v.entries))
	for _, e := range v.entries {
		entries = append(entries, e)
	}
	v.mu.RUnlock()
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].BotID != entries[j].BotID {
			return entries[i].BotID < entries[j].BotID
		}
		return entries[i].Name < entries[j].Name
	})
	data, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return fmt.Errorf("secret: encode vault: %w", err)
	}
	dir := filepath.Dir(v.filePath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("secret: mkdir: %w", err)
	}
	tmp := v.filePath + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("secret: write vault: %w", err)
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		return fmt.Errorf("secret: chmod vault: %w", err)
	}
	if err := os.Rename(tmp, v.filePath); err != nil {
		return fmt.Errorf("secret: rename vault: %w", err)
	}
	return nil
}
