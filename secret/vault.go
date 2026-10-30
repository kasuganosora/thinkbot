// Package secret 提供进程内密钥保险库（Vault）。
//
// 目标：LLM 与其供应商永远只能看到占位符 {{secret:name}}，真实值仅在工具
// 执行前的唯一咽喉点被还原，工具返回后立即被遮蔽回占位符。
//
// 存储：明文落在 data/secrets/vault.json（0600，/data/ 已被 .gitignore 覆盖），
// v1 不加密——主密钥自举只会把问题平移，靠文件权限 + 出站硬闸收敛。
// 登记时同步 util/log.RegisterSecret，让日志层也遮蔽这些值。
package secret

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/kasuganosora/thinkbot/util/errs"
	ulog "github.com/kasuganosora/thinkbot/util/log"
)

// DefaultPath 是默认持久化路径；/data/ 已在 .gitignore 中被忽略。
const DefaultPath = "data/secrets/vault.json"

// minSecretLen 与 util/log.RegisterSecret 的下限一致：短于 12 字符的值拒绝
// 登记，避免把 "true" / "8080" 这类普通字符串误当密钥遮蔽。
const minSecretLen = 12

// PlaceholderPrefix 是占位符的固定命名空间，避免与通知类 {{name}} 冲突。
const PlaceholderPrefix = "secret:"

var (
	// nameRE 约束登记名：字母/数字开头，允许点、下划线、连字符。
	nameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)
	// placeholderRE 匹配 {{secret:name}}（括号内侧允许空白）。
	// 采用单遍替换语义：替换进去的真实值不会被再次解析，防注入。
	placeholderRE = regexp.MustCompile(`\{\{\s*secret:([A-Za-z0-9_.-]+)\s*\}\}`)
)

// Entry 是一条已登记的密钥。
type Entry struct {
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

// Vault 是并发安全的密钥注册表。
type Vault struct {
	mu       sync.RWMutex
	entries  map[string]Entry
	filePath string // 空表示纯内存（测试用）
}

// New 构造 Vault 并从 filePath 载入已有条目。文件不存在时返回空库。
func New(filePath string) (*Vault, error) {
	v := &Vault{entries: make(map[string]Entry), filePath: filePath}
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
	var loaded map[string]Entry
	if err := json.Unmarshal(data, &loaded); err != nil {
		return nil, fmt.Errorf("secret: decode vault: %w", err)
	}
	for k, e := range loaded {
		if e.Value == "" {
			continue
		}
		e.Name = k
		ulog.RegisterSecret(e.Value)
		v.entries[k] = e
	}
	return v, nil
}

var (
	defaultMu    sync.RWMutex
	defaultVault *Vault
)

// Default 返回进程级单例（懒加载）。加载失败时降级为空内存库，保证调用方拿到非 nil。
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
			nv = &Vault{entries: make(map[string]Entry)}
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

// Register 登记或覆盖一条密钥，并同步注册到日志脱敏层。
func (v *Vault) Register(name, value, description string) error {
	name = strings.TrimSpace(name)
	if !nameRE.MatchString(name) {
		return errs.New("secret: 非法密钥名")
	}
	value = strings.TrimSpace(value)
	if len(value) < minSecretLen {
		return errs.New("secret: 密钥值过短，拒绝登记")
	}
	v.mu.Lock()
	old := v.entries[name]
	v.entries[name] = Entry{
		Name:        name,
		Description: description,
		Value:       value,
		UpdatedAt:   time.Now().Unix(),
	}
	v.mu.Unlock()

	ulog.RegisterSecret(value)
	if old.Value != "" && old.Value != value {
		ulog.RegisterSecret(old.Value)
	}
	return v.save()
}

// Get 取真实值。仅供执行咽喉点调用，禁止把返回值写进任何出站文本。
func (v *Vault) Get(name string) (string, bool) {
	v.mu.RLock()
	e, ok := v.entries[name]
	v.mu.RUnlock()
	if !ok {
		return "", false
	}
	return e.Value, true
}

// Catalog 返回按名字排序的目录（不含值），供系统提示词注入。
func (v *Vault) Catalog() []CatalogEntry {
	v.mu.RLock()
	out := make([]CatalogEntry, 0, len(v.entries))
	for _, e := range v.entries {
		out = append(out, CatalogEntry{Name: e.Name, Description: e.Description})
	}
	v.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Placeholder 返回某名字对应的占位符文本。
func Placeholder(name string) string {
	return "{{" + PlaceholderPrefix + name + "}}"
}

// Resolve 递归把 input 中的占位符还原为真实值。未知占位符原样保留并交由
// 调用方告警；单遍替换保证真实值内若含 {{...}} 不会被二次解析。
func (v *Vault) Resolve(input any) any {
	switch t := input.(type) {
	case string:
		return v.resolveString(t)
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			out[k] = v.Resolve(val)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = v.Resolve(val)
		}
		return out
	default:
		return input
	}
}

func (v *Vault) resolveString(s string) string {
	if !strings.Contains(s, "{{"+PlaceholderPrefix) {
		return s
	}
	var b strings.Builder
	last := 0
	for _, m := range placeholderRE.FindAllStringSubmatchIndex(s, -1) {
		b.WriteString(s[last:m[0]])
		name := s[m[2]:m[3]]
		if val, ok := v.Get(name); ok {
			b.WriteString(val)
		} else {
			b.WriteString(s[m[0]:m[1]])
		}
		last = m[1]
	}
	b.WriteString(s[last:])
	return b.String()
}

// Redact 把文本中出现的任何已登记真实值遮蔽为对应占位符。
// 按值长度降序替换，避免短值先替换破坏长值。
func (v *Vault) Redact(text string) string {
	if text == "" {
		return text
	}
	type pair struct{ name, val string }
	v.mu.RLock()
	pairs := make([]pair, 0, len(v.entries))
	for name, e := range v.entries {
		if e.Value != "" {
			pairs = append(pairs, pair{name, e.Value})
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
// 把整条流扣到 Flush 才吐出、破坏实时性。
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

// save 原子写入持久化文件（0600）。内存库（filePath 为空）为空操作。
func (v *Vault) save() error {
	if v.filePath == "" {
		return nil
	}
	v.mu.RLock()
	entries := make(map[string]Entry, len(v.entries))
	for k, e := range v.entries {
		entries[k] = e
	}
	v.mu.RUnlock()
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
