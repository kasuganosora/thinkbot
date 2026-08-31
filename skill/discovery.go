package skill

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
)

// ============================================================================
// Skill 自动发现
//
// Skill 自动发现机制：
//   - 扫描指定目录下的所有 SKILL.md 文件
//   - 自动加载并注册到 SkillManager
//   - 支持热重载（文件变化时自动刷新）
//
// 与现有 Loader 的区别：
//   - Loader：从单个根目录加载（一级子目录）
//   - Discovery：支持多根目录、递归深度、热重载
//
// 目录结构约定：
//   skills/
//   ├── pdf/
//   │   ├── SKILL.md
//   │   ├── scripts/
//   │   └── references/
//   ├── web-search/
//   │   └── SKILL.md
//   └── code-review/
//       └── SKILL.md
// ============================================================================

// DiscoveryConfig 配置 Skill 自动发现。
type DiscoveryConfig struct {
	// RootDirs 要扫描的根目录列表。
	// 每个子目录如果包含 SKILL.md 则被视为一个 Skill。
	RootDirs []string

	// MaxDepth 递归搜索的最大深度。
	// 0 = 仅扫描 RootDirs 直接子目录
	// 1 = 扫描两级深度
	// 默认 0。
	MaxDepth int

	// EnableHotReload 是否启用文件变化热重载。
	// 默认 false。
	EnableHotReload bool

	// ReloadInterval 热重载检查间隔。
	// 默认 5 秒。
	ReloadInterval time.Duration
}

// DefaultDiscoveryConfig 返回默认发现配置。
func DefaultDiscoveryConfig() DiscoveryConfig {
	return DiscoveryConfig{
		MaxDepth:       0,
		ReloadInterval: 5 * time.Second,
	}
}

// DiscoveryResult 包含发现结果。
type DiscoveryResult struct {
	// Discovered 成功发现的 Skill。
	Discovered []Skill

	// Skipped 被跳过的目录（无 SKILL.md 或无效）。
	Skipped []string

	// Errors 发现过程中的错误。
	Errors []DiscoveryError
}

// DiscoveryError 是发现过程中的错误。
type DiscoveryError struct {
	Path  string `json:"path"`
	Error string `json:"error"`
}

// Discover 扫描目录并发现 Skill。
func Discover(config DiscoveryConfig) *DiscoveryResult {
	result := &DiscoveryResult{}

	for _, rootDir := range config.RootDirs {
		discoverInDir(rootDir, config.MaxDepth, result)
	}

	return result
}

// discoverInDir 在指定目录中搜索 Skill。
func discoverInDir(dir string, depth int, result *DiscoveryResult) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		result.Errors = append(result.Errors, DiscoveryError{
			Path:  dir,
			Error: fmt.Sprintf("read dir: %v", err),
		})
		return
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		subDir := filepath.Join(dir, entry.Name())
		skillFile := filepath.Join(subDir, "SKILL.md")

		if _, err := os.Stat(skillFile); err == nil {
			// 发现 SKILL.md，尝试加载（复用 loader.go 的 parseFrontMatter）
			skill, err := loadSkillFromDir(subDir)
			if err != nil {
				result.Errors = append(result.Errors, DiscoveryError{
					Path:  subDir,
					Error: fmt.Sprintf("load skill: %v", err),
				})
				continue
			}
			skill.Source = "fs"
			skill.Dir = subDir
			result.Discovered = append(result.Discovered, *skill)
		} else if depth > 0 {
			// 递归搜索
			discoverInDir(subDir, depth-1, result)
		} else {
			result.Skipped = append(result.Skipped, subDir)
		}
	}
}

// loadSkillFromDir 从目录加载 Skill。
// 复用 loader.go 中的 parseFrontMatter + scanResources。
func loadSkillFromDir(dir string) (*Skill, error) {
	skillFile := filepath.Join(dir, "SKILL.md")

	data, err := os.ReadFile(skillFile)
	if err != nil {
		return nil, fmt.Errorf("read SKILL.md: %w", err)
	}

	content := string(data)
	meta, body := parseFrontMatter(content)

	skill := &Skill{
		Name:          meta.Name,
		Description:   meta.Description,
		Compatibility: meta.Compatibility,
		Content:       body,
		Enabled:       true,
		Resources:     scanResources(dir),
	}

	if meta.Enabled != nil {
		skill.Enabled = *meta.Enabled
	}

	if skill.Name == "" {
		return nil, fmt.Errorf("skill name is required in front matter")
	}
	if skill.Description == "" {
		return nil, fmt.Errorf("skill description is required in front matter")
	}

	return skill, nil
}

// ============================================================================
// SkillHotReloader — 热重载器
// ============================================================================

// SkillHotReloader 监控 Skill 文件变化并触发重载。
type SkillHotReloader struct {
	mu       sync.Mutex
	config   DiscoveryConfig
	logger   *zap.SugaredLogger
	lastMod  map[string]time.Time
	stopChan chan struct{}
}

// NewSkillHotReloader 创建热重载器。
func NewSkillHotReloader(config DiscoveryConfig, logger *zap.SugaredLogger) *SkillHotReloader {
	if logger == nil {
		logger = zap.NewNop().Sugar()
	}
	return &SkillHotReloader{
		config:   config,
		logger:   logger,
		lastMod:  make(map[string]time.Time),
		stopChan: make(chan struct{}),
	}
}

// Start 启动后台热重载 goroutine。
// onReload 在检测到变化时被调用，接收最新的 DiscoveryResult。
func (r *SkillHotReloader) Start(onReload func(*DiscoveryResult)) {
	if !r.config.EnableHotReload {
		return
	}

	interval := r.config.ReloadInterval
	if interval <= 0 {
		interval = 5 * time.Second
	}

	// 先记录当前所有 SKILL.md 的修改时间
	r.scanInitial()

	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for {
			select {
			case <-r.stopChan:
				return
			case <-ticker.C:
				if r.checkChanges() {
					result := Discover(r.config)
					r.logger.Infow("skill hot reload triggered",
						"discovered", len(result.Discovered),
						"errors", len(result.Errors))
					onReload(result)
				}
			}
		}
	}()
}

// Stop 停止热重载。
func (r *SkillHotReloader) Stop() {
	select {
	case <-r.stopChan:
	default:
		close(r.stopChan)
	}
}

// scanInitial 记录所有现有 SKILL.md 文件的修改时间。
func (r *SkillHotReloader) scanInitial() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, rootDir := range r.config.RootDirs {
		_ = filepath.Walk(rootDir, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return nil
			}
			if info.IsDir() || !strings.HasSuffix(path, "SKILL.md") {
				return nil
			}
			r.lastMod[path] = info.ModTime()
			return nil
		})
	}
}

// checkChanges 检查文件是否有变化。
func (r *SkillHotReloader) checkChanges() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	changed := false

	for _, rootDir := range r.config.RootDirs {
		_ = filepath.Walk(rootDir, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return nil
			}
			if info.IsDir() || !strings.HasSuffix(path, "SKILL.md") {
				return nil
			}

			lastTime, exists := r.lastMod[path]
			if !exists {
				// 新文件
				r.lastMod[path] = info.ModTime()
				changed = true
			} else if info.ModTime().After(lastTime) {
				r.lastMod[path] = info.ModTime()
				changed = true
			}
			return nil
		})
	}

	return changed
}
