package testconfig

import (
	"os"
	"path/filepath"
	"sync"

	"github.com/kasuganosora/thinkbot/config"
)

// TestLLMConfig 描述集成测试所需的 LLM 配置。
type TestLLMConfig struct {
	Provider  string
	APIKey    string
	BaseURL   string
	ChatPath  string
	Model     string
	MaxTokens int
}

// .env 文件中使用的键名。
const (
	keyAPIKey   = "TEST_LLM_API_KEY"
	keyBaseURL  = "TEST_LLM_BASE_URL"
	keyChatPath = "TEST_LLM_CHAT_PATH"
	keyModel    = "TEST_LLM_MODEL"
	keyProvider = "TEST_LLM_PROVIDER"
)

// 默认值（从 .env 或环境变量加载失败时使用）。
var defaults = TestLLMConfig{
	Provider:  "bigmodel",
	APIKey:    "",
	BaseURL:   "https://open.bigmodel.cn/api/coding/paas",
	ChatPath:  "/v4/chat/completions",
	Model:     "glm-5.2",
	MaxTokens: 4096,
}

var (
	envOnce  sync.Once
	envCache map[string]string
)

// loadEnvFile 延迟加载项目根目录的 .env 文件（仅加载一次）。
func loadEnvFile() map[string]string {
	envOnce.Do(func() {
		// 尝试从当前目录向上查找 .env 文件
		dir, _ := os.Getwd()
		for d := dir; d != filepath.Dir(d); d = filepath.Dir(d) {
			if _, err := os.Stat(filepath.Join(d, ".env")); err == nil {
				values, err := config.LoadEnvFile(filepath.Join(d, ".env"))
				if err == nil && values != nil {
					envCache = values
					return
				}
			}
		}
		envCache = map[string]string{}
	})
	return envCache
}

// getString 按优先级获取值：.env 文件 → OS 环境变量 → 默认值。
func getString(key, def string) string {
	// 1. .env 文件
	if env := loadEnvFile(); env != nil {
		if v, ok := env[key]; ok && v != "" {
			return v
		}
	}
	// 2. OS 环境变量
	if v := os.Getenv(key); v != "" {
		return v
	}
	// 3. 默认值
	return def
}

// Load 返回测试用的 LLM 配置。
// 优先级：.env 文件 > OS 环境变量 > 内置默认值。
//
// 在 .env 文件中添加以下行即可覆盖测试模型配置：
//
//	TEST_LLM_API_KEY=your-api-key
//	TEST_LLM_BASE_URL=https://open.bigmodel.cn/api/coding/paas
//	TEST_LLM_CHAT_PATH=/v4/chat/completions
//	TEST_LLM_MODEL=glm-5.2
func Load() TestLLMConfig {
	return TestLLMConfig{
		Provider:  getString(keyProvider, defaults.Provider),
		APIKey:    getString(keyAPIKey, defaults.APIKey),
		BaseURL:   getString(keyBaseURL, defaults.BaseURL),
		ChatPath:  getString(keyChatPath, defaults.ChatPath),
		Model:     getString(keyModel, defaults.Model),
		MaxTokens: defaults.MaxTokens,
	}
}
