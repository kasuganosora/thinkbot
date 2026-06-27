package config

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kasuganosora/thinkbot/dao"
	"github.com/kasuganosora/thinkbot/util/errs"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Setting 是 dao.Setting 的类型别名。
// 实际模型定义在 dao/setting.go，此处仅保留别名以保持 config 包内部代码简洁。
type Setting = dao.Setting

// ============================================================================\
// Store — 全局配置中心
//
// 配置读取优先级（从高到低）：
//  1. 运行时覆盖（SetTemporary / overrides map）
//  2. .env 文件
//  3. 数据库（config_settings 表）
//  4. 操作系统环境变量
//  5. 默认值（调用方提供）
//
// 所有模块必须通过 Store 读取配置，不允许直接访问 os.Getenv 或硬编码。
// ============================================================================

// Store 是全局配置访问器。
// 它是并发安全的，可以在多个 goroutine 中使用。
type Store struct {
	db *gorm.DB

	mu        sync.RWMutex
	envFile   map[string]string // .env 文件值
	overrides map[string]string // 运行时临时覆盖（最高优先级）
	dbCache   map[string]string // 数据库缓存

	// 监听器
	listenersMu sync.RWMutex
	listeners   []func(key, oldValue, newValue string)
}

// NewStore 创建配置 Store。
// db 为 nil 时跳过数据库层（仅从 .env 和环境变量读取）。
func NewStore(db *gorm.DB) *Store {
	return &Store{
		db:        db,
		overrides: make(map[string]string),
		envFile:   make(map[string]string),
		dbCache:   make(map[string]string),
	}
}

// ============================================================================\
// 核心读取接口
// ============================================================================

// Get 按优先级返回配置值。
// 键不存在时返回空字符串和 false。
func (s *Store) Get(key string) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.getLocked(key)
}

// getLocked 在已持锁的情况下按优先级返回配置值。
func (s *Store) getLocked(key string) (string, bool) {
	// 1. 运行时覆盖
	if v, ok := s.overrides[key]; ok {
		return v, true
	}

	// 2. .env 文件
	if v, ok := s.envFile[key]; ok {
		return v, true
	}

	// 3. 数据库缓存（Set/Reload 后即生效）
	if v, ok := s.dbCache[key]; ok {
		return v, true
	}

	// 4. 操作系统环境变量
	envKey := ConfigKeyToEnvKey(key)
	if v, ok := os.LookupEnv(envKey); ok {
		return v, true
	}

	return "", false
}

// GetString 返回字符串配置值，不存在时返回默认值。
func (s *Store) GetString(key, def string) string {
	if v, ok := s.Get(key); ok {
		return v
	}
	return def
}

// GetInt 返回 int 配置值。
func (s *Store) GetInt(key string, def int) int {
	v, ok := s.Get(key)
	if !ok {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

// GetInt64 返回 int64 配置值。
func (s *Store) GetInt64(key string, def int64) int64 {
	v, ok := s.Get(key)
	if !ok {
		return def
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return def
	}
	return n
}

// GetFloat64 返回 float64 配置值。
func (s *Store) GetFloat64(key string, def float64) float64 {
	v, ok := s.Get(key)
	if !ok {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return def
	}
	return f
}

// GetBool 返回布尔配置值。
// 支持的 true 值：true, 1, yes, on, enable, enabled（不区分大小写）。
func (s *Store) GetBool(key string, def bool) bool {
	v, ok := s.Get(key)
	if !ok {
		return def
	}
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "true", "1", "yes", "on", "enable", "enabled":
		return true
	case "false", "0", "no", "off", "disable", "disabled":
		return false
	default:
		return def
	}
}

// GetDuration 返回 time.Duration 配置值。
// 支持标准 Go duration 字符串（"5s", "30m", "1h30m"）或纯秒数。
func (s *Store) GetDuration(key string, def time.Duration) time.Duration {
	v, ok := s.Get(key)
	if !ok {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		// 尝试解析为秒数
		if secs, err2 := strconv.ParseFloat(v, 64); err2 == nil {
			return time.Duration(secs * float64(time.Second))
		}
		return def
	}
	return d
}

// GetStringSlice 返回字符串切片配置值。
// 值以逗号分隔，自动去除空白。
func (s *Store) GetStringSlice(key string, def []string) []string {
	v, ok := s.Get(key)
	if !ok {
		return def
	}
	parts := strings.Split(v, ",")
	result := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			result = append(result, p)
		}
	}
	if len(result) == 0 {
		return def
	}
	return result
}

// ============================================================================\
// 批量读取 & 结构体绑定
// ============================================================================

// Unmarshal 将配置键绑定到结构体。
//
// prefix 是键前缀（如 "bot"），结构体字段通过 `config:"field_name"` tag 映射。
// 支持嵌套（嵌套结构体的字段名用点号连接）。
//
// 示例：
//
//	type BotSettings struct {
//	    Model       string  `config:"model"`
//	    Temperature float64 `config:"temperature"`
//	    MaxTokens   int     `config:"max_tokens"`
//	}
//	var settings BotSettings
//	store.Unmarshal("bot", &settings)
func (s *Store) Unmarshal(prefix string, target any) error {
	return s.unmarshalStruct(prefix, target)
}

// GetByPrefix 返回所有以 prefix 开头的配置项。
// 返回的 map 键为去除 prefix 后的剩余部分。
func (s *Store) GetByPrefix(prefix string) map[string]string {
	result := make(map[string]string)

	s.mu.RLock()
	defer s.mu.RUnlock()

	check := func(source map[string]string) {
		for k, v := range source {
			if rest, ok := strings.CutPrefix(k, prefix); ok {
				rest = strings.TrimPrefix(rest, ".")
				if _, exists := result[rest]; !exists {
					result[rest] = v
				}
			}
		}
	}

	check(s.overrides)
	check(s.envFile)
	check(s.dbCache)

	// 环境变量
	// 去掉 prefix 尾部的分隔符，避免 ConfigKeyToEnvKey 转换后产生双下划线
	envKeyPrefix := ConfigKeyToEnvKey(strings.TrimSuffix(prefix, ".")) + "_"
	for _, kv := range os.Environ() {
		envKey, envVal, found := strings.Cut(kv, "=")
		if !found {
			continue
		}
		if rest, ok := strings.CutPrefix(envKey, envKeyPrefix); ok {
			rest = EnvKeyToConfigKey(rest)
			if _, exists := result[rest]; !exists {
				result[rest] = envVal
			}
		}
	}

	return result
}

// ============================================================================\
// 写入 & 持久化
// ============================================================================

// Set 将配置值持久化到数据库。
// 同时更新内存缓存和触发监听器。
func (s *Store) Set(ctx context.Context, key, value string) error {
	if err := ValidateKey(key); err != nil {
		return err
	}

	if s.db == nil {
		// 无数据库时仅写入运行时覆盖
		s.SetTemporary(key, value)
		return nil
	}

	setting := Setting{
		Key:   key,
		Value: value,
	}
	if err := s.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "key"}},
		DoUpdates: clause.AssignmentColumns([]string{"value", "updated_at"}),
	}).Create(&setting).Error; err != nil {
		return errs.Wrapf(err, "config: persist setting %q", key)
	}

	// 更新缓存（在锁内读取旧值，避免 TOCTOU）
	s.mu.Lock()
	oldVal, _ := s.getLocked(key)
	s.dbCache[key] = value
	s.mu.Unlock()

	s.notifyListeners(key, oldVal, value)
	return nil
}

// SetWithMeta 将配置值和元数据（分类、描述）一起持久化到数据库。
// 用于在注册新配置项时声明其元信息，方便前端渲染设置界面。
// 对已有记录，会更新 value、category、description 三个字段。
func (s *Store) SetWithMeta(ctx context.Context, key, value, category, description string) error {
	if err := ValidateKey(key); err != nil {
		return err
	}

	if s.db == nil {
		s.SetTemporary(key, value)
		return nil
	}

	setting := Setting{
		Key:         key,
		Value:       value,
		Category:    category,
		Description: description,
	}
	if err := s.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "key"}},
		DoUpdates: clause.AssignmentColumns([]string{"value", "category", "description", "updated_at"}),
	}).Create(&setting).Error; err != nil {
		return errs.Wrapf(err, "config: persist setting %q with meta", key)
	}

	// 更新缓存（在锁内读取旧值，避免 TOCTOU）
	s.mu.Lock()
	oldVal, _ := s.getLocked(key)
	s.dbCache[key] = value
	s.mu.Unlock()

	s.notifyListeners(key, oldVal, value)
	return nil
}

// RegisterMeta 为配置键声明元数据（分类、描述），不改变当前值。
// 如果键不存在则创建空值记录；如果已存在则仅更新 category 和 description。
// 适合在启动时批量注册所有配置项的元信息。
func (s *Store) RegisterMeta(ctx context.Context, key, category, description string) error {
	if err := ValidateKey(key); err != nil {
		return err
	}

	if s.db == nil {
		return nil
	}

	setting := Setting{
		Key:         key,
		Category:    category,
		Description: description,
	}
	if err := s.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "key"}},
		DoUpdates: clause.AssignmentColumns([]string{"category", "description", "updated_at"}),
	}).Create(&setting).Error; err != nil {
		return errs.Wrapf(err, "config: register meta for %q", key)
	}
	return nil
}

// RegisterMany 批量注册元数据。
func (s *Store) RegisterMany(ctx context.Context, metas []MetaSpec) error {
	if s.db == nil {
		return nil
	}
	for _, m := range metas {
		if err := s.RegisterMeta(ctx, m.Key, m.Category, m.Description); err != nil {
			return err
		}
	}
	return nil
}

// MetaSpec 描述一个配置项的元数据。
type MetaSpec struct {
	Key         string `json:"key"`
	Category    string `json:"category"`
	Description string `json:"description"`
}

// SetMany 批量持久化配置值。
func (s *Store) SetMany(ctx context.Context, kv map[string]string) error {
	for k := range kv {
		if err := ValidateKey(k); err != nil {
			return err
		}
	}

	if s.db == nil {
		s.mu.Lock()
		maps.Copy(s.overrides, kv)
		s.mu.Unlock()
		return nil
	}

	tx := s.db.WithContext(ctx).Begin()
	if tx.Error != nil {
		return tx.Error
	}

	for key, value := range kv {
		setting := Setting{Key: key, Value: value}
		if err := tx.Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "key"}},
			DoUpdates: clause.AssignmentColumns([]string{"value", "updated_at"}),
		}).Create(&setting).Error; err != nil {
			if rbErr := tx.Rollback().Error; rbErr != nil {
				return errs.Wrapf(rbErr, "config: rollback failed while handling persist error for %q: %v", key, err)
			}
			return errs.Wrapf(err, "config: persist setting %q", key)
		}
	}

	if err := tx.Commit().Error; err != nil {
		return errs.Wrap(err, "config: commit settings batch")
	}

	// 更新缓存并通知监听器
	s.mu.Lock()
	oldVals := make(map[string]string, len(kv))
	for key, value := range kv {
		oldVals[key] = s.dbCache[key]
		s.dbCache[key] = value
	}
	s.mu.Unlock()

	for key, value := range kv {
		s.notifyListeners(key, oldVals[key], value)
	}

	return nil
}

// SetTemporary 设置运行时覆盖值（不持久化到数据库）。
// 优先级最高，立即生效。
func (s *Store) SetTemporary(key, value string) {
	s.mu.Lock()
	oldVal, _ := s.getLocked(key)
	s.overrides[key] = value
	s.mu.Unlock()
	s.notifyListeners(key, oldVal, value)
}

// Delete 从数据库中删除配置项。
// 运行时覆盖也会被清除。
func (s *Store) Delete(ctx context.Context, key string) error {
	if s.db != nil {
		if err := s.db.WithContext(ctx).Where("key = ?", key).Delete(&Setting{}).Error; err != nil {
			return errs.Wrapf(err, "config: delete setting %q", key)
		}
	}

	s.mu.Lock()
	oldVal, _ := s.getLocked(key)
	delete(s.overrides, key)
	delete(s.dbCache, key)
	s.mu.Unlock()

	s.notifyListeners(key, oldVal, "")
	return nil
}

// ============================================================================
// 查询接口（供前端设置界面使用）
// ============================================================================

// GetSetting 返回完整的 Setting 记录（包含 value、category、description）。
// 从数据库读取，不存在时返回 nil。
func (s *Store) GetSetting(ctx context.Context, key string) (*Setting, error) {
	if s.db == nil {
		return nil, fmt.Errorf("config: database not available")
	}
	var setting Setting
	err := s.db.WithContext(ctx).Where("key = ?", key).First(&setting).Error
	if err != nil {
		return nil, errs.Wrapf(err, "config: get setting %q", key)
	}
	return &setting, nil
}

// ListSettings 返回所有配置项（含元数据），按 category、key 排序。
// 供前端设置界面全量渲染使用。
func (s *Store) ListSettings(ctx context.Context) ([]Setting, error) {
	if s.db == nil {
		return nil, fmt.Errorf("config: database not available")
	}
	var settings []Setting
	err := s.db.WithContext(ctx).
		Order("category ASC, key ASC").
		Find(&settings).Error
	if err != nil {
		return nil, errs.Wrap(err, "config: list settings")
	}
	return settings, nil
}

// ListByCategory 返回指定分类下的所有配置项。
func (s *Store) ListByCategory(ctx context.Context, category string) ([]Setting, error) {
	if s.db == nil {
		return nil, fmt.Errorf("config: database not available")
	}
	var settings []Setting
	err := s.db.WithContext(ctx).
		Where("category = ?", category).
		Order("key ASC").
		Find(&settings).Error
	if err != nil {
		return nil, errs.Wrapf(err, "config: list settings by category %q", category)
	}
	return settings, nil
}

// ListCategories 返回所有已使用的分类名（去重、排序）。
func (s *Store) ListCategories(ctx context.Context) ([]string, error) {
	if s.db == nil {
		return nil, fmt.Errorf("config: database not available")
	}
	var categories []string
	err := s.db.WithContext(ctx).
		Model(&Setting{}).
		Distinct("category").
		Where("category != ''").
		Order("category ASC").
		Pluck("category", &categories).Error
	if err != nil {
		return nil, errs.Wrap(err, "config: list categories")
	}
	return categories, nil
}

// ============================================================================
// .env 文件加载
// ============================================================================

// LoadEnvFile 从文件加载 .env 值。
// .env 文件中的键名直接作为配置键存储（如 "llm.openai.api_key"）。
// 多次调用会合并（后加载的覆盖先前的）。
func (s *Store) LoadEnvFile(path string) error {
	values, err := LoadEnvFile(path)
	if err != nil {
		return err
	}
	if values == nil {
		return nil
	}

	s.mu.Lock()
	maps.Copy(s.envFile, values)
	s.mu.Unlock()

	return nil
}

// LoadEnvMap 从内存映射加载配置（用于测试）。
func (s *Store) LoadEnvMap(values map[string]string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	maps.Copy(s.envFile, values)
}

// ============================================================================\
// 数据库刷新
// ============================================================================

// Reload 从数据库重新加载所有配置到缓存。
func (s *Store) Reload(ctx context.Context) error {
	if s.db == nil {
		return nil
	}

	var settings []Setting
	if err := s.db.WithContext(ctx).Find(&settings).Error; err != nil {
		return errs.Wrap(err, "config: reload from database")
	}

	newCache := make(map[string]string, len(settings))
	for _, s := range settings {
		newCache[s.Key] = s.Value
	}

	s.mu.Lock()
	s.dbCache = newCache
	s.mu.Unlock()

	return nil
}

// Migrate 创建配置表（幂等）。
func (s *Store) Migrate() error {
	if s.db == nil {
		return nil
	}
	return s.db.AutoMigrate(&Setting{})
}

// ============================================================================\
// 变更监听
// ============================================================================

// OnChange 注册配置变更监听器。
// 回调在值发生变化时被调用（同步执行，注意不要阻塞）。
// 返回取消注册的函数。
func (s *Store) OnChange(fn func(key, oldValue, newValue string)) func() {
	s.listenersMu.Lock()
	defer s.listenersMu.Unlock()

	s.listeners = append(s.listeners, fn)
	idx := len(s.listeners) - 1

	return func() {
		s.listenersMu.Lock()
		defer s.listenersMu.Unlock()
		s.listeners[idx] = nil // 置空避免索引偏移
	}
}

func (s *Store) notifyListeners(key, oldVal, newVal string) {
	if oldVal == newVal {
		return
	}
	s.listenersMu.RLock()
	listeners := make([]func(string, string, string), 0, len(s.listeners))
	for _, l := range s.listeners {
		if l != nil {
			listeners = append(listeners, l)
		}
	}
	s.listenersMu.RUnlock()

	for _, l := range listeners {
		l(key, oldVal, newVal)
	}
}

// ============================================================================\
// 结构体绑定（内部实现）
// ============================================================================

func (s *Store) unmarshalStruct(prefix string, target any) error {
	// 收集所有匹配前缀的配置项
	all := s.GetByPrefix(prefix)

	// 构建嵌套 map 并转为 JSON，再 unmarshal 到结构体
	jsonMap := buildNestedMap(all)
	jsonBytes, err := json.Marshal(jsonMap)
	if err != nil {
		return errs.Wrap(err, "config: marshal for unmarshal")
	}

	if err := json.Unmarshal(jsonBytes, target); err != nil {
		return errs.Wrap(err, "config: unmarshal into target")
	}

	return nil
}

// buildNestedMap 将扁平的 "a.b.c" → "value" 映射转换为嵌套 map。
// 例：{"model": "gpt-4o", "max_tokens": "4096"} → {"model":"gpt-4o","max_tokens":"4096"}
func buildNestedMap(flat map[string]string) map[string]any {
	result := make(map[string]any)
	for k, v := range flat {
		result[k] = autoType(v)
	}
	return result
}

// autoType 尝试将字符串值转换为最合适的 JSON 类型。
func autoType(s string) any {
	// 尝试 int
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		return n
	}
	// 尝试 float
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		return f
	}
	// 尝试 bool
	switch strings.ToLower(s) {
	case "true":
		return true
	case "false":
		return false
	}
	return s
}
