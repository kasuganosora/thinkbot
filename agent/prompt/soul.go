package prompt

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kasuganosora/thinkbot/util/errs"
)

// ============================================================================
// SoulLoader — SOUL.md 人格定义加载器
//
// 设计理念（受 Hermes Agent 的 SOUL.md 机制启发）：
//
// SOUL.md 是 bot 人格的权威来源，必须存在。
// 如果文件不存在则自动创建一个默认模板，用户可自行编辑。
// SOUL.md 定义 bot 的性格、身份、行为准则和核心指令，
// 注入到 system prompt 最高优先级位置（Order=0, Section="identity"）。
//
// 文件查找顺序（botID 用于区分多 bot）：
//   1. 显式指定的路径（NewSoulLoader config.Path）
//   2. 二进制所在目录下的 {botID}/SOUL.md（os.Executable() 解析）
//   3. 当前工作目录下的 {botID}/SOUL.md（兜底）
//   4. 如果 botID 为空，则查找二进制目录下的 SOUL.md（单 bot 兼容）
//
// 工作流程：
//   1. SoulLoader 从解析后的路径读取 SOUL.md
//   2. 将内容解析为 Section（支持 YAML front matter 和 {{.Var}} 模板变量）
//   3. 注册为 "identity" Section（Order=0），覆盖 PromptStage 的 fallback 逻辑
//   4. 后台轮询文件修改时间，变更时自动重新加载并更新 Registry
//
// 优先级规则：
//   - 如果 SOUL.md 存在 → 加载并使用
//   - 如果 SOUL.md 不存在 → 自动创建默认模板并加载（用户可编辑后热重载生效）
//
// 热重载：
//   - 通过轮询文件 mtime 检测变更（兼容所有操作系统，不依赖 inotify/fsnotify）
//   - 变更后自动重新注册 Section，下一次 Assemble() 即生效
// ============================================================================

// SoulLoaderConfig 配置 SoulLoader。
type SoulLoaderConfig struct {
	// Path SOUL.md 文件路径。
	// 留空时自动解析为二进制目录下的 {BotID}/SOUL.md。
	Path string

	// BotID bot 标识符，用于多 bot 隔离。
	// 路径解析为 {二进制目录}/{BotID}/SOUL.md。
	// 为空时退化为 {二进制目录}/SOUL.md（单 bot 兼容）。
	BotID string

	// SectionName 注册到 Registry 的 Section 名称（默认 "identity"）。
	SectionName string

	// Order Section 排序权重（默认 0，最高优先级）。
	Order int

	// ReloadInterval 文件变更检测的轮询间隔。
	// 0 表示不自动检测（仅在手动调用 Load() 时更新）。
	// 推荐值：5s ~ 30s。
	ReloadInterval time.Duration

	// MaxContentBytes SOUL.md 内容的最大字节数。
	// 超限时执行头尾保留截断（70% 头 + 20% 尾，中间省略）。
	// 0 表示不截断（默认 20000，约 5K tokens）。
	MaxContentBytes int

	// ScanMode 安全扫描模式：
	//   ScanModeOff   — 不扫描
	//   ScanModeWarn  — 扫描并记录告警日志（默认）
	//   ScanModeBlock — 扫描并阻止加载
	ScanMode ScanMode

	// Store 抽象文件 IO 后端（可选）。
	// 留空时默认使用 osSoulStore（直接 os.* 操作 config.Path 指向的宿主路径），
	// 兼容 local 后端与单测。docker 持久容器（DooD）模式下由调用方注入基于
	// sandbox.Workspace 的实现，使 SOUL.md 读写落到 bot 容器 named volume 内的
	// 真实文件（/data/SOUL.md）而非主程序侧空目录 —— 单一数据源，agent 自改可持久化。
	Store SoulStore
}

// DefaultSoulLoaderConfig 返回合理的默认配置。
// Path 留空，NewSoulLoader 中会自动解析为二进制目录。
func DefaultSoulLoaderConfig() SoulLoaderConfig {
	return SoulLoaderConfig{
		Path:            "",
		SectionName:     "identity",
		Order:           0,
		ReloadInterval:  5 * time.Second,
		MaxContentBytes: 20000,
		ScanMode:        ScanModeWarn,
	}
}

// DefaultSoulPath 解析二进制所在目录下的 {botID}/SOUL.md 路径。
// botID 为空时退化为 SOUL.md（单 bot 兼容）。
// 回退顺序：二进制目录/{botID}/SOUL.md → 当前工作目录/{botID}/SOUL.md。
func DefaultSoulPath(botID string) string {
	name := "SOUL.md"
	if botID != "" {
		name = filepath.Join(botID, "SOUL.md")
	}

	// 1. 尝试二进制目录
	if exe, err := os.Executable(); err == nil {
		p := filepath.Join(filepath.Dir(exe), name)
		if fileExists(p) {
			return p
		}
	}
	// 2. 回退到当前工作目录
	return name
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// DefaultSoulContent 是自动创建的默认 SOUL.md 模板。
// 用户可以编辑这个文件来定义 bot 的人格和行为准则。
const DefaultSoulContent = `# Soul

You are a helpful AI assistant.

## Personality

- Friendly and approachable
- Concise and direct in communication
- Helpful and knowledgeable

## Guidelines

- Respond in the same language as the user
- Be honest and transparent
- If you don't know something, say so

<!-- Edit this file to customize your bot's personality. Changes are hot-reloaded. -->
`

// ============================================================================
// SoulStore — 文件 IO 抽象
//
// SoulLoader 默认直接调用 os.* 操作宿主路径（local 后端 / 单测）。
// docker 持久容器（DooD）模式下，bot 的 SOUL.md 真实文件位于 bot 容器
// named volume 内的 /data/SOUL.md，而主程序侧 {WorkspaceDir}/{botID}/ 是空目录。
// 此时调用方应注入基于 sandbox.Workspace 的 SoulStore 实现，让读取 / 写入 /
// 轮询 mtime 都经 docker exec 落到真实文件，实现单一数据源（agent 自改可持久化）。
// ============================================================================

// SoulStat 是 StatSoul 的结果。
// Exists 表示文件存在；ModTime 为其最后修改时间；
// Err 表示传输层错误（与「文件不存在」区分：后者 Exists=false 且 Err=nil）。
type SoulStat struct {
	Exists  bool
	ModTime time.Time
	Err     error
}

// SoulStore 抽象 SoulLoader 所需的文件 IO。
// path 的语义由实现决定：osSoulStore 视其为宿主路径；
// workspaceSoulStore 视其为工作空间内相对路径（如 "SOUL.md"）。
type SoulStore interface {
	ReadSoul(ctx context.Context, path string) ([]byte, error)
	WriteSoul(ctx context.Context, path string, data []byte) error
	StatSoul(ctx context.Context, path string) SoulStat
}

// osSoulStore 是默认实现，直接操作宿主文件系统（os.ReadFile/Stat/WriteFile/MkdirAll）。
// 行为与改造前完全一致，保证 local 后端与既有测试零回归。
type osSoulStore struct{}

func (osSoulStore) ReadSoul(_ context.Context, path string) ([]byte, error) {
	return os.ReadFile(path)
}

func (osSoulStore) WriteSoul(_ context.Context, path string, data []byte) error {
	dir := filepath.Dir(path)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	return os.WriteFile(path, data, 0o644)
}

func (osSoulStore) StatSoul(_ context.Context, path string) SoulStat {
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return SoulStat{Exists: false}
		}
		return SoulStat{Exists: false, Err: err}
	}
	return SoulStat{Exists: true, ModTime: info.ModTime()}
}

// OnReloadFunc 是 SOUL.md 热重载后的回调。
// content 为重新加载后的内容（已去除 front matter）。
type OnReloadFunc func(content string)

// SoulLoader 负责加载和热重载 SOUL.md 文件。
type SoulLoader struct {
	config   SoulLoaderConfig
	registry *Registry

	mu        sync.RWMutex
	content   string    // 当前 SOUL.md 内容（已去除 front matter）
	modTime   time.Time // 文件的最后修改时间
	variables []Variable
	loaded    atomic.Bool // 是否已成功加载
	logger    SoulLogger  // 可选日志接口

	// onReload 热重载后回调（可选）。调用方在此更新依赖方（如 AdaptiveEngagementSyncer）。
	onReload OnReloadFunc

	stopCh  chan struct{}
	stopped atomic.Bool
}

// SoulLogger 是 SoulLoader 使用的最小日志接口。
type SoulLogger interface {
	Infof(format string, args ...any)
	Warnf(format string, args ...any)
	Errorf(format string, args ...any)
}

// noopSoulLogger 是默认的空日志实现。
type noopSoulLogger struct{}

func (noopSoulLogger) Infof(string, ...any)  {}
func (noopSoulLogger) Warnf(string, ...any)  {}
func (noopSoulLogger) Errorf(string, ...any) {}

// NewSoulLoader 创建 SOUL.md 加载器。
// 如果 config.Path 为空，自动解析为二进制目录下的 {BotID}/SOUL.md。
func NewSoulLoader(config SoulLoaderConfig, registry *Registry) *SoulLoader {
	if config.Path == "" {
		config.Path = DefaultSoulPath(config.BotID)
	}
	if config.SectionName == "" {
		config.SectionName = "identity"
	}
	if config.Store == nil {
		config.Store = osSoulStore{}
	}
	return &SoulLoader{
		config:   config,
		registry: registry,
		logger:   noopSoulLogger{},
		stopCh:   make(chan struct{}),
	}
}

// WithLogger 设置日志接口。
func (l *SoulLoader) WithLogger(logger SoulLogger) *SoulLoader {
	if logger != nil {
		l.logger = logger
	}
	return l
}

// Path 返回实际使用的 SOUL.md 路径。
func (l *SoulLoader) Path() string {
	return l.config.Path
}

// createDefault 创建默认 SOUL.md 文件（父目录由 Store 实现负责创建）。
func (l *SoulLoader) createDefault() error {
	return l.config.Store.WriteSoul(context.Background(), l.config.Path, []byte(DefaultSoulContent))
}

// Load 读取 SOUL.md 并注册为 Section。
// 文件不存在时自动创建默认模板。
func (l *SoulLoader) Load() error {
	// 先探测存在性（后端无关：OS 走 os.Stat，docker 走 ListDir），
	// 再决定是否读取或创建默认模板。避免依赖 Store 返回 os.IsNotExist
	// （workspace 后端的错误来自 docker exec，非 os.PathError）。
	st := l.config.Store.StatSoul(context.Background(), l.config.Path)
	if st.Err != nil {
		return errs.Wrapf(st.Err, "soul: stat %s", l.config.Path)
	}

	var data []byte
	if !st.Exists {
		// 文件不存在 → 自动创建默认 SOUL.md 模板
		if createErr := l.createDefault(); createErr != nil {
			return errs.Wrapf(createErr, "soul: create default %s", l.config.Path)
		}
		l.logger.Infof("soul: created default SOUL.md at %s — edit it to customize personality", l.config.Path)
		data = []byte(DefaultSoulContent)
	} else {
		read, err := l.config.Store.ReadSoul(context.Background(), l.config.Path)
		if err != nil {
			return errs.Wrapf(err, "soul: read %s", l.config.Path)
		}
		data = read
	}

	content := string(data)

	// 文件修改时间（已由上方 StatSoul 取得；刚创建默认模板时用当前时间兜底）
	info := st.ModTime
	if !st.Exists {
		info = time.Now()
	}

	// 解析 front matter
	body, meta := parseFrontMatter(content)

	// 安全扫描
	if l.config.ScanMode != ScanModeOff {
		findings := ScanForThreats(body)
		if len(findings) > 0 {
			summary := FindingsSummary(findings)
			switch l.config.ScanMode {
			case ScanModeBlock:
				l.logger.Errorf("soul: blocked loading %s — threat patterns detected: %s", l.config.Path, summary)
				return errs.Newf("soul: threat patterns detected in %s: %s", l.config.Path, summary)
			case ScanModeWarn:
				l.logger.Warnf("soul: threat patterns detected in %s: %s (content still loaded)", l.config.Path, summary)
			}
		}
	}

	// 内容截断
	body = truncateContent(body, l.config.MaxContentBytes)

	// 自动发现模板变量
	variables := discoverVariables(body, meta)

	// 处理 enabled 开关（文件内 front matter 控制）
	enabled := true
	if v, ok := meta["enabled"]; ok {
		enabled = v == "true"
	}

	// 注册 Section
	l.registry.Register(Section{
		Name:      l.config.SectionName,
		Order:     l.config.Order,
		Content:   strings.TrimSpace(body),
		Enabled:   enabled,
		Variables: variables,
	})

	// 更新内部状态
	l.mu.Lock()
	l.content = strings.TrimSpace(body)
	l.modTime = info
	l.variables = variables
	onReload := l.onReload
	l.mu.Unlock()

	l.loaded.Store(true)
	l.logger.Infof("soul: loaded %s (%d bytes, %d variables, mtime=%s)",
		l.config.Path, len(body), len(variables), info.Format(time.RFC3339))

	// 触发热重载回调（在锁外回调，避免死锁）
	if onReload != nil {
		go onReload(strings.TrimSpace(body))
	}

	return nil
}

// Content 返回当前加载的 SOUL.md 内容（线程安全）。
func (l *SoulLoader) Content() string {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.content
}

// SetOnReload 设置热重载后的回调。
// 回调在 Load() 成功后、锁释放前被调用，传入新内容。
// 典型用法：通知 AdaptiveEngagementSyncer 重新解析画像。
func (l *SoulLoader) SetOnReload(cb OnReloadFunc) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.onReload = cb
}

// Loaded 返回是否已成功加载。
func (l *SoulLoader) Loaded() bool {
	return l.loaded.Load()
}

// StartWatcher 启动后台文件变更检测。
func (l *SoulLoader) StartWatcher(ctx context.Context) {
	if l.config.ReloadInterval <= 0 {
		return
	}

	go func() {
		ticker := time.NewTicker(l.config.ReloadInterval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-l.stopCh:
				return
			case <-ticker.C:
				l.checkAndReload()
			}
		}
	}()

	l.logger.Infof("soul: watcher started (interval=%s, path=%s)", l.config.ReloadInterval, l.config.Path)
}

// Stop 停止后台文件变更检测。
func (l *SoulLoader) Stop() {
	if l.stopped.CompareAndSwap(false, true) {
		close(l.stopCh)
	}
}

// checkAndReload 检查文件是否被修改，如果是则重新加载。
func (l *SoulLoader) checkAndReload() {
	st := l.config.Store.StatSoul(context.Background(), l.config.Path)
	if st.Err != nil {
		l.logger.Warnf("soul: stat %s failed: %v", l.config.Path, st.Err)
		return
	}
	if !st.Exists {
		if l.loaded.Load() {
			// 文件被删除，重建默认模板
			l.logger.Warnf("soul: %s was removed, recreating default", l.config.Path)
			if createErr := l.createDefault(); createErr != nil {
				l.logger.Errorf("soul: recreate default failed: %v", createErr)
				return
			}
			if loadErr := l.Load(); loadErr != nil {
				l.logger.Errorf("soul: reload after recreate failed: %v", loadErr)
			}
		}
		return
	}

	l.mu.RLock()
	lastModTime := l.modTime
	l.mu.RUnlock()

	if !st.ModTime.After(lastModTime) {
		return
	}

	l.logger.Infof("soul: %s changed (mtime %s → %s), reloading",
		l.config.Path, lastModTime.Format(time.RFC3339), st.ModTime.Format(time.RFC3339))

	if err := l.Load(); err != nil {
		l.logger.Errorf("soul: reload failed: %v", err)
	}
}

// Variables 返回当前 SOUL.md 中发现的模板变量（线程安全副本）。
func (l *SoulLoader) Variables() []Variable {
	l.mu.RLock()
	defer l.mu.RUnlock()
	if l.variables == nil {
		return nil
	}
	result := make([]Variable, len(l.variables))
	copy(result, l.variables)
	return result
}

// ModTime 返回当前已加载文件的最后修改时间。
func (l *SoulLoader) ModTime() time.Time {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.modTime
}
